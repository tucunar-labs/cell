package canarynode

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

const (
	incidentStatusOpen      = "open"
	incidentStatusUpdate    = "update"
	incidentStatusRecovered = "recovered"
	incidentDeliveryPending = "pending"
	incidentDeliverySent    = "sent"
	incidentDeliveryFailed  = "failed"
)

const incidentSchema = `
CREATE TABLE IF NOT EXISTS canary_incidents (
    organization_id    LowCardinality(String) DEFAULT '',
    canary_id          String DEFAULT '',
    incident_key       String,
    version            UInt64,
    status             LowCardinality(String),
    rule_id            String,
    family             LowCardinality(String),
    source             LowCardinality(String),
    tenant_id          LowCardinality(String),
    entity_id          String DEFAULT '',
    observed_value     Float64 DEFAULT 0,
    baseline_value     Float64 DEFAULT 0,
    threshold          Float64 DEFAULT 0,
    operator           LowCardinality(String) DEFAULT '',
    window_seconds     UInt32 DEFAULT 0,
    recent_samples     UInt64 DEFAULT 0,
    baseline_samples   UInt64 DEFAULT 0,
    numerator_samples  UInt64 DEFAULT 0,
    denominator_samples UInt64 DEFAULT 0,
    fingerprint        String DEFAULT '',
    first_seen_at      DateTime64(3, 'UTC'),
    last_seen_at       DateTime64(3, 'UTC'),
    resolved_at        DateTime64(3, 'UTC') DEFAULT toDateTime64(0, 3, 'UTC')
) ENGINE = ReplacingMergeTree(version)
ORDER BY (organization_id, canary_id, incident_key)
TTL toDateTime(last_seen_at) + INTERVAL 13 MONTH DELETE
`

const incidentDeliverySchema = `
CREATE TABLE IF NOT EXISTS canary_incident_deliveries (
    organization_id LowCardinality(String) DEFAULT '',
    canary_id       String DEFAULT '',
    incident_key String,
    fingerprint  String,
    status       LowCardinality(String),
    attempts     UInt32,
    updated_at   DateTime64(3, 'UTC'),
    version      UInt64
) ENGINE = ReplacingMergeTree(version)
ORDER BY (organization_id, canary_id, incident_key, fingerprint)
TTL toDateTime(updated_at) + INTERVAL 13 MONTH DELETE
`

type IncidentCandidate struct {
	Key                string
	Scope              string
	RuleID             string
	Family             string
	Source             string
	TenantID           string
	EntityID           string
	ObservedValue      float64
	BaselineValue      float64
	Threshold          float64
	Operator           ThresholdOperator
	Window             time.Duration
	RecentSamples      uint64
	BaselineSamples    uint64
	NumeratorSamples   uint64
	DenominatorSamples uint64
	Fingerprint        string
}

type IncidentTransition struct {
	IncidentCandidate
	Status      string
	FirstSeenAt time.Time
	LastSeenAt  time.Time
	Attempt     uint32
}

type durableIncidentState struct {
	IncidentTransition
	Version uint64
}

type incidentDelivery struct {
	Status   string
	Attempts uint32
}

