package observability

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics bundles every Prometheus collector owned by the service.
type Metrics struct {
	Registry *prometheus.Registry

	HTTPRequests *prometheus.CounterVec
	HTTPDuration *prometheus.HistogramVec
	HTTPInflight prometheus.Gauge

	StageDuration   *prometheus.HistogramVec
	CacheLookups    *prometheus.CounterVec
	SearchResults   prometheus.Histogram
	Extractions     *prometheus.CounterVec
	RerankCalls     *prometheus.CounterVec
	RerankBatch     prometheus.Histogram
	Deps            *prometheus.GaugeVec
	RateLimited     prometheus.Counter
	CleanRemoved    *prometheus.CounterVec
	CleanDuration   prometheus.Histogram
	ProxyRequests   *prometheus.CounterVec
	ProxyLatencyH   prometheus.Histogram
	ProxyPoolSizeG  *prometheus.GaugeVec
	FeedbackTotal   prometheus.Counter
	EngineSuspended *prometheus.GaugeVec
	EnginesUsed     prometheus.Histogram
}

// NewMetrics builds a metrics set on its own registry (safe for tests).
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	f := promauto.With(reg)
	m := &Metrics{
		Registry: reg,
		HTTPRequests: f.NewCounterVec(prometheus.CounterOpts{
			Name: "tavilyd_http_requests_total", Help: "HTTP requests by route and status.",
		}, []string{"route", "method", "status"}),
		HTTPDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "tavilyd_http_request_duration_seconds", Help: "End-to-end HTTP latency.",
			Buckets: []float64{0.005, 0.01, 0.03, 0.1, 0.25, 0.5, 1, 2, 3, 4, 6, 10},
		}, []string{"route"}),
		HTTPInflight: f.NewGauge(prometheus.GaugeOpts{
			Name: "tavilyd_http_inflight_requests", Help: "Requests currently being served.",
		}),
		StageDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "tavilyd_stage_duration_seconds", Help: "Per-stage latency.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.03, 0.1, 0.3, 0.6, 1, 2, 4},
		}, []string{"stage"}),
		CacheLookups: f.NewCounterVec(prometheus.CounterOpts{
			Name: "tavilyd_cache_lookups_total", Help: "Cache lookups by result.",
		}, []string{"result"}),
		SearchResults: f.NewHistogram(prometheus.HistogramOpts{
			Name: "tavilyd_search_results", Help: "Candidates returned by SearXNG.",
			Buckets: []float64{0, 1, 3, 5, 10, 15, 20, 25, 30},
		}),
		Extractions: f.NewCounterVec(prometheus.CounterOpts{
			Name: "tavilyd_extractions_total", Help: "Page extractions by tier and outcome.",
		}, []string{"tier", "outcome"}),
		RerankCalls: f.NewCounterVec(prometheus.CounterOpts{
			Name: "tavilyd_rerank_calls_total", Help: "Reranker invocations by backend and result.",
		}, []string{"backend", "result"}),
		RerankBatch: f.NewHistogram(prometheus.HistogramOpts{
			Name: "tavilyd_rerank_batch_size", Help: "Documents per rerank call.",
			Buckets: []float64{1, 2, 5, 10, 20, 32, 64, 128},
		}),
		Deps: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "tavilyd_dependency_up", Help: "Dependency health: 1 ok, 0.5 degraded, 0 down.",
		}, []string{"dependency"}),
		RateLimited: f.NewCounter(prometheus.CounterOpts{
			Name: "tavilyd_rate_limited_total", Help: "Requests rejected by the rate limiter.",
		}),
		CleanRemoved: f.NewCounterVec(prometheus.CounterOpts{
			Name: "tavilyd_clean_removed_bytes_total", Help: "Bytes removed by the content cleanup layer, per rule.",
		}, []string{"rule"}),
		CleanDuration: f.NewHistogram(prometheus.HistogramOpts{
			Name: "tavilyd_clean_duration_seconds", Help: "Time spent cleaning one page.",
			Buckets: []float64{0.0005, 0.001, 0.002, 0.005, 0.01, 0.02, 0.05, 0.1},
		}),
		ProxyRequests: f.NewCounterVec(prometheus.CounterOpts{
			Name: "tavilyd_proxy_requests_total", Help: "Proxied/direct fetch outcomes.",
		}, []string{"outcome"}),
		ProxyLatencyH: f.NewHistogram(prometheus.HistogramOpts{
			Name: "tavilyd_proxy_latency_seconds", Help: "Proxy fetch latency.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8},
		}),
		ProxyPoolSizeG: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "tavilyd_proxy_pool_size", Help: "Proxy pool size by state.",
		}, []string{"state"}),
		FeedbackTotal: f.NewCounter(prometheus.CounterOpts{
			Name: "tavilyd_feedback_total", Help: "Feedback submissions received.",
		}),
		EngineSuspended: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "tavilyd_engine_suspended", Help: "1 when an engine is currently suspended/unresponsive.",
		}, []string{"engine"}),
		EnginesUsed: f.NewHistogram(prometheus.HistogramOpts{
			Name: "tavilyd_search_engines_used", Help: "Distinct engines contributing to one query.",
			Buckets: []float64{0, 1, 2, 3, 4, 5, 6},
		}),
	}
	reg.MustRegister(prometheus.NewGoCollector())
	reg.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	return m
}

// Handler serves the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

// ObserveStage records a stage duration, keyed by the stage name.
func (m *Metrics) ObserveStage(stage string, d time.Duration) {
	m.StageDuration.WithLabelValues(stage).Observe(d.Seconds())
}

// ObserveRequest records one finished HTTP request.
func (m *Metrics) ObserveRequest(route, method string, status int, d time.Duration) {
	m.HTTPRequests.WithLabelValues(route, method, strconv.Itoa(status)).Inc()
	m.HTTPDuration.WithLabelValues(route).Observe(d.Seconds())
}

// ObserveClean records bytes removed by a cleanup rule.
func (m *Metrics) ObserveClean(rule string, bytes int64) {
	if bytes <= 0 {
		return
	}
	m.CleanRemoved.WithLabelValues(rule).Add(float64(bytes))
}

// ObserveCleanDuration records the time spent cleaning one page.
func (m *Metrics) ObserveCleanDuration(d time.Duration) {
	if d > 0 {
		m.CleanDuration.Observe(d.Seconds())
	}
}

// ProxyRequest counts one proxy/direct outcome.
func (m *Metrics) ProxyRequest(outcome string) {
	m.ProxyRequests.WithLabelValues(outcome).Inc()
}

// ProxyLatency records proxy fetch latency.
func (m *Metrics) ProxyLatency(d time.Duration) {
	if d > 0 {
		m.ProxyLatencyH.Observe(d.Seconds())
	}
}

// ProxyPoolSize sets a pool size gauge.
func (m *Metrics) ProxyPoolSize(state string, n int) {
	m.ProxyPoolSizeG.WithLabelValues(state).Set(float64(n))
}

// SetDependency maps a health status string to the dependency gauge.
func (m *Metrics) SetDependency(name, status string) {
	v := 0.0
	switch status {
	case "ok":
		v = 1
	case "degraded":
		v = 0.5
	}
	m.Deps.WithLabelValues(name).Set(v)
}
