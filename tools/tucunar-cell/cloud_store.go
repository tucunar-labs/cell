package canarynode

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

const cloudSchema = `
CREATE TABLE IF NOT EXISTS canary_cloud_outbox (
    outbox_id UUID,
    node_id UUID DEFAULT toUUID('00000000-0000-0000-0000-000000000000'),
    operation LowCardinality(String),
    path String,
    idempotency_key String,
    payload String,
    status LowCardinality(String) DEFAULT 'pending',
    attempts UInt32 DEFAULT 0,
    next_attempt_at DateTime64(3, 'UTC'),
    last_error String DEFAULT '',
    sent_at DateTime64(3, 'UTC') DEFAULT toDateTime64('1970-01-01 00:00:00', 3, 'UTC'),
    version UInt64,
    created_at DateTime64(3, 'UTC') DEFAULT now64(3)
) ENGINE = ReplacingMergeTree(version)
PARTITION BY toYYYYMM(created_at)
ORDER BY (node_id, outbox_id)
	TTL toDateTime(created_at) + INTERVAL 30 DAY DELETE WHERE status != 'pending'
;
`

const unixEpoch = "toDateTime64('1970-01-01 00:00:00', 3, 'UTC')"

type cloudOutboxRecord struct {
	OutboxID       string
	NodeID         string
	Operation      string
	Path           string
	IdempotencyKey string
	Payload        string
	Status         string
	Attempts       uint32
	NextAttemptAt  time.Time
}

type cloudOutboxDiagnostics struct {
	NodeID  string `json:"node_id"`
	Pending uint64 `json:"pending"`
	Failed  uint64 `json:"failed"`
}

func (s *Store) InitCloudSchema(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "ClickHouse DDL cloud reporting", withAttributes(spanString("db.system", "clickhouse")))
	defer span.End()
	statements := strings.Split(cloudSchema, ";")
	for _, statement := range statements {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			continue
		}
		if err := s.conn.Exec(ctx, statement); err != nil {
			span.RecordError(err)
			span.SetStatus(spanError, err.Error())
			return fmt.Errorf("apply cloud schema: %w", err)
		}
	}
	return nil
}

