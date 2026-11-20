package canarynode

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactPayloadRecursesThroughObjectsAndArrays(t *testing.T) {
	input := []byte(`{"token":"top-secret","profile":{"Email":"person@example.test","name":"Ada"},"items":[{"card_number":"4111111111111111","ok":true}]}`)
	keys := map[string]struct{}{"token": {}, "email": {}, "card_number": {}}
	redacted, err := redactPayload(input, keys)
	if err != nil {
		t.Fatalf("redactPayload() error = %v", err)
	}
	var value map[string]any
	if err := json.Unmarshal(redacted, &value); err != nil {
		t.Fatalf("redacted payload is invalid JSON: %v", err)
	}
	if value["token"] != redactedPayloadValue {
		t.Fatalf("top-level token = %#v", value["token"])
	}
	profile := value["profile"].(map[string]any)
	if profile["Email"] != redactedPayloadValue || profile["name"] != "Ada" {
		t.Fatalf("nested profile = %#v", profile)
	}
	items := value["items"].([]any)
	if items[0].(map[string]any)["card_number"] != redactedPayloadValue {
		t.Fatalf("array item = %#v", items[0])
	}
	if strings.Contains(string(redacted), "top-secret") || strings.Contains(string(redacted), "4111111111111111") {
		t.Fatal("redacted payload still contains sensitive values")
	}
}

func TestRedactPayloadPreservesBytesWhenNoKeysConfigured(t *testing.T) {
	input := []byte(` { "token": "keep-for-local-debug" } `)
	redacted, err := redactPayload(input, nil)
	if err != nil {
		t.Fatalf("redactPayload() error = %v", err)
	}
	if string(redacted) != string(input) {
		t.Fatalf("payload changed without configured keys: %q", redacted)
	}
}

func TestPayloadTTLFromEnvIsBounded(t *testing.T) {
	t.Setenv("CANARY_RAW_PAYLOAD_TTL_DAYS", "30")
	if got := payloadTTLFromEnv(); got != 30 {
		t.Fatalf("payloadTTLFromEnv() = %d, want 30", got)
	}
	t.Setenv("CANARY_RAW_PAYLOAD_TTL_DAYS", "0")
	if got := payloadTTLFromEnv(); got != defaultPayloadTTLDays {
		t.Fatalf("payloadTTLFromEnv() invalid value = %d, want default", got)
	}
	t.Setenv("CANARY_RAW_PAYLOAD_TTL_DAYS", "4000")
	if got := payloadTTLFromEnv(); got != defaultPayloadTTLDays {
		t.Fatalf("payloadTTLFromEnv() oversized value = %d, want default", got)
	}
}
