package obs

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/3th1nk/mammoth"

// SetupTracing initializes the global tracer provider. Without an OTLP
// endpoint the process stays on the SDK-no-op path: boundary instrumentation
// exists in code but costs nothing and exports nothing. With an endpoint, the
// OTLP gRPC exporter is wired up and spans flow to the collector.
func SetupTracing(ctx context.Context, endpoint string) (stop func(context.Context) error, err error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpoint(endpoint), otlptracegrpc.WithInsecure())
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// Tracer returns the process tracer. Call sites at the four instrumented
// boundaries (HTTP handler, queue consume, BMC call, render) start spans
// through this.
func Tracer() trace.Tracer {
	return otel.Tracer(tracerName)
}

// InjectTraceContext serializes the current span context into a carrier map
// so it can travel inside a queue message payload (docs/02-architecture.md
// §5.2: span context rides with queue messages across process facets).
func InjectTraceContext(ctx context.Context) map[string]string {
	carrier := make(map[string]string, 4)
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(carrier))
	if len(carrier) == 0 {
		return nil
	}
	return carrier
}

// ExtractTraceContext rebuilds a span context from a queue message payload.
func ExtractTraceContext(ctx context.Context, carrier map[string]string) context.Context {
	if len(carrier) == 0 {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(carrier))
}
