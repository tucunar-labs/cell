package canarynode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

type scenarioSummary struct {
	ID             string    `json:"id"`
	Profile        string    `json:"profile"`
	TenantID       string    `json:"tenant_id"`
	SiteID         string    `json:"site_id"`
	Asset          string    `json:"asset,omitempty"`
	Location       string    `json:"location,omitempty"`
	Metric         string    `json:"metric,omitempty"`
	Rule           string    `json:"rule,omitempty"`
	FirstEventAt   time.Time `json:"first_event_at"`
	LastEventAt    time.Time `json:"last_event_at"`
	EventCount     uint64    `json:"event_count"`
	CriticalCount  uint64    `json:"critical_count"`
	WarningCount   uint64    `json:"warning_count"`
	LastPhase      string    `json:"last_phase"`
	LastStatus     string    `json:"last_status"`
	IncidentOpened bool      `json:"incident_opened"`
	Recovered      bool      `json:"recovered"`
}

type scenariosResponse struct {
	GeneratedAt time.Time         `json:"generated_at"`
	WindowHours int               `json:"window_hours"`
	Scenarios   []scenarioSummary `json:"scenarios"`
}

// Scenarios returns a bounded case index from the durable ledger. It is the
// navigation model for the local console; a customer can have many sources
// and tenants without making the browser guess from insertion order.
func (s *Store) Scenarios(ctx context.Context, window time.Duration, limit int) ([]scenarioSummary, error) {
	ctx, span := tracer.Start(ctx, "ClickHouse SELECT scenario index",
		withAttributes(
			spanString("db.system", "clickhouse"),
			spanInt64("lookback_seconds", int64(window.Seconds())),
			spanInt("scenario.limit", limit),
		))
	defer span.End()
	if window <= 0 {
		return nil, fmt.Errorf("scenario window must be positive")
	}
	if limit < 1 || limit > 100 {
		return nil, fmt.Errorf("scenario limit must be between 1 and 100")
	}
	until := time.Now().UTC()
	since := until.Add(-window)
	query := fmt.Sprintf(`
	SELECT
	    scenario_id,
	    argMax(profile, occurred_at),
	    argMin(tenant_id, occurred_at),
    argMin(site_id, occurred_at),
	    argMax(asset, occurred_at),
	    argMax(location, occurred_at),
	    argMax(metric, occurred_at),
	    argMax(rule, occurred_at),
    min(occurred_at),
    max(occurred_at),
    count(),
    countIf(severity = 'critical'),
    countIf(severity = 'warning'),
    argMax(phase, occurred_at),
    argMax(status, occurred_at),
    countIf(name = 'incident.opened') > 0,
	    countIf(status = 'recovered' OR phase = 'recovery' OR name LIKE '%%recovered%%') > 0
FROM (
    SELECT
	        JSONExtractString(dimensions, 'scenario_id') AS scenario_id,
	        JSONExtractString(dimensions, 'profile') AS profile,
        JSONExtractString(dimensions, 'phase') AS phase,
        JSONExtractString(dimensions, 'status') AS status,
        JSONExtractString(dimensions, 'severity') AS severity,
		name,
		JSONExtractString(dimensions, 'asset') AS asset,
		JSONExtractString(dimensions, 'location') AS location,
		JSONExtractString(dimensions, 'metric') AS metric,
		JSONExtractString(dimensions, 'rule') AS rule,
        tenant_id,
        site_id,
        occurred_at
    FROM canary_signal_events FINAL
    WHERE occurred_at >= {since:DateTime64(3)}
      AND occurred_at < {until:DateTime64(3)}
      AND JSONExtractString(dimensions, 'scenario_id') != ''
      AND ({organization_id:String} = '' OR organization_id = {organization_id:String})
      AND ({canary_id:String} = '' OR canary_id = {canary_id:String})
)
GROUP BY scenario_id
ORDER BY max(occurred_at) DESC
LIMIT %d
SETTINGS max_execution_time = 10, max_rows_to_read = 5000000, max_result_rows = 100, result_overflow_mode = 'break'`, limit)
	rows, err := s.conn.Query(ctx, query,
		clickhouse.Named("since", since),
		clickhouse.Named("until", until),
		clickhouse.Named("organization_id", s.organizationID),
		clickhouse.Named("canary_id", s.canaryID),
	)
	if err != nil {
		logger.Warn("scenario index query failed", "error", err)
		span.RecordError(err)
		span.SetStatus(spanError, "scenario index query failed")
		return nil, err
	}
	defer rows.Close()
	result := make([]scenarioSummary, 0, limit)
	for rows.Next() {
		var scenario scenarioSummary
		if err := rows.Scan(&scenario.ID, &scenario.Profile, &scenario.TenantID, &scenario.SiteID, &scenario.Asset, &scenario.Location, &scenario.Metric, &scenario.Rule, &scenario.FirstEventAt, &scenario.LastEventAt,
			&scenario.EventCount, &scenario.CriticalCount, &scenario.WarningCount, &scenario.LastPhase, &scenario.LastStatus, &scenario.IncidentOpened, &scenario.Recovered); err != nil {
			logger.Warn("scenario index decode failed", "error", err)
			span.RecordError(err)
			span.SetStatus(spanError, "scenario index decode failed")
			return nil, err
		}
		result = append(result, scenario)
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "scenario index rows failed")
		return nil, err
	}
	span.SetAttributes(spanInt("scenario.count", len(result)))
	return result, nil
}

func (s *Server) handleScenariosAPI(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "HTTP GET /api/scenarios",
		withAttributes(spanString("http.method", r.Method), spanString("http.target", r.URL.Path)))
	defer span.End()
	if !s.authorizeDashboardAPI(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="canary-operator"`)
		http.Error(w, "operator token required", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	window := 7 * 24 * time.Hour
	if raw := strings.TrimSpace(r.URL.Query().Get("window_seconds")); raw != "" {
		var seconds int
		if _, err := fmt.Sscanf(raw, "%d", &seconds); err != nil || seconds < 1 || seconds > 30*24*60*60 {
			http.Error(w, "window_seconds must be between 1 and 2592000", http.StatusBadRequest)
			return
		}
		window = time.Duration(seconds) * time.Second
	}
	limit := 20
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if _, err := fmt.Sscanf(raw, "%d", &limit); err != nil || limit < 1 || limit > 100 {
			http.Error(w, "limit must be between 1 and 100", http.StatusBadRequest)
			return
		}
	}
	scenarios, err := s.store.Scenarios(ctx, window, limit)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "scenario index unavailable")
		http.Error(w, "scenario index unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(scenariosResponse{GeneratedAt: time.Now().UTC(), WindowHours: int(window / time.Hour), Scenarios: scenarios})
}
