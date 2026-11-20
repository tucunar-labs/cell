package canarynode

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

const maxKingfisherObservations = 256

func init() {
	registerCapabilityExecutor(capabilityKingfisher, func(d CapabilityDefinition) CapabilityExecutor { return newFunctionalCapability(d, runKingfisher) })
}

func runKingfisher(ctx context.Context, request CapabilityRequest) (any, error) {
	p, err := decodeCapabilityPayload[KingfisherProbeRequest](request)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Target) == "" && len(p.Observations) == 0 {
		return nil, fmt.Errorf("target or observations are required")
	}
	result := KingfisherProbeResponse{CapabilityResult: CapabilityResult{ContractVersion: capabilityContractVersion, RequestID: request.RequestID, Capability: string(capabilityKingfisher), Operation: "probe", Status: "completed"}}
	obs := p.Observations
	if len(obs) > maxKingfisherObservations {
		obs = obs[:maxKingfisherObservations]
	}
	if len(obs) > 0 {
		return kingfisherFromObservations(result, obs), nil
	}
	timeout := time.Duration(p.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	ctx, span := tracer.Start(ctx, "kingfisher.network_probe", traceAttrs(p.Target))
	defer span.End()
	client := &http.Client{Timeout: timeout}
	method := http.MethodGet
	if strings.EqualFold(p.Protocol, "head") {
		method = http.MethodHead
	}
	req, err := http.NewRequestWithContext(ctx, method, p.Target, nil)
	if err != nil {
		return nil, fmt.Errorf("create probe request: %w", err)
	}
	start := time.Now()
	resp, err := client.Do(req)
	elapsed := float64(time.Since(start).Microseconds()) / 1000
	result.LatencyMS, result.AvailabilityPercent, result.LossPercent = elapsed, 0, 0
	if err != nil {
		result.ProbeStatus, result.AvailabilityPercent, result.LossPercent = "unavailable", 0, 100
		return result, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		result.ProbeStatus = "available"
		result.AvailabilityPercent = 100
	} else {
		result.ProbeStatus = "unavailable"
		result.AvailabilityPercent = 0
		result.LossPercent = 100
	}
	span.SetAttributes(spanInt("http.status_code", resp.StatusCode))
	return result, nil
}

func traceAttrs(target string) spanStartEventOption {
	return withAttributes(spanString("tucunar.target", target))
}

func kingfisherFromObservations(result KingfisherProbeResponse, observations []map[string]any) KingfisherProbeResponse {
	available, latencies := 0, []float64{}
	for _, o := range observations {
		ok := false
		if v, exists := o["available"].(bool); exists {
			ok = v
		} else if v, exists := o["reachable"].(bool); exists {
			ok = v
		} else if code, exists := number(o["status_code"]); exists {
			ok = code >= 200 && code < 400
		}
		if ok {
			available++
		}
		if v, exists := number(o["latency_ms"]); exists && v >= 0 {
			latencies = append(latencies, v)
		}
	}
	if len(observations) > 0 {
		result.AvailabilityPercent = float64(available) * 100 / float64(len(observations))
		result.LossPercent = 100 - result.AvailabilityPercent
	}
	if len(latencies) > 0 {
		sort.Float64s(latencies)
		result.LatencyMS = latencies[len(latencies)/2]
	}
	if result.AvailabilityPercent == 100 {
		result.ProbeStatus = "available"
	} else if result.AvailabilityPercent == 0 {
		result.ProbeStatus = "unavailable"
	} else {
		result.ProbeStatus = "degraded"
	}
	return result
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}
