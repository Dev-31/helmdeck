// Package telemetry wires OpenTelemetry tracing into helmdeck (T510,
// ADR 013). It exports a thin facade over the OTel SDK so the rest
// of the codebase can call telemetry.Tracer("subsystem") without
// importing five OTel packages, and so a no-op tracer is the
// default when OTel isn't configured.
//
// What gets traced:
//
//   - LLM gateway calls — every Provider.ChatCompletion span carries
//     the OTel GenAI semantic-convention attributes (gen_ai.system,
//     gen_ai.request.model, gen_ai.usage.input_tokens, etc.).
//
//   - Pack engine executions — each pack run is one span tagged
//     with helmdeck.pack.name, helmdeck.pack.version, and the
//     final exit status.
//
//   - MCP tool calls — each tools/call invocation served by the
//     PackServer becomes a span linked to the underlying pack span.
//
// The OTLP HTTP exporter is configured via the standard OTel env
// vars (OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_SERVICE_NAME, etc.). When
// neither HELMDECK_OTEL_ENABLED=true nor OTEL_EXPORTER_OTLP_ENDPOINT
// is set, helmdeck installs a no-op tracer provider — every Tracer
// call still works but spans go nowhere, and there's no overhead
// beyond the function-call indirection.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Config drives Init. All fields are optional — empty values fall
// back to environment variables (the standard OTel set) and then to
// safe defaults.
type Config struct {
	// ServiceName populates the resource attribute that downstream
	// observability backends key on. Defaults to "helmdeck-control-plane"
	// when neither this field nor OTEL_SERVICE_NAME is set.
	ServiceName string
	// ServiceVersion is the helmdeck binary version surfaced as a
	// resource attribute. Surfaces in tools like Grafana Tempo and
	// Honeycomb so operators can correlate traces with releases.
	ServiceVersion string
	// Endpoint overrides OTEL_EXPORTER_OTLP_ENDPOINT for tests and
	// non-standard deployments. Empty = honor the env var = honor
	// the OTLP default of localhost:4318.
	Endpoint string
	// Insecure forces http:// (no TLS). Honors OTEL_EXPORTER_OTLP_INSECURE
	// when unset. Defaults to true for the local-dev path.
	Insecure bool
	// SampleRatio is the head-based sampling fraction (0..1). Zero
	// means "use OTEL_TRACES_SAMPLER_ARG or 1.0".
	SampleRatio float64
}

// Provider bundles a SDK TracerProvider with its shutdown closure.
// Init returns a Provider regardless of whether OTel is actually
// enabled — the no-op path returns a Provider whose Shutdown is a
// no-op so callers always get a clean defer.
type Provider struct {
	tp       trace.TracerProvider
	shutdown func(context.Context) error
	enabled  bool
}

// Tracer returns a named tracer scoped to the provider. Use this
// instead of calling otel.Tracer() directly so the no-op fallback
// is honored even when callers haven't installed a global provider.
func (p *Provider) Tracer(name string) trace.Tracer {
	if p == nil || p.tp == nil {
		return noop.NewTracerProvider().Tracer(name)
	}
	return p.tp.Tracer(name)
}

// Enabled reports whether the provider is wired to a real exporter.
// Useful for log lines like "OTel enabled, traces flowing to ..." at
// startup.
func (p *Provider) Enabled() bool {
	if p == nil {
		return false
	}
	return p.enabled
}

// Shutdown flushes pending spans and tears down the exporter. Safe
// to call on a nil or no-op provider.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.shutdown == nil {
		return nil
	}
	return p.shutdown(ctx)
}

