// Package obs wires OpenTelemetry: traces, metrics, and logs exported over OTLP.
// It is entirely config-driven and opt-in — with no OTLP endpoint configured the
// process runs unchanged and logs only to stdout/stderr (Docker output). When an
// endpoint is set, the standard library logger is teed so every log line goes to
// BOTH stdout and the OTLP log pipeline, and HTTP handlers/clients emit spans and
// metrics via the global providers (see cmd/* wiring with otelhttp).
//
// Configuration uses the standard OTEL_* environment variables read by the SDK
// (OTEL_EXPORTER_OTLP_ENDPOINT, _HEADERS, _PROTOCOL, OTEL_SERVICE_NAME, ...).
package obs

import (
	"context"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Enabled reports whether OTLP export is configured. Without it, Setup is a no-op
// and the process keeps its plain stdout logging.
func Enabled() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_ENABLED") == "true"
}

// Setup installs global tracer/meter/logger providers backed by OTLP exporters
// and tees the stdlib logger to OTLP logs (keeping stdout). It returns a shutdown
// func to flush and close the pipelines. When OTLP is not configured it returns a
// no-op shutdown and leaves logging untouched.
func Setup(ctx context.Context, service string) (func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	if !Enabled() {
		return noop, nil
	}

	// The explicit service name is the default; OTEL_SERVICE_NAME (via
	// WithFromEnv, applied last) overrides it when set.
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(service)),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, err
	}

	texp, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(texp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	mexp, err := otlpmetricgrpc.New(ctx)
	if err != nil {
		return nil, err
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(mexp)),
		sdkmetric.WithResource(res))
	otel.SetMeterProvider(mp)

	lexp, err := otlploggrpc.New(ctx)
	if err != nil {
		return nil, err
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(lexp)),
		sdklog.WithResource(res))
	global.SetLoggerProvider(lp)

	// Tee the stdlib logger: stderr (Docker output) + OTLP logs. Existing
	// log.Printf call sites are unchanged.
	log.SetOutput(io.MultiWriter(os.Stderr, &otelLogWriter{logger: lp.Logger(service)}))

	return func(ctx context.Context) error {
		log.SetOutput(os.Stderr) // stop teeing while shutting the pipeline down
		var first error
		for _, sh := range []func(context.Context) error{tp.Shutdown, mp.Shutdown, lp.Shutdown} {
			if err := sh(ctx); err != nil && first == nil {
				first = err
			}
		}
		return first
	}, nil
}

// otelLogWriter turns each stdlib log line into an OTLP log record.
type otelLogWriter struct{ logger otellog.Logger }

func (w *otelLogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	if msg != "" {
		var r otellog.Record
		r.SetTimestamp(time.Now())
		r.SetSeverity(otellog.SeverityInfo)
		r.SetBody(otellog.StringValue(msg))
		w.logger.Emit(context.Background(), r)
	}
	return len(p), nil
}
