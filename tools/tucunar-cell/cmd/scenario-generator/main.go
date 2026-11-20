// Command scenario-generator creates finite, deterministic demo scenarios for the
// local dashboard. It models bird-to-bird packets as canonical Signals so the
// same ClickHouse ledger and timeline contract can be used by real connectors.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	canarynode "tucunar-cell"
	selfreport "tucunar-cell/internal/selfreport"
)

const (
	defaultSimpleScenario  = "demo-simple"
	defaultComplexScenario = "mine-major-incident"
)

type packetBatch struct {
	Signals []canarynode.Signal `json:"signals"`
}

type participant struct {
	Name string
	Kind string
}

var participants = []participant{
	{Name: "swift", Kind: "bird"},
	{Name: "canary", Kind: "bird"},
	{Name: "condor", Kind: "bird"},
	{Name: "kingfisher", Kind: "bird"},
	{Name: "kingbird", Kind: "bird"},
	{Name: "pigeon", Kind: "bird"},
	{Name: "woodpecker", Kind: "bird"},
	{Name: "heron", Kind: "bird"},
	{Name: "eagle", Kind: "bird"},
	{Name: "kestrel", Kind: "bird"},
	{Name: "hawk", Kind: "bird"},
	{Name: "raven", Kind: "bird"},
	{Name: "phoenix", Kind: "bird"},
	{Name: "lark", Kind: "bird"},
	{Name: "owl", Kind: "bird"},
	{Name: "toucan", Kind: "bird"},
	{Name: "shiitake", Kind: "fungus"},
	{Name: "mycelium", Kind: "fungus"},
	{Name: "polypore", Kind: "fungus"},
	{Name: "hummingbird", Kind: "bridge"},
	{Name: "tucunar-cloud", Kind: "cloud"},
}

type scenarioBuilder struct {
	scenarioID string
	profile    string
	tenantID   string
	orgID      string
	canaryID   string
	siteID     string
	sequence   int
	signals    []canarynode.Signal
}

func main() {
	target := flag.String("target", "http://127.0.0.1:18080/v1/signals/batch", "generic signal batch endpoint")
	token := flag.String("token", os.Getenv("SIGNAL_INGEST_TOKEN"), "optional generic ingest token")
	scenarioID := flag.String("scenario", "", "scenario id; defaults to a unique run id")
	tenantID := flag.String("tenant", "minera-andina", "tenant id")
	organizationID := flag.String("organization-id", "", "optional organization id")
	canaryID := flag.String("canary-id", "", "optional canary id")
	siteID := flag.String("site-id", "mina-norte", "site id")
	startRaw := flag.String("start", "", "simulated start time in RFC3339; defaults to four minutes ago")
	state := flag.String("state", "open", "incident state: open or recovered")
	profile := flag.String("profile", "complex", "demo profile: simple or complex")
	batchSize := flag.Int("batch-size", 500, "signals per HTTP batch")
	flag.Parse()

	if *batchSize < 1 || *batchSize > 1000 {
		log.Fatal("batch-size must be between 1 and 1000")
	}
	if *state != "open" && *state != "recovered" {
		log.Fatal("state must be open or recovered")
	}
	if *profile != "simple" && *profile != "complex" {
		log.Fatal("profile must be simple or complex")
	}
	start, err := parseStart(*startRaw)
	if err != nil {
		log.Fatal(err)
	}
	if strings.TrimSpace(*startRaw) == "" && *state == "recovered" {
		// Keep recovered demo signals outside the live 15-minute rule window so
		// the evaluator can close the durable incident while the 24-hour
		// scenario timeline still tells the overnight story.
		start = time.Now().UTC().Add(-20 * time.Minute).Truncate(time.Millisecond)
	}
	id := strings.TrimSpace(*scenarioID)
	if id == "" {
		prefix := defaultComplexScenario
		if *profile == "simple" {
			prefix = defaultSimpleScenario
		}
		id = fmt.Sprintf("%s-%s", prefix, time.Now().UTC().Format("20060102T150405Z"))
	}

	builder := scenarioBuilder{
		scenarioID: id,
		profile:    *profile,
		tenantID:   strings.TrimSpace(*tenantID),
		orgID:      strings.TrimSpace(*organizationID),
		canaryID:   strings.TrimSpace(*canaryID),
		siteID:     strings.TrimSpace(*siteID),
		signals:    make([]canarynode.Signal, 0, 900),
	}
	builder.build(start, *state)

	ctx := context.Background()
	if err := sendSignals(ctx, strings.TrimRight(strings.TrimSpace(*target), "/"), strings.TrimSpace(*token), builder.signals, *batchSize); err != nil {
		log.Fatal(err)
	}

	result := map[string]any{
		"scenario_id":       id,
		"profile":           builder.profile,
		"tenant_id":         builder.tenantID,
		"event_count":       len(builder.signals),
		"simulated_start":   builder.signals[0].OccurredAt,
		"simulated_end":     builder.signals[len(builder.signals)-1].OccurredAt,
		"dashboard_url":     fmt.Sprintf("http://127.0.0.1:18080/?scenario=%s", id),
		"timeline_api":      fmt.Sprintf("/api/timeline?scenario=%s", id),
		"participant_count": len(participants),
		"state":             *state,
		"handoff_count":     len(builder.signals),
		"note":              "Timestamps are simulated; packets follow a compact causal path through the canonical signal contract.",
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		log.Fatal(err)
	}
}

