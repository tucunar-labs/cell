package canarynode

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

const signalEventsTable = "canary_signal_events"

// Greenfield source-neutral signal ledger. ReplacingMergeTree collapses
// retries by the durable dedup_key while preserving append-only writes.
// Tenant context leads the key because hot queries are scoped by node identity;
// the timestamp then prunes lookback windows before grouping dimensions scan.
func signalEventsSchema(payloadTTLDays int) string {
	return fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS canary_signal_events (
    signal_id      UUID,
    external_id    String DEFAULT '',
    organization_id LowCardinality(String) DEFAULT '',
    canary_id      String DEFAULT '',
    site_id        String DEFAULT '',
    dedup_key      String,
    dedup_version  UInt64,
    source         LowCardinality(String),
    tenant_id      LowCardinality(String),
    entity_id      String,
    kind           LowCardinality(String),
    name           LowCardinality(String),
    value          Float64 DEFAULT 0,
    unit           LowCardinality(String) DEFAULT '',
    dimensions     String DEFAULT '{}',
    payload        String DEFAULT '{}' TTL toDateTime(occurred_at) + INTERVAL 13 MONTH,
    occurred_at    DateTime64(3, 'UTC'),
    ingested_at    DateTime64(3, 'UTC') DEFAULT now64(3),
    INDEX signal_dedup_lookup dedup_key TYPE bloom_filter(0.001) GRANULARITY 1
) ENGINE = ReplacingMergeTree(dedup_version)
PARTITION BY toYYYYMM(occurred_at)
ORDER BY (organization_id, canary_id, source, kind, name, occurred_at, tenant_id, entity_id, dedup_key)
TTL toDateTime(occurred_at) + INTERVAL %d DAY DELETE
`, payloadTTLDays)
}

type Store struct {
	conn           clickhouse.Conn
	organizationID string
	canaryID       string
	redactKeys     map[string]struct{}
	cloudNodeIDMu  sync.RWMutex
	cloudNodeID    string
}

func (s *Store) SetCloudNodeID(nodeID string) {
	s.cloudNodeIDMu.Lock()
	s.cloudNodeID = strings.TrimSpace(nodeID)
	s.cloudNodeIDMu.Unlock()
}

func (s *Store) CloudNodeID() string {
	s.cloudNodeIDMu.RLock()
	defer s.cloudNodeIDMu.RUnlock()
	return s.cloudNodeID
}

const (
	clickhouseStartupTimeout = 30 * time.Second
	clickhouseRetryInterval  = time.Second
	defaultPayloadTTLDays    = 395 // equivalent to the historical 13-month retention
	maxPayloadTTLDays        = 3650
)

func newStore(ctx context.Context, addr, user, password, database string) (*Store, error) {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:        []string{addr},
		Auth:        clickhouse.Auth{Database: database, Username: user, Password: password},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("open clickhouse: %w", err)
	}
	defer func() {
		if err != nil {
			_ = conn.Close()
		}
	}()

	deadline := time.Now().Add(clickhouseStartupTimeout)
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err = conn.Ping(pingCtx)
		cancel()
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("ping clickhouse: %w", ctx.Err())
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("ping clickhouse after %s: %w", clickhouseStartupTimeout, err)
		}
		time.Sleep(clickhouseRetryInterval)
	}
	payloadTTLDays := payloadTTLFromEnv()
	for name, statement := range map[string]string{
		"signal ledger schema":    signalEventsSchema(payloadTTLDays),
		"incident schema":         incidentSchema,
		"delivery schema":         incidentDeliverySchema,
		"case story schema":       caseStorySchema,
		"AI call ledger schema":   deepSeekCallLedgerSchema,
		"consumer offsets schema": consumerOffsetsSchema(),
	} {
		if err = conn.Exec(ctx, statement); err != nil {
			return nil, fmt.Errorf("apply %s: %w", name, err)
		}
	}
	// Existing ledgers gain the index for new parts; historical materialization is
	// an explicit maintenance operation, never a startup-wide table rewrite.
	if err = conn.Exec(ctx, `ALTER TABLE canary_signal_events ADD INDEX IF NOT EXISTS signal_dedup_lookup dedup_key TYPE bloom_filter(0.001) GRANULARITY 1`); err != nil {
		return nil, fmt.Errorf("apply signal dedup index: %w", err)
	}
	store := &Store{
		conn:           conn,
		organizationID: strings.TrimSpace(os.Getenv("CANARY_ORGANIZATION_ID")),
		canaryID:       strings.TrimSpace(os.Getenv("CANARY_ID")),
		redactKeys:     loadPayloadRedactKeys(),
	}
	return store, nil
}

func payloadTTLFromEnv() int {
	return intFromEnvBounded("CANARY_RAW_PAYLOAD_TTL_DAYS", defaultPayloadTTLDays, 1, maxPayloadTTLDays)
}

func intFromEnvBounded(key string, fallback, minimum, maximum int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	number, err := strconv.Atoi(value)
	if err != nil || number < minimum || number > maximum {
		return fallback
	}
	return number
}

func (s *Store) Close() error { return s.conn.Close() }

// InsertSignals writes a bounded batch using ClickHouse's native batch path.
// Single signals use confirmed async inserts so high-frequency producers do
// not create a part for every request.
func (s *Store) InsertSignals(ctx context.Context, signals []Signal) error {
	started := time.Now()
	defer func() { nodeRuntimeMetrics.recordInsert(len(signals), time.Since(started)) }()
	ctx, span := tracer.Start(ctx, "ClickHouse INSERT canary_signal_events",
		withAttributes(
			spanString("db.system", "clickhouse"),
			spanInt("signal.count", len(signals)),
		))
	defer span.End()
	if len(signals) == 0 {
		return fmt.Errorf("signals batch is empty")
	}
	rows := make([]signalRow, 0, len(signals))
	seen := make(map[string]struct{}, len(signals))
	for _, signal := range signals {
		normalized, err := signal.Normalize()
		if err != nil {
			span.RecordError(err)
			span.SetStatus(spanError, err.Error())
			return err
		}
		dimensions, err := encodeDimensions(normalized.Dimensions)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(spanError, err.Error())
			return err
		}
		dedupKey := normalized.DedupKey()
		if _, exists := seen[dedupKey]; exists {
			continue
		}
		seen[dedupKey] = struct{}{}
		payload, err := redactPayload(normalized.Payload, s.redactKeys)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(spanError, "payload redaction failed")
			return err
		}
		rows = append(rows, signalRow{
			signal: normalized, dedupKey: dedupKey, dedupVersion: uint64(time.Now().UnixNano()),
			dimensions: dimensions, payload: string(payload),
		})
	}
	if len(rows) == 0 {
		return fmt.Errorf("signals batch is empty")
	}

	existing, err := s.existingSignalKeys(ctx, rows)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	newRows := rows[:0]
	for _, row := range rows {
		if _, exists := existing[row.dedupKey]; !exists {
			newRows = append(newRows, row)
		}
	}
	if len(newRows) == 0 {
		span.SetAttributes(spanInt("signal.duplicate_count", len(rows)))
		return nil
	}
	rows = newRows

	if err := s.insertSignalRows(ctx, rows); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	span.SetAttributes(spanInt("signal.inserted_count", len(rows)), spanInt("signal.duplicate_count", len(signals)-len(rows)))
	return nil
}

type signalRow struct {
	signal       Signal
	dedupKey     string
	dedupVersion uint64
	dimensions   string
	payload      string
}

func (s *Store) existingSignalKeys(ctx context.Context, rows []signalRow) (map[string]struct{}, error) {
	ctx, span := tracer.Start(ctx, "ClickHouse SELECT signal dedup identities",
		withAttributes(spanString("db.system", "clickhouse"), spanInt("signal.key_count", len(rows))))
	defer span.End()
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, row.dedupKey)
	}
	// Existence needs any persisted version, not a FINAL merge of the full ledger.
	query := `SELECT DISTINCT dedup_key
FROM canary_signal_events
WHERE dedup_key IN {keys:Array(String)}
LIMIT 10000
SETTINGS max_execution_time = 10, max_result_rows = 10000, result_overflow_mode = 'break'`
	result, err := s.conn.Query(ctx, query, clickhouse.Named("keys", keys))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return nil, err
	}
	defer result.Close()
	existing := make(map[string]struct{}, len(keys))
	for result.Next() {
		var key string
		if err := result.Scan(&key); err != nil {
			span.RecordError(err)
			span.SetStatus(spanError, err.Error())
			return nil, err
		}
		existing[key] = struct{}{}
	}
	if err := result.Err(); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return nil, err
	}
	span.SetAttributes(spanInt("signal.existing_key_count", len(existing)))
	return existing, nil
}

func (s *Store) insertSignalRows(ctx context.Context, rows []signalRow) error {
	ctx, span := tracer.Start(ctx, "ClickHouse INSERT signal ledger",
		withAttributes(spanString("db.system", "clickhouse"), spanString("db.table", signalEventsTable), spanInt("signal.count", len(rows))))
	defer span.End()
	const insertQuery = `INSERT INTO canary_signal_events
	    (signal_id, external_id, organization_id, canary_id, site_id, dedup_key, dedup_version, source, tenant_id, entity_id, kind, name, value, unit, dimensions, payload, occurred_at)`
	if len(rows) == 1 {
		row := rows[0]
		asyncCtx := clickhouse.Context(ctx, clickhouse.WithAsync(true))
		err := s.conn.Exec(asyncCtx, insertQuery+` VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.signal.ID, row.signal.ExternalID, row.signal.OrganizationID, row.signal.CanaryID, row.signal.SiteID,
			row.dedupKey, row.dedupVersion,
			row.signal.Source, row.signal.TenantID, row.signal.EntityID, row.signal.Kind,
			row.signal.Name, row.signal.Value, row.signal.Unit, row.dimensions, row.payload, row.signal.OccurredAt)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(spanError, err.Error())
		}
		return err
	}
	batch, err := s.conn.PrepareBatch(ctx, insertQuery)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	defer batch.Close()
	for _, row := range rows {
		if err := batch.Append(row.signal.ID, row.signal.ExternalID, row.signal.OrganizationID, row.signal.CanaryID, row.signal.SiteID,
			row.dedupKey, row.dedupVersion,
			row.signal.Source, row.signal.TenantID, row.signal.EntityID, row.signal.Kind,
			row.signal.Name, row.signal.Value, row.signal.Unit, row.dimensions, row.payload, row.signal.OccurredAt); err != nil {
			span.RecordError(err)
			span.SetStatus(spanError, err.Error())
			return err
		}
	}
	if err := batch.Send(); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return err
	}
	return nil
}

