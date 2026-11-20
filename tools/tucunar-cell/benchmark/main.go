// Command benchmark measures bounded generic-signal ingestion through the
// public HTTP batch endpoint. It is intentionally a test tool: it never runs
// indefinitely and reports the benchmark client's memory, not the node's.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	selfreport "tucunar-cell/internal/selfreport"
)

var benchmarkTracer = selfreport.NewTracer("canary-benchmark")

type benchmarkSignal struct {
	ExternalID string    `json:"external_id"`
	Source     string    `json:"source"`
	TenantID   string    `json:"tenant_id"`
	EntityID   string    `json:"entity_id"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	Value      float64   `json:"value"`
	OccurredAt time.Time `json:"occurred_at"`
}

type batchRequest struct {
	Signals []benchmarkSignal `json:"signals"`
}

type benchmarkConfig struct {
	baseURL            string
	token              string
	duration           time.Duration
	batchSize          int
	workers            int
	queueSize          int
	timeout            time.Duration
	clickHouseURL      string
	clickHouseUser     string
	clickHousePassword string
}

type benchmarkResult struct {
	TargetSignalsPerSecond   float64 `json:"target_signals_per_second"`
	BatchSize                int     `json:"batch_size"`
	Workers                  int     `json:"workers"`
	TrafficDurationSeconds   float64 `json:"traffic_duration_seconds"`
	TotalDurationSeconds     float64 `json:"total_duration_seconds"`
	RequestedSignals         uint64  `json:"requested_signals"`
	AcceptedSignals          uint64  `json:"accepted_signals"`
	FailedRequests           uint64  `json:"failed_requests"`
	AchievedSignalsPerSecond float64 `json:"achieved_signals_per_second"`
	P50Milliseconds          float64 `json:"p50_ms"`
	P95Milliseconds          float64 `json:"p95_ms"`
	P99Milliseconds          float64 `json:"p99_ms"`
	ClientHeapMaxBytes       uint64  `json:"client_heap_max_bytes"`
	ActiveParts              uint64  `json:"active_parts,omitempty"`
	ClickHouseBytesOnDisk    uint64  `json:"clickhouse_bytes_on_disk,omitempty"`
}

type benchmarkRun struct {
	client    *http.Client
	config    benchmarkConfig
	runID     string
	jobs      chan []byte
	latencies latencyRecorder
	accepted  atomic.Uint64
	failed    atomic.Uint64
	maxHeap   atomic.Uint64
}

type latencyRecorder struct {
	mu     sync.Mutex
	values []time.Duration
}

func main() {
	rates := flag.String("rates", "100,1000,10000", "comma-separated target signal rates")
	baseURL := flag.String("base-url", "http://127.0.0.1:18080", "Tucunar node base URL")
	token := flag.String("token", os.Getenv("SIGNAL_INGEST_TOKEN"), "generic ingest token")
	duration := flag.Duration("duration", 10*time.Second, "traffic duration per rate")
	batchSize := flag.Int("batch-size", 100, "signals per HTTP batch")
	workers := flag.Int("workers", 64, "concurrent HTTP workers")
	queueSize := flag.Int("queue-size", 256, "bounded HTTP request queue")
	timeout := flag.Duration("timeout", 15*time.Second, "HTTP request timeout")
	clickHouseURL := flag.String("clickhouse-http-url", os.Getenv("CLICKHOUSE_HTTP_URL"), "optional ClickHouse HTTP URL for active-parts/storage measurement")
	clickHouseUser := flag.String("clickhouse-user", envOrDefault("CLICKHOUSE_USER", "default"), "ClickHouse HTTP user")
	clickHousePassword := flag.String("clickhouse-password", os.Getenv("CLICKHOUSE_PASSWORD"), "ClickHouse HTTP password")
	flag.Parse()

	parsedRates, err := parseRates(*rates)
	if err != nil {
		log.Fatal(err)
	}
	if *duration <= 0 || *batchSize <= 0 || *batchSize > 1000 || *workers <= 0 || *queueSize <= 0 || *timeout <= 0 {
		log.Fatal("duration, workers, queue-size and timeout must be > 0; batch-size must be between 1 and 1000")
	}
	config := benchmarkConfig{
		baseURL: strings.TrimRight(strings.TrimSpace(*baseURL), "/"), token: strings.TrimSpace(*token),
		duration: *duration, batchSize: *batchSize, workers: *workers, queueSize: *queueSize, timeout: *timeout,
		clickHouseURL: strings.TrimRight(strings.TrimSpace(*clickHouseURL), "/"), clickHouseUser: strings.TrimSpace(*clickHouseUser), clickHousePassword: *clickHousePassword,
	}
	if config.baseURL == "" {
		log.Fatal("base-url must not be empty")
	}

	results := make([]benchmarkResult, 0, len(parsedRates))
	for _, rate := range parsedRates {
		result, err := run(context.Background(), config, rate)
		if err != nil {
			log.Fatal(err)
		}
		results = append(results, result)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(results); err != nil {
		log.Fatal(err)
	}
}

func parseRates(raw string) ([]float64, error) {
	parts := strings.Split(raw, ",")
	if len(parts) == 0 {
		return nil, fmt.Errorf("rates must not be empty")
	}
	rates := make([]float64, 0, len(parts))
	for _, part := range parts {
		rate, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil || rate <= 0 || math.IsInf(rate, 0) || math.IsNaN(rate) {
			return nil, fmt.Errorf("invalid rate %q", part)
		}
		rates = append(rates, rate)
	}
	return rates, nil
}

func run(parent context.Context, config benchmarkConfig, rate float64) (benchmarkResult, error) {
	result := benchmarkResult{TargetSignalsPerSecond: rate, BatchSize: config.batchSize, Workers: config.workers}
	ctx, cancel := context.WithTimeout(parent, config.duration+config.timeout+30*time.Second)
	defer cancel()

	run := &benchmarkRun{
		client: &http.Client{Timeout: config.timeout},
		config: config,
		runID:  fmt.Sprintf("benchmark-%d", time.Now().UnixNano()),
		jobs:   make(chan []byte, config.queueSize),
	}
	var workers sync.WaitGroup
	workers.Add(config.workers)
	for i := 0; i < config.workers; i++ {
		go run.worker(ctx, &workers)
	}

	trafficStarted := time.Now()
	deadline := trafficStarted.Add(config.duration)
	period := time.Duration(float64(time.Second) * float64(config.batchSize) / rate)
	if period <= 0 {
		period = time.Nanosecond
	}
	var requested uint64
	for next := trafficStarted; time.Now().Before(deadline); next = next.Add(period) {
		if err := waitUntil(ctx, next); err != nil {
			close(run.jobs)
			workers.Wait()
			return result, err
		}
		body, err := encodeBatch(run.runID, rate, requested, config.batchSize)
		if err != nil {
			close(run.jobs)
			workers.Wait()
			return result, fmt.Errorf("encode batch: %w", err)
		}
		select {
		case <-ctx.Done():
			close(run.jobs)
			workers.Wait()
			return result, ctx.Err()
		case run.jobs <- body:
			requested += uint64(config.batchSize)
		}
	}
	trafficDuration := time.Since(trafficStarted)
	close(run.jobs)
	workers.Wait()
	totalDuration := time.Since(trafficStarted)

	result.TrafficDurationSeconds = trafficDuration.Seconds()
	result.TotalDurationSeconds = totalDuration.Seconds()
	result.RequestedSignals = requested
	result.AcceptedSignals = run.accepted.Load()
	result.FailedRequests = run.failed.Load()
	if totalDuration > 0 {
		result.AchievedSignalsPerSecond = float64(result.AcceptedSignals) / totalDuration.Seconds()
	}
	result.P50Milliseconds, result.P95Milliseconds, result.P99Milliseconds = run.latencies.percentiles()
	result.ClientHeapMaxBytes = run.maxHeap.Load()
	if config.clickHouseURL != "" {
		parts, bytesOnDisk, err := readClickHouseParts(ctx, config)
		if err != nil {
			return result, err
		}
		result.ActiveParts = parts
		result.ClickHouseBytesOnDisk = bytesOnDisk
	}
	return result, nil
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func readClickHouseParts(ctx context.Context, config benchmarkConfig) (uint64, uint64, error) {
	requestCtx, span := benchmarkTracer.Start(ctx, "HTTP POST ClickHouse benchmark diagnostics",
		withAttributes(spanString("http.method", http.MethodPost), spanString("http.target", "/")))
	defer span.End()
	query := `SELECT count() AS active_parts, coalesce(sum(bytes_on_disk), 0) AS bytes_on_disk FROM system.parts WHERE active = 1 AND database = currentDatabase() AND table IN ('canary_signal_events', 'canary_incidents', 'canary_cloud_outbox') SETTINGS output_format_json_quote_64bit_integers=0 FORMAT JSONEachRow`
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, config.clickHouseURL, strings.NewReader(query))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return 0, 0, err
	}
	if config.clickHouseUser != "" {
		req.SetBasicAuth(config.clickHouseUser, config.clickHousePassword)
	}
	client := &http.Client{Timeout: config.timeout}
	response, err := client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return 0, 0, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		err := fmt.Errorf("ClickHouse diagnostics returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return 0, 0, err
	}
	var result struct {
		ActiveParts uint64 `json:"active_parts"`
		BytesOnDisk uint64 `json:"bytes_on_disk"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return 0, 0, err
	}
	span.SetAttributes(spanInt64("clickhouse.active_parts", int64(result.ActiveParts)), spanInt64("clickhouse.bytes_on_disk", int64(result.BytesOnDisk)))
	return result.ActiveParts, result.BytesOnDisk, nil
}

