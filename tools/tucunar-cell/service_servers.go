package canarynode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	connect "connectrpc.com/connect"
	tucunarv1 "github.com/tucunar/contracts/gen/tucunar/v1"
	tucunarv1connect "github.com/tucunar/contracts/gen/tucunar/v1/tucunarv1connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const maxInternalRequestBody = 4 << 20

type capabilityServer struct {
	definition CapabilityDefinition
	executor   CapabilityExecutor
	store      *Store
	condor     condorWindowReader
}

func newCapabilityServer(definition CapabilityDefinition) http.Handler {
	return newCapabilityServerWithStore(definition, nil)
}

func newCapabilityServerWithStore(definition CapabilityDefinition, store *Store) http.Handler {
	return newCapabilityServerWithCondor(definition, store, nil)
}

func newCapabilityServerWithCondor(definition CapabilityDefinition, store *Store, condor condorWindowReader) http.Handler {
	server := &capabilityServer{
		definition: definition,
		executor:   capabilityExecutorFor(definition),
		store:      store,
		condor:     condor,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", server.ready)
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		writeSelfReport(w, r, string(definition.ID), server.store)
	})
	mux.HandleFunc("/version", server.version)
	mux.HandleFunc("/v1/capability", server.definitionHandler)
	mux.HandleFunc("/v1/capability/execute", server.execute)
	return requestLoggingMiddleware(nil, mux)
}

func (s *capabilityServer) ready(w http.ResponseWriter, r *http.Request) {
	if s.store != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.store.Ready(ctx); err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

func (s *capabilityServer) version(w http.ResponseWriter, _ *http.Request) {
	writeServiceJSON(w, http.StatusOK, map[string]string{
		"service": string(s.definition.ID),
		"version": currentNodeVersion(),
	})
}

func (s *capabilityServer) definitionHandler(w http.ResponseWriter, _ *http.Request) {
	writeServiceJSON(w, http.StatusOK, s.definition)
}

func (s *capabilityServer) execute(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "capability.execute",
		withAttributes(spanString("tucunar.capability", string(s.definition.ID))))
	defer span.End()
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !authorizeInternalRequest(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := readLimitedBody(r, maxInternalRequestBody)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var request CapabilityRequest
	if err := json.Unmarshal(body, &request); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		http.Error(w, "invalid capability request", http.StatusBadRequest)
		return
	}
	if request.Capability == "" {
		request.Capability = s.definition.ID
	}
	if s.store != nil {
		ctx = withAIStore(ctx, s.store)
	}
	if s.condor != nil {
		ctx = withCondorWindowReader(ctx, s.condor)
	}
	response, err := s.executor.Execute(ctx, request)
	if err != nil {
		status := http.StatusInternalServerError
		if _, rateLimited := deepSeekRateLimitFromError(err); rateLimited || response.ErrorCode == "rate_limited" {
			status = http.StatusTooManyRequests
			if rateLimitError, ok := deepSeekRateLimitFromError(err); ok && rateLimitError.scope == "daily" {
				w.Header().Set("Retry-After", "86400")
			}
		} else if response.Status == "rejected" {
			status = http.StatusBadRequest
		}
		writeServiceJSON(w, status, response)
		return
	}
	writeServiceJSON(w, http.StatusOK, response)
}

type condorServer struct {
	store *Store
	ready *dependencyHandler
}

func newCondorServer(store *Store) http.Handler {
	server := &condorServer{store: store, ready: &dependencyHandler{store: store, name: "condor"}}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", server.ready.health)
	mux.HandleFunc("/readyz", server.ready.ready)
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		writeSelfReport(w, r, "condor", server.store)
	})
	mux.HandleFunc("/version", server.ready.version)
	mux.HandleFunc("/v1/streams/append", server.append)
	mux.HandleFunc("/v1/streams/read", server.read)
	connectPath, connectHandler := tucunarv1connect.NewCondorServiceHandler(&condorConnectService{store: store})
	mux.Handle(connectPath, connectHandler)
	return requestLoggingMiddleware(nil, mux)
}

type condorConnectService struct {
	tucunarv1connect.UnimplementedCondorServiceHandler
	store *Store
}

func (s *condorConnectService) Append(ctx context.Context, request *connect.Request[tucunarv1.AppendRequest]) (*connect.Response[tucunarv1.AppendResponse], error) {
	if request == nil || request.Msg == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("append request is required"))
	}
	if !authorizeInternalHeaders(request.Header()) {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("internal authorization is invalid"))
	}
	appendRequest := request.Msg
	if appendRequest.Context == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("tenant context is required"))
	}
	if err := validateCondorContext(appendRequest.Context); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := validateCondorScope(s.store, appendRequest.Context); err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, err)
	}
	if len(appendRequest.Signals) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("signals must not be empty"))
	}

	signals := make([]Signal, 0, len(appendRequest.Signals))
	for _, protoSignal := range appendRequest.Signals {
		signal, err := condorSignalFromProto(appendRequest.Context, protoSignal)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		signals = append(signals, signal)
	}
	if err := s.store.InsertSignals(ctx, signals); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("append signals: %w", err))
	}
	return connect.NewResponse(&tucunarv1.AppendResponse{
		Context:      appendRequest.Context,
		Accepted:     uint64(len(signals)),
		Deduplicated: 0,
	}), nil
}