// ConversionAlert es una tienda cuya conversión de checkout está bajo el umbral.
type ConversionAlert struct {
	ShopID          string
	CheckoutStarted uint64
	PaymentSuccess  uint64
	ConversionRate  float64
}

func (s *Store) conversionMetrics(ctx context.Context, lookbackWindow time.Duration, threshold *float64, spanName string) ([]ConversionAlert, error) {
	attributes := []spanStartOption{
		withAttributes(
			spanString("db.system", "clickhouse"),
			spanInt64("lookback_seconds", int64(lookbackWindow.Seconds())),
		),
	}
	if threshold != nil {
		attributes = append(attributes, withAttributes(spanFloat64("conversion_threshold", *threshold)))
	}
	ctx, span := tracer.Start(ctx, spanName, attributes...)
	defer span.End()

	having := "checkout_started > 0"
	if threshold != nil {
		having = fmt.Sprintf("checkout_started >= %d AND conversion_rate < {conversion_threshold:Float64}", defaultRuleMinSamples)
	}
	query := fmt.Sprintf(`
SELECT
    tenant_id,
    countIf(name = 'checkout_started') AS checkout_started,
    countIf(name = 'payment_success') AS payment_success,
    toFloat64(payment_success) / nullIf(checkout_started, 0) AS conversion_rate
FROM (
    SELECT tenant_id, name
	    FROM canary_signal_events FINAL
	    WHERE source = 'shopify'
	      AND kind = 'event'
	      AND ({organization_id:String} = '' OR organization_id = {organization_id:String})
	      AND ({canary_id:String} = '' OR canary_id = {canary_id:String})
	      AND occurred_at >= {since:DateTime64(3)}
      AND occurred_at < {until:DateTime64(3)}
)
GROUP BY tenant_id
HAVING %s
ORDER BY conversion_rate ASC
LIMIT 10000
SETTINGS max_execution_time = 30,
         max_rows_to_read = 1000000000,
         max_result_rows = 10000,
         result_overflow_mode = 'break'`, having)
	until := time.Now().UTC()
	since := until.Add(-lookbackWindow)
	args := []any{
		clickhouse.Named("organization_id", s.organizationID),
		clickhouse.Named("canary_id", s.canaryID),
		clickhouse.Named("since", since),
		clickhouse.Named("until", until),
	}
	if threshold != nil {
		args = append(args, clickhouse.Named("conversion_threshold", *threshold))
	}

	rows, err := s.conn.Query(ctx, query, args...)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return nil, err
	}
	defer rows.Close()

	var metrics []ConversionAlert
	for rows.Next() {
		var metric ConversionAlert
		if err := rows.Scan(&metric.ShopID, &metric.CheckoutStarted, &metric.PaymentSuccess, &metric.ConversionRate); err != nil {
			span.RecordError(err)
			span.SetStatus(spanError, err.Error())
			return nil, err
		}
		metrics = append(metrics, metric)
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return nil, err
	}
	span.SetAttributes(spanInt("conversion.shop_count", len(metrics)))
	return metrics, nil
}

