package canarynode

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const toucanMaxInputs = 64

func init() { registerCapabilityExecutor(capabilityToucan, newToucanExecutor) }
func newToucanExecutor(definition CapabilityDefinition) CapabilityExecutor {
	return newFunctionalCapability(definition, toucanCompose)
}

func toucanCompose(_ context.Context, request CapabilityRequest) (any, error) {
	p, err := decodeCapabilityPayload[ToucanComposeRequest](request)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.WorkflowID) == "" || len(p.WorkflowID) > 256 || len(p.Inputs) == 0 || len(p.Inputs) > toucanMaxInputs || len(p.RequestedOutputs) > owlMaxItems {
		return nil, errors.New("workflow_id and bounded inputs are required")
	}
	outputs := make(map[string]any)
	if len(p.RequestedOutputs) == 0 {
		for k, v := range p.Inputs {
			outputs[k] = v
		}
	} else {
		for _, key := range p.RequestedOutputs {
			if len(key) == 0 || len(key) > 128 {
				return nil, errors.New("requested output name is bounded")
			}
			if value, ok := p.Inputs[key]; ok {
				outputs[key] = value
			}
		}
	}
	state, next := "ready", []string{"review outputs"}
	if len(outputs) == 0 {
		state, next = "blocked", []string{"provide requested workflow inputs"}
	} else {
		next = []string{fmt.Sprintf("review %d composed output(s)", len(outputs))}
	}
	return ToucanComposeResponse{CapabilityResult: CapabilityResult{ContractVersion: capabilityContractVersion, RequestID: request.RequestID, Capability: string(capabilityToucan), Operation: request.Operation, Status: "composed"}, State: state, Outputs: outputs, NextActions: next}, nil
}
