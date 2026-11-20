package canarynode

import (
	"os"
	"strings"
	"testing"
)

func TestLoadAnalyticalRulesValidatesFamiliesAndDefaults(t *testing.T) {
	path := t.TempDir() + "/rules.json"
	data := `{"rules":[
{"rule_id":"payments.ratio","family":"ratio","source":"checkout","kind":"event","numerator":"payment_success","denominator":"checkout_started","operator":"lt","threshold":0.2,"window_seconds":900,"min_samples":20,"group_by":["tenant_id"]},
{"rule_id":"crane.absent","family":"absence","source":"crane","kind":"measurement","name":"load","window_seconds":60,"baseline_window_seconds":300,"min_samples":2},
{"rule_id":"crane.change","family":"window_change","source":"crane","kind":"measurement","name":"load","operator":"lt","threshold":0.5,"window_seconds":60,"aggregate":"avg","group_by":["tenant_id","entity_id"]}
]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	rules, err := LoadAnalyticalRules(path)
	if err != nil {
		t.Fatalf("LoadAnalyticalRules() error = %v", err)
	}
	if len(rules) != 3 || rules[1].MinSamples != 2 || rules[2].BaselineWindowSeconds != 60 {
		t.Fatalf("unexpected normalized rules: %#v", rules)
	}
}

func TestAnalyticalRuleQueryUsesParametersAndBoundedSettings(t *testing.T) {
	rule := AnalyticalRule{
		ID: "ratio.safe", Family: RuleFamilyRatio, Source: "checkout' OR 1=1", Kind: "event", Numerator: "paid", Denominator: "started",
		Operator: OperatorLT, Threshold: 0.2, WindowSeconds: 900, MinSamples: 10,
	}
	query, err := analyticalRuleQuery(rule)
	if err != nil {
		t.Fatalf("analyticalRuleQuery() error = %v", err)
	}
	for _, value := range []string{"checkout' OR 1=1", "paid", "started", "ratio.safe"} {
		if strings.Contains(query, value) {
			t.Fatalf("query embedded rule value %q: %s", value, query)
		}
	}
	for _, fragment := range []string{"{source:String}", "{numerator:String}", "{denominator:String}", "{min_samples:UInt64}", "LIMIT 10000", "max_execution_time = 30"} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("query missing %q: %s", fragment, query)
		}
	}
}

func TestAnalyticalRuleValidationRejectsUnsafeOrIncompleteRules(t *testing.T) {
	tests := []AnalyticalRule{
		{ID: "bad;drop", Family: RuleFamilyAbsence, Source: "s", Kind: "k", Name: "n", WindowSeconds: 1},
		{ID: "ratio", Family: RuleFamilyRatio, Source: "s", Kind: "k", Numerator: "same", Denominator: "same", WindowSeconds: 1, Operator: OperatorLT},
		{ID: "change", Family: RuleFamilyWindowChange, Source: "s", Kind: "k", Name: "n", WindowSeconds: 1, Operator: OperatorLT, Aggregate: "sum"},
	}
	for _, rule := range tests {
		if _, err := rule.Validate(); err == nil {
			t.Fatalf("Validate() accepted invalid rule %#v", rule)
		}
	}
}
