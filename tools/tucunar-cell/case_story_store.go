package canarynode

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

const caseStoriesTable = "canary_case_stories"

const deepSeekCallLedgerSchema = `
CREATE TABLE IF NOT EXISTS canary_ai_call_ledger (
    organization_id LowCardinality(String) DEFAULT '',
    cell_id         LowCardinality(String) DEFAULT '',
    scenario_id     String,
    request_id      String,
    provider        LowCardinality(String) DEFAULT 'deepseek',
    model           LowCardinality(String) DEFAULT '',
    reserved_at     DateTime64(3, 'UTC')
) ENGINE = MergeTree
PARTITION BY toYYYYMM(reserved_at)
ORDER BY (organization_id, cell_id, scenario_id, reserved_at, request_id)
TTL toDateTime(reserved_at) + INTERVAL 2 DAY DELETE`

const caseStorySchema = `
CREATE TABLE IF NOT EXISTS canary_case_stories (
    organization_id LowCardinality(String) DEFAULT '',
    canary_id       String DEFAULT '',
    scenario_id     String,
    facts_hash      String,
    version         UInt64,
    provider        LowCardinality(String) DEFAULT '',
    model           LowCardinality(String) DEFAULT '',
    status          LowCardinality(String),
    story_json      String,
    error           String DEFAULT '',
    generated_at    DateTime64(3, 'UTC')
) ENGINE = ReplacingMergeTree(version)
PARTITION BY toYYYYMM(generated_at)
ORDER BY (organization_id, canary_id, scenario_id)
TTL toDateTime(generated_at) + INTERVAL 30 DAY DELETE`

func (s *Store) BuildCaseStory(ctx context.Context, scenarioID string, force bool) (CaseStory, error) {
	ctx, span := tracer.Start(ctx, "Build operator case story",
		withAttributes(spanString("story.scenario_id", scenarioID), spanBool("story.force", force)))
	defer span.End()
	scenarioID = strings.TrimSpace(scenarioID)
	if scenarioID == "" {
		return CaseStory{}, fmt.Errorf("scenario_id is required")
	}
	if !force {
		if story, found, err := s.StoredCaseStory(ctx, scenarioID); err != nil {
			span.RecordError(err)
		} else if found {
			return story, nil
		}
	}
	timeline, err := s.Timeline(ctx, scenarioID, 7*24*time.Hour)
	if err != nil {
		return CaseStory{}, fmt.Errorf("read case facts: %w", err)
	}
	if len(timeline.Events) == 0 {
		return CaseStory{}, fmt.Errorf("scenario %q has no events", scenarioID)
	}
	facts := caseFactsFromTimeline(timeline)
	story, err := s.callAIConsoleStory(ctx, facts)
	if err != nil {
		span.RecordError(err)
		var rateLimitError *deepSeekRateLimitError
		if errors.As(err, &rateLimitError) {
			span.SetStatus(spanError, "DeepSeek rate limit exceeded")
			return CaseStory{}, err
		}
		span.SetStatus(spanError, "AI story request failed; using deterministic fallback")
		story = fallbackCaseStory(facts, boundedError(err))
	}
	story = customerFocusStory(story, facts)
	if err := s.saveCaseStory(ctx, story); err != nil {
		span.RecordError(err)
		return CaseStory{}, fmt.Errorf("persist case story: %w", err)
	}
	span.SetAttributes(spanString("story.status", story.Status), spanInt("story.flow_nodes", len(story.Flow.Nodes)), spanInt("story.flow_edges", len(story.Flow.Edges)))
	return story, nil
}

func (s *Store) StoredCaseStory(ctx context.Context, scenarioID string) (CaseStory, bool, error) {
	ctx, span := tracer.Start(ctx, "ClickHouse SELECT case story", withAttributes(spanString("db.system", "clickhouse"), spanString("story.scenario_id", scenarioID)))
	defer span.End()
	var storyJSON string
	query := `SELECT story_json FROM canary_case_stories FINAL
WHERE organization_id = {organization_id:String}
  AND canary_id = {canary_id:String}
  AND scenario_id = {scenario_id:String}
ORDER BY generated_at DESC LIMIT 1`
	rows, err := s.conn.Query(ctx, query,
		clickhouse.Named("organization_id", s.organizationID),
		clickhouse.Named("canary_id", s.canaryID),
		clickhouse.Named("scenario_id", scenarioID),
	)
	if err != nil {
		return CaseStory{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return CaseStory{}, false, err
		}
		return CaseStory{}, false, nil
	}
	if err := rows.Scan(&storyJSON); err != nil {
		return CaseStory{}, false, err
	}
	if decoded, decodeErr := base64.StdEncoding.DecodeString(storyJSON); decodeErr == nil {
		storyJSON = string(decoded)
	}
	var story CaseStory
	if err := json.Unmarshal([]byte(storyJSON), &story); err != nil {
		return CaseStory{}, false, fmt.Errorf("decode stored case story: %w", err)
	}
	return story, true, nil
}

