package canarynode

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

type storageDiagnostics struct {
	Table            string  `json:"table"`
	RowsLastWindow   uint64  `json:"rows_last_window"`
	TenantCount      uint64  `json:"tenant_count"`
	EntityCount      uint64  `json:"entity_count"`
	SignalsPerSecond float64 `json:"signals_per_second"`
	ActiveParts      uint64  `json:"active_parts"`
	BytesOnDisk      uint64  `json:"bytes_on_disk"`
}

func (s *Store) Diagnostics(ctx context.Context, lookbackWindow time.Duration) (storageDiagnostics, error) {
	ctx, span := tracer.Start(ctx, "ClickHouse SELECT node diagnostics",
		withAttributes(spanString("db.system", "clickhouse"), spanInt64("lookback_seconds", int64(lookbackWindow.Seconds()))))
	defer span.End()
	until := time.Now().UTC()
	since := until.Add(-lookbackWindow)
	rows, err := s.conn.Query(ctx, `
SELECT count(), uniqExact(tenant_id), uniqExact(entity_id),
       (SELECT count() FROM system.parts WHERE active = 1 AND database = currentDatabase() AND table = {table:String}),
       (SELECT coalesce(sum(bytes_on_disk), 0) FROM system.parts WHERE active = 1 AND database = currentDatabase() AND table = {table:String})
FROM canary_signal_events FINAL
WHERE occurred_at >= {since:DateTime64(3)}
  AND occurred_at < {until:DateTime64(3)}
  AND ({organization_id:String} = '' OR organization_id = {organization_id:String})
  AND ({canary_id:String} = '' OR canary_id = {canary_id:String})
SETTINGS max_execution_time = 10, max_rows_to_read = 1000000000`,
		clickhouse.Named("since", since), clickhouse.Named("until", until), clickhouse.Named("table", signalEventsTable),
		clickhouse.Named("organization_id", s.organizationID), clickhouse.Named("canary_id", s.canaryID))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return storageDiagnostics{}, fmt.Errorf("read signal diagnostics: %w", err)
	}
	defer rows.Close()
	var result storageDiagnostics
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			span.RecordError(err)
			span.SetStatus(spanError, err.Error())
			return storageDiagnostics{}, err
		}
		return result, fmt.Errorf("clickhouse returned no diagnostic totals")
	}
	if err := rows.Scan(&result.RowsLastWindow, &result.TenantCount, &result.EntityCount, &result.ActiveParts, &result.BytesOnDisk); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return storageDiagnostics{}, err
	}
	if lookbackWindow > 0 {
		result.SignalsPerSecond = float64(result.RowsLastWindow) / lookbackWindow.Seconds()
	}
	span.SetAttributes(
		spanInt64("diagnostics.rows_last_window", int64(result.RowsLastWindow)),
		spanInt64("diagnostics.active_parts", int64(result.ActiveParts)),
		spanInt64("diagnostics.bytes_on_disk", int64(result.BytesOnDisk)),
	)
	result.Table = signalEventsTable
	return result, nil
}
