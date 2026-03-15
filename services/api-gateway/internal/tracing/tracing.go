// Package tracing provides OpenTelemetry tracer initialization for distributed tracing.
//
// This package sets up the OpenTelemetry SDK with OTLP/HTTP exporter to send traces
// to a Jaeger-compatible backend. It configures:
//   - Trace context propagation (W3C TraceContext and Baggage)
//   - Batch trace exporting with 5-second timeout
//   - Service name resource attribute
//
// Environment Variables:
//
//	OTEL_EXPORTER_OTLP_ENDPOINT - The OTLP endpoint (default: http://localhost:4318)
//
// Example usage:
//
//	shutdown, err := tracing.InitTracer("my-service")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer shutdown(context.Background())
package tracing

import (
	"context"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

// InitTracer initializes the OpenTelemetry tracer for the given service.
// Returns a shutdown function that should be called on application termination
// to flush any remaining traces.
func InitTracer(serviceName string) (func(context.Context) error, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:4318"
	}

	ctx := context.Background()

	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(extractHost(endpoint)),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(5*time.Second)),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceNameKey.String(serviceName),
		)),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp.Shutdown, nil
}

// extractHost removes the http:// or https:// prefix from the endpoint URL.
// Required because otlptracehttp expects a host:port format, not a full URL.
func extractHost(endpoint string) string {
	// Remove http:// or https:// prefix for otlptracehttp
	host := endpoint
	if len(host) > 7 && host[:7] == "http://" {
		host = host[7:]
	} else if len(host) > 8 && host[:8] == "https://" {
		host = host[8:]
	}
	return host
}