func (s *Store) callAIConsoleStory(ctx context.Context, facts ConsoleCaseFacts) (CaseStory, error) {
	requestPayload, err := json.Marshal(AIConsoleStoryRequest{
		CapabilityContext: CapabilityContext{
			ContractVersion: capabilityContractVersion,
			RequestID:       newRequestID(),
			OrganizationID:  firstNonEmpty(s.organizationID, "local-organization"),
			CellID:          firstNonEmpty(env("CANARY_CELL_ID", ""), s.canaryID, "local-cell"),
			NodeID:          firstNonEmpty(env("CANARY_NODE_ID", ""), s.canaryID, "local-node"),
		},
		ScenarioID: facts.ScenarioID,
		Facts:      facts,
	})
	if err != nil {
		return CaseStory{}, fmt.Errorf("encode story capability request: %w", err)
	}
	body, err := json.Marshal(CapabilityRequest{
		ContractVersion: capabilityContractVersion,
		RequestID:       newRequestID(),
		OrganizationID:  firstNonEmpty(s.organizationID, "local-organization"),
		CellID:          firstNonEmpty(env("CANARY_CELL_ID", ""), s.canaryID, "local-cell"),
		NodeID:          firstNonEmpty(env("CANARY_NODE_ID", ""), s.canaryID, "local-node"),
		Capability:      capabilityAIReasoning,
		Operation:       "console_story",
		Payload:         requestPayload,
	})
	if err != nil {
		return CaseStory{}, fmt.Errorf("encode story envelope: %w", err)
	}
	baseURL := strings.TrimRight(env("AI_REASONING_URL", "http://shiitake:8080"), "/")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/capability/execute", bytes.NewReader(body))
	if err != nil {
		return CaseStory{}, fmt.Errorf("create story request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if token := strings.TrimSpace(env("TUCUNAR_INTERNAL_TOKEN", "")); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := (&http.Client{Timeout: aiReasoningTimeout()}).Do(request)
	if err != nil {
		return CaseStory{}, fmt.Errorf("call AI reasoning: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return CaseStory{}, fmt.Errorf("read AI reasoning response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusTooManyRequests {
			var envelope CapabilityResponse
			scope := "unknown"
			if json.Unmarshal(responseBody, &envelope) == nil {
				scope = deepSeekRateLimitScope(envelope.ErrorMessage)
			}
			return CaseStory{}, &deepSeekRateLimitError{scope: scope, limit: deepSeekRateLimitLimit(scope)}
		}
		return CaseStory{}, fmt.Errorf("AI reasoning returned HTTP %d", response.StatusCode)
	}
	var envelope CapabilityResponse
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return CaseStory{}, fmt.Errorf("decode AI reasoning envelope: %w", err)
	}
	if envelope.Status != "completed" || len(envelope.Payload) == 0 {
		return CaseStory{}, fmt.Errorf("AI reasoning returned status %q", envelope.Status)
	}
	var payload AIConsoleStoryResponse
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return CaseStory{}, fmt.Errorf("decode AI story payload: %w", err)
	}
	return payload.Story, nil
}

func (s *Store) saveCaseStory(ctx context.Context, story CaseStory) error {
	encoded, err := json.Marshal(story)
	if err != nil {
		return err
	}
	// ClickHouse's text parameter binding interprets escaped newlines in a
	// String value. Base64 keeps the stored JSON byte-for-byte valid, including
	// the Mermaid source, while the API still returns normal JSON.
	encodedForStorage := base64.StdEncoding.EncodeToString(encoded)
	return s.conn.Exec(ctx, `INSERT INTO canary_case_stories
(organization_id, canary_id, scenario_id, facts_hash, version, provider, model, status, story_json, error, generated_at)
VALUES ({organization_id:String}, {canary_id:String}, {scenario_id:String}, {facts_hash:String}, {version:UInt64}, {provider:String}, {model:String}, {status:String}, {story_json:String}, {error:String}, {generated_at:DateTime64(3)})`,
		clickhouse.Named("organization_id", s.organizationID),
		clickhouse.Named("canary_id", s.canaryID),
		clickhouse.Named("scenario_id", story.ScenarioID),
		clickhouse.Named("facts_hash", story.FactsHash),
		clickhouse.Named("version", uint64(time.Now().UnixNano())),
		clickhouse.Named("provider", story.Provider),
		clickhouse.Named("model", story.Model),
		clickhouse.Named("status", story.Status),
		clickhouse.Named("story_json", encodedForStorage),
		clickhouse.Named("error", story.Error),
		clickhouse.Named("generated_at", story.GeneratedAt),
	)
}
