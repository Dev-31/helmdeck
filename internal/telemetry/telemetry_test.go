package telemetry

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestInit_NoOpWhenDisabled(t *testing.T) {
	t.Setenv("HELMDECK_OTEL_ENABLED", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	p, err := Init(context.Background(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Enabled() {
		t.Errorf("expected disabled provider when no OTel env is set")
	}
	// No-op tracer should still produce a valid Tracer.
	tracer := p.Tracer("x")
	if tracer == nil {
		t.Errorf("Tracer() returned nil on no-op provider")
	}
	_, span := tracer.Start(context.Background(), "test")
	span.End() // must not panic
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown on no-op should be nil, got %v", err)
	}
}

func TestInit_NilProviderTracerSafe(t *testing.T) {
	var p *Provider
	if p.Enabled() {
		t.Error("nil provider should report Enabled=false")
	}
	tracer := p.Tracer("x")
	if tracer == nil {
		t.Fatal("nil provider should still return a usable tracer")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown on nil provider should be nil, got %v", err)
	}
}

// TestSpanRecorder verifies the GenAI semantic-convention attributes
// land on a span recorded by the standard tracetest exporter. This
// is the canonical "did the instrumentation actually fire" check —
// it doesn't depend on a real OTLP collector.
func TestSpanAttributes_GenAI(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	tracer := tp.Tracer("test")
	_, span := tracer.Start(context.Background(), "gen_ai.chat")
	span.SetAttributes(
		GenAI.System.String("openai"),
		GenAI.RequestModel.String("gpt-4o"),
		GenAI.UsageInputTok.Int(100),
		GenAI.UsageOutputTok.Int(50),
	)
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	got := map[string]any{}
	for _, a := range spans[0].Attributes {
		got[string(a.Key)] = a.Value.AsInterface()
	}
	if got["gen_ai.system"] != "openai" {
		t.Errorf("system attr wrong: %v", got)
	}
	if got["gen_ai.request.model"] != "gpt-4o" {
		t.Errorf("model attr wrong: %v", got)
	}
	if got["gen_ai.usage.input_tokens"] != int64(100) {
		t.Errorf("input tokens wrong: %v", got["gen_ai.usage.input_tokens"])
	}
	if got["gen_ai.usage.output_tokens"] != int64(50) {
		t.Errorf("output tokens wrong: %v", got["gen_ai.usage.output_tokens"])
	}
}

func TestSpanAttributes_HelmdeckPack(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	tracer := tp.Tracer("test")
	_, span := tracer.Start(context.Background(), "pack.test")
	span.SetAttributes(
		Helmdeck.PackName.String("browser.screenshot_url"),
		Helmdeck.PackVersion.String("v1"),
		Helmdeck.PackResult.String("ok"),
	)
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	got := map[string]any{}
	for _, a := range spans[0].Attributes {
		got[string(a.Key)] = a.Value.AsInterface()
	}
	if got["helmdeck.pack.name"] != "browser.screenshot_url" {
		t.Errorf("pack name wrong: %v", got)
	}
	if got["helmdeck.pack.result"] != "ok" {
		t.Errorf("result wrong: %v", got)
	}
}
