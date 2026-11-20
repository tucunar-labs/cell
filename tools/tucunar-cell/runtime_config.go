package canarynode

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

func validateRuntimeEnvironment() error {
	problems := make([]string, 0)
	if rawPort := strings.TrimSpace(os.Getenv("PORT")); rawPort != "" {
		port, err := strconv.Atoi(rawPort)
		if err != nil || port < 1 || port > 65535 {
			problems = append(problems, "PORT must be an integer between 1 and 65535")
		}
	}
	if rawTTL := strings.TrimSpace(os.Getenv("CANARY_RAW_PAYLOAD_TTL_DAYS")); rawTTL != "" {
		ttl, err := strconv.Atoi(rawTTL)
		if err != nil || ttl < 1 || ttl > maxPayloadTTLDays {
			problems = append(problems, fmt.Sprintf("CANARY_RAW_PAYLOAD_TTL_DAYS must be between 1 and %d", maxPayloadTTLDays))
		}
	}
	if rawRequireContext := strings.TrimSpace(os.Getenv("SIGNAL_REQUIRE_CONTEXT")); rawRequireContext != "" && rawRequireContext != "0" && rawRequireContext != "1" {
		problems = append(problems, "SIGNAL_REQUIRE_CONTEXT must be 0 or 1")
	}
	if os.Getenv("SIGNAL_REQUIRE_CONTEXT") == "1" {
		if strings.TrimSpace(os.Getenv("CANARY_ORGANIZATION_ID")) == "" || strings.TrimSpace(os.Getenv("CANARY_ID")) == "" {
			problems = append(problems, "CANARY_ORGANIZATION_ID and CANARY_ID are required when SIGNAL_REQUIRE_CONTEXT=1")
		}
	}
	if webhookSecret := strings.TrimSpace(os.Getenv("ALERT_WEBHOOK_SECRET")); webhookSecret != "" && strings.TrimSpace(os.Getenv("ALERT_WEBHOOK_URL")) == "" {
		problems = append(problems, "ALERT_WEBHOOK_SECRET requires ALERT_WEBHOOK_URL")
	}
	if operatorToken := strings.TrimSpace(os.Getenv("CANARY_NODE_API_TOKEN")); operatorToken != "" && len(operatorToken) < 16 {
		problems = append(problems, "CANARY_NODE_API_TOKEN must contain at least 16 characters")
	}
	bind := strings.TrimSpace(os.Getenv("CANARY_NODE_BIND"))
	if bind != "" && !isLoopbackBind(bind) {
		if strings.TrimSpace(os.Getenv("CANARY_NODE_API_TOKEN")) == "" {
			problems = append(problems, "CANARY_NODE_API_TOKEN is required when CANARY_NODE_BIND is not loopback")
		}
		if strings.TrimSpace(os.Getenv("SIGNAL_INGEST_TOKEN")) == "" {
			problems = append(problems, "SIGNAL_INGEST_TOKEN is required when CANARY_NODE_BIND is not loopback")
		}
	}
	listenAddr := strings.TrimSpace(os.Getenv("CANARY_NODE_LISTEN_ADDR"))
	if listenAddr == "" {
		listenAddr = "127.0.0.1"
	}
	if bind == "" && !isLoopbackBind(listenAddr) && strings.TrimSpace(os.Getenv("CANARY_NODE_API_TOKEN")) == "" {
		problems = append(problems, "CANARY_NODE_API_TOKEN is required when CANARY_NODE_LISTEN_ADDR is not loopback")
	}
	for _, key := range []string{"CANARY_HEARTBEAT_INTERVAL_SECONDS", "CANARY_ROLLUP_INTERVAL_SECONDS", "CANARY_OUTBOX_FLUSH_INTERVAL_SECONDS", "MERCADOPAGO_POLL_INTERVAL_SECONDS"} {
		if raw := strings.TrimSpace(os.Getenv(key)); raw != "" {
			seconds, err := strconv.Atoi(raw)
			if err != nil || seconds < 1 {
				problems = append(problems, key+" must be a positive integer in seconds")
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid runtime configuration: %s", strings.Join(problems, "; "))
	}
	return nil
}

func isLoopbackBind(value string) bool {
	if value == "localhost" || value == "127.0.0.1" || value == "::1" || value == "[::1]" {
		return true
	}
	if parsed := net.ParseIP(strings.Trim(value, "[]")); parsed != nil {
		return parsed.IsLoopback()
	}
	return false
}
