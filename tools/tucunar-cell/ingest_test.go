package canarynode

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSetAlertWebhookHeadersSignsBodyAndAddsIdempotencyKey(t *testing.T) {
	t.Setenv("ALERT_WEBHOOK_SECRET", "alert-secret")
	request := httptest.NewRequest("POST", "http://example.test/alerts", bytes.NewReader(nil))
	body := []byte(`{"incident_key":"incident-1"}`)
	setAlertWebhookHeaders(request, body, "incident-1:opened")

	mac := hmac.New(sha256.New, []byte("alert-secret"))
	_, _ = mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if request.Header.Get("X-Tucunar-Signature") != want {
		t.Fatalf("signature = %q, want %q", request.Header.Get("X-Tucunar-Signature"), want)
	}
	if request.Header.Get("Idempotency-Key") != "incident-1:opened" {
		t.Fatalf("idempotency key = %q", request.Header.Get("Idempotency-Key"))
	}
	if request.Header.Get("X-Tucunar-Event") != "incident" {
		t.Fatalf("event = %q", request.Header.Get("X-Tucunar-Event"))
	}
}

func TestVerifyShopifyHMACAcceptsOnlyTheExactBodyAndSecret(t *testing.T) {
	secret := "shopify-secret"
	body := []byte(`{"id":"evt-1","type":"payment_success"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if !verifyShopifyHMAC(secret, body, signature) {
		t.Fatal("verifyShopifyHMAC() rejected a valid signature")
	}
	if verifyShopifyHMAC(secret, append(body, ' '), signature) {
		t.Fatal("verifyShopifyHMAC() accepted a modified body")
	}
	if verifyShopifyHMAC("other-secret", body, signature) {
		t.Fatal("verifyShopifyHMAC() accepted a different secret")
	}
	if verifyShopifyHMAC(secret, body, "not-base64") {
		t.Fatal("verifyShopifyHMAC() accepted an invalid signature encoding")
	}
}

func TestApplySignalContextBindsAndRejectsCrossTenantSignals(t *testing.T) {
	server := &Server{organizationID: "org-1", canaryID: "canary-1"}
	input := Signal{Source: "erp", TenantID: "tenant", EntityID: "entity", Kind: "measurement", Name: "load", OccurredAt: time.Now().UTC()}

	bound, err := server.applySignalContext(input)
	if err != nil {
		t.Fatalf("applySignalContext() error = %v", err)
	}
	if bound.OrganizationID != "org-1" || bound.CanaryID != "canary-1" {
		t.Fatalf("bound context = %#v", bound)
	}

	input.OrganizationID = "org-2"
	if _, err := server.applySignalContext(input); err == nil || !strings.Contains(err.Error(), "organization_id") {
		t.Fatalf("cross-organization signal error = %v", err)
	}
	input.OrganizationID = ""
	input.CanaryID = "canary-2"
	if _, err := server.applySignalContext(input); err == nil || !strings.Contains(err.Error(), "canary_id") {
		t.Fatalf("cross-canary signal error = %v", err)
	}
}

func TestAuthorizeOperatorAPIUsesBearerOrAPIKey(t *testing.T) {
	server := &Server{operatorToken: "operator-token-with-32-bytes-0000"}
	validBearer := httptest.NewRequest("GET", "/", nil)
	validBearer.Header.Set("Authorization", "Bearer operator-token-with-32-bytes-0000")
	if !server.authorizeOperatorAPI(validBearer) {
		t.Fatal("authorizeOperatorAPI() rejected a valid bearer token")
	}
	validKey := httptest.NewRequest("GET", "/", nil)
	validKey.Header.Set("X-Canary-API-Key", "operator-token-with-32-bytes-0000")
	if !server.authorizeOperatorAPI(validKey) {
		t.Fatal("authorizeOperatorAPI() rejected a valid API key")
	}
	invalid := httptest.NewRequest("GET", "/", nil)
	invalid.Header.Set("Authorization", "Bearer wrong-token")
	if server.authorizeOperatorAPI(invalid) {
		t.Fatal("authorizeOperatorAPI() accepted an invalid token")
	}
}

func TestApplySignalContextRequiresConfiguredIdentityWhenEnabled(t *testing.T) {
	server := &Server{requireContext: true}
	input := Signal{Source: "erp", TenantID: "tenant", EntityID: "entity", Kind: "measurement", Name: "load", OccurredAt: time.Now().UTC()}
	if _, err := server.applySignalContext(input); err == nil {
		t.Fatal("applySignalContext() accepted a signal without node identity")
	}
}

func TestReadRequestBodyEnforcesConfiguredLimit(t *testing.T) {
	request := httptest.NewRequest("POST", "/v1/signals", bytes.NewReader([]byte("12345")))
	body, err := readRequestBody(request, 5)
	if err != nil || string(body) != "12345" {
		t.Fatalf("readRequestBody() exact limit = %q, %v", body, err)
	}

	request = httptest.NewRequest("POST", "/v1/signals", bytes.NewReader([]byte("123456")))
	if _, err := readRequestBody(request, 5); err == nil || err.Error() != "request body too large" {
		t.Fatalf("readRequestBody() over limit error = %v", err)
	}
}

func TestDedupKeyScopesOrganizationAndCanary(t *testing.T) {
	base := Signal{ID: "550e8400-e29b-41d4-a716-446655440000", OrganizationID: "org-1", CanaryID: "canary-1", ExternalID: "provider-1", Source: "erp", TenantID: "tenant", EntityID: "entity", Kind: "event", Name: "reading"}
	if base.DedupKey() == (Signal{ID: base.ID, OrganizationID: "org-2", CanaryID: base.CanaryID, ExternalID: base.ExternalID, Source: base.Source, TenantID: base.TenantID}).DedupKey() {
		t.Fatal("DedupKey() did not scope external identity by organization")
	}
	if base.DedupKey() == (Signal{ID: base.ID, OrganizationID: base.OrganizationID, CanaryID: "canary-2", ExternalID: base.ExternalID, Source: base.Source, TenantID: base.TenantID}).DedupKey() {
		t.Fatal("DedupKey() did not scope external identity by canary")
	}
}
