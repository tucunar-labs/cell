package canarynode

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const (
	defaultTickInterval = time.Minute      // cada 1 minuto
	lookbackWindow      = 15 * time.Minute // ventana de retrospectiva
	conversionThreshold = 0.2              // conversión menor a 20% => alerta
)

// runEvaluator es el motor de reglas sin estado: un Ticker que pide snapshots
// acotados a Condor. No guarda estado de negocio en memoria; toda la resiliencia
// depende de reconstruir la ventana desde el stream durable.
func runEvaluator(ctx context.Context, store *Store, reader condorWindowReader, alertURL string, cloud *cloudReporter, genericRules ...ThresholdRule) {
	analyticalRules, err := LoadAnalyticalRules(env("ANALYTICAL_RULES_FILE", ""))
	if err != nil {
		logger.Error("load analytical rules", "error", err)
		return
	}
	logger.Info("loaded analytical rules", "count", len(analyticalRules))
	// Evaluate once on startup so a restart does not add an avoidable minute of
	// detection delay. The result is still reconstructed entirely from Condor;
	// no incident state is kept in this goroutine.
	evaluateLookback(ctx, store, reader, alertURL, cloud, genericRules, analyticalRules)
	interval := evaluatorInterval()
	logger.Info("evaluator ticker configured", "interval", interval.String())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			evaluateLookback(ctx, store, reader, alertURL, cloud, genericRules, analyticalRules)
		}
	}
}

func evaluatorInterval() time.Duration {
	seconds := intFromEnvBounded("CANARY_EVALUATION_INTERVAL_SECONDS", int(defaultTickInterval/time.Second), 1, 3600)
	return time.Duration(seconds) * time.Second
}

func evaluateLookback(ctx context.Context, store *Store, reader condorWindowReader, alertURL string, cloud *cloudReporter, genericRules []ThresholdRule, analyticalRules []AnalyticalRule) {
	evaluationStarted := time.Now()
	defer func() { nodeRuntimeMetrics.recordEvaluator(time.Since(evaluationStarted)) }()
	ctx, span := tracer.Start(ctx, "Ticker Evaluate_Lookback_Window",
		withAttributes(
			spanInt64("lookback_minutes", int64(lookbackWindow.Minutes())),
			spanFloat64("conversion_threshold", conversionThreshold),
		))
	defer span.End()

	now := time.Now().UTC()
	signals, err := evaluatorWindow(reader, ctx, genericRules, analyticalRules, now)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "Condor snapshot failed")
		return
	}
	conversionAlerts := conversionAlertsFromSignals(signals, lookbackWindow, conversionThreshold, now)
	candidates := make([]IncidentCandidate, 0)
	scopes := make([]string, 0, 1+len(genericRules)+len(analyticalRules))
	scopes = append(scopes, "conversion/")
	for _, alert := range conversionAlerts {
		candidates = append(candidates, IncidentCandidate{
			Key: "conversion/" + alert.ShopID, Scope: "conversion/", RuleID: conversionIncidentRuleID, Family: "conversion",
			Source: "shopify", TenantID: alert.ShopID, EntityID: alert.ShopID, ObservedValue: alert.ConversionRate,
			Threshold: conversionThreshold, Operator: OperatorLT, Window: lookbackWindow,
			RecentSamples: alert.CheckoutStarted, NumeratorSamples: alert.PaymentSuccess,
			Fingerprint: fmt.Sprintf("rate=%.3f", alert.ConversionRate),
		})
	}
	for _, rule := range genericRules {
		thresholdAlerts, err := thresholdAlertsFromSignals(signals, rule, now)
		if err != nil {
			span.RecordError(err)
			continue
		}
		scopes = append(scopes, "threshold/"+rule.ID+"/")
		for _, alert := range thresholdAlerts {
			candidates = append(candidates, IncidentCandidate{
				Key:   "threshold/" + alert.RuleID + "/" + alert.TenantID + "/" + alert.EntityID,
				Scope: "threshold/" + alert.RuleID + "/", RuleID: alert.RuleID, Family: "threshold",
				Source: alert.Source, TenantID: alert.TenantID, EntityID: alert.EntityID, ObservedValue: alert.ObservedValue,
				Threshold: alert.Threshold, Operator: alert.Operator, Window: alert.Window,
				Fingerprint: fmt.Sprintf("observed=%.3f", alert.ObservedValue),
			})
		}
	}

	for _, rule := range analyticalRules {
		analyticalAlerts, err := analyticalAlertsFromSignals(signals, rule, now)
		if err != nil {
			span.RecordError(err)
			continue
		}
		scopes = append(scopes, "analytical/"+rule.ID+"/")
		for _, alert := range analyticalAlerts {
			candidates = append(candidates, mapAnalyticalResult(alert))
		}
	}

	transitions, err := store.ReconcileIncidents(ctx, candidates, scopes)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return
	}
	for _, transition := range transitions {
		delivered := true
		if cloud != nil {
			if err := cloud.enqueueDurableIncident(ctx, transition); err != nil {
				span.RecordError(err)
				delivered = false
			}
		}
		if alertURL == "" {
			logger.Info("incident", "status", transition.Status, "key", transition.Key,
				"rule_id", transition.RuleID, "source", transition.Source,
				"tenant_id", transition.TenantID, "entity_id", transition.EntityID,
				"observed_value", transition.ObservedValue, "threshold", transition.Threshold,
				"attempt", transition.Attempt)
		} else if err := sendIncidentAlert(ctx, alertURL, transition); err != nil {
			span.RecordError(err)
			delivered = false
		}
		if err := store.MarkIncidentDelivery(ctx, transition, delivered); err != nil {
			span.RecordError(err)
		}
	}
}

