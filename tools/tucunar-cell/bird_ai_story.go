package canarynode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	defaultDeepSeekBaseURL = "https://api.deepseek.com"
	defaultDeepSeekModel   = "deepseek-v4-flash"
	maxStoryPromptBytes    = 48 << 10
	maxStoryText           = 800
)

type deepSeekChatRequest struct {
	Model          string              `json:"model"`
	Messages       []deepSeekMessage   `json:"messages"`
	Thinking       deepSeekThinking    `json:"thinking"`
	ResponseFormat deepSeekResponseFmt `json:"response_format"`
	MaxTokens      int                 `json:"max_tokens"`
	Stream         bool                `json:"stream"`
	UserID         string              `json:"user_id,omitempty"`
}

type deepSeekMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type deepSeekThinking struct {
	Type string `json:"type"`
}

type deepSeekResponseFmt struct {
	Type string `json:"type"`
}

type deepSeekChatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func runAIConsoleStory(ctx context.Context, request CapabilityRequest) (any, error) {
	payload, err := decodeCapabilityPayload[AIConsoleStoryRequest](request)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(payload.ScenarioID) == "" {
		return nil, errors.New("scenario_id is required")
	}
	if payload.Facts.ScenarioID == "" {
		payload.Facts.ScenarioID = payload.ScenarioID
	}
	if payload.Facts.ScenarioID != payload.ScenarioID {
		return nil, errors.New("scenario_id does not match facts.scenario_id")
	}
	if len(payload.Facts.Events) == 0 {
		return nil, errors.New("facts.events must not be empty")
	}

	promptFacts := storyPromptFacts(payload.Facts)
	encodedFacts, err := json.Marshal(promptFacts)
	if err != nil {
		return nil, fmt.Errorf("encode case facts: %w", err)
	}
	if len(encodedFacts) > maxStoryPromptBytes {
		return nil, fmt.Errorf("case facts exceed %d bytes", maxStoryPromptBytes)
	}

	model := strings.TrimSpace(env("DEEPSEEK_MODEL", defaultDeepSeekModel))
	if !aiReasoningEnabled() || strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")) == "" {
		story := fallbackCaseStory(payload.Facts, "DeepSeek is not configured for this process")
		return AIConsoleStoryResponse{
			CapabilityResult: completedAIResult(request, "console_story"),
			Story:            story,
		}, nil
	}
	if store := aiStoreFromContext(ctx); store != nil {
		organizationID := firstNonEmpty(request.OrganizationID, store.organizationID, "local-organization")
		cellID := firstNonEmpty(request.CellID, store.canaryID, "local-cell")
		if err := store.reserveDeepSeekCall(ctx, organizationID, cellID, payload.ScenarioID, request.RequestID, model); err != nil {
			return nil, err
		}
	}

	story, err := requestDeepSeekStory(ctx, payload.Facts, string(encodedFacts), model)
	if err != nil {
		spanFromContext(ctx).SetAttributes(spanString("tucunar.reasoning.fallback", "deterministic"))
		story = fallbackCaseStory(payload.Facts, boundedError(err))
	} else {
		story.Status = "generated"
		story.Provider = "deepseek"
		story.Model = model
		story.GeneratedAt = time.Now().UTC()
		story.FactsHash = caseFactsHash(payload.Facts)
		story.ScenarioID = payload.Facts.ScenarioID
		story.Mermaid = renderEventDiagram(payload.Facts)
	}
	return AIConsoleStoryResponse{
		CapabilityResult: completedAIResult(request, "console_story"),
		Story:            story,
	}, nil
}

func completedAIResult(request CapabilityRequest, operation string) CapabilityResult {
	return CapabilityResult{
		ContractVersion: capabilityContractVersion,
		RequestID:       request.RequestID,
		Capability:      string(capabilityAIReasoning),
		Operation:       operation,
		Status:          "completed",
	}
}

func aiReasoningEnabled() bool {
	return env("AI_REASONING_ENABLED", "1") != "0"
}

