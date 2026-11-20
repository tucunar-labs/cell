package canarynode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	maxAIReasoningTask         = 2048
	maxAIReasoningAnswer       = 1200
	maxAIReasoningContextBytes = 32 << 10
	maxAIReasoningContextKeys  = 64
	maxAIReasoningCitations    = 8
)

func init() {
	registerCapabilityExecutor(capabilityAIReasoning, func(definition CapabilityDefinition) CapabilityExecutor {
		return newFunctionalCapability(definition, runAIReasoning)
	})
}

func runAIReasoning(ctx context.Context, request CapabilityRequest) (any, error) {
	if request.Operation == "console_story" {
		return runAIConsoleStory(ctx, request)
	}
	payload, err := decodeCapabilityPayload[AIReasoningRequest](request)
	if err != nil {
		return nil, err
	}
	task := strings.TrimSpace(payload.Task)
	if task == "" {
		return nil, errors.New("task is required")
	}
	if len([]rune(task)) > maxAIReasoningTask {
		return nil, fmt.Errorf("task exceeds %d characters", maxAIReasoningTask)
	}
	if payload.Context == nil {
		return nil, errors.New("context is required")
	}
	if len(payload.Context) > maxAIReasoningContextKeys {
		return nil, fmt.Errorf("context exceeds %d fields", maxAIReasoningContextKeys)
	}
	encoded, err := json.Marshal(payload.Context)
	if err != nil {
		return nil, fmt.Errorf("context must be JSON-compatible: %w", err)
	}
	if len(encoded) > maxAIReasoningContextBytes {
		return nil, fmt.Errorf("context exceeds %d bytes", maxAIReasoningContextBytes)
	}
	if payload.ModelPolicy == nil {
		return nil, errors.New("model_policy is required")
	}
	enabled, provider := reasoningPolicy(payload.ModelPolicy)
	spanFromContext(ctx).SetAttributes(spanBool("tucunar.reasoning.policy_enabled", enabled), spanString("tucunar.reasoning.provider", provider))
	if !enabled {
		return nil, errors.New("reasoning disabled by model_policy")
	}
	citations := explicitContextReferences(payload.Context)
	keys := sortedContextKeys(payload.Context)
	return AIReasoningResponse{CapabilityResult: CapabilityResult{ContractVersion: capabilityContractVersion, RequestID: request.RequestID, Capability: string(capabilityAIReasoning), Operation: request.Operation, Status: "completed"}, Answer: boundedReasoningAnswer(task, keys, citations), Citations: citations, Confidence: reasoningConfidence(len(keys), len(citations)), ActionRequired: actionRequired(task, payload.Context)}, nil
}

func reasoningPolicy(policy map[string]any) (bool, string) {
	provider := "baseline"
	if value, ok := policy["provider"].(string); ok && strings.TrimSpace(value) != "" {
		provider = boundedProvider(value)
	}
	for _, key := range []string{"enabled", "reasoning_enabled", "allow_reasoning"} {
		if value, ok := policy[key].(bool); ok && !value {
			return false, provider
		}
	}
	return true, provider
}

func boundedProvider(value string) string {
	var result []rune
	for _, r := range strings.TrimSpace(value) {
		if len(result) == 64 {
			break
		}
		if r == '.' || r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			result = append(result, r)
		}
	}
	if len(result) == 0 {
		return "baseline"
	}
	return string(result)
}

func sortedContextKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > 12 {
		keys = keys[:12]
	}
	return keys
}

func explicitContextReferences(values map[string]any) []string {
	refs := make([]string, 0)
	for key, value := range values {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "reference", "references", "ref", "refs", "reference_id", "reference_ids", "evidence_ref", "evidence_refs", "citation", "citations":
			collectReferenceValues(value, &refs)
		}
	}
	sort.Strings(refs)
	if len(refs) > maxAIReasoningCitations {
		refs = refs[:maxAIReasoningCitations]
	}
	return refs
}

func collectReferenceValues(value any, refs *[]string) {
	switch value := value.(type) {
	case string:
		if value = strings.TrimSpace(value); value != "" && len([]rune(value)) <= 256 {
			*refs = append(*refs, value)
		}
	case []any:
		for _, item := range value {
			collectReferenceValues(item, refs)
		}
	case []string:
		for _, item := range value {
			collectReferenceValues(item, refs)
		}
	}
}

func boundedReasoningAnswer(task string, keys, citations []string) string {
	category := "analysis"
	for _, candidate := range []struct{ marker, label string }{{"summar", "summary"}, {"decid", "decision"}, {"recommend", "recommendation"}, {"explain", "explanation"}, {"plan", "planning"}} {
		if strings.Contains(strings.ToLower(task), candidate.marker) {
			category = candidate.label
			break
		}
	}
	answer := fmt.Sprintf("Baseline %s reasoning reviewed %d context field(s) (%s). No external model was called.", category, len(keys), strings.Join(keys, ", "))
	if len(citations) > 0 {
		answer += fmt.Sprintf(" Explicit references considered: %s.", strings.Join(citations, ", "))
	} else {
		answer += " No explicit references were provided."
	}
	return truncateReasoning(answer, maxAIReasoningAnswer)
}

func reasoningConfidence(keyCount, citationCount int) float64 {
	confidence := 0.45
	if keyCount > 0 {
		confidence += 0.1
	}
	confidence += float64(citationCount) * 0.05
	if confidence > 1 {
		return 1
	}
	return confidence
}

func actionRequired(task string, values map[string]any) bool {
	if value, ok := values["action_required"].(bool); ok && value {
		return true
	}
	lower := strings.ToLower(task)
	for _, marker := range []string{"please deploy", "please run", "please fix", "please update", "please create", "please delete", "please restart", "execute ", "deploy ", "remediate ", "restart "} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func truncateReasoning(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}
