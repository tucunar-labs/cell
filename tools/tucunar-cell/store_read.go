package canarynode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

const (
	defaultCondorReadLimit = 100
	maxCondorReadLimit     = 1000
)

type signalActivity struct {
	Summary         operationalActivitySummary
	TotalSignals    uint64
	SourceCount     uint64
	EntityCount     uint64
	LastSignalAt    time.Time
	PrimaryTenant   string
	Events          []operationalEvent
	Routes          []operationalRoute
	Incidents       []operationalIncident
	ActiveIncidents int
}

// SignalActivity builds the operator-facing activity model from the same
// ClickHouse ledger as SignalOverview. It deliberately returns a small,
// bounded result: counts for the decision surface, the latest events for
// context, routes for understanding propagation, and durable incidents for
// action.
func (s *Store) SignalActivity(ctx context.Context, lookbackWindow time.Duration) (signalActivity, error) {
	ctx, span := tracer.Start(ctx, "ClickHouse SELECT signal activity",
		withAttributes(spanString("db.system", "clickhouse"), spanInt64("lookback_seconds", int64(lookbackWindow.Seconds()))))
	defer span.End()

	until := time.Now().UTC()
	since := until.Add(-lookbackWindow)
	args := []any{
		clickhouse.Named("since", since),
		clickhouse.Named("until", until),
		clickhouse.Named("organization_id", s.organizationID),
		clickhouse.Named("canary_id", s.canaryID),
	}
	const settings = `
SETTINGS max_execution_time = 10, max_rows_to_read = 5000000, max_result_rows = 100, result_overflow_mode = 'break'`
	activity := signalActivity{Events: make([]operationalEvent, 0, 12), Routes: make([]operationalRoute, 0, 8), Incidents: make([]operationalIncident, 0, 20)}

	summaryRows, err := s.conn.Query(ctx, `
SELECT
	count(),
	uniqCombined64(source),
	uniqCombined64(entity_id),
	max(occurred_at),
	argMax(tenant_id, occurred_at),
    countIf(JSONExtractString(dimensions, 'severity') = 'critical'),
    countIf(JSONExtractString(dimensions, 'severity') = 'warning'),
    countIf(JSONExtractString(dimensions, 'severity') = 'info'),
    argMax(JSONExtractString(dimensions, 'scenario_id'), occurred_at),
    argMax(JSONExtractString(dimensions, 'phase'), occurred_at),
    argMax(JSONExtractString(dimensions, 'severity'), occurred_at),
    argMax(JSONExtractString(dimensions, 'status'), occurred_at)
FROM canary_signal_events FINAL
WHERE occurred_at >= {since:DateTime64(3)}
  AND occurred_at < {until:DateTime64(3)}
  AND ({organization_id:String} = '' OR organization_id = {organization_id:String})
  AND ({canary_id:String} = '' OR canary_id = {canary_id:String})`+settings, args...)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "signal activity summary failed")
		return signalActivity{}, err
	}
	if summaryRows.Next() {
		if err := summaryRows.Scan(
			&activity.TotalSignals, &activity.SourceCount, &activity.EntityCount,
			&activity.LastSignalAt, &activity.PrimaryTenant,
			&activity.Summary.CriticalSignals, &activity.Summary.WarningSignals, &activity.Summary.InfoSignals,
			&activity.Summary.LatestScenario, &activity.Summary.LatestPhase,
			&activity.Summary.LatestSeverity, &activity.Summary.LatestStatus,
		); err != nil {
			summaryRows.Close()
			span.RecordError(err)
			span.SetStatus(spanError, "signal activity summary decode failed")
			return signalActivity{}, err
		}
	}
	if err := summaryRows.Err(); err != nil {
		summaryRows.Close()
		span.RecordError(err)
		span.SetStatus(spanError, "signal activity summary rows failed")
		return signalActivity{}, err
	}
	summaryRows.Close()
	if activity.TotalSignals == 0 {
		activity.LastSignalAt = time.Time{}
	}

	eventRows, err := s.conn.Query(ctx, `
SELECT occurred_at, source, entity_id, name, dimensions, payload
FROM canary_signal_events FINAL
WHERE occurred_at >= {since:DateTime64(3)}
  AND occurred_at < {until:DateTime64(3)}
  AND ({organization_id:String} = '' OR organization_id = {organization_id:String})
  AND ({canary_id:String} = '' OR canary_id = {canary_id:String})
ORDER BY occurred_at DESC, ingested_at DESC, signal_id DESC
LIMIT 12`+settings, args...)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "recent signal activity failed")
		return signalActivity{}, err
	}
	for eventRows.Next() {
		var event operationalEvent
		var dimensionsJSON, payloadJSON string
		if err := eventRows.Scan(&event.OccurredAt, &event.Source, &event.EntityID, &event.Name, &dimensionsJSON, &payloadJSON); err != nil {
			eventRows.Close()
			span.RecordError(err)
			span.SetStatus(spanError, "recent signal activity decode failed")
			return signalActivity{}, err
		}
		dimensions := map[string]string{}
		if dimensionsJSON != "" && dimensionsJSON != "{}" {
			if err := json.Unmarshal([]byte(dimensionsJSON), &dimensions); err != nil {
				eventRows.Close()
				span.RecordError(err)
				span.SetStatus(spanError, "recent signal dimensions decode failed")
				return signalActivity{}, err
			}
		}
		payload := map[string]any{}
		if payloadJSON != "" && payloadJSON != "{}" {
			if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
				eventRows.Close()
				span.RecordError(err)
				span.SetStatus(spanError, "recent signal payload decode failed")
				return signalActivity{}, err
			}
		}
		event.From = dimensions["from"]
		event.To = dimensions["to"]
		event.Phase = dimensions["phase"]
		event.Status = dimensions["status"]
		event.Severity = dimensions["severity"]
		event.Message = stringValue(payload["message"])
		activity.Events = append(activity.Events, event)
	}
	if err := eventRows.Err(); err != nil {
		eventRows.Close()
		span.RecordError(err)
		span.SetStatus(spanError, "recent signal activity rows failed")
		return signalActivity{}, err
	}
	eventRows.Close()

	routeRows, err := s.conn.Query(ctx, `
SELECT JSONExtractString(dimensions, 'from'), JSONExtractString(dimensions, 'to'), count()
FROM canary_signal_events FINAL
WHERE occurred_at >= {since:DateTime64(3)}
  AND occurred_at < {until:DateTime64(3)}
  AND JSONExtractString(dimensions, 'from') != ''
  AND JSONExtractString(dimensions, 'to') != ''
  AND ({organization_id:String} = '' OR organization_id = {organization_id:String})
  AND ({canary_id:String} = '' OR canary_id = {canary_id:String})
GROUP BY JSONExtractString(dimensions, 'from'), JSONExtractString(dimensions, 'to')
ORDER BY count() DESC
LIMIT 8`+settings, args...)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "signal route summary failed")
		return signalActivity{}, err
	}
	for routeRows.Next() {
		var route operationalRoute
		if err := routeRows.Scan(&route.From, &route.To, &route.Events); err != nil {
			routeRows.Close()
			span.RecordError(err)
			span.SetStatus(spanError, "signal route summary decode failed")
			return signalActivity{}, err
		}
		activity.Routes = append(activity.Routes, route)
	}
	if err := routeRows.Err(); err != nil {
		routeRows.Close()
		span.RecordError(err)
		span.SetStatus(spanError, "signal route summary rows failed")
		return signalActivity{}, err
	}
	routeRows.Close()

	detailRows, err := s.conn.Query(ctx, `
SELECT incident_key, status, family, source, tenant_id, entity_id,
       observed_value, threshold, first_seen_at, last_seen_at
FROM canary_incidents FINAL
WHERE status IN ('open', 'update')
  AND ({organization_id:String} = '' OR organization_id = {organization_id:String})
  AND ({canary_id:String} = '' OR canary_id = {canary_id:String})
ORDER BY last_seen_at DESC
LIMIT 20`+settings, args[2:]...)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "incident detail query failed")
		return signalActivity{}, err
	}
	for detailRows.Next() {
		var incident operationalIncident
		if err := detailRows.Scan(&incident.Key, &incident.Status, &incident.Family, &incident.Source, &incident.TenantID, &incident.EntityID,
			&incident.ObservedValue, &incident.Threshold, &incident.FirstSeenAt, &incident.LastSeenAt); err != nil {
			detailRows.Close()
			span.RecordError(err)
			span.SetStatus(spanError, "incident detail decode failed")
			return signalActivity{}, err
		}
		activity.Incidents = append(activity.Incidents, incident)
	}
	if err := detailRows.Err(); err != nil {
		detailRows.Close()
		span.RecordError(err)
		span.SetStatus(spanError, "incident detail rows failed")
		return signalActivity{}, err
	}
	detailRows.Close()
	activity.ActiveIncidents = len(activity.Incidents)

	span.SetAttributes(
		spanInt("activity.event_count", len(activity.Events)),
		spanInt("activity.route_count", len(activity.Routes)),
		spanInt("activity.active_incident_count", activity.ActiveIncidents),
	)
	return activity, nil
}

