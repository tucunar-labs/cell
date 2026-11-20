package canarynode

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// TimelineEvent is the read model used by the local incident timeline. The
// event remains a normal Signal in ClickHouse; this type only flattens the
// packet metadata needed by the operator view.
type TimelineEvent struct {
	ID            string            `json:"id"`
	OccurredAt    time.Time         `json:"occurred_at"`
	IngestedAt    time.Time         `json:"ingested_at"`
	Source        string            `json:"source"`
	TenantID      string            `json:"tenant_id"`
	SiteID        string            `json:"site_id"`
	EntityID      string            `json:"entity_id"`
	Kind          string            `json:"kind"`
	From          string            `json:"from"`
	To            string            `json:"to"`
	Name          string            `json:"name"`
	Phase         string            `json:"phase"`
	Status        string            `json:"status"`
	Severity      string            `json:"severity"`
	Message       string            `json:"message"`
	ScenarioID    string            `json:"scenario_id"`
	CorrelationID string            `json:"correlation_id"`
	CausationID   string            `json:"causation_id"`
	Value         float64           `json:"value"`
	Unit          string            `json:"unit"`
	Context       map[string]string `json:"context,omitempty"`
}

type timelineResponse struct {
	ScenarioID   string          `json:"scenario_id"`
	GeneratedAt  time.Time       `json:"generated_at"`
	FirstEventAt time.Time       `json:"first_event_at"`
	LastEventAt  time.Time       `json:"last_event_at"`
	EventCount   int             `json:"event_count"`
	Participants []string        `json:"participants"`
	Events       []TimelineEvent `json:"events"`
}

// Timeline returns the bounded, causally annotated packet stream for one
// scenario. It intentionally reads only through the normal signal ledger so
// the simulator and production sources share the same storage contract.
const (
	defaultConsoleEventLimit = 12000
	maxConsoleEventLimit     = 40000
)

func (s *Store) Timeline(ctx context.Context, scenarioID string, lookbackWindow time.Duration) (timelineResponse, error) {
	ctx, span := tracer.Start(ctx, "ClickHouse SELECT incident timeline",
		withAttributes(
			spanString("db.system", "clickhouse"),
			spanString("timeline.scenario_id", scenarioID),
			spanInt64("lookback_seconds", int64(lookbackWindow.Seconds())),
		))
	defer span.End()

	scenarioID = strings.TrimSpace(scenarioID)
	if scenarioID == "" {
		return timelineResponse{}, fmt.Errorf("scenario_id is required")
	}
	if lookbackWindow <= 0 {
		return timelineResponse{}, fmt.Errorf("timeline lookback must be positive")
	}

	until := time.Now().UTC()
	since := until.Add(-lookbackWindow)
	query := `
	SELECT signal_id, occurred_at, ingested_at, source, tenant_id, site_id, entity_id, kind, name, value, unit, dimensions, payload
FROM canary_signal_events FINAL
WHERE occurred_at >= {since:DateTime64(3)}
  AND occurred_at < {until:DateTime64(3)}
  AND JSONExtractString(dimensions, 'scenario_id') = {scenario_id:String}
  AND ({organization_id:String} = '' OR organization_id = {organization_id:String})
  AND ({canary_id:String} = '' OR canary_id = {canary_id:String})
ORDER BY occurred_at DESC, ingested_at DESC, signal_id DESC
LIMIT 2000
SETTINGS max_execution_time = 10, max_result_rows = 2000, result_overflow_mode = 'break'`
	rows, err := s.conn.Query(ctx, query,
		clickhouse.DateNamed("since", since, clickhouse.MilliSeconds),
		clickhouse.DateNamed("until", until, clickhouse.MilliSeconds),
		clickhouse.Named("scenario_id", scenarioID),
		clickhouse.Named("organization_id", s.organizationID),
		clickhouse.Named("canary_id", s.canaryID),
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "timeline query failed")
		return timelineResponse{}, err
	}
	defer rows.Close()

	response := timelineResponse{
		ScenarioID:   scenarioID,
		GeneratedAt:  until,
		Participants: make([]string, 0),
		Events:       make([]TimelineEvent, 0),
	}
	participants := make(map[string]struct{})
	for rows.Next() {
		var (
			id, source, tenantID, siteID, entityID, kind, name, unit, dimensionsJSON, payloadJSON string
			occurredAt, ingestedAt                                                                time.Time
			value                                                                                 float64
		)
		if err := rows.Scan(&id, &occurredAt, &ingestedAt, &source, &tenantID, &siteID, &entityID, &kind, &name, &value, &unit, &dimensionsJSON, &payloadJSON); err != nil {
			span.RecordError(err)
			span.SetStatus(spanError, "timeline row decode failed")
			return timelineResponse{}, err
		}

		dimensions := map[string]string{}
		if dimensionsJSON != "" && dimensionsJSON != "{}" {
			if err := json.Unmarshal([]byte(dimensionsJSON), &dimensions); err != nil {
				span.RecordError(err)
				span.SetStatus(spanError, "timeline dimensions decode failed")
				return timelineResponse{}, err
			}
		}
		payload := map[string]any{}
		if payloadJSON != "" && payloadJSON != "{}" {
			if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
				span.RecordError(err)
				span.SetStatus(spanError, "timeline payload decode failed")
				return timelineResponse{}, err
			}
		}

		from := dimensions["from"]
		to := dimensions["to"]
		if from != "" {
			participants[from] = struct{}{}
		}
		if to != "" {
			participants[to] = struct{}{}
		}
		response.Events = append(response.Events, TimelineEvent{
			ID:            id,
			OccurredAt:    occurredAt,
			IngestedAt:    ingestedAt,
			Source:        source,
			TenantID:      tenantID,
			SiteID:        siteID,
			EntityID:      entityID,
			Kind:          kind,
			From:          from,
			To:            to,
			Name:          name,
			Phase:         dimensions["phase"],
			Status:        dimensions["status"],
			Severity:      dimensions["severity"],
			Message:       stringValue(payload["message"]),
			ScenarioID:    dimensions["scenario_id"],
			CorrelationID: dimensions["correlation_id"],
			CausationID:   dimensions["causation_id"],
			Value:         value,
			Unit:          unit,
			Context:       timelineContext(dimensions),
		})
	}
	// The query takes the newest rows; the response keeps chronological order.
	for left, right := 0, len(response.Events)-1; left < right; left, right = left+1, right-1 {
		response.Events[left], response.Events[right] = response.Events[right], response.Events[left]
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "timeline rows failed")
		return timelineResponse{}, err
	}
	for participant := range participants {
		response.Participants = append(response.Participants, participant)
	}
	if len(response.Events) > 0 {
		response.FirstEventAt = response.Events[0].OccurredAt
		response.LastEventAt = response.Events[len(response.Events)-1].OccurredAt
	}
	response.EventCount = len(response.Events)
	span.SetAttributes(spanInt("timeline.event_count", response.EventCount), spanInt("timeline.participant_count", len(response.Participants)))
	return response, nil
}

