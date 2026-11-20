package canarynode

import "time"

// CapabilityContext is repeated by every internal capability request. The
// organization, cell, and node fields are mandatory tenancy boundaries; a
// caller must not infer them from an untrusted payload.
type CapabilityContext struct {
	ContractVersion string `json:"contract_version"`
	RequestID       string `json:"request_id"`
	CorrelationID   string `json:"correlation_id,omitempty"`
	IdempotencyKey  string `json:"idempotency_key,omitempty"`
	OrganizationID  string `json:"organization_id"`
	CellID          string `json:"cell_id"`
	NodeID          string `json:"node_id"`
}

type CapabilityResult struct {
	ContractVersion string `json:"contract_version"`
	RequestID       string `json:"request_id"`
	Capability      string `json:"capability"`
	Operation       string `json:"operation"`
	Status          string `json:"status"`
	ErrorCode       string `json:"error_code,omitempty"`
	ErrorMessage    string `json:"error_message,omitempty"`
}

type SwiftIngestRequest struct {
	CapabilityContext
	Source     string         `json:"source"`
	Protocol   string         `json:"protocol"`
	Payload    map[string]any `json:"payload"`
	ReceivedAt time.Time      `json:"received_at,omitempty"`
}

type SwiftIngestResponse struct {
	CapabilityResult
	Signals  []Signal         `json:"signals,omitempty"`
	Rejected []RejectedRecord `json:"rejected,omitempty"`
}

type CanaryEvaluateRequest struct {
	CapabilityContext
	WindowSeconds uint32   `json:"window_seconds"`
	RuleIDs       []string `json:"rule_ids"`
}

type CanaryEvaluateResponse struct {
	CapabilityResult
	Metrics   map[string]float64   `json:"metrics,omitempty"`
	Incidents []IncidentTransition `json:"incidents,omitempty"`
}

type KingfisherProbeRequest struct {
	CapabilityContext
	Target       string           `json:"target"`
	Protocol     string           `json:"protocol,omitempty"`
	TimeoutMS    uint32           `json:"timeout_ms,omitempty"`
	Observations []map[string]any `json:"observations,omitempty"`
}

type KingfisherProbeResponse struct {
	CapabilityResult
	ProbeStatus         string  `json:"probe_status,omitempty"`
	AvailabilityPercent float64 `json:"availability_percent,omitempty"`
	LatencyMS           float64 `json:"latency_ms,omitempty"`
	LossPercent         float64 `json:"loss_percent,omitempty"`
}

type KingbirdCorrelateRequest struct {
	CapabilityContext
	IncidentIDs  []string `json:"incident_ids,omitempty"`
	SignalRefs   []string `json:"signal_refs,omitempty"`
	TopologyRefs []string `json:"topology_refs,omitempty"`
}

type KingbirdCorrelateResponse struct {
	CapabilityResult
	Hypotheses []Hypothesis `json:"hypotheses,omitempty"`
}

type PigeonDispatchRequest struct {
	CapabilityContext
	IncidentID string         `json:"incident_id"`
	Channels   []string       `json:"channels"`
	Message    string         `json:"message,omitempty"`
	Payload    map[string]any `json:"payload,omitempty"`
}

type PigeonDispatchResponse struct {
	CapabilityResult
	DeliveryIDs []string `json:"delivery_ids,omitempty"`
	Attempts    uint32   `json:"attempts,omitempty"`
}

type WoodpeckerValidateRequest struct {
	CapabilityContext
	SchemaRef string           `json:"schema_ref"`
	Records   []map[string]any `json:"records"`
}

type WoodpeckerValidateResponse struct {
	CapabilityResult
	Valid          bool        `json:"valid"`
	RecordsChecked uint64      `json:"records_checked"`
	Violations     []Violation `json:"violations,omitempty"`
}

type HeronReconcileRequest struct {
	CapabilityContext
	Sources     []string  `json:"sources"`
	JoinKey     string    `json:"join_key"`
	WindowStart time.Time `json:"window_start,omitempty"`
	WindowEnd   time.Time `json:"window_end,omitempty"`
}

type HeronReconcileResponse struct {
	CapabilityResult
	Matched       uint64        `json:"matched"`
	Mismatched    uint64        `json:"mismatched"`
	Discrepancies []Discrepancy `json:"discrepancies,omitempty"`
}

type EagleExtractRequest struct {
	CapabilityContext
	ArtifactURI       string `json:"artifact_uri"`
	MediaType         string `json:"media_type"`
	SHA256            string `json:"sha256"`
	ExtractionProfile string `json:"extraction_profile,omitempty"`
}

type EagleExtractResponse struct {
	CapabilityResult
	Facts []Fact `json:"facts,omitempty"`
}

type KestrelAuthorizeRequest struct {
	CapabilityContext
	Principal string         `json:"principal"`
	Action    string         `json:"action"`
	Resource  string         `json:"resource"`
	Context   map[string]any `json:"context,omitempty"`
}

type KestrelAuthorizeResponse struct {
	CapabilityResult
	Allowed       bool     `json:"allowed"`
	Reasons       []string `json:"reasons,omitempty"`
	PolicyVersion string   `json:"policy_version,omitempty"`
}

type HawkAssessRequest struct {
	CapabilityContext
	Assets   []map[string]any `json:"assets"`
	Controls []map[string]any `json:"controls,omitempty"`
}

