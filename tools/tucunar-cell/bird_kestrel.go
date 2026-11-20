package canarynode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
)

type kestrelRule struct {
	Principal string `json:"principal"`
	Action    string `json:"action"`
	Resource  string `json:"resource"`
	Allow     bool   `json:"allow"`
}
type kestrelPolicy struct {
	Version string        `json:"version"`
	Rules   []kestrelRule `json:"rules"`
}

func init() { registerCapabilityExecutor(capabilityKestrel, newKestrelExecutor) }
func newKestrelExecutor(definition CapabilityDefinition) CapabilityExecutor {
	return newFunctionalCapability(definition, kestrelAuthorize)
}

func kestrelAuthorize(_ context.Context, request CapabilityRequest) (any, error) {
	p, err := decodeCapabilityPayload[KestrelAuthorizeRequest](request)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Principal) == "" || strings.TrimSpace(p.Action) == "" || strings.TrimSpace(p.Resource) == "" {
		return nil, errors.New("principal, action, and resource are required")
	}
	if len(p.Principal) > 128 || len(p.Action) > 128 || len(p.Resource) > 256 || len(p.Context) > 32 {
		return nil, errors.New("authorization input exceeds bounds")
	}
	policy := kestrelPolicy{Version: "deny-all-v1"}
	if raw := strings.TrimSpace(os.Getenv("KESTREL_POLICY")); raw != "" {
		if len(raw) > 64<<10 {
			return nil, errors.New("KESTREL_POLICY exceeds bounds")
		}
		if err := json.Unmarshal([]byte(raw), &policy); err != nil {
			return nil, errors.New("KESTREL_POLICY must be valid JSON")
		}
		if policy.Version == "" {
			policy.Version = "configured-v1"
		}
		if len(policy.Rules) > 256 {
			return nil, errors.New("policy has too many rules")
		}
	}
	response := KestrelAuthorizeResponse{CapabilityResult: CapabilityResult{ContractVersion: capabilityContractVersion, RequestID: request.RequestID, Capability: string(capabilityKestrel), Operation: request.Operation, Status: "evaluated"}, PolicyVersion: policy.Version, Allowed: false, Reasons: []string{"no matching allow rule"}}
	for _, rule := range policy.Rules {
		if rule.Principal == p.Principal && rule.Action == p.Action && rule.Resource == p.Resource {
			response.Allowed = rule.Allow
			if rule.Allow {
				response.Reasons = []string{"matched explicit allow rule"}
			} else {
				response.Reasons = []string{"matched explicit deny rule"}
			}
			break
		}
	}
	return response, nil
}
