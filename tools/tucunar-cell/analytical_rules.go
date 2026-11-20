package canarynode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

const (
	maxAnalyticalRules           = 100
	maxRuleSamples               = 1_000_000_000
	defaultRuleMinSamples uint64 = 3
)

type AnalyticalRuleFamily string

const (
	RuleFamilyAbsence      AnalyticalRuleFamily = "absence"
	RuleFamilyRatio        AnalyticalRuleFamily = "ratio"
	RuleFamilyWindowChange AnalyticalRuleFamily = "window_change"
	RuleAggregateCount     string               = "count"
	RuleAggregateAverage   string               = "avg"
)

// AnalyticalRule adds bounded rule families without changing the original
// ThresholdRule JSON contract. It is loaded from ANALYTICAL_RULES_FILE.
type AnalyticalRule struct {
	ID                    string               `json:"rule_id"`
	Family                AnalyticalRuleFamily `json:"family"`
	Source                string               `json:"source"`
	Kind                  string               `json:"kind"`
	Name                  string               `json:"name"`
	Numerator             string               `json:"numerator,omitempty"`
	Denominator           string               `json:"denominator,omitempty"`
	Operator              ThresholdOperator    `json:"operator,omitempty"`
	Threshold             float64              `json:"threshold,omitempty"`
	WindowSeconds         int                  `json:"window_seconds"`
	BaselineWindowSeconds int                  `json:"baseline_window_seconds,omitempty"`
	MinSamples            uint64               `json:"min_samples,omitempty"`
	Aggregate             string               `json:"aggregate,omitempty"`
	GroupBy               []string             `json:"group_by,omitempty"`
}

type analyticalRulesFile struct {
	Rules []AnalyticalRule `json:"rules"`
}

type AnalyticalAlert struct {
	RuleID             string
	Family             AnalyticalRuleFamily
	Source             string
	TenantID           string
	EntityID           string
	ObservedValue      float64
	BaselineValue      float64
	Threshold          float64
	Operator           ThresholdOperator
	Window             time.Duration
	RecentSamples      uint64
	BaselineSamples    uint64
	NumeratorSamples   uint64
	DenominatorSamples uint64
}

func LoadAnalyticalRules(path string) ([]AnalyticalRule, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat analytical rules file: %w", err)
	}
	if info.IsDir() || info.Size() > maxRulesFileBytes {
		return nil, fmt.Errorf("invalid analytical rules file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read analytical rules file: %w", err)
	}
	var document analyticalRulesFile
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode analytical rules file: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("analytical rules file must contain one JSON object")
		}
		return nil, fmt.Errorf("decode analytical rules file trailing JSON: %w", err)
	}
	if len(document.Rules) > maxAnalyticalRules {
		return nil, fmt.Errorf("analytical rules cannot contain more than %d items", maxAnalyticalRules)
	}
	result := make([]AnalyticalRule, 0, len(document.Rules))
	seen := make(map[string]struct{}, len(document.Rules))
	for index, rule := range document.Rules {
		normalized, err := rule.Validate()
		if err != nil {
			return nil, fmt.Errorf("rules[%d]: %w", index, err)
		}
		if _, exists := seen[normalized.ID]; exists {
			return nil, fmt.Errorf("rules[%d]: duplicate rule_id %q", index, normalized.ID)
		}
		seen[normalized.ID] = struct{}{}
		result = append(result, normalized)
	}
	return result, nil
}

