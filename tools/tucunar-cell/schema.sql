-- DDL append-only para señales operacionales genéricas y lifecycle durable.
-- La app aplica estos mismos esquemas al arrancar en instalaciones greenfield.
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
    ingested_at    DateTime64(3, 'UTC') DEFAULT now64(3)
) ENGINE = ReplacingMergeTree(dedup_version)
PARTITION BY toYYYYMM(occurred_at)
ORDER BY (organization_id, canary_id, source, kind, name, occurred_at, tenant_id, entity_id, dedup_key)
TTL toDateTime(occurred_at) + INTERVAL 13 MONTH DELETE;

CREATE TABLE IF NOT EXISTS canary_incidents (
    organization_id    LowCardinality(String) DEFAULT '',
    canary_id          String DEFAULT '',
    incident_key         String,
    version               UInt64,
    status                LowCardinality(String),
    rule_id               String,
    family                LowCardinality(String),
    source                LowCardinality(String),
    tenant_id             LowCardinality(String),
    entity_id             String DEFAULT '',
    observed_value        Float64 DEFAULT 0,
    baseline_value        Float64 DEFAULT 0,
    threshold             Float64 DEFAULT 0,
    operator              LowCardinality(String) DEFAULT '',
    window_seconds        UInt32 DEFAULT 0,
    recent_samples        UInt64 DEFAULT 0,
    baseline_samples      UInt64 DEFAULT 0,
    numerator_samples     UInt64 DEFAULT 0,
    denominator_samples   UInt64 DEFAULT 0,
    fingerprint           String DEFAULT '',
    first_seen_at         DateTime64(3, 'UTC'),
    last_seen_at          DateTime64(3, 'UTC'),
    resolved_at           DateTime64(3, 'UTC') DEFAULT toDateTime64(0, 3, 'UTC')
) ENGINE = ReplacingMergeTree(version)
ORDER BY (organization_id, canary_id, incident_key)
TTL toDateTime(last_seen_at) + INTERVAL 13 MONTH DELETE;

CREATE TABLE IF NOT EXISTS canary_incident_deliveries (
    organization_id LowCardinality(String) DEFAULT '',
    canary_id       String DEFAULT '',
    incident_key String,
    fingerprint  String,
    status       LowCardinality(String),
    attempts     UInt32,
    updated_at   DateTime64(3, 'UTC'),
    version      UInt64
) ENGINE = ReplacingMergeTree(version)
ORDER BY (organization_id, canary_id, incident_key, fingerprint)
TTL toDateTime(updated_at) + INTERVAL 13 MONTH DELETE;

-- Outbound control-plane reports. The node stores only redacted envelopes,
-- never raw source payloads, and retries them from this durable outbox.
CREATE TABLE IF NOT EXISTS canary_cloud_outbox (
    outbox_id UUID,
    node_id UUID DEFAULT toUUID('00000000-0000-0000-0000-000000000000'),
    operation LowCardinality(String),
    path String,
    idempotency_key String,
    payload String,
    status LowCardinality(String) DEFAULT 'pending',
    attempts UInt32 DEFAULT 0,
    next_attempt_at DateTime64(3, 'UTC'),
    last_error String DEFAULT '',
    sent_at DateTime64(3, 'UTC') DEFAULT toDateTime64('1970-01-01 00:00:00', 3, 'UTC'),
    version UInt64,
    created_at DateTime64(3, 'UTC') DEFAULT now64(3)
) ENGINE = ReplacingMergeTree(version)
PARTITION BY toYYYYMM(created_at)
ORDER BY (node_id, outbox_id)
TTL toDateTime(created_at) + INTERVAL 30 DAY DELETE WHERE status != 'pending';
