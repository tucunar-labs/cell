package canarynode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const capabilityContractVersion = "1.0"

// CapabilityID identifies a logical capability in a Tenant Cell. It is a
// contract identity, not a process name or a promise of independent scaling.
type CapabilityID string

const (
	capabilitySwift       CapabilityID = "swift"
	capabilityCanary      CapabilityID = "canary"
	capabilityKingfisher  CapabilityID = "kingfisher"
	capabilityKingbird    CapabilityID = "kingbird"
	capabilityPigeon      CapabilityID = "pigeon"
	capabilityWoodpecker  CapabilityID = "woodpecker"
	capabilityHeron       CapabilityID = "heron"
	capabilityEagle       CapabilityID = "eagle"
	capabilityKestrel     CapabilityID = "kestrel"
	capabilityHawk        CapabilityID = "hawk"
	capabilityRaven       CapabilityID = "raven"
	capabilityPhoenix     CapabilityID = "phoenix"
	capabilityLark        CapabilityID = "lark"
	capabilityOwl         CapabilityID = "owl"
	capabilityToucan      CapabilityID = "toucan"
	capabilitySignalStore CapabilityID = "signal_store"
	capabilityAIReasoning CapabilityID = "ai_reasoning"
)

type CapabilityOperation struct {
	Name        string
	Input       string
	Output      string
	SideEffects []string
}

type CapabilityDefinition struct {
	ID          CapabilityID
	Description string
	Operations  []CapabilityOperation
}

var capabilityDefinitions = []CapabilityDefinition{
	{
		ID:          capabilitySwift,
		Description: "Normalize source protocols into canonical signals.",
		Operations:  []CapabilityOperation{{Name: "ingest", Input: "SwiftIngestRequest", Output: "SwiftIngestResponse"}},
	},
	{
		ID:          capabilityCanary,
		Description: "Evaluate operational rules from Condor/ClickHouse lookback windows.",
		Operations:  []CapabilityOperation{{Name: "evaluate", Input: "CanaryEvaluateRequest", Output: "CanaryEvaluateResponse", SideEffects: []string{"incident_lifecycle", "cloud_outbox"}}},
	},
	{
		ID:          capabilityKingfisher,
		Description: "Measure target reachability and transport health.",
		Operations:  []CapabilityOperation{{Name: "probe", Input: "KingfisherProbeRequest", Output: "KingfisherProbeResponse"}},
	},
	{
		ID:          capabilityKingbird,
		Description: "Correlate observations into probable causes.",
		Operations:  []CapabilityOperation{{Name: "correlate", Input: "KingbirdCorrelateRequest", Output: "KingbirdCorrelateResponse"}},
	},
	{
		ID:          capabilityPigeon,
		Description: "Deliver approved incidents to configured channels.",
		Operations:  []CapabilityOperation{{Name: "dispatch", Input: "PigeonDispatchRequest", Output: "PigeonDispatchResponse", SideEffects: []string{"delivery"}}},
	},
	{
		ID:          capabilityWoodpecker,
		Description: "Validate records against a declared data contract.",
		Operations:  []CapabilityOperation{{Name: "validate", Input: "WoodpeckerValidateRequest", Output: "WoodpeckerValidateResponse"}},
	},
	{
		ID:          capabilityHeron,
		Description: "Reconcile facts from multiple systems with explicit join rules.",
		Operations:  []CapabilityOperation{{Name: "reconcile", Input: "HeronReconcileRequest", Output: "HeronReconcileResponse"}},
	},
	{
		ID:          capabilityEagle,
		Description: "Extract structured facts from a referenced artifact.",
		Operations:  []CapabilityOperation{{Name: "extract", Input: "EagleExtractRequest", Output: "EagleExtractResponse"}},
	},
	{
		ID:          capabilityKestrel,
		Description: "Authorize a principal action on a resource.",
		Operations:  []CapabilityOperation{{Name: "authorize", Input: "KestrelAuthorizeRequest", Output: "KestrelAuthorizeResponse"}},
	},
	{
		ID:          capabilityHawk,
		Description: "Assess asset exposure and control coverage.",
		Operations:  []CapabilityOperation{{Name: "assess", Input: "HawkAssessRequest", Output: "HawkAssessResponse"}},
	},
	{
		ID:          capabilityRaven,
		Description: "Build an incident evidence timeline.",
		Operations:  []CapabilityOperation{{Name: "evidence", Input: "RavenEvidenceRequest", Output: "RavenEvidenceResponse"}},
	},
	{
		ID:          capabilityPhoenix,
		Description: "Plan and verify recovery work.",
		Operations:  []CapabilityOperation{{Name: "recover", Input: "PhoenixRecoverRequest", Output: "PhoenixRecoverResponse", SideEffects: []string{"recovery_record"}}},
	},
	{
		ID:          capabilityLark,
		Description: "Track ownership and workflow transitions.",
		Operations:  []CapabilityOperation{{Name: "handoff", Input: "LarkHandoffRequest", Output: "LarkHandoffResponse", SideEffects: []string{"workflow_record"}}},
	},
	{
		ID:          capabilityOwl,
		Description: "Produce a bounded decision recommendation from evidence.",
		Operations:  []CapabilityOperation{{Name: "decide", Input: "OwlDecideRequest", Output: "OwlDecideResponse"}},
	},
	{
		ID:          capabilityToucan,
		Description: "Compose capability outputs into a workflow outcome.",
		Operations:  []CapabilityOperation{{Name: "compose", Input: "ToucanComposeRequest", Output: "ToucanComposeResponse"}},
	},
	{
		ID:          capabilitySignalStore,
		Description: "Append and query tenant-scoped ClickHouse data.",
		Operations: []CapabilityOperation{
			{Name: "append", Input: "SignalStoreAppendRequest", Output: "SignalStoreAppendResponse", SideEffects: []string{"clickhouse"}},
			{Name: "query", Input: "SignalStoreQueryRequest", Output: "SignalStoreQueryResponse"},
		},
	},
	{
		ID:          capabilityAIReasoning,
		Description: "Generate a tenant-scoped recommendation from redacted context.",
		Operations: []CapabilityOperation{
			{Name: "reason", Input: "AIReasoningRequest", Output: "AIReasoningResponse"},
			{Name: "console_story", Input: "AIConsoleStoryRequest", Output: "AIConsoleStoryResponse"},
		},
	},
}

