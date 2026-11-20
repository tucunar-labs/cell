package canarynode

// Server-sent events: the cell tells every open console when new signals have
// been written, so the UI never polls and never needs a refresh button. The
// event carries no payload on purpose: the console re-reads the two read models
// it already knows (/api/overview and /api/timeline) on the same origin. Pushing
// the snapshot itself is a later optimisation; the ordering guarantee that
// matters here is "the change is already in ClickHouse when you hear about it".

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

type streamHub struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func newStreamHub() *streamHub {
	return &streamHub{subs: make(map[chan struct{}]struct{})}
}

// subscribe returns a channel that receives one value per published change and
// a function that removes the subscription.
func (h *streamHub) subscribe() (chan struct{}, func()) {
	channel := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[channel] = struct{}{}
	h.mu.Unlock()
	return channel, func() {
		h.mu.Lock()
		if _, ok := h.subs[channel]; ok {
			delete(h.subs, channel)
			close(channel)
		}
		h.mu.Unlock()
	}
}

// publish wakes every subscriber. A subscriber that has not been read yet is
// skipped (its buffer is already full), so one slow console cannot block ingest.
func (h *streamHub) publish() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for channel := range h.subs {
		select {
		case channel <- struct{}{}:
		default:
		}
	}
}

func (h *streamHub) subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorizeDashboardAPI(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="canary-operator"`)
		http.Error(w, "operator token required", http.StatusUnauthorized)
		return
	}
	// Headers first: flushing commits the response, and the browser needs the
	// event-stream content type before the first byte.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	events, unsubscribe := s.hub.subscribe()
	defer unsubscribe()

	// The logging middleware wraps the writer; ResponseController unwraps it
	// through its Unwrap method and reaches the real http.Flusher.
	controller := http.NewResponseController(w)
	fmt.Fprint(w, "retry: 2000\n\n")
	fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	if err := controller.Flush(); err != nil {
		return
	}

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case _, open := <-events:
			if !open {
				return
			}
			fmt.Fprint(w, "event: signals\ndata: {}\n\n")
			_ = controller.Flush()
		case <-heartbeat.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			_ = controller.Flush()
		}
	}
}
