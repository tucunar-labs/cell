package canarynode

import (
	"context"
	"fmt"
	"strings"
)

func init() {
	registerCapabilityExecutor(capabilityHawk, func(d CapabilityDefinition) CapabilityExecutor { return newFunctionalCapability(d, runHawk) })
}

func runHawk(ctx context.Context, request CapabilityRequest) (any, error) {
	p, err := decodeCapabilityPayload[HawkAssessRequest](request)
	if err != nil {
		return nil, err
	}
	findings := []Finding{}
	controls := map[string]bool{}
	for _, c := range p.Controls {
		ref := firstString(c, "asset_ref", "asset_id", "asset")
		if ref != "" {
			controls[ref] = strings.EqualFold(firstString(c, "status", "state", "coverage"), "active") || c["enabled"] == true || c["covered"] == true
		}
	}
	for i, asset := range p.Assets {
		ref := firstString(asset, "asset_ref", "asset_id", "id", "name")
		if ref == "" {
			ref = fmt.Sprintf("asset[%d]", i)
		}
		if !controls[ref] {
			sev := "high"
			if asset["critical"] == false {
				sev = "medium"
			}
			findings = append(findings, Finding{FindingID: "hawk-" + fmt.Sprint(i+1), Severity: sev, Title: "asset lacks an active matching control", AssetRefs: []string{ref}})
		}
	}
	return HawkAssessResponse{CapabilityResult: CapabilityResult{ContractVersion: capabilityContractVersion, RequestID: request.RequestID, Capability: string(capabilityHawk), Operation: "assess", Status: "completed"}, Findings: findings}, nil
}
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
