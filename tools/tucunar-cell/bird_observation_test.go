package canarynode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

func birdObservationRequest(capability CapabilityID, operation string, payload any) CapabilityRequest {
	data, _ := json.Marshal(payload)
	return CapabilityRequest{ContractVersion: capabilityContractVersion, RequestID: "test-request", OrganizationID: "org", CellID: "cell", NodeID: "node", Capability: capability, Operation: operation, Payload: data}
}

func TestObservationBirdsAreFunctional(t *testing.T) {
	ctx := context.Background()
	kf, err := runKingfisher(ctx, birdObservationRequest(capabilityKingfisher, "probe", KingfisherProbeRequest{Observations: []map[string]any{{"available": true, "latency_ms": 10}, {"available": false, "latency_ms": 30}}}))
	if err != nil || kf.(KingfisherProbeResponse).AvailabilityPercent != 50 {
		t.Fatalf("kingfisher fallback: %#v %v", kf, err)
	}
	wood, err := runWoodpecker(ctx, birdObservationRequest(capabilityWoodpecker, "validate", WoodpeckerValidateRequest{SchemaRef: "signal.v1;required:tenant_id,kind", Records: []map[string]any{{"tenant_id": "t"}}}))
	if err != nil || wood.(WoodpeckerValidateResponse).Valid {
		t.Fatalf("woodpecker validation: %#v %v", wood, err)
	}
	heron, err := runHeron(ctx, birdObservationRequest(capabilityHeron, "reconcile", HeronReconcileRequest{JoinKey: "id", Sources: []string{"a"}}))
	if err != nil || len(heron.(HeronReconcileResponse).Discrepancies) != 1 {
		t.Fatalf("heron bounded insufficiency: %#v %v", heron, err)
	}
	raven, err := runRaven(ctx, birdObservationRequest(capabilityRaven, "evidence", RavenEvidenceRequest{IncidentID: "i", EventRefs: []string{"z", "a", "z"}}))
	if err != nil || raven.(RavenEvidenceResponse).EvidenceRefs[0] != "a" {
		t.Fatalf("raven ordering: %#v %v", raven, err)
	}
}

func TestEagleExtractsVerifiedLocalJSON(t *testing.T) {
	f, err := os.CreateTemp("", "eagle-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	content := []byte(`{"answer":42}`)
	if _, err = f.Write(content); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	eagle, err := runEagle(context.Background(), birdObservationRequest(capabilityEagle, "extract", EagleExtractRequest{ArtifactURI: f.Name(), MediaType: "application/json", SHA256: hex.EncodeToString(digest[:])}))
	if err != nil || len(eagle.(EagleExtractResponse).Facts) != 1 {
		t.Fatalf("eagle extraction: %#v %v", eagle, err)
	}
}
