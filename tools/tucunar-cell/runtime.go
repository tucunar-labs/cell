package canarynode

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// env reads a process configuration value without making callers repeat the
// empty-value fallback. Each bird is configured independently by its
// deployment; no process-local registry is required.
func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// RunEvaluator starts the stateless lookback evaluator. All operational state
// is read from ClickHouse, so restarting this process does not lose incidents.
func RunEvaluator(ctx context.Context) error {
	if err := validateRuntimeEnvironment(); err != nil {
		return err
	}
	if err := validateAirGappedEnvironment(); err != nil {
		return err
	}
	store, err := newStore(ctx, clickHouseAddr(), clickHouseUser(), clickHousePassword(), clickHouseDB())
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.InitCloudSchema(ctx); err != nil {
		return fmt.Errorf("init evaluator schema: %w", err)
	}
	cloudConfig, err := loadCloudConfig()
	if err != nil {
		return err
	}
	var cloudReporter *cloudReporter
	if cloudConfig != nil {
		// The evaluator only appends durable outbox records in ClickHouse. The
		// hummingbird is the process that flushes those records over HTTPS.
		cloudReporter, err = newCloudReporter(store, cloudConfig)
		if err != nil {
			return fmt.Errorf("init evaluator cloud identity: %w", err)
		}
	}
	rules, err := LoadThresholdRules(env("RULES_FILE", ""))
	if err != nil {
		return fmt.Errorf("load threshold rules: %w", err)
	}
	condor, err := newCondorClient()
	if err != nil {
		return fmt.Errorf("init Condor Connect client: %w", err)
	}
	logger.Info("evaluator started", "rules", len(rules))
	go runEvaluator(ctx, store, condor, env("ALERT_WEBHOOK_URL", ""), cloudReporter, rules...)
	return serveProcess(ctx, "canary", newHTTPServer(newDependencyServer(store, "canary")))
}

// RunSwift owns provider-facing pull adapters that can be separated from the
// generic HTTP intake. It currently carries the optional Mercado Pago adapter;
// new client protocols should be added here and emit canonical Signal values
// into Condor rather than inventing another storage path.
func RunSwift(ctx context.Context) error {
	if err := validateRuntimeEnvironment(); err != nil {
		return err
	}
	if err := validateAirGappedEnvironment(); err != nil {
		return err
	}
	store, err := newStore(ctx, clickHouseAddr(), clickHouseUser(), clickHousePassword(), clickHouseDB())
	if err != nil {
		return err
	}
	defer store.Close()

	shopifySecrets, err := loadShopifySecrets(env("SHOPIFY_SECRETS_FILE", ""))
	if err != nil {
		return fmt.Errorf("load Shopify secrets: %w", err)
	}
	sink, err := newCondorClient()
	if err != nil {
		return fmt.Errorf("init Condor Connect client: %w", err)
	}
	handler := newRouterWithShopifySecretsAndWriter(store, env("SHOPIFY_SECRET", ""), shopifySecrets,
		env("SIGNAL_INGEST_TOKEN", ""), sink, env("CANARY_ORGANIZATION_ID", ""), env("CANARY_ID", ""))
	config, err := loadMercadoPagoScannerConfig()
	if err != nil {
		return err
	}
	if scanner := newMercadoPagoScanner(sink, config); scanner != nil {
		logger.Info("swift adapter enabled", "adapter", "mercadopago", "tenant_id", config.TenantID)
		go scanner.Run(ctx)
	}
	return serveProcess(ctx, "swift", newHTTPServer(handler))
}

