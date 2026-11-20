package canarynode

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

func init() { registerCapabilityExecutor(capabilityPhoenix, newPhoenixExecutor) }
func newPhoenixExecutor(definition CapabilityDefinition) CapabilityExecutor {
	return newFunctionalCapability(definition, phoenixRecover)
}

func phoenixRecover(_ context.Context, request CapabilityRequest) (any, error) {
	p, err := decodeCapabilityPayload[PhoenixRecoverRequest](request)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.IncidentID) == "" || strings.TrimSpace(p.PlanID) == "" {
		return nil, errors.New("incident_id and plan_id are required")
	}
	if len(p.IncidentID) > 256 || len(p.PlanID) > 256 || len(p.Status) > 64 {
		return nil, errors.New("recovery input exceeds bounds")
	}
	state, next := "", ""
	switch p.Action {
	case "plan":
		state, next = "planned", "start"
	case "start":
		if p.Status != "planned" {
			return nil, fmt.Errorf("start requires status planned")
		}
		state, next = "started", "verify"
	case "verify":
		if p.Status != "started" {
			return nil, fmt.Errorf("verify requires status started")
		}
		state, next = "verified", "complete"
	case "cancel":
		if p.Status != "planned" && p.Status != "started" {
			return nil, fmt.Errorf("cancel requires status planned or started")
		}
		state, next = "cancelled", "none"
	default:
		return nil, fmt.Errorf("unsupported recovery action %q", p.Action)
	}
	return PhoenixRecoverResponse{CapabilityResult: CapabilityResult{ContractVersion: capabilityContractVersion, RequestID: request.RequestID, Capability: string(capabilityPhoenix), Operation: request.Operation, Status: "transitioned"}, State: state, NextAction: next, EvidenceRefs: []string{"recovery:" + p.IncidentID + ":" + p.PlanID + ":" + state}}, nil
}