// ConversionAlerts evalúa conversión por tienda en una ventana de ClickHouse.
// No conserva estado en memoria: cada evaluación reconstruye el resultado desde los eventos.
func (s *Store) ConversionAlerts(ctx context.Context, lookbackWindow time.Duration, threshold float64) ([]ConversionAlert, error) {
	alerts, err := s.conversionMetrics(ctx, lookbackWindow, &threshold, "ClickHouse SELECT multi_tenant_conversion")
	if err == nil {
		// Keep the count on the evaluator span for external trace exporters.
		span := spanFromContext(ctx)
		span.SetAttributes(spanInt("conversion.alert_count", len(alerts)))
	}
	return alerts, err
}

// DashboardMetrics returns all stores with checkout traffic for the dashboard.
func (s *Store) DashboardMetrics(ctx context.Context, lookbackWindow time.Duration) ([]ConversionAlert, error) {
	return s.conversionMetrics(ctx, lookbackWindow, nil, "ClickHouse SELECT dashboard_conversion")
}

// SignalOverview returns source-neutral activity for the local dashboard. Both
// queries share the same bounded [since, until) window and explicit ClickHouse
// limits so a busy node cannot turn the overview into an unbounded scan/result.
func (s *Store) SignalOverview(ctx context.Context, lookbackWindow time.Duration) (signalOverview, error) {
	ctx, span := tracer.Start(ctx, "ClickHouse SELECT signals_overview",
		withAttributes(
			spanString("db.system", "clickhouse"),
			spanInt64("lookback_seconds", int64(lookbackWindow.Seconds())),
		))
	defer span.End()

	until := time.Now().UTC()
	since := until.Add(-lookbackWindow)
	const querySettings = `
SETTINGS max_execution_time = 10,
         max_rows_to_read = 1000000000,
         max_bytes_to_read = 100000000000,
         max_result_rows = 100,
         result_overflow_mode = 'break'`

	totalsQuery := `
SELECT count(), uniqExact(source), uniqExact(entity_id)
    FROM canary_signal_events FINAL
WHERE occurred_at >= {since:DateTime64(3)}
	  AND ({organization_id:String} = '' OR organization_id = {organization_id:String})
	  AND ({canary_id:String} = '' OR canary_id = {canary_id:String})
	  AND occurred_at < {until:DateTime64(3)}` + querySettings
	totalsRows, err := s.conn.Query(ctx, totalsQuery,
		clickhouse.Named("organization_id", s.organizationID), clickhouse.Named("canary_id", s.canaryID),
		clickhouse.Named("since", since), clickhouse.Named("until", until))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return signalOverview{}, err
	}
	if !totalsRows.Next() {
		err := totalsRows.Err()
		totalsRows.Close()
		if err == nil {
			err = fmt.Errorf("clickhouse returned no overview totals")
		}
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return signalOverview{}, err
	}
	var overview signalOverview
	if err := totalsRows.Scan(&overview.TotalSignals, &overview.SourceCount, &overview.EntityCount); err != nil {
		totalsRows.Close()
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return signalOverview{}, err
	}
	if err := totalsRows.Err(); err != nil {
		totalsRows.Close()
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return signalOverview{}, err
	}
	totalsRows.Close()

	groupsQuery := `
SELECT source, tenant_id, entity_id, kind, name, unit,
       count() AS samples, avg(value) AS avg_value, max(occurred_at) AS last_occurred_at
FROM canary_signal_events FINAL
WHERE occurred_at >= {since:DateTime64(3)}
	  AND ({organization_id:String} = '' OR organization_id = {organization_id:String})
	  AND ({canary_id:String} = '' OR canary_id = {canary_id:String})
	  AND occurred_at < {until:DateTime64(3)}
GROUP BY source, tenant_id, entity_id, kind, name, unit
ORDER BY last_occurred_at DESC
LIMIT 100` + querySettings
	groupRows, err := s.conn.Query(ctx, groupsQuery,
		clickhouse.Named("organization_id", s.organizationID), clickhouse.Named("canary_id", s.canaryID),
		clickhouse.Named("since", since), clickhouse.Named("until", until))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return signalOverview{}, err
	}
	defer groupRows.Close()
	overview.Summaries = make([]signalSummary, 0, 100)
	for groupRows.Next() {
		var summary signalSummary
		if err := groupRows.Scan(
			&summary.Source, &summary.TenantID, &summary.EntityID, &summary.Kind,
			&summary.Name, &summary.Unit, &summary.Samples, &summary.AverageValue, &summary.LastOccurredAt,
		); err != nil {
			span.RecordError(err)
			span.SetStatus(spanError, err.Error())
			return signalOverview{}, err
		}
		overview.Summaries = append(overview.Summaries, summary)
	}
	if err := groupRows.Err(); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return signalOverview{}, err
	}
	overview.GeneratedAt = until
	overview.WindowMinutes = int(lookbackWindow.Minutes())
	span.SetAttributes(spanInt("signals.summary_count", len(overview.Summaries)))
	return overview, nil
}

