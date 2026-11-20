package canarynode

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

const (
	maxDeepSeekCallsPerEvent = 2
	maxDeepSeekCalls24h      = 10
)

// The AI deployment is intentionally single-replica while this hard quota is
// enforced with a check-then-insert reservation. The durable ledger survives
// restarts; the mutex closes the race between concurrent requests in the one
// process. Scaling this process horizontally requires a distributed
// reservation primitive and must not be done silently.
var deepSeekRateLimitMu sync.Mutex

type deepSeekRateLimitError struct {
	scope string
	limit int
}

func (e *deepSeekRateLimitError) Error() string {
	if e == nil {
		return "DeepSeek rate limit exceeded"
	}
	switch e.scope {
	case "event":
		return fmt.Sprintf("DeepSeek rate limit exceeded for event: maximum %d calls", e.limit)
	case "daily":
		return fmt.Sprintf("DeepSeek rate limit exceeded for 24h window: maximum %d calls", e.limit)
	default:
		return "DeepSeek rate limit exceeded"
	}
}

func deepSeekRateLimitFromError(err error) (*deepSeekRateLimitError, bool) {
	var limitError *deepSeekRateLimitError
	if !errors.As(err, &limitError) {
		return nil, false
	}
	return limitError, true
}

func deepSeekRateLimitScope(message string) string {
	message = strings.ToLower(message)
	if strings.Contains(message, "24h") || strings.Contains(message, "daily") {
		return "daily"
	}
	if strings.Contains(message, "event") {
		return "event"
	}
	return "unknown"
}

func deepSeekRateLimitLimit(scope string) int {
	if scope == "daily" {
		return maxDeepSeekCalls24h
	}
	return maxDeepSeekCallsPerEvent
}

type aiStoreContextKey struct{}

func withAIStore(ctx context.Context, store *Store) context.Context {
	return context.WithValue(ctx, aiStoreContextKey{}, store)
}

func aiStoreFromContext(ctx context.Context) *Store {
	store, _ := ctx.Value(aiStoreContextKey{}).(*Store)
	return store
}

// reserveDeepSeekCall atomically reserves one provider attempt from the
// process's durable quota. It fails closed: if ClickHouse cannot answer or
// persist the reservation, the provider is not called.
func (s *Store) reserveDeepSeekCall(ctx context.Context, organizationID, cellID, scenarioID, requestID, model string) error {
	if s == nil || s.conn == nil {
		return fmt.Errorf("DeepSeek rate ledger is unavailable")
	}
	ctx, span := tracer.Start(ctx, "ClickHouse reserve DeepSeek call",
		withAttributes(
			spanString("db.system", "clickhouse"),
			spanString("ai.provider", "deepseek"),
			spanString("ai.model", model),
			spanString("ai.scenario_id", scenarioID),
		))
	defer span.End()

	organizationID = firstNonEmpty(organizationID, s.organizationID, "local-organization")
	cellID = firstNonEmpty(cellID, s.canaryID, "local-cell")
	scenarioID = firstNonEmpty(scenarioID, "unknown-event")
	requestID = firstNonEmpty(requestID, newRequestID())

	deepSeekRateLimitMu.Lock()
	defer deepSeekRateLimitMu.Unlock()

	var eventCalls, dailyCalls uint64
	rows, err := s.conn.Query(ctx, `
SELECT
    countIf(scenario_id = {scenario_id:String}) AS event_calls,
    count() AS daily_calls
FROM canary_ai_call_ledger
WHERE organization_id = {organization_id:String}
  AND cell_id = {cell_id:String}
  AND reserved_at >= now64(3) - INTERVAL 24 HOUR`,
		clickhouse.Named("organization_id", organizationID),
		clickhouse.Named("cell_id", cellID),
		clickhouse.Named("scenario_id", scenarioID),
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "rate ledger query failed closed")
		return fmt.Errorf("query DeepSeek rate ledger: %w", err)
	}
	if rows.Next() {
		if err := rows.Scan(&eventCalls, &dailyCalls); err != nil {
			rows.Close()
			span.RecordError(err)
			span.SetStatus(spanError, "rate ledger scan failed closed")
			return fmt.Errorf("scan DeepSeek rate ledger: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		span.RecordError(err)
		span.SetStatus(spanError, "rate ledger read failed closed")
		return fmt.Errorf("read DeepSeek rate ledger: %w", err)
	}
	rows.Close()
	span.SetAttributes(spanInt("ai.event_calls", int(eventCalls)), spanInt("ai.daily_calls", int(dailyCalls)))

	if eventCalls >= maxDeepSeekCallsPerEvent {
		err := &deepSeekRateLimitError{scope: "event", limit: maxDeepSeekCallsPerEvent}
		span.SetStatus(spanError, "event rate limit exceeded")
		return err
	}
	if dailyCalls >= maxDeepSeekCalls24h {
		err := &deepSeekRateLimitError{scope: "daily", limit: maxDeepSeekCalls24h}
		span.SetStatus(spanError, "daily rate limit exceeded")
		return err
	}

	reservedAt := time.Now().UTC()
	if err := s.conn.Exec(ctx, `
INSERT INTO canary_ai_call_ledger
(organization_id, cell_id, scenario_id, request_id, provider, model, reserved_at)
VALUES ({organization_id:String}, {cell_id:String}, {scenario_id:String}, {request_id:String}, {provider:String}, {model:String}, {reserved_at:DateTime64(3)})`,
		clickhouse.Named("organization_id", organizationID),
		clickhouse.Named("cell_id", cellID),
		clickhouse.Named("scenario_id", scenarioID),
		clickhouse.Named("request_id", requestID),
		clickhouse.Named("provider", "deepseek"),
		clickhouse.Named("model", model),
		clickhouse.Named("reserved_at", reservedAt),
	); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "rate ledger reservation failed closed")
		return fmt.Errorf("reserve DeepSeek call: %w", err)
	}
	span.SetAttributes(spanInt("ai.event_calls_after_reservation", int(eventCalls+1)), spanInt("ai.daily_calls_after_reservation", int(dailyCalls+1)))
	return nil
}
