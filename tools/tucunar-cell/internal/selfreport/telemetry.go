// Package selfreport provides the small in-process context used by the cell's
// own diagnostics. It intentionally has no exporter, collector, network
// protocol, or external dependency.
package selfreport

import (
	"context"
	"sync"
	"time"
)

type Attribute struct {
	Key   string
	Value any
}

type StartOption func(*Span)
type StartEventOption = StartOption

type Tracer struct {
	service string
}

func NewTracer(service string) Tracer { return Tracer{service: service} }

type Span struct {
	name      string
	service   string
	startedAt time.Time

	mu          sync.Mutex
	status      string
	description string
	err         error
	attributes  map[string]any
}

type spanContextKey struct{}

func (t Tracer) Start(ctx context.Context, name string, options ...StartOption) (context.Context, *Span) {
	span := &Span{name: name, service: t.service, startedAt: time.Now()}
	for _, option := range options {
		if option != nil {
			option(span)
		}
	}
	return context.WithValue(ctx, spanContextKey{}, span), span
}

func SpanFromContext(ctx context.Context) *Span {
	if span, ok := ctx.Value(spanContextKey{}).(*Span); ok && span != nil {
		return span
	}
	return &Span{}
}

func (s *Span) End() {}

func (s *Span) RecordError(err error) {
	if s == nil || err == nil {
		return
	}
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

func (s *Span) SetStatus(status, description string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.status, s.description = status, description
	s.mu.Unlock()
}

func (s *Span) SetAttributes(attributes ...Attribute) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.attributes == nil {
		s.attributes = make(map[string]any, len(attributes))
	}
	for _, attribute := range attributes {
		s.attributes[attribute.Key] = attribute.Value
	}
	s.mu.Unlock()
}

func WithAttributes(attributes ...Attribute) StartOption {
	return func(span *Span) { span.SetAttributes(attributes...) }
}

func String(key, value string) Attribute          { return Attribute{Key: key, Value: value} }
func Int(key string, value int) Attribute         { return Attribute{Key: key, Value: value} }
func Int64(key string, value int64) Attribute     { return Attribute{Key: key, Value: value} }
func Float64(key string, value float64) Attribute { return Attribute{Key: key, Value: value} }
func Bool(key string, value bool) Attribute       { return Attribute{Key: key, Value: value} }

const Error = "error"
