package canarynode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// capabilityRun is the small implementation seam shared by independent bird
// processes. Each bird owns its operation logic and returns only its typed
// operation payload; this wrapper owns the stable CapabilityResponse envelope,
// request validation and the common OTel span.
type capabilityRun func(context.Context, CapabilityRequest) (any, error)

type functionalCapability struct {
	definition CapabilityDefinition
	run        capabilityRun
}

func newFunctionalCapability(definition CapabilityDefinition, run capabilityRun) CapabilityExecutor {
	return functionalCapability{definition: definition, run: run}
}

func (c functionalCapability) Definition() CapabilityDefinition { return c.definition }

func (c functionalCapability) Execute(ctx context.Context, request CapabilityRequest) (CapabilityResponse, error) {
	ctx, span := tracer.Start(ctx, "bird."+string(c.definition.ID)+"."+request.Operation,
		withAttributes(
			spanString("tucunar.capability", string(c.definition.ID)),
			spanString("tucunar.operation", request.Operation),
			spanString("tucunar.organization_id", request.OrganizationID),
			spanString("tucunar.cell_id", request.CellID),
			spanString("tucunar.node_id", request.NodeID),
			spanString("tucunar.request_id", request.RequestID),
		))
	defer span.End()

	if err := validateCapabilityRequest(request, c.definition); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "invalid capability request")
		return rejectedCapabilityResponse(request, c.definition, "invalid_request", err), err
	}

	payload, err := c.run(ctx, request)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "capability execution failed")
		return rejectedCapabilityResponse(request, c.definition, "execution_failed", err), err
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "capability payload encoding failed")
		return rejectedCapabilityResponse(request, c.definition, "payload_encoding_failed", err), err
	}
	span.SetAttributes(spanString("tucunar.status", "completed"))
	return CapabilityResponse{
		ContractVersion: capabilityContractVersion,
		RequestID:       request.RequestID,
		Capability:      c.definition.ID,
		Operation:       request.Operation,
		Status:          "completed",
		Payload:         encoded,
	}, nil
}

func rejectedCapabilityResponse(request CapabilityRequest, definition CapabilityDefinition, code string, err error) CapabilityResponse {
	return CapabilityResponse{
		ContractVersion: capabilityContractVersion,
		RequestID:       request.RequestID,
		Capability:      definition.ID,
		Operation:       request.Operation,
		Status:          "rejected",
		ErrorCode:       code,
		ErrorMessage:    err.Error(),
	}
}

func decodeCapabilityPayload[T any](request CapabilityRequest) (T, error) {
	var payload T
	if len(request.Payload) == 0 || !json.Valid(request.Payload) {
		return payload, errors.New("payload must be valid JSON")
	}
	if err := json.Unmarshal(request.Payload, &payload); err != nil {
		return payload, fmt.Errorf("decode %s payload: %w", request.Operation, err)
	}
	return payload, nil
}

// Capability registration is deliberately process-local. A bird binary only
// registers its own implementation; the map is used by the generic HTTP
// surface for local contract tests and by the independent entrypoints.
var capabilityFactories = struct {
	sync.RWMutex
	values map[CapabilityID]func(CapabilityDefinition) CapabilityExecutor
}{values: make(map[CapabilityID]func(CapabilityDefinition) CapabilityExecutor)}

func registerCapabilityExecutor(id CapabilityID, factory func(CapabilityDefinition) CapabilityExecutor) {
	capabilityFactories.Lock()
	defer capabilityFactories.Unlock()
	capabilityFactories.values[id] = factory
}

func capabilityExecutorFor(definition CapabilityDefinition) CapabilityExecutor {
	capabilityFactories.RLock()
	factory := capabilityFactories.values[definition.ID]
	capabilityFactories.RUnlock()
	if factory == nil {
		return capabilityStubFor(definition.ID)
	}
	return factory(definition)
}
