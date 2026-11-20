package canarynode

import (
	"strings"
	"testing"
)

func TestAirGappedModeRejectsOutboundIntegrations(t *testing.T) {
	t.Setenv("CANARY_AIR_GAPPED", "1")
	t.Setenv("TUCUNAR_API_BASE_URL", "https://app.example.com")
	t.Setenv("TUCUNAR_ENROLLMENT_TOKEN", "")
	t.Setenv("MERCADOPAGO_ACCESS_TOKEN", "")
	t.Setenv("ALERT_WEBHOOK_URL", "")

	if !isAirGapped() {
		t.Fatal("air-gapped mode was not detected")
	}
	if err := validateAirGappedEnvironment(); err == nil || !strings.Contains(err.Error(), "TUCUNAR_API_BASE_URL") {
		t.Fatalf("validateAirGappedEnvironment() error = %v, want cloud URL rejection", err)
	}
}

func TestAirGappedModeAllowsLocalOnlyConfiguration(t *testing.T) {
	t.Setenv("CANARY_AIR_GAPPED", "true")
	t.Setenv("TUCUNAR_API_BASE_URL", "")
	t.Setenv("TUCUNAR_ENROLLMENT_TOKEN", "")
	t.Setenv("MERCADOPAGO_ACCESS_TOKEN", "")
	t.Setenv("ALERT_WEBHOOK_URL", "")

	if err := validateAirGappedEnvironment(); err != nil {
		t.Fatalf("validateAirGappedEnvironment() error = %v", err)
	}
}
