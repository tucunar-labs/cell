package canarynode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	connect "connectrpc.com/connect"
	tucunarv1 "github.com/tucunar/contracts/gen/tucunar/v1"
	tucunarv1connect "github.com/tucunar/contracts/gen/tucunar/v1/tucunarv1connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	defaultCondorConnectURL  = "http://condor:8080"
	condorClientTimeout      = 10 * time.Second
	maxCondorAppendBatch     = 1000
	maxCondorWindowSignals   = 100000
	maxCondorWindowPages     = maxCondorWindowSignals / maxCondorReadLimit
	defaultCondorWindowLimit = 1000
)

// CondorReadOptions describes a bounded read without exposing ClickHouse
// details to a bird. Since is an explicit snapshot lower bound; Cursor is the
// opaque continuation returned by Condor.
type CondorReadOptions struct {
	Consumer string
	Cursor   string
	Since    time.Time
	Until    time.Time
	Limit    uint32
}

// CondorReadResult is the transport-neutral result shared by all bird clients.
type CondorReadResult struct {
	Signals    []Signal
	NextCursor string
	HasMore    bool
}

// CondorReader is the small read seam used by consumers such as Canary and
// Raven. Keeping it separate from the append method makes replacement birds
// easy to test without a ClickHouse dependency.
type CondorReader interface {
	ReadSignals(context.Context, CondorReadOptions) (CondorReadResult, error)
}

type condorClient struct {
	baseURL        string
	token          string
	organizationID string
	cellID         string
	nodeID         string
	client         tucunarv1connect.CondorServiceClient
}

// newCondorClient creates the one shared Connect client used by birds in this
// cell. The URL is internal service configuration; HTTP is allowed because
// k0s network policy keeps the call inside the cell, while external calls are
// still required to use HTTPS elsewhere in the runtime.
func newCondorClient() (*condorClient, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(firstNonEmpty(
		env("CONDOR_CONNECT_URL", ""),
		env("SWIFT_CONDOR_URL", ""),
		defaultCondorConnectURL,
	)), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("CONDOR_CONNECT_URL must be an absolute URL without credentials or query parameters")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("CONDOR_CONNECT_URL must use HTTP or HTTPS")
	}
	organizationID := strings.TrimSpace(env("CANARY_ORGANIZATION_ID", ""))
	cellID := strings.TrimSpace(env("CANARY_ID", ""))
	nodeID := strings.TrimSpace(firstNonEmpty(
		env("CANARY_NODE_ID", ""),
		env("CANARY_NODE_INSTANCE_ID", ""),
		cellID,
		"local-node",
	))
	token := strings.TrimSpace(firstNonEmpty(env("TUCUNAR_INTERNAL_TOKEN", ""), env("CAPABILITY_API_TOKEN", "")))
	httpClient := &http.Client{Timeout: condorClientTimeout}
	return &condorClient{
		baseURL:        baseURL,
		token:          token,
		organizationID: organizationID,
		cellID:         cellID,
		nodeID:         nodeID,
		client:         tucunarv1connect.NewCondorServiceClient(httpClient, baseURL),
	}, nil
}

func (c *condorClient) InsertSignals(ctx context.Context, signals []Signal) error {
	return c.AppendSignals(ctx, signals)
}

// AppendSignals writes canonical signals through the versioned Condor
// boundary. The caller never needs to know the generated protobuf types.
func (c *condorClient) AppendSignals(ctx context.Context, signals []Signal) error {
	if c == nil || c.client == nil {
		return errors.New("condor client is unavailable")
	}
	if len(signals) == 0 || len(signals) > maxCondorAppendBatch {
		return fmt.Errorf("signals must contain 1 to %d entries", maxCondorAppendBatch)
	}
	requestContext := c.newContext("append", appendIdempotencyKey(signals))
	encoded := make([]*tucunarv1.Signal, 0, len(signals))
	for _, signal := range signals {
		value, err := condorSignalToProto(signal)
		if err != nil {
			return fmt.Errorf("encode signal: %w", err)
		}
		encoded = append(encoded, value)
	}
	request := connect.NewRequest(&tucunarv1.AppendRequest{Context: requestContext, Signals: encoded})
	c.authorize(request)
	response, err := c.client.Append(ctx, request)
	if err != nil {
		return fmt.Errorf("Condor Connect Append: %w", err)
	}
	if response == nil || response.Msg == nil || response.Msg.Accepted != uint64(len(signals)) {
		return errors.New("Condor Connect Append returned an incomplete acceptance")
	}
	return nil
}