func timelineContext(dimensions map[string]string) map[string]string {
	keys := []string{
		"asset", "location", "operation", "team", "metric", "rule", "impact",
		"owner", "responsibility", "exposure", "contact", "notification_target",
		"channel", "acknowledgement", "response_status", "error_rate", "trend",
	}
	context := make(map[string]string)
	for _, key := range keys {
		if value := strings.TrimSpace(dimensions[key]); value != "" {
			context[key] = value
		}
	}
	if len(context) == 0 {
		return nil
	}
	return context
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if stringValue, ok := value.(string); ok {
		return stringValue
	}
	return fmt.Sprint(value)
}

// consoleEvent is the lean row the browser console needs: no ids, no payloads,
// no empty context fields. Keeping the shape small is what makes a 15 minute
// window affordable to send to a browser.
type consoleEvent struct {
	OccurredAt time.Time `json:"occurred_at"`
	EntityID   string    `json:"entity_id"`
	Name       string    `json:"name"`
	Value      float64   `json:"value"`
	Unit       string    `json:"unit"`
	Quality    string    `json:"quality,omitempty"`
}

type consoleEventsResponse struct {
	ScenarioID  string         `json:"scenario_id"`
	LastEventAt time.Time      `json:"last_event_at"`
	Count       int            `json:"count"`
	Events      []consoleEvent `json:"events"`
}

// ConsoleEvents returns the newest rows in [since, until) for one scenario.
// A delta call passes since (exclusive); a first paint passes the window and
// gets the newest `limit` rows, in chronological order.
func (s *Store) ConsoleEvents(ctx context.Context, scenarioID string, since, until time.Time, limit uint32) (consoleEventsResponse, error) {
	scenarioID = strings.TrimSpace(scenarioID)
	if scenarioID == "" {
		return consoleEventsResponse{}, fmt.Errorf("scenario_id is required")
	}
	if limit == 0 {
		limit = defaultConsoleEventLimit
	}
	if limit > maxConsoleEventLimit {
		limit = maxConsoleEventLimit
	}
	if !until.After(since) {
		until = since.Add(time.Second)
	}
	// ClickHouse settings must be literals, so the bounded limit is formatted in.
	query := fmt.Sprintf(`
SELECT occurred_at, entity_id, name, value, unit, JSONExtractString(dimensions, 'quality')
FROM canary_signal_events FINAL
WHERE occurred_at > {since:DateTime64(3)}
  AND occurred_at <= {until:DateTime64(3)}
  AND JSONExtractString(dimensions, 'scenario_id') = {scenario_id:String}
  AND ({organization_id:String} = '' OR organization_id = {organization_id:String})
  AND ({canary_id:String} = '' OR canary_id = {canary_id:String})
ORDER BY occurred_at DESC
LIMIT %d
SETTINGS max_execution_time = 10, max_result_rows = %d, result_overflow_mode = 'break'`, limit, limit)

	rows, err := s.conn.Query(ctx, query,
		clickhouse.DateNamed("since", since, clickhouse.MilliSeconds),
		clickhouse.DateNamed("until", until, clickhouse.MilliSeconds),
		clickhouse.Named("scenario_id", scenarioID),
		clickhouse.Named("organization_id", s.organizationID),
		clickhouse.Named("canary_id", s.canaryID),
	)
	if err != nil {
		return consoleEventsResponse{}, err
	}
	defer rows.Close()

	events := make([]consoleEvent, 0, 256)
	for rows.Next() {
		var event consoleEvent
		if err := rows.Scan(&event.OccurredAt, &event.EntityID, &event.Name, &event.Value, &event.Unit, &event.Quality); err != nil {
			return consoleEventsResponse{}, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return consoleEventsResponse{}, err
	}
	for left, right := 0, len(events)-1; left < right; left, right = left+1, right-1 {
		events[left], events[right] = events[right], events[left]
	}
	response := consoleEventsResponse{ScenarioID: scenarioID, Count: len(events), Events: events}
	if len(events) > 0 {
		response.LastEventAt = events[len(events)-1].OccurredAt
	}
	return response, nil
}
