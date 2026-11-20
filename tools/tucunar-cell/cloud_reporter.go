package canarynode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

const conversionIncidentRuleID = "checkout_conversion_below_threshold"

// Run keeps all cloud communication outbound. It never handles an inbound
// control-plane request and it keeps only credentials/configuration in memory;
// reports and incident state live in ClickHouse.
func (r *cloudReporter) Run(ctx context.Context) {
	if err := r.ensureEnrolled(ctx); err != nil {
		logger.Warn("cloud enrollment pending", "error", err,
			"organization_id", r.config.OrganizationID, "canary_id", r.config.CanaryID)
	}
	heartbeatTicker := time.NewTicker(r.heartbeatInterval())
	defer heartbeatTicker.Stop()
	rollupTicker := time.NewTicker(r.rollupInterval())
	defer rollupTicker.Stop()
	flushTicker := time.NewTicker(r.flushInterval())
	defer flushTicker.Stop()
	if r.enrolled() {
		r.enqueueHeartbeat(ctx)
		r.enqueueRollup(ctx)
		r.flushPending(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeatTicker.C:
			if !r.enrolled() {
				if err := r.ensureEnrolled(ctx); err != nil {
					logger.Warn("cloud enrollment pending", "error", err,
						"organization_id", r.config.OrganizationID, "canary_id", r.config.CanaryID)
					continue
				}
			}
			r.enqueueHeartbeat(ctx)
			r.flushPending(ctx)
		case <-rollupTicker.C:
			if !r.enrolled() {
				if err := r.ensureEnrolled(ctx); err != nil {
					logger.Warn("cloud enrollment pending", "error", err,
						"organization_id", r.config.OrganizationID, "canary_id", r.config.CanaryID)
					continue
				}
			}
			r.enqueueRollup(ctx)
			r.flushPending(ctx)
		case <-flushTicker.C:
			if r.enrolled() {
				r.flushPending(ctx)
			}
		}
	}
}

func (r *cloudReporter) heartbeatInterval() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.config.HeartbeatInterval < time.Second {
		return time.Minute
	}
	return r.config.HeartbeatInterval
}

func (r *cloudReporter) rollupInterval() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.config.RollupInterval < time.Second {
		return time.Minute
	}
	return r.config.RollupInterval
}

func (r *cloudReporter) flushInterval() time.Duration {
	if r.config.OutboxFlushInterval < time.Second {
		return 5 * time.Second
	}
	return r.config.OutboxFlushInterval
}

func (r *cloudReporter) enqueueHeartbeat(ctx context.Context) {
	identity := r.identity()
	now := time.Now().UTC().Truncate(time.Millisecond)
	status, components, data := r.heartbeatHealth(ctx)
	heartbeat := heartbeatRequest{
		ContractVersion: cloudContractVersion,
		OrganizationID:  identity.OrganizationID,
		CanaryID:        identity.CanaryID,
		NodeID:          identity.NodeID,
		SentAt:          now,
		NodeVersion:     r.config.NodeVersion,
		Status:          status,
		UptimeSeconds:   int64(time.Since(r.start).Seconds()),
		Components:      components,
		Data:            data,
	}
	if err := r.enqueue(ctx, "heartbeat", "/v1/canaries/"+identity.CanaryID+"/heartbeat", "heartbeat:"+identity.NodeID+":"+now.Format(time.RFC3339Nano), heartbeat); err != nil {
		logger.Warn("enqueue cloud heartbeat", "error", err,
			"organization_id", identity.OrganizationID, "canary_id", identity.CanaryID)
	}
}

