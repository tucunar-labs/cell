package canarynode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	cloudMaxAttempts       = 5
	cloudRetryBudget       = 30 * time.Second
	cloudResponseBodyLimit = 1 << 20
	cloudAccessTokenSkew   = 5 * time.Minute
)

type cloudConfig struct {
	BaseURL             string
	EnrollmentToken     string
	OrganizationID      string
	CanaryID            string
	DeploymentMode      string
	NodeInstanceID      string
	NodeVersion         string
	StateFile           string
	AllowInsecureHTTP   bool
	HeartbeatInterval   time.Duration
	RollupInterval      time.Duration
	OutboxFlushInterval time.Duration
	MaxPendingOutbox    int
}

type cloudState struct {
	NodeInstanceID       string    `json:"node_instance_id"`
	OrganizationID       string    `json:"organization_id"`
	CanaryID             string    `json:"canary_id"`
	NodeID               string    `json:"node_id"`
	AccessToken          string    `json:"access_token"`
	AccessTokenExpiresAt time.Time `json:"access_token_expires_at"`
	EnrollmentTokenHash  string    `json:"enrollment_token_hash,omitempty"`
}

type cloudClient struct {
	baseURL string
	http    *http.Client
	sleep   func(time.Duration)
	now     func() time.Time
}

type cloudHTTPError struct {
	StatusCode   int
	ResponseBody string
	RetryAfter   time.Duration
	Retryable    bool
}

func (e *cloudHTTPError) Error() string {
	if e.StatusCode == 0 {
		return "cloud control API request failed"
	}
	if e.ResponseBody != "" {
		return fmt.Sprintf("cloud control API returned HTTP %d: %s", e.StatusCode, e.ResponseBody)
	}
	return fmt.Sprintf("cloud control API returned HTTP %d", e.StatusCode)
}

func loadCloudConfig() (*cloudConfig, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("TUCUNAR_API_BASE_URL")), "/")
	deploymentMode := env("CANARY_DEPLOYMENT_MODE", "on_premise")
	if deploymentMode != "cloud" && deploymentMode != "on_premise" {
		return nil, fmt.Errorf("CANARY_DEPLOYMENT_MODE must be cloud or on_premise")
	}
	if baseURL == "" {
		if deploymentMode == "cloud" {
			return nil, fmt.Errorf("cloud deployment requires TUCUNAR_API_BASE_URL")
		}
		return nil, nil
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("TUCUNAR_API_BASE_URL must be an absolute URL")
	}
	allowInsecure := env("TUCUNAR_API_ALLOW_INSECURE_HTTP", "") == "1"
	if parsed.Scheme != "https" && !allowInsecure {
		return nil, fmt.Errorf("TUCUNAR_API_BASE_URL must use HTTPS")
	}
	organizationID := strings.TrimSpace(os.Getenv("CANARY_ORGANIZATION_ID"))
	canaryID := strings.TrimSpace(os.Getenv("CANARY_ID"))
	if !validUUID(organizationID) || !validUUID(canaryID) {
		return nil, fmt.Errorf("CANARY_ORGANIZATION_ID and CANARY_ID must be UUIDs")
	}
	return &cloudConfig{
		BaseURL:             baseURL,
		EnrollmentToken:     strings.TrimSpace(os.Getenv("TUCUNAR_ENROLLMENT_TOKEN")),
		OrganizationID:      organizationID,
		CanaryID:            canaryID,
		DeploymentMode:      deploymentMode,
		NodeInstanceID:      strings.TrimSpace(os.Getenv("CANARY_NODE_INSTANCE_ID")),
		NodeVersion:         currentNodeVersion(),
		StateFile:           env("TUCUNAR_CELL_STATE_FILE", "./tucunar-cell-state.json"),
		AllowInsecureHTTP:   allowInsecure,
		HeartbeatInterval:   durationFromEnv("CANARY_HEARTBEAT_INTERVAL_SECONDS", time.Minute),
		RollupInterval:      durationFromEnv("CANARY_ROLLUP_INTERVAL_SECONDS", time.Minute),
		OutboxFlushInterval: durationFromEnv("CANARY_OUTBOX_FLUSH_INTERVAL_SECONDS", 5*time.Second),
		MaxPendingOutbox:    intFromEnv("CANARY_MAX_PENDING_REPORTS", 10000),
	}, nil
}