// ReadSignals performs one bounded Connect read. A caller that needs a full
// window can use ReadWindow, which retains the same explicit bounds.
func (c *condorClient) ReadSignals(ctx context.Context, options CondorReadOptions) (CondorReadResult, error) {
	if c == nil || c.client == nil {
		return CondorReadResult{}, errors.New("condor client is unavailable")
	}
	consumer, err := validateCondorConsumer(options.Consumer)
	if err != nil {
		return CondorReadResult{}, err
	}
	if options.Limit == 0 {
		options.Limit = defaultCondorWindowLimit
	}
	if options.Limit > maxCondorReadLimit {
		return CondorReadResult{}, fmt.Errorf("limit cannot exceed %d", maxCondorReadLimit)
	}
	requestContext := c.newContext("read", "read:"+consumer)
	requestMessage := &tucunarv1.ReadRequest{
		Context:  requestContext,
		Stream:   "signals",
		Cursor:   strings.TrimSpace(options.Cursor),
		Consumer: consumer,
		Limit:    options.Limit,
	}
	if !options.Since.IsZero() {
		requestMessage.Since = timestamppb.New(options.Since.UTC())
	}
	if !options.Until.IsZero() {
		requestMessage.Until = timestamppb.New(options.Until.UTC())
	}
	request := connect.NewRequest(requestMessage)
	c.authorize(request)
	response, err := c.client.Read(ctx, request)
	if err != nil {
		return CondorReadResult{}, fmt.Errorf("Condor Connect Read: %w", err)
	}
	if response == nil || response.Msg == nil {
		return CondorReadResult{}, errors.New("Condor Connect Read returned an empty response")
	}
	responseContext := response.Msg.Context
	if responseContext == nil {
		responseContext = requestContext
	}
	result := CondorReadResult{
		Signals:    make([]Signal, 0, len(response.Msg.Signals)),
		NextCursor: response.Msg.NextCursor,
		HasMore:    response.Msg.HasMore,
	}
	for _, value := range response.Msg.Signals {
		signal, err := condorSignalFromProto(responseContext, value)
		if err != nil {
			return CondorReadResult{}, fmt.Errorf("decode Condor signal: %w", err)
		}
		result.Signals = append(result.Signals, signal)
	}
	return result, nil
}

// ReadWindow reconstructs a bounded lookback snapshot for stateless rules.
// The explicit since bound intentionally bypasses stored offsets; cursor
// continuation still prevents duplicate rows within this single snapshot.
func (c *condorClient) ReadWindow(ctx context.Context, consumer string, since, until time.Time) ([]Signal, error) {
	consumer, err := validateCondorConsumer(consumer)
	if err != nil {
		return nil, err
	}
	if until.IsZero() {
		until = time.Now().UTC()
	}
	result := make([]Signal, 0)
	cursor := ""
	for pages := 0; pages < maxCondorWindowPages && len(result) < maxCondorWindowSignals; pages++ {
		page, err := c.ReadSignals(ctx, CondorReadOptions{Consumer: consumer, Cursor: cursor, Since: since, Until: until, Limit: maxCondorReadLimit})
		if err != nil {
			return nil, err
		}
		if len(result)+len(page.Signals) > maxCondorWindowSignals {
			return nil, fmt.Errorf("Condor window exceeds %d signals", maxCondorWindowSignals)
		}
		result = append(result, page.Signals...)
		if !page.HasMore || page.NextCursor == "" {
			return result, nil
		}
		if page.NextCursor == cursor {
			return nil, errors.New("Condor returned a repeated cursor")
		}
		cursor = page.NextCursor
	}
	return nil, fmt.Errorf("Condor window exceeds %d signals", maxCondorWindowSignals)
}

func (c *condorClient) newContext(operation, idempotency string) *tucunarv1.TenantContext {
	return &tucunarv1.TenantContext{
		ContractVersion: "1.0",
		RequestId:       newRequestID(),
		IdempotencyKey:  idempotency,
		OrganizationId:  c.organizationID,
		CellId:          c.cellID,
		NodeId:          c.nodeID,
		CorrelationId:   operation,
	}
}

func (c *condorClient) authorize(request interface{ Header() http.Header }) {
	if c.token != "" {
		request.Header().Set("Authorization", "Bearer "+c.token)
	}
}

func appendIdempotencyKey(signals []Signal) string {
	if len(signals) == 0 {
		return "append:empty"
	}
	parts := make([]string, 0, len(signals))
	for _, signal := range signals {
		identity := strings.TrimSpace(signal.ID)
		if identity == "" {
			identity = signal.DedupKey()
		}
		parts = append(parts, identity)
	}
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "append:" + hex.EncodeToString(digest[:16])
}
