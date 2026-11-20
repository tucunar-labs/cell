package canarynode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCloudClientRetriesTransientResponsesAndKeepsHeaders(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("Idempotency-Key"); got != "report:123" {
			t.Errorf("idempotency key = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q", got)
		}
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"accepted"}`))
	}))
	defer server.Close()

	client := &cloudClient{baseURL: server.URL, http: server.Client(), sleep: func(time.Duration) {}, now: time.Now}
	response, err := client.postJSON(context.Background(), "/v1/report", "access-token", "report:123", []byte(`{"ok":true}`), time.Second)
	if err != nil {
		t.Fatalf("postJSON() error = %v", err)
	}
	if string(response) != `{"status":"accepted"}` {
		t.Fatalf("response = %s", response)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

func TestStableUUIDIsDeterministic(t *testing.T) {
	first := stableUUID("incident:key", "updated", "2026-08-31T00:00:00Z")
	if first != stableUUID("incident:key", "updated", "2026-08-31T00:00:00Z") {
		t.Fatal("stableUUID changed for identical inputs")
	}
	if first == stableUUID("incident:key", "recovered", "2026-08-31T00:00:00Z") {
		t.Fatal("stableUUID collided for different inputs")
	}
}

func TestLoadCloudConfigRequiresHTTPSAndScopedUUIDs(t *testing.T) {
	t.Setenv("TUCUNAR_API_BASE_URL", "http://control.example.test")
	t.Setenv("CANARY_ORGANIZATION_ID", "550e8400-e29b-41d4-a716-446655440000")
	t.Setenv("CANARY_ID", "550e8400-e29b-41d4-a716-446655440001")
	t.Setenv("TUCUNAR_API_ALLOW_INSECURE_HTTP", "")
	if _, err := loadCloudConfig(); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("loadCloudConfig() error = %v, want HTTPS rejection", err)
	}

	t.Setenv("TUCUNAR_API_ALLOW_INSECURE_HTTP", "1")
	config, err := loadCloudConfig()
	if err != nil {
		t.Fatalf("loadCloudConfig() error = %v", err)
	}
	if config.DeploymentMode != "on_premise" {
		t.Fatalf("deployment mode = %q", config.DeploymentMode)
	}
}

func TestLoadCloudConfigRejectsURLCredentials(t *testing.T) {
	t.Setenv("TUCUNAR_API_BASE_URL", "https://user:password@control.example.test")
	t.Setenv("CANARY_ORGANIZATION_ID", "550e8400-e29b-41d4-a716-446655440000")
	t.Setenv("CANARY_ID", "550e8400-e29b-41d4-a716-446655440001")
	if _, err := loadCloudConfig(); err == nil {
		t.Fatal("loadCloudConfig() accepted URL credentials")
	}
}

func TestLoadCloudConfigRequiresControlPlaneForCloudMode(t *testing.T) {
	t.Setenv("TUCUNAR_API_BASE_URL", "")
	t.Setenv("CANARY_DEPLOYMENT_MODE", "cloud")
	if _, err := loadCloudConfig(); err == nil || !strings.Contains(err.Error(), "TUCUNAR_API_BASE_URL") {
		t.Fatalf("loadCloudConfig() error = %v, want cloud control-plane requirement", err)
	}
}

func TestRetryPolicyDoesNotRetryClientErrors(t *testing.T) {
	if isRetryableCloudStatus(http.StatusUnauthorized) {
		t.Fatal("401 must not be retried")
	}
	if isRetryableCloudStatus(http.StatusBadRequest) {
		t.Fatal("400 must not be retried")
	}
	if !isRetryableCloudStatus(http.StatusTooManyRequests) || !isRetryableCloudStatus(http.StatusBadGateway) {
		t.Fatal("429 and 502 must be retried")
	}
}

func TestCloudAuthErrorRequiresReenrollment(t *testing.T) {
	if !cloudAuthError(&cloudHTTPError{StatusCode: http.StatusUnauthorized}) {
		t.Fatal("401 should invalidate the node token")
	}
	if !cloudAuthError(&cloudHTTPError{StatusCode: http.StatusForbidden}) {
		t.Fatal("403 should invalidate the node token")
	}
	if cloudAuthError(&cloudHTTPError{StatusCode: http.StatusBadGateway}) {
		t.Fatal("502 should remain a transient transport error")
	}
}

func TestInvalidateAccessTokenPersistsWithoutDroppingIdentity(t *testing.T) {
	statePath := t.TempDir() + "/state.json"
	reporter := &cloudReporter{
		config: cloudConfig{StateFile: statePath},
		state: cloudState{
			NodeInstanceID:       "550e8400-e29b-41d4-a716-446655440000",
			OrganizationID:       "550e8400-e29b-41d4-a716-446655440001",
			CanaryID:             "550e8400-e29b-41d4-a716-446655440002",
			NodeID:               "550e8400-e29b-41d4-a716-446655440003",
			AccessToken:          "revoked-token",
			AccessTokenExpiresAt: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
			EnrollmentTokenHash:  "previous-token",
		},
	}
	if err := reporter.invalidateAccessToken(); err != nil {
		t.Fatalf("invalidateAccessToken() error = %v", err)
	}
	state, err := loadCloudState(statePath)
	if err != nil {
		t.Fatalf("loadCloudState() error = %v", err)
	}
	if state.AccessToken != "" || !state.AccessTokenExpiresAt.IsZero() {
		t.Fatalf("token was not cleared: %#v", state)
	}
	if state.NodeID == "" || state.CanaryID == "" || state.EnrollmentTokenHash != "previous-token" {
		t.Fatalf("identity was changed while clearing token: %#v", state)
	}
}

func TestMakeRollupMaintainsContractInvariants(t *testing.T) {
	rollup := makeRollup(cloudState{
		OrganizationID: "550e8400-e29b-41d4-a716-446655440000",
		CanaryID:       "550e8400-e29b-41d4-a716-446655440001",
		NodeID:         "550e8400-e29b-41d4-a716-446655440002",
	}, time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC), []ConversionAlert{
		{ShopID: "shop-a", CheckoutStarted: 100, PaymentSuccess: 10, ConversionRate: 0.1},
		{ShopID: "shop-b", CheckoutStarted: 100, PaymentSuccess: 50, ConversionRate: 0.5},
	})
	if rollup.Summary.TotalShops != len(rollup.Shops) || rollup.Summary.AtRiskShops != 1 {
		t.Fatalf("summary = %#v, shops = %d", rollup.Summary, len(rollup.Shops))
	}
	if rollup.Summary.CheckoutStarted != 200 || rollup.Summary.PaymentSuccess != 60 || rollup.Summary.ConversionRate != 0.3 {
		t.Fatalf("summary totals = %#v", rollup.Summary)
	}
	if rollup.Signals == nil {
		t.Fatal("rollup signals must be an empty array, not null")
	}
}

func TestMakeSignalRollupsPreservesGenericIdentity(t *testing.T) {
	last := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	rollups := makeSignalRollups([]signalSummary{{Source: "crane-monitor", TenantID: "acme", EntityID: "crane-17", Kind: "measurement", Name: "load", Unit: "tons", Samples: 4, AverageValue: 8.25, LastOccurredAt: last}})
	if len(rollups) != 1 || rollups[0].Source != "crane-monitor" || rollups[0].TenantID != "acme" || rollups[0].EntityID != "crane-17" || rollups[0].Unit != "tons" || rollups[0].Samples != 4 {
		t.Fatalf("generic rollup = %#v", rollups)
	}
}

func TestCloudReporterEnrollsWithOneTimeBearerAndPersistsState(t *testing.T) {
	const (
		organizationID = "550e8400-e29b-41d4-a716-446655440000"
		canaryID       = "550e8400-e29b-41d4-a716-446655440001"
	)
	var received enrollmentRequest
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/nodes/enroll" || r.Method != http.MethodPost {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer one-time-token" {
			t.Fatalf("authorization = %q", got)
		}
		if got := r.Header.Get("Idempotency-Key"); got != "enroll:550e8400-e29b-41d4-a716-446655440002" {
			t.Fatalf("idempotency key = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"contract_version":"1.0","organization_id":"550e8400-e29b-41d4-a716-446655440000","canary_id":"550e8400-e29b-41d4-a716-446655440001","node_id":"550e8400-e29b-41d4-a716-446655440003","deployment_mode":"on_premise","access_token":"issued-node-token","access_token_expires_at":"2099-01-01T00:00:00Z","reporting":{"heartbeat_interval_seconds":60,"rollup_interval_seconds":60},"raw_event_forwarding":false}`))
	}))
	defer server.Close()

	statePath := t.TempDir() + "/state.json"
	config := &cloudConfig{
		BaseURL: server.URL, EnrollmentToken: "one-time-token", OrganizationID: organizationID,
		CanaryID: canaryID, DeploymentMode: "on_premise", NodeInstanceID: "550e8400-e29b-41d4-a716-446655440002",
		NodeVersion: "0.1.0", StateFile: statePath, HeartbeatInterval: time.Minute, RollupInterval: time.Minute,
	}
	reporter, err := newCloudReporter(nil, config)
	if err != nil {
		t.Fatalf("newCloudReporter() error = %v", err)
	}
	reporter.client.http = server.Client()
	if err := reporter.ensureEnrolled(context.Background()); err != nil {
		t.Fatalf("ensureEnrolled() error = %v", err)
	}
	if received.ContractVersion != cloudContractVersion || received.OrganizationID != organizationID || received.CanaryID != canaryID {
		t.Fatalf("enrollment request = %#v", received)
	}
	state, err := loadCloudState(statePath)
	if err != nil {
		t.Fatalf("loadCloudState() error = %v", err)
	}
	if state.AccessToken != "issued-node-token" || state.NodeID == "" {
		t.Fatalf("persisted state = %#v", state)
	}
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("state file permissions = %v", info.Mode().Perm())
	}
}

func TestCloudReporterDoesNotReuseRejectedEnrollmentToken(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	config := &cloudConfig{
		BaseURL: server.URL, EnrollmentToken: "rejected-once", OrganizationID: "550e8400-e29b-41d4-a716-446655440000",
		CanaryID: "550e8400-e29b-41d4-a716-446655440001", DeploymentMode: "on_premise",
		NodeInstanceID: "550e8400-e29b-41d4-a716-446655440002", StateFile: t.TempDir() + "/state.json",
	}
	reporter, err := newCloudReporter(nil, config)
	if err != nil {
		t.Fatalf("newCloudReporter() error = %v", err)
	}
	reporter.client.http = server.Client()
	if err := reporter.ensureEnrolled(context.Background()); err == nil {
		t.Fatal("ensureEnrolled() unexpectedly succeeded")
	}
	if err := reporter.ensureEnrolled(context.Background()); err == nil || !strings.Contains(err.Error(), "provide a new token") {
		t.Fatalf("second ensureEnrolled() error = %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("enrollment requests = %d, want 1", got)
	}
}