func waitUntil(ctx context.Context, target time.Time) error {
	delay := time.Until(target)
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func encodeBatch(runID string, rate float64, offset uint64, size int) ([]byte, error) {
	now := time.Now().UTC()
	signals := make([]benchmarkSignal, size)
	for i := range signals {
		sequence := offset + uint64(i)
		signals[i] = benchmarkSignal{
			ExternalID: fmt.Sprintf("%s-%.0f-%d", runID, rate, sequence), Source: "benchmark",
			TenantID: "benchmark", EntityID: "load-test", Kind: "measurement", Name: "throughput",
			Value: 1, OccurredAt: now,
		}
	}
	return json.Marshal(batchRequest{Signals: signals})
}

func (r *benchmarkRun) worker(ctx context.Context, workers *sync.WaitGroup) {
	defer workers.Done()
	for body := range r.jobs {
		started := time.Now()
		requestCtx, span := benchmarkTracer.Start(ctx, "HTTP POST /v1/signals/batch",
			withAttributes(spanInt("signal.count", r.config.batchSize)))
		req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, r.config.baseURL+"/v1/signals/batch", bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			if r.config.token != "" {
				req.Header.Set("Authorization", "Bearer "+r.config.token)
			}
		}
		if err == nil {
			var response *http.Response
			response, err = r.client.Do(req)
			if err == nil {
				response.Body.Close()
				span.SetAttributes(spanInt("http.status_code", response.StatusCode))
				if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
					err = fmt.Errorf("HTTP %d", response.StatusCode)
				}
			}
		}
		elapsed := time.Since(started)
		r.latencies.add(elapsed)
		if err != nil {
			r.failed.Add(1)
			span.RecordError(err)
			span.SetStatus(spanError, err.Error())
		} else {
			r.accepted.Add(uint64(r.config.batchSize))
		}
		span.End()
		r.sampleHeap()
	}
}

func (r *benchmarkRun) sampleHeap() {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	for {
		previous := r.maxHeap.Load()
		if stats.HeapAlloc <= previous || r.maxHeap.CompareAndSwap(previous, stats.HeapAlloc) {
			return
		}
	}
}

func (r *latencyRecorder) add(value time.Duration) {
	r.mu.Lock()
	r.values = append(r.values, value)
	r.mu.Unlock()
}

func (r *latencyRecorder) percentiles() (float64, float64, float64) {
	r.mu.Lock()
	values := append([]time.Duration(nil), r.values...)
	r.mu.Unlock()
	if len(values) == 0 {
		return 0, 0, 0
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return percentileMilliseconds(values, 0.50), percentileMilliseconds(values, 0.95), percentileMilliseconds(values, 0.99)
}

func percentileMilliseconds(values []time.Duration, percentile float64) float64 {
	index := int(math.Ceil(percentile*float64(len(values)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(values) {
		index = len(values) - 1
	}
	return float64(values[index]) / float64(time.Millisecond)
}
