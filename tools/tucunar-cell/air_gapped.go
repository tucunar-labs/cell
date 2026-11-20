package canarynode

import (
	"fmt"
	"os"
	"strings"
)

// isAirGapped reports whether the node is running as a local-only appliance.
// The explicit flag prevents an operator from mistaking a missing cloud URL
// for a deliberate no-egress deployment.
func isAirGapped() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CANARY_AIR_GAPPED"))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// validateAirGappedEnvironment fails closed when an outbound integration is
// configured. The local dashboard, signal ingestion and ClickHouse remain
// available; external alerting and cloud reporting must be explicitly absent.
func validateAirGappedEnvironment() error {
	if !isAirGapped() {
		return nil
	}
	for _, key := range []string{
		"TUCUNAR_API_BASE_URL",
		"TUCUNAR_ENROLLMENT_TOKEN",
		"MERCADOPAGO_ACCESS_TOKEN",
		"ALERT_WEBHOOK_URL",
		"ALERT_WEBHOOK_SECRET",
	} {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			return fmt.Errorf("CANARY_AIR_GAPPED=1 forbids outbound integration %s", key)
		}
	}
	return nil
}