// heartbeatHealth turns local dependency state into a small cloud-safe
// summary. It deliberately sends no raw signal data and lets the app
// distinguish a healthy node from one that is alive but unable to evaluate or
// drain its durable outbound queue.
func (r *cloudReporter) heartbeatHealth(ctx context.Context) (string, map[string]string, heartbeatData) {
	status := "ready"
	components := map[string]string{
		"http_ingest": "ready",
		"clickhouse":  "ready",
		"evaluator":   "running",
		"outbox":      "ready",
	}
	data := heartbeatData{LookbackSeconds: int(lookbackWindow.Seconds()), OutboxLimit: r.config.MaxPendingOutbox}
	if err := r.store.Ready(ctx); err != nil {
		status = "unhealthy"
		components["clickhouse"] = "unhealthy"
		components["evaluator"] = "paused"
	} else if stats, err := r.store.CloudOutboxDiagnostics(ctx, r.identity().NodeID); err != nil {
		status = "degraded"
		components["outbox"] = "unknown"
	} else {
		data.OutboxPending = stats.Pending
		data.OutboxFailed = stats.Failed
		if r.config.MaxPendingOutbox > 0 && stats.Pending >= uint64(r.config.MaxPendingOutbox) {
			status = "degraded"
			components["outbox"] = "saturated"
		} else if r.config.MaxPendingOutbox > 0 && stats.Pending >= uint64(r.config.MaxPendingOutbox)*8/10 {
			status = "degraded"
			components["outbox"] = "near_limit"
		} else if stats.Failed > 0 {
			status = "degraded"
			components["outbox"] = "failed_records"
		}
	}
	return status, components, data
}

func (r *cloudReporter) enqueueRollup(ctx context.Context) {
	identity := r.identity()
	now := time.Now().UTC().Truncate(time.Millisecond)
	metrics, err := r.store.DashboardMetrics(ctx, lookbackWindow)
	if err != nil {
		logger.Warn("build cloud rollup", "error", err,
			"organization_id", identity.OrganizationID, "canary_id", identity.CanaryID)
		return
	}
	rollup := makeRollup(identity, now, metrics)
	if overview, overviewErr := r.store.SignalOverview(ctx, lookbackWindow); overviewErr != nil {
		logger.Warn("build generic cloud rollup", "error", overviewErr,
			"organization_id", identity.OrganizationID, "canary_id", identity.CanaryID)
	} else {
		rollup.Summary.TotalSignals = overview.TotalSignals
		rollup.Summary.SourceCount = overview.SourceCount
		rollup.Summary.EntityCount = overview.EntityCount
		rollup.Signals = makeSignalRollups(overview.Summaries)
	}
	if err := r.enqueue(ctx, "rollup", "/v1/canaries/"+identity.CanaryID+"/rollups", "rollup:"+rollup.RollupID, rollup); err != nil {
		logger.Warn("enqueue cloud rollup", "error", err,
			"organization_id", identity.OrganizationID, "canary_id", identity.CanaryID)
	}
}

func makeSignalRollups(summaries []signalSummary) []signalRollup {
	result := make([]signalRollup, 0, len(summaries))
	for _, summary := range summaries {
		result = append(result, signalRollup{
			Source: summary.Source, TenantID: summary.TenantID, EntityID: summary.EntityID,
			Kind: summary.Kind, Name: summary.Name, Unit: summary.Unit, Samples: summary.Samples,
			AverageValue: summary.AverageValue, LastOccurredAt: summary.LastOccurredAt,
		})
	}
	return result
}

func makeRollup(identity cloudState, now time.Time, metrics []ConversionAlert) rollupRequest {
	shops := make([]shopRollup, 0, len(metrics))
	summary := rollupSummary{TotalShops: len(metrics)}
	for _, metric := range metrics {
		status := "healthy"
		if metric.ConversionRate < conversionThreshold {
			summary.AtRiskShops++
			status = "at_risk"
		}
		summary.CheckoutStarted += metric.CheckoutStarted
		summary.PaymentSuccess += metric.PaymentSuccess
		shops = append(shops, shopRollup{ShopID: metric.ShopID, CheckoutStarted: metric.CheckoutStarted, PaymentSuccess: metric.PaymentSuccess, ConversionRate: metric.ConversionRate, Status: status})
	}
	if summary.CheckoutStarted > 0 {
		summary.ConversionRate = float64(summary.PaymentSuccess) / float64(summary.CheckoutStarted)
	}
	return rollupRequest{
		ContractVersion: cloudContractVersion,
		OrganizationID:  identity.OrganizationID,
		CanaryID:        identity.CanaryID,
		NodeID:          identity.NodeID,
		RollupID:        newID(),
		GeneratedAt:     now,
		Window:          cloudWindow{Start: now.Add(-lookbackWindow), End: now, Seconds: int(lookbackWindow.Seconds())},
		RuleSetVersion:  "conversion-v1",
		Summary:         summary,
		Shops:           shops,
		Signals:         make([]signalRollup, 0),
	}
}

