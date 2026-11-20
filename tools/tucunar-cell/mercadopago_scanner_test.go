package canarynode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type mercadoPagoSignalWriter struct {
	signals []Signal
}

func (w *mercadoPagoSignalWriter) InsertSignals(_ context.Context, signals []Signal) error {
	w.signals = append(w.signals, signals...)
	return nil
}

func TestMercadoPagoPaymentsToSignals(t *testing.T) {
	payments := []mercadoPagoPayment{
		{ID: json.RawMessage(`12345`), Status: "approved", DateCreated: "2026-08-31T18:00:00.000-04:00", DateApproved: "2026-08-31T18:00:02.000-04:00", TransactionAmount: 1500, CurrencyID: "clp"},
		{ID: json.RawMessage(`"98765"`), Status: "rejected", DateCreated: "2026-08-31T18:01:00Z", TransactionAmount: 900, CurrencyID: "CLP"},
	}

	signals, err := mercadoPagoPaymentsToSignals(payments, "mercadolibre-test", time.Time{})
	if err != nil {
		t.Fatalf("paymentsToSignals() error = %v", err)
	}
	if len(signals) != 2 {
		t.Fatalf("paymentsToSignals() returned %d signals, want 2", len(signals))
	}
	if signals[0].Source != "mercadopago" || signals[0].TenantID != "mercadolibre-test" || signals[0].ExternalID != "12345" || signals[0].Name != "payment_success" {
		t.Fatalf("unexpected approved signal: %+v", signals[0])
	}
	if signals[0].OccurredAt.Location() != time.UTC || signals[0].OccurredAt.Hour() != 22 {
		t.Fatalf("approved signal timestamp = %s, want UTC 22:00", signals[0].OccurredAt)
	}
	if signals[1].ExternalID != "98765" || signals[1].Name != "payment_rejected" || signals[1].Value != 900 {
		t.Fatalf("unexpected rejected signal: %+v", signals[1])
	}
	var payload mercadoPagoSignalPayload
	if err := json.Unmarshal(signals[0].Payload, &payload); err != nil {
		t.Fatalf("decode signal payload: %v", err)
	}
	if payload.Status != "approved" || payload.CurrencyID != "CLP" {
		t.Fatalf("unexpected signal payload: %+v", payload)
	}
}

func TestMercadoPagoScannerSyncOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/payments/search" {
			t.Fatalf("path = %s, want /v1/payments/search", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("authorization = %q, want test token", got)
		}
		for _, key := range []string{"sort", "criteria", "range", "begin_date", "end_date", "limit", "offset"} {
			if r.URL.Query().Get(key) == "" {
				t.Fatalf("missing query parameter %q", key)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"paging":{"total":1,"limit":50,"offset":0},"results":[{"id":42,"status":"approved","date_created":"2026-08-31T18:00:00Z","date_approved":"2026-08-31T18:00:01Z","transaction_amount":2500,"currency_id":"CLP"}]}`))
	}))
	defer server.Close()

	writer := &mercadoPagoSignalWriter{}
	scanner := newMercadoPagoScanner(writer, &mercadoPagoScannerConfig{
		AccessToken:  "test-token",
		APIBaseURL:   server.URL,
		TenantID:     "mercadolibre-test",
		PollInterval: time.Minute,
		Lookback:     time.Hour,
		PageSize:     50,
		HTTPTimeout:  time.Second,
	})
	scanner.now = func() time.Time { return time.Date(2026, 8, 31, 19, 0, 0, 0, time.UTC) }
	if err := scanner.syncOnce(context.Background()); err != nil {
		t.Fatalf("syncOnce() error = %v", err)
	}
	if len(writer.signals) != 1 || writer.signals[0].ExternalID != "42" {
		t.Fatalf("signals = %+v, want one payment 42", writer.signals)
	}
}

func TestMercadoPagoScannerRejectsInsecureURL(t *testing.T) {
	t.Setenv("MERCADOPAGO_ACCESS_TOKEN", "test-token")
	t.Setenv("MERCADOPAGO_API_BASE_URL", "http://localhost:8080")
	t.Setenv("MERCADOPAGO_ALLOW_INSECURE_HTTP", "")
	if _, err := loadMercadoPagoScannerConfig(); err == nil || !strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("loadMercadoPagoScannerConfig() error = %v, want HTTPS validation", err)
	}
}