func (r AnalyticalRule) Validate() (AnalyticalRule, error) {
	r.ID = strings.TrimSpace(r.ID)
	r.Source = strings.TrimSpace(r.Source)
	r.Kind = strings.TrimSpace(r.Kind)
	r.Name = strings.TrimSpace(r.Name)
	r.Numerator = strings.TrimSpace(r.Numerator)
	r.Denominator = strings.TrimSpace(r.Denominator)
	r.Aggregate = strings.TrimSpace(r.Aggregate)
	if r.ID == "" || !validRuleID(r.ID) || len(r.ID) > maxRuleIdentifierBytes {
		return AnalyticalRule{}, fmt.Errorf("rule_id is invalid")
	}
	for field, value := range map[string]string{"source": r.Source, "kind": r.Kind} {
		if value == "" || len(value) > maxRuleIdentifierBytes {
			return AnalyticalRule{}, fmt.Errorf("%s is required and must be short", field)
		}
	}
	if r.Name != "" && len(r.Name) > maxRuleIdentifierBytes {
		return AnalyticalRule{}, fmt.Errorf("name is too long")
	}
	if r.WindowSeconds < 1 || r.WindowSeconds > maxRuleWindowSeconds {
		return AnalyticalRule{}, fmt.Errorf("window_seconds must be between 1 and %d", maxRuleWindowSeconds)
	}
	if r.MinSamples == 0 {
		r.MinSamples = 1
	}
	if r.MinSamples > maxRuleSamples {
		return AnalyticalRule{}, fmt.Errorf("min_samples is too large")
	}
	if r.BaselineWindowSeconds == 0 {
		r.BaselineWindowSeconds = r.WindowSeconds
	}
	if r.BaselineWindowSeconds < 1 || r.BaselineWindowSeconds > maxRuleWindowSeconds {
		return AnalyticalRule{}, fmt.Errorf("baseline_window_seconds is invalid")
	}
	groupBy, err := normalizeGroupBy(r.GroupBy)
	if err != nil {
		return AnalyticalRule{}, err
	}
	r.GroupBy = groupBy
	switch r.Family {
	case RuleFamilyAbsence:
		if r.Name == "" {
			return AnalyticalRule{}, fmt.Errorf("name is required for absence")
		}
	case RuleFamilyRatio:
		if r.Numerator == "" || r.Denominator == "" || r.Numerator == r.Denominator {
			return AnalyticalRule{}, fmt.Errorf("ratio requires distinct numerator and denominator")
		}
		if len(r.Numerator) > maxRuleIdentifierBytes || len(r.Denominator) > maxRuleIdentifierBytes {
			return AnalyticalRule{}, fmt.Errorf("ratio signal names are too long")
		}
		if !validThresholdOperator(r.Operator) {
			return AnalyticalRule{}, fmt.Errorf("operator must be one of gt, gte, lt, lte")
		}
	case RuleFamilyWindowChange:
		if r.Name == "" {
			return AnalyticalRule{}, fmt.Errorf("name is required for window_change")
		}
		if r.Aggregate == "" {
			r.Aggregate = RuleAggregateCount
		}
		if r.Aggregate != RuleAggregateCount && r.Aggregate != RuleAggregateAverage {
			return AnalyticalRule{}, fmt.Errorf("aggregate must be count or avg")
		}
		if !validThresholdOperator(r.Operator) {
			return AnalyticalRule{}, fmt.Errorf("operator must be one of gt, gte, lt, lte")
		}
	default:
		return AnalyticalRule{}, fmt.Errorf("family must be absence, ratio or window_change")
	}
	if math.IsNaN(r.Threshold) || math.IsInf(r.Threshold, 0) {
		return AnalyticalRule{}, fmt.Errorf("threshold must be finite")
	}
	return r, nil
}

