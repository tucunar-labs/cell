package canarynode

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

//go:embed dashboard.html assets/console.html assets/logos/*.svg
var dashboardFiles embed.FS

type dashboardShop struct {
	ShopID          string  `json:"shop_id"`
	CheckoutStarted uint64  `json:"checkout_started"`
	PaymentSuccess  uint64  `json:"payment_success"`
	ConversionRate  float64 `json:"conversion_rate"`
	Status          string  `json:"status"`
}

type dashboardResponse struct {
	GeneratedAt     time.Time       `json:"generated_at"`
	WindowMinutes   int             `json:"window_minutes"`
	TotalShops      int             `json:"total_shops"`
	AtRiskShops     int             `json:"at_risk_shops"`
	CheckoutStarted uint64          `json:"checkout_started"`
	PaymentSuccess  uint64          `json:"payment_success"`
	ConversionRate  float64         `json:"conversion_rate"`
	Shops           []dashboardShop `json:"shops"`
}

type signalOverview struct {
	GeneratedAt   time.Time       `json:"generated_at"`
	WindowMinutes int             `json:"window_minutes"`
	TotalSignals  uint64          `json:"total_signals"`
	SourceCount   uint64          `json:"source_count"`
	EntityCount   uint64          `json:"entity_count"`
	Summaries     []signalSummary `json:"summaries"`
}

type signalSummary struct {
	Source         string    `json:"source"`
	TenantID       string    `json:"tenant_id"`
	EntityID       string    `json:"entity_id"`
	Kind           string    `json:"kind"`
	Name           string    `json:"name"`
	Unit           string    `json:"unit"`
	Samples        uint64    `json:"samples"`
	AverageValue   float64   `json:"avg_value"`
	LastOccurredAt time.Time `json:"last_occurred_at"`
}

// dashboardSource is intentionally derived from the already bounded signal
// groups. It gives the operator a useful concentration view without adding a
// second high-cardinality ClickHouse query just to draw a chart.
type dashboardSource struct {
	Source          string    `json:"source"`
	ObservedSamples uint64    `json:"observed_samples"`
	GroupCount      int       `json:"group_count"`
	LastOccurredAt  time.Time `json:"last_occurred_at"`
}

type operationalActivitySummary struct {
	CriticalSignals uint64 `json:"critical_signals"`
	WarningSignals  uint64 `json:"warning_signals"`
	InfoSignals     uint64 `json:"info_signals"`
	LatestScenario  string `json:"latest_scenario,omitempty"`
	LatestPhase     string `json:"latest_phase,omitempty"`
	LatestSeverity  string `json:"latest_severity,omitempty"`
	LatestStatus    string `json:"latest_status,omitempty"`
}

type operationalEvent struct {
	OccurredAt time.Time `json:"occurred_at"`
	Source     string    `json:"source"`
	From       string    `json:"from,omitempty"`
	To         string    `json:"to,omitempty"`
	EntityID   string    `json:"entity_id"`
	Name       string    `json:"name"`
	Phase      string    `json:"phase,omitempty"`
	Status     string    `json:"status,omitempty"`
	Severity   string    `json:"severity,omitempty"`
	Message    string    `json:"message,omitempty"`
}

type operationalRoute struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Events uint64 `json:"events"`
}

type operationalIncident struct {
	Key           string    `json:"key"`
	Status        string    `json:"status"`
	Family        string    `json:"family"`
	Source        string    `json:"source"`
	TenantID      string    `json:"tenant_id"`
	EntityID      string    `json:"entity_id"`
	ObservedValue float64   `json:"observed_value"`
	Threshold     float64   `json:"threshold"`
	FirstSeenAt   time.Time `json:"first_seen_at"`
	LastSeenAt    time.Time `json:"last_seen_at"`
}

