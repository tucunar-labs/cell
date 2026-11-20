package canarynode

import (
	"testing"
	"time"
)

func TestIncidentInScope(t *testing.T) {
	if !incidentInScope("analytical.ratio/tenant/entity", []string{"analytical.ratio/"}) {
		t.Fatal("incidentInScope() did not match rule scope")
	}
	if incidentInScope("threshold/other/tenant/entity", []string{"analytical.ratio/"}) {
		t.Fatal("incidentInScope() matched unrelated scope")
	}
}

func TestDeliveryAttemptIsDurableAndMonotonic(t *testing.T) {
	deliveries := map[string]incidentDelivery{
		deliveryKey("k", "f"): {Status: incidentDeliveryFailed, Attempts: 3},
	}
	if got := deliveryAttempt(deliveries, "k", "f"); got != 4 {
		t.Fatalf("deliveryAttempt() = %d, want 4", got)
	}
	if got := deliveryAttempt(deliveries, "k", "new"); got != 1 {
		t.Fatalf("deliveryAttempt() = %d, want 1", got)
	}
}

func TestIncidentStateRowRecoveryUsesResolutionTime(t *testing.T) {
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	row := incidentStateRowFromTransition(IncidentTransition{
		IncidentCandidate: IncidentCandidate{Key: "k", RuleID: "r", TenantID: "t", Window: time.Minute},
		Status:            incidentStatusRecovered, LastSeenAt: now,
	})
	if row.Status != incidentStatusRecovered || row.LastSeenAt != now {
		t.Fatalf("unexpected recovery row: %#v", row)
	}
	if row.Version == 0 {
		t.Fatal("recovery row has no version")
	}
}

func TestDurableIncidentFailsWhenCloudEnrollmentIsUnavailable(t *testing.T) {
	reporter := &cloudReporter{state: cloudState{}}
	err := reporter.enqueueDurableIncident(nil, IncidentTransition{
		IncidentCandidate: IncidentCandidate{
			Key:         "conversion/shop-a",
			Family:      "conversion",
			RuleID:      conversionIncidentRuleID,
			TenantID:    "shop-a",
			EntityID:    "shop-a",
			Fingerprint: "rate=0.100",
		},
		Status: incidentStatusOpen,
	})
	if err == nil {
		t.Fatal("unenrolled cloud reporter silently accepted an incident")
	}
}
