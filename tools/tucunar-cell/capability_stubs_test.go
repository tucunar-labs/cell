package canarynode

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestCapabilityStubsExposeTheContractInventory(t *testing.T) {
	definitions := CapabilityDefinitions()
	stubs := NewCapabilityStubs()
	if len(definitions) != 17 {
		t.Fatalf("expected 17 capability definitions, got %d", len(definitions))
	}
	if len(stubs) != len(definitions) {
		t.Fatalf("expected one stub per definition, got %d stubs for %d definitions", len(stubs), len(definitions))
	}

	for _, definition := range definitions {
		stub, ok := stubs[definition.ID]
		if !ok {
			t.Fatalf("missing stub for %q", definition.ID)
		}
		if len(definition.Operations) == 0 {
			t.Fatalf("capability %q has no operation", definition.ID)
		}
		if stub.Definition().ID != definition.ID {
			t.Fatalf("stub definition mismatch for %q", definition.ID)
		}
	}
}

func TestCapabilityStubValidatesScopeAndReturnsExplicitPlaceholder(t *testing.T) {
	stub := NewSwiftStub()
	request := CapabilityRequest{
		ContractVersion: capabilityContractVersion,
		RequestID:       "request-1",
		OrganizationID:  "org-1",
		CellID:          "cell-1",
		NodeID:          "node-1",
		Capability:      capabilitySwift,
		Operation:       "ingest",
		Payload:         json.RawMessage(`{"source":"test","protocol":"generic","payload":{}}`),
	}

	response, err := stub.Execute(context.Background(), request)
	if !errors.Is(err, errCapabilityNotImplemented) {
		t.Fatalf("expected explicit placeholder error, got %v", err)
	}
	if response.Status != "not_implemented" || response.ErrorCode != "not_implemented" {
		t.Fatalf("unexpected placeholder response: %+v", response)
	}
	if response.RequestID != request.RequestID || response.Capability != request.Capability {
		t.Fatalf("response lost request identity: %+v", response)
	}

	request.OrganizationID = ""
	if _, err := stub.Execute(context.Background(), request); err == nil {
		t.Fatal("expected missing organization scope to be rejected")
	}
}