type operationsOverviewResponse struct {
	GeneratedAt      time.Time                  `json:"generated_at"`
	PrimaryTenant    string                     `json:"primary_tenant,omitempty"`
	WindowSeconds    int64                      `json:"window_seconds"`
	WindowMinutes    int                        `json:"window_minutes"`
	TotalSignals     uint64                     `json:"total_signals"`
	SourceCount      uint64                     `json:"source_count"`
	EntityCount      uint64                     `json:"entity_count"`
	GroupCount       int                        `json:"group_count"`
	ObservedSamples  uint64                     `json:"observed_samples"`
	IngestRate       float64                    `json:"signals_per_second"`
	LastSignalAt     *time.Time                 `json:"last_signal_at,omitempty"`
	FreshnessSeconds int64                      `json:"freshness_seconds"`
	Status           string                     `json:"status"`
	CriticalSignals  uint64                     `json:"critical_signals"`
	WarningSignals   uint64                     `json:"warning_signals"`
	ActiveIncidents  int                        `json:"active_incidents"`
	Activity         operationalActivitySummary `json:"activity"`
	Sources          []dashboardSource          `json:"sources"`
	Streams          []signalSummary            `json:"streams"`
	Events           []operationalEvent         `json:"events"`
	Routes           []operationalRoute         `json:"routes"`
	Incidents        []operationalIncident      `json:"incidents"`
	Runtime          runtimeMetricsSnapshot     `json:"runtime"`
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeDashboardAPI(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="canary-operator"`)
		http.Error(w, "operator token required", http.StatusUnauthorized)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/assets/logos/") {
		asset := strings.TrimPrefix(r.URL.Path, "/assets/logos/")
		if asset == "" || path.Base(asset) != asset || !strings.HasSuffix(asset, ".svg") {
			http.NotFound(w, r)
			return
		}
		data, err := dashboardFiles.ReadFile("assets/logos/" + asset)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(data)
		return
	}
	if r.URL.Path == "/api/signals/overview" {
		s.handleSignalsOverviewAPI(w, r)
		return
	}
	if r.URL.Path == "/api/timeline" {
		s.handleTimelineAPI(w, r)
		return
	}
	if r.URL.Path == "/legacy" {
		serveEmbeddedHTML(w, "dashboard.html")
		return
	}
	if r.URL.Path != "/" && r.URL.Path != "/console" {
		http.NotFound(w, r)
		return
	}
	serveEmbeddedHTML(w, "assets/console.html")
}

func (s *Server) handleTimelineAPI(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "HTTP GET /api/timeline",
		withAttributes(
			spanString("http.method", r.Method),
			spanString("http.target", r.URL.Path),
		))
	defer span.End()

	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scenarioID := strings.TrimSpace(r.URL.Query().Get("scenario"))
	if scenarioID == "" {
		scenarioID = "mine-major-incident"
	}
	window := lookbackWindow
	if rawWindow := strings.TrimSpace(r.URL.Query().Get("window_seconds")); rawWindow != "" {
		var seconds int
		if _, err := fmt.Sscanf(rawWindow, "%d", &seconds); err != nil || seconds < 1 || seconds > 7*24*60*60 {
			http.Error(w, "window_seconds must be between 1 and 604800", http.StatusBadRequest)
			return
		}
		window = time.Duration(seconds) * time.Second
	}
	if !s.authorizeDashboardAPI(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="canary-operator"`)
		http.Error(w, "operator token required", http.StatusUnauthorized)
		return
	}

	timeline, err := s.store.Timeline(ctx, scenarioID, window)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "timeline query failed")
		http.Error(w, "timeline query failed", http.StatusInternalServerError)
		return
	}
	span.SetAttributes(spanInt("timeline.event_count", timeline.EventCount))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(timeline); err != nil {
		span.RecordError(err)
	}
}

func (s *Server) handleSignalsOverviewAPI(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "HTTP GET /api/signals/overview",
		withAttributes(
			spanString("http.method", r.Method),
			spanString("http.target", r.URL.Path),
			spanInt64("lookback_minutes", int64(lookbackWindow.Minutes())),
		))
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

	overview, err := s.store.SignalOverview(ctx, lookbackWindow)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "signals overview query failed")
		http.Error(w, "signals overview query failed", http.StatusInternalServerError)
		return
	}
	span.SetAttributes(
		spanInt64("signals.total", int64(overview.TotalSignals)),
		spanInt64("signals.source_count", int64(overview.SourceCount)),
		spanInt64("signals.entity_count", int64(overview.EntityCount)),
		spanInt("signals.summary_count", len(overview.Summaries)),
	)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(overview); err != nil {
		span.RecordError(err)
	}
}

