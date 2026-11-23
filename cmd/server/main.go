// Command server runs the Tavily-compatible search and extract API.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/astralabs/tavilyd/internal/api"
	"github.com/astralabs/tavilyd/internal/cache"
	"github.com/astralabs/tavilyd/internal/config"
	"github.com/astralabs/tavilyd/internal/extract"
	"github.com/astralabs/tavilyd/internal/llm"
	"github.com/astralabs/tavilyd/internal/observability"
	"github.com/astralabs/tavilyd/internal/proxy"
	"github.com/astralabs/tavilyd/internal/rerank"
	"github.com/astralabs/tavilyd/internal/research"
	"github.com/astralabs/tavilyd/internal/search"
	"github.com/astralabs/tavilyd/internal/security"
	"github.com/astralabs/tavilyd/internal/usage"
)

// buildVersion is overridden at build time via -ldflags.
var buildVersion = api.Version

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-healthcheck" || os.Args[1] == "--healthcheck") {
		os.Exit(healthcheck())
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

// healthcheck is the distroless-compatible container probe: it calls /livez on
// the local listener and exits 0/1.
func healthcheck() int {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		port = "8080"
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/livez")
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	log := observability.NewLogger(cfg.LogLevel, os.Stdout)
	slog.SetDefault(log)
	log.Info("starting tavilyd", "version", buildVersion, "config", cfg.Redacted())

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	metrics := observability.NewMetrics()
	tracing, err := observability.SetupTracing(rootCtx, cfg.OTelEnabled, cfg.OTelEndpoint, cfg.ServiceName, cfg.OTelInsecure)
	if err != nil {
		log.Warn("tracing disabled", "error", err)
		tracing = &observability.Tracing{}
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tracing.Shutdown(ctx)
	}()

	cacheStore, err := cache.New(cfg, log, metrics)
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}
	defer cacheStore.Close()

	proxyEngine, proxyPool, err := buildProxyEngine(cfg, log, metrics)
	if err != nil {
		return fmt.Errorf("proxy: %w", err)
	}
	defer proxyEngine.Close()
	if cfg.ProxyMode != "off" {
		log.Info("proxy layer enabled", "mode", cfg.ProxyMode, "policy", cfg.ProxyPolicy)
	}

	usageStore, err := usage.New(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("usage store: %w", err)
	}
	defer usageStore.Close()

	guard := security.NewGuard(cfg.AllowPrivateIPs, 4*time.Second)
	if err := guard.SetAllowlist(cfg.SSRFAllowlist); err != nil {
		return fmt.Errorf("ssrf allowlist: %w", err)
	}
	searcher := search.NewClient(cfg, internalClient(cfg.SearxTimeout, cfg.SearxMaxConns), log, metrics)
	var searcherProxy *search.Client
	if cfg.SearxProxyURL != "" {
		proxyCfg := *cfg
		proxyCfg.SearxURL = cfg.SearxProxyURL
		searcherProxy = search.NewClient(&proxyCfg, internalClient(cfg.SearxTimeout, cfg.SearxMaxConns), log, metrics)
	}
	extractor := extract.NewExtractor(cfg, guard, cacheStore, rootCtx, log, metrics)
	extractor.SetProxyPolicy(proxyEngine, cfg.ProxyMaxRetries, cfg.ProxyFetchExtra)
	defer extractor.Close()
	tei := rerank.NewTEIClient(cfg, internalClient(cfg.TEITimeout, cfg.TEIParallelism*2))
	scorer := rerank.NewScorer(tei, log, metrics)
	chunker := rerank.NewChunker(rerank.ChunkerConfig{
		Tokens: cfg.ChunkTokens, Overlap: cfg.ChunkOverlap, MaxPerPage: cfg.MaxChunksPerPage,
		CharsPerToken: cfg.CharsPerToken, Margin: cfg.TokenSafetyMargin,
	})

	var (
		llmClient   *llm.Client
		researchMgr *research.Manager
	)
	if cfg.ResearchEnabled && cfg.LLMBaseURL != "" {
		llmClient, err = llm.New(cfg, log)
		if err != nil {
			return fmt.Errorf("llm: %w", err)
		}
		if strings.HasPrefix(cfg.LLMBaseURL, "http://") && !privateLLMHost(cfg.LLMBaseURL) {
			log.Warn("LLM_BASE_URL is plain HTTP to a non-private host; prompts and API keys cross the network unencrypted", "host", cfg.LLMBaseURL)
		}
		store, serr := research.NewStore(cfg.RedisURL, cfg.ResearchResultTTL)
		if serr != nil {
			return fmt.Errorf("research store: %w", serr)
		}
		tools := &research.LiveTools{Searcher: searcher, Extractor: extractor, Scorer: scorer, Chunker: chunker}
		researchMgr = research.NewManager(cfg, llmClient, tools, store, log)
		researchMgr.Start(context.Background())
		defer researchMgr.Close()
		log.Info("research enabled", "model", cfg.LLMModel, "workers", cfg.ResearchWorkers)
	}

	srv := api.NewServer(api.Deps{
		Config: cfg, Log: log, Metrics: metrics, Cache: cacheStore, Searcher: searcher, SearcherProxy: searcherProxy,
		Extractor: extractor, Scorer: scorer, Chunker: chunker,
		Auth:     security.NewAuthenticator(cfg.APIKeys),
		Limiter:  security.NewRateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst, 10*time.Minute),
		Proxy:    proxyEngine,
		Research: researchMgr,
		LLM:      llmClient,
		Usage:    usageStore,
	})

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           h2c.NewHandler(srv.Router(), &http2.Server{MaxConcurrentStreams: 250}),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    1 << 20,
		BaseContext:       func(net.Listener) context.Context { return rootCtx },
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	internal := startInternalServers(cfg, metrics, log)

	errCh := make(chan error, 1)
	go func() {
		ln, lerr := net.Listen("tcp", cfg.Addr)
		if lerr != nil {
			errCh <- lerr
			return
		}
		log.Info("listening", "addr", cfg.Addr, "http2", "h2c")
		if serr := httpSrv.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			errCh <- serr
			return
		}
		errCh <- nil
	}()

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for range hup {
			if l, ok := proxyPool.(*proxy.List); ok {
				if err := l.Reload(); err != nil {
					log.Warn("proxy list reload failed", "error", err)
				} else {
					log.Info("proxy list reloaded")
				}
			}
		}
	}()

	select {
	case <-rootCtx.Done():
		log.Info("shutdown signal received, draining")
	case serveErr := <-errCh:
		if serveErr != nil {
			return fmt.Errorf("http server: %w", serveErr)
		}
		return nil
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown incomplete", "error", err)
		_ = httpSrv.Close()
	}
	internal.stop(shutdownCtx)
	log.Info("stopped")
	return nil
}