// ThresholdAlerts evaluates one source-neutral rule entirely in ClickHouse.
// The query groups by the configured tenant/entity dimensions and returns only
// groups whose configured aggregate crosses the rule threshold.
func (s *Store) ThresholdAlerts(ctx context.Context, rule ThresholdRule) ([]ThresholdAlert, error) {
	rule, err := rule.Validate()
	if err != nil {
		return nil, err
	}
	query, err := thresholdQuery(rule)
	if err != nil {
		return nil, err
	}
	ctx, span := tracer.Start(ctx, "ClickHouse SELECT threshold_rule_lookback",
		withAttributes(
			spanString("db.system", "clickhouse"),
			spanString("rule.id", rule.ID),
			spanString("rule.source", rule.Source),
			spanString("rule.kind", rule.Kind),
			spanString("rule.name", rule.Name),
			spanInt64("lookback_seconds", int64(rule.WindowSeconds)),
			spanFloat64("rule.threshold", rule.Threshold),
		))
	defer span.End()

	since := time.Now().UTC().Add(-time.Duration(rule.WindowSeconds) * time.Second)
	rows, err := s.conn.Query(ctx, query,
		clickhouse.Named("source", rule.Source),
		clickhouse.Named("kind", rule.Kind),
		clickhouse.Named("name", rule.Name),
		clickhouse.Named("since", since),
		clickhouse.Named("threshold", rule.Threshold),
		clickhouse.Named("min_samples", defaultRuleMinSamples),
		clickhouse.Named("organization_id", s.organizationID),
		clickhouse.Named("canary_id", s.canaryID),
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return nil, err
	}
	defer rows.Close()

	alerts := make([]ThresholdAlert, 0)
	for rows.Next() {
		var tenantID, entityID string
		var observedValue float64
		if err := rows.Scan(&tenantID, &entityID, &observedValue); err != nil {
			span.RecordError(err)
			span.SetStatus(spanError, err.Error())
			return nil, err
		}
		alerts = append(alerts, mapThresholdResult(rule, tenantID, entityID, observedValue))
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		span.SetStatus(spanError, err.Error())
		return nil, err
	}
	span.SetAttributes(spanInt("rule.alert_count", len(alerts)))
	return alerts, nil
}

