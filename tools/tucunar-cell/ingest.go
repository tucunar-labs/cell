package canarynode

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type Server struct {
	store           *Store
	writer          signalWriter
	secret          string
	shopifySecrets  map[string]string
	signalToken     string
	organizationID  string
	canaryID        string
	requireContext  bool
	operatorToken   string
	dashboardPublic bool
	queryEnabled    bool
	hub             *streamHub
}

func newRouter(store *Store, secret, signalToken string, identity ...string) http.Handler {
	return newRouterWithShopifySecretsAndWriter(store, secret, nil, signalToken, nil, identity...)
}

func newRouterWithShopifySecrets(store *Store, secret string, shopifySecrets map[string]string, signalToken string, identity ...string) http.Handler {
	return newRouterWithShopifySecretsAndWriter(store, secret, shopifySecrets, signalToken, nil, identity...)
}

func newRouterWithShopifySecretsAndWriter(store *Store, secret string, shopifySecrets map[string]string, signalToken string, writer signalWriter, identity ...string) http.Handler {
	organizationID, canaryID := "", ""
	if len(identity) > 0 {
		organizationID = strings.TrimSpace(identity[0])
	}
	if len(identity) > 1 {
		canaryID = strings.TrimSpace(identity[1])
	}
	if writer == nil {
		writer = store
	}
	s := &Server{
		store: store, secret: secret, shopifySecrets: shopifySecrets, signalToken: signalToken,
		organizationID: organizationID, canaryID: canaryID,
		writer:          writer,
		requireContext:  env("SIGNAL_REQUIRE_CONTEXT", "0") == "1",
		operatorToken:   strings.TrimSpace(os.Getenv("CANARY_NODE_API_TOKEN")),
		dashboardPublic: env("CANARY_DASHBOARD_PUBLIC", "0") == "1",
		queryEnabled:    env("CANARY_QUERY_ENABLED", "0") == "1",
		hub:             newStreamHub(),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/api/dashboard", s.handleDashboardAPI)
	mux.HandleFunc("/api/overview", s.handleOperationsOverviewAPI)
	mux.HandleFunc("/api/stream", s.stream)
	mux.HandleFunc("/api/console/events", s.handleConsoleEventsAPI)
	mux.HandleFunc("/api/scenarios", s.handleScenariosAPI)
	mux.HandleFunc("/api/story", s.handleCaseStoryAPI)
	mux.HandleFunc("/api/query", s.handleQueryAPI)
	mux.HandleFunc("/api/diagnostics", s.handleDiagnosticsAPI)
	mux.HandleFunc("/version", s.handleVersionAPI)
	mux.HandleFunc("/v1/signals", s.handleSignal)
	mux.HandleFunc("/v1/signals/batch", s.handleSignalBatch)
	mux.HandleFunc("/webhooks/shopify", s.handleShopifyWebhook)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", s.handleReadiness)
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		writeSelfReport(w, r, "swift", s.store)
	})
	return requestLoggingMiddleware(s, mux)
}

func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ready(ctx); err != nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

func (s *Server) authorizeOperatorAPI(r *http.Request) bool {
	if s.operatorToken == "" {
		return true
	}
	provided := strings.TrimSpace(r.Header.Get("X-Canary-API-Key"))
	if authorization := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(strings.ToLower(authorization), "bearer ") {
		provided = strings.TrimSpace(authorization[len("Bearer "):])
	}
	expectedHash := sha256.Sum256([]byte(s.operatorToken))
	providedHash := sha256.Sum256([]byte(provided))
	return subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) == 1
}

// authorizeDashboardAPI allows a local, explicitly opted-in read-only
// dashboard for demos. Intake, diagnostics and all operator controls continue
// to require the operator token. Production defaults to protected (false).
func (s *Server) authorizeDashboardAPI(r *http.Request) bool {
	if s.dashboardPublic {
		return true
	}
	return s.authorizeOperatorAPI(r)
}

const (
	maxSingleSignalBody = 1 << 20
	maxBatchSignalBody  = 4 << 20
	maxSignalBatchSize  = 1000
)