func (s *condorConnectService) Read(ctx context.Context, request *connect.Request[tucunarv1.ReadRequest]) (*connect.Response[tucunarv1.ReadResponse], error) {
	if request == nil || request.Msg == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("read request is required"))
	}
	if !authorizeInternalHeaders(request.Header()) {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("internal authorization is invalid"))
	}
	readRequest := request.Msg
	if readRequest.Context == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("tenant context is required"))
	}
	if err := validateCondorContext(readRequest.Context); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := validateCondorScope(s.store, readRequest.Context); err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, err)
	}
	if strings.TrimSpace(readRequest.Stream) != "signals" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("stream must be signals"))
	}
	consumer, err := validateCondorConsumer(readRequest.Consumer)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	storedCursor, err := s.store.ConsumerOffset(ctx, consumer)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	cursor := strings.TrimSpace(readRequest.Cursor)
	var since time.Time
	if readRequest.Since != nil {
		if err := readRequest.Since.CheckValid(); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("since is invalid: %w", err))
		}
		since = readRequest.Since.AsTime()
	}
	if cursor == "" && since.IsZero() {
		cursor = storedCursor
	}
	until := time.Time{}
	if readRequest.Until != nil {
		if err := readRequest.Until.CheckValid(); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("until is invalid: %w", err))
		}
		until = readRequest.Until.AsTime()
	}
	signals, nextCursor, hasMore, err := s.store.ReadSignalsSince(ctx, cursor, readRequest.Limit, since, until)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	encoded := make([]*tucunarv1.Signal, 0, len(signals))
	for _, signal := range signals {
		protoSignal, encodeErr := condorSignalToProto(signal)
		if encodeErr != nil {
			return nil, connect.NewError(connect.CodeInternal, encodeErr)
		}
		encoded = append(encoded, protoSignal)
	}
	if nextCursor != "" && since.IsZero() && (storedCursor == "" || cursor == storedCursor) {
		if err := s.store.SaveConsumerOffset(ctx, consumer, nextCursor); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	return connect.NewResponse(&tucunarv1.ReadResponse{
		Context:    readRequest.Context,
		Signals:    encoded,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}), nil
}

func validateCondorContext(value *tucunarv1.TenantContext) error {
	if value.ContractVersion != "1.0" {
		return errors.New("unsupported contract version")
	}
	for name, field := range map[string]string{
		"request_id":      value.RequestId,
		"idempotency_key": value.IdempotencyKey,
		"organization_id": value.OrganizationId,
		"cell_id":         value.CellId,
		"node_id":         value.NodeId,
	} {
		if strings.TrimSpace(field) == "" {
			return fmt.Errorf("tenant context %s is required", name)
		}
	}
	return nil
}

func validateCondorScope(store *Store, value *tucunarv1.TenantContext) error {
	if store == nil {
		return errors.New("condor store is unavailable")
	}
	if store.organizationID != "" && store.organizationID != value.OrganizationId {
		return errors.New("tenant organization_id is not served by this cell")
	}
	if store.canaryID != "" && store.canaryID != value.CellId {
		return errors.New("tenant cell_id is not served by this cell")
	}
	return nil
}

func validateCondorConsumer(value string) (string, error) {
	consumer := strings.TrimSpace(value)
	if consumer == "" || len(consumer) > 128 {
		return "", errors.New("consumer is required and must be at most 128 characters")
	}
	return consumer, nil
}

func condorSignalFromProto(context *tucunarv1.TenantContext, value *tucunarv1.Signal) (Signal, error) {
	if value == nil {
		return Signal{}, errors.New("signal must not be null")
	}
	if value.OccurredAt == nil {
		return Signal{}, errors.New("signal occurred_at is required")
	}
	if err := value.OccurredAt.CheckValid(); err != nil {
		return Signal{}, fmt.Errorf("signal occurred_at is invalid: %w", err)
	}
	payload := json.RawMessage(`{}`)
	if value.Payload != nil {
		encoded, err := protojson.Marshal(value.Payload)
		if err != nil {
			return Signal{}, fmt.Errorf("signal payload is invalid: %w", err)
		}
		payload = encoded
	}
	dimensions := make(map[string]string, len(value.Dimensions))
	for key, item := range value.Dimensions {
		dimensions[key] = item
	}
	if value.OrganizationId != "" && value.OrganizationId != context.OrganizationId {
		return Signal{}, errors.New("signal organization_id does not match tenant context")
	}
	if value.CanaryId != "" && value.CanaryId != context.CellId {
		return Signal{}, errors.New("signal canary_id does not match tenant context")
	}
	return Signal{
		ID:             value.Id,
		ExternalID:     value.ExternalId,
		OrganizationID: context.OrganizationId,
		CanaryID:       context.CellId,
		SiteID:         value.SiteId,
		Source:         value.Source,
		TenantID:       value.TenantId,
		EntityID:       value.EntityId,
		Kind:           value.Kind,
		Name:           value.Name,
		Value:          value.Value,
		Unit:           value.Unit,
		Dimensions:     dimensions,
		Payload:        payload,
		OccurredAt:     value.OccurredAt.AsTime(),
	}, nil
}