func requestDeepSeekStory(ctx context.Context, facts ConsoleCaseFacts, encodedFacts, model string) (CaseStory, error) {
	ctx, span := tracer.Start(ctx, "DeepSeek console story",
		withAttributes(
			spanString("ai.provider", "deepseek"),
			spanString("ai.model", model),
			spanString("ai.prompt_version", caseStoryPromptVersion),
			spanString("ai.scenario_id", facts.ScenarioID),
		))
	defer span.End()

	system := `You are Shiitake, the reasoning layer for an operations console.
Use only the supplied case facts. Do not invent services, causes, metrics,
people, timestamps, or actions. Return one JSON object and no markdown.
Write for the operator responsible for the affected customer operation, not
for the platform engineer. Make the asset, location, symptom, trend, impact,
current state, owner, contacts and notification response the subject of the
headline, summary and timeline. Use context fields such as asset, location,
metric, rule, impact, owner, responsibility, notification_target, channel,
acknowledgement and response_status when present. Never call internal birds,
fungi or transport processes the incident subject. Mention them only in the
flow as internal provenance. Never infer a person, owner, acknowledgement or
working state when the facts do not contain it.
The flow must contain only observed participants and observed edges from the
facts. Do not draw the complete cell or a packet mesh: choose at most 8 nodes
and 12 edges that explain the incident and its response. Keep the explanation
concise and useful to an operator who is reviewing an alert or the previous
shift. Required JSON shape:
{"headline":"string","summary":"string","severity":"info|warning|critical|recovered","what_happened":[{"title":"string","detail":"string","evidence_refs":["event id"]}],"next_actions":["string"],"evidence_refs":["event id"],"flow":{"nodes":[{"id":"observed participant","label":"string","role":"string","status":"observed|degraded|failed|recovered"}],"edges":[{"source":"observed participant","target":"observed participant","label":"observed event","status":"observed|failed|recovered"}]}}`
	user := "Case facts JSON:\n" + encodedFacts
	requestBody := deepSeekChatRequest{
		Model: model,
		Messages: []deepSeekMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Thinking:       deepSeekThinking{Type: "disabled"},
		ResponseFormat: deepSeekResponseFmt{Type: "json_object"},
		MaxTokens:      2400,
		Stream:         false,
		UserID:         boundedUserID(facts.ScenarioID),
	}
	body, err := json.Marshal(requestBody)
	if err != nil {
		return CaseStory{}, fmt.Errorf("encode DeepSeek request: %w", err)
	}
	baseURL := strings.TrimRight(env("DEEPSEEK_BASE_URL", defaultDeepSeekBaseURL), "/")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return CaseStory{}, fmt.Errorf("create DeepSeek request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")))
	client := &http.Client{Timeout: aiReasoningTimeout()}
	response, err := client.Do(request)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "DeepSeek request failed")
		return CaseStory{}, fmt.Errorf("DeepSeek request failed: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return CaseStory{}, fmt.Errorf("read DeepSeek response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		span.SetStatus(spanError, "DeepSeek returned an error")
		return CaseStory{}, fmt.Errorf("DeepSeek returned HTTP %d", response.StatusCode)
	}
	var completion deepSeekChatResponse
	if err := json.Unmarshal(responseBody, &completion); err != nil {
		return CaseStory{}, fmt.Errorf("decode DeepSeek response: %w", err)
	}
	if len(completion.Choices) == 0 || strings.TrimSpace(completion.Choices[0].Message.Content) == "" {
		return CaseStory{}, errors.New("DeepSeek returned no story")
	}
	if completion.Choices[0].FinishReason == "length" {
		return CaseStory{}, errors.New("DeepSeek story was truncated")
	}
	var story CaseStory
	if err := json.Unmarshal([]byte(completion.Choices[0].Message.Content), &story); err != nil {
		return CaseStory{}, fmt.Errorf("decode DeepSeek story JSON: %w", err)
	}
	return normalizeCaseStory(story, facts)
}

