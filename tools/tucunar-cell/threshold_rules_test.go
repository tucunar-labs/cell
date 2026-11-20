package canarynode

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadThresholdRulesValidatesAndNormalizes(t *testing.T) {
	path := t.TempDir() + "/rules.json"
	data := `{
  "rules": [{
    "rule_id": "crane.load.high",
    "source": " crane-monitor ",
    "kind": "measurement",
    "name": "load",
    "operator": "gte",
    "threshold": 10.5,
    "window_seconds": 300,
    "group_by": ["entity_id", "tenant_id"]
  }]
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	rules, err := LoadThresholdRules(path)
	if err != nil {
		t.Fatalf("LoadThresholdRules() error = %v", err)
	}
	want := []ThresholdRule{{
		ID:            "crane.load.high",
		Source:        "crane-monitor",
		Kind:          "measurement",
		Name:          "load",
		Aggregate:     AggregateAverage,
		Operator:      OperatorGTE,
		Threshold:     10.5,
		WindowSeconds: 300,
		GroupBy:       []string{"tenant_id", "entity_id"},
	}}
	if !reflect.DeepEqual(rules, want) {
		t.Fatalf("LoadThresholdRules() = %#v, want %#v", rules, want)
	}
}

func TestLoadThresholdRulesEmptyPathDisablesRules(t *testing.T) {
	rules, err := LoadThresholdRules("")
	if err != nil {
		t.Fatalf("LoadThresholdRules() error = %v", err)
	}
	if rules != nil {
		t.Fatalf("LoadThresholdRules() = %#v, want nil", rules)
	}
}

func TestLoadThresholdRulesRejectsUnsafeOrInvalidConfig(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		wantErr string
	}{
		{
			name:    "unknown field",
			data:    `{"rules":[{"rule_id":"r","source":"s","kind":"measurement","name":"load","operator":"gt","threshold":1,"window_seconds":10,"unexpected":true}]}`,
			wantErr: "decode rules file",
		},
		{
			name:    "invalid operator",
			data:    `{"rules":[{"rule_id":"r","source":"s","kind":"measurement","name":"load","operator":"DROP TABLE","threshold":1,"window_seconds":10}]}`,
			wantErr: "operator must be one of",
		},
		{
			name:    "invalid aggregate",
			data:    `{"rules":[{"rule_id":"r","source":"s","kind":"measurement","name":"load","aggregate":"median","operator":"gt","threshold":1,"window_seconds":10}]}`,
			wantErr: "aggregate must be one of",
		},
		{
			name:    "entity-only grouping",
			data:    `{"rules":[{"rule_id":"r","source":"s","kind":"measurement","name":"load","operator":"gt","threshold":1,"window_seconds":10,"group_by":["entity_id"]}]}`,
			wantErr: "group_by must include tenant_id",
		},
		{
			name:    "window too large",
			data:    `{"rules":[{"rule_id":"r","source":"s","kind":"measurement","name":"load","operator":"gt","threshold":1,"window_seconds":999999999}]}`,
			wantErr: "window_seconds must be between",
		},
		{
			name:    "unsafe rule id",
			data:    `{"rules":[{"rule_id":"r;DROP TABLE","source":"s","kind":"measurement","name":"load","operator":"gt","threshold":1,"window_seconds":10}]}`,
			wantErr: "rule_id contains unsupported characters",
		},
		{
			name:    "duplicate ids",
			data:    `{"rules":[{"rule_id":"r","source":"s","kind":"measurement","name":"a","operator":"gt","threshold":1,"window_seconds":10},{"rule_id":"r","source":"s","kind":"measurement","name":"b","operator":"gt","threshold":1,"window_seconds":10}]}`,
			wantErr: "duplicate rule_id",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := t.TempDir() + "/rules.json"
			if err := os.WriteFile(path, []byte(test.data), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadThresholdRules(path)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("LoadThresholdRules() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestThresholdResultMapping(t *testing.T) {
	rule := ThresholdRule{
		ID:            "crane.load.high",
		Source:        "crane-monitor",
		Kind:          "measurement",
		Name:          "load",
		Operator:      OperatorGTE,
		Threshold:     10,
		WindowSeconds: 300,
		GroupBy:       []string{"tenant_id", "entity_id"},
	}
	alert := mapThresholdResult(rule, "acme", "crane-17", 12.25)
	want := ThresholdAlert{
		RuleID:        "crane.load.high",
		Source:        "crane-monitor",
		TenantID:      "acme",
		EntityID:      "crane-17",
		Aggregate:     AggregateAverage,
		ObservedValue: 12.25,
		Threshold:     10,
		Operator:      OperatorGTE,
		Window:        5 * time.Minute,
	}
	if !reflect.DeepEqual(alert, want) {
		t.Fatalf("mapThresholdResult() = %#v, want %#v", alert, want)
	}
}

func TestThresholdQueryWhitelistsAggregates(t *testing.T) {
	for _, test := range []struct {
		aggregate string
		fragment  string
	}{
		{AggregateAverage, "avg(value)"},
		{AggregateSum, "sum(value)"},
		{AggregateMin, "min(value)"},
		{AggregateMax, "max(value)"},
		{AggregateP50, "toFloat64(quantileTDigest(0.50)(value))"},
		{AggregateP90, "toFloat64(quantileTDigest(0.90)(value))"},
		{AggregateP95, "toFloat64(quantileTDigest(0.95)(value))"},
		{AggregateP99, "toFloat64(quantileTDigest(0.99)(value))"},
	} {
		t.Run(test.aggregate, func(t *testing.T) {
			rule := ThresholdRule{ID: "rule", Source: "source", Kind: "measurement", Name: "load", Aggregate: test.aggregate, Operator: OperatorGT, Threshold: 1, WindowSeconds: 60}
			query, err := thresholdQuery(rule)
			if err != nil {
				t.Fatalf("thresholdQuery() error = %v", err)
			}
			if !strings.Contains(query, test.fragment) {
				t.Fatalf("thresholdQuery() missing aggregate %q: %s", test.fragment, query)
			}
		})
	}

	if _, err := thresholdAggregateSQL("median"); err == nil {
		t.Fatal("thresholdAggregateSQL() accepted an unsupported aggregate")
	}
}

func TestThresholdQueryBindsValuesAndUsesWhitelistedSQL(t *testing.T) {
	rule := ThresholdRule{
		ID:            "safe-rule",
		Source:        "internal' source",
		Kind:          "measurement",
		Name:          "load",
		Operator:      OperatorLTE,
		Threshold:     2,
		WindowSeconds: 60,
	}
	query, err := thresholdQuery(rule)
	if err != nil {
		t.Fatalf("thresholdQuery() error = %v", err)
	}
	for _, value := range []string{"internal' source", "load", "safe-rule"} {
		if strings.Contains(query, value) {
			t.Fatalf("thresholdQuery() embedded user value %q: %s", value, query)
		}
	}
	for _, fragment := range []string{"{source:String}", "{kind:String}", "{name:String}", "{since:DateTime64(3)}", "{threshold:Float64}", "HAVING observed_value <="} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("thresholdQuery() missing %q: %s", fragment, query)
		}
	}
}