func condorSignalToProto(value Signal) (*tucunarv1.Signal, error) {
	payload := &structpb.Struct{}
	if len(value.Payload) > 0 && string(value.Payload) != "{}" {
		if err := protojson.Unmarshal(value.Payload, payload); err != nil {
			return nil, fmt.Errorf("signal payload is invalid: %w", err)
		}
	}
	return &tucunarv1.Signal{
		Id:             value.ID,
		ExternalId:     value.ExternalID,
		OrganizationId: value.OrganizationID,
		CanaryId:       value.CanaryID,
		SiteId:         value.SiteID,
		Source:         value.Source,
		TenantId:       value.TenantID,
		EntityId:       value.EntityID,
		Kind:           value.Kind,
		Name:           value.Name,
		Value:          value.Value,
		Unit:           value.Unit,
		Dimensions:     value.Dimensions,
		Payload:        payload,
		OccurredAt:     timestamppb.New(value.OccurredAt),
	}, nil
}

func (s *condorServer) append(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "condor.append",
		withAttributes(spanString("tucunar.service", "condor")))
	defer span.End()
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !authorizeInternalRequest(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := readLimitedBody(r, maxInternalRequestBody)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var request SignalStoreAppendRequest
	if err := json.Unmarshal(body, &request); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		http.Error(w, "invalid append request", http.StatusBadRequest)
		return
	}
	if len(request.Signals) == 0 {
		http.Error(w, "signals must not be empty", http.StatusBadRequest)
		return
	}
	if err := s.store.InsertSignals(ctx, request.Signals); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		http.Error(w, "append failed", http.StatusInternalServerError)
		return
	}
	writeServiceJSON(w, http.StatusAccepted, SignalStoreAppendResponse{
		CapabilityResult: CapabilityResult{ContractVersion: capabilityContractVersion, RequestID: request.RequestID, Capability: string(capabilitySignalStore), Operation: "append", Status: "accepted"},
		Accepted:         uint64(len(request.Signals)),
	})
}

func (s *condorServer) read(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "condor.read",
		withAttributes(spanString("tucunar.service", "condor")))
	defer span.End()
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !authorizeInternalRequest(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := readLimitedBody(r, maxInternalRequestBody)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var request signalStreamReadRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "invalid read request")
		http.Error(w, "invalid read request", http.StatusBadRequest)
		return
	}
	if request.RequestID == "" {
		request.RequestID = newRequestID()
	}
	signals, nextCursor, hasMore, err := s.store.ReadSignals(ctx, request.Cursor, request.Limit, request.Until)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "read failed")
		http.Error(w, "read failed", http.StatusBadRequest)
		return
	}
	span.SetAttributes(spanInt("signal.count", len(signals)), spanBool("signal.has_more", hasMore))
	writeServiceJSON(w, http.StatusOK, signalStreamReadResponse{
		CapabilityResult: CapabilityResult{ContractVersion: capabilityContractVersion, RequestID: request.RequestID, Capability: string(capabilitySignalStore), Operation: "query", Status: "completed"},
		Signals:          signals, NextCursor: nextCursor, HasMore: hasMore,
	})
}

func authorizeInternalRequest(r *http.Request) bool {
	return authorizeInternalHeaders(r.Header)
}

func authorizeInternalHeaders(headers http.Header) bool {
	expected := strings.TrimSpace(env("TUCUNAR_INTERNAL_TOKEN", ""))
	if expected == "" {
		expected = strings.TrimSpace(env("CAPABILITY_API_TOKEN", ""))
	}
	if expected == "" {
		return true
	}
	provided := strings.TrimSpace(headers.Get("X-Tucunar-Internal-Token"))
	if authorization := strings.TrimSpace(headers.Get("Authorization")); strings.HasPrefix(strings.ToLower(authorization), "bearer ") {
		provided = strings.TrimSpace(authorization[len("Bearer "):])
	}
	expectedHash := sha256.Sum256([]byte(expected))
	providedHash := sha256.Sum256([]byte(provided))
	return subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) == 1
}

func readLimitedBody(r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		return nil, errors.New("request body is required")
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil || int64(len(body)) > limit {
		return nil, errors.New("request body is too large or unreadable")
	}
	return body, nil
}

func writeServiceJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
