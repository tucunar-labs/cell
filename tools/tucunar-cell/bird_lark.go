package canarynode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

func init() { registerCapabilityExecutor(capabilityLark, newLarkExecutor) }
func newLarkExecutor(definition CapabilityDefinition) CapabilityExecutor {
	return newFunctionalCapability(definition, larkHandoff)
}

func larkHandoff(_ context.Context, request CapabilityRequest) (any, error) {
	p, err := decodeCapabilityPayload[LarkHandoffRequest](request)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.TaskID) == "" || len(p.TaskID) > 256 || len(p.Owner) > 128 || len(p.Note) > 4096 {
		return nil, errors.New("task and bounded transition fields are required")
	}
	state, owner := "", p.Owner
	switch p.Action {
	case "assign":
		if owner == "" {
			return nil, errors.New("assign requires owner")
		}
		state = "assigned"
	case "acknowledge":
		state = "acknowledged"
	case "handoff":
		if owner == "" {
			return nil, errors.New("handoff requires owner")
		}
		state = "handed_off"
	case "complete":
		state = "completed"
	case "reopen":
		state = "reopened"
	default:
		return nil, fmt.Errorf("unsupported workflow action %q", p.Action)
	}
	h := sha256.Sum256([]byte(request.RequestID + "\x00" + p.TaskID + "\x00" + p.Action + "\x00" + owner))
	return LarkHandoffResponse{CapabilityResult: CapabilityResult{ContractVersion: capabilityContractVersion, RequestID: request.RequestID, Capability: string(capabilityLark), Operation: request.Operation, Status: "transitioned"}, State: state, Owner: owner, TransitionID: "lark-" + hex.EncodeToString(h[:8])}, nil
}