// Init constructs a Provider. When OTel is disabled (no endpoint
// configured AND HELMDECK_OTEL_ENABLED is unset/"false") it returns
// a no-op Provider that's still safe to use for Tracer() calls.
//
// When OTel is enabled, Init also installs the provider as the
// global so packages that use otel.Tracer() directly (e.g. transitive
// libraries) pick it up.
func Init(ctx context.Context, cfg Config) (*Provider, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	}
	enabledFlag := strings.EqualFold(os.Getenv("HELMDECK_OTEL_ENABLED"), "true")
	if endpoint == "" && !enabledFlag {
		// Disabled — return a no-op provider so the rest of the
		// codebase can use telemetry.Tracer() unconditionally.
		return &Provider{}, nil
	}

	serviceName := cfg.ServiceName
	if serviceName == "" {
		serviceName = os.Getenv("OTEL_SERVICE_NAME")
	}
	if serviceName == "" {
		serviceName = "helmdeck-control-plane"
	}
	serviceVersion := cfg.ServiceVersion
	if serviceVersion == "" {
		serviceVersion = "dev"
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(serviceVersion),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: resource: %w", err)
	}

	opts := []otlptracehttp.Option{}
	if endpoint != "" {
		opts = append(opts, otlptracehttp.WithEndpointURL(endpoint))
	}
	if cfg.Insecure || strings.EqualFold(os.Getenv("OTEL_EXPORTER_OTLP_INSECURE"), "true") {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: otlp exporter: %w", err)
	}

	sampler := sdktrace.AlwaysSample()
	if cfg.SampleRatio > 0 && cfg.SampleRatio < 1 {
		sampler = sdktrace.TraceIDRatioBased(cfg.SampleRatio)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter,
			sdktrace.WithBatchTimeout(2*time.Second),
		),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)
	otel.SetTracerProvider(tp)

	return &Provider{
		tp:      tp,
		enabled: true,
		shutdown: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := tp.Shutdown(ctx); err != nil {
				return errors.Join(err, exporter.Shutdown(ctx))
			}
			return exporter.Shutdown(ctx)
		},
	}, nil
}

// --- GenAI semantic conventions ----------------------------------------

// GenAI is a tiny constructor for the OTel GenAI attributes
// (https://opentelemetry.io/docs/specs/semconv/gen-ai/). The
// upstream Go semconv package doesn't carry GenAI attributes yet
// as of v1.26.0, so we re-declare the keys here. When upstream
// adds them, swap to the imported constants.
var GenAI = struct {
	System          attribute.Key // openai|anthropic|gemini|ollama|deepseek
	OperationName   attribute.Key // chat|completion|embedding
	RequestModel    attribute.Key
	RequestMaxTok   attribute.Key
	RequestTemp     attribute.Key
	ResponseModel   attribute.Key
	ResponseFinish  attribute.Key
	UsageInputTok   attribute.Key
	UsageOutputTok  attribute.Key
	UsageTotalTok   attribute.Key
}{
	System:         "gen_ai.system",
	OperationName:  "gen_ai.operation.name",
	RequestModel:   "gen_ai.request.model",
	RequestMaxTok:  "gen_ai.request.max_tokens",
	RequestTemp:    "gen_ai.request.temperature",
	ResponseModel:  "gen_ai.response.model",
	ResponseFinish: "gen_ai.response.finish_reasons",
	UsageInputTok:  "gen_ai.usage.input_tokens",
	UsageOutputTok: "gen_ai.usage.output_tokens",
	UsageTotalTok:  "gen_ai.usage.total_tokens",
}

// Helmdeck holds helmdeck-specific attribute keys for spans on
// subsystems that don't have a published semconv group yet.
var Helmdeck = struct {
	PackName    attribute.Key
	PackVersion attribute.Key
	PackResult  attribute.Key
	SessionID   attribute.Key
	MCPServer   attribute.Key
	MCPTool     attribute.Key
}{
	PackName:    "helmdeck.pack.name",
	PackVersion: "helmdeck.pack.version",
	PackResult:  "helmdeck.pack.result",
	SessionID:   "helmdeck.session.id",
	MCPServer:   "helmdeck.mcp.server",
	MCPTool:     "helmdeck.mcp.tool",
}
