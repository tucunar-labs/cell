package canarynode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	caseStoryPromptVersion = "console-story-v2-customer"
	maxCaseStoryEvents     = 64
	maxCaseStoryNodes      = 24
	maxCaseStoryEdges      = 48
	maxEventDiagramEvents  = 6
)

// ConsoleCaseFacts is the bounded, ordered read model sent to Shiitake. It is
// intentionally independent of ClickHouse rows so a customer-owned reasoning
// service can implement the same contract over the network.
type ConsoleCaseFacts struct {
	ScenarioID     string          `json:"scenario_id"`
	TenantID       string          `json:"tenant_id,omitempty"`
	SiteID         string          `json:"site_id,omitempty"`
	FirstEventAt   time.Time       `json:"first_event_at,omitempty"`
	LastEventAt    time.Time       `json:"last_event_at,omitempty"`
	EventCount     int             `json:"event_count"`
	OpenIncident   bool            `json:"open_incident"`
	Recovered      bool            `json:"recovered"`
	Participants   []string        `json:"participants,omitempty"`
	SignalCounts   map[string]int  `json:"signal_counts,omitempty"`
	SeverityCounts map[string]int  `json:"severity_counts,omitempty"`
	Events         []CaseFactEvent `json:"events"`
	Edges          []CaseFactEdge  `json:"edges,omitempty"`
}

type CaseFactEvent struct {
	ID         string            `json:"id"`
	OccurredAt time.Time         `json:"occurred_at"`
	Source     string            `json:"source"`
	From       string            `json:"from,omitempty"`
	To         string            `json:"to,omitempty"`
	EntityID   string            `json:"entity_id,omitempty"`
	Name       string            `json:"name"`
	Phase      string            `json:"phase,omitempty"`
	Status     string            `json:"status,omitempty"`
	Severity   string            `json:"severity,omitempty"`
	Message    string            `json:"message,omitempty"`
	Value      float64           `json:"value,omitempty"`
	Unit       string            `json:"unit,omitempty"`
	Context    map[string]string `json:"context,omitempty"`
}

type CaseFactEdge struct {
	Source   string   `json:"source"`
	Target   string   `json:"target"`
	Label    string   `json:"label,omitempty"`
	Status   string   `json:"status,omitempty"`
	EventIDs []string `json:"event_ids,omitempty"`
}

type CaseStory struct {
	ScenarioID   string      `json:"scenario_id"`
	Status       string      `json:"status"`
	Model        string      `json:"model,omitempty"`
	Provider     string      `json:"provider,omitempty"`
	GeneratedAt  time.Time   `json:"generated_at"`
	FactsHash    string      `json:"facts_hash,omitempty"`
	Headline     string      `json:"headline"`
	Summary      string      `json:"summary"`
	Severity     string      `json:"severity"`
	WhatHappened []StoryStep `json:"what_happened"`
	NextActions  []string    `json:"next_actions,omitempty"`
	EvidenceRefs []string    `json:"evidence_refs,omitempty"`
	Flow         StoryFlow   `json:"flow"`
	Mermaid      string      `json:"mermaid"`
	Error        string      `json:"error,omitempty"`
}

type StoryStep struct {
	Title        string    `json:"title"`
	Detail       string    `json:"detail"`
	OccurredAt   time.Time `json:"occurred_at,omitempty"`
	EvidenceRefs []string  `json:"evidence_refs,omitempty"`
}

type StoryFlow struct {
	Nodes []StoryFlowNode `json:"nodes"`
	Edges []StoryFlowEdge `json:"edges"`
}

type StoryFlowNode struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Role   string `json:"role,omitempty"`
	Status string `json:"status,omitempty"`
}

type StoryFlowEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Label  string `json:"label,omitempty"`
	Status string `json:"status,omitempty"`
}

