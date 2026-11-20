package canarynode

import (
	"strings"
	"testing"
)

func TestBoundedReadQueryAllowsReadOnlyQueries(t *testing.T) {
	query, err := boundedReadQuery("SELECT source, count() FROM canary_signal_events GROUP BY source")
	if err != nil {
		t.Fatalf("boundedReadQuery() error = %v", err)
	}
	if !strings.HasPrefix(query, "SELECT * FROM (SELECT source") {
		t.Fatalf("query was not wrapped: %q", query)
	}
	if !strings.Contains(query, "readonly = 1") || !strings.Contains(query, "LIMIT 200") {
		t.Fatalf("query guard missing: %q", query)
	}
}

func TestBoundedReadQueryRejectsMutationAndMultipleStatements(t *testing.T) {
	for _, query := range []string{
		"INSERT INTO canary_signal_events VALUES (1)",
		"SELECT 1; DROP TABLE canary_signal_events",
		"ALTER TABLE canary_signal_events DELETE WHERE 1",
		"SELECT * FROM canary_signal_events FORMAT JSONEachRow",
	} {
		if _, err := boundedReadQuery(query); err == nil {
			t.Fatalf("boundedReadQuery(%q) succeeded, want error", query)
		}
	}
}
