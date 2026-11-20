package canarynode

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

type caseStoryRequest struct {
	ScenarioID string `json:"scenario_id"`
	Force      bool   `json:"force,omitempty"`
}

func (s *Server) handleCaseStoryAPI(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "HTTP case story",
		withAttributes(spanString("http.method", r.Method), spanString("http.target", r.URL.Path)))
	defer span.End()
	if !s.authorizeDashboardAPI(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="canary-operator"`)
		http.Error(w, "operator token required", http.StatusUnauthorized)
		return
	}
	var scenarioID string
	force := false
	switch r.Method {
	case http.MethodGet:
		scenarioID = strings.TrimSpace(r.URL.Query().Get("scenario"))
	case http.MethodPost:
		body, err := readLimitedBody(r, 16<<10)
		if err != nil {
			http.Error(w, "invalid story request", http.StatusBadRequest)
			return
		}
		var request caseStoryRequest
		if len(body) > 0 {
			if err := json.Unmarshal(body, &request); err != nil {
				http.Error(w, "invalid story request", http.StatusBadRequest)
				return
			}
		}
		scenarioID, force = strings.TrimSpace(request.ScenarioID), request.Force
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if scenarioID == "" {
		http.Error(w, "scenario_id is required", http.StatusBadRequest)
		return
	}
	var (
		story CaseStory
		err   error
	)
	if r.Method == http.MethodGet {
		var found bool
		story, found, err = s.store.StoredCaseStory(ctx, scenarioID)
		if err == nil && !found {
			http.Error(w, "story not generated", http.StatusNotFound)
			return
		}
		if err == nil {
			// Older cached stories may predate the customer-focused narrative.
			// Re-read bounded facts and present them through the current contract
			// without spending another Shiitake call or mutating storage.
			timeline, timelineErr := s.store.Timeline(ctx, scenarioID, 7*24*time.Hour)
			if timelineErr != nil {
				err = timelineErr
			} else {
				story = customerFocusStory(story, caseFactsFromTimeline(timeline))
			}
		}
	} else {
		story, err = s.store.BuildCaseStory(ctx, scenarioID, force)
	}
	if err != nil {
		logger.Error("case story lookup failed", "scenario_id", scenarioID, "error", err)
		span.RecordError(err)
		span.SetStatus(spanError, "case story unavailable")
		var rateLimitError *deepSeekRateLimitError
		if errors.As(err, &rateLimitError) {
			if rateLimitError.scope == "daily" {
				w.Header().Set("Retry-After", "86400")
			}
			writeServiceJSON(w, http.StatusTooManyRequests, map[string]any{
				"error": rateLimitError.Error(),
				"code":  "deepseek_rate_limited",
				"scope": rateLimitError.scope,
			})
			return
		}
		http.Error(w, "case story unavailable", http.StatusServiceUnavailable)
		return
	}
	span.SetAttributes(spanString("story.status", story.Status), spanInt("story.flow_nodes", len(story.Flow.Nodes)), spanInt("story.flow_edges", len(story.Flow.Edges)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(story)
}