// RunHummingbird owns the optional outbound control-plane connection. Keeping it
// separate means a cloud outage cannot restart intake or evaluation.
func RunHummingbird(ctx context.Context) error {
	if err := validateRuntimeEnvironment(); err != nil {
		return err
	}
	if err := validateAirGappedEnvironment(); err != nil {
		return err
	}
	store, err := newStore(ctx, clickHouseAddr(), clickHouseUser(), clickHousePassword(), clickHouseDB())
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.InitCloudSchema(ctx); err != nil {
		return fmt.Errorf("init hummingbird schema: %w", err)
	}
	cloudConfig, err := loadCloudConfig()
	if err != nil {
		return err
	}
	if cloudConfig != nil {
		reporter, err := newCloudReporter(store, cloudConfig)
		if err != nil {
			return fmt.Errorf("init cloud reporter: %w", err)
		}
		go reporter.Run(ctx)
		logger.Info("hummingbird cloud reporting enabled", "base_url", cloudConfig.BaseURL)
	} else {
		logger.Info("hummingbird running without a cloud control plane")
	}
	return serveProcess(ctx, "hummingbird", newHTTPServer(newDependencyServer(store, "hummingbird")))
}

// RunCondor starts the durable signal-store process. The first transport is a
// deliberately narrow JSON bootstrap endpoint; the versioned Connect/Protobuf
// contract in /contracts is the replacement boundary for customer-owned
// implementations and will be wired without changing the storage contract.
func RunCondor(ctx context.Context) error {
	if err := validateRuntimeEnvironment(); err != nil {
		return err
	}
	store, err := newStore(ctx, clickHouseAddr(), clickHouseUser(), clickHousePassword(), clickHouseDB())
	if err != nil {
		return err
	}
	defer store.Close()
	return serveProcess(ctx, "condor", newHTTPServer(newCondorServer(store)))
}

// RunCapability exposes a separately deployable capability process. Each
// process selects its implementation through the contract registry while
// retaining the same HTTP envelope and health surface.
func RunCapability(ctx context.Context, id string) error {
	definition, ok := capabilityDefinition(CapabilityID(strings.TrimSpace(id)))
	if !ok {
		return fmt.Errorf("unknown capability %q", id)
	}
	if definition.ID != capabilityAIReasoning {
		var reader condorWindowReader
		if definition.ID == capabilityRaven || definition.ID == capabilityPigeon {
			condor, err := newCondorClient()
			if err != nil {
				return fmt.Errorf("init Condor Connect client: %w", err)
			}
			reader = condor
		}
		return serveProcess(ctx, string(definition.ID), newHTTPServer(newCapabilityServerWithCondor(definition, nil, reader)))
	}
	if err := validateRuntimeEnvironment(); err != nil {
		return err
	}
	store, err := newStore(ctx, clickHouseAddr(), clickHouseUser(), clickHousePassword(), clickHouseDB())
	if err != nil {
		return fmt.Errorf("init AI reasoning rate ledger: %w", err)
	}
	defer store.Close()
	return serveProcess(ctx, string(definition.ID), newHTTPServer(newCapabilityServerWithStore(definition, store)))
}

func clickHouseAddr() string     { return env("CLICKHOUSE_ADDR", "localhost:19000") }
func clickHouseUser() string     { return env("CLICKHOUSE_USER", "default") }
func clickHousePassword() string { return env("CLICKHOUSE_PASSWORD", "clickhouse") }
func clickHouseDB() string       { return env("CLICKHOUSE_DB", "default") }

func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              net.JoinHostPort(env("CANARY_NODE_LISTEN_ADDR", "127.0.0.1"), env("PORT", "8080")),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
}

func serveProcess(ctx context.Context, name string, server *http.Server) error {
	if server == nil {
		return fmt.Errorf("%s server is nil", name)
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("process listening", "process", name, "address", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
		close(serverErr)
	}()
	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}

type dependencyHandler struct {
	store *Store
	name  string
}

func newDependencyServer(store *Store, name string) http.Handler {
	h := &dependencyHandler{store: store, name: name}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", h.health)
	mux.HandleFunc("/readyz", h.ready)
	mux.HandleFunc("/version", h.version)
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		writeSelfReport(w, r, h.name, h.store)
	})
	return requestLoggingMiddleware(nil, mux)
}

func (h *dependencyHandler) health(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *dependencyHandler) ready(w http.ResponseWriter, r *http.Request) {
	if h.store != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := h.store.Ready(ctx); err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

func (h *dependencyHandler) version(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"service": h.name, "version": currentNodeVersion()})
}
