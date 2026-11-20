package canarynode

import (
	"context"
	"errors"
	"time"
)

func conversionIncidentKey(shopID string) string {
	return conversionIncidentRuleID + ":" + shopID
}

// enqueueDurableIncident bridges the ClickHouse-backed lifecycle to the
// existing outbound cloud outbox. The evaluator decides when a transition
// exists; the outbox owns retry/idempotency after that point.
func (r *cloudReporter) enqueueDurableIncident(ctx context.Context, transition IncidentTransition) error {
	if !r.enrolled() {
		// A transition is only marked as delivered after it has been accepted by
		// the durable cloud outbox. Treat an unavailable/expired enrollment as a
		// delivery failure so the ClickHouse incident lifecycle can retry it after
		// the node reconnects instead of silently losing the cloud notification.
		return errors.New("cloud node is not enrolled")
	}
	event := transition.Status
	if event == incidentStatusOpen {
		event = "opened"
	} else if event == incidentStatusUpdate {
		event = "updated"
	}
	now := transition.FirstSeenAt.UTC()
	if now.IsZero() {
		now = transition.LastSeenAt.UTC()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	window := transition.Window
	if window <= 0 {
		window = lookbackWindow
	}
	status := "open"
	if transition.Status == incidentStatusRecovered {
		status = "recovered"
	}
	identity := r.identity()
	incidentKey := transition.Key
	shopID := ""
	conversionRate := float64(0)
	if transition.Family == "conversion" {
		incidentKey = conversionIncidentKey(transition.TenantID)
		shopID = transition.EntityID
		conversionRate = transition.ObservedValue
	}
	emissionID := stableUUID(incidentKey, event, transition.Fingerprint)
	payload := incidentRequest{
		ContractVersion: cloudContractVersion,
		OrganizationID:  identity.OrganizationID,
		CanaryID:        identity.CanaryID,
		NodeID:          identity.NodeID,
		EmissionID:      emissionID,
		IncidentKey:     incidentKey,
		RuleID:          transition.RuleID,
		LifecycleEvent:  event,
		Status:          status,
		OccurredAt:      now,
		Family:          transition.Family,
		Source:          transition.Source,
		TenantID:        transition.TenantID,
		EntityID:        transition.EntityID,
		ShopID:          shopID,
		Window:          cloudWindow{Start: now.Add(-window), End: now, Seconds: int(window.Seconds())},
		Threshold:       incidentLimit{Operator: string(transition.Operator), Value: transition.Threshold},
		Metrics: incidentMetrics{
			CheckoutStarted: transition.RecentSamples,
			PaymentSuccess:  transition.NumeratorSamples,
			ConversionRate:  conversionRate,
			ObservedValue:   transition.ObservedValue,
			BaselineValue:   transition.BaselineValue,
		},
	}
	return r.enqueue(ctx, "incident", "/v1/canaries/"+identity.CanaryID+"/incidents", "incident:"+emissionID, payload)
}