func validUUID(value string) bool {
	_, err := uuid.Parse(value)
	return err == nil
}

func durationFromEnv(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 1 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

func intFromEnv(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	number, err := strconv.Atoi(value)
	if err != nil || number < 1 {
		return fallback
	}
	return number
}

func newCloudClient(config *cloudConfig) *cloudClient {
	return &cloudClient{
		baseURL: strings.TrimRight(config.BaseURL, "/"),
		http:    &http.Client{},
		sleep:   time.Sleep,
		now:     time.Now,
	}
}

func (c *cloudClient) postJSON(ctx context.Context, path, token, idempotencyKey string, body []byte, timeout time.Duration) ([]byte, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, errors.New("cloud API path must start with /")
	}
	if !validIdempotencyKey(idempotencyKey) {
		return nil, errors.New("cloud API idempotency key must be 1-128 ASCII characters")
	}
	if len(body) > cloudResponseBodyLimit {
		return nil, fmt.Errorf("cloud control API request too large")
	}
	now := c.now
	if now == nil {
		now = time.Now
	}
	deadline := now().Add(cloudRetryBudget)
	for attempt := 0; attempt < cloudMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		client := c.http
		if client == nil {
			client = http.DefaultClient
		}
		remaining := deadline.Sub(now())
		if remaining <= 0 {
			return nil, context.DeadlineExceeded
		}
		attemptTimeout := timeout
		if attemptTimeout <= 0 || attemptTimeout > remaining {
			attemptTimeout = remaining
		}
		requestCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
		if err != nil {
			cancel()
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", idempotencyKey)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, requestErr := client.Do(req)
		if requestErr != nil {
			cancel()
			if attempt == cloudMaxAttempts-1 || now().After(deadline) {
				return nil, requestErr
			}
			if err := c.waitForRetry(ctx, attempt, 0, deadline); err != nil {
				return nil, requestErr
			}
			continue
		}
		responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, cloudResponseBodyLimit+1))
		resp.Body.Close()
		cancel()
		if readErr != nil {
			if attempt == cloudMaxAttempts-1 || now().After(deadline) {
				return nil, readErr
			}
			if err := c.waitForRetry(ctx, attempt, 0, deadline); err != nil {
				return nil, readErr
			}
			continue
		}
		if len(responseBody) > cloudResponseBodyLimit {
			return nil, fmt.Errorf("cloud control API response too large")
		}
		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			return responseBody, nil
		}
		httpErr := &cloudHTTPError{
			StatusCode:   resp.StatusCode,
			ResponseBody: strings.TrimSpace(string(responseBody)),
			RetryAfter:   parseRetryAfter(resp.Header.Get("Retry-After"), now()),
			Retryable:    isRetryableCloudStatus(resp.StatusCode),
		}
		if len(httpErr.ResponseBody) > 512 {
			httpErr.ResponseBody = httpErr.ResponseBody[:512]
		}
		if !httpErr.Retryable || attempt == cloudMaxAttempts-1 || now().After(deadline) {
			return nil, httpErr
		}
		if err := c.waitForRetry(ctx, attempt, httpErr.RetryAfter, deadline); err != nil {
			return nil, httpErr
		}
	}
	return nil, errors.New("cloud control API retry budget exhausted")
}

func (c *cloudClient) waitForRetry(ctx context.Context, attempt int, retryAfter time.Duration, deadline time.Time) error {
	delay := time.Duration(1<<attempt) * time.Second
	if retryAfter > delay {
		delay = retryAfter
	}
	if remaining := time.Until(deadline); delay > remaining {
		return context.DeadlineExceeded
	}
	if c.sleep != nil {
		c.sleep(delay)
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isRetryableCloudStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 && status <= 504
}

func validIdempotencyKey(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] > 0x7f {
			return false
		}
	}
	return true
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil && when.After(now) {
		return time.Until(when)
	}
	return 0
}

