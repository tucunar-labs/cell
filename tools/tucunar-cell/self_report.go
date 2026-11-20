package canarynode

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// selfReport is the node's built-in operational contract. It deliberately
// contains process health and bounded counters, not raw signals, credentials,
// or a second telemetry protocol. Operators and k0s can inspect it directly.
type selfReport struct {
	Service       string                 `json:"service"`
	Version       string                 `json:"version"`
	GeneratedAt   time.Time              `json:"generated_at"`
	UptimeSeconds int64                  `json:"uptime_seconds"`
	Ready         bool                   `json:"ready"`
	Dependencies  map[string]string      `json:"dependencies,omitempty"`
	Runtime       runtimeMetricsSnapshot `json:"runtime"`
}

func writeSelfReport(w http.ResponseWriter, r *http.Request, service string, store *Store) {
	report := selfReport{
		Service:      service,
		Version:      currentNodeVersion(),
		GeneratedAt:  time.Now().UTC(),
		Runtime:      nodeRuntimeMetrics.snapshot(),
		Dependencies: map[string]string{},
	}
	report.UptimeSeconds = report.Runtime.UptimeSeconds

	status := http.StatusOK
	if store != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := store.Ready(ctx); err != nil {
			report.Dependencies["clickhouse"] = "unavailable"
			report.Ready = false
			status = http.StatusServiceUnavailable
		} else {
			report.Dependencies["clickhouse"] = "ready"
			report.Ready = true
		}
	} else {
		report.Ready = true
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(report)
}