// thresholdQuery uses only fixed SQL fragments. Source, kind, name, threshold
// and since are ClickHouse query parameters; operator and grouping are selected
// from validated closed sets and never contain raw JSON text.
func thresholdQuery(rule ThresholdRule) (string, error) {
	rule, err := rule.Validate()
	if err != nil {
		return "", err
	}
	selectEntity := "entity_id"
	groupBy := "tenant_id, entity_id"
	if len(rule.GroupBy) == 1 {
		selectEntity = "'' AS entity_id"
		groupBy = "tenant_id"
	}
	aggregate, err := thresholdAggregateSQL(rule.Aggregate)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`
SELECT
    tenant_id,
    %s,
    %s AS observed_value
FROM canary_signal_events FINAL
WHERE source = {source:String}
  AND kind = {kind:String}
  AND name = {name:String}
  AND ({organization_id:String} = '' OR organization_id = {organization_id:String})
  AND ({canary_id:String} = '' OR canary_id = {canary_id:String})
  AND occurred_at >= {since:DateTime64(3)}
GROUP BY %s
HAVING observed_value %s {threshold:Float64}
   AND count() >= {min_samples:UInt64}
LIMIT 10000
SETTINGS max_execution_time = 30,
         max_rows_to_read = 1000000000,
         max_result_rows = 10000,
         result_overflow_mode = 'break'`, selectEntity, aggregate, groupBy, thresholdSQLOperator(rule.Operator)), nil
}

// thresholdAggregateSQL maps the public rule vocabulary to fixed SQL. Values
// are deliberately not interpolated from the JSON configuration.
func thresholdAggregateSQL(aggregate string) (string, error) {
	switch aggregate {
	case AggregateAverage:
		return "avg(value)", nil
	case AggregateSum:
		return "sum(value)", nil
	case AggregateMin:
		return "min(value)", nil
	case AggregateMax:
		return "max(value)", nil
	case AggregateP50:
		return "toFloat64(quantileTDigest(0.50)(value))", nil
	case AggregateP90:
		return "toFloat64(quantileTDigest(0.90)(value))", nil
	case AggregateP95:
		return "toFloat64(quantileTDigest(0.95)(value))", nil
	case AggregateP99:
		return "toFloat64(quantileTDigest(0.99)(value))", nil
	default:
		return "", fmt.Errorf("unsupported threshold aggregate %q", aggregate)
	}
}

func thresholdSQLOperator(operator ThresholdOperator) string {
	switch operator {
	case OperatorGT:
		return ">"
	case OperatorGTE:
		return ">="
	case OperatorLT:
		return "<"
	case OperatorLTE:
		return "<="
	default:
		return "=" // unreachable after ThresholdRule.Validate
	}
}
