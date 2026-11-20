package canarynode

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestSignalDedupLookupAcrossPersistedVersions(t *testing.T) {
	if os.Getenv("CANARY_CH_TEST") != "1" {
		t.Skip("set CANARY_CH_TEST=1 for ClickHouse")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := newStore(ctx, env("CLICKHOUSE_ADDR", "localhost:19000"), env("CLICKHOUSE_USER", "default"), env("CLICKHOUSE_PASSWORD", "clickhouse"), env("CLICKHOUSE_DB", "default"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	signal, err := (Signal{ExternalID: newID(), Source: "dedup-integration", TenantID: "test", EntityID: "compressor", Kind: "measurement", Name: "pressure", Value: 42, OccurredAt: time.Now().UTC().Truncate(time.Millisecond)}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	row := signalRow{signal: signal, dedupKey: signal.DedupKey(), dedupVersion: uint64(time.Now().UnixNano()), dimensions: "{}", payload: "{}"}
	// Existence must return one identity across physical versions, without FINAL.
	second := row
	second.signal.OccurredAt = second.signal.OccurredAt.Add(time.Second)
	if err := store.insertSignalRows(ctx, []signalRow{row, second}); err != nil {
		t.Fatal(err)
	}
	keys, err := store.existingSignalKeys(ctx, []signalRow{row})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected one persisted identity, got %d", len(keys))
	}
	retry := signal
	retry.ID = newID()
	retry.OccurredAt = retry.OccurredAt.Add(time.Hour)
	if err := store.InsertSignals(ctx, []Signal{retry}); err != nil {
		t.Fatal(err)
	}
	var count uint64
	if err := store.conn.QueryRow(ctx, "SELECT count() FROM canary_signal_events WHERE dedup_key = ?", row.dedupKey).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("retry added a physical row: got %d, want 2", count)
	}
}
