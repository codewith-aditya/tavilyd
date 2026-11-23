package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

// Version is reported by /health and the root endpoint.
const Version = "1.0.0"

type depStatus struct {
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
}

type healthResponse struct {
	Status           string               `json:"status"`
	Engines          *engineStatus        `json:"engines,omitempty"`
	Version          string               `json:"version"`
	UptimeSeconds    int64                `json:"uptime_seconds"`
	Dependencies     map[string]depStatus `json:"dependencies"`
	Optional         map[string]depStatus `json:"optional,omitempty"`
	OptionalDegraded []string             `json:"optional_degraded,omitempty"`
	Cache            cacheStats           `json:"cache"`
	Proxy            *proxyStatus         `json:"proxy,omitempty"`
}

type engineStatus struct {
	Used      []string          `json:"used"`
	Suspended map[string]string `json:"suspended,omitempty"`
}

type proxyStatus struct {
	Mode      string `json:"mode"`
	Total     int    `json:"total"`
	Healthy   int    `json:"healthy"`
	Degraded  bool   `json:"degraded"`
	LastProbe string `json:"last_probe,omitempty"`
}

type cacheStats struct {
	Hits    uint64 `json:"hits_l1"`
	Misses  uint64 `json:"misses_l1"`
	Entries int    `json:"entries_l1"`
}

// coreSlowThreshold returns the latency above which a healthy core dependency
// is reported as degraded ("slow").
func coreSlowThreshold(name string) time.Duration {
	switch name {
	case "redis":
		return 250 * time.Millisecond
	case "searxng":
		return 2 * time.Second
	case "reranker":
		return 2 * time.Second
	case "browser":
		return 2 * time.Second
	default:
		return 2 * time.Second
	}
}

// overallStatus: core dependencies decide the service status. Optional
// failures are surfaced separately and never flip an otherwise healthy service.
func overallStatus(core map[string]depStatus) string {
	status := "ok"
	for _, st := range core {
		if st.Status == "down" {
			return "down"
		}
		if st.Status != "ok" {
			status = "degraded"
		}
	}
	return status
}

// HandleHealth reports core dependencies synchronously and serves the cached
// LLM/proxy/engine state. It never probes the LLM on the request path.
func (s *Server) HandleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	coreChecks := map[string]func(context.Context) (string, error){
		"redis": s.cache.Health,
		"searxng": func(ctx context.Context) (string, error) {
			if err := s.searcher.Healthy(ctx); err != nil {
				if s.searcher.IsOpen() {
					return "degraded", err
				}
				return "down", err
			}
			return "ok", nil
		},
		"reranker": s.scorer.Health,
		"browser":  s.extractor.Health,
	}

	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		core = make(map[string]depStatus, len(coreChecks))
	)
	for name, fn := range coreChecks {
		name, fn := name, fn
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			status, err := fn(ctx)
			lat := time.Since(start)
			st := depStatus{Status: status, LatencyMS: lat.Milliseconds()}
			if err != nil {
				st.Detail = truncateReason(err.Error())
			}
			if st.Status == "" {
				st.Status = "down"
			}
			if st.Status == "ok" && lat > coreSlowThreshold(name) {
				st.Status = "degraded"
				st.Detail = "slow: " + lat.Round(time.Millisecond).String()
			}
			if s.metrics != nil {
				s.metrics.SetDependency(name, st.Status)
			}
			mu.Lock()
			core[name] = st
			mu.Unlock()
		}()
	}
	wg.Wait()

	optional := map[string]depStatus{}
	var optionalDegraded []string

	if s.llmProbe != nil {
		status, detail, checked := s.llmProbe.snapshot()
		st := depStatus{Status: status, Detail: detail}
		if !checked.IsZero() {
			st.Detail = trimDetail(detail, "probe cached")
		}
		optional["llm"] = st
		if s.metrics != nil {
			s.metrics.SetDependency("llm", status)
		}
		if status != "ok" {
			optionalDegraded = append(optionalDegraded, "llm")
		}
	}

	if s.proxy != nil {
		status, err := s.proxyHealth(ctx)
		st := depStatus{Status: status}
		if err != nil {
			st.Detail = truncateReason(err.Error())
		}
		optional["proxy"] = st
		if s.metrics != nil {
			s.metrics.SetDependency("proxy", status)
		}
		if status != "ok" {
			optionalDegraded = append(optionalDegraded, "proxy")
		}
	}

	s.engineMu.Lock()
	suspended := make(map[string]string, len(s.lastSuspended))
	for engine, reason := range s.lastSuspended {
		suspended[engine] = reason
	}
	used := append([]string(nil), s.lastEnginesUsed...)
	s.engineMu.Unlock()
	for engine, reason := range suspended {
		name := "engine:" + engine
		optional[name] = depStatus{Status: "degraded", Detail: truncateReason(reason)}
		optionalDegraded = append(optionalDegraded, name)
		if s.metrics != nil {
			s.metrics.SetDependency(name, "degraded")
		}
	}

	hits, misses, entries := s.cache.Stats()
	resp := healthResponse{
		Status:           overallStatus(core),
		Version:          Version,
		UptimeSeconds:    int64(time.Since(s.started).Seconds()),
		Dependencies:     core,
		Optional:         optional,
		OptionalDegraded: optionalDegraded,
		Cache:            cacheStats{Hits: hits, Misses: misses, Entries: entries},
	}
	if used != nil || suspended != nil {
		resp.Engines = &engineStatus{Used: used, Suspended: suspended}
	}
	if s.proxy != nil {
		snap := s.proxy.Snapshot()
		resp.Proxy = &proxyStatus{Mode: snap.Mode, Total: snap.Total, Healthy: snap.Healthy, Degraded: snap.Degraded, LastProbe: snap.LastProbe}
	}
	writeJSON(w, http.StatusOK, resp)
}

func trimDetail(detail, fallback string) string {
	if detail == "" {
		return fallback
	}
	return detail
}

// proxyHealth reports pool health without exposing endpoints or credentials.
func (s *Server) proxyHealth(context.Context) (string, error) {
	if s.proxy == nil {
		return "ok", nil
	}
	snap := s.proxy.Snapshot()
	if snap.Mode == "off" {
		return "ok", nil
	}
	if snap.Healthy == 0 || snap.Degraded {
		return "degraded", errors.New("proxy pool unhealthy; serving direct")
	}
	return "ok", nil
}

// HandleLivez is a liveness probe: it only proves the process is running.
func (s *Server) HandleLivez(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// HandleReadyz is a readiness probe: requests can be served when SearXNG is
// reachable (Redis and the reranker degrade gracefully).
func (s *Server) HandleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 1500*time.Millisecond)
	defer cancel()
	if err := s.searcher.Healthy(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "reason": "searxng"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// HandleRoot advertises the service.
func (s *Server) HandleRoot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        "tavilyd",
		"version":     Version,
		"description": "Self-hosted, Tavily-compatible search and extract API",
		"endpoints":   []string{"POST /search", "POST /extract", "GET /health", "GET /livez", "GET /readyz"},
	})
}

// HandleNotFound returns a JSON 404 in the Tavily envelope.
func (s *Server) HandleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, Errorf(http.StatusNotFound, "Not found"))
}

func truncateReason(s string) string {
	if len(s) <= 240 {
		return s
	}
	return s[:240] + "..."
}