func parseStart(raw string) (time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return time.Now().UTC().Add(-4 * time.Minute).Truncate(time.Millisecond), nil
	}
	value, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("start must be RFC3339: %w", err)
	}
	return value.UTC(), nil
}

func (b *scenarioBuilder) build(start time.Time, state string) {
	if b.profile == "simple" {
		b.buildSimple(start, state)
		return
	}
	b.buildComplex(start, state)
}

// buildSimple is intentionally small enough to explain in a first demo. It
// still crosses the real ingest, durable-store and evaluator boundaries, and
// emits three timeout samples so the dev ClickHouse rule can open an incident.
func (b *scenarioBuilder) buildSimple(start time.Time, state string) {
	b.packet(start.Add(0*time.Second), "mycelium", "swift", "ingest.accepted", "normal", "info", "A service signal entered the cell.", 1)
	b.packet(start.Add(2*time.Second), "swift", "condor", "signal.appended", "normal", "info", "The signal reached the durable event boundary.", 1)
	b.packet(start.Add(4*time.Second), "condor", "polypore", "event.persisted", "normal", "info", "The raw event is available for analysis.", 1)
	b.packet(start.Add(6*time.Second), "polypore", "canary", "baseline.ready", "normal", "info", "The evaluator has a current baseline.", 1)

	// Three samples keep the demo connected to the real threshold evaluator.
	b.packetWithContext(start.Add(30*time.Second), "swift", "canary", "timeout.surge", "incident", "critical", "Truck 17 response latency crosses the first operating limit.", 24, map[string]string{"error_rate": "8", "trend": "rising"})
	b.packetWithContext(start.Add(31*time.Second), "swift", "canary", "timeout.surge", "incident", "critical", "Truck 17 response latency continues to rise.", 31, map[string]string{"error_rate": "10", "trend": "rising"})
	b.packetWithContext(start.Add(32*time.Second), "swift", "canary", "timeout.surge", "incident", "critical", "Truck 17 response latency crosses the alert threshold.", 38, map[string]string{"error_rate": "12", "trend": "rising"})
	b.packet(start.Add(34*time.Second), "polypore", "canary", "derived.candidate", "incident", "critical", "The rolling signal confirms a sustained degradation.", 38)
	b.packet(start.Add(36*time.Second), "canary", "condor", "incident.opened", "incident", "critical", "An operational incident is now open.", 1)
	b.packet(start.Add(38*time.Second), "condor", "mycelium", "incident.persisted", "incident", "critical", "The incident state is durable.", 1)
	b.packetWithContext(start.Add(40*time.Second), "hawk", "lark", "responsibility.assessed", "response", "warning", "Fleet Operations is the accountable team for Truck 17.", 1, map[string]string{"owner": "Fleet Operations duty supervisor", "responsibility": "Truck 17 response and dispatch visibility", "exposure": "haul route delay", "contact": "Fleet Operations duty supervisor"})
	b.packetWithContext(start.Add(42*time.Second), "lark", "toucan", "handoff.created", "response", "critical", "Fleet Operations duty supervisor owns the response.", 1, map[string]string{"owner": "Fleet Operations duty supervisor", "responsibility": "Truck 17 response and dispatch visibility", "response_status": "pending"})
	b.packetWithContext(start.Add(44*time.Second), "toucan", "pigeon", "notification.requested", "notification", "critical", "Fleet control room was notified about the Truck 17 delay.", 1, map[string]string{"notification_target": "Fleet control room", "channel": "SMS", "acknowledgement": "none"})
	b.packetWithContext(start.Add(46*time.Second), "pigeon", "mycelium", "notification.failed", "notification", "critical", "Fleet control room did not acknowledge the alert.", 1, map[string]string{"notification_target": "Fleet control room", "channel": "SMS", "acknowledgement": "none", "response_status": "no response"})
	if state == "open" {
		return
	}

	b.packetWithContext(start.Add(50*time.Second), "mycelium", "canary", "response.started", "response", "info", "Fleet Operations began the response workflow.", 1, map[string]string{"owner": "Fleet Operations duty supervisor", "response_status": "working"})
	b.packet(start.Add(65*time.Second), "canary", "condor", "incident.recovered", "recovery", "info", "The signal returned to the expected operating range.", 1)
	b.packet(start.Add(67*time.Second), "condor", "polypore", "recovery.persisted", "recovery", "info", "The recovery transition is durable.", 1)
}

