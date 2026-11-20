package canarynode

import (
	"context"
	"fmt"
	"strings"
)

func init() {
	registerCapabilityExecutor(capabilityHeron, func(d CapabilityDefinition) CapabilityExecutor { return newFunctionalCapability(d, runHeron) })
}

func runHeron(ctx context.Context, request CapabilityRequest) (any, error) {
	p, err := decodeCapabilityPayload[HeronReconcileRequest](request)
	if err != nil {
		return nil, err
	}
	r := HeronReconcileResponse{CapabilityResult: CapabilityResult{ContractVersion: capabilityContractVersion, RequestID: request.RequestID, Capability: string(capabilityHeron), Operation: "reconcile", Status: "completed"}}
	if strings.TrimSpace(p.JoinKey) == "" {
		r.Mismatched = 1
		r.Discrepancies = []Discrepancy{{JoinValue: "", Reason: "insufficient inputs: join_key is required"}}
		return r, nil
	}
	if len(p.Sources) < 2 || (!p.WindowStart.IsZero() && !p.WindowEnd.IsZero() && p.WindowEnd.Before(p.WindowStart)) {
		r.Mismatched = 1
		r.Discrepancies = []Discrepancy{{JoinValue: p.JoinKey, Reason: "insufficient inputs for bounded reconciliation", SourceRefs: uniqueSorted(p.Sources)}}
		return r, nil
	}
	sources := uniqueSorted(p.Sources)
	if len(sources) < 2 {
		r.Mismatched = 1
		r.Discrepancies = []Discrepancy{{JoinValue: p.JoinKey, Reason: "insufficient distinct source references", SourceRefs: sources}}
		return r, nil
	}
	if len(sources) == len(p.Sources) {
		r.Matched = 1
	} else {
		r.Mismatched = 1
		r.Discrepancies = []Discrepancy{{JoinValue: p.JoinKey, Reason: fmt.Sprintf("duplicate source references: supplied %d, distinct %d", len(p.Sources), len(sources)), SourceRefs: sources}}
	}
	return r, nil
}