func (s *Server) handleSignal(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "HTTP POST /v1/signals",
		withAttributes(
			spanString("http.method", r.Method),
			spanString("http.target", r.URL.Path),
		))
	defer span.End()

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorizeSignalIngest(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="canary-signals"`)
		http.Error(w, "invalid ingest token", http.StatusUnauthorized)
		return
	}
	body, err := readRequestBody(r, maxSingleSignalBody)
	defer r.Body.Close()
	if err != nil {
		writeSignalError(w, err)
		return
	}
	signal, err := decodeSignal(body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "invalid signal")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if signal, err = s.applySignalContext(signal); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if err := s.writer.InsertSignals(ctx, []Signal{signal}); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "insert failed")
		http.Error(w, "insert failed", http.StatusInternalServerError)
		return
	}
	span.SetAttributes(spanString("signal.source", signal.Source), spanString("signal.name", signal.Name))
	if s.hub != nil {
		s.hub.publish()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "id": signal.ID})
}

type signalBatchRequest struct {
	Signals []Signal `json:"signals"`
}

func (s *Server) handleSignalBatch(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "HTTP POST /v1/signals/batch",
		withAttributes(
			spanString("http.method", r.Method),
			spanString("http.target", r.URL.Path),
		))
	defer span.End()

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorizeSignalIngest(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="canary-signals"`)
		http.Error(w, "invalid ingest token", http.StatusUnauthorized)
		return
	}
	body, err := readRequestBody(r, maxBatchSignalBody)
	defer r.Body.Close()
	if err != nil {
		writeSignalError(w, err)
		return
	}
	var request signalBatchRequest
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "invalid signal batch")
		http.Error(w, "invalid signal batch JSON", http.StatusBadRequest)
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			http.Error(w, "request must contain one JSON object", http.StatusBadRequest)
			return
		}
		http.Error(w, "invalid trailing JSON", http.StatusBadRequest)
		return
	}
	if len(request.Signals) == 0 {
		http.Error(w, "signals must not be empty", http.StatusBadRequest)
		return
	}
	if len(request.Signals) > maxSignalBatchSize {
		http.Error(w, fmt.Sprintf("signals cannot contain more than %d items", maxSignalBatchSize), http.StatusBadRequest)
		return
	}
	for i := range request.Signals {
		request.Signals[i], err = request.Signals[i].Normalize()
		if err != nil {
			span.RecordError(err)
			span.SetStatus(spanError, "invalid signal")
			http.Error(w, fmt.Sprintf("signals[%d]: %v", i, err), http.StatusBadRequest)
			return
		}
		request.Signals[i], err = s.applySignalContext(request.Signals[i])
		if err != nil {
			span.RecordError(err)
			span.SetStatus(spanError, "invalid signal context")
			http.Error(w, fmt.Sprintf("signals[%d]: %v", i, err), http.StatusUnprocessableEntity)
			return
		}
	}
	if err := s.writer.InsertSignals(ctx, request.Signals); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "insert failed")
		http.Error(w, "insert failed", http.StatusInternalServerError)
		return
	}
	span.SetAttributes(spanInt("signal.count", len(request.Signals)))
	if s.hub != nil {
		s.hub.publish()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "accepted": len(request.Signals)})
}

func readRequestBody(r *http.Request, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read body")
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("request body too large")
	}
	return body, nil
}

func writeSignalError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if err.Error() == "request body too large" {
		status = http.StatusRequestEntityTooLarge
	}
	http.Error(w, err.Error(), status)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// authorizeSignalIngest keeps generic ingest separate from Shopify HMAC.
// Hashing both values first makes the comparison constant-time even when token
// lengths differ. An empty configured token intentionally enables local dev.
func (s *Server) authorizeSignalIngest(r *http.Request) bool {
	if s.signalToken == "" {
		return true
	}
	provided := ""
	if authorization := strings.TrimSpace(r.Header.Get("Authorization")); authorization != "" {
		parts := strings.SplitN(authorization, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			provided = strings.TrimSpace(parts[1])
		}
	}
	if provided == "" {
		provided = strings.TrimSpace(r.Header.Get("X-Canary-API-Key"))
	}
	expectedHash := sha256.Sum256([]byte(s.signalToken))
	providedHash := sha256.Sum256([]byte(provided))
	return subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) == 1
}