// CapabilityDefinitions returns a defensive copy for diagnostics, tests, and
// future topology reporting. Callers cannot mutate the registry.
func CapabilityDefinitions() []CapabilityDefinition {
	definitions := make([]CapabilityDefinition, len(capabilityDefinitions))
	for index, definition := range capabilityDefinitions {
		definitions[index] = definition
		definitions[index].Operations = append([]CapabilityOperation(nil), definition.Operations...)
		for operationIndex := range definitions[index].Operations {
			definitions[index].Operations[operationIndex].SideEffects = append([]string(nil), definition.Operations[operationIndex].SideEffects...)
		}
	}
	return definitions
}

func capabilityDefinition(id CapabilityID) (CapabilityDefinition, bool) {
	for _, definition := range capabilityDefinitions {
		if definition.ID == id {
			return definition, true
		}
	}
	return CapabilityDefinition{}, false
}

type CapabilityRequest struct {
	ContractVersion string          `json:"contract_version"`
	RequestID       string          `json:"request_id"`
	CorrelationID   string          `json:"correlation_id,omitempty"`
	IdempotencyKey  string          `json:"idempotency_key,omitempty"`
	OrganizationID  string          `json:"organization_id"`
	CellID          string          `json:"cell_id"`
	NodeID          string          `json:"node_id"`
	Capability      CapabilityID    `json:"capability"`
	Operation       string          `json:"operation"`
	Payload         json.RawMessage `json:"payload"`
}

type CapabilityResponse struct {
	ContractVersion string          `json:"contract_version"`
	RequestID       string          `json:"request_id"`
	Capability      CapabilityID    `json:"capability"`
	Operation       string          `json:"operation"`
	Status          string          `json:"status"`
	Payload         json.RawMessage `json:"payload,omitempty"`
	ErrorCode       string          `json:"error_code,omitempty"`
	ErrorMessage    string          `json:"error_message,omitempty"`
}

var errCapabilityNotImplemented = errors.New("capability operation is not implemented")

type CapabilityExecutor interface {
	Definition() CapabilityDefinition
	Execute(context.Context, CapabilityRequest) (CapabilityResponse, error)
}

type capabilityStub struct {
	definition CapabilityDefinition
}

func (s capabilityStub) Definition() CapabilityDefinition { return s.definition }

func (s capabilityStub) Execute(ctx context.Context, request CapabilityRequest) (CapabilityResponse, error) {
	ctx, span := tracer.Start(ctx, "capability."+string(s.definition.ID)+"."+request.Operation,
		withAttributes(
			spanString("tucunar.capability", string(s.definition.ID)),
			spanString("tucunar.operation", request.Operation),
			spanString("tucunar.organization_id", request.OrganizationID),
			spanString("tucunar.cell_id", request.CellID),
			spanString("tucunar.request_id", request.RequestID),
		))
	defer span.End()

	if err := validateCapabilityRequest(request, s.definition); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return CapabilityResponse{}, err
	}
	response := CapabilityResponse{
		ContractVersion: capabilityContractVersion,
		RequestID:       request.RequestID,
		Capability:      s.definition.ID,
		Operation:       request.Operation,
		Status:          "not_implemented",
		ErrorCode:       "not_implemented",
		ErrorMessage:    errCapabilityNotImplemented.Error(),
	}
	span.SetAttributes(spanString("tucunar.stub", "true"))
	return response, errCapabilityNotImplemented
}