func (b *scenarioBuilder) buildComplex(start time.Time, state string) {
	// Establish the normal data path before the incident.
	b.packet(start.Add(0*time.Second), "mycelium", "swift", "ingest.accepted", "normal", "info", "Telemetry from the haul fleet entered the cell.", 1)
	b.packet(start.Add(2*time.Second), "swift", "woodpecker", "contract.validated", "normal", "info", "Fleet payload matches the canonical signal contract.", 1)
	b.packet(start.Add(4*time.Second), "woodpecker", "heron", "facts.ready", "normal", "info", "Validated asset facts are ready for reconciliation.", 1)
	b.packet(start.Add(6*time.Second), "swift", "condor", "signal.appended", "normal", "info", "Canonical telemetry was handed to the durable signal boundary.", 1)
	b.packet(start.Add(8*time.Second), "condor", "polypore", "event.persisted", "normal", "info", "Raw signal is durable in the analytical store.", 1)
	b.packet(start.Add(10*time.Second), "polypore", "condor", "rollup.updated", "normal", "info", "Five-second equipment rollups are current.", 1)
	b.packet(start.Add(12*time.Second), "condor", "canary", "baseline.ready", "normal", "info", "Canary has a current operating baseline.", 1)
	b.packet(start.Add(14*time.Second), "kingfisher", "condor", "health.observed", "normal", "info", "Plant network and gateway are reachable.", 99.98)

	// The major incident: connectivity degradation and rising crusher timeouts.
	b.packetWithContext(start.Add(70*time.Second), "kingfisher", "canary", "health.degraded", "incident", "critical", "Packet loss rises on the North Plant gateway serving Crusher 02.", 18, map[string]string{"trend": "rising", "error_rate": "12"})
	// Three samples make the generic ClickHouse threshold rule real in dev;
	// they are still one operational symptom, not three separate incidents.
	b.packetWithContext(start.Add(72*time.Second), "swift", "canary", "timeout.surge", "incident", "critical", "Crusher 02 telemetry timeouts begin to accelerate.", 42, map[string]string{"error_rate": "10", "trend": "rising"})
	b.packetWithContext(start.Add(73*time.Second), "swift", "canary", "timeout.surge", "incident", "critical", "Crusher 02 telemetry timeouts continue to rise.", 47, map[string]string{"error_rate": "11", "trend": "rising"})
	b.packetWithContext(start.Add(74*time.Second), "swift", "canary", "timeout.surge", "incident", "critical", "Crusher 02 telemetry timeouts cross the operating limit.", 54, map[string]string{"error_rate": "12", "trend": "rising"})
	b.packet(start.Add(74*time.Second), "swift", "condor", "signal.appended", "incident", "warning", "Timeout observations continue to arrive and remain durable.", 42)
	b.packet(start.Add(76*time.Second), "condor", "polypore", "rollup.updated", "incident", "warning", "Timeout slope crosses the analytical candidate threshold.", 42)
	b.packet(start.Add(78*time.Second), "polypore", "condor", "derived.candidate", "incident", "critical", "Polypore detects a rapidly growing timeout series.", 42)
	b.packet(start.Add(80*time.Second), "condor", "canary", "derived.candidate", "incident", "critical", "Candidate is sent to the hot evaluator for incident semantics.", 42)
	b.packet(start.Add(82*time.Second), "canary", "kingbird", "correlation.requested", "incident", "critical", "Find related transport, asset and process observations.", 1)
	b.packet(start.Add(84*time.Second), "canary", "raven", "evidence.requested", "incident", "critical", "Build an evidence timeline for the crusher interruption.", 1)
	b.packet(start.Add(86*time.Second), "canary", "condor", "incident.opened", "incident", "critical", "Crusher interruption incident is now open.", 1)
	b.packet(start.Add(88*time.Second), "condor", "polypore", "incident.persisted", "incident", "critical", "Incident lifecycle and evidence references are durable.", 1)
	b.packetWithContext(start.Add(90*time.Second), "hawk", "lark", "responsibility.assessed", "response", "warning", "North Plant maintenance is accountable for the Crusher 02 response.", 1, map[string]string{"owner": "North Plant maintenance team", "responsibility": "Crusher 02 gateway and telemetry recovery", "exposure": "production-window loss", "contact": "North Plant duty engineer"})
	b.packetWithContext(start.Add(92*time.Second), "lark", "toucan", "handoff.created", "response", "critical", "North Plant maintenance team owns the incident response.", 1, map[string]string{"owner": "North Plant maintenance team", "responsibility": "Crusher 02 gateway and telemetry recovery", "response_status": "pending"})
	b.packetWithContext(start.Add(94*time.Second), "toucan", "pigeon", "notification.requested", "notification", "critical", "The control room and shift supervisor were notified about the Crusher 02 interruption.", 1, map[string]string{"notification_target": "Control room + shift supervisor", "channel": "Microsoft Teams", "acknowledgement": "none"})
	b.packetWithContext(start.Add(96*time.Second), "pigeon", "mycelium", "notification.failed", "notification", "critical", "The control room and shift supervisor did not acknowledge the alert.", 1, map[string]string{"notification_target": "Control room + shift supervisor", "channel": "Microsoft Teams", "acknowledgement": "none", "response_status": "no response"})
	b.packetWithContext(start.Add(98*time.Second), "mycelium", "canary", "delivery.degraded", "notification", "critical", "The missing acknowledgement was recorded as an operational delivery issue.", 1, map[string]string{"notification_target": "Control room + shift supervisor", "channel": "Microsoft Teams", "acknowledgement": "none", "response_status": "no response"})
	if state == "open" {
		return
	}

	// Decision and response layer.
	b.packet(start.Add(95*time.Second), "heron", "kingbird", "reconciliation.mismatch", "response", "warning", "SCADA and fleet records disagree on the last valid reading.", 1)
	b.packet(start.Add(97*time.Second), "eagle", "raven", "artifact.facts", "response", "info", "Maintenance artifact facts are available with provenance.", 1)
	b.packetWithContext(start.Add(99*time.Second), "hawk", "kingbird", "exposure.assessed", "response", "warning", "Crusher 02 is exposed to a production-window loss.", 1, map[string]string{"owner": "North Plant maintenance team", "responsibility": "Crusher 02 gateway and telemetry recovery", "exposure": "production-window loss"})
	b.packet(start.Add(101*time.Second), "kingbird", "canary", "correlation.completed", "response", "critical", "Probable cause: gateway degradation amplified by a conveyor fault.", .91)
	b.packet(start.Add(103*time.Second), "raven", "canary", "timeline.completed", "response", "info", "Evidence timeline is complete enough for a response decision.", 1)
	b.packet(start.Add(105*time.Second), "kestrel", "phoenix", "action.authorized", "response", "info", "Maintenance restart is authorized for the affected asset.", 1)
	b.packetWithContext(start.Add(107*time.Second), "lark", "phoenix", "handoff.updated", "response", "info", "North Plant maintenance team accepted the recovery task and is working on it.", 1, map[string]string{"owner": "North Plant maintenance team", "responsibility": "Crusher 02 gateway and telemetry recovery", "response_status": "working", "contact": "North Plant duty engineer"})
	b.packet(start.Add(109*time.Second), "phoenix", "canary", "recovery.plan", "response", "warning", "Controlled restart and gateway inspection are planned.", 1)
	b.packet(start.Add(111*time.Second), "owl", "toucan", "recommendation.ready", "response", "info", "Inspect the gateway before restarting the conveyor.", .88)
	b.packetWithContext(start.Add(113*time.Second), "toucan", "pigeon", "notification.escalated", "response", "critical", "The alert was escalated to the North Plant duty engineer.", 1, map[string]string{"notification_target": "North Plant duty engineer", "channel": "Email", "acknowledgement": "working", "response_status": "working", "contact": "North Plant duty engineer"})
	b.packetWithContext(start.Add(115*time.Second), "pigeon", "mycelium", "notification.delivered", "response", "info", "The North Plant duty engineer acknowledged the alert and is working on gateway inspection.", 1, map[string]string{"notification_target": "North Plant duty engineer", "channel": "Email", "acknowledgement": "acknowledged", "response_status": "working", "contact": "North Plant duty engineer"})
	b.packetWithContext(start.Add(117*time.Second), "mycelium", "canary", "delivery.confirmed", "response", "info", "The escalation path is active and has an acknowledged owner.", 1, map[string]string{"notification_target": "North Plant duty engineer", "channel": "Email", "acknowledgement": "acknowledged", "response_status": "working"})

	// Shiitake is deliberately an advanced-model escalation, not part of the
	// normal hot path. Its input is scoped evidence, never the raw stream.
	b.packet(start.Add(125*time.Second), "toucan", "shiitake", "reasoning.requested", "advanced", "info", "Advanced model requested to explain the compound failure.", 1)
	b.packet(start.Add(132*time.Second), "shiitake", "toucan", "reasoning.completed", "advanced", "info", "Compound failure explanation returned with evidence references.", .94)
	b.packet(start.Add(134*time.Second), "toucan", "pigeon", "notification.requested", "advanced", "critical", "Escalated explanation is ready for the control room.", 1)
	b.packet(start.Add(136*time.Second), "pigeon", "mycelium", "notification.delivered", "advanced", "info", "Fallback email delivery succeeded.", 1)

	// Recovery and cloud push. Hummingbird is outbound-only and carries only
	// summaries, health and incidents, never raw mining telemetry.
	b.packet(start.Add(155*time.Second), "phoenix", "kingfisher", "recovery.action", "recovery", "info", "Gateway inspection and controlled restart begin.", 1)
	b.packet(start.Add(165*time.Second), "kingfisher", "canary", "health.recovered", "recovery", "info", "Gateway packet loss returned to baseline.", 99.99)
	b.packet(start.Add(168*time.Second), "canary", "condor", "incident.recovered", "recovery", "info", "Crusher interruption is recovered and evidence is complete.", 1)
	b.packet(start.Add(170*time.Second), "condor", "polypore", "recovery.persisted", "recovery", "info", "Recovery transition and final rollups are durable.", 1)
	b.packet(start.Add(172*time.Second), "condor", "mycelium", "cloud.summary.ready", "recovery", "info", "Redacted incident and rollup summary is ready for cloud push.", 1)
	b.packet(start.Add(174*time.Second), "mycelium", "hummingbird", "cloud.push.requested", "recovery", "info", "Hummingbird receives the outbound summary.", 1)
	b.packet(start.Add(176*time.Second), "hummingbird", "tucunar-cloud", "cloud.push.sent", "recovery", "info", "Incident, health and rollup summary delivered outbound.", 1)

}

