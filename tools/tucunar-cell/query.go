package canarynode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	maxReadQueryBytes = 16 * 1024
	maxReadQueryRows  = 200
)

type readQueryRequest struct {
	Query string `json:"query"`
}

type readQueryResponse struct {
	Columns  []string         `json:"columns"`
	Rows     []map[string]any `json:"rows"`
	RowCount int              `json:"row_count"`
	Elapsed  int64            `json:"elapsed_ms"`
}

// queryValue is a small database/sql.Scanner adapter. ClickHouse columns do
// not all scan into *any directly, but they can all hand their native value to
// a Scanner. This keeps the explorer independent from a fixed result schema.
type queryValue struct{ value any }

func (v *queryValue) Scan(value any) error {
	switch typed := value.(type) {
	case []byte:
		v.value = string(typed)
	default:
		v.value = value
	}
	return nil
}

func (v queryValue) Value() any { return v.value }

func (s *Server) handleQueryAPI(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "HTTP POST /api/query",
		withAttributes(spanString("http.method", r.Method), spanString("http.target", r.URL.Path)))
	defer span.End()

	if !s.queryEnabled {
		http.NotFound(w, r)
		return
	}
	if !s.authorizeDashboardAPI(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="canary-operator"`)
		http.Error(w, "operator token required", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxReadQueryBytes+1))
	if err != nil {
		http.Error(w, "invalid query request", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()
	if len(body) > maxReadQueryBytes {
		http.Error(w, "query is too large", http.StatusRequestEntityTooLarge)
		return
	}
	var request readQueryRequest
	if err := json.Unmarshal(body, &request); err != nil {
		http.Error(w, "query must be a JSON object", http.StatusBadRequest)
		return
	}
	query, err := boundedReadQuery(request.Query)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	queryCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	started := time.Now()
	rows, err := s.store.conn.Query(queryCtx, query)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "read query failed")
		http.Error(w, fmt.Sprintf("query failed: %v", err), http.StatusBadRequest)
		return
	}
	defer rows.Close()

	columns := rows.Columns()
	result := readQueryResponse{Columns: columns, Rows: make([]map[string]any, 0, maxReadQueryRows)}
	for rows.Next() {
		values := make([]queryValue, len(columns))
		destinations := make([]any, len(columns))
		for index := range values {
			destinations[index] = &values[index]
		}
		if err := rows.Scan(destinations...); err != nil {
			span.RecordError(err)
			span.SetStatus(spanError, "read query decode failed")
			http.Error(w, "query result could not be decoded", http.StatusInternalServerError)
			return
		}
		row := make(map[string]any, len(columns))
		for index, column := range columns {
			row[column] = values[index].Value()
		}
		result.Rows = append(result.Rows, row)
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, "read query rows failed")
		http.Error(w, "query result could not be read", http.StatusInternalServerError)
		return
	}
	result.RowCount = len(result.Rows)
	result.Elapsed = time.Since(started).Milliseconds()
	span.SetAttributes(spanInt("query.row_count", result.RowCount), spanInt64("query.elapsed_ms", result.Elapsed))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func boundedReadQuery(raw string) (string, error) {
	query := strings.TrimSpace(raw)
	query = strings.TrimSuffix(query, ";")
	query = strings.TrimSpace(query)
	if query == "" {
		return "", fmt.Errorf("query is required")
	}
	if len(query) > maxReadQueryBytes {
		return "", fmt.Errorf("query is too large")
	}
	if strings.Contains(query, ";") {
		return "", fmt.Errorf("multiple statements are not allowed")
	}
	upper := strings.ToUpper(query)
	if !strings.HasPrefix(upper, "SELECT ") && !strings.HasPrefix(upper, "SELECT\n") && !strings.HasPrefix(upper, "WITH ") && !strings.HasPrefix(upper, "WITH\n") {
		return "", fmt.Errorf("only SELECT or WITH queries are allowed")
	}
	for _, keyword := range []string{"INSERT", "ALTER", "CREATE", "DROP", "TRUNCATE", "OPTIMIZE", "SYSTEM", "KILL", "GRANT", "REVOKE", "SETTINGS", "FORMAT", "INTO OUTFILE"} {
		if strings.Contains(upper, keyword) {
			return "", fmt.Errorf("query contains a blocked operation or clause: %s", keyword)
		}
	}
	// The outer SELECT enforces the row cap even when the operator omits LIMIT.
	// readonly=1 adds a second guard at the ClickHouse session level.
	return fmt.Sprintf("SELECT * FROM (%s) AS read_query LIMIT %d SETTINGS readonly = 1, max_execution_time = 5, max_result_rows = %d, result_overflow_mode = 'break'", query, maxReadQueryRows, maxReadQueryRows), nil
}
