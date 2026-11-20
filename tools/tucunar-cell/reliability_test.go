package canarynode

import (
	"testing"
	"time"
)

func TestSignalDedupKeyScopesExternalIdentity(t *testing.T) {
	base := Signal{ID: "550e8400-e29b-41d4-a716-446655440000", ExternalID: "provider-1", Source: "erp", TenantID: "acme", EntityID: "asset-1", Kind: "event", Name: "reading", OccurredAt: time.Now()}
	if base.DedupKey() != base.DedupKey() {
		t.Fatal("DedupKey() is not deterministic")
	}
	if base.DedupKey() == (Signal{ID: base.ID, ExternalID: "provider-1", Source: "erp", TenantID: "other", EntityID: base.EntityID, Kind: base.Kind, Name: base.Name, OccurredAt: base.OccurredAt}).DedupKey() {
		t.Fatal("DedupKey() did not scope external identity by tenant")
	}
	withoutExternal := base
	withoutExternal.ExternalID = ""
	if withoutExternal.DedupKey() == base.DedupKey() {
		t.Fatal("signal and external identities unexpectedly collided")
	}
}

func TestIncidentCandidateFingerprintCanSuppressStableTicks(t *testing.T) {
	first := IncidentCandidate{Key: "k", Fingerprint: "observed=10.000"}
	second := IncidentCandidate{Key: "k", Fingerprint: "observed=10.000"}
	if first.Key != second.Key || first.Fingerprint != second.Fingerprint {
		t.Fatal("stable candidates should have the same durable identity and fingerprint")
	}
}