func caseFactsFromTimeline(timeline timelineResponse) ConsoleCaseFacts {
	facts := ConsoleCaseFacts{
		ScenarioID:     timeline.ScenarioID,
		EventCount:     timeline.EventCount,
		Events:         make([]CaseFactEvent, 0, minInt(len(timeline.Events), maxCaseStoryEvents)),
		Participants:   make([]string, 0),
		SignalCounts:   make(map[string]int),
		SeverityCounts: make(map[string]int),
	}
	participants := make(map[string]struct{})
	edges := make(map[string]*CaseFactEdge)
	for index, event := range timeline.Events {
		if index >= maxCaseStoryEvents {
			break
		}
		if facts.TenantID == "" {
			facts.TenantID = event.TenantID
		}
		if facts.SiteID == "" {
			facts.SiteID = event.SiteID
		}
		facts.Events = append(facts.Events, CaseFactEvent{
			ID: event.ID, OccurredAt: event.OccurredAt, Source: event.Source,
			From: event.From, To: event.To, EntityID: event.EntityID,
			Name: event.Name, Phase: event.Phase, Status: event.Status,
			Severity: event.Severity, Message: event.Message,
			Value: event.Value, Unit: event.Unit,
			Context: event.Context,
		})
		facts.SignalCounts[event.Name]++
		severity := strings.TrimSpace(event.Severity)
		if severity != "" {
			facts.SeverityCounts[severity]++
		}
		for _, participant := range []string{event.Source, event.From, event.To} {
			if participant = strings.TrimSpace(participant); participant != "" {
				participants[participant] = struct{}{}
			}
		}
		if event.From != "" && event.To != "" {
			key := event.From + "\x00" + event.To
			if edges[key] == nil {
				edges[key] = &CaseFactEdge{Source: event.From, Target: event.To, Label: event.Name, Status: event.Status}
			}
			edges[key].EventIDs = append(edges[key].EventIDs, event.ID)
		}
	}
	for participant := range participants {
		facts.Participants = append(facts.Participants, participant)
	}
	sort.Strings(facts.Participants)
	for _, edge := range edges {
		facts.Edges = append(facts.Edges, *edge)
	}
	sort.SliceStable(facts.Edges, func(i, j int) bool {
		if facts.Edges[i].Source != facts.Edges[j].Source {
			return facts.Edges[i].Source < facts.Edges[j].Source
		}
		return facts.Edges[i].Target < facts.Edges[j].Target
	})
	if len(facts.Events) > 0 {
		facts.FirstEventAt = facts.Events[0].OccurredAt
		facts.LastEventAt = facts.Events[len(facts.Events)-1].OccurredAt
		facts.Recovered = strings.EqualFold(facts.Events[len(facts.Events)-1].Status, "recovered") || hasStoryRecovery(facts.Events)
		facts.OpenIncident = hasStoryOpenIncident(facts.Events) && !facts.Recovered
	}
	return facts
}

func hasStoryOpenIncident(events []CaseFactEvent) bool {
	for _, event := range events {
		if event.Name == "incident.opened" || strings.EqualFold(event.Status, "open") {
			return true
		}
	}
	return false
}

func hasStoryRecovery(events []CaseFactEvent) bool {
	for _, event := range events {
		if strings.Contains(strings.ToLower(event.Name), "recover") || strings.EqualFold(event.Phase, "recovery") || strings.EqualFold(event.Status, "recovered") {
			return true
		}
	}
	return false
}