type condorCursor struct {
	OccurredAt time.Time `json:"occurred_at"`
	SignalID   string    `json:"signal_id"`
}

type signalStreamReadRequest struct {
	CapabilityContext
	Stream string    `json:"stream"`
	Cursor string    `json:"cursor,omitempty"`
	Limit  uint32    `json:"limit,omitempty"`
	Until  time.Time `json:"until,omitempty"`
}

type signalStreamReadResponse struct {
	CapabilityResult
	Signals    []Signal `json:"signals,omitempty"`
	NextCursor string   `json:"next_cursor,omitempty"`
	HasMore    bool     `json:"has_more"`
}

// ReadSignals implements Condor's bounded replacement boundary. The cursor is
// a stateless, opaque (timestamp, UUID) key; consumers persist it themselves,
// so restarting Condor cannot lose progress and no in-memory queue is needed.
func (s *Store) ReadSignals(ctx context.Context, cursor string, limit uint32, until time.Time) ([]Signal, string, bool, error) {
	return s.ReadSignalsSince(ctx, cursor, limit, time.Time{}, until)
}

// ReadSignalsSince adds an optional lower bound for snapshot consumers. A
// non-zero since is combined with the opaque cursor and keeps the query
// bounded without exposing the ledger to callers.
func (s *Store) ReadSignalsSince(ctx context.Context, cursor string, limit uint32, since, until time.Time) ([]Signal, string, bool, error) {
	if limit == 0 {
		limit = defaultCondorReadLimit
	}
	if limit > maxCondorReadLimit {
		return nil, "", false, fmt.Errorf("limit cannot exceed %d", maxCondorReadLimit)
	}
	if until.IsZero() {
		until = time.Now().UTC()
	}

	var decodedCursor condorCursor
	if strings.TrimSpace(cursor) != "" {
		if err := decodeCondorCursor(cursor, &decodedCursor); err != nil {
			return nil, "", false, err
		}
	}

	ctx, span := tracer.Start(ctx, "ClickHouse SELECT condor stream",
		withAttributes(
			spanString("db.system", "clickhouse"),
			spanString("db.operation", "read"),
			spanInt64("condor.limit", int64(limit)),
			spanBool("condor.has_cursor", cursor != ""),
		))
	defer span.End()

	cursorClause := ""
	args := []any{
		clickhouse.Named("until", until.UTC()),
		clickhouse.Named("organization_id", s.organizationID),
		clickhouse.Named("canary_id", s.canaryID),
	}
	sinceClause := ""
	if !since.IsZero() {
		sinceClause = "\n  AND occurred_at >= {since:DateTime64(3)}"
		args = append(args, clickhouse.Named("since", since.UTC()))
	}
	if cursor != "" {
		cursorClause = `
  AND (occurred_at > {cursor_at:DateTime64(3)}
       OR (occurred_at = {cursor_at:DateTime64(3)} AND toString(signal_id) > {cursor_id:String}))`
		args = append(args,
			clickhouse.Named("cursor_at", decodedCursor.OccurredAt.UTC()),
			clickhouse.Named("cursor_id", decodedCursor.SignalID),
		)
	}
	// The validated numeric limit is a literal for ClickHouse 24.3 compatibility.
	// That server rejects a native UInt32 query parameter in the LIMIT clause.
	query := fmt.Sprintf(`
SELECT signal_id, external_id, organization_id, canary_id, site_id,
       source, tenant_id, entity_id, kind, name, value, unit,
       dimensions, payload, occurred_at
FROM canary_signal_events FINAL
WHERE occurred_at < {until:DateTime64(3)}%s
  AND ({organization_id:String} = '' OR organization_id = {organization_id:String})
  AND ({canary_id:String} = '' OR canary_id = {canary_id:String})%s
ORDER BY occurred_at ASC, signal_id ASC
	LIMIT %d
	SETTINGS max_execution_time = 10, max_result_rows = 1001, result_overflow_mode = 'break'`, sinceClause, cursorClause, limit+1)
	rows, err := s.conn.Query(ctx, query, args...)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "condor stream query failed")
		return nil, "", false, err
	}
	defer rows.Close()

	signals := make([]Signal, 0, limit)
	for rows.Next() {
		var signal Signal
		var dimensionsJSON, payloadJSON string
		if err := rows.Scan(
			&signal.ID, &signal.ExternalID, &signal.OrganizationID, &signal.CanaryID, &signal.SiteID,
			&signal.Source, &signal.TenantID, &signal.EntityID, &signal.Kind, &signal.Name,
			&signal.Value, &signal.Unit, &dimensionsJSON, &payloadJSON, &signal.OccurredAt,
		); err != nil {
			span.RecordError(err)
			span.SetStatus(spanError, "condor stream row decode failed")
			return nil, "", false, err
		}
		if dimensionsJSON != "" && dimensionsJSON != "{}" {
			if err := json.Unmarshal([]byte(dimensionsJSON), &signal.Dimensions); err != nil {
				return nil, "", false, fmt.Errorf("decode signal dimensions: %w", err)
			}
		}
		if payloadJSON == "" {
			payloadJSON = "{}"
		}
		signal.Payload = json.RawMessage(payloadJSON)
		signals = append(signals, signal)
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "condor stream rows failed")
		return nil, "", false, err
	}

	hasMore := len(signals) > int(limit)
	if hasMore {
		signals = signals[:limit]
	}
	nextCursor := ""
	if len(signals) > 0 {
		last := signals[len(signals)-1]
		nextCursor, err = encodeCondorCursor(condorCursor{OccurredAt: last.OccurredAt, SignalID: last.ID})
		if err != nil {
			return nil, "", false, err
		}
	}
	span.SetAttributes(spanInt("condor.signal_count", len(signals)), spanBool("condor.has_more", hasMore))
	return signals, nextCursor, hasMore, nil
}

func encodeCondorCursor(cursor condorCursor) (string, error) {
	payload, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode condor cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeCondorCursor(encoded string, cursor *condorCursor) error {
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return fmt.Errorf("invalid condor cursor: %w", err)
	}
	if err := json.Unmarshal(payload, cursor); err != nil || cursor.OccurredAt.IsZero() || cursor.SignalID == "" {
		return fmt.Errorf("invalid condor cursor payload")
	}
	return nil
}
