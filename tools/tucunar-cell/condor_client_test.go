package canarynode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	connect "connectrpc.com/connect"
	tucunarv1 "github.com/tucunar/contracts/gen/tucunar/v1"
	tucunarv1connect "github.com/tucunar/contracts/gen/tucunar/v1/tucunarv1connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeCondorRPC struct {
	appendRequest *connect.Request[tucunarv1.AppendRequest]
	readRequests  []*connect.Request[tucunarv1.ReadRequest]
	readResponses []*tucunarv1.ReadResponse
}

func (f *fakeCondorRPC) Append(_ context.Context, request *connect.Request[tucunarv1.AppendRequest]) (*connect.Response[tucunarv1.AppendResponse], error) {
	f.appendRequest = request
	return connect.NewResponse(&tucunarv1.AppendResponse{Accepted: uint64(len(request.Msg.Signals))}), nil
}

func (f *fakeCondorRPC) Read(_ context.Context, request *connect.Request[tucunarv1.ReadRequest]) (*connect.Response[tucunarv1.ReadResponse], error) {
	f.readRequests = append(f.readRequests, request)
	response := f.readResponses[0]
	f.readResponses = f.readResponses[1:]
	return connect.NewResponse(response), nil
}

func TestCondorClientAppendBindsContextAndAuthorization(t *testing.T) {
	fake := &fakeCondorRPC{}
	client := &condorClient{token: "internal-token", organizationID: "org-1", cellID: "cell-1", nodeID: "node-1", client: fake}
	signal := Signal{ID: "550e8400-e29b-41d4-a716-446655440000", Source: "sensor", TenantID: "tenant", EntityID: "entity", Kind: "measurement", Name: "load", OccurredAt: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)}
	if err := client.AppendSignals(context.Background(), []Signal{signal}); err != nil {
		t.Fatalf("AppendSignals() error = %v", err)
	}
	if fake.appendRequest == nil || fake.appendRequest.Header().Get("Authorization") != "Bearer internal-token" {
		t.Fatalf("append authorization = %q", fake.appendRequest.Header().Get("Authorization"))
	}
	requestContext := fake.appendRequest.Msg.Context
	if requestContext.OrganizationId != "org-1" || requestContext.CellId != "cell-1" || requestContext.NodeId != "node-1" || requestContext.ContractVersion != "1.0" {
		t.Fatalf("append context = %#v", requestContext)
	}
	if fake.appendRequest.Msg.Signals[0].Source != "sensor" {
		t.Fatalf("append signal = %#v", fake.appendRequest.Msg.Signals[0])
	}
}

func TestCondorClientReadCarriesSnapshotBounds(t *testing.T) {
	fake := &fakeCondorRPC{readResponses: []*tucunarv1.ReadResponse{{
		Context:    &tucunarv1.TenantContext{ContractVersion: "1.0", OrganizationId: "org-1", CellId: "cell-1", NodeId: "node-1"},
		Signals:    []*tucunarv1.Signal{{Source: "sensor", TenantId: "tenant", EntityId: "entity", Kind: "measurement", Name: "load", OccurredAt: timestampForTest()}},
		NextCursor: "next", HasMore: false,
	}}}
	client := &condorClient{token: "token", organizationID: "org-1", cellID: "cell-1", nodeID: "node-1", client: fake}
	since := time.Date(2026, 9, 6, 11, 0, 0, 0, time.UTC)
	until := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	result, err := client.ReadSignals(context.Background(), CondorReadOptions{Consumer: "canary-evaluator", Since: since, Until: until, Limit: 50})
	if err != nil {
		t.Fatalf("ReadSignals() error = %v", err)
	}
	if len(result.Signals) != 1 || result.NextCursor != "next" {
		t.Fatalf("read result = %#v", result)
	}
	request := fake.readRequests[0]
	if request.Header().Get("Authorization") != "Bearer token" || request.Msg.Consumer != "canary-evaluator" || request.Msg.Limit != 50 {
		t.Fatalf("read request = %#v", request.Msg)
	}
	if !request.Msg.Since.AsTime().Equal(since) || !request.Msg.Until.AsTime().Equal(until) {
		t.Fatalf("read bounds = %v / %v", request.Msg.Since.AsTime(), request.Msg.Until.AsTime())
	}
}