// handleOperationsOverviewAPI is the single read model used by the local
// operator screen. It deliberately combines the bounded ClickHouse overview
// with process self-reporting so the browser does not fan out to every bird.
func (s *Server) handleOperationsOverviewAPI(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "HTTP GET /api/overview",
		withAttributes(
			spanString("http.method", r.Method),
			spanString("http.target", r.URL.Path),
			spanInt64("lookback_minutes", int64(lookbackWindow.Minutes())),
		))
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

	activity, err := s.store.SignalActivity(ctx, lookbackWindow)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "operations activity query failed")
		http.Error(w, "operations activity unavailable", http.StatusServiceUnavailable)
		return
	}
	freshnessSeconds := int64(0)
	if !activity.LastSignalAt.IsZero() {
		freshnessSeconds = int64(time.Since(activity.LastSignalAt).Seconds())
		if freshnessSeconds < 0 {
			freshnessSeconds = 0
		}
	}
	windowSeconds := int64(lookbackWindow.Seconds())
	var lastSignalAtPtr *time.Time
	if !activity.LastSignalAt.IsZero() {
		lastSignalAtCopy := activity.LastSignalAt
		lastSignalAtPtr = &lastSignalAtCopy
	}
	response := operationsOverviewResponse{
		GeneratedAt:      time.Now().UTC(),
		PrimaryTenant:    activity.PrimaryTenant,
		WindowSeconds:    windowSeconds,
		WindowMinutes:    int(lookbackWindow.Minutes()),
		TotalSignals:     activity.TotalSignals,
		SourceCount:      activity.SourceCount,
		EntityCount:      activity.EntityCount,
		GroupCount:       len(activity.Events),
		ObservedSamples:  activity.TotalSignals,
		IngestRate:       float64(activity.TotalSignals) / float64(windowSeconds),
		LastSignalAt:     lastSignalAtPtr,
		FreshnessSeconds: freshnessSeconds,
		Status:           "live",
		CriticalSignals:  activity.Summary.CriticalSignals,
		WarningSignals:   activity.Summary.WarningSignals,
		ActiveIncidents:  activity.ActiveIncidents,
		Activity:         activity.Summary,
		Sources:          make([]dashboardSource, 0),
		Streams:          make([]signalSummary, 0),
		Events:           activity.Events,
		Routes:           activity.Routes,
		Incidents:        activity.Incidents,
		Runtime:          nodeRuntimeMetrics.snapshot(),
	}
	span.SetAttributes(
		spanInt64("signals.total", int64(response.TotalSignals)),
		spanInt64("signals.source_count", int64(response.SourceCount)),
		spanInt("signals.group_count", response.GroupCount),
	)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		span.RecordError(err)
	}
}

func (s *Server) handleDashboardAPI(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "HTTP GET /api/dashboard",
		withAttributes(
			spanString("http.method", r.Method),
			spanString("http.target", r.URL.Path),
		),
	)
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

	metrics, err := s.store.DashboardMetrics(ctx, lookbackWindow)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "dashboard query failed")
		http.Error(w, "dashboard query failed", http.StatusInternalServerError)
		return
	}

	response := dashboardResponse{
		GeneratedAt:   time.Now().UTC(),
		WindowMinutes: int(lookbackWindow.Minutes()),
		TotalShops:    len(metrics),
		Shops:         make([]dashboardShop, 0, len(metrics)),
	}
	for _, metric := range metrics {
		status := "healthy"
		if metric.ConversionRate < conversionThreshold {
			status = "at_risk"
			response.AtRiskShops++
		}
		response.CheckoutStarted += metric.CheckoutStarted
		response.PaymentSuccess += metric.PaymentSuccess
		response.Shops = append(response.Shops, dashboardShop{
			ShopID:          metric.ShopID,
			CheckoutStarted: metric.CheckoutStarted,
			PaymentSuccess:  metric.PaymentSuccess,
			ConversionRate:  metric.ConversionRate,
			Status:          status,
		})
	}
	if response.CheckoutStarted > 0 {
		response.ConversionRate = float64(response.PaymentSuccess) / float64(response.CheckoutStarted)
	}

	span.SetAttributes(
		spanInt("dashboard.shop_count", response.TotalShops),
		spanInt("dashboard.at_risk_shops", response.AtRiskShops),
	)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		span.RecordError(err)
	}
}

