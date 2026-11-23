// Command proxyrelay is an internal forward proxy for the headless browser.
//
// Chrome cannot send proxy credentials in --proxy-server, so the browser points
// at this relay (no auth) and the relay applies the same policy engine and
// upstream credentials as the Go fetcher. It only listens on the compose
// network and refuses non-public destinations.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/astralabs/tavilyd/internal/proxy"
	"github.com/astralabs/tavilyd/internal/security"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck())
	}
	if err := run(); err != nil {
		log.Fatalf("proxyrelay: %v", err)
	}
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func healthcheck() int {
	port := env("LISTEN", ":8888")
	_, p, err := net.SplitHostPort(port)
	if err != nil || p == "" {
		p = "8888"
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + p + "/health")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func run() error {
	mode := env("PROXY_MODE", "off")
	policy := env("PROXY_POLICY", "direct_first")
	var (
		pool proxy.Pool
		err  error
	)
	switch mode {
	case "off":
	case "rotating":
		pool, err = proxy.NewRotating(env("PROXY_ENDPOINT", ""), env("PROXY_STICKY_TEMPLATE", ""), nil, env("PROXY_TLS_INSECURE", "false") == "true")
	case "list":
		pool, err = proxy.NewList(env("PROXY_LIST_FILE", ""), env("PROXY_STICKY_TEMPLATE", ""), nil, env("PROXY_TLS_INSECURE", "false") == "true")
	default:
		return fmt.Errorf("unsupported PROXY_MODE %q", mode)
	}
	if err != nil {
		return err
	}
	var rules []proxy.Rule
	if path := env("PROXY_HOST_RULES", ""); path != "" {
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rules, rerr = proxy.ParseRules(data)
		if rerr != nil {
			return rerr
		}
	}
	engine := proxy.NewEngine(mode, policy, pool, rules, time.Hour, nil)
	if n, _ := strconv.Atoi(env("PROXY_MAX_CONNS_PER_PROXY", "64")); n > 0 {
		engine.SetMaxConns(n)
	}
	defer engine.Close()

	guard := security.NewGuard(false, 3*time.Second)
	srv := &http.Server{
		Addr:              env("LISTEN", ":8888"),
		ReadHeaderTimeout: 5 * time.Second,
		Handler:           &relay{engine: engine, guard: guard},
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	log.Printf("proxyrelay listening on %s (mode=%s policy=%s)", srv.Addr, mode, policy)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type relay struct {
	engine *proxy.Engine
	guard  *security.Guard
}

func (r *relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodGet && req.URL.Path == "/health" {
		snap := r.engine.Snapshot()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "mode": snap.Mode, "total": snap.Total, "healthy": snap.Healthy, "degraded": snap.Degraded,
		})
		return
	}
	if req.Method != http.MethodConnect {
		http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
		return
	}
	host, port, err := net.SplitHostPort(req.Host)
	if err != nil || host == "" || port == "" {
		http.Error(w, "bad CONNECT target", http.StatusBadRequest)
		return
	}
	// Refuse non-public destinations before dialing anything.
	target, err := r.guard.ValidateURL(req.Context(), "https://"+net.JoinHostPort(host, port))
	if err != nil {
		http.Error(w, "destination blocked", http.StatusForbidden)
		return
	}
	route := r.engine.Decide(req.Context(), target.Host, target.Host)
	addr := net.JoinHostPort(target.IPs[0].String(), target.Port)

	var upstream net.Conn
	if route.UseProxy && route.Proxy != nil {
		if err := r.engine.Acquire(req.Context(), route.Proxy); err != nil {
			http.Error(w, "proxy busy", http.StatusServiceUnavailable)
			return
		}
		defer r.engine.Release(route.Proxy)
		upstream, err = proxy.Dial(req.Context(), route.Proxy, addr, 4*time.Second)
	} else {
		d := net.Dialer{Timeout: 4 * time.Second}
		upstream, err = d.DialContext(req.Context(), "tcp", addr)
	}
	if err != nil {
		http.Error(w, "upstream dial failed", http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	_, _ = io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n")
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}