func caseFactsHash(facts ConsoleCaseFacts) string {
	encoded, _ := json.Marshal(facts)
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

func fallbackCaseStory(facts ConsoleCaseFacts, reason string) CaseStory {
	severity := "info"
	if facts.OpenIncident {
		severity = "critical"
	} else if facts.Recovered {
		severity = "recovered"
	}
	headline := "Operational case reconstructed"
	if facts.OpenIncident {
		headline = "An operational incident is open"
	} else if facts.Recovered {
		headline = "The incident recovered"
	}
	steps := make([]StoryStep, 0, 6)
	refs := make([]string, 0, 8)
	for index, event := range facts.Events {
		important := index == 0 || index == len(facts.Events)-1 || event.Severity == "critical" || event.Name == "incident.opened" || strings.Contains(event.Name, "candidate") || strings.Contains(event.Name, "recovered")
		if !important || len(steps) >= 6 {
			continue
		}
		title := humanizeStory(event.Name)
		detail := event.Message
		if detail == "" {
			detail = fmt.Sprintf("%s reported %s", humanizeStory(event.Source), title)
		}
		steps = append(steps, StoryStep{Title: title, Detail: detail, OccurredAt: event.OccurredAt, EvidenceRefs: []string{event.ID}})
		refs = append(refs, event.ID)
	}
	nodes := make([]StoryFlowNode, 0, len(facts.Participants))
	for _, participant := range facts.Participants {
		nodes = append(nodes, StoryFlowNode{ID: participant, Label: humanizeStory(participant), Status: "observed"})
	}
	edges := make([]StoryFlowEdge, 0, len(facts.Edges))
	for _, edge := range facts.Edges {
		edges = append(edges, StoryFlowEdge{Source: edge.Source, Target: edge.Target, Label: edge.Label, Status: edge.Status})
	}
	story := CaseStory{
		ScenarioID: facts.ScenarioID, Status: "fallback", Provider: "deterministic",
		GeneratedAt: time.Now().UTC(), FactsHash: caseFactsHash(facts),
		Headline: headline,
		Summary:  fmt.Sprintf("%d ordered signals were read from the durable ledger for %s.", facts.EventCount, humanizeStory(firstNonEmpty(facts.SiteID, facts.TenantID, "this case"))),
		Severity: severity, WhatHappened: steps, NextActions: fallbackNextActions(facts), EvidenceRefs: refs,
		Flow: StoryFlow{Nodes: nodes, Edges: edges}, Error: reason,
	}
	story.Mermaid = renderEventDiagram(facts)
	return customerFocusStory(story, facts)
}

// customerFocusStory is the deterministic presentation layer for a case. The
// durable facts still include the internal capability path, but the operator
// story is written from the asset, symptom, impact and responsibility facts
// supplied by those capabilities. This keeps a customer-owned console or AI
// implementation interchangeable without making birds the subject of the UI.
func customerFocusStory(story CaseStory, facts ConsoleCaseFacts) CaseStory {
	asset := firstNonEmpty(storyContextValue(facts, "asset"), facts.SiteID, facts.TenantID, "operational system")
	location := storyContextValue(facts, "location")
	metric := firstNonEmpty(storyContextValue(facts, "metric"), "operating condition")
	owner := firstNonEmpty(storyContextValue(facts, "owner"), storyContextValue(facts, "responsibility"), storyContextValue(facts, "team"))
	impact := firstNonEmpty(storyContextValue(facts, "impact"), storyContextValue(facts, "exposure"))
	symptoms := customerSymptomEvents(facts.Events)
	firstSymptom, lastSymptom := firstCaseEvent(symptoms), lastCaseEvent(symptoms)
	incident := firstCaseEventByName(facts.Events, "incident.opened")
	recovery := lastCaseEventMatching(facts.Events, func(event CaseFactEvent) bool {
		return strings.Contains(strings.ToLower(event.Name), "recover") || strings.EqualFold(event.Phase, "recovery")
	})

	severity := "info"
	if facts.OpenIncident {
		severity = "critical"
	} else if facts.Recovered {
		severity = "recovered"
	}
	stateText := "The case is being monitored."
	if facts.OpenIncident {
		stateText = "The incident remains open."
	} else if facts.Recovered {
		stateText = "The incident recovered and the recovery transition was recorded."
	}

	stateNoun := "operating case"
	if facts.OpenIncident {
		stateNoun = "incident remains open"
	} else if facts.Recovered {
		stateNoun = "incident recovered"
	}
	story.Headline = fmt.Sprintf("%s: %s", asset, stateNoun)
	story.Severity = severity
	if story.ScenarioID == "" {
		story.ScenarioID = facts.ScenarioID
	}
	if story.FactsHash == "" {
		story.FactsHash = caseFactsHash(facts)
	}
	if story.GeneratedAt.IsZero() {
		story.GeneratedAt = time.Now().UTC()
	}

	var summaryParts []string
	if !firstSymptom.OccurredAt.IsZero() {
		start := fmt.Sprintf("At %s, %s", storyTimestamp(firstSymptom.OccurredAt), asset)
		if location != "" {
			start += " at " + location
		}
		start += " began showing " + metric + "."
		summaryParts = append(summaryParts, start)
	} else {
		summaryParts = append(summaryParts, fmt.Sprintf("The case contains operational evidence for %s.", asset))
	}
	if trend := storyTrend(firstSymptom, lastSymptom, metric); trend != "" {
		summaryParts = append(summaryParts, trend)
	}
	if impact != "" {
		summaryParts = append(summaryParts, "Impact: "+impact+".")
	}
	if errorRate := firstNonEmpty(storyContextValue(facts, "error_rate"), ""); errorRate != "" {
		summaryParts = append(summaryParts, "The latest evidence records "+errorRate+" errors per minute.")
	}
	summaryParts = append(summaryParts, stateText)
	if notification := customerNotificationSummary(facts.Events); notification != "" {
		summaryParts = append(summaryParts, notification)
	}
	if owner != "" && facts.OpenIncident {
		summaryParts = append(summaryParts, "Current responsibility: "+owner+".")
	}
	story.Summary = strings.Join(summaryParts, " ")

	steps, refs := customerStorySteps(facts, asset, location, metric, owner, firstSymptom, lastSymptom, incident, recovery)
	if len(steps) > 0 {
		story.WhatHappened = steps
		story.EvidenceRefs = refs
	}
	if facts.OpenIncident {
		story.NextActions = customerOpenActions(owner, impact)
	} else if facts.Recovered {
		story.NextActions = []string{"Confirm the recovery remains stable", "Review the evidence and assigned owner for follow-up", "Keep the case available for post-incident review"}
	}
	if len(story.Flow.Nodes) == 0 {
		fallback := flowFromFacts(facts)
		story.Flow = fallback
	}
	// The console diagram is deliberately derived from ordered evidence rather
	// than the model's participant graph. A dependency graph answers "what is
	// connected?"; an incident console needs "what happened, and then what?".
	story.Mermaid = renderEventDiagram(facts)
	return story
}

func flowFromFacts(facts ConsoleCaseFacts) StoryFlow {
	nodes := make([]StoryFlowNode, 0, len(facts.Participants))
	for _, participant := range facts.Participants {
		nodes = append(nodes, StoryFlowNode{ID: participant, Label: humanizeStory(participant), Status: "observed"})
	}
	edges := make([]StoryFlowEdge, 0, len(facts.Edges))
	for _, edge := range facts.Edges {
		edges = append(edges, StoryFlowEdge{Source: edge.Source, Target: edge.Target, Label: edge.Label, Status: edge.Status})
	}
	return StoryFlow{Nodes: nodes, Edges: edges}
}

func storyContextValue(facts ConsoleCaseFacts, key string) string {
	for _, event := range facts.Events {
		if value := strings.TrimSpace(event.Context[key]); value != "" {
			return value
		}
	}
	return ""
}

func customerSymptomEvents(events []CaseFactEvent) []CaseFactEvent {
	primary := make([]CaseFactEvent, 0, len(events))
	secondary := make([]CaseFactEvent, 0, len(events))
	for _, event := range events {
		name := strings.ToLower(event.Name)
		if strings.Contains(name, "timeout.surge") {
			primary = append(primary, event)
			continue
		}
		if strings.Contains(name, "health.degraded") || strings.Contains(name, "derived.candidate") {
			secondary = append(secondary, event)
		}
	}
	if len(primary) > 0 {
		return primary
	}
	return secondary
}

func firstCaseEvent(events []CaseFactEvent) CaseFactEvent {
	if len(events) == 0 {
		return CaseFactEvent{}
	}
	return events[0]
}

func lastCaseEvent(events []CaseFactEvent) CaseFactEvent {
	if len(events) == 0 {
		return CaseFactEvent{}
	}
	return events[len(events)-1]
}

func firstCaseEventByName(events []CaseFactEvent, name string) CaseFactEvent {
	for _, event := range events {
		if event.Name == name {
			return event
		}
	}
	return CaseFactEvent{}
}

func lastCaseEventMatching(events []CaseFactEvent, predicate func(CaseFactEvent) bool) CaseFactEvent {
	for index := len(events) - 1; index >= 0; index-- {
		if predicate(events[index]) {
			return events[index]
		}
	}
	return CaseFactEvent{}
}

func storyTrend(first, last CaseFactEvent, metric string) string {
	if first.OccurredAt.IsZero() || last.OccurredAt.IsZero() || first.ID == last.ID || first.Unit == "packet" || last.Unit == "packet" {
		return ""
	}
	duration := last.OccurredAt.Sub(first.OccurredAt)
	if duration < 0 {
		duration = 0
	}
	return fmt.Sprintf("%s rose from %s to %s over %s.", metric, storyMetric(first), storyMetric(last), storyDuration(duration))
}

func storyMetric(event CaseFactEvent) string {
	switch strings.ToLower(strings.TrimSpace(event.Unit)) {
	case "seconds", "second", "s":
		return fmt.Sprintf("%.0f seconds", event.Value)
	case "milliseconds", "millisecond", "ms":
		return fmt.Sprintf("%.0f ms", event.Value)
	case "percent", "%":
		return fmt.Sprintf("%.2f%%", event.Value)
	case "confidence":
		return fmt.Sprintf("%.0f%% confidence", event.Value*100)
	default:
		return fmt.Sprintf("%.2f %s", event.Value, event.Unit)
	}
}

func storyDuration(duration time.Duration) string {
	if duration < time.Minute {
		return fmt.Sprintf("%.0f seconds", duration.Seconds())
	}
	return fmt.Sprintf("%.1f minutes", duration.Minutes())
}

func storyTimestamp(value time.Time) string {
	return value.UTC().Format("2006-01-02 15:04:05 UTC")
}

func customerNotificationSummary(events []CaseFactEvent) string {
	var noResponse, working []string
	for _, event := range events {
		name := strings.ToLower(event.Name)
		if !strings.Contains(name, "notification") && !strings.Contains(name, "delivery") {
			continue
		}
		context := event.Context
		target := firstNonEmpty(context["notification_target"], context["contact"])
		channel := context["channel"]
		if target == "" && channel == "" {
			continue
		}
		label := target
		if channel != "" {
			label = firstNonEmpty(label, "recipient") + " via " + channel
		}
		ack := strings.ToLower(firstNonEmpty(context["acknowledgement"], context["response_status"], ""))
		if strings.Contains(name, "failed") || ack == "none" || strings.Contains(ack, "no") || ack == "pending" {
			noResponse = appendUniqueString(noResponse, label)
		}
		if strings.Contains(name, "delivered") || strings.Contains(ack, "working") || strings.Contains(ack, "acknowledged") {
			working = appendUniqueString(working, label)
		}
	}
	var parts []string
	if len(noResponse) > 0 {
		parts = append(parts, "A notification was sent to "+strings.Join(noResponse, ", ")+", but no acknowledgement was recorded.")
	}
	if len(working) > 0 {
		parts = append(parts, "The alert was escalated to "+strings.Join(working, ", ")+", and the recipient acknowledged it and is working on it now.")
	}
	return strings.Join(parts, " ")
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func customerStorySteps(facts ConsoleCaseFacts, asset, location, metric, owner string, firstSymptom, lastSymptom, incident, recovery CaseFactEvent) ([]StoryStep, []string) {
	steps := make([]StoryStep, 0, 6)
	refs := make([]string, 0, 8)
	add := func(title, detail string, event CaseFactEvent) {
		if strings.TrimSpace(detail) == "" || len(steps) >= 6 {
			return
		}
		ref := event.ID
		if ref != "" {
			refs = appendUniqueString(refs, ref)
		}
		steps = append(steps, StoryStep{Title: title, Detail: detail, OccurredAt: event.OccurredAt, EvidenceRefs: []string{ref}})
	}
	if !firstSymptom.OccurredAt.IsZero() {
		detail := fmt.Sprintf("At %s, %s", storyTimestamp(firstSymptom.OccurredAt), asset)
		if location != "" {
			detail += " at " + location
		}
		detail += " began showing " + metric + "."
		add("Operational symptom detected", detail, firstSymptom)
	}
	if trend := storyTrend(firstSymptom, lastSymptom, metric); trend != "" {
		add("Trend crossed the operating limit", trend, lastSymptom)
	}
	if !incident.OccurredAt.IsZero() {
		detail := "The incident was opened after the condition crossed its rule."
		if owner != "" {
			detail += " Responsibility was assigned to " + owner + "."
		}
		add("Incident opened", detail, incident)
	}
	if notification := customerNotificationSummary(facts.Events); notification != "" {
		add("Notification and ownership", notification, firstCaseEventMatching(facts.Events, func(event CaseFactEvent) bool {
			name := strings.ToLower(event.Name)
			return strings.Contains(name, "notification") || strings.Contains(name, "delivery")
		}))
	}
	if !recovery.OccurredAt.IsZero() {
		add("Recovery recorded", fmt.Sprintf("At %s, the recovery transition was recorded for %s.", storyTimestamp(recovery.OccurredAt), asset), recovery)
	}
	return steps, refs
}

func firstCaseEventMatching(events []CaseFactEvent, predicate func(CaseFactEvent) bool) CaseFactEvent {
	for _, event := range events {
		if predicate(event) {
			return event
		}
	}
	return CaseFactEvent{}
}

func customerOpenActions(owner, impact string) []string {
	owner = strings.TrimSpace(owner)
	actions := make([]string, 0, 3)
	if owner != "" {
		actions = append(actions, "Confirm that "+owner+" has accepted the response")
	} else {
		actions = append(actions, "Assign an operational owner")
	}
	if impact != "" {
		actions = append(actions, "Verify the affected operation and protect the next production window")
	} else {
		actions = append(actions, "Confirm impact and the next response action")
	}
	actions = append(actions, "Close only after the signal returns to baseline and the owner confirms recovery")
	return actions
}

func fallbackNextActions(facts ConsoleCaseFacts) []string {
	if facts.OpenIncident {
		return []string{"Review the latest critical evidence", "Confirm ownership and response status", "Record the next operator decision"}
	}
	if facts.Recovered {
		return []string{"Review the recovery evidence", "Confirm the underlying cause is addressed", "Keep the case available for post-incident follow-up"}
	}
	return []string{"Continue watching the active signal window"}
}

func humanizeStory(value string) string {
	value = strings.NewReplacer("_", " ", ".", " ", "-", " ").Replace(strings.TrimSpace(value))
	words := strings.Fields(value)
	for index, word := range words {
		if word == "" {
			continue
		}
		words[index] = strings.ToUpper(word[:1]) + word[1:]
	}
	return strings.Join(words, " ")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

// renderEventDiagram is derived locally from ordered evidence. It shows a
// bounded causal sequence of customer-facing events instead of a mesh of
// internal birds and fungi. Mermaid remains the interchange format while the
// dashboard renders the selected facts as a compact causal chain.
func renderEventDiagram(facts ConsoleCaseFacts) string {
	events := selectEventDiagramEvents(facts.Events)
	if len(events) == 0 {
		return "flowchart LR\n  empty[No observed events]"
	}
	var builder strings.Builder
	builder.WriteString("sequenceDiagram\n")
	asset := firstNonEmpty(storyContextValue(facts, "asset"), "Affected operation")
	owner := firstNonEmpty(storyContextValue(facts, "owner"), storyContextValue(facts, "responsibility"), "Response owner")
	builder.WriteString(fmt.Sprintf("  participant asset as %s\n", mermaidLabel(asset)))
	builder.WriteString("  participant monitor as Detection and analysis\n")
	builder.WriteString(fmt.Sprintf("  participant response as %s\n", mermaidLabel(owner)))
	builder.WriteString("  participant contacts as Operations contacts\n")
	builder.WriteString("  participant cloud as Tucunar cloud\n")
	for _, event := range events {
		from, arrow, to := eventDiagramRoute(event)
		builder.WriteString(fmt.Sprintf("  %s%s%s: %s\n", from, arrow, to, mermaidLabel(eventDiagramLabel(event))))
	}
	return strings.TrimSpace(builder.String())
}

func eventDiagramRoute(event CaseFactEvent) (from, arrow, to string) {
	name := strings.ToLower(event.Name)
	switch {
	case strings.Contains(name, "notification") || strings.Contains(name, "delivery"):
		return "monitor", "-->>", "contacts"
	case strings.Contains(name, "cloud"):
		return "monitor", "->>", "cloud"
	case strings.Contains(name, "recovery") || strings.Contains(name, "handoff") || strings.Contains(name, "action"):
		return "response", "->>", "asset"
	case strings.Contains(name, "incident") || strings.Contains(name, "correlation") || strings.Contains(name, "evidence") || strings.Contains(name, "reasoning") || strings.Contains(name, "timeline"):
		return "monitor", "->>", "response"
	default:
		return "asset", "->>", "monitor"
	}
}

func selectEventDiagramEvents(events []CaseFactEvent) []CaseFactEvent {
	if len(events) <= maxEventDiagramEvents {
		return events
	}
	selected := make(map[int]struct{}, maxEventDiagramEvents)
	add := func(index int) {
		if index >= 0 && index < len(events) && len(selected) < maxEventDiagramEvents {
			selected[index] = struct{}{}
		}
	}
	addFirstEvent(events, selected, func(event CaseFactEvent) bool {
		name := strings.ToLower(event.Name)
		return strings.Contains(name, "timeout.surge") || strings.Contains(name, "health.degraded")
	})
	addFirstEvent(events, selected, func(event CaseFactEvent) bool {
		return strings.Contains(strings.ToLower(event.Name), "incident.opened")
	})
	addFirstEvent(events, selected, func(event CaseFactEvent) bool {
		name := strings.ToLower(event.Name)
		return strings.Contains(name, "correlation.completed") || strings.Contains(name, "handoff.created")
	})
	addFirstEvent(events, selected, func(event CaseFactEvent) bool {
		name := strings.ToLower(event.Name)
		return strings.Contains(name, "notification.failed") || strings.Contains(name, "notification.delivered")
	})
	addLastEvent(events, selected, func(event CaseFactEvent) bool {
		name := strings.ToLower(event.Name)
		return strings.Contains(name, "incident.recovered") || strings.Contains(name, "health.recovered") || strings.Contains(name, "recovery.action")
	})
	add(len(events) - 1)
	if len(selected) < maxEventDiagramEvents {
		stride := len(events) / maxEventDiagramEvents
		if stride < 1 {
			stride = 1
		}
		for index := stride; index < len(events)-1 && len(selected) < maxEventDiagramEvents; index += stride {
			add(index)
		}
	}
	indexes := make([]int, 0, len(selected))
	for index := range selected {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	result := make([]CaseFactEvent, 0, len(indexes))
	for _, index := range indexes {
		result = append(result, events[index])
	}
	return result
}

func addFirstEvent(events []CaseFactEvent, selected map[int]struct{}, predicate func(CaseFactEvent) bool) {
	if len(selected) >= maxEventDiagramEvents {
		return
	}
	for index, event := range events {
		if predicate(event) {
			selected[index] = struct{}{}
			return
		}
	}
}

func addLastEvent(events []CaseFactEvent, selected map[int]struct{}, predicate func(CaseFactEvent) bool) {
	if len(selected) >= maxEventDiagramEvents {
		return
	}
	for index := len(events) - 1; index >= 0; index-- {
		if predicate(events[index]) {
			selected[index] = struct{}{}
			return
		}
	}
}

func eventDiagramLabel(event CaseFactEvent) string {
	label := firstNonEmpty(event.Message, humanizeStory(event.Name), "Observed event")
	if runes := []rune(label); len(runes) > 110 {
		label = string(runes[:107]) + "…"
	}
	return event.OccurredAt.UTC().Format("15:04:05") + " · " + label
}

func mermaidLabel(value string) string {
	value = strings.TrimSpace(value)
	value = strings.NewReplacer("\"", "'", "[", "(", "]", ")", "\n", " ", "\r", " ").Replace(value)
	if len([]rune(value)) > 80 {
		value = string([]rune(value)[:80]) + "…"
	}
	return value
}
