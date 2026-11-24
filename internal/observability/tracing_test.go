package observability

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestTraceparentContinuesTrace(t *testing.T) {
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
	})

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	ctx, span := otel.Tracer("test").Start(context.Background(), "root")
	defer span.End()

	traceparent := Traceparent(ctx)
	if traceparent == "" {
		t.Fatal("traceparent is empty")
	}

	childCtx, child := ContinueTrace(context.Background(), traceparent, "child")
	defer child.End()

	parentID := span.SpanContext().TraceID()
	childID := trace.SpanContextFromContext(childCtx).TraceID()
	if parentID != childID {
		t.Errorf("child trace id = %s, want %s", childID, parentID)
	}
	if Traceparent(childCtx) == "" {
		t.Error("child traceparent is empty")
	}
}

func TestTraceparentWithoutSpanIsEmpty(t *testing.T) {
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
	})

	otel.SetTextMapPropagator(propagation.TraceContext{})
	if got := Traceparent(context.Background()); got != "" {
		t.Errorf("traceparent = %q, want empty", got)
	}
}
