package canarynode

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const owlMaxItems = 32

func init() { registerCapabilityExecutor(capabilityOwl, newOwlExecutor) }
func newOwlExecutor(definition CapabilityDefinition) CapabilityExecutor {
	return newFunctionalCapability(definition, owlDecide)
}

func owlDecide(_ context.Context, request CapabilityRequest) (any, error) {
	p, err := decodeCapabilityPayload[OwlDecideRequest](request)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Question) == "" || len(p.Question) > 4096 {
		return nil, errors.New("question is required and bounded")
	}
	if len(p.EvidenceRefs) == 0 || len(p.EvidenceRefs) > owlMaxItems || len(p.Constraints) > owlMaxItems {
		return nil, errors.New("evidence and constraints exceed bounds")
	}
	for _, ref := range append(append([]string{}, p.EvidenceRefs...), p.Constraints...) {
		if len(ref) == 0 || len(ref) > 512 {
			return nil, errors.New("evidence and constraint references are bounded")
		}
	}
	decision, rationale, confidence := "pause", "Evidence is present, but the bounded input does not establish a safe action; gather more confirmation.", 0.55
	if len(p.EvidenceRefs) >= 2 && len(p.Constraints) == 0 {
		decision, rationale, confidence = "proceed", "Multiple evidence references are available and no blocking constraint was supplied.", 0.7
	}
	return OwlDecideResponse{CapabilityResult: CapabilityResult{ContractVersion: capabilityContractVersion, RequestID: request.RequestID, Capability: string(capabilityOwl), Operation: request.Operation, Status: "recommended"}, Decision: decision, Alternatives: []string{"pause", "proceed"}, Rationale: fmt.Sprintf("Question considered: %s. %s", p.Question, rationale), Confidence: confidence}, nil
}
