package canarynode

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestConsoleEventsPreserveMillisecondBounds(t *testing.T) {
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
	now := time.Now().UTC().Truncate(time.Millisecond)
	scenario := "console-bounds-" + newID()
	signal, err := (Signal{
		ExternalID:     scenario,
		OrganizationID: store.organizationID,
		CanaryID:       store.canaryID,
		Source:         "console-test",
		TenantID:       scenario,
		EntityID:       "tank",
		Kind:           "measurement",
		Name:           "level",
		Value:          42,
		Unit:           "%",
		Dimensions:     map[string]string{"scenario_id": scenario},
		OccurredAt:     now,
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	dimensions, err := encodeDimensions(signal.Dimensions)
	if err != nil {
		t.Fatal(err)
	}
	second := signal
	second.ID = newID()
	second.ExternalID = newID()
	second.EntityID = "second-tank"
	rows := []signalRow{
		{signal: signal, dedupKey: signal.DedupKey(), dedupVersion: uint64(time.Now().UnixNano()), dimensions: dimensions, payload: "{}"},
		{signal: second, dedupKey: second.DedupKey(), dedupVersion: uint64(time.Now().UnixNano()), dimensions: dimensions, payload: "{}"},
	}
	if err := store.insertSignalRows(ctx, rows); err != nil {
		t.Fatal(err)
	}

	result, err := store.ConsoleEvents(ctx, scenario, now.Add(-time.Second).Add(time.Nanosecond), now.Add(time.Second).Add(time.Nanosecond), 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 2 || !result.Events[0].OccurredAt.Equal(now) {
		t.Fatalf("expected two readings at %s, got %+v", now, result)
	}
}
