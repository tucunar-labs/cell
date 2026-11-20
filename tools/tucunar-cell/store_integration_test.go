package canarynode

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCondorWindowWithClickHouse(t *testing.T) {
	if os.Getenv("CANARY_CH_TEST") != "1" {
		t.Skip("set CANARY_CH_TEST=1 to run ClickHouse integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := newStore(ctx, env("CLICKHOUSE_ADDR", "localhost:19000"), env("CLICKHOUSE_USER", "default"), env("CLICKHOUSE_PASSWORD", "clickhouse"), env("CLICKHOUSE_DB", "default"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	signals, _, _, err := store.ReadSignalsSince(ctx, "", 1000, now.Add(-15*time.Minute), now)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("read %d signals through the bounded Condor window", len(signals))
}

func TestAnalyticalAlertsWithClickHouse(t *testing.T) {
	if os.Getenv("CANARY_CH_TEST") != "1" {
		t.Skip("set CANARY_CH_TEST=1 to run ClickHouse integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := newStore(ctx, env("CLICKHOUSE_ADDR", "localhost:19000"), "default", "clickhouse", "default")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rules, err := LoadAnalyticalRules("analytical_rules.example.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range rules {
		if _, err := store.AnalyticalAlerts(ctx, rule); err != nil {
			t.Fatalf("AnalyticalAlerts(%s): %v", rule.ID, err)
		}
	}
	transitions, err := store.ReconcileIncidents(ctx, nil, []string{"conversion/", "analytical/checkout.conversion.low/", "analytical/crane.load.absent/", "analytical/crane.load.drop/"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("lifecycle transitions: %#v", transitions)
}

func TestClickHouseIndustrialRuleSemantics(t *testing.T) {
	if os.Getenv("CANARY_CH_TEST") != "1" {
		t.Skip("set CANARY_CH_TEST=1 to run ClickHouse integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	store, err := newStore(ctx, env("CLICKHOUSE_ADDR", "localhost:19000"), env("CLICKHOUSE_USER", "default"), env("CLICKHOUSE_PASSWORD", "clickhouse"), env("CLICKHOUSE_DB", "default"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InitCloudSchema(ctx); err != nil {
		t.Fatal(err)
	}

	fixture := strings.ToLower(strings.ReplaceAll(newID(), "-", ""))
	miningSource := "integration-mining-" + fixture
	lowTenant := "low-" + fixture
	healthyTenant := "healthy-" + fixture
	lowVolumeTenant := "low-volume-" + fixture
	now := time.Now().UTC().Truncate(time.Millisecond)
	signals := make([]Signal, 0, 20)
	add := func(tenant, entity, source, kind, name string, value float64, occurredAt time.Time) {
		signals = append(signals, Signal{
			ExternalID: fmt.Sprintf("%s-%d", fixture, len(signals)), Source: source, TenantID: tenant,
			EntityID: entity, Kind: kind, Name: name, Value: value, OccurredAt: occurredAt,
		})
	}

	// Three load readings in the current window exercise max, sum, min and p95.
	for index, value := range []float64{4, 6, 12} {
		add("mine-"+fixture, "crusher-01", miningSource, "measurement", "load", value, now.Add(-time.Duration(index+1)*10*time.Second))
	}
	// Three baseline readings and no recent readings exercise absence.
	for index := 0; index < 3; index++ {
		add("mine-"+fixture, "crusher-02", miningSource, "measurement", "temperature", 70+float64(index), now.Add(-2*time.Minute-time.Duration(index)*10*time.Second))
	}

	// Low conversion, healthy conversion and below-minimum traffic tenants.
	for index := 0; index < 3; index++ {
		add(lowTenant, lowTenant, "shopify", "event", "checkout_started", 0, now.Add(-time.Duration(index+1)*10*time.Second))
		add(healthyTenant, healthyTenant, "shopify", "event", "checkout_started", 0, now.Add(-time.Duration(index+1)*10*time.Second))
	}
	for index := 0; index < 2; index++ {
		add(healthyTenant, healthyTenant, "shopify", "event", "payment_success", 0, now.Add(-time.Duration(index+1)*10*time.Second))
	}
	add(lowVolumeTenant, lowVolumeTenant, "shopify", "event", "checkout_started", 0, now.Add(-10*time.Second))

	if err := store.InsertSignals(ctx, signals); err != nil {
		t.Fatal(err)
	}

	conversion, err := store.ConversionAlerts(ctx, 5*time.Minute, conversionThreshold)
	if err != nil {
		t.Fatal(err)
	}
	if !containsConversionAlert(conversion, lowTenant, 0, 3) {
		t.Fatalf("conversion alerts = %#v, expected low tenant with 0/3", conversion)
	}
	if containsConversionAlert(conversion, healthyTenant, 0, 0) || containsConversionAlert(conversion, lowVolumeTenant, 0, 0) {
		t.Fatalf("conversion alerts included healthy or low-volume tenant: %#v", conversion)
	}

	assertThreshold := func(name, aggregate string, operator ThresholdOperator, threshold float64, want bool) {
		rule := ThresholdRule{ID: "integration." + name + "." + fixture, Source: miningSource, Kind: "measurement", Name: "load", Aggregate: aggregate, Operator: operator, Threshold: threshold, WindowSeconds: 300, GroupBy: []string{"tenant_id", "entity_id"}}
		alerts, queryErr := store.ThresholdAlerts(ctx, rule)
		if queryErr != nil {
			t.Fatalf("ThresholdAlerts(%s): %v", aggregate, queryErr)
		}
		got := containsThresholdAlert(alerts, "mine-"+fixture, "crusher-01")
		if got != want {
			t.Fatalf("ThresholdAlerts(%s) contains fixture = %v, alerts = %#v", aggregate, got, alerts)
		}
	}
	assertThreshold("max", AggregateMax, OperatorGTE, 12, true)
	assertThreshold("sum", AggregateSum, OperatorGT, 21, true)
	assertThreshold("min", AggregateMin, OperatorLT, 5, true)
	assertThreshold("p95", AggregateP95, OperatorGT, 11, true)

	absenceRule := AnalyticalRule{
		ID: "integration.absence." + fixture, Family: RuleFamilyAbsence, Source: miningSource,
		Kind: "measurement", Name: "temperature", WindowSeconds: 60,
		BaselineWindowSeconds: 300, MinSamples: 3, GroupBy: []string{"tenant_id", "entity_id"},
	}
	absent, err := store.AnalyticalAlerts(ctx, absenceRule)
	if err != nil {
		t.Fatal(err)
	}
	if !containsAnalyticalAlert(absent, "mine-"+fixture, "crusher-02") {
		t.Fatalf("absence alerts = %#v, expected crusher-02", absent)
	}

	incidentScope := "integration/" + fixture + "/"
	incidentKey := incidentScope + "recovery"
	candidate := IncidentCandidate{
		Key: incidentKey, Scope: incidentScope, RuleID: "integration.recovery." + fixture,
		Family: "threshold", Source: miningSource, TenantID: "mine-" + fixture,
		EntityID: "crusher-01", ObservedValue: 12, Threshold: 10, Operator: OperatorGT,
		Window: 5 * time.Minute, Fingerprint: "max=12",
	}
	first, err := store.ReconcileIncidents(ctx, []IncidentCandidate{candidate}, []string{incidentScope})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Status != incidentStatusOpen {
		t.Fatalf("open transitions = %#v", first)
	}
	recovered, err := store.ReconcileIncidents(ctx, nil, []string{incidentScope})
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].Status != incidentStatusRecovered {
		t.Fatalf("recovery transitions = %#v", recovered)
	}

	nodeID := newID()
	outbox := cloudOutboxRecord{
		OutboxID: newID(), NodeID: nodeID, Operation: "integration", Path: "/integration",
		IdempotencyKey: "integration:" + fixture, Payload: `{"fixture":"one"}`,
		NextAttemptAt: now.Add(-time.Second),
	}
	if err := store.EnqueueCloudMessage(ctx, outbox, 10); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueueCloudMessage(ctx, outbox, 10); err != nil {
		t.Fatal("same idempotency key should be a no-op: ", err)
	}
	pending, err := store.PendingCloudMessages(ctx, nodeID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].IdempotencyKey != outbox.IdempotencyKey {
		t.Fatalf("pending outbox = %#v", pending)
	}
}

func containsConversionAlert(alerts []ConversionAlert, tenant string, paymentSuccess, checkoutStarted uint64) bool {
	for _, alert := range alerts {
		if alert.ShopID == tenant && alert.PaymentSuccess == paymentSuccess && alert.CheckoutStarted == checkoutStarted {
			return true
		}
	}
	return false
}

func containsThresholdAlert(alerts []ThresholdAlert, tenant, entity string) bool {
	for _, alert := range alerts {
		if alert.TenantID == tenant && alert.EntityID == entity {
			return true
		}
	}
	return false
}

func containsAnalyticalAlert(alerts []AnalyticalAlert, tenant, entity string) bool {
	for _, alert := range alerts {
		if alert.TenantID == tenant && alert.EntityID == entity {
			return true
		}
	}
	return false
}

func TestCloudOutboxResumesInOrderWithoutDuplicates(t *testing.T) {
	if os.Getenv("CANARY_CH_TEST") != "1" {
		t.Skip("set CANARY_CH_TEST=1 to run ClickHouse integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	store, err := newStore(ctx, env("CLICKHOUSE_ADDR", "localhost:19000"), env("CLICKHOUSE_USER", "default"), env("CLICKHOUSE_PASSWORD", "clickhouse"), env("CLICKHOUSE_DB", "default"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InitCloudSchema(ctx); err != nil {
		t.Fatal(err)
	}

	cloudDown := true
	acceptedKeys := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cloudDown {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		acceptedKeys = append(acceptedKeys, r.Header.Get("Idempotency-Key"))
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	nodeID := newID()
	reporter := &cloudReporter{
		store:  store,
		config: cloudConfig{BaseURL: server.URL, MaxPendingOutbox: 10},
		client: &cloudClient{baseURL: server.URL, http: server.Client(), sleep: func(time.Duration) {}, now: time.Now},
		state:  cloudState{NodeID: nodeID, OrganizationID: newID(), CanaryID: newID(), AccessToken: "test-token"},
	}

	now := time.Now().UTC()
	first := cloudOutboxRecord{
		OutboxID: newID(), NodeID: nodeID, Operation: "integration", Path: "/first",
		IdempotencyKey: "integration:first:" + nodeID, Payload: `{"sequence":1}`, NextAttemptAt: now.Add(-2 * time.Second),
	}
	second := cloudOutboxRecord{
		OutboxID: newID(), NodeID: nodeID, Operation: "integration", Path: "/second",
		IdempotencyKey: "integration:second:" + nodeID, Payload: `{"sequence":2}`, NextAttemptAt: now.Add(-time.Second),
	}
	if err := store.EnqueueCloudMessage(ctx, first, 10); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueueCloudMessage(ctx, second, 10); err != nil {
		t.Fatal(err)
	}

	// The first failed message is retried and later records are not overtaken.
	reporter.flushPending(ctx)
	cloudDown = false
	if err := store.RetryCloudMessage(ctx, first, time.Now().UTC().Add(-2*time.Second), "simulated outage"); err != nil {
		t.Fatal(err)
	}
	reporter.flushPending(ctx)
	if len(acceptedKeys) != 2 || acceptedKeys[0] != first.IdempotencyKey || acceptedKeys[1] != second.IdempotencyKey {
		t.Fatalf("accepted keys = %#v, want ordered first/second", acceptedKeys)
	}

	pending, err := store.PendingCloudMessages(ctx, nodeID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after recovery = %#v", pending)
	}
	reporter.flushPending(ctx)
	if len(acceptedKeys) != 2 {
		t.Fatalf("second flush duplicated reports: %#v", acceptedKeys)
	}
}