func (r *cloudReporter) enqueue(ctx context.Context, operation, path, idempotencyKey string, value any) error {
	if !r.enrolled() {
		return errors.New("cloud node is not enrolled")
	}
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return r.store.EnqueueCloudMessage(ctx, cloudOutboxRecord{
		OutboxID:       newID(),
		NodeID:         r.identity().NodeID,
		Operation:      operation,
		Path:           path,
		IdempotencyKey: idempotencyKey,
		Payload:        string(body),
	}, r.config.MaxPendingOutbox)
}

func (r *cloudReporter) flushPending(ctx context.Context) {
	records, err := r.store.PendingCloudMessages(ctx, r.identity().NodeID, 100)
	if err != nil {
		identity := r.identity()
		logger.Warn("read cloud outbox", "error", err,
			"organization_id", identity.OrganizationID, "canary_id", identity.CanaryID)
		return
	}
	nodeRuntimeMetrics.recordOutboxPending(len(records))
	token, ok := r.accessToken()
	if !ok {
		return
	}
	for _, record := range records {
		requestTimeout := 15 * time.Second
		if record.Operation == "heartbeat" {
			requestTimeout = 10 * time.Second
		}
		requestCtx, span := tracer.Start(ctx, "HTTP POST cloud report", withAttributes(
			spanString("http.method", http.MethodPost),
			spanString("http.route", record.Path),
			spanString("cloud.operation", record.Operation),
		))
		_, requestErr := r.client.postJSON(requestCtx, record.Path, token, record.IdempotencyKey, []byte(record.Payload), requestTimeout)
		if requestErr == nil {
			if err := r.store.MarkCloudMessageSent(ctx, record); err != nil {
				recordCloudError(span, err)
				span.End()
				// The report was accepted by the control plane but its durable
				// acknowledgement could not be persisted locally. Stop here so a
				// later report cannot overtake this one; the idempotency key makes
				// the next retry safe.
				return
			}
			span.End()
			continue
		}
		recordCloudError(span, requestErr)
		logger.Warn("send cloud report", "operation", record.Operation, "path", record.Path, "error", requestErr)
		if cloudAuthError(requestErr) {
			// A revoked/expired node token must not permanently fail every
			// queued report. Keep the record pending, clear the local token and
			// let the next enrollment (with a newly issued one-time token)
			// resume delivery using the same idempotency key.
			if err := r.invalidateAccessToken(); err != nil {
				recordCloudError(span, err)
			}
			next := time.Now().UTC().Add(outboxBackoff(record.Attempts))
			if err := r.store.RetryCloudMessage(ctx, record, next, requestErr.Error()); err != nil {
				recordCloudError(span, err)
			}
			span.End()
			return
		}
		if permanentCloudError(requestErr) {
			if err := r.store.FailCloudMessage(ctx, record, requestErr.Error()); err != nil {
				recordCloudError(span, err)
			}
		} else {
			next := time.Now().UTC().Add(outboxBackoff(record.Attempts))
			if err := r.store.RetryCloudMessage(ctx, record, next, requestErr.Error()); err != nil {
				recordCloudError(span, err)
			}
			// Preserve logical ordering while the control plane is down. A
			// later record must not be delivered ahead of this retry.
			span.End()
			return
		}
		span.End()
	}
}

func cloudAuthError(err error) bool {
	var httpErr *cloudHTTPError
	return errors.As(err, &httpErr) && (httpErr.StatusCode == http.StatusUnauthorized || httpErr.StatusCode == http.StatusForbidden)
}

func (r *cloudReporter) invalidateAccessToken() error {
	r.mu.Lock()
	state := r.state
	state.AccessToken = ""
	state.AccessTokenExpiresAt = time.Time{}
	r.state = state
	stateFile := r.config.StateFile
	r.mu.Unlock()
	return saveCloudState(stateFile, state)
}

func permanentCloudError(err error) bool {
	var httpErr *cloudHTTPError
	return errors.As(err, &httpErr) && !httpErr.Retryable
}

func outboxBackoff(attempts uint32) time.Duration {
	if attempts > 9 {
		attempts = 9
	}
	return time.Duration(1<<attempts) * time.Second
}
