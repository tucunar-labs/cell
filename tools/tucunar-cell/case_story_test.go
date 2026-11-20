package canarynode

import (
	"strings"
	"testing"
	"time"
)

func TestCaseFactsBuildsOrderedBoundedFlow(t *testing.T) {
	timeline := timelineResponse{ScenarioID: "case-1", EventCount: 3, Events: []TimelineEvent{
		{ID: "a", TenantID: "tenant", SiteID: "site", OccurredAt: mustStoryTime(t, "2026-09-05T00:00:00Z"), Source: "swift", From: "mycelium", To: "swift", Name: "ingest.accepted", Severity: "info"},
		{ID: "b", TenantID: "tenant", SiteID: "site", OccurredAt: mustStoryTime(t, "2026-09-05T00:00:01Z"), Source: "canary", From: "swift", To: "canary", Name: "incident.opened", Status: "open", Severity: "critical"},
		{ID: "c", TenantID: "tenant", SiteID: "site", OccurredAt: mustStoryTime(t, "2026-09-05T00:00:02Z"), Source: "condor", From: "canary", To: "condor", Name: "incident.persisted", Status: "open", Severity: "critical"},
	}}
	facts := caseFactsFromTimeline(timeline)
	if !facts.OpenIncident || facts.Recovered || len(facts.Events) != 3 {
		t.Fatalf("unexpected facts: %#v", facts)
	}
	if len(facts.Edges) != 3 || facts.Edges[0].Source != "canary" {
		t.Fatalf("unexpected edges: %#v", facts.Edges)
	}
	story := fallbackCaseStory(facts, "")
	if !strings.Contains(story.Mermaid, "sequenceDiagram") || !strings.Contains(story.Mermaid, "Incident Opened") || !strings.Contains(story.Mermaid, "asset->>monitor") || strings.Contains(story.Mermaid, "node1[") {
		t.Fatalf("event diagram does not describe the incident sequence: %s", story.Mermaid)
	}
}

func TestNormalizeStoryFlowRejectsUnobservedNodes(t *testing.T) {
	facts := ConsoleCaseFacts{ScenarioID: "case-2", Events: []CaseFactEvent{{ID: "a"}}, Participants: []string{"swift", "condor"}}
	flow := normalizeStoryFlow(StoryFlow{
		Nodes: []StoryFlowNode{{ID: "swift", Label: "Swift"}, {ID: "evil\"]--x", Label: "Injected"}},
		Edges: []StoryFlowEdge{{Source: "swift", Target: "evil\"]--x", Label: "bad"}},
	}, facts)
	if len(flow.Nodes) != 1 || flow.Nodes[0].ID != "swift" {
		t.Fatalf("unobserved node survived: %#v", flow.Nodes)
	}
	if len(flow.Edges) != 0 {
		t.Fatalf("unobserved edge survived: %#v", flow.Edges)
	}
}

func TestCustomerFocusStoryUsesResponsibilityAndDeliveryFacts(t *testing.T) {
	timeline := timelineResponse{ScenarioID: "mine-1", EventCount: 7, Events: []TimelineEvent{
		{ID: "symptom-1", TenantID: "mine", SiteID: "north-plant", OccurredAt: mustStoryTime(t, "2026-09-05T02:11:00Z"), Source: "swift", Name: "timeout.surge", Severity: "critical", Value: 42, Unit: "seconds", Context: map[string]string{"asset": "Crusher 02", "location": "North Plant", "metric": "telemetry timeout rate", "impact": "crusher telemetry unavailable", "error_rate": "10"}},
		{ID: "symptom-2", TenantID: "mine", SiteID: "north-plant", OccurredAt: mustStoryTime(t, "2026-09-05T02:12:00Z"), Source: "swift", Name: "timeout.surge", Severity: "critical", Value: 54, Unit: "seconds", Context: map[string]string{"asset": "Crusher 02", "location": "North Plant", "metric": "telemetry timeout rate", "error_rate": "12"}},
		{ID: "incident", TenantID: "mine", SiteID: "north-plant", OccurredAt: mustStoryTime(t, "2026-09-05T02:13:00Z"), Name: "incident.opened", Status: "open", Severity: "critical"},
		{ID: "owner", TenantID: "mine", SiteID: "north-plant", OccurredAt: mustStoryTime(t, "2026-09-05T02:14:00Z"), Name: "handoff.created", Context: map[string]string{"owner": "North Plant maintenance team", "responsibility": "Crusher 02 gateway and telemetry recovery"}},
		{ID: "notify", TenantID: "mine", SiteID: "north-plant", OccurredAt: mustStoryTime(t, "2026-09-05T02:15:00Z"), Name: "notification.failed", Context: map[string]string{"notification_target": "Control room + shift supervisor", "channel": "Microsoft Teams", "acknowledgement": "none"}},
	}}
	facts := caseFactsFromTimeline(timeline)
	story := fallbackCaseStory(facts, "")
	if !strings.Contains(story.Summary, "Crusher 02") || !strings.Contains(story.Summary, "North Plant maintenance team") || !strings.Contains(story.Summary, "no acknowledgement") {
		t.Fatalf("customer facts missing from summary: %s", story.Summary)
	}
	if strings.Contains(strings.ToLower(story.Summary), "swift") || strings.Contains(strings.ToLower(story.Summary), "condor") {
		t.Fatalf("internal birds leaked into customer summary: %s", story.Summary)
	}
}

func mustStoryTime(t *testing.T, value string) (resultTime time.Time) {
	t.Helper()
	resultTime, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return resultTime
}