func validateCapabilityRequest(request CapabilityRequest, definition CapabilityDefinition) error {
	if request.ContractVersion != capabilityContractVersion {
		return fmt.Errorf("unsupported capability contract version %q", request.ContractVersion)
	}
	if strings.TrimSpace(request.RequestID) == "" {
		return errors.New("request_id is required")
	}
	if strings.TrimSpace(request.OrganizationID) == "" || strings.TrimSpace(request.CellID) == "" || strings.TrimSpace(request.NodeID) == "" {
		return errors.New("organization_id, cell_id, and node_id are required")
	}
	if request.Capability != definition.ID {
		return fmt.Errorf("request capability %q does not match stub %q", request.Capability, definition.ID)
	}
	for _, operation := range definition.Operations {
		if request.Operation == operation.Name {
			if len(request.Payload) == 0 || !json.Valid(request.Payload) {
				return errors.New("payload must be valid JSON")
			}
			return nil
		}
	}
	return fmt.Errorf("unsupported operation %q for capability %q", request.Operation, definition.ID)
}

type SwiftStub struct{ capabilityStub }
type CanaryStub struct{ capabilityStub }
type KingfisherStub struct{ capabilityStub }
type KingbirdStub struct{ capabilityStub }
type PigeonStub struct{ capabilityStub }
type WoodpeckerStub struct{ capabilityStub }
type HeronStub struct{ capabilityStub }
type EagleStub struct{ capabilityStub }
type KestrelStub struct{ capabilityStub }
type HawkStub struct{ capabilityStub }
type RavenStub struct{ capabilityStub }
type PhoenixStub struct{ capabilityStub }
type LarkStub struct{ capabilityStub }
type OwlStub struct{ capabilityStub }
type ToucanStub struct{ capabilityStub }
type SignalStoreStub struct{ capabilityStub }
type AIReasoningStub struct{ capabilityStub }

func capabilityStubFor(id CapabilityID) capabilityStub {
	for _, definition := range capabilityDefinitions {
		if definition.ID == id {
			return capabilityStub{definition: definition}
		}
	}
	panic("missing capability definition: " + string(id))
}

func NewSwiftStub() SwiftStub   { return SwiftStub{capabilityStubFor(capabilitySwift)} }
func NewCanaryStub() CanaryStub { return CanaryStub{capabilityStubFor(capabilityCanary)} }
func NewKingfisherStub() KingfisherStub {
	return KingfisherStub{capabilityStubFor(capabilityKingfisher)}
}
func NewKingbirdStub() KingbirdStub { return KingbirdStub{capabilityStubFor(capabilityKingbird)} }
func NewPigeonStub() PigeonStub     { return PigeonStub{capabilityStubFor(capabilityPigeon)} }
func NewWoodpeckerStub() WoodpeckerStub {
	return WoodpeckerStub{capabilityStubFor(capabilityWoodpecker)}
}
func NewHeronStub() HeronStub     { return HeronStub{capabilityStubFor(capabilityHeron)} }
func NewEagleStub() EagleStub     { return EagleStub{capabilityStubFor(capabilityEagle)} }
func NewKestrelStub() KestrelStub { return KestrelStub{capabilityStubFor(capabilityKestrel)} }
func NewHawkStub() HawkStub       { return HawkStub{capabilityStubFor(capabilityHawk)} }
func NewRavenStub() RavenStub     { return RavenStub{capabilityStubFor(capabilityRaven)} }
func NewPhoenixStub() PhoenixStub { return PhoenixStub{capabilityStubFor(capabilityPhoenix)} }
func NewLarkStub() LarkStub       { return LarkStub{capabilityStubFor(capabilityLark)} }
func NewOwlStub() OwlStub         { return OwlStub{capabilityStubFor(capabilityOwl)} }
func NewToucanStub() ToucanStub   { return ToucanStub{capabilityStubFor(capabilityToucan)} }
func NewSignalStoreStub() SignalStoreStub {
	return SignalStoreStub{capabilityStubFor(capabilitySignalStore)}
}
func NewAIReasoningStub() AIReasoningStub {
	return AIReasoningStub{capabilityStubFor(capabilityAIReasoning)}
}

// NewCapabilityStubs exposes every proposed boundary for local contract tests.
// Production deployment uses one process per capability; this map does not
// register network listeners or create a shared runtime registry.
func NewCapabilityStubs() map[CapabilityID]CapabilityExecutor {
	return map[CapabilityID]CapabilityExecutor{
		capabilitySwift:       NewSwiftStub(),
		capabilityCanary:      NewCanaryStub(),
		capabilityKingfisher:  NewKingfisherStub(),
		capabilityKingbird:    NewKingbirdStub(),
		capabilityPigeon:      NewPigeonStub(),
		capabilityWoodpecker:  NewWoodpeckerStub(),
		capabilityHeron:       NewHeronStub(),
		capabilityEagle:       NewEagleStub(),
		capabilityKestrel:     NewKestrelStub(),
		capabilityHawk:        NewHawkStub(),
		capabilityRaven:       NewRavenStub(),
		capabilityPhoenix:     NewPhoenixStub(),
		capabilityLark:        NewLarkStub(),
		capabilityOwl:         NewOwlStub(),
		capabilityToucan:      NewToucanStub(),
		capabilitySignalStore: NewSignalStoreStub(),
		capabilityAIReasoning: NewAIReasoningStub(),
	}
}
