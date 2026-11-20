package canarynode

import "time"

const cloudContractVersion = "1.0"

type cloudWindow struct {
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Seconds int       `json:"seconds"`
}

type enrollmentRequest struct {
	ContractVersion string   `json:"contract_version"`
	OrganizationID  string   `json:"organization_id"`
	CanaryID        string   `json:"canary_id"`
	DeploymentMode  string   `json:"deployment_mode"`
	NodeInstanceID  string   `json:"node_instance_id"`
	NodeVersion     string   `json:"node_version"`
	Capabilities    []string `json:"capabilities"`
}

type enrollmentResponse struct {
	ContractVersion      string `json:"contract_version"`
	OrganizationID       string `json:"organization_id"`
	CanaryID             string `json:"canary_id"`
	NodeID               string `json:"node_id"`
	DeploymentMode       string `json:"deployment_mode"`
	AccessToken          string `json:"access_token"`
	AccessTokenExpiresAt string `json:"access_token_expires_at"`
	Reporting            struct {
		HeartbeatIntervalSeconds int `json:"heartbeat_interval_seconds"`
		RollupIntervalSeconds    int `json:"rollup_interval_seconds"`
	} `json:"reporting"`
	RawEventForwarding bool `json:"raw_event_forwarding"`
}

type heartbeatRequest struct {
	ContractVersion string            `json:"contract_version"`
	OrganizationID  string            `json:"organization_id"`
	CanaryID        string            `json:"canary_id"`
	NodeID          string            `json:"node_id"`
	SentAt          time.Time         `json:"sent_at"`
	NodeVersion     string            `json:"node_version"`
	Status          string            `json:"status"`
	UptimeSeconds   int64             `json:"uptime_seconds"`
	Components      map[string]string `json:"components"`
	Data            heartbeatData     `json:"data"`
}

type heartbeatData struct {
	LookbackSeconds int        `json:"lookback_seconds"`
	LastEventAt     *time.Time `json:"last_event_at,omitempty"`
	OutboxPending   uint64     `json:"outbox_pending"`
	OutboxFailed    uint64     `json:"outbox_failed"`
	OutboxLimit     int        `json:"outbox_limit"`
}

type rollupRequest struct {
	ContractVersion string         `json:"contract_version"`
	OrganizationID  string         `json:"organization_id"`
	CanaryID        string         `json:"canary_id"`
	NodeID          string         `json:"node_id"`
	RollupID        string         `json:"rollup_id"`
	GeneratedAt     time.Time      `json:"generated_at"`
	Window          cloudWindow    `json:"window"`
	RuleSetVersion  string         `json:"rule_set_version"`
	Summary         rollupSummary  `json:"summary"`
	Shops           []shopRollup   `json:"shops"`
	Signals         []signalRollup `json:"signals"`
}

type rollupSummary struct {
	TotalShops      int     `json:"total_shops"`
	AtRiskShops     int     `json:"at_risk_shops"`
	CheckoutStarted uint64  `json:"checkout_started"`
	PaymentSuccess  uint64  `json:"payment_success"`
	ConversionRate  float64 `json:"conversion_rate"`
	TotalSignals    uint64  `json:"total_signals"`
	SourceCount     uint64  `json:"source_count"`
	EntityCount     uint64  `json:"entity_count"`
}

type shopRollup struct {
	ShopID          string  `json:"shop_id"`
	CheckoutStarted uint64  `json:"checkout_started"`
	PaymentSuccess  uint64  `json:"payment_success"`
	ConversionRate  float64 `json:"conversion_rate"`
	Status          string  `json:"status"`
}

type signalRollup struct {
	Source         string    `json:"source"`
	TenantID       string    `json:"tenant_id"`
	EntityID       string    `json:"entity_id"`
	Kind           string    `json:"kind"`
	Name           string    `json:"name"`
	Unit           string    `json:"unit"`
	Samples        uint64    `json:"samples"`
	AverageValue   float64   `json:"avg_value"`
	LastOccurredAt time.Time `json:"last_occurred_at"`
}

type incidentRequest struct {
	ContractVersion string          `json:"contract_version"`
	OrganizationID  string          `json:"organization_id"`
	CanaryID        string          `json:"canary_id"`
	NodeID          string          `json:"node_id"`
	EmissionID      string          `json:"emission_id"`
	IncidentKey     string          `json:"incident_key"`
	RuleID          string          `json:"rule_id"`
	LifecycleEvent  string          `json:"lifecycle_event"`
	Status          string          `json:"status"`
	OccurredAt      time.Time       `json:"occurred_at"`
	Family          string          `json:"family"`
	Source          string          `json:"source"`
	TenantID        string          `json:"tenant_id"`
	EntityID        string          `json:"entity_id"`
	ShopID          string          `json:"shop_id"`
	Window          cloudWindow     `json:"window"`
	Threshold       incidentLimit   `json:"threshold"`
	Metrics         incidentMetrics `json:"metrics"`
}

type incidentLimit struct {
	Operator string  `json:"operator"`
	Value    float64 `json:"value"`
}

type incidentMetrics struct {
	CheckoutStarted uint64  `json:"checkout_started"`
	PaymentSuccess  uint64  `json:"payment_success"`
	ConversionRate  float64 `json:"conversion_rate"`
	ObservedValue   float64 `json:"observed_value"`
	BaselineValue   float64 `json:"baseline_value"`
}