type internalServers struct {
	servers []*http.Server
}

func (i *internalServers) stop(ctx context.Context) {
	var wg sync.WaitGroup
	for _, s := range i.servers {
		wg.Add(1)
		go func(s *http.Server) {
			defer wg.Done()
			if err := s.Shutdown(ctx); err != nil {
				_ = s.Close()
			}
		}(s)
	}
	wg.Wait()
}

// startInternalServers exposes /metrics and /debug/pprof on loopback-only ports.
func startInternalServers(cfg *config.Config, m *observability.Metrics, log *slog.Logger) *internalServers {
	out := &internalServers{}

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", m.Handler())
	metricsMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	metricsSrv := &http.Server{
		Addr: cfg.MetricsAddr, Handler: metricsMux,
		ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second,
	}
	out.servers = append(out.servers, metricsSrv)
	go func() {
		log.Info("metrics listening", "addr", cfg.MetricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("metrics server stopped", "error", err)
		}
	}()

	if cfg.PprofAddr != "" && cfg.PprofAddr != cfg.MetricsAddr {
		pprofMux := http.NewServeMux()
		pprofMux.HandleFunc("/debug/pprof/", pprof.Index)
		pprofMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		pprofMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		pprofMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		pprofMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		pprofSrv := &http.Server{
			Addr: cfg.PprofAddr, Handler: pprofMux,
			ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 60 * time.Second,
		}
		out.servers = append(out.servers, pprofSrv)
		go func() {
			log.Info("pprof listening", "addr", cfg.PprofAddr)
			if err := pprofSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Warn("pprof server stopped", "error", err)
			}
		}()
	}
	return out
}

// privateLLMHost reports whether an http:// LLM endpoint stays on a private
// address (no warning needed).
func privateLLMHost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsPrivate() || ip.IsLoopback())
}

// buildProxyEngine constructs the configured pool and policy engine.
func buildProxyEngine(cfg *config.Config, log *slog.Logger, m *observability.Metrics) (*proxy.Engine, proxy.Pool, error) {
	if cfg.ProxyMode == "off" {
		return proxy.NewEngine("off", cfg.ProxyPolicy, nil, nil, cfg.ProxyHostMemoryTTL, m), nil, nil
	}
	var (
		pool proxy.Pool
		err  error
	)
	switch cfg.ProxyMode {
	case "rotating":
		pool, err = proxy.NewRotating(cfg.ProxyEndpoint, cfg.ProxyStickyTemplate, m, cfg.ProxyTLSInsecure)
	case "list":
		pool, err = proxy.NewList(cfg.ProxyListFile, cfg.ProxyStickyTemplate, m, cfg.ProxyTLSInsecure)
	}
	if err != nil {
		return nil, nil, err
	}
	var rules []proxy.Rule
	if cfg.ProxyHostRulesFile != "" {
		data, rerr := os.ReadFile(cfg.ProxyHostRulesFile)
		if rerr != nil {
			pool.Close()
			return nil, nil, rerr
		}
		rules, rerr = proxy.ParseRules(data)
		if rerr != nil {
			pool.Close()
			return nil, nil, rerr
		}
	}
	log.Info("proxy pool ready", "mode", cfg.ProxyMode, "rules", len(rules))
	eng := proxy.NewEngine(cfg.ProxyMode, cfg.ProxyPolicy, pool, rules, cfg.ProxyHostMemoryTTL, m)
	eng.SetMaxConns(cfg.ProxyMaxConnsPerProxy)
	return eng, pool, nil
}

// internalClient builds the shared outbound client for trusted services on the
// compose network (no proxy, HTTP/2, pooled connections).
func internalClient(timeout time.Duration, perHost int) *http.Client {
	if perHost <= 0 {
		perHost = 32
	}
	tr := &http.Transport{
		Proxy:                 nil,
		MaxIdleConns:          perHost * 4,
		MaxIdleConnsPerHost:   perHost,
		MaxConnsPerHost:       perHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   3 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
		DialContext: (&net.Dialer{
			Timeout:   2 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}