func stableUUID(parts ...string) string {
	hash := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	bytes := hash[:16]
	// Keep the deterministic value RFC 4122-shaped so APIs that validate UUID
	// version and variant bits can accept it as an emission id.
	bytes[6] = (bytes[6] & 0x0f) | 0x50
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(bytes)
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[0:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:32])
}

func loadCloudState(path string) (cloudState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cloudState{}, nil
	}
	if err != nil {
		return cloudState{}, fmt.Errorf("read cloud state: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return cloudState{}, fmt.Errorf("stat cloud state: %w", err)
	}
	// Windows does not expose NTFS ACLs through FileMode.Perm(). The state
	// file is still created with 0600 where supported; operators on Windows
	// should enforce an equivalent user-only ACL on the containing directory.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return cloudState{}, errors.New("cloud state file must not be readable by group or others")
	}
	var state cloudState
	if err := json.Unmarshal(data, &state); err != nil {
		return cloudState{}, fmt.Errorf("decode cloud state: %w", err)
	}
	if state.NodeInstanceID != "" && !validUUID(state.NodeInstanceID) {
		return cloudState{}, errors.New("cloud state has invalid node_instance_id")
	}
	return state, nil
}

func saveCloudState(path string, state cloudState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode cloud state: %w", err)
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return fmt.Errorf("create cloud state directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".tucunar-cell-state-*")
	if err != nil {
		return fmt.Errorf("create cloud state temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return fmt.Errorf("protect cloud state: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write cloud state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync cloud state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close cloud state: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("replace cloud state: %w", err)
	}
	return nil
}

func (c *cloudClient) startSpan(ctx context.Context, name string, path string) (context.Context, *span) {
	return tracer.Start(ctx, name, withAttributes(
		spanString("http.method", http.MethodPost),
		spanString("http.route", path),
		spanString("cloud.control_plane", "canary"),
	))
}

func recordCloudError(span *span, err error) {
	span.RecordError(err)
	span.SetStatus(spanError, err.Error())
}

type cloudReporter struct {
	store  *Store
	config cloudConfig
	client *cloudClient
	start  time.Time

	mu    sync.RWMutex
	state cloudState
}

func newCloudReporter(store *Store, config *cloudConfig) (*cloudReporter, error) {
	if config == nil {
		return nil, nil
	}
	state, err := loadCloudState(config.StateFile)
	if err != nil {
		return nil, err
	}
	if config.NodeInstanceID != "" {
		if !validUUID(config.NodeInstanceID) {
			return nil, errors.New("CANARY_NODE_INSTANCE_ID must be a UUID")
		}
		if state.NodeInstanceID != "" && state.NodeInstanceID != config.NodeInstanceID && state.AccessToken != "" {
			return nil, errors.New("CANARY_NODE_INSTANCE_ID does not match enrolled cloud state")
		}
		state.NodeInstanceID = config.NodeInstanceID
	}
	if state.NodeInstanceID == "" {
		state.NodeInstanceID = newID()
		if err := saveCloudState(config.StateFile, state); err != nil {
			return nil, err
		}
	}
	if state.OrganizationID != "" && state.OrganizationID != config.OrganizationID {
		return nil, errors.New("cloud state organization_id does not match configuration")
	}
	if state.CanaryID != "" && state.CanaryID != config.CanaryID {
		return nil, errors.New("cloud state canary_id does not match configuration")
	}
	if store != nil {
		store.SetCloudNodeID(state.NodeID)
	}
	return &cloudReporter{store: store, config: *config, client: newCloudClient(config), start: time.Now().UTC(), state: state}, nil
}

func (r *cloudReporter) accessToken() (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.state.AccessToken == "" || (!r.state.AccessTokenExpiresAt.IsZero() && time.Now().UTC().Add(cloudAccessTokenSkew).After(r.state.AccessTokenExpiresAt)) {
		return "", false
	}
	return r.state.AccessToken, true
}

func (r *cloudReporter) enrolled() bool {
	_, ok := r.accessToken()
	return ok
}

func (r *cloudReporter) ensureEnrolled(ctx context.Context) error {
	if r.enrolled() {
		return nil
	}
	if r.config.EnrollmentToken == "" {
		return errors.New("cloud reporter is not enrolled; TUCUNAR_ENROLLMENT_TOKEN is required")
	}
	tokenHash := hashEnrollmentToken(r.config.EnrollmentToken)
	r.mu.RLock()
	instanceID := r.state.NodeInstanceID
	previousTokenHash := r.state.EnrollmentTokenHash
	r.mu.RUnlock()
	if previousTokenHash != "" && subtle.ConstantTimeCompare([]byte(previousTokenHash), []byte(tokenHash)) == 1 {
		return errors.New("enrollment token was already consumed or rejected; provide a new token")
	}
	request := enrollmentRequest{
		ContractVersion: cloudContractVersion,
		OrganizationID:  r.config.OrganizationID,
		CanaryID:        r.config.CanaryID,
		DeploymentMode:  r.config.DeploymentMode,
		NodeInstanceID:  instanceID,
		NodeVersion:     r.config.NodeVersion,
		Capabilities:    []string{"shopify_webhooks", "mercadopago_payments", "clickhouse_rollups", "incidents"},
	}
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	ctx, span := r.client.startSpan(ctx, "HTTP POST /v1/nodes/enroll", "/v1/nodes/enroll")
	defer span.End()
	responseBody, err := r.client.postJSON(ctx, "/v1/nodes/enroll", r.config.EnrollmentToken, "enroll:"+instanceID, body, 10*time.Second)
	if err != nil {
		recordCloudError(span, err)
		if permanentCloudError(err) {
			if rememberErr := r.rememberEnrollmentToken(tokenHash); rememberErr != nil {
				recordCloudError(span, rememberErr)
			}
		}
		return err
	}
	var response enrollmentResponse
	if err := json.Unmarshal(responseBody, &response); err != nil {
		recordCloudError(span, err)
		return fmt.Errorf("decode enrollment response: %w", err)
	}
	expiresAt, err := time.Parse(time.RFC3339, response.AccessTokenExpiresAt)
	if err != nil || expiresAt.Before(time.Now().UTC()) || response.AccessToken == "" || response.NodeID == "" || response.OrganizationID != r.config.OrganizationID || response.CanaryID != r.config.CanaryID || response.DeploymentMode != r.config.DeploymentMode || response.RawEventForwarding {
		return errors.New("invalid enrollment response")
	}
	if !validUUID(response.NodeID) {
		return errors.New("enrollment response has invalid node_id")
	}
	state := cloudState{
		NodeInstanceID:       instanceID,
		OrganizationID:       response.OrganizationID,
		CanaryID:             response.CanaryID,
		NodeID:               response.NodeID,
		AccessToken:          response.AccessToken,
		AccessTokenExpiresAt: expiresAt,
		EnrollmentTokenHash:  tokenHash,
	}
	if err := saveCloudState(r.config.StateFile, state); err != nil {
		recordCloudError(span, err)
		return err
	}
	r.mu.Lock()
	r.state = state
	if r.store != nil {
		r.store.SetCloudNodeID(response.NodeID)
	}
	if response.Reporting.HeartbeatIntervalSeconds > 0 {
		r.config.HeartbeatInterval = time.Duration(response.Reporting.HeartbeatIntervalSeconds) * time.Second
	}
	if response.Reporting.RollupIntervalSeconds > 0 {
		r.config.RollupInterval = time.Duration(response.Reporting.RollupIntervalSeconds) * time.Second
	}
	r.mu.Unlock()
	span.SetAttributes(spanString("node.id", response.NodeID))
	return nil
}

func (r *cloudReporter) identity() cloudState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.state
}

func hashEnrollmentToken(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}

func (r *cloudReporter) rememberEnrollmentToken(tokenHash string) error {
	r.mu.Lock()
	state := r.state
	state.EnrollmentTokenHash = tokenHash
	r.state = state
	r.mu.Unlock()
	return saveCloudState(r.config.StateFile, state)
}
