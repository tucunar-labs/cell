package canarynode

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

const consumerOffsetRetentionDays = 90

func consumerOffsetsSchema() string {
	return fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS canary_consumer_offsets (
    organization_id LowCardinality(String),
    canary_id       LowCardinality(String),
    consumer        LowCardinality(String),
    cursor          String,
    updated_at      DateTime64(3, 'UTC')
) ENGINE = ReplacingMergeTree(updated_at)
ORDER BY (organization_id, canary_id, consumer)
TTL toDateTime(updated_at) + INTERVAL %d DAY DELETE
`, consumerOffsetRetentionDays)
}

func (s *Store) ConsumerOffset(ctx context.Context, consumer string) (string, error) {
	consumer = strings.TrimSpace(consumer)
	if consumer == "" {
		return "", nil
	}
	rows, err := s.conn.Query(ctx, `
SELECT argMax(cursor, updated_at)
FROM canary_consumer_offsets FINAL
WHERE organization_id = {organization_id:String}
  AND canary_id = {canary_id:String}
  AND consumer = {consumer:String}`,
		clickhouse.Named("organization_id", s.organizationID),
		clickhouse.Named("canary_id", s.canaryID),
		clickhouse.Named("consumer", consumer),
	)
	if err != nil {
		return "", fmt.Errorf("read consumer offset: %w", err)
	}
	defer rows.Close()
	var cursor string
	if rows.Next() {
		if err := rows.Scan(&cursor); err != nil {
			return "", fmt.Errorf("decode consumer offset: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("read consumer offset rows: %w", err)
	}
	return cursor, nil
}

func (s *Store) SaveConsumerOffset(ctx context.Context, consumer, cursor string) error {
	var err error
	consumer, err = validateCondorConsumer(consumer)
	if err != nil {
		return err
	}
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return nil
	}
	var decoded condorCursor
	if err := decodeCondorCursor(cursor, &decoded); err != nil {
		return fmt.Errorf("offset cursor is invalid: %w", err)
	}
	if err := s.conn.Exec(ctx, `
INSERT INTO canary_consumer_offsets (organization_id, canary_id, consumer, cursor, updated_at)
VALUES ({organization_id:String}, {canary_id:String}, {consumer:String}, {cursor:String}, {updated_at:DateTime64(3)})`,
		clickhouse.Named("organization_id", s.organizationID),
		clickhouse.Named("canary_id", s.canaryID),
		clickhouse.Named("consumer", consumer),
		clickhouse.Named("cursor", cursor),
		clickhouse.Named("updated_at", time.Now().UTC()),
	); err != nil {
		return fmt.Errorf("save consumer offset: %w", err)
	}
	return nil
}