func (s *Server) handleShopifyWebhook(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "HTTP POST /webhooks/shopify",
		withAttributes(
			spanString("http.method", r.Method),
			spanString("http.target", r.URL.Path),
			spanString("shop_id", r.Header.Get("X-Shopify-Shop-Domain")),
			spanString("event_type", r.Header.Get("X-Shopify-Topic")),
		))
	defer span.End()

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := readRequestBody(r, maxSingleSignalBody)
	if err != nil {
		writeSignalError(w, err)
		return
	}
	defer r.Body.Close()

	// HMAC de Shopify sobre el body crudo. A production secrets file requires a
	// matching shop; the global secret remains only as a local-dev fallback.
	shop := normalizeShopDomain(r.Header.Get("X-Shopify-Shop-Domain"))
	secret, configured := s.shopifySecret(shop)
	if !configured {
		http.Error(w, "shop is not configured", http.StatusUnauthorized)
		return
	}
	if secret != "" && !verifyShopifyHMAC(secret, body, r.Header.Get("X-Shopify-Hmac-Sha256")) {
		span.SetStatus(spanError, "invalid HMAC")
		http.Error(w, "invalid HMAC signature", http.StatusUnauthorized)
		return
	}

	signal, err := mapShopifySignal(r, body)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "invalid Shopify webhook")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if signal, err = s.applySignalContext(signal); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if err := s.writer.InsertSignals(ctx, []Signal{signal}); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "insert failed")
		http.Error(w, "insert failed", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// applySignalContext binds every signal to the node's configured tenant. A
// producer cannot smuggle another organization or canary through the generic
// endpoint. Development permits an unscoped node when context is not
// configured; deployed nodes should set SIGNAL_REQUIRE_CONTEXT=1.
func (s *Server) applySignalContext(signal Signal) (Signal, error) {
	if s.requireContext && (s.organizationID == "" || s.canaryID == "") {
		return Signal{}, fmt.Errorf("organization_id and canary_id are required on the node")
	}
	if s.organizationID != "" && signal.OrganizationID != "" && signal.OrganizationID != s.organizationID {
		return Signal{}, fmt.Errorf("organization_id does not match node configuration")
	}
	if s.canaryID != "" && signal.CanaryID != "" && signal.CanaryID != s.canaryID {
		return Signal{}, fmt.Errorf("canary_id does not match node configuration")
	}
	if s.organizationID != "" {
		signal.OrganizationID = s.organizationID
	}
	if s.canaryID != "" {
		signal.CanaryID = s.canaryID
	}
	if s.requireContext && (signal.OrganizationID == "" || signal.CanaryID == "") {
		return Signal{}, fmt.Errorf("organization_id and canary_id are required")
	}
	return signal, nil
}

func mapShopifySignal(r *http.Request, body []byte) (Signal, error) {
	shopID := r.Header.Get("X-Shopify-Shop-Domain")
	if shopID == "" {
		shopID = "unknown"
	}
	eventType := r.Header.Get("X-Shopify-Topic")
	if eventType == "" {
		eventType = extractEventType(body)
	}
	signal := Signal{
		ExternalID: r.Header.Get("X-Shopify-Webhook-Id"),
		Source:     "shopify",
		TenantID:   shopID,
		EntityID:   shopID,
		Kind:       "event",
		Name:       eventType,
		Payload:    json.RawMessage(body),
		OccurredAt: time.Now().UTC(),
	}
	return signal.Normalize()
}

func verifyShopifyHMAC(secret string, body []byte, header string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(header))
}

func extractEventType(body []byte) string {
	var v struct {
		EventType string `json:"event_type"`
		Type      string `json:"type"`
	}
	if json.Unmarshal(body, &v) == nil {
		if v.EventType != "" {
			return v.EventType
		}
		return v.Type
	}
	return "unknown"
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // UUID v4
	b[8] = (b[8] & 0x3f) | 0x80 // variante 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
