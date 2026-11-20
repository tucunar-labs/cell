package canarynode

import (
	"context"
	"fmt"
	"strings"
)

func init() {
	registerCapabilityExecutor(capabilityWoodpecker, func(d CapabilityDefinition) CapabilityExecutor { return newFunctionalCapability(d, runWoodpecker) })
}

func runWoodpecker(ctx context.Context, request CapabilityRequest) (any, error) {
	p, err := decodeCapabilityPayload[WoodpeckerValidateRequest](request)
	if err != nil {
		return nil, err
	}
	result := WoodpeckerValidateResponse{CapabilityResult: CapabilityResult{ContractVersion: capabilityContractVersion, RequestID: request.RequestID, Capability: string(capabilityWoodpecker), Operation: "validate", Status: "completed"}, Valid: true, RecordsChecked: uint64(len(p.Records))}
	schema := strings.TrimSpace(p.SchemaRef)
	if !strings.HasPrefix(schema, "signal.v1") {
		result.Valid = false
		result.Violations = append(result.Violations, Violation{Path: "schema_ref", Message: "schema_ref must be signal.v1 or signal.v1;required:<field,...>", Severity: "error"})
		return result, nil
	}
	if i := strings.Index(schema, "required:"); i >= 0 {
		for _, field := range strings.Split(schema[i+len("required:"):], ",") {
			field = strings.TrimSpace(field)
			if field == "" {
				continue
			}
			for n, record := range p.Records {
				if _, ok := record[field]; !ok || record[field] == nil {
					result.Valid = false
					result.Violations = append(result.Violations, Violation{Path: fmt.Sprintf("records[%d].%s", n, field), Message: "required field is missing", Severity: "error"})
				}
			}
		}
	}
	return result, nil
}
