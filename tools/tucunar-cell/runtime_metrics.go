package canarynode

import (
	"math"
	"runtime"
	"sync/atomic"
	"time"
)

// runtimeMetrics is deliberately limited to process health telemetry. It is
// not business state: incidents, deduplication and recovery remain durable in
// ClickHouse. The values reset with the process and are safe to expose through
// the local diagnostics endpoint.
type runtimeMetrics struct {
	startedAt time.Time

	requests      atomic.Uint64
	requestErrors atomic.Uint64
	insertCalls   atomic.Uint64
	insertSignals atomic.Uint64
	evaluatorRuns atomic.Uint64
	outboxPending atomic.Uint64

	insertBuckets    [6]atomic.Uint64
	evaluatorBuckets [6]atomic.Uint64
}

var nodeRuntimeMetrics = newRuntimeMetrics()

func newRuntimeMetrics() *runtimeMetrics {
	return &runtimeMetrics{startedAt: time.Now().UTC()}
}

func (m *runtimeMetrics) recordRequest(status int) {
	if m == nil {
		return
	}
	m.requests.Add(1)
	if status >= 500 {
		m.requestErrors.Add(1)
	}
}

func (m *runtimeMetrics) recordInsert(signalCount int, duration time.Duration) {
	if m == nil {
		return
	}
	m.insertCalls.Add(1)
	if signalCount > 0 {
		m.insertSignals.Add(uint64(signalCount))
	}
	addDurationBucket(&m.insertBuckets, duration)
}

func (m *runtimeMetrics) recordEvaluator(duration time.Duration) {
	if m == nil {
		return
	}
	m.evaluatorRuns.Add(1)
	addDurationBucket(&m.evaluatorBuckets, duration)
}

func (m *runtimeMetrics) recordOutboxPending(count int) {
	if m == nil || count < 0 {
		return
	}
	m.outboxPending.Store(uint64(count))
}

// Buckets are cumulative enough for a useful p95 without retaining request
// data. The final bucket is the overflow bucket.
func addDurationBucket(buckets *[6]atomic.Uint64, duration time.Duration) {
	thresholds := [...]time.Duration{time.Millisecond, 5 * time.Millisecond, 25 * time.Millisecond, 100 * time.Millisecond, 500 * time.Millisecond}
	for index, threshold := range thresholds {
		if duration <= threshold {
			buckets[index].Add(1)
			return
		}
	}
	buckets[len(buckets)-1].Add(1)
}

func percentileMillis(buckets [6]uint64, percentile float64) float64 {
	total := uint64(0)
	for _, count := range buckets {
		total += count
	}
	if total == 0 {
		return 0
	}
	target := uint64(math.Ceil(float64(total) * percentile))
	if target < 1 {
		target = 1
	}
	cumulative := uint64(0)
	thresholds := [...]float64{1, 5, 25, 100, 500, 1000}
	for index, count := range buckets {
		cumulative += count
		if cumulative >= target {
			return thresholds[index]
		}
	}
	return thresholds[len(thresholds)-1]
}

type runtimeMetricsSnapshot struct {
	UptimeSeconds       int64   `json:"uptime_seconds"`
	Requests            uint64  `json:"requests"`
	RequestErrors       uint64  `json:"request_errors_5xx"`
	InsertCalls         uint64  `json:"insert_calls"`
	InsertSignals       uint64  `json:"insert_signals"`
	InsertP95Millis     float64 `json:"insert_p95_ms"`
	EvaluatorRuns       uint64  `json:"evaluator_runs"`
	EvaluatorP95Millis  float64 `json:"evaluator_p95_ms"`
	OutboxPendingSample uint64  `json:"outbox_pending_sample"`
	Goroutines          int     `json:"goroutines"`
	MemoryAllocBytes    uint64  `json:"memory_alloc_bytes"`
	MemorySysBytes      uint64  `json:"memory_sys_bytes"`
}

func (m *runtimeMetrics) snapshot() runtimeMetricsSnapshot {
	if m == nil {
		return runtimeMetricsSnapshot{}
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	insertBuckets := [6]uint64{}
	evaluatorBuckets := [6]uint64{}
	for index := range insertBuckets {
		insertBuckets[index] = m.insertBuckets[index].Load()
		evaluatorBuckets[index] = m.evaluatorBuckets[index].Load()
	}
	return runtimeMetricsSnapshot{
		UptimeSeconds:       int64(time.Since(m.startedAt).Seconds()),
		Requests:            m.requests.Load(),
		RequestErrors:       m.requestErrors.Load(),
		InsertCalls:         m.insertCalls.Load(),
		InsertSignals:       m.insertSignals.Load(),
		InsertP95Millis:     percentileMillis(insertBuckets, 0.95),
		EvaluatorRuns:       m.evaluatorRuns.Load(),
		EvaluatorP95Millis:  percentileMillis(evaluatorBuckets, 0.95),
		OutboxPendingSample: m.outboxPending.Load(),
		Goroutines:          runtime.NumGoroutine(),
		MemoryAllocBytes:    memory.Alloc,
		MemorySysBytes:      memory.Sys,
	}
}
