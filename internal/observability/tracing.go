package observability

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Tracing holds the tracer provider and its shutdown hook.
type Tracing struct {
	Provider *sdktrace.TracerProvider
	Enabled  bool
}

// SetupTracing configures an OTLP/HTTP trace exporter. When enabled is false it
// returns a no-op provider so callers never branch on nil.
func SetupTracing(ctx context.Context, enabled bool, endpoint, service string, insecure bool) (*Tracing, error) {
	if !enabled {
		tp := sdktrace.NewTracerProvider()
		otel.SetTracerProvider(tp)
		return &Tracing{Provider: tp}, nil
	}
	opts := []otlptracehttp.Option{}
	if endpoint != "" {
		opts = append(opts, otlptracehttp.WithEndpoint(endpoint))
	}
	if insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL, semconv.ServiceName(service),
	))
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(2*time.Second)),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.1))),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	return &Tracing{Provider: tp, Enabled: true}, nil
}

// Shutdown flushes pending spans with a bounded deadline.
func (t *Tracing) Shutdown(ctx context.Context) error {
	if t == nil || t.Provider == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return t.Provider.Shutdown(ctx)
}

// Tracer returns the service tracer.
func Tracer() trace.Tracer { return otel.Tracer("tavilyd") }
