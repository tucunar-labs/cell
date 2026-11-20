package canarynode

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCapabilitySelfReport(t *testing.T) {
	definition, ok := capabilityDefinition(capabilityKingfisher)
	if !ok {
		t.Fatal("kingfisher definition missing")
	}
	request := httptest.NewRequest(http.MethodGet, "/status", nil)
	response := httptest.NewRecorder()
	newCapabilityServer(definition).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	var report selfReport
	if err := json.NewDecoder(response.Body).Decode(&report); err != nil {
		t.Fatal(err)
	}
	if report.Service != "kingfisher" || !report.Ready || report.Version == "" {
		t.Fatalf("unexpected report: %#v", report)
	}
}
