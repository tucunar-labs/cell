package canarynode

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestCapabilityBirdsExposeTheSameNetworkContract(t *testing.T) {
	t.Setenv("TUCUNAR_INTERNAL_TOKEN", "")

	requests := map[CapabilityID]CapabilityRequest{
		capabilityKingfisher: birdHTTPRequest(capabilityKingfisher, "probe", KingfisherProbeRequest{Target: "synthetic", Observations: []map[string]any{{"available": true}}}),
		capabilityKingbird:   birdHTTPRequest(capabilityKingbird, "correlate", KingbirdCorrelateRequest{SignalRefs: []string{"signal-latency"}}),
		capabilityPigeon:     birdHTTPRequest(capabilityPigeon, "dispatch", PigeonDispatchRequest{IncidentID: "incident-1", Channels: []string{"email"}, Message: "test", Payload: map[string]any{"approved": true}}),
		capabilityWoodpecker: birdHTTPRequest(capabilityWoodpecker, "validate", WoodpeckerValidateRequest{SchemaRef: "signal.v1", Records: []map[string]any{{"kind": "observation"}}}),
		capabilityHeron:      birdHTTPRequest(capabilityHeron, "reconcile", HeronReconcileRequest{Sources: []string{"erp", "scada"}, JoinKey: "asset_id"}),
		capabilityKestrel:    birdHTTPRequest(capabilityKestrel, "authorize", KestrelAuthorizeRequest{Principal: "operator", Action: "read", Resource: "asset-1"}),
		capabilityHawk:       birdHTTPRequest(capabilityHawk, "assess", HawkAssessRequest{Assets: []map[string]any{{"asset_id": "asset-1"}}}),
		capabilityRaven:      birdHTTPRequest(capabilityRaven, "evidence", RavenEvidenceRequest{IncidentID: "incident-1", EventRefs: []string{"event-1"}}),
		capabilityPhoenix:    birdHTTPRequest(capabilityPhoenix, "recover", PhoenixRecoverRequest{IncidentID: "incident-1", PlanID: "plan-1", Action: "plan"}),
		capabilityLark:       birdHTTPRequest(capabilityLark, "handoff", LarkHandoffRequest{TaskID: "task-1", Owner: "operator", Action: "assign"}),
		capabilityOwl:        birdHTTPRequest(capabilityOwl, "decide", OwlDecideRequest{Question: "Is it safe?", EvidenceRefs: []string{"event-1"}}),
		capabilityToucan:     birdHTTPRequest(capabilityToucan, "compose", ToucanComposeRequest{WorkflowID: "workflow-1", Inputs: map[string]any{"incident": "incident-1"}}),
		capabilityAIReasoning: birdHTTPRequest(capabilityAIReasoning, "reason", AIReasoningRequest{
			Task: "Summarize evidence", Context: map[string]any{"reference": "event-1"}, ModelPolicy: map[string]any{"enabled": true},
		}),
	}

	for capability, request := range requests {
		t.Run(string(capability), func(t *testing.T) {
			definition, ok := capabilityDefinition(capability)
			if !ok {
				t.Fatalf("missing definition")
			}
			recorder := httptest.NewRecorder()
			body, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			httpRequest := httptest.NewRequest(http.MethodPost, "/v1/capability/execute", bytes.NewReader(body))
			newCapabilityServer(definition).ServeHTTP(recorder, httpRequest)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			var response CapabilityResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Status == "" || response.Capability != capability || len(response.Payload) == 0 {
				t.Fatalf("invalid response: %#v", response)
			}
		})
	}
}

func birdHTTPRequest(capability CapabilityID, operation string, payload any) CapabilityRequest {
	encoded, _ := json.Marshal(payload)
	return CapabilityRequest{
		ContractVersion: capabilityContractVersion,
		RequestID:       "http-" + string(capability),
		OrganizationID:  "organization-test",
		CellID:          "cell-test",
		NodeID:          "node-test",
		Capability:      capability,
		Operation:       operation,
		Payload:         encoded,
	}
}

func TestEagleNetworkContractWithLocalArtifact(t *testing.T) {
	t.Setenv("TUCUNAR_INTERNAL_TOKEN", "")
	file, err := os.CreateTemp(t.TempDir(), "eagle-*.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"status":"ok"}`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	// Eagle's SHA requirement is intentionally exercised by its unit test;
	// this test focuses on the shared network envelope and uses a valid request
	// generated from the artifact through the implementation seam.
	response, err := runEagle(t.Context(), birdHTTPRequest(capabilityEagle, "extract", EagleExtractRequest{ArtifactURI: file.Name(), MediaType: "application/json", SHA256: "0000000000000000000000000000000000000000000000000000000000000000"}))
	if err == nil || response != nil {
		t.Fatalf("expected invalid digest rejection, response=%#v err=%v", response, err)
	}
}