func (b *scenarioBuilder) packet(at time.Time, from, to, name, phase, severity, message string, value float64) {
	b.packetWithContext(at, from, to, name, phase, severity, message, value, nil)
}

func (b *scenarioBuilder) packetWithContext(at time.Time, from, to, name, phase, severity, message string, value float64, extra map[string]string) {
	b.sequence++
	packetID := fmt.Sprintf("%s-packet-%04d", b.scenarioID, b.sequence)
	dimensions := b.domainContext()
	for key, item := range extra {
		if strings.TrimSpace(item) != "" {
			dimensions[key] = item
		}
	}
	dimensions["scenario_id"] = b.scenarioID
	dimensions["profile"] = b.profile
	dimensions["from"] = from
	dimensions["to"] = to
	dimensions["phase"] = phase
	dimensions["status"] = packetStatus(name)
	dimensions["severity"] = severity
	dimensions["correlation_id"] = b.scenarioID
	dimensions["causation_id"] = b.causationID()
	context := make(map[string]string, len(dimensions))
	for key, item := range extra {
		if strings.TrimSpace(item) != "" {
			context[key] = item
		}
	}
	payload, err := json.Marshal(map[string]any{
		"packet_id":      packetID,
		"message":        message,
		"from":           from,
		"to":             to,
		"simulated_at":   at.UTC().Format(time.RFC3339Nano),
		"causation_id":   b.causationID(),
		"correlation_id": b.scenarioID,
		"context":        context,
	})
	if err != nil {
		log.Fatalf("encode packet: %v", err)
	}
	b.signals = append(b.signals, canarynode.Signal{
		ExternalID:     packetID,
		OrganizationID: b.orgID,
		CanaryID:       b.canaryID,
		SiteID:         b.siteID,
		Source:         from,
		TenantID:       b.tenantID,
		EntityID:       to,
		Kind:           "event",
		Name:           name,
		Value:          value,
		Unit:           packetUnit(name),
		Dimensions:     dimensions,
		Payload:        payload,
		OccurredAt:     at.UTC(),
	})
}