type HawkAssessResponse struct {
	CapabilityResult
	Findings []Finding `json:"findings,omitempty"`
}

type RavenEvidenceRequest struct {
	CapabilityContext
	IncidentID   string   `json:"incident_id"`
	EventRefs    []string `json:"event_refs,omitempty"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
}

type RavenEvidenceResponse struct {
	CapabilityResult
	Timeline     []map[string]any `json:"timeline,omitempty"`
	EvidenceRefs []string         `json:"evidence_refs,omitempty"`
}

type PhoenixRecoverRequest struct {
	CapabilityContext
	IncidentID string `json:"incident_id"`
	PlanID     string `json:"plan_id"`
	Action     string `json:"action"`
	Status     string `json:"status,omitempty"`
}

type PhoenixRecoverResponse struct {
	CapabilityResult
	State        string   `json:"state,omitempty"`
	NextAction   string   `json:"next_action,omitempty"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
}

type LarkHandoffRequest struct {
	CapabilityContext
	IncidentID string `json:"incident_id,omitempty"`
	TaskID     string `json:"task_id"`
	Owner      string `json:"owner,omitempty"`
	Action     string `json:"action"`
	Note       string `json:"note,omitempty"`
}

type LarkHandoffResponse struct {
	CapabilityResult
	State        string `json:"state,omitempty"`
	Owner        string `json:"owner,omitempty"`
	TransitionID string `json:"transition_id,omitempty"`
}

type OwlDecideRequest struct {
	CapabilityContext
	Question     string   `json:"question"`
	EvidenceRefs []string `json:"evidence_refs"`
	Constraints  []string `json:"constraints,omitempty"`
}

type OwlDecideResponse struct {
	CapabilityResult
	Decision     string   `json:"decision,omitempty"`
	Alternatives []string `json:"alternatives,omitempty"`
	Rationale    string   `json:"rationale,omitempty"`
	Confidence   float64  `json:"confidence,omitempty"`
}

type ToucanComposeRequest struct {
	CapabilityContext
	WorkflowID       string         `json:"workflow_id"`
	Inputs           map[string]any `json:"inputs"`
	RequestedOutputs []string       `json:"requested_outputs,omitempty"`
}

type ToucanComposeResponse struct {
	CapabilityResult
	State       string         `json:"state,omitempty"`
	Outputs     map[string]any `json:"outputs,omitempty"`
	NextActions []string       `json:"next_actions,omitempty"`
}

type SignalStoreAppendRequest struct {
	CapabilityContext
	Signals []Signal `json:"signals"`
}

type SignalStoreAppendResponse struct {
	CapabilityResult
	Accepted     uint64 `json:"accepted"`
	Deduplicated uint64 `json:"deduplicated"`
}

type SignalStoreQueryRequest struct {
	CapabilityContext
	View        string            `json:"view"`
	WindowStart time.Time         `json:"window_start"`
	WindowEnd   time.Time         `json:"window_end"`
	Filters     map[string]string `json:"filters,omitempty"`
	Limit       uint32            `json:"limit,omitempty"`
}

type SignalStoreQueryResponse struct {
	CapabilityResult
	Rows      []map[string]any `json:"rows,omitempty"`
	Truncated bool             `json:"truncated,omitempty"`
}

type AIReasoningRequest struct {
	CapabilityContext
	Task        string         `json:"task"`
	Context     map[string]any `json:"context"`
	ModelPolicy map[string]any `json:"model_policy"`
}

type AIReasoningResponse struct {
	CapabilityResult
	Answer         string   `json:"answer,omitempty"`
	Citations      []string `json:"citations,omitempty"`
	Confidence     float64  `json:"confidence,omitempty"`
	ActionRequired bool     `json:"action_required,omitempty"`
}

// AIConsoleStoryRequest is deliberately fact-shaped. Shiitake receives an
// ordered, bounded case read model, never an unbounded ClickHouse query or raw
// source payload. This lets a customer replace the AI process while keeping
// the same network contract.
type AIConsoleStoryRequest struct {
	CapabilityContext
	ScenarioID string           `json:"scenario_id"`
	Facts      ConsoleCaseFacts `json:"facts"`
}

type AIConsoleStoryResponse struct {
	CapabilityResult
	Story CaseStory `json:"story"`
}

type RejectedRecord struct {
	RecordRef string `json:"record_ref"`
	Reason    string `json:"reason"`
}

type Hypothesis struct {
	Cause        string   `json:"cause"`
	Confidence   float64  `json:"confidence"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
}

type Violation struct {
	Path     string `json:"path"`
	Message  string `json:"message"`
	Severity string `json:"severity,omitempty"`
}

type Discrepancy struct {
	JoinValue  string   `json:"join_value"`
	Reason     string   `json:"reason"`
	SourceRefs []string `json:"source_refs,omitempty"`
}

type Fact struct {
	Key        string   `json:"key"`
	Value      any      `json:"value"`
	Confidence float64  `json:"confidence"`
	Provenance []string `json:"provenance,omitempty"`
}

type Finding struct {
	FindingID string   `json:"finding_id"`
	Severity  string   `json:"severity"`
	Title     string   `json:"title"`
	AssetRefs []string `json:"asset_refs,omitempty"`
}
