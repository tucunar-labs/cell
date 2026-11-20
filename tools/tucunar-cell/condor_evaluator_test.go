package canarynode

import (
	"testing"
	"time"
)

func TestConversionAlertsFromSignalsGroupsShopifyTenants(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	signals := make([]Signal, 0, 4)
	for i := 0; i < 3; i++ {
		signals = append(signals, Signal{Source: "shopify", Kind: "event", TenantID: "shop-a", Name: "checkout_started", OccurredAt: now.Add(-time.Duration(i+1) * time.Minute)})
	}
	signals = append(signals, Signal{Source: "shopify", Kind: "event", TenantID: "shop-a", Name: "payment_success", OccurredAt: now.Add(-time.Minute)})
	alerts := conversionAlertsFromSignals(signals, 15*time.Minute, 0.5, now)
	if len(alerts) != 1 || alerts[0].ShopID != "shop-a" || alerts[0].CheckoutStarted != 3 || alerts[0].PaymentSuccess != 1 {
		t.Fatalf("conversion alerts = %#v", alerts)
	}
}

func TestThresholdAlertsFromSignalsHonorsGroupingAndAggregate(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	signals := []Signal{
		{Source: "crane", Kind: "measurement", Name: "load", TenantID: "mine", EntityID: "crane-1", Value: 12, OccurredAt: now.Add(-time.Minute)},
		{Source: "crane", Kind: "measurement", Name: "load", TenantID: "mine", EntityID: "crane-1", Value: 10, OccurredAt: now.Add(-2 * time.Minute)},
		{Source: "crane", Kind: "measurement", Name: "load", TenantID: "mine", EntityID: "crane-1", Value: 11, OccurredAt: now.Add(-3 * time.Minute)},
	}
	rule := ThresholdRule{ID: "load.high", Source: "crane", Kind: "measurement", Name: "load", Aggregate: AggregateAverage, Operator: OperatorGTE, Threshold: 11, WindowSeconds: 300}
	alerts, err := thresholdAlertsFromSignals(signals, rule, now)
	if err != nil || len(alerts) != 1 || alerts[0].EntityID != "crane-1" || alerts[0].ObservedValue != 11 {
		t.Fatalf("threshold alerts = %#v, err=%v", alerts, err)
	}
}

func TestAnalyticalRatioAlertsFromSignalsUsesRecentWindow(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	signals := []Signal{
		{Source: "checkout", Kind: "event", Name: "checkout_started", TenantID: "shop", EntityID: "store", OccurredAt: now.Add(-time.Minute)},
		{Source: "checkout", Kind: "event", Name: "checkout_started", TenantID: "shop", EntityID: "store", OccurredAt: now.Add(-2 * time.Minute)},
		{Source: "checkout", Kind: "event", Name: "checkout_started", TenantID: "shop", EntityID: "store", OccurredAt: now.Add(-3 * time.Minute)},
		{Source: "checkout", Kind: "event", Name: "payment_success", TenantID: "shop", EntityID: "store", OccurredAt: now.Add(-time.Minute)},
	}
	rule := AnalyticalRule{ID: "conversion.low", Family: RuleFamilyRatio, Source: "checkout", Kind: "event", Numerator: "payment_success", Denominator: "checkout_started", Operator: OperatorLT, Threshold: 0.5, WindowSeconds: 15 * 60, MinSamples: 3, GroupBy: []string{"tenant_id", "entity_id"}}
	alerts, err := analyticalAlertsFromSignals(signals, rule, now)
	if err != nil || len(alerts) != 1 || alerts[0].ObservedValue != 1.0/3.0 || alerts[0].NumeratorSamples != 1 || alerts[0].DenominatorSamples != 3 {
		t.Fatalf("analytical alerts = %#v, err=%v", alerts, err)
	}
}
