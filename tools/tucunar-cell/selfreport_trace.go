package canarynode

import (
	"context"

	selfreport "tucunar-cell/internal/selfreport"
)

// tracer is an in-process context only. It does not export spans or require
// OpenTelemetry; the public operational surface is /status and structured
// process logs.
var tracer = selfreport.NewTracer("tucunar-cell")

type span = selfreport.Span
type spanStartOption = selfreport.StartOption
type spanStartEventOption = selfreport.StartEventOption

var spanError = selfreport.Error

func withAttributes(attributes ...selfreport.Attribute) selfreport.StartOption {
	return selfreport.WithAttributes(attributes...)
}
func spanFromContext(ctx context.Context) *selfreport.Span   { return selfreport.SpanFromContext(ctx) }
func spanString(key, value string) selfreport.Attribute      { return selfreport.String(key, value) }
func spanInt(key string, value int) selfreport.Attribute     { return selfreport.Int(key, value) }
func spanInt64(key string, value int64) selfreport.Attribute { return selfreport.Int64(key, value) }
func spanFloat64(key string, value float64) selfreport.Attribute {
	return selfreport.Float64(key, value)
}
func spanBool(key string, value bool) selfreport.Attribute { return selfreport.Bool(key, value) }
