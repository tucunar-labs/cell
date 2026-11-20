package canarynode

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRunAIReasoning(t *testing.T) {
	tests := []struct {
		name          string
		payload       AIReasoningRequest
		wantErr       string
		wantCitations []string
		wantAction    bool
	}{
		{"enabled", AIReasoningRequest{Task: "Summarize evidence", Context: map[string]any{"references": []any{"ref-2", "ref-1"}}, ModelPolicy: map[string]any{"enabled": true}}, "", []string{"ref-1", "ref-2"}, false},
		{"disabled", AIReasoningRequest{Task: "Analyze", Context: map[string]any{"status": "degraded"}, ModelPolicy: map[string]any{"enabled": false}}, "reasoning disabled by model_policy", nil, false},
		{"empty task", AIReasoningRequest{Context: map[string]any{}, ModelPolicy: map[string]any{"enabled": true}}, "task is required", nil, false},
		{"explicit action", AIReasoningRequest{Task: "Please deploy the fix", Context: map[string]any{"reference": "change-1"}, ModelPolicy: map[string]any{"enabled": true}}, "", []string{"change-1"}, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value, err := runAIReasoning(context.Background(), aiReasoningTestRequest(t, test.payload))
			if test.wantErr != "" {
				if err == nil || err.Error() != test.wantErr {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			response := value.(AIReasoningResponse)
			if !equalReasoningStrings(response.Citations, test.wantCitations) {
				t.Fatalf("citations = %#v, want %#v", response.Citations, test.wantCitations)
			}
			if response.ActionRequired != test.wantAction {
				t.Fatalf("action_required = %v, want %v", response.ActionRequired, test.wantAction)
			}
			if response.Confidence < 0 || response.Confidence > 1 || len([]rune(response.Answer)) > maxAIReasoningAnswer {
				t.Fatalf("invalid bounded response: %#v", response)
			}
		})
	}
}

func TestRunAIReasoningDoesNotEchoRawContext(t *testing.T) {
	value, err := runAIReasoning(context.Background(), aiReasoningTestRequest(t, AIReasoningRequest{Task: "Analyze", Context: map[string]any{"secret": "do-not-echo"}, ModelPolicy: map[string]any{"enabled": true}}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(value.(AIReasoningResponse).Answer, "do-not-echo") {
		t.Fatal("answer echoed raw context")
	}
}

func aiReasoningTestRequest(t *testing.T, payload AIReasoningRequest) CapabilityRequest {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return CapabilityRequest{ContractVersion: capabilityContractVersion, RequestID: "request-ai-reasoning-test", OrganizationID: "organization-test", CellID: "cell-test", NodeID: "node-test", Capability: capabilityAIReasoning, Operation: "reason", Payload: encoded}
}

func equalReasoningStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