func normalizeCaseStory(story CaseStory, facts ConsoleCaseFacts) (CaseStory, error) {
	base := fallbackCaseStory(facts, "")
	if strings.TrimSpace(story.Headline) == "" || strings.TrimSpace(story.Summary) == "" {
		return CaseStory{}, errors.New("DeepSeek story is missing headline or summary")
	}
	base.Headline = boundedStoryText(story.Headline)
	base.Summary = boundedStoryText(story.Summary)
	base.Severity = normalizeStorySeverity(story.Severity, base.Severity)
	base.WhatHappened = normalizeStorySteps(story.WhatHappened, facts)
	if len(base.WhatHappened) == 0 {
		base.WhatHappened = fallbackCaseStory(facts, "").WhatHappened
	}
	base.NextActions = boundedStoryList(story.NextActions, 5)
	base.EvidenceRefs = validStoryRefs(story.EvidenceRefs, facts)
	base.Flow = normalizeStoryFlow(story.Flow, facts)
	return customerFocusStory(base, facts), nil
}

func normalizeStoryFlow(flow StoryFlow, facts ConsoleCaseFacts) StoryFlow {
	allowed := make(map[string]string, len(facts.Participants))
	for _, participant := range facts.Participants {
		allowed[strings.ToLower(participant)] = participant
	}
	nodes := make([]StoryFlowNode, 0, maxCaseStoryNodes)
	seen := make(map[string]struct{})
	for _, node := range flow.Nodes {
		canonical := allowed[strings.ToLower(strings.TrimSpace(node.ID))]
		if canonical == "" {
			continue
		}
		if _, exists := seen[canonical]; exists || len(nodes) >= maxCaseStoryNodes {
			continue
		}
		seen[canonical] = struct{}{}
		role := boundedStoryText(node.Role)
		if role == "" && strings.TrimSpace(node.Label) != "" && !strings.EqualFold(strings.TrimSpace(node.Label), canonical) {
			role = boundedStoryText(node.Label)
		}
		nodes = append(nodes, StoryFlowNode{ID: canonical, Label: humanizeStory(canonical), Role: role, Status: normalizeNodeStatus(node.Status)})
	}
	if len(nodes) == 0 {
		fallback := fallbackCaseStory(facts, "")
		return fallback.Flow
	}
	edges := make([]StoryFlowEdge, 0, maxCaseStoryEdges)
	seenEdges := make(map[string]struct{})
	for _, edge := range flow.Edges {
		source := allowed[strings.ToLower(strings.TrimSpace(edge.Source))]
		target := allowed[strings.ToLower(strings.TrimSpace(edge.Target))]
		if source == "" || target == "" || source == target {
			continue
		}
		key := source + "\x00" + target + "\x00" + edge.Label
		if _, exists := seenEdges[key]; exists || len(edges) >= maxCaseStoryEdges {
			continue
		}
		seenEdges[key] = struct{}{}
		edges = append(edges, StoryFlowEdge{Source: source, Target: target, Label: boundedStoryText(edge.Label), Status: normalizeNodeStatus(edge.Status)})
	}
	if len(edges) == 0 {
		fallback := fallbackCaseStory(facts, "")
		edges = fallback.Flow.Edges
	}
	return StoryFlow{Nodes: nodes, Edges: edges}
}

