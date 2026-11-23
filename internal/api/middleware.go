package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/astralabs/tavilyd/internal/observability"
	"github.com/astralabs/tavilyd/internal/security"
	"github.com/astralabs/tavilyd/internal/usage"
)

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxAPIKey
)

// RequestIDMiddleware assigns a request id, stores it in the context and echoes
// it back in X-Request-ID.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if id == "" || len(id) > 128 {
			id = uuid.NewString()
		}
		ctx := context.WithValue(r.Context(), ctxRequestID, id)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestID extracts the request id from ctx.
func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxRequestID).(string); ok {
		return v
	}
	return ""
}

// WithAPIKey stores the authenticated key identity for rate limiting.
func WithAPIKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, ctxAPIKey, key)
}

// APIKeyFromContext returns the key identity ("anonymous" when auth is off).
func APIKeyFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxAPIKey).(string); ok && v != "" {
		return v
	}
	return "anonymous"
}

// RecoverMiddleware converts panics into 500 responses instead of killing the
// process, and logs the stack-free panic value.
func RecoverMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic recovered", "panic", rec, "path", r.URL.Path, "request_id", RequestID(r.Context()))
					writeError(w, r, Errorf(http.StatusInternalServerError, "Internal server error"))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// BodyLimitMiddleware caps request bodies; exceeding the cap yields 422.
func BodyLimitMiddleware(max int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, max)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// statusRecorder captures the status code and response size for logs/metrics.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Flush forwards flushes (needed if streaming is added later).
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// AccessLogMiddleware logs one structured line per request and feeds metrics.
func AccessLogMiddleware(log *slog.Logger, m *observability.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			if m != nil {
				m.HTTPInflight.Inc()
				defer m.HTTPInflight.Dec()
			}
			rec := &statusRecorder{ResponseWriter: w}
			rctx := observability.WithLogger(r.Context(), log.With(
				"request_id", RequestID(r.Context()), "path", r.URL.Path, "method", r.Method,
			))
			next.ServeHTTP(rec, r.WithContext(rctx))
			dur := time.Since(start)
			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			route := r.Pattern
			if route == "" {
				route = "unmatched"
			}
			fields := []any{
				"request_id", RequestID(r.Context()), "route", route, "method", r.Method,
				"status", rec.status, "bytes", rec.bytes, "duration_ms", dur.Milliseconds(),
			}
			if sid := strings.TrimSpace(r.Header.Get("X-Session-Id")); sid != "" {
				sum := sha256.Sum256([]byte(sid))
				fields = append(fields, "session", hex.EncodeToString(sum[:6]))
			}
			log.Info("http_request", fields...)
			if m != nil {
				m.ObserveRequest(route, r.Method, rec.status, dur)
			}
		})
	}
}

// RateLimitMiddleware applies the per-key token bucket.
func RateLimitMiddleware(limiter *security.RateLimiter, m *observability.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if limiter == nil || !limiter.Enabled() || isHealthPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			key := bearerToken(r)
			if key == "" {
				key = "anonymous"
			}
			ok, retry := limiter.Allow(key)
			if !ok {
				if m != nil {
					m.RateLimited.Inc()
				}
				setRetryAfter(w, int64(retry.Seconds()))
				writeError(w, r, ErrRateLimited)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func isHealthPath(p string) bool {
	return p == "/health" || p == "/livez" || p == "/readyz"
}

// bearerToken extracts "Authorization: Bearer <key>" or X-API-Key.
func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
			return strings.TrimSpace(h[7:])
		}
	}
	if k := strings.TrimSpace(r.Header.Get("X-API-Key")); k != "" {
		return k
	}
	return ""
}

// authenticate enforces the API key, accepting the legacy body api_key too.
func (s *Server) authenticate(r *http.Request, bodyKey string) error {
	if !s.auth.Enabled() {
		return nil
	}
	token := bearerToken(r)
	if token == "" {
		token = strings.TrimSpace(bodyKey)
	}
	if !s.auth.Check(token) {
		return ErrUnauthorized
	}
	return nil
}

// APIKeyContextMiddleware puts the validated key identity into the request
// context so usage counters never fall back to "anonymous".
func APIKeyContextMiddleware(auth *security.Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := ""
			if auth != nil && auth.Enabled() {
				if token := bearerToken(r); token != "" && auth.Check(token) {
					key = token
				}
			} else {
				key = "anonymous"
			}
			if key != "" {
				r = r.WithContext(WithAPIKey(r.Context(), key))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// UsageMiddleware counts authenticated requests and endpoint usage per key.
func UsageMiddleware(store *usage.Store, auth *security.Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			if store == nil || isHealthPath(r.URL.Path) || r.URL.Path == "/" || r.URL.Path == "/{$}" {
				return
			}
			key, hasKey := r.Context().Value(ctxAPIKey).(string)
			if !hasKey || key == "" {
				if auth != nil && auth.Enabled() {
					return // invalid/missing key: not billable
				}
				key = "anonymous"
			}
			d := usage.Delta{Requests: 1}
			switch {
			case r.URL.Path == "/search" || r.URL.Path == "/exa/search":
				d.Searches = 1
			case r.URL.Path == "/extract" || r.URL.Path == "/exa/contents":
				d.Extracts = 1
			case r.URL.Path == "/crawl":
				d.Crawls = 1
			case r.URL.Path == "/map":
				d.Maps = 1
			case r.URL.Path == "/research" && r.Method == http.MethodPost:
				d.Research = 1
			}
			ctx, cancel := context.WithTimeout(r.Context(), 900*time.Millisecond)
			defer cancel()
			_ = store.Record(ctx, key, d)
		})
	}
}

// Chain applies middleware in the order given (outermost first).
func Chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