// ReconcileIncidents reconstructs the current state from ClickHouse on every
// evaluation. It writes only lifecycle transitions, so a healthy rule does not
// produce an alert or a row on every ticker iteration.
func (s *Store) ReconcileIncidents(ctx context.Context, candidates []IncidentCandidate, scopes []string) ([]IncidentTransition, error) {
	ctx, span := tracer.Start(ctx, "ClickHouse RECONCILE incidents",
		withAttributes(spanString("db.system", "clickhouse"), spanInt("candidate.count", len(candidates))))
	defer span.End()
	states, err := s.loadIncidentStates(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return nil, err
	}
	deliveries, err := s.loadIncidentDeliveries(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return nil, err
	}
	now := time.Now().UTC()
	byKey := make(map[string]IncidentCandidate, len(candidates))
	for _, candidate := range candidates {
		if candidate.Key == "" || candidate.Scope == "" || candidate.TenantID == "" {
			return nil, fmt.Errorf("incident candidate is missing identity")
		}
		byKey[candidate.Key] = candidate
	}
	transitions := make([]IncidentTransition, 0, len(candidates))
	rows := make([]incidentStateRow, 0, len(candidates))
	for _, candidate := range byKey {
		current, exists := states[candidate.Key]
		transition := IncidentTransition{IncidentCandidate: candidate, LastSeenAt: now, Attempt: 1}
		if exists && current.Status != incidentStatusRecovered {
			transition.FirstSeenAt = current.FirstSeenAt
			transition.Status = incidentStatusUpdate
			if current.Fingerprint == candidate.Fingerprint {
				transition.Status = current.Status
				if delivery, delivered := deliveries[deliveryKey(candidate.Key, candidate.Fingerprint)]; delivered && delivery.Status == incidentDeliverySent {
					continue
				}
				transition.Attempt = deliveryAttempt(deliveries, candidate.Key, candidate.Fingerprint)
			} else {
				rows = append(rows, incidentStateRowFromTransition(transition))
			}
		} else {
			transition.Status = incidentStatusOpen
			transition.FirstSeenAt = now
			rows = append(rows, incidentStateRowFromTransition(transition))
		}
		transitions = append(transitions, transition)
	}
	for key, current := range states {
		if !incidentInScope(key, scopes) {
			continue
		}
		if current.Status == incidentStatusRecovered {
			if delivery, delivered := deliveries[deliveryKey(key, current.Fingerprint)]; delivered && delivery.Status == incidentDeliverySent {
				continue
			}
			transition := current.IncidentTransition
			transition.Status = incidentStatusRecovered
			transition.LastSeenAt = now
			transition.Attempt = deliveryAttempt(deliveries, key, transition.Fingerprint)
			transitions = append(transitions, transition)
			continue
		}
		if _, stillFailing := byKey[key]; stillFailing {
			continue
		}
		transition := current.IncidentTransition
		transition.Status = incidentStatusRecovered
		transition.LastSeenAt = now
		transition.Attempt = deliveryAttempt(deliveries, key, transition.Fingerprint)
		if delivery, delivered := deliveries[deliveryKey(key, transition.Fingerprint)]; delivered && delivery.Status == incidentDeliverySent {
			rows = append(rows, incidentStateRowFromTransition(transition))
			continue
		}
		rows = append(rows, incidentStateRowFromTransition(transition))
		transitions = append(transitions, transition)
	}
	if err := s.insertIncidentStates(ctx, rows); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return nil, err
	}
	span.SetAttributes(spanInt("incident.transition_count", len(transitions)))
	return transitions, nil
}

func incidentInScope(key string, scopes []string) bool {
	for _, scope := range scopes {
		if len(scope) > 0 && len(key) >= len(scope) && key[:len(scope)] == scope {
			return true
		}
	}
	return false
}

func deliveryKey(key, fingerprint string) string { return key + "\x1f" + fingerprint }

func deliveryAttempt(deliveries map[string]incidentDelivery, key, fingerprint string) uint32 {
	if delivery, exists := deliveries[deliveryKey(key, fingerprint)]; exists {
		return delivery.Attempts + 1
	}
	return 1
}

type incidentStateRow struct {
	IncidentTransition
	Version uint64
}

func incidentStateRowFromTransition(transition IncidentTransition) incidentStateRow {
	return incidentStateRow{IncidentTransition: transition, Version: uint64(time.Now().UnixNano())}
}

func (s *Store) loadIncidentStates(ctx context.Context) (map[string]durableIncidentState, error) {
	ctx, span := tracer.Start(ctx, "ClickHouse SELECT incident state", withAttributes(spanString("db.system", "clickhouse")))
	defer span.End()
	query := `SELECT organization_id, canary_id, incident_key, version, status, rule_id, family, source, tenant_id, entity_id,
    observed_value, baseline_value, threshold, operator, window_seconds, recent_samples,
    baseline_samples, numerator_samples, denominator_samples, fingerprint, first_seen_at, last_seen_at
FROM canary_incidents FINAL
WHERE ({organization_id:String} = '' OR organization_id = {organization_id:String})
  AND ({canary_id:String} = '' OR canary_id = {canary_id:String})
ORDER BY last_seen_at DESC
LIMIT 10000
SETTINGS max_execution_time = 10, max_rows_to_read = 100000000, max_result_rows = 10000, result_overflow_mode = 'break'`
	rows, err := s.conn.Query(ctx, query,
		clickhouse.Named("organization_id", s.organizationID), clickhouse.Named("canary_id", s.canaryID))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return nil, err
	}
	defer rows.Close()
	states := make(map[string]durableIncidentState)
	for rows.Next() {
		var state durableIncidentState
		var windowSeconds uint32
		var operator string
		var organizationID, canaryID string
		if err := rows.Scan(&organizationID, &canaryID, &state.Key, &state.Version, &state.Status, &state.RuleID, &state.Family,
			&state.Source, &state.TenantID, &state.EntityID, &state.ObservedValue, &state.BaselineValue,
			&state.Threshold, &operator, &windowSeconds, &state.RecentSamples, &state.BaselineSamples,
			&state.NumeratorSamples, &state.DenominatorSamples, &state.Fingerprint, &state.FirstSeenAt, &state.LastSeenAt); err != nil {
			span.RecordError(err)
			span.SetStatus(spanError, err.Error())
			return nil, err
		}
		state.Operator = ThresholdOperator(operator)
		state.Window = time.Duration(windowSeconds) * time.Second
		states[state.Key] = state
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return nil, err
	}
	span.SetAttributes(spanInt("incident.state_count", len(states)))
	return states, nil
}

