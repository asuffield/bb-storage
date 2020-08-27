package util

import (
	"context"

	oc "go.opencensus.io/trace"
	"go.opentelemetry.io/otel/api/global"
	"go.opentelemetry.io/otel/api/propagation"
	otel "go.opentelemetry.io/otel/api/trace"
	"go.opentelemetry.io/otel/codes"
	"google.golang.org/grpc/status"
)

// PropagateOtelToOpencensus arranges for spans that let us trace into libraries using opencensus.
func PropagateOtelToOpencensus(ctx context.Context) context.Context {
	span := otel.SpanFromContext(ctx)

	// In oc we have to create a new span immediately, which replaces any lingering otel span
	ctx, _ = oc.StartSpanWithRemoteParent(ctx, "opentelemetry-propagation", oc.SpanContext{
		TraceID:      oc.TraceID(span.SpanContext().TraceID),
		SpanID:       oc.SpanID(span.SpanContext().SpanID),
		TraceOptions: oc.TraceOptions(span.SpanContext().TraceFlags),
	})
	return ctx
}

// RecordError injects an error event and status code into a trace span, if there is one.
func RecordError(ctx context.Context, err error) {
	if err == nil {
		return
	}
	otel.SpanFromContext(ctx).RecordError(ctx, err, otel.WithErrorStatus(codes.Code(status.Code(err))))
}

// Implement go.opentelemetry.io/otel/api/propagation.HTTPSupplier
type mapSupplier map[string]string

func (ms mapSupplier) Get(key string) string {
	return ms[key]
}

func (ms mapSupplier) Set(key string, value string) {
	ms[key] = value
}

// PropagateInjectMap runs HTTP context injection into a map type.
func PropagateInjectMap(ctx context.Context, data map[string]string) {
	propagation.InjectHTTP(ctx, global.Propagators(), mapSupplier(data))
}

// PropagateExtractMap runs HTTP context extraction from a map type.
func PropagateExtractMap(ctx context.Context, data map[string]string) context.Context {
	return propagation.ExtractHTTP(ctx, global.Propagators(), mapSupplier(data))
}
