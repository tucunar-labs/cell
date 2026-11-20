package canarynode

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"
)

const (
	maxRulesFileBytes      = 1 << 20
	maxThresholdRules      = 100
	maxRuleWindowSeconds   = 30 * 24 * 60 * 60
	maxRuleIdentifierBytes = 128
)

// ThresholdOperator is deliberately a closed set. The value is the only
// configuration fragment used in the fixed ClickHouse HAVING expression.
type ThresholdOperator string

const (
	OperatorGT  ThresholdOperator = "gt"
	OperatorGTE ThresholdOperator = "gte"
	OperatorLT  ThresholdOperator = "lt"
	OperatorLTE ThresholdOperator = "lte"
)

const (
	AggregateAverage = "avg"
	AggregateSum     = "sum"
	AggregateMin     = "min"
	AggregateMax     = "max"
	AggregateP50     = "p50"
	AggregateP90     = "p90"
	AggregateP95     = "p95"
	AggregateP99     = "p99"
)

// ThresholdRule describes a numeric aggregate over a ClickHouse lookback
// window, grouped by tenant and optionally entity. Aggregate defaults to avg
// and is selected from a closed whitelist before it reaches SQL.
type ThresholdRule struct {
	ID            string            `json:"rule_id"`
	Source        string            `json:"source"`
	Kind          string            `json:"kind"`
	Name          string            `json:"name"`
	Aggregate     string            `json:"aggregate,omitempty"`
	Operator      ThresholdOperator `json:"operator"`
	Threshold     float64           `json:"threshold"`
	WindowSeconds int               `json:"window_seconds"`
	GroupBy       []string          `json:"group_by,omitempty"`
}

// ThresholdAlert is the source-neutral result emitted by a threshold rule.
type ThresholdAlert struct {
	RuleID        string
	Source        string
	TenantID      string
	EntityID      string
	Aggregate     string
	ObservedValue float64
	Threshold     float64
	Operator      ThresholdOperator
	Window        time.Duration
}

type thresholdRulesFile struct {
	Rules []ThresholdRule `json:"rules"`
}

// LoadThresholdRules loads a strict local JSON file. An empty path intentionally
// disables generic rules, which keeps the default node behavior unchanged.
func LoadThresholdRules(path string) ([]ThresholdRule, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat rules file: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("rules path must be a file")
	}
	if info.Size() > maxRulesFileBytes {
		return nil, fmt.Errorf("rules file exceeds %d bytes", maxRulesFileBytes)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read rules file: %w", err)
	}
	var document thresholdRulesFile
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode rules file: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != nil && err != io.EOF {
		return nil, fmt.Errorf("decode rules file trailing JSON: %w", err)
	} else if err == nil {
		return nil, fmt.Errorf("rules file must contain one JSON object")
	}
	if len(document.Rules) > maxThresholdRules {
		return nil, fmt.Errorf("rules cannot contain more than %d items", maxThresholdRules)
	}

	rules := make([]ThresholdRule, 0, len(document.Rules))
	seenIDs := make(map[string]struct{}, len(document.Rules))
	for index, rule := range document.Rules {
		normalized, err := rule.Validate()
		if err != nil {
			return nil, fmt.Errorf("rules[%d]: %w", index, err)
		}
		if _, exists := seenIDs[normalized.ID]; exists {
			return nil, fmt.Errorf("rules[%d]: duplicate rule_id %q", index, normalized.ID)
		}
		seenIDs[normalized.ID] = struct{}{}
		rules = append(rules, normalized)
	}
	return rules, nil
}

// Validate normalizes and validates every user-controlled rule field before it
// can reach the evaluator or query builder.
func (r ThresholdRule) Validate() (ThresholdRule, error) {
	r.ID = strings.TrimSpace(r.ID)
	r.Source = strings.TrimSpace(r.Source)
	r.Kind = strings.TrimSpace(r.Kind)
	r.Name = strings.TrimSpace(r.Name)
	r.Aggregate = strings.TrimSpace(r.Aggregate)
	if r.Aggregate == "" {
		r.Aggregate = AggregateAverage
	}
	for field, value := range map[string]string{
		"rule_id": r.ID,
		"source":  r.Source,
		"kind":    r.Kind,
		"name":    r.Name,
	} {
		if value == "" {
			return ThresholdRule{}, fmt.Errorf("%s is required", field)
		}
		if len(value) > maxRuleIdentifierBytes {
			return ThresholdRule{}, fmt.Errorf("%s is too long", field)
		}
	}
	if !validRuleID(r.ID) {
		return ThresholdRule{}, fmt.Errorf("rule_id contains unsupported characters")
	}
	if !validThresholdOperator(r.Operator) {
		return ThresholdRule{}, fmt.Errorf("operator must be one of gt, gte, lt, lte")
	}
	if !validThresholdAggregate(r.Aggregate) {
		return ThresholdRule{}, fmt.Errorf("aggregate must be one of avg, sum, min, max, p50, p90, p95, p99")
	}
	if math.IsNaN(r.Threshold) || math.IsInf(r.Threshold, 0) {
		return ThresholdRule{}, fmt.Errorf("threshold must be finite")
	}
	if r.WindowSeconds < 1 || r.WindowSeconds > maxRuleWindowSeconds {
		return ThresholdRule{}, fmt.Errorf("window_seconds must be between 1 and %d", maxRuleWindowSeconds)
	}
	groupBy, err := normalizeGroupBy(r.GroupBy)
	if err != nil {
		return ThresholdRule{}, err
	}
	r.GroupBy = groupBy
	return r, nil
}

func validThresholdAggregate(aggregate string) bool {
	switch aggregate {
	case AggregateAverage, AggregateSum, AggregateMin, AggregateMax,
		AggregateP50, AggregateP90, AggregateP95, AggregateP99:
		return true
	default:
		return false
	}
}

func validRuleID(value string) bool {
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' || char == ':' {
			continue
		}
		return false
	}
	return true
}

func validThresholdOperator(operator ThresholdOperator) bool {
	switch operator {
	case OperatorGT, OperatorGTE, OperatorLT, OperatorLTE:
		return true
	default:
		return false
	}
}

func normalizeGroupBy(groupBy []string) ([]string, error) {
	if len(groupBy) == 0 {
		return []string{"tenant_id", "entity_id"}, nil
	}
	seen := make(map[string]struct{}, len(groupBy))
	for _, field := range groupBy {
		if field != "tenant_id" && field != "entity_id" {
			return nil, fmt.Errorf("group_by only supports tenant_id and entity_id")
		}
		if _, exists := seen[field]; exists {
			return nil, fmt.Errorf("group_by contains duplicate field %q", field)
		}
		seen[field] = struct{}{}
	}
	if _, exists := seen["tenant_id"]; !exists {
		return nil, fmt.Errorf("group_by must include tenant_id")
	}
	if _, entity := seen["entity_id"]; entity {
		return []string{"tenant_id", "entity_id"}, nil
	}
	return []string{"tenant_id"}, nil
}

func mapThresholdResult(rule ThresholdRule, tenantID, entityID string, observedValue float64) ThresholdAlert {
	if normalized, err := rule.Validate(); err == nil {
		rule = normalized
	}
	return ThresholdAlert{
		RuleID:        rule.ID,
		Source:        rule.Source,
		TenantID:      tenantID,
		EntityID:      entityID,
		Aggregate:     rule.Aggregate,
		ObservedValue: observedValue,
		Threshold:     rule.Threshold,
		Operator:      rule.Operator,
		Window:        time.Duration(rule.WindowSeconds) * time.Second,
	}
}
