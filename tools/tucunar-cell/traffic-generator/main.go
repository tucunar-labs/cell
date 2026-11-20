// Command traffic-generator emits signed Shopify-like traffic for two stores.
// POST /incident stops payment_success for shop_id_1 until POST /resume.
//
// The generator is a development tool. Its queue is intentionally bounded so
// an overloaded app produces visible backpressure instead of unbounded memory use.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	selfreport "tucunar-cell/internal/selfreport"
)

var generatorTracer = selfreport.NewTracer("chaos-generator")

type webhookEvent struct {
	shop      string
	eventType string
}

type generator struct {
	target      string
	secret      string
	shop1       string
	shop2       string
	successRate float64
	client      *http.Client
	jobs        chan webhookEvent
	incident    atomic.Bool
	sequence    atomic.Uint64
	enqueued    atomic.Uint64
	completed   atomic.Uint64
	failed      atomic.Uint64
}

func main() {
	target := flag.String("target", "http://localhost:8080/webhooks/shopify", "webhook endpoint")
	listen := flag.String("listen", ":9090", "control endpoint")
	rate := flag.Float64("rate", 0, "traffic cycles per second; 0 uses interval")
	interval := flag.Duration("interval", 5*time.Second, "traffic interval when rate is 0")
	workers := flag.Int("workers", 16, "concurrent webhook workers")
	queueSize := flag.Int("queue-size", 2048, "maximum queued webhooks")
	secret := flag.String("secret", "test_secret", "Shopify HMAC secret")
	successRate := flag.Float64("success-rate", 0.7, "healthy payment success probability")
	flag.Parse()
	if *rate < 0 || *interval <= 0 || *workers <= 0 || *queueSize <= 0 || *successRate < 0 || *successRate > 1 {
		log.Fatal("rate must be >= 0; interval, workers and queue-size must be > 0; success-rate must be between 0 and 1")
	}

	g := &generator{
		target:      *target,
		secret:      *secret,
		shop1:       "shop_id_1",
		shop2:       "shop_id_2",
		successRate: *successRate,
		client:      &http.Client{Timeout: 10 * time.Second},
		jobs:        make(chan webhookEvent, *queueSize),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	control := &http.Server{Addr: *listen, Handler: g.controlHandler()}
	go func() {
		log.Printf("chaos control listening on %s", *listen)
		if err := control.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("control server: %v", err)
		}
	}()

	var workersDone sync.WaitGroup
	workersDone.Add(*workers)
	for i := 0; i < *workers; i++ {
		go g.worker(ctx, &workersDone)
	}

	log.Printf("sending traffic to %s (rate=%.2f cycles/s, workers=%d, queue=%d)", *target, *rate, *workers, *queueSize)
	g.runTraffic(ctx, *rate, *interval)
	close(g.jobs)
	workersDone.Wait()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = control.Shutdown(shutdownCtx)
}

func (g *generator) controlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/incident", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		g.incident.Store(true)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/resume", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		g.incident.Store(false)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"incident":  g.incident.Load(),
			"enqueued":  g.enqueued.Load(),
			"completed": g.completed.Load(),
			"failed":    g.failed.Load(),
			"queue":     len(g.jobs),
		})
	})
	return mux
}

func (g *generator) runTraffic(ctx context.Context, rate float64, interval time.Duration) {
	if rate == 0 {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				g.emitCycle(ctx)
			}
		}
	}

	const schedulerTick = 10 * time.Millisecond
	ticker := time.NewTicker(schedulerTick)
	defer ticker.Stop()
	credits := 0.0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			credits += rate * schedulerTick.Seconds()
			cycles := int(credits)
			credits -= float64(cycles)
			for i := 0; i < cycles; i++ {
				g.emitCycle(ctx)
			}
		}
	}
}

func (g *generator) emitCycle(ctx context.Context) {
	for _, shop := range []string{g.shop1, g.shop2} {
		if !g.enqueue(ctx, webhookEvent{shop: shop, eventType: "checkout_started"}) {
			return
		}
		healthy := shop != g.shop1 || !g.incident.Load()
		if healthy && rand.Float64() < g.successRate {
			if !g.enqueue(ctx, webhookEvent{shop: shop, eventType: "payment_success"}) {
				return
			}
		}
	}
}

func (g *generator) enqueue(ctx context.Context, event webhookEvent) bool {
	select {
	case <-ctx.Done():
		return false
	case g.jobs <- event:
		g.enqueued.Add(1)
		return true
	}
}

func (g *generator) worker(ctx context.Context, done *sync.WaitGroup) {
	defer done.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-g.jobs:
			if !ok {
				return
			}
			if err := g.send(ctx, event); err != nil {
				g.failed.Add(1)
				log.Printf("%s %s: %v", event.eventType, event.shop, err)
				continue
			}
			g.completed.Add(1)
		}
	}
}

func (g *generator) send(ctx context.Context, event webhookEvent) error {
	ctx, span := generatorTracer.Start(ctx, "HTTP POST /webhooks/shopify",
		withAttributes(
			spanString("shop_id", event.shop),
			spanString("event_type", event.eventType),
		))
	defer span.End()

	body, err := json.Marshal(map[string]any{
		"id":         g.sequence.Add(1),
		"shop_id":    event.shop,
		"event_type": event.eventType,
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}

	mac := hmac.New(sha256.New, []byte(g.secret))
	_, _ = mac.Write(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.target, bytes.NewReader(body))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Shopify-Hmac-Sha256", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-Shopify-Topic", event.eventType)
	req.Header.Set("X-Shopify-Shop-Domain", event.shop)

	resp, err := g.client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	defer resp.Body.Close()
	span.SetAttributes(spanInt("http.status_code", resp.StatusCode))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		err := fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	return nil
}