// storyPromptFacts keeps the complete ordered event ledger available to the
// model while constraining the candidate graph. This prevents a rich case
// from turning into an unreadable all-to-all topology diagram.
func storyPromptFacts(facts ConsoleCaseFacts) ConsoleCaseFacts {
	result := facts
	// Keep the complete counts and time bounds, but send a compact ordered
	// evidence digest. The operator story does not need every packet exchange
	// to explain a case, and a bounded prompt keeps the cheap model reliable.
	allEvents := facts.Events
	selectedEvents := make([]CaseFactEvent, 0, 16)
	selectedIDs := make(map[string]struct{}, 16)
	addEvent := func(event CaseFactEvent) {
		if len(selectedEvents) >= 16 || event.ID == "" {
			return
		}
		if _, exists := selectedIDs[event.ID]; exists {
			return
		}
		selectedIDs[event.ID] = struct{}{}
		selectedEvents = append(selectedEvents, event)
	}
	for index, event := range allEvents {
		important := index == 0 || index == len(allEvents)-1 || event.Severity == "critical" || event.Severity == "warning" || event.Name == "incident.opened" || strings.Contains(event.Name, "candidate") || strings.Contains(event.Name, "recover") || event.Phase == "response"
		if important {
			addEvent(event)
		}
	}
	for _, event := range allEvents {
		addEvent(event)
	}
	result.Events = selectedEvents
	participants := make([]string, 0, maxCaseStoryNodes)
	seen := make(map[string]struct{}, maxCaseStoryNodes)
	addParticipant := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || len(participants) >= 8 {
			return
		}
		if _, exists := seen[value]; exists {
			return
		}
		seen[value] = struct{}{}
		participants = append(participants, value)
	}
	for _, event := range selectedEvents {
		important := event.Name == "incident.opened" || event.Severity == "critical" || event.Severity == "warning" || strings.Contains(event.Name, "candidate") || strings.Contains(event.Name, "recover") || event.Phase == "response"
		if important {
			addParticipant(event.From)
			addParticipant(event.To)
			addParticipant(event.Source)
		}
	}
	for _, event := range selectedEvents {
		if len(participants) >= 8 {
			break
		}
		addParticipant(event.From)
		addParticipant(event.To)
		addParticipant(event.Source)
	}
	allowed := make(map[string]struct{}, len(participants))
	for _, participant := range participants {
		allowed[participant] = struct{}{}
	}
	edges := make([]CaseFactEdge, 0, 12)
	seenEdges := make(map[string]struct{}, 12)
	for _, edge := range facts.Edges {
		if len(edges) >= 12 {
			break
		}
		if _, ok := allowed[edge.Source]; !ok {
			continue
		}
		if _, ok := allowed[edge.Target]; !ok {
			continue
		}
		key := edge.Source + "\x00" + edge.Target
		if _, exists := seenEdges[key]; exists {
			continue
		}
		seenEdges[key] = struct{}{}
		edges = append(edges, edge)
	}
	result.Participants = participants
	result.Edges = edges
	return result
}

func normalizeStorySteps(steps []StoryStep, facts ConsoleCaseFacts) []StoryStep {
	refs := make(map[string]struct{}, len(facts.Events))
	for _, event := range facts.Events {
		refs[event.ID] = struct{}{}
	}
	result := make([]StoryStep, 0, 6)
	for _, step := range steps {
		if len(result) >= 6 || strings.TrimSpace(step.Title) == "" || strings.TrimSpace(step.Detail) == "" {
			continue
		}
		result = append(result, StoryStep{Title: boundedStoryText(step.Title), Detail: boundedStoryText(step.Detail), OccurredAt: step.OccurredAt, EvidenceRefs: filterStoryRefs(step.EvidenceRefs, refs)})
	}
	return result
}

func validStoryRefs(values []string, facts ConsoleCaseFacts) []string {
	refs := make(map[string]struct{}, len(facts.Events))
	for _, event := range facts.Events {
		refs[event.ID] = struct{}{}
	}
	return filterStoryRefs(values, refs)
}

func filterStoryRefs(values []string, allowed map[string]struct{}) []string {
	result := make([]string, 0, 8)
	seen := make(map[string]struct{})
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(result) >= 8 {
			continue
		}
		if _, ok := allowed[value]; !ok {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func boundedStoryList(values []string, limit int) []string {
	result := make([]string, 0, limit)
	for _, value := range values {
		if value = boundedStoryText(value); value != "" && len(result) < limit {
			result = append(result, value)
		}
	}
	return result
}

func boundedStoryText(value string) string {
	value = strings.TrimSpace(value)
	if len([]rune(value)) > maxStoryText {
		return string([]rune(value)[:maxStoryText-1]) + "…"
	}
	return value
}

func normalizeStorySeverity(value, fallback string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "info", "warning", "critical", "recovered":
		return value
	default:
		return fallback
	}
}

func normalizeNodeStatus(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "observed", "degraded", "failed", "recovered":
		return value
	default:
		return "observed"
	}
}

func aiReasoningTimeout() time.Duration {
	seconds := intFromEnvBounded("AI_REASONING_TIMEOUT_SECONDS", 30, 2, 120)
	return time.Duration(seconds) * time.Second
}

func boundedUserID(value string) string {
	var builder strings.Builder
	for _, char := range strings.ToLower(value) {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' || char == '_' {
			builder.WriteRune(char)
		}
		if builder.Len() >= 64 {
			break
		}
	}
	return builder.String()
}

func boundedError(err error) string {
	if err == nil {
		return ""
	}
	return boundedStoryText(err.Error())
}