func sendIncidentAlert(ctx context.Context, alertURL string, transition IncidentTransition) error {
	ctx, span := tracer.Start(ctx, "HTTP POST Incident_Alert_Webhook",
		withAttributes(spanString("http.url", alertURL), spanString("incident.key", transition.Key), spanString("incident.status", transition.Status)))
	defer span.End()
	body, err := json.Marshal(map[string]any{
		"text": "Tucunar operational incident", "incident_key": transition.Key, "status": transition.Status,
		"rule_id": transition.RuleID, "family": transition.Family, "source": transition.Source,
		"tenant_id": transition.TenantID, "entity_id": transition.EntityID,
		"observed_value": transition.ObservedValue, "baseline_value": transition.BaselineValue,
		"operator": transition.Operator, "threshold": transition.Threshold,
		"window_seconds": int64(transition.Window.Seconds()), "recent_samples": transition.RecentSamples,
		"baseline_samples": transition.BaselineSamples, "numerator_samples": transition.NumeratorSamples,
		"denominator_samples": transition.DenominatorSamples,
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, alertURL, bytes.NewReader(body))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	setAlertWebhookHeaders(req, body, transition.Key+":"+transition.Status+":"+transition.FirstSeenAt.Format(time.RFC3339Nano))
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	defer resp.Body.Close()
	span.SetAttributes(spanInt("http.status_code", resp.StatusCode))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		err := fmt.Errorf("incident webhook returned HTTP %d", resp.StatusCode)
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	return nil
}

func setAlertWebhookHeaders(req *http.Request, body []byte, idempotencyKey string) {
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	secret := env("ALERT_WEBHOOK_SECRET", "")
	if secret == "" {
		return
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	req.Header.Set("X-Tucunar-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-Tucunar-Event", "incident")
}

func sendThresholdAlert(ctx context.Context, alertURL string, alert ThresholdAlert) error {
	ctx, span := tracer.Start(ctx, "HTTP POST Threshold_Alert_Webhook",
		withAttributes(
			spanString("http.url", alertURL),
			spanString("rule.id", alert.RuleID),
			spanString("rule.source", alert.Source),
			spanString("tenant.id", alert.TenantID),
			spanString("entity.id", alert.EntityID),
			spanFloat64("rule.observed_value", alert.ObservedValue),
			spanFloat64("rule.threshold", alert.Threshold),
			spanInt64("rule.window_seconds", int64(alert.Window.Seconds())),
		))
	defer span.End()

	body, err := json.Marshal(map[string]any{
		"text":           "Tucunar threshold rule triggered",
		"rule_id":        alert.RuleID,
		"source":         alert.Source,
		"tenant_id":      alert.TenantID,
		"entity_id":      alert.EntityID,
		"aggregate":      alert.Aggregate,
		"observed_value": alert.ObservedValue,
		"operator":       alert.Operator,
		"threshold":      alert.Threshold,
		"window_seconds": int64(alert.Window.Seconds()),
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, alertURL, bytes.NewReader(body))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	setAlertWebhookHeaders(req, body, alert.RuleID+":"+alert.TenantID+":"+alert.EntityID)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	defer resp.Body.Close()

	span.SetAttributes(spanInt("http.status_code", resp.StatusCode))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		err := fmt.Errorf("alert webhook returned HTTP %d", resp.StatusCode)
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	return nil
}

// sendAlert simula el webhook de Slack. La llamada de salida genera su propio span.
func sendAlert(ctx context.Context, alertURL string, alert ConversionAlert) error {
	ctx, span := tracer.Start(ctx, "HTTP POST Slack_Alert_Webhook",
		withAttributes(
			spanString("http.url", alertURL),
			spanString("shop_id", alert.ShopID),
			spanFloat64("conversion_rate", alert.ConversionRate),
		))
	defer span.End()

	body, err := json.Marshal(map[string]any{
		"text":             "⚠️ Checkout conversion below 20%",
		"shop_id":          alert.ShopID,
		"checkout_started": alert.CheckoutStarted,
		"payment_success":  alert.PaymentSuccess,
		"conversion_rate":  alert.ConversionRate,
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, alertURL, bytes.NewReader(body))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	setAlertWebhookHeaders(req, body, "conversion:"+alert.ShopID)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	defer resp.Body.Close()

	span.SetAttributes(spanInt("http.status_code", resp.StatusCode))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		err := fmt.Errorf("alert webhook returned HTTP %d", resp.StatusCode)
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	return nil
}