func TestCondorClientReadWindowRejectsRepeatedCursor(t *testing.T) {
	fake := &fakeCondorRPC{readResponses: []*tucunarv1.ReadResponse{
		{Context: &tucunarv1.TenantContext{ContractVersion: "1.0", OrganizationId: "org-1", CellId: "cell-1", NodeId: "node-1"}, NextCursor: "same", HasMore: true},
		{Context: &tucunarv1.TenantContext{ContractVersion: "1.0", OrganizationId: "org-1", CellId: "cell-1", NodeId: "node-1"}, NextCursor: "same", HasMore: true},
	}}
	client := &condorClient{organizationID: "org-1", cellID: "cell-1", nodeID: "node-1", client: fake}
	if _, err := client.ReadWindow(context.Background(), "reader", time.Now().Add(-time.Minute), time.Now()); err == nil {
		t.Fatal("ReadWindow() accepted a repeated cursor")
	}
}

type replacementCondor struct {
	authorized bool
	appended   int
}

func (r *replacementCondor) Append(_ context.Context, request *connect.Request[tucunarv1.AppendRequest]) (*connect.Response[tucunarv1.AppendResponse], error) {
	r.authorized = request.Header().Get("Authorization") == "Bearer replacement-token"
	r.appended += len(request.Msg.Signals)
	return connect.NewResponse(&tucunarv1.AppendResponse{Accepted: uint64(len(request.Msg.Signals))}), nil
}

func (r *replacementCondor) Read(_ context.Context, request *connect.Request[tucunarv1.ReadRequest]) (*connect.Response[tucunarv1.ReadResponse], error) {
	r.authorized = r.authorized && request.Header().Get("Authorization") == "Bearer replacement-token"
	return connect.NewResponse(&tucunarv1.ReadResponse{
		Context: request.Msg.Context,
		Signals: []*tucunarv1.Signal{{Id: "550e8400-e29b-41d4-a716-446655440001", Source: "replacement", TenantId: "tenant", EntityId: "entity", Kind: "event", Name: "heartbeat", OccurredAt: timestampForTest()}},
	}), nil
}

func TestCondorClientWorksWithExternalReplacementHandler(t *testing.T) {
	replacement := &replacementCondor{}
	path, handler := tucunarv1connect.NewCondorServiceHandler(replacement)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	defer server.Close()
	t.Setenv("CONDOR_CONNECT_URL", server.URL)
	t.Setenv("SWIFT_CONDOR_URL", "")
	t.Setenv("CANARY_ORGANIZATION_ID", "org-replacement")
	t.Setenv("CANARY_ID", "cell-replacement")
	t.Setenv("CANARY_NODE_ID", "node-replacement")
	t.Setenv("TUCUNAR_INTERNAL_TOKEN", "replacement-token")
	client, err := newCondorClient()
	if err != nil {
		t.Fatalf("newCondorClient() error = %v", err)
	}
	signal := Signal{ID: "550e8400-e29b-41d4-a716-446655440002", Source: "source", TenantID: "tenant", EntityID: "entity", Kind: "event", Name: "input", OccurredAt: timestampForTest().AsTime()}
	if err := client.AppendSignals(context.Background(), []Signal{signal}); err != nil {
		t.Fatalf("AppendSignals() error = %v", err)
	}
	result, err := client.ReadSignals(context.Background(), CondorReadOptions{Consumer: "replacement", Limit: 1})
	if err != nil {
		t.Fatalf("ReadSignals() error = %v", err)
	}
	if !replacement.authorized || replacement.appended != 1 || len(result.Signals) != 1 || result.Signals[0].Source != "replacement" {
		t.Fatalf("replacement interaction = authorized=%v appended=%d result=%#v", replacement.authorized, replacement.appended, result)
	}
}

func timestampForTest() *timestamppb.Timestamp {
	return timestamppb.New(time.Date(2026, 9, 6, 11, 30, 0, 0, time.UTC))
}

var _ interface {
	Append(context.Context, *connect.Request[tucunarv1.AppendRequest]) (*connect.Response[tucunarv1.AppendResponse], error)
	Read(context.Context, *connect.Request[tucunarv1.ReadRequest]) (*connect.Response[tucunarv1.ReadResponse], error)
} = (*fakeCondorRPC)(nil)