func (s *Store) loadIncidentDeliveries(ctx context.Context) (map[string]incidentDelivery, error) {
	ctx, span := tracer.Start(ctx, "ClickHouse SELECT incident deliveries", withAttributes(spanString("db.system", "clickhouse")))
	defer span.End()
	query := `SELECT incident_key, fingerprint, status, attempts
FROM canary_incident_deliveries FINAL
WHERE ({organization_id:String} = '' OR organization_id = {organization_id:String})
  AND ({canary_id:String} = '' OR canary_id = {canary_id:String})
ORDER BY updated_at DESC
LIMIT 10000
SETTINGS max_execution_time = 10, max_rows_to_read = 100000000, max_result_rows = 10000, result_overflow_mode = 'break'`
	rows, err := s.conn.Query(ctx, query,
		clickhouse.Named("organization_id", s.organizationID), clickhouse.Named("canary_id", s.canaryID))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return nil, err
	}
	defer rows.Close()
	deliveries := make(map[string]incidentDelivery)
	for rows.Next() {
		var key, fingerprint string
		var delivery incidentDelivery
		if err := rows.Scan(&key, &fingerprint, &delivery.Status, &delivery.Attempts); err != nil {
			span.RecordError(err)
			span.SetStatus(spanError, err.Error())
			return nil, err
		}
		deliveries[deliveryKey(key, fingerprint)] = delivery
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return nil, err
	}
	span.SetAttributes(spanInt("incident.delivery_count", len(deliveries)))
	return deliveries, nil
}

func (s *Store) insertIncidentStates(ctx context.Context, rows []incidentStateRow) error {
	ctx, span := tracer.Start(ctx, "ClickHouse INSERT incident lifecycle", withAttributes(spanString("db.system", "clickhouse"), spanInt("incident.transition_count", len(rows))))
	defer span.End()
	if len(rows) == 0 {
		return nil
	}
	const query = `INSERT INTO canary_incidents
    (organization_id, canary_id, incident_key, version, status, rule_id, family, source, tenant_id, entity_id, observed_value, baseline_value, threshold, operator, window_seconds, recent_samples, baseline_samples, numerator_samples, denominator_samples, fingerprint, first_seen_at, last_seen_at, resolved_at)`
	batch, err := s.conn.PrepareBatch(ctx, query)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	defer batch.Close()
	for _, row := range rows {
		resolved := time.Unix(0, 0).UTC()
		if row.Status == incidentStatusRecovered {
			resolved = row.LastSeenAt
		}
		if err := batch.Append(s.organizationID, s.canaryID, row.Key, row.Version, row.Status, row.RuleID, row.Family, row.Source, row.TenantID,
			row.EntityID, row.ObservedValue, row.BaselineValue, row.Threshold, string(row.Operator), uint32(row.Window.Seconds()),
			row.RecentSamples, row.BaselineSamples, row.NumeratorSamples, row.DenominatorSamples, row.Fingerprint,
			row.FirstSeenAt, row.LastSeenAt, resolved); err != nil {
			span.RecordError(err)
			span.SetStatus(spanError, err.Error())
			return err
		}
	}
	if err := batch.Send(); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	return nil
}

// MarkIncidentDelivery is itself append-only. The latest version is selected
// with ReplacingMergeTree FINAL on the next tick, allowing failed webhooks to
// retry without creating an in-memory queue.
func (s *Store) MarkIncidentDelivery(ctx context.Context, transition IncidentTransition, delivered bool) error {
	ctx, span := tracer.Start(ctx, "ClickHouse INSERT incident_delivery",
		withAttributes(spanString("db.system", "clickhouse"), spanString("incident.key", transition.Key)))
	defer span.End()
	status := incidentDeliveryFailed
	if delivered {
		status = incidentDeliverySent
	}
	query := `INSERT INTO canary_incident_deliveries
    (organization_id, canary_id, incident_key, fingerprint, status, attempts, updated_at, version) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if err := s.conn.Exec(clickhouse.Context(ctx, clickhouse.WithAsync(true)), query,
		s.organizationID, s.canaryID, transition.Key, transition.Fingerprint, status, transition.Attempt, time.Now().UTC(), uint64(time.Now().UnixNano())); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	return nil
}
