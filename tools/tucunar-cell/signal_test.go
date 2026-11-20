package canarynode

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSignalNormalize(t *testing.T) {
	occurredAt := time.Date(2026, 8, 30, 20, 0, 0, 0, time.FixedZone("CLT", -4*60*60))
	tests := []struct {
		name    string
		signal  Signal
		wantErr string
	}{
		{
			name: "normalizes and generates id",
			signal: Signal{
				Source:     " crane-monitor ",
				TenantID:   " acme ",
				EntityID:   " crane-17 ",
				Kind:       "measurement",
				Name:       "load",
				Value:      8.4,
				Unit:       "tons",
				OccurredAt: occurredAt,
			},
		},
		{
			name:    "requires source",
			signal:  Signal{TenantID: "acme", EntityID: "crane-17", Kind: "measurement", Name: "load", OccurredAt: time.Now()},
			wantErr: "source is required",
		},
		{
			name:    "requires timestamp",
			signal:  Signal{Source: "internal", TenantID: "acme", EntityID: "crane-17", Kind: "measurement", Name: "load"},
			wantErr: "occurred_at is required",
		},
		{
			name:    "rejects invalid id",
			signal:  Signal{ID: "not-a-uuid", Source: "internal", TenantID: "acme", EntityID: "crane-17", Kind: "measurement", Name: "load", OccurredAt: time.Now()},
			wantErr: "id must be a UUID",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.signal.Normalize()
			if test.wantErr != "" {
				if err == nil || err.Error() != test.wantErr {
					t.Fatalf("Normalize() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Normalize() error = %v", err)
			}
			if !isUUID(got.ID) {
				t.Fatalf("Normalize() generated invalid id %q", got.ID)
			}
			if got.Source != "crane-monitor" || got.TenantID != "acme" || got.EntityID != "crane-17" {
				t.Fatalf("Normalize() did not trim identity fields: %#v", got)
			}
			if got.OccurredAt.Location() != time.UTC {
				t.Fatalf("Normalize() location = %v, want UTC", got.OccurredAt.Location())
			}
			if string(got.Payload) != "{}" {
				t.Fatalf("Normalize() payload = %s, want {}", got.Payload)
			}
		})
	}
}

func TestDecodeSignalRejectsUnknownAndTrailingJSON(t *testing.T) {
	valid := `{"source":"internal","tenant_id":"acme","entity_id":"crane-17","kind":"measurement","name":"load","occurred_at":"2026-08-30T20:00:00Z"}`
	if _, err := decodeSignal([]byte(valid + ` {}`)); err == nil {
		t.Fatal("decodeSignal() accepted trailing JSON")
	}
	if _, err := decodeSignal([]byte(strings.Replace(valid, `"name":"load"`, `"unknown":"field","name":"load"`, 1))); err == nil {
		t.Fatal("decodeSignal() accepted unknown JSON field")
	}
}

func TestMapShopifySignal(t *testing.T) {
	req := httptest.NewRequest("POST", "/webhooks/shopify", strings.NewReader(`{"id":123}`))
	req.Header.Set("X-Shopify-Shop-Domain", "shop.example.com")
	req.Header.Set("X-Shopify-Topic", "checkout_started")
	req.Header.Set("X-Shopify-Webhook-Id", "webhook-123")

	signal, err := mapShopifySignal(req, []byte(`{"id":123}`))
	if err != nil {
		t.Fatalf("mapShopifySignal() error = %v", err)
	}
	if signal.Source != "shopify" || signal.TenantID != "shop.example.com" || signal.EntityID != "shop.example.com" {
		t.Fatalf("unexpected Shopify identity mapping: %#v", signal)
	}
	if signal.Kind != "event" || signal.Name != "checkout_started" || signal.ExternalID != "webhook-123" {
		t.Fatalf("unexpected Shopify event mapping: %#v", signal)
	}
	if string(signal.Payload) != `{"id":123}` {
		t.Fatalf("payload = %s, want original body", signal.Payload)
	}
}

func TestGenericSignalValidationHTTP(t *testing.T) {
	request := httptest.NewRequest("POST", "/v1/signals", strings.NewReader(`{"source":"internal"}`))
	response := httptest.NewRecorder()
	(&Server{}).handleSignal(response, request)
	if response.Code != 400 {
		t.Fatalf("handleSignal() status = %d, want 400", response.Code)
	}
}

func TestGenericSignalIngestAuthentication(t *testing.T) {
	invalidJSON := `{"source":"internal"}`
	tests := []struct {
		name       string
		server     *Server
		configure  func(*http.Request)
		wantStatus int
	}{
		{
			name:       "missing token is rejected",
			server:     &Server{signalToken: "secret"},
			configure:  func(*http.Request) {},
			wantStatus: 401,
		},
		{
			name:   "wrong bearer token is rejected",
			server: &Server{signalToken: "secret"},
			configure: func(request *http.Request) {
				request.Header.Set("Authorization", "Bearer wrong")
			},
			wantStatus: 401,
		},
		{
			name:   "api key is accepted by auth and reaches validation",
			server: &Server{signalToken: "secret"},
			configure: func(request *http.Request) {
				request.Header.Set("X-Canary-API-Key", "secret")
			},
			wantStatus: 400,
		},
		{
			name:       "empty configured token keeps local development mode",
			server:     &Server{},
			configure:  func(*http.Request) {},
			wantStatus: 400,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/v1/signals", strings.NewReader(invalidJSON))
			test.configure(request)
			response := httptest.NewRecorder()
			test.server.handleSignal(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("handleSignal() status = %d, want %d", response.Code, test.wantStatus)
			}
		})
	}
}

func TestGenericSignalBatchIngestAuthentication(t *testing.T) {
	tests := []struct {
		name       string
		token      string
		authorize  func(*http.Request)
		wantStatus int
	}{
		{
			name:       "missing token is rejected",
			token:      "secret",
			authorize:  func(*http.Request) {},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:  "bearer token reaches validation",
			token: "secret",
			authorize: func(request *http.Request) {
				request.Header.Set("Authorization", "Bearer secret")
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "empty token is local development mode",
			token:      "",
			authorize:  func(*http.Request) {},
			wantStatus: http.StatusBadRequest,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/v1/signals/batch", strings.NewReader(`{"signals":[]}`))
			test.authorize(request)
			response := httptest.NewRecorder()
			(&Server{signalToken: test.token}).handleSignalBatch(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("handleSignalBatch() status = %d, want %d", response.Code, test.wantStatus)
			}
		})
	}
}

func TestSignalJSONContract(t *testing.T) {
	signal := Signal{Source: "internal", TenantID: "acme", EntityID: "crane-17", Kind: "measurement", Name: "load", Value: 8.4, OccurredAt: time.Date(2026, 8, 30, 20, 0, 0, 0, time.UTC)}
	data, err := json.Marshal(signal)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"occurred_at":"2026-08-30T20:00:00Z"`) {
		t.Fatalf("JSON contract omitted RFC3339 timestamp: %s", data)
	}
}