func (b *scenarioBuilder) domainContext() map[string]string {
	if b.profile == "simple" {
		return map[string]string{
			"asset":     "Truck 17",
			"location":  "North Yard",
			"operation": "haul route",
			"team":      "Fleet Operations",
			"metric":    "response latency",
			"rule":      "haul.route.response_latency",
			"impact":    "truck telemetry and dispatch visibility",
		}
	}
	return map[string]string{
		"asset":     "Crusher 02",
		"location":  "North Plant",
		"operation": "primary crushing",
		"team":      "North Plant Operations",
		"metric":    "telemetry timeout rate",
		"rule":      "crusher.telemetry.timeout_growth",
		"impact":    "crusher telemetry unavailable",
	}
}

func packetStatus(name string) string {
	switch {
	case strings.HasSuffix(name, ".failed"):
		return "failed"
	case name == "incident.opened":
		return "open"
	case strings.Contains(name, "requested"):
		return "requested"
	case strings.Contains(name, "authorized"):
		return "authorized"
	case strings.Contains(name, "degraded"):
		return "degraded"
	case strings.Contains(name, "recovered"):
		return "recovered"
	default:
		return "delivered"
	}
}

func packetUnit(name string) string {
	switch {
	case strings.Contains(name, "timeout"):
		return "seconds"
	case strings.Contains(name, "health"):
		return "percent"
	case strings.Contains(name, "correlation") || strings.Contains(name, "reasoning"):
		return "confidence"
	default:
		return "event"
	}
}