func (s *Store) AnalyticalAlerts(ctx context.Context, rule AnalyticalRule) ([]AnalyticalAlert, error) {
	rule, err := rule.Validate()
	if err != nil {
		return nil, err
	}
	query, err := analyticalRuleQuery(rule)
	if err != nil {
		return nil, err
	}
	ctx, span := tracer.Start(ctx, "ClickHouse SELECT analytical_rule_lookback",
		withAttributes(
			spanString("db.system", "clickhouse"),
			spanString("rule.id", rule.ID),
			spanString("rule.family", string(rule.Family)),
			spanInt64("rule.window_seconds", int64(rule.WindowSeconds)),
			spanInt64("rule.baseline_window_seconds", int64(rule.BaselineWindowSeconds)),
			spanInt64("rule.min_samples", int64(rule.MinSamples)),
		))
	defer span.End()
	now := time.Now().UTC()
	recentSince := now.Add(-time.Duration(rule.WindowSeconds) * time.Second)
	baselineSince := recentSince.Add(-time.Duration(rule.BaselineWindowSeconds) * time.Second)
	args := []any{
		clickhouse.Named("baseline_since", baselineSince),
		clickhouse.Named("recent_since", recentSince),
		clickhouse.Named("until", now),
		clickhouse.Named("source", rule.Source),
		clickhouse.Named("kind", rule.Kind),
		clickhouse.Named("name", rule.Name),
		clickhouse.Named("organization_id", s.organizationID),
		clickhouse.Named("canary_id", s.canaryID),
		clickhouse.Named("numerator", rule.Numerator),
		clickhouse.Named("denominator", rule.Denominator),
		clickhouse.Named("min_samples", rule.MinSamples),
		clickhouse.Named("threshold", rule.Threshold),
	}
	rows, err := s.conn.Query(ctx, query, args...)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return nil, err
	}
	defer rows.Close()
	alerts := make([]AnalyticalAlert, 0)
	for rows.Next() {
		var tenantID, entityID string
		var observed, baseline, recentSamples, baselineSamples float64
		if err := rows.Scan(&tenantID, &entityID, &observed, &baseline, &recentSamples, &baselineSamples); err != nil {
			span.RecordError(err)
			span.SetStatus(spanError, err.Error())
			return nil, err
		}
		alert := AnalyticalAlert{
			RuleID: rule.ID, Family: rule.Family, Source: rule.Source, TenantID: tenantID, EntityID: entityID,
			ObservedValue: observed, BaselineValue: baseline, Threshold: rule.Threshold, Operator: rule.Operator,
			Window:        time.Duration(rule.WindowSeconds) * time.Second,
			RecentSamples: uint64(maxFloat(recentSamples, 0)), BaselineSamples: uint64(maxFloat(baselineSamples, 0)),
		}
		if rule.Family == RuleFamilyRatio {
			alert.NumeratorSamples = uint64(maxFloat(recentSamples, 0))
			alert.DenominatorSamples = uint64(maxFloat(baselineSamples, 0))
		}
		alerts = append(alerts, alert)
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return nil, err
	}
	span.SetAttributes(spanInt("rule.alert_count", len(alerts)))
	return alerts, nil
}

func maxFloat(value, minimum float64) float64 {
	if value < minimum {
		return minimum
	}
	return value
}

