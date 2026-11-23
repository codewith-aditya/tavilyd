// Package observability wires structured logging, Prometheus metrics, optional
// OpenTelemetry tracing, and per-stage request timings.
package observability

import (
	"context"
	"io"
	"log/slog"
	"os"
)

// NewLogger returns a JSON slog logger writing to w (os.Stdout when nil).
func NewLogger(level slog.Level, w io.Writer) *slog.Logger {
	if w == nil {
		w = os.Stdout
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:     level,
		AddSource: level <= slog.LevelDebug,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			// Never allow a raw api key to reach the logs even by accident.
			if a.Key == "api_key" || a.Key == "authorization" {
				if s, ok := a.Value.Any().(string); ok && s != "" {
					return slog.String(a.Key, "redacted")
				}
			}
			return a
		},
	})
	return slog.New(h)
}

type ctxKey struct{}

// WithLogger stores a request-scoped logger in ctx.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext returns the request-scoped logger or the default one.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}
