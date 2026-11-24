package observability

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/zezao/ticket-checkout-system/internal/app"
)

// SetupTracing installs the global tracer provider and W3C propagator.
// Without OTEL_EXPORTER_OTLP_ENDPOINT it still generates trace ids and
// propagates traceparent, but does not export spans.
func SetupTracing(ctx context.Context, defaultServiceName string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.TraceContext{})

	noop := func(context.Context) error { return nil }
	if os.Getenv("OTEL_SDK_DISABLED") == "true" {
		return noop, nil
	}

	serviceName := defaultServiceName
	if value := os.Getenv("OTEL_SERVICE_NAME"); value != "" {
		serviceName = value
	}

	options := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
		sdktrace.WithResource(resource.NewWithAttributes(
			"",
			attribute.String("service.name", serviceName),
		)),
	}

	if endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); endpoint != "" {
		exporterOptions := make([]otlptracehttp.Option, 0, 1)
		if strings.HasPrefix(endpoint, "http://") {
			exporterOptions = append(exporterOptions, otlptracehttp.WithInsecure())
		}
		exporter, err := otlptracehttp.New(ctx, exporterOptions...)
		if err != nil {
			return nil, fmt.Errorf("create otlp exporter: %w", err)
		}
		options = append(options, sdktrace.WithBatcher(exporter))
	}

	provider := sdktrace.NewTracerProvider(options...)
	otel.SetTracerProvider(provider)
	return provider.Shutdown, nil
}

// HTTPHandler starts a server span and extracts the incoming traceparent.
func HTTPHandler(next http.Handler, operation string) http.Handler {
	return otelhttp.NewHandler(next, operation)
}

// Traceparent returns the W3C traceparent for the span in ctx, or empty.
func Traceparent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

// ContinueTrace resumes the stored traceparent and starts a child span.
func ContinueTrace(
	ctx context.Context,
	traceparent, name string,
) (context.Context, trace.Span) {
	if traceparent != "" {
		carrier := propagation.MapCarrier{}
		carrier.Set("traceparent", traceparent)
		ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)
	}
	return otel.Tracer("ticket-checkout").Start(ctx, name)
}

// TracedProvider records spans around provider calls.
type TracedProvider struct {
	next app.Provider
}

// NewTracedProvider wraps a Provider with tracing.
func NewTracedProvider(next app.Provider) *TracedProvider {
	return &TracedProvider{next: next}
}

// CreateCharge traces one charge creation.
func (p *TracedProvider) CreateCharge(
	ctx context.Context,
	req app.ChargeRequest,
) (app.ChargeResult, error) {
	ctx, span := otel.Tracer("provider").Start(ctx, "provider.create_charge",
		trace.WithAttributes(attribute.String("provider.idempotency_key", req.IdempotencyKey)),
	)
	defer span.End()

	result, err := p.next.CreateCharge(ctx, req)
	recordProviderResult(span, result, err)
	return result, err
}

// GetCharge traces one charge lookup.
func (p *TracedProvider) GetCharge(
	ctx context.Context,
	req app.ChargeRequest,
) (app.ChargeResult, error) {
	ctx, span := otel.Tracer("provider").Start(ctx, "provider.get_charge",
		trace.WithAttributes(attribute.String("provider.idempotency_key", req.IdempotencyKey)),
	)
	defer span.End()

	result, err := p.next.GetCharge(ctx, req)
	recordProviderResult(span, result, err)
	return result, err
}

func recordProviderResult(span trace.Span, result app.ChargeResult, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return
	}
	span.SetAttributes(attribute.String("provider.status", string(result.Status)))
}
