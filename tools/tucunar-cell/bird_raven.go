package canarynode

import (
	"context"
	"fmt"
	"sort"
	"time"
)

func init() {
	registerCapabilityExecutor(capabilityRaven, func(d CapabilityDefinition) CapabilityExecutor { return newFunctionalCapability(d, runRaven) })
}

func runRaven(ctx context.Context, request CapabilityRequest) (any, error) {
	p, err := decodeCapabilityPayload[RavenEvidenceRequest](request)
	if err != nil {
		return nil, err
	}
	refs := uniqueSorted(append(append([]string{}, p.EventRefs...), p.EvidenceRefs...))
	if len(refs) == 0 {
		if reader := condorWindowReaderFromContext(ctx); reader != nil {
			since := time.Now().UTC().Add(-15 * time.Minute)
			signals, readErr := reader.ReadWindow(ctx, "raven-"+request.RequestID, since, time.Now().UTC())
			if readErr != nil {
				return nil, fmt.Errorf("read evidence window from Condor: %w", readErr)
			}
			for _, signal := range signals {
				if signal.ID != "" {
					refs = append(refs, signal.ID)
				}
			}
			refs = uniqueSorted(refs)
		}
	}
	timeline := make([]map[string]any, 0, len(refs))
	for i, ref := range refs {
		timeline = append(timeline, map[string]any{"sequence": i + 1, "ref": ref, "incident_id": p.IncidentID, "timestamp": "", "provenance": []string{ref}})
	}
	sort.SliceStable(timeline, func(i, j int) bool {
		a, _ := timeline[i]["timestamp"].(string)
		b, _ := timeline[j]["timestamp"].(string)
		if a != b && a != "" && b != "" {
			return a < b
		}
		return fmt.Sprint(timeline[i]["ref"]) < fmt.Sprint(timeline[j]["ref"])
	})
	ordered := make([]string, 0, len(refs))
	for _, item := range timeline {
		ordered = append(ordered, item["ref"].(string))
	}
	return RavenEvidenceResponse{CapabilityResult: CapabilityResult{ContractVersion: capabilityContractVersion, RequestID: request.RequestID, Capability: string(capabilityRaven), Operation: "evidence", Status: "completed"}, Timeline: timeline, EvidenceRefs: ordered}, nil
}
