package main

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

var tracer = otel.Tracer("edge-proxy")

func setupTracing(ctx context.Context, cfg TracingConfig) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	if !cfg.Enabled {
		provider := sdktrace.NewTracerProvider()
		otel.SetTracerProvider(provider)
		tracer = provider.Tracer("edge-proxy")
		return provider.Shutdown, nil
	}

	exporterOpts := []otlptracehttp.Option{}
	if endpoint := strings.TrimSpace(cfg.Endpoint); endpoint != "" {
		if parsed, err := url.Parse(endpoint); err == nil && parsed.Host != "" {
			exporterOpts = append(exporterOpts, otlptracehttp.WithEndpoint(parsed.Host))
			if parsed.Scheme == "http" {
				exporterOpts = append(exporterOpts, otlptracehttp.WithInsecure())
			}
		} else {
			exporterOpts = append(exporterOpts, otlptracehttp.WithEndpoint(endpoint))
		}
	}
	if cfg.Insecure {
		exporterOpts = append(exporterOpts, otlptracehttp.WithInsecure())
	}
	exporter, err := otlptracehttp.New(ctx, exporterOpts...)
	if err != nil {
		return nil, err
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			attribute.String("service.version", version),
		),
	)
	if err != nil {
		return nil, err
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
	)
	otel.SetTracerProvider(provider)
	tracer = provider.Tracer("edge-proxy")
	return provider.Shutdown, nil
}

func startIngressSpan(req *http.Request, requestID string) (context.Context, trace.Span) {
	ctx := otel.GetTextMapPropagator().Extract(context.Background(), propagation.HeaderCarrier(req.Header))
	attrs := []attribute.KeyValue{
		attribute.String("http.method", req.Method),
		attribute.String("http.target", req.URL.Path),
		attribute.String("edge.request_id", requestID),
	}
	return tracer.Start(ctx, "edge-proxy ingress", trace.WithAttributes(attrs...))
}

func injectTraceHeaders(ctx context.Context, headers http.Header) {
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(headers))
}

func startUpstreamSpan(ctx context.Context, backend string) (context.Context, trace.Span) {
	return tracer.Start(ctx, "edge-proxy upstream",
		trace.WithAttributes(attribute.String("edge.backend", backend)),
	)
}