func (s *Server) handleDiagnosticsAPI(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "HTTP GET /api/diagnostics",
		withAttributes(spanString("http.method", r.Method), spanString("http.target", r.URL.Path)))
	defer span.End()
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorizeOperatorAPI(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="canary-operator"`)
		http.Error(w, "operator token required", http.StatusUnauthorized)
		return
	}
	storage, err := s.store.Diagnostics(ctx, lookbackWindow)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "diagnostics query failed")
		http.Error(w, "diagnostics unavailable", http.StatusServiceUnavailable)
		return
	}
	outbox := map[string]any{"scoped": false, "pending": 0, "failed": 0}
	if nodeID := s.store.CloudNodeID(); nodeID != "" {
		stats, outboxErr := s.store.CloudOutboxDiagnostics(ctx, nodeID)
		if outboxErr != nil {
			span.RecordError(outboxErr)
			span.SetStatus(spanError, "outbox diagnostics query failed")
			http.Error(w, "diagnostics unavailable", http.StatusServiceUnavailable)
			return
		}
		outbox = map[string]any{"scoped": true, "node_id": stats.NodeID, "pending": stats.Pending, "failed": stats.Failed}
	}
	response := map[string]any{
		"generated_at":    time.Now().UTC(),
		"version":         currentNodeVersion(),
		"deployment_mode": env("CANARY_DEPLOYMENT_MODE", "on_premise"),
		"air_gapped":      isAirGapped(),
		"runtime":         nodeRuntimeMetrics.snapshot(),
		"storage":         storage,
		"outbox":          outbox,
		"limits": map[string]any{
			"lookback_minutes":          int(lookbackWindow.Minutes()),
			"max_single_body_bytes":     maxSingleSignalBody,
			"max_batch_body_bytes":      maxBatchSignalBody,
			"max_batch_signals":         maxSignalBatchSize,
			"max_pending_cloud_reports": intFromEnv("CANARY_MAX_PENDING_REPORTS", 10000),
		},
		"integrations": map[string]any{
			"signal_auth_required":       s.signalToken != "",
			"operator_auth_required":     s.operatorToken != "",
			"cloud_reporting_configured": strings.TrimSpace(os.Getenv("TUCUNAR_API_BASE_URL")) != "",
			"alert_webhook_configured":   strings.TrimSpace(os.Getenv("ALERT_WEBHOOK_URL")) != "",
		},
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		span.RecordError(err)
	}
}

func (s *Server) handleVersionAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"service": "tucunar-cell", "version": currentNodeVersion()})
}

func serveEmbeddedHTML(w http.ResponseWriter, name string) {
	data, err := dashboardFiles.ReadFile(name)
	if err != nil {
		http.Error(w, "console unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// handleConsoleEventsAPI is the console's read path: lean rows, newest first
// internally, chronological on the wire. `since` asks for a delta.
func (s *Server) handleConsoleEventsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorizeDashboardAPI(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="canary-operator"`)
		http.Error(w, "operator token required", http.StatusUnauthorized)
		return
	}
	query := r.URL.Query()
	scenarioID := strings.TrimSpace(query.Get("scenario"))
	if scenarioID == "" {
		scenarioID = "mine-major-incident"
	}
	window := lookbackWindow
	if raw := strings.TrimSpace(query.Get("window_seconds")); raw != "" {
		var seconds int
		if _, err := fmt.Sscanf(raw, "%d", &seconds); err != nil || seconds < 1 || seconds > 7*24*60*60 {
			http.Error(w, "window_seconds must be between 1 and 604800", http.StatusBadRequest)
			return
		}
		window = time.Duration(seconds) * time.Second
	}
	var limit uint32 = defaultConsoleEventLimit
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		var parsed uint32
		if _, err := fmt.Sscanf(raw, "%d", &parsed); err != nil {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		limit = parsed
	}
	until := time.Now().UTC()
	since := until.Add(-window)
	if raw := strings.TrimSpace(query.Get("since")); raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			http.Error(w, "since must be an RFC3339 timestamp", http.StatusBadRequest)
			return
		}
		since = parsed
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	response, err := s.store.ConsoleEvents(ctx, scenarioID, since, until, limit)
	if err != nil {
		http.Error(w, "console events query failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}