func (s *Store) EnqueueCloudMessage(ctx context.Context, record cloudOutboxRecord, maxPending int) error {
	ctx, span := tracer.Start(ctx, "ClickHouse INSERT cloud outbox",
		withAttributes(spanString("db.system", "clickhouse"), spanString("cloud.operation", record.Operation)))
	defer span.End()
	if record.OutboxID == "" || !validUUID(record.NodeID) || record.Operation == "" || record.Path == "" || record.IdempotencyKey == "" || record.Payload == "" {
		return fmt.Errorf("cloud outbox record is incomplete")
	}
	if maxPending < 1 {
		return fmt.Errorf("cloud outbox limit must be positive")
	}
	var existingPayload, existingStatus string
	existingRows, err := s.conn.Query(ctx, `
SELECT argMax(payload, version), argMax(status, version)
FROM canary_cloud_outbox
WHERE node_id = toUUID({node_id:String}) AND operation = {operation:String} AND idempotency_key = {idempotency_key:String}
GROUP BY operation, idempotency_key`, clickhouse.Named("node_id", record.NodeID), clickhouse.Named("operation", record.Operation), clickhouse.Named("idempotency_key", record.IdempotencyKey))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	existing := existingRows.Next()
	if existing {
		err = existingRows.Scan(&existingPayload, &existingStatus)
	}
	existingRows.Close()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	if existing {
		if existingPayload != record.Payload {
			return fmt.Errorf("cloud idempotency key reused with a different body")
		}
		if existingStatus == "pending" || existingStatus == "sent" {
			return nil
		}
		return fmt.Errorf("cloud idempotency key is permanently failed")
	}
	var pending uint64
	countRows, err := s.conn.Query(ctx, `
SELECT count()
FROM (
    SELECT argMax(status, version) AS status, argMax(sent_at, version) AS sent_at
    FROM canary_cloud_outbox
    WHERE node_id = toUUID({node_id:String})
    GROUP BY outbox_id
    HAVING status = 'pending' AND sent_at = `+unixEpoch+`
)
`, clickhouse.Named("node_id", record.NodeID))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	if countRows.Next() {
		err = countRows.Scan(&pending)
	} else {
		err = countRows.Err()
	}
	countRows.Close()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	if pending >= uint64(maxPending) {
		return fmt.Errorf("cloud outbox reached %d pending records", maxPending)
	}
	now := time.Now().UTC()
	if record.NextAttemptAt.IsZero() {
		record.NextAttemptAt = now
	}
	if record.Attempts == 0 {
		record.Attempts = 0
	}
	if err := s.conn.Exec(ctx, `INSERT INTO canary_cloud_outbox
        (outbox_id, node_id, operation, path, idempotency_key, payload, status, attempts, next_attempt_at, last_error, sent_at, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.OutboxID, record.NodeID, record.Operation, record.Path, record.IdempotencyKey, record.Payload,
		"pending", record.Attempts, record.NextAttemptAt, "", time.Unix(0, 0).UTC(), uint64(now.UnixNano())); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	return nil
}

func (s *Store) PendingCloudMessages(ctx context.Context, nodeID string, limit int) ([]cloudOutboxRecord, error) {
	ctx, span := tracer.Start(ctx, "ClickHouse SELECT cloud outbox",
		withAttributes(spanString("db.system", "clickhouse"), spanInt("cloud.limit", limit)))
	defer span.End()
	if !validUUID(nodeID) {
		return nil, fmt.Errorf("cloud node id is invalid")
	}
	if limit < 1 {
		return nil, fmt.Errorf("cloud outbox limit must be positive")
	}
	rows, err := s.conn.Query(ctx, `
SELECT
    argMax(node_id, version),
    outbox_id,
    argMax(operation, version),
    argMax(path, version),
    argMax(idempotency_key, version),
    argMax(payload, version),
    argMax(status, version),
    argMax(attempts, version),
    argMax(next_attempt_at, version)
FROM canary_cloud_outbox
WHERE node_id = toUUID({node_id:String})
GROUP BY outbox_id
HAVING argMax(status, version) = 'pending'
   AND argMax(sent_at, version) = `+unixEpoch+`
   AND argMax(next_attempt_at, version) <= now64(3)
ORDER BY argMax(next_attempt_at, version) ASC
LIMIT `+strconv.Itoa(limit), clickhouse.Named("node_id", nodeID))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return nil, err
	}
	defer rows.Close()
	result := make([]cloudOutboxRecord, 0, limit)
	for rows.Next() {
		var record cloudOutboxRecord
		if err := rows.Scan(&record.NodeID, &record.OutboxID, &record.Operation, &record.Path, &record.IdempotencyKey, &record.Payload, &record.Status, &record.Attempts, &record.NextAttemptAt); err != nil {
			span.RecordError(err)
			span.SetStatus(spanError, err.Error())
			return nil, err
		}
		result = append(result, record)
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return nil, err
	}
	return result, nil
}

func (s *Store) CloudOutboxDiagnostics(ctx context.Context, nodeID string) (cloudOutboxDiagnostics, error) {
	ctx, span := tracer.Start(ctx, "ClickHouse SELECT cloud outbox diagnostics",
		withAttributes(spanString("db.system", "clickhouse")))
	defer span.End()
	if !validUUID(nodeID) {
		return cloudOutboxDiagnostics{}, fmt.Errorf("cloud node id is invalid")
	}
	rows, err := s.conn.Query(ctx, `
SELECT
    countIf(status = 'pending' AND sent_at = `+unixEpoch+`),
    countIf(status = 'failed')
FROM (
    SELECT argMax(status, version) AS status, argMax(sent_at, version) AS sent_at
    FROM canary_cloud_outbox
    WHERE node_id = toUUID({node_id:String})
    GROUP BY outbox_id
)`, clickhouse.Named("node_id", nodeID))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return cloudOutboxDiagnostics{}, err
	}
	defer rows.Close()
	result := cloudOutboxDiagnostics{NodeID: nodeID}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			span.RecordError(err)
			span.SetStatus(spanError, err.Error())
			return cloudOutboxDiagnostics{}, err
		}
		return result, nil
	}
	if err := rows.Scan(&result.Pending, &result.Failed); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return cloudOutboxDiagnostics{}, err
	}
	span.SetAttributes(spanInt64("outbox.pending", int64(result.Pending)), spanInt64("outbox.failed", int64(result.Failed)))
	return result, nil
}

func (s *Store) updateCloudMessage(ctx context.Context, record cloudOutboxRecord, status string, sentAt time.Time, nextAttemptAt time.Time, lastError string) error {
	version := uint64(time.Now().UnixNano())
	return s.conn.Exec(ctx, `INSERT INTO canary_cloud_outbox
        (outbox_id, node_id, operation, path, idempotency_key, payload, status, attempts, next_attempt_at, last_error, sent_at, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, record.OutboxID, record.NodeID, record.Operation, record.Path,
		record.IdempotencyKey, record.Payload, status, record.Attempts, nextAttemptAt, lastError, sentAt, version)
}

func (s *Store) MarkCloudMessageSent(ctx context.Context, record cloudOutboxRecord) error {
	ctx, span := tracer.Start(ctx, "ClickHouse UPDATE cloud outbox sent", withAttributes(spanString("db.system", "clickhouse")))
	defer span.End()
	return s.updateCloudMessage(ctx, record, "sent", time.Now().UTC(), record.NextAttemptAt, "")
}

func (s *Store) RetryCloudMessage(ctx context.Context, record cloudOutboxRecord, next time.Time, errText string) error {
	ctx, span := tracer.Start(ctx, "ClickHouse UPDATE cloud outbox retry", withAttributes(spanString("db.system", "clickhouse")))
	defer span.End()
	record.Attempts++
	if len(errText) > 512 {
		errText = errText[:512]
	}
	return s.updateCloudMessage(ctx, record, "pending", time.Unix(0, 0).UTC(), next.UTC(), errText)
}

func (s *Store) FailCloudMessage(ctx context.Context, record cloudOutboxRecord, errText string) error {
	ctx, span := tracer.Start(ctx, "ClickHouse UPDATE cloud outbox failed", withAttributes(spanString("db.system", "clickhouse")))
	defer span.End()
	record.Attempts++
	if len(errText) > 512 {
		errText = errText[:512]
	}
	return s.updateCloudMessage(ctx, record, "failed", time.Unix(0, 0).UTC(), time.Unix(0, 0).UTC(), errText)
}
