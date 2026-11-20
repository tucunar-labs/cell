package canarynode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	pigeonMaxChannels = 16
	pigeonMaxMessage  = 32 << 10
	pigeonTimeout     = 5 * time.Second
)

var pigeonChannelName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func init() { registerCapabilityExecutor(capabilityPigeon, newPigeonExecutor) }

func newPigeonExecutor(definition CapabilityDefinition) CapabilityExecutor {
	return newFunctionalCapability(definition, pigeonDispatch)
}

func pigeonDispatch(ctx context.Context, request CapabilityRequest) (any, error) {
	payload, err := decodeCapabilityPayload[PigeonDispatchRequest](request)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(payload.IncidentID) == "" || len(payload.IncidentID) > 256 {
		return nil, errors.New("incident_id is required and bounded")
	}
	if len(payload.Channels) == 0 || len(payload.Channels) > pigeonMaxChannels {
		return nil, errors.New("channels must contain 1 to 16 entries")
	}
	if len(payload.Message) > pigeonMaxMessage {
		return nil, errors.New("message exceeds 32 KiB")
	}
	approved, ok := payload.Payload["approved"].(bool)
	if !ok || !approved {
		return nil, errors.New("incident dispatch requires payload.approved=true")
	}
	for _, channel := range payload.Channels {
		if !pigeonChannelName.MatchString(channel) {
			return nil, fmt.Errorf("invalid channel %q", channel)
		}
	}

	response := PigeonDispatchResponse{CapabilityResult: CapabilityResult{ContractVersion: capabilityContractVersion, RequestID: request.RequestID, Capability: string(capabilityPigeon), Operation: request.Operation, Status: "dry_run"}}
	client := &http.Client{Timeout: pigeonTimeout}
	for _, channel := range payload.Channels {
		id := pigeonDeliveryID(request, payload.IncidentID, channel)
		response.DeliveryIDs = append(response.DeliveryIDs, id)
		url := pigeonWebhookURL(channel)
		if url == "" {
			continue
		}
		response.Attempts++
		callCtx, span := tracer.Start(ctx, "pigeon.webhook", withAttributes(
			spanString("tucunar.channel", channel), spanString("tucunar.target_configured", "true")))
		body, marshalErr := json.Marshal(map[string]any{"incident_id": payload.IncidentID, "message": payload.Message, "payload": payload.Payload})
		if marshalErr != nil {
			span.RecordError(marshalErr)
			span.SetStatus(spanError, "marshal failed")
			span.End()
			return nil, marshalErr
		}
		req, reqErr := http.NewRequestWithContext(callCtx, http.MethodPost, url, bytes.NewReader(body))
		if reqErr != nil {
			span.RecordError(reqErr)
			span.SetStatus(spanError, "request failed")
			span.End()
			return nil, fmt.Errorf("create webhook request: %w", reqErr)
		}
		req.Header.Set("Content-Type", "application/json")
		res, doErr := client.Do(req)
		if doErr != nil {
			span.RecordError(doErr)
			span.SetStatus(spanError, "webhook failed")
			span.End()
			return nil, fmt.Errorf("pigeon webhook failed: %w", doErr)
		}
		_ = res.Body.Close()
		span.SetAttributes(spanInt("http.status_code", res.StatusCode))
		span.End()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return nil, fmt.Errorf("pigeon webhook returned HTTP %d", res.StatusCode)
		}
		response.Status = "delivered"
	}
	return response, nil
}

func pigeonWebhookURL(channel string) string {
	key := "PIGEON_" + strings.ToUpper(strings.NewReplacer("-", "_", " ", "_").Replace(channel)) + "_WEBHOOK_URL"
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv(strings.TrimSuffix(key, "_URL")))
}

func pigeonDeliveryID(request CapabilityRequest, incident, channel string) string {
	h := sha256.Sum256([]byte(request.IdempotencyKey + "\x00" + incident + "\x00" + channel))
	return "pigeon-" + hex.EncodeToString(h[:8])
}
