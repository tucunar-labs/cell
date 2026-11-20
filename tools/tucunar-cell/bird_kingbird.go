package canarynode

import (
	"context"
	"sort"
	"strings"
)

func init() {
	registerCapabilityExecutor(capabilityKingbird, func(d CapabilityDefinition) CapabilityExecutor { return newFunctionalCapability(d, runKingbird) })
}

func runKingbird(ctx context.Context, request CapabilityRequest) (any, error) {
	p, err := decodeCapabilityPayload[KingbirdCorrelateRequest](request)
	if err != nil {
		return nil, err
	}
	refs := uniqueSorted(append(append(append([]string{}, p.IncidentIDs...), p.SignalRefs...), p.TopologyRefs...))
	hypotheses := make([]Hypothesis, 0, 3)
	for _, rule := range []struct {
		words []string
		cause string
	}{{[]string{"topology", "network", "route"}, "topology_or_network_path"}, {[]string{"signal", "metric", "latency", "loss"}, "signal_transport_degradation"}, {[]string{"incident", "error", "failure"}, "upstream_service_failure"}} {
		matched := []string{}
		for _, ref := range refs {
			low := strings.ToLower(ref)
			for _, word := range rule.words {
				if strings.Contains(low, word) {
					matched = append(matched, ref)
					break
				}
			}
		}
		if len(matched) > 0 {
			hypotheses = append(hypotheses, Hypothesis{Cause: rule.cause, Confidence: minFloat(0.95, 0.4+0.15*float64(len(matched))), EvidenceRefs: matched})
		}
	}
	sort.SliceStable(hypotheses, func(i, j int) bool {
		if hypotheses[i].Confidence != hypotheses[j].Confidence {
			return hypotheses[i].Confidence > hypotheses[j].Confidence
		}
		return hypotheses[i].Cause < hypotheses[j].Cause
	})
	return KingbirdCorrelateResponse{CapabilityResult: CapabilityResult{ContractVersion: capabilityContractVersion, RequestID: request.RequestID, Capability: string(capabilityKingbird), Operation: "correlate", Status: "completed"}, Hypotheses: hypotheses}, nil
}
func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