func (b *scenarioBuilder) causationID() string {
	if b.sequence == 0 {
		return ""
	}
	return fmt.Sprintf("%s-packet-%04d", b.scenarioID, b.sequence)
}

func sendSignals(parent context.Context, target, token string, signals []canarynode.Signal, batchSize int) error {
	if target == "" {
		return fmt.Errorf("target must not be empty")
	}
	client := &http.Client{Timeout: 20 * time.Second}
	tracer := selfreport.NewTracer("tucunar-scenario-generator")
	for start := 0; start < len(signals); start += batchSize {
		end := start + batchSize
		if end > len(signals) {
			end = len(signals)
		}
		batch := packetBatch{Signals: signals[start:end]}
		body, err := json.Marshal(batch)
		if err != nil {
			return fmt.Errorf("encode batch %d: %w", start/batchSize, err)
		}
		ctx, span := tracer.Start(parent, "scenario-generator.send_batch",
			withAttributes(
				spanString("http.target", target),
				spanInt("signal.count", end-start),
				spanString("scenario.id", signals[start].Dimensions["scenario_id"]),
			))
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			span.RecordError(err)
			span.SetStatus(spanError, err.Error())
			span.End()
			return fmt.Errorf("create batch request: %w", err)
		}
		request.Header.Set("Content-Type", "application/json")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(request)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(spanError, err.Error())
			span.End()
			return fmt.Errorf("send batch %d: %w", start/batchSize, err)
		}
		_, readErr := io.Copy(io.Discard, response.Body)
		response.Body.Close()
		span.SetAttributes(spanInt("http.status_code", response.StatusCode))
		if readErr != nil {
			span.RecordError(readErr)
			span.SetStatus(spanError, readErr.Error())
			span.End()
			return fmt.Errorf("read batch %d response: %w", start/batchSize, readErr)
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			err := fmt.Errorf("batch %d returned HTTP %d", start/batchSize, response.StatusCode)
			span.RecordError(err)
			span.SetStatus(spanError, err.Error())
			span.End()
			return err
		}
		span.End()
	}
	return nil
}
