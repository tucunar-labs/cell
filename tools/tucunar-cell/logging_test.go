package canarynode

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSanitizeRequestIDRejectsControlCharactersAndOversizedValues(t *testing.T) {
	if got := sanitizeRequestID("req-123"); got != "req-123" {
		t.Fatalf("sanitizeRequestID() = %q", got)
	}
	if got := sanitizeRequestID("req\n123"); got != "" {
		t.Fatalf("sanitizeRequestID() accepted a newline: %q", got)
	}
	if got := sanitizeRequestID(string(make([]byte, 129))); got != "" {
		t.Fatalf("sanitizeRequestID() accepted an oversized value: %q", got)
	}
}

func TestRequestLoggingMiddlewareEchoesOrGeneratesRequestID(t *testing.T) {
	called := false
	firstRequest := true
	handler := requestLoggingMiddleware(&Server{organizationID: "org-1", canaryID: "canary-1"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		requestID := requestIDFromContext(r.Context())
		if firstRequest && requestID != "req-123" {
			t.Fatalf("request id in context = %q", requestIDFromContext(r.Context()))
		}
		if requestID == "" {
			t.Fatal("request id in context is empty")
		}
		firstRequest = false
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Header.Set("X-Request-ID", "req-123")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if !called || response.Code != http.StatusNoContent || response.Header().Get("X-Request-ID") != "req-123" {
		t.Fatalf("middleware response = called:%v code:%d request_id:%q", called, response.Code, response.Header().Get("X-Request-ID"))
	}

	request = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Header().Get("X-Request-ID") == "" {
		t.Fatal("middleware did not generate a request id")
	}
}
