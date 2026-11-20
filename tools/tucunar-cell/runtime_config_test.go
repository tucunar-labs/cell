package canarynode

import (
	"strings"
	"testing"
)

func TestValidateRuntimeEnvironmentRejectsExposedNodeWithoutOperatorToken(t *testing.T) {
	t.Setenv("CANARY_NODE_BIND", "0.0.0.0")
	t.Setenv("CANARY_NODE_API_TOKEN", "")
	t.Setenv("SIGNAL_INGEST_TOKEN", "")

	err := validateRuntimeEnvironment()
	if err == nil || !strings.Contains(err.Error(), "CANARY_NODE_API_TOKEN") {
		t.Fatalf("validateRuntimeEnvironment() error = %v, want operator token rejection", err)
	}
}

func TestValidateRuntimeEnvironmentAcceptsLoopbackDevelopmentDefaults(t *testing.T) {
	t.Setenv("CANARY_NODE_BIND", "127.0.0.1")
	t.Setenv("CANARY_NODE_API_TOKEN", "")
	t.Setenv("SIGNAL_INGEST_TOKEN", "")
	t.Setenv("SIGNAL_REQUIRE_CONTEXT", "0")
	t.Setenv("ALERT_WEBHOOK_SECRET", "")
	t.Setenv("ALERT_WEBHOOK_URL", "")

	if err := validateRuntimeEnvironment(); err != nil {
		t.Fatalf("validateRuntimeEnvironment() error = %v", err)
	}
}

func TestValidateRuntimeEnvironmentRejectsExposedDirectListener(t *testing.T) {
	t.Setenv("CANARY_NODE_BIND", "")
	t.Setenv("CANARY_NODE_LISTEN_ADDR", "0.0.0.0")
	t.Setenv("CANARY_NODE_API_TOKEN", "")
	t.Setenv("SIGNAL_INGEST_TOKEN", "")

	err := validateRuntimeEnvironment()
	if err == nil || !strings.Contains(err.Error(), "CANARY_NODE_LISTEN_ADDR") {
		t.Fatalf("validateRuntimeEnvironment() error = %v, want listener rejection", err)
	}
}

func TestIsLoopbackBind(t *testing.T) {
	for _, value := range []string{"localhost", "127.0.0.1", "::1", "[::1]"} {
		if !isLoopbackBind(value) {
			t.Errorf("isLoopbackBind(%q) = false", value)
		}
	}
	if isLoopbackBind("10.0.0.4") {
		t.Error("isLoopbackBind() accepted a private non-loopback address")
	}
}
