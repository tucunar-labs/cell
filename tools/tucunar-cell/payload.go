package canarynode

import (
	"bytes"
	"encoding/json"
	"strings"
)

const redactedPayloadValue = "[REDACTED]"

func loadPayloadRedactKeys() map[string]struct{} {
	keys := make(map[string]struct{})
	for _, raw := range strings.Split(env("CANARY_REDACT_PAYLOAD_KEYS", ""), ",") {
		key := strings.ToLower(strings.TrimSpace(raw))
		if key != "" && len(key) <= maxSignalStringLength {
			keys[key] = struct{}{}
		}
	}
	return keys
}

// redactPayload removes configured sensitive fields before a payload reaches
// ClickHouse. Matching is case-insensitive and recursive through objects and
// arrays. With no configured keys, the original bytes are preserved exactly.
func redactPayload(payload []byte, keys map[string]struct{}) ([]byte, error) {
	if len(keys) == 0 || len(payload) == 0 {
		return payload, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	redactPayloadValue(&value, keys)
	return json.Marshal(value)
}

func redactPayloadValue(value *any, keys map[string]struct{}) {
	switch current := (*value).(type) {
	case map[string]any:
		for key, child := range current {
			if _, redact := keys[strings.ToLower(key)]; redact {
				current[key] = redactedPayloadValue
				continue
			}
			redactPayloadValue(&child, keys)
			current[key] = child
		}
	case []any:
		for index := range current {
			redactPayloadValue(&current[index], keys)
		}
	}
}
