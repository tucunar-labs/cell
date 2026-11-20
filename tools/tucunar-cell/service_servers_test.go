package canarynode

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	tucunarv1 "github.com/tucunar/contracts/gen/tucunar/v1"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestCondorSignalFromProtoBindsTenantContext(t *testing.T) {
	context := &tucunarv1.TenantContext{
		ContractVersion: "1.0",
		RequestId:       "request-1",
		IdempotencyKey:  "append-1",
		OrganizationId:  "org-1",
		CellId:          "cell-1",
		NodeId:          "node-1",
	}
	payload, err := structpb.NewStruct(map[string]any{"source": "operator"})
	if err != nil {
		t.Fatalf("build payload: %v", err)
	}
	occurredAt := timestamppb.New(time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC))
	got, err := condorSignalFromProto(context, &tucunarv1.Signal{
		ExternalId: "reading-1",
		Source:     "crane-monitor",
		TenantId:   "mine-1",
		EntityId:   "crane-17",
		Kind:       "measurement",
		Name:       "load",
		Value:      8.4,
		Unit:       "tons",
		Dimensions: map[string]string{"site": "north"},
		Payload:    payload,
		OccurredAt: occurredAt,
	})
	if err != nil {
		t.Fatalf("convert signal: %v", err)
	}
	if got.OrganizationID != "org-1" || got.CanaryID != "cell-1" || got.ExternalID != "reading-1" {
		t.Fatalf("tenant binding mismatch: %+v", got)
	}
	if got.OccurredAt != occurredAt.AsTime() || got.Dimensions["site"] != "north" {
		t.Fatalf("signal fields mismatch: %+v", got)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got.Payload, &decoded); err != nil || decoded["source"] != "operator" {
		t.Fatalf("payload conversion mismatch: %s", got.Payload)
	}
}

func TestCondorSignalFromProtoRejectsScopeMismatch(t *testing.T) {
	context := &tucunarv1.TenantContext{OrganizationId: "org-1", CellId: "cell-1"}
	_, err := condorSignalFromProto(context, &tucunarv1.Signal{
		OrganizationId: "other-org",
		Source:         "source",
		TenantId:       "tenant",
		EntityId:       "entity",
		Kind:           "measurement",
		Name:           "value",
		OccurredAt:     timestamppb.Now(),
	})
	if err == nil {
		t.Fatal("expected organization scope mismatch")
	}
}

func TestCondorSignalToProtoPreservesPayloadAndTimestamp(t *testing.T) {
	occurredAt := time.Date(2026, 9, 6, 12, 0, 0, 123_000_000, time.UTC)
	got, err := condorSignalToProto(Signal{
		ID:             "550e8400-e29b-41d4-a716-446655440000",
		OrganizationID: "org-1",
		CanaryID:       "cell-1",
		Source:         "source",
		TenantID:       "tenant",
		EntityID:       "entity",
		Kind:           "measurement",
		Name:           "load",
		Payload:        json.RawMessage(`{"message":"ok"}`),
		OccurredAt:     occurredAt,
	})
	if err != nil {
		t.Fatalf("encode signal: %v", err)
	}
	if got.OrganizationId != "org-1" || got.CanaryId != "cell-1" || !got.OccurredAt.AsTime().Equal(occurredAt) {
		t.Fatalf("signal conversion mismatch: %+v", got)
	}
	encoded, err := json.Marshal(got.Payload)
	if err != nil || string(encoded) != `{"message":"ok"}` {
		t.Fatalf("payload conversion mismatch: %s", encoded)
	}
}

func TestValidateCondorScopeRejectsOtherCell(t *testing.T) {
	store := &Store{organizationID: "org-1", canaryID: "cell-1"}
	context := &tucunarv1.TenantContext{OrganizationId: "org-1", CellId: "other-cell"}
	if err := validateCondorScope(store, context); err == nil {
		t.Fatal("expected cell scope mismatch")
	}
}

func TestValidateCondorConsumerBoundsAndTrims(t *testing.T) {
	if got, err := validateCondorConsumer("  evaluator  "); err != nil || got != "evaluator" {
		t.Fatalf("expected trimmed consumer, got %q (%v)", got, err)
	}
	if _, err := validateCondorConsumer(""); err == nil {
		t.Fatal("expected empty consumer to be rejected")
	}
	if _, err := validateCondorConsumer(strings.Repeat("x", 129)); err == nil {
		t.Fatal("expected oversized consumer to be rejected")
	}
}

func TestAuthorizeInternalHeadersUsesBearerToken(t *testing.T) {
	t.Setenv("TUCUNAR_INTERNAL_TOKEN", "internal-secret")
	if !authorizeInternalHeaders(http.Header{"Authorization": []string{"Bearer internal-secret"}}) {
		t.Fatal("expected bearer token to authorize")
	}
	if authorizeInternalHeaders(http.Header{"Authorization": []string{"Bearer wrong"}}) {
		t.Fatal("expected wrong bearer token to be rejected")
	}
}
