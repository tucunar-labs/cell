package canarynode

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

const (
	maxSignalStringLength = 512
	maxSignalDimensions   = 32
)

// Signal is Tucunar's source-neutral operational measurement/event contract.
// Connectors (including Shopify) translate their native payload into Signal.
type Signal struct {
	ID             string            `json:"id,omitempty"`
	ExternalID     string            `json:"external_id,omitempty"`
	OrganizationID string            `json:"organization_id,omitempty"`
	CanaryID       string            `json:"canary_id,omitempty"`
	SiteID         string            `json:"site_id,omitempty"`
	Source         string            `json:"source"`
	TenantID       string            `json:"tenant_id"`
	EntityID       string            `json:"entity_id"`
	Kind           string            `json:"kind"`
	Name           string            `json:"name"`
	Value          float64           `json:"value,omitempty"`
	Unit           string            `json:"unit,omitempty"`
	Dimensions     map[string]string `json:"dimensions,omitempty"`
	Payload        json.RawMessage   `json:"payload,omitempty"`
	OccurredAt     time.Time         `json:"occurred_at"`
}

// Normalize validates an incoming signal and fills server-owned defaults.
func (s Signal) Normalize() (Signal, error) {
	s.Source = strings.TrimSpace(s.Source)
	s.OrganizationID = strings.TrimSpace(s.OrganizationID)
	s.CanaryID = strings.TrimSpace(s.CanaryID)
	s.SiteID = strings.TrimSpace(s.SiteID)
	s.TenantID = strings.TrimSpace(s.TenantID)
	s.EntityID = strings.TrimSpace(s.EntityID)
	s.Kind = strings.TrimSpace(s.Kind)
	s.Name = strings.TrimSpace(s.Name)
	s.Unit = strings.TrimSpace(s.Unit)

	for field, value := range map[string]string{
		"source":    s.Source,
		"tenant_id": s.TenantID,
		"entity_id": s.EntityID,
		"kind":      s.Kind,
		"name":      s.Name,
	} {
		if value == "" {
			return Signal{}, fmt.Errorf("%s is required", field)
		}
		if len(value) > maxSignalStringLength {
			return Signal{}, fmt.Errorf("%s is too long", field)
		}
	}
	if len(s.ExternalID) > maxSignalStringLength {
		return Signal{}, fmt.Errorf("external_id is too long")
	}
	for field, value := range map[string]string{
		"organization_id": s.OrganizationID,
		"canary_id":       s.CanaryID,
		"site_id":         s.SiteID,
	} {
		if len(value) > maxSignalStringLength {
			return Signal{}, fmt.Errorf("%s is too long", field)
		}
	}
	if s.ID == "" {
		s.ID = newID()
	} else if !isUUID(s.ID) {
		return Signal{}, fmt.Errorf("id must be a UUID")
	}
	if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
		return Signal{}, fmt.Errorf("value must be finite")
	}
	if s.OccurredAt.IsZero() {
		return Signal{}, fmt.Errorf("occurred_at is required")
	}
	s.OccurredAt = s.OccurredAt.UTC()
	if len(s.Dimensions) > maxSignalDimensions {
		return Signal{}, fmt.Errorf("dimensions cannot contain more than %d entries", maxSignalDimensions)
	}
	for key, value := range s.Dimensions {
		if strings.TrimSpace(key) == "" || len(key) > maxSignalStringLength || len(value) > maxSignalStringLength {
			return Signal{}, fmt.Errorf("dimension keys and values must be non-empty and no longer than %d bytes", maxSignalStringLength)
		}
	}
	if len(s.Payload) == 0 {
		s.Payload = json.RawMessage(`{}`)
	} else if !json.Valid(s.Payload) {
		return Signal{}, fmt.Errorf("payload must be valid JSON")
	}
	return s, nil
}

// DedupKey returns the durable identity used by the ClickHouse append-only
// event ledger. Provider identities are scoped by source and tenant; signals
// without one use the server/client UUID. The hash keeps the ORDER BY key
// bounded even when a provider sends a long external identifier.
func (s Signal) DedupKey() string {
	identity := "id:" + s.OrganizationID + "\x1f" + s.CanaryID + "\x1f" + s.ID
	if s.ExternalID != "" {
		identity = "external:" + s.OrganizationID + "\x1f" + s.CanaryID + "\x1f" + s.Source + "\x1f" + s.TenantID + "\x1f" + s.ExternalID
	}
	digest := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%x", digest[:])
}

func encodeDimensions(dimensions map[string]string) (string, error) {
	if len(dimensions) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(dimensions)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func decodeSignal(body []byte) (Signal, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var signal Signal
	if err := decoder.Decode(&signal); err != nil {
		return Signal{}, fmt.Errorf("invalid signal JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Signal{}, fmt.Errorf("request must contain one JSON object")
		}
		return Signal{}, fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return signal.Normalize()
}

func isUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, char := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if char != '-' {
				return false
			}
			continue
		}
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}
