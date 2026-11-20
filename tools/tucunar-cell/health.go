package canarynode

import (
	"context"
	"fmt"
)

// Ready verifies the dependency that makes the node useful. Liveness remains
// independent from ClickHouse so an orchestrator can restart the dependency
// without creating a restart loop for the process itself.
func (s *Store) Ready(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "ClickHouse PING readiness",
		withAttributes(spanString("db.system", "clickhouse")))
	defer span.End()
	if err := s.conn.Ping(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return fmt.Errorf("clickhouse is not ready: %w", err)
	}
	return nil
}