func analyticalRuleQuery(rule AnalyticalRule) (string, error) {
	rule, err := rule.Validate()
	if err != nil {
		return "", err
	}
	selectEntity, groupBy := analyticalGroupSQL(rule)
	base := `
FROM canary_signal_events FINAL
WHERE source = {source:String}
  AND kind = {kind:String}
  AND ({organization_id:String} = '' OR organization_id = {organization_id:String})
  AND ({canary_id:String} = '' OR canary_id = {canary_id:String})
`
	settings := `
LIMIT 10000
SETTINGS max_execution_time = 30,
         max_rows_to_read = 1000000000,
         max_result_rows = 10000,
         result_overflow_mode = 'break'`
	switch rule.Family {
	case RuleFamilyAbsence:
		return fmt.Sprintf(`
SELECT tenant_id, %s,
       toFloat64(recent_samples) AS observed_value,
       toFloat64(baseline_samples) AS baseline_value,
       toFloat64(recent_samples) AS recent_samples,
       toFloat64(baseline_samples) AS baseline_samples
		FROM (
		    SELECT tenant_id, %s,
		           countIf(occurred_at >= {recent_since:DateTime64(3)}) AS recent_samples,
		           count() AS baseline_samples
		    %s
		      AND name = {name:String}
		      AND occurred_at >= {baseline_since:DateTime64(3)}
		      AND occurred_at < {until:DateTime64(3)}
		    GROUP BY %s
		)
WHERE baseline_samples >= {min_samples:UInt64}
  AND recent_samples < {min_samples:UInt64}%s`,
			selectEntity, selectEntity, base, groupBy, settings), nil
	case RuleFamilyRatio:
		return fmt.Sprintf(`
SELECT tenant_id, %s,
       toFloat64(numerator_samples) / nullIf(denominator_samples, 0) AS observed_value,
       toFloat64(denominator_samples) AS baseline_value,
       toFloat64(numerator_samples) AS recent_samples,
       toFloat64(denominator_samples) AS baseline_samples
		FROM (
		    SELECT tenant_id, %s,
		           countIf(name = {numerator:String}) AS numerator_samples,
		           countIf(name = {denominator:String}) AS denominator_samples
		    %s
		      AND name IN ({numerator:String}, {denominator:String})
		      AND occurred_at >= {recent_since:DateTime64(3)}
		      AND occurred_at < {until:DateTime64(3)}
		    GROUP BY %s
		)
WHERE denominator_samples >= {min_samples:UInt64}
	AND isFinite(toFloat64(numerator_samples) / nullIf(denominator_samples, 0))
	AND (toFloat64(numerator_samples) / nullIf(denominator_samples, 0)) %s {threshold:Float64}%s`,
			selectEntity, selectEntity, base, groupBy, thresholdSQLOperator(rule.Operator), settings), nil
	case RuleFamilyWindowChange:
		aggregateRecent, aggregateBaseline := "countIf(occurred_at >= {recent_since:DateTime64(3)})", "countIf(occurred_at < {recent_since:DateTime64(3)})"
		if rule.Aggregate == RuleAggregateAverage {
			aggregateRecent = "avgIf(value, occurred_at >= {recent_since:DateTime64(3)})"
			aggregateBaseline = "avgIf(value, occurred_at < {recent_since:DateTime64(3)})"
		}
		return fmt.Sprintf(`
SELECT tenant_id, %s,
       toFloat64(recent_value) / nullIf(previous_value, 0) AS observed_value,
       toFloat64(previous_value) AS baseline_value,
       toFloat64(recent_samples) AS recent_samples,
       toFloat64(previous_samples) AS baseline_samples
FROM (
    SELECT tenant_id, %s,
           %s AS recent_value,
           %s AS previous_value,
           countIf(occurred_at >= {recent_since:DateTime64(3)}) AS recent_samples,
           countIf(occurred_at < {recent_since:DateTime64(3)}) AS previous_samples
    %s
      AND name = {name:String}
      AND occurred_at >= {baseline_since:DateTime64(3)}
      AND occurred_at < {until:DateTime64(3)}
    GROUP BY %s
)
WHERE previous_samples >= {min_samples:UInt64}
	AND isFinite(toFloat64(recent_value) / nullIf(previous_value, 0))
	AND (toFloat64(recent_value) / nullIf(previous_value, 0)) %s {threshold:Float64}%s`,
			selectEntity, selectEntity, aggregateRecent, aggregateBaseline, base, groupBy, thresholdSQLOperator(rule.Operator), settings), nil
	default:
		return "", fmt.Errorf("unsupported analytical rule family")
	}
}

func analyticalGroupSQL(rule AnalyticalRule) (string, string) {
	if len(rule.GroupBy) == 1 {
		return "'' AS entity_id", "tenant_id"
	}
	return "entity_id", "tenant_id, entity_id"
}

func mapAnalyticalResult(alert AnalyticalAlert) IncidentCandidate {
	return IncidentCandidate{
		Key:                analyticalIncidentKey(alert.RuleID, alert.TenantID, alert.EntityID),
		Scope:              "analytical/" + alert.RuleID + "/",
		RuleID:             alert.RuleID,
		Family:             string(alert.Family),
		Source:             alert.Source,
		TenantID:           alert.TenantID,
		EntityID:           alert.EntityID,
		ObservedValue:      alert.ObservedValue,
		BaselineValue:      alert.BaselineValue,
		Threshold:          alert.Threshold,
		Operator:           alert.Operator,
		Window:             alert.Window,
		RecentSamples:      alert.RecentSamples,
		BaselineSamples:    alert.BaselineSamples,
		NumeratorSamples:   alert.NumeratorSamples,
		DenominatorSamples: alert.DenominatorSamples,
		Fingerprint:        fmt.Sprintf("%.3f/%.3f", alert.ObservedValue, alert.BaselineValue),
	}
}

func analyticalIncidentKey(ruleID, tenantID, entityID string) string {
	return "analytical/" + ruleID + "/" + tenantID + "/" + entityID
}
