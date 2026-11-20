# Tucunar cell runtime

## Producto Community

Tucunar Cell es un producto open core de Tucunar Labs. El runtime, la consola
nativa y los contratos distribuidos llevan Apache-2.0. La versión
`0.1.0-alpha.1` se entrega para desarrollo y pilotos supervisados. El core
funciona sin cuenta cloud, caducidad ni servidor de licencias.

La guía de distribución autónoma está en
[`distribution/README.md`](../../README.md). Para preparar una instancia
con identidad estable y credenciales privadas,

```bash
python3 scripts/community-init.py "$HOME/.local/share/tucunar-cell"
```

El perfil generado Community activa Swift, Condor, Canary y Hummingbird,
deshabilita AI y acceso público al dashboard, y mantiene en cero réplicas los
birds auxiliares. La integración SCADA opcional vive en
[`examples/scada/README.md`](examples/scada/README.md). El exportador
`scripts/export-core.py` genera un archivo fuente por allowlist, sin incluir
historial Git, sitio comercial, archivos de secretos ni binarios locales.

> Diseño objetivo y separación cloud/on-premise: [`docs/architecture.md`](docs/architecture.md).

Runtime portable de observabilidad operacional para sistemas internos y fuentes
externas. La celda se compone de procesos Go independientes, con ClickHouse
como plano de datos y contratos versionados para que un cliente pueda sustituir
cualquier capacidad por su propio servicio.

## Arquitectura

```
Fuentes ──▶ Swift ──Connect Append──▶ Condor / ClickHouse
                                      ├── Connect Read ──▶ Canary (lookbacks)
                                      ├── Kingbird / Raven / Owl
                                      └── Pigeon (delivery)

Swift, Canary, Condor y Hummingbird son despliegues distintos.

El dashboard local usa los SVG oficiales desde `assets/logos/`. Se sincronizan
desde `tucunar-app/public/logos/` con `scripts/sync-brand-assets.sh` antes de
publicar una nueva imagen; el binario los embebe y no depende de la app cloud.
Cada bird tiene un entrypoint, imagen, Service y ejecutor funcional propio. El
supervisor de la celda es Kubernetes ligero (k0s), no un proceso Go compartido.
```

The logical capability boundaries are documented in
[`../../contracts/tucunar-capabilities.openapi.yaml`](../../contracts/tucunar-capabilities.openapi.yaml).
`Swift` is the protocol-facing edge; `Condor` owns the raw durable stream;
`Canary` owns lookback evaluation and incidents. Protobuf/Connect contracts
live in [`../../contracts/proto/tucunar/v1`](../../contracts/proto/tucunar/v1).
The bootstrap JSON endpoint on Condor is intentionally narrow and is not the
long-term replacement boundary. Swift and Canary use the small shared client in
`condor_client.go`; Raven and Pigeon can use the same `CondorReader` seam when
their evidence or delivery workflows need a bounded stream read.

El runtime mínimo requiere solo la app Go y ClickHouse. No hay collector,
Jaeger, Prometheus ni otra plataforma de observabilidad que instalar. Cada
proceso se reporta a sí mismo en JSON mediante `/healthz`, `/readyz`, `/version`
y `/status`; este último incluye dependencias, uptime y contadores acotados de
requests, errores, inserts y evaluaciones. Los logs JSON de stdout/stderr son
la segunda salida operativa y nunca contienen payloads crudos o credenciales.

La API OpenTelemetry no forma parte del despliegue ni es un requisito de
ejecución. Si en el futuro se necesita exportar trazas, debe ser una extensión
explícita del cliente y no una dependencia del node base.

## Reporte saliente al control plane

El perfil on-premise no recibe conexiones desde la nube. Si se configura
`TUCUNAR_API_BASE_URL`, el node se enrola con un token de un solo uso y abre
únicamente conexiones HTTPS salientes para heartbeats, rollups e incidentes.
El token de acceso emitido se conserva en el archivo local protegido por el
node; los reportes pendientes y el estado de entrega viven durablemente en
ClickHouse. Cada heartbeat también resume la salud de ClickHouse y del outbox
(`ready`, `near_limit`, `saturated`, `failed_records`), para que la app
pueda mostrar un node degradado antes de que el heartbeat se vuelva stale.

```bash
TUCUNAR_API_BASE_URL=https://api.tucunar.com \
TUCUNAR_ENROLLMENT_TOKEN='token-entregado-por-la-app' \
CANARY_ORGANIZATION_ID='9b6f8e2f-5fb6-4ef1-9818-0f0b6d5d8f22' \
CANARY_ID='1b3c29d1-e8d7-4d1e-8a0c-2f1ecf2cb7b3' \
CANARY_DEPLOYMENT_MODE=on_premise \
go run ./cmd/hummingbird
```

El enrollment se reintenta solo ante errores transitorios. Un `401`, `403` o
`409` detiene el enrollment automático y requiere emitir credenciales nuevas
desde la app. Los reportes usan `Authorization: Bearer`, `Idempotency-Key`,
timeout total por operación y backoff acotado. La cola local tiene un límite
configurable con `CANARY_MAX_PENDING_REPORTS`; nunca contiene payloads crudos.

## Scanner de pagos Mercado Pago

El scanner opcional de Swift consulta `GET /v1/payments/search` de Mercado Pago
y traduce cada pago a una señal `source=mercadopago` que envía a Condor. Los pagos con
estado `approved` usan `name=payment_success`; los demás conservan su estado
(`payment_pending`, `payment_rejected`, etc.). El `id` del pago se usa como
`external_id`, por lo que las consultas repetidas son idempotentes. El payload
guardado en la celda contiene solo estado, monto, moneda y fechas; nunca se
copia la respuesta completa del proveedor.

Para el canary interno de Production (`mercadolibre-test`), configura el token
de prueba únicamente en el `.env` del node:

```bash
MERCADOPAGO_ACCESS_TOKEN='TEST-…' \
MERCADOPAGO_TENANT_ID=mercadolibre-test \
TUCUNAR_API_BASE_URL=https://api.tucunar.com \
TUCUNAR_ENROLLMENT_TOKEN='token-de-enrollment' \
CANARY_ORGANIZATION_ID='organizacion-uuid' \
CANARY_ID='canary-uuid' \
CANARY_DEPLOYMENT_MODE=cloud \
kubectl apply -k deploy/k0s/overlays/single-node
```

`MERCADOPAGO_POLL_INTERVAL_SECONDS` (60 s por defecto),
`MERCADOPAGO_LOOKBACK_MINUTES` (24 h por defecto) y
`MERCADOPAGO_PAGE_SIZE` (50 como máximo) controlan la frecuencia y la ventana
de relectura. Si `MERCADOPAGO_ACCESS_TOKEN` está vacío, el scanner queda
desactivado y el resto del node funciona normalmente.

## Señales genéricas

El contrato mínimo para un sistema interno es:

```json
{
  "external_id": "reading-123",
  "source": "crane-monitor",
  "tenant_id": "acme",
  "entity_id": "crane-17",
  "kind": "measurement",
  "name": "load",
  "value": 8.4,
  "unit": "tons",
  "dimensions": {"site": "plant-01", "shift": "night"},
  "payload": {"operator": "team-a"},
  "occurred_at": "2026-08-30T20:00:00Z"
}
```

`source`, `tenant_id`, `entity_id`, `kind`, `name` y `occurred_at` son
obligatorios. `id` es opcional y, si se envía, debe ser UUID; si se omite,
Tucunar lo genera. `external_id` permite conservar el identificador de la
fuente sin imponerle UUIDs. El body individual está limitado a 1 MiB y un batch
a 1.000 señales/4 MiB.

Endpoints:

```text
POST /v1/signals        -> {"status":"accepted","id":"..."}
POST /v1/signals/batch  -> {"status":"accepted","accepted":N}
```

El endpoint batch es el camino recomendado para alto volumen. El endpoint
individual usa `async_insert=1` con `wait_for_async_insert=1`, por lo que el
cliente recibe aceptación después de que ClickHouse confirma la cola de
inserción. Los batches usan el formato nativo del driver y permanecen acotados
para no crecer sin límite.

### Ventana local de control

Para controlar el runtime local desde una ventana mínima (encender/apagar y
enviar escenarios de alerta/recuperación):

```bash
python3 ../tucunar-cell-control/control.py
```

`Turn cell off` escala deployments y ClickHouse a cero; conserva los PVC. Los
botones de escenario usan `cmd/scenario-generator` y el token local de k0s.

## Simulación de incidente multi-bird

`cmd/scenario-generator` creates finite demo scenarios using the same canonical
signal contract as real connectors. The `simple` profile crosses the minimum
operational path—ingest, durable storage, evaluation, incident and recovery.
The `complex` profile adds correlation, evidence, response, notification
failure, advanced reasoning and outbound cloud delivery through the full cell.
The complex profile deliberately does not emit an all-to-all packet mesh: the
timeline is for an operator, not a topology test.

Run it against the local Swift intake:

```bash
SIGNAL_TOKEN="$(kubectl -n tucunar-cell get secret tucunar-cell-secrets \
  -o jsonpath='{.data.SIGNAL_INGEST_TOKEN}' | base64 -d)"
go run ./cmd/scenario-generator \
  -profile simple \
  -state open \
  -target http://127.0.0.1:18080/v1/signals/batch \
  -token "$SIGNAL_TOKEN"
```

Use `-profile complex -state recovered` to exercise the full response story.
Both profiles accept `-state open|recovered`, `-tenant`, `-site-id`,
`-scenario` and `-start`, and persist the selected profile in each event's
dimensions so the console can distinguish cases without parsing their names.

The default `-state open` run stops at `incident.opened`, so the console shows
the exact alarm state a person would wake up to. Use `-state recovered` to
send the response and recovery chapters as well; without an explicit `-start`,
that version is placed 20 minutes in the past so the live rule can close while
the 24-hour timeline remains available for an overnight review. The dev overlay mounts a
real ClickHouse rule (`crusher.telemetry.timeout_growth`) and evaluates it
every five seconds; this is why the open run becomes a durable incident
instead of a decorative critical packet.

The command prints a unique scenario ID and dashboard URL. The live timeline
is available at `/api/timeline?scenario=<scenario-id>` and is queried from
ClickHouse through the Swift dashboard surface. `CANARY_DASHBOARD_PUBLIC=1`
is an explicit development-only setting for a local read-only dashboard;
intake, diagnostics and operator controls remain token-protected.

The console case selector is not a simulator-specific list. Swift builds its
case index from `scenario_id` metadata in ClickHouse at
`/api/scenarios?window_seconds=604800&limit=20`, and the browser loads one
case at a time through `/api/timeline`. The URL is stable and shareable, so
the same surface supports twenty visible cases (and the endpoint accepts up
to one hundred) without putting the full ledger in the browser. Story labels
are derived from signal phase, severity, status, source, entity and route
metadata; if a connector omits optional narrative fields, the console falls
back to canonical fields instead of assuming mining vocabulary.

### AI operator story

The `Explain case` action is an optional Shiitake enrichment. Swift reads the
ordered facts from ClickHouse and sends a bounded `console_story` capability
request to `shiitake`; the latter calls DeepSeek only when
`AI_REASONING_ENABLED=1` and `DEEPSEEK_API_KEY` are configured. The model never
participates in ingestion, evaluation, incident lifecycle, or recovery.

The response is validated against observed event IDs and participants. Go
derives the Mermaid source from the ordered evidence as a compact causal event
sequence instead of accepting model-generated diagram syntax or exposing the
internal cell topology. Stories are persisted in `canary_case_stories`, so a
refresh uses the stored result and does not call the model again. If the model
is unavailable or returns invalid JSON, the console stores and displays the
deterministic story instead.

Shiitake has a hard provider budget: at most **2 DeepSeek calls per
`scenario_id`** (the operator's case/event identity) and **10 calls per
organization/cell in any rolling 24-hour window**. Each attempt is reserved in
the durable `canary_ai_call_ledger` table before the provider request. The
reservation fails closed if ClickHouse is unavailable, and the third call for a
case returns HTTP `429` instead of calling DeepSeek. The AI deployment stays at
one replica because this check-then-insert reservation is intentionally strict;
horizontal AI scaling requires a distributed reservation design first.

For local development, keep the actual values in the ignored
`tools/tucunar-cell/.env` and run the AI process with that environment loaded.
For k0s, create the optional `tucunar-cell-ai-secrets` out-of-band; the base
deployment marks it optional so cells without an AI key remain fully
functional. The exact contract is `AIConsoleStoryRequest` /
`AIConsoleStoryResponse` in `contracts/tucunar-capabilities.openapi.yaml`.

## Shopify y conversión

El evaluador consulta los últimos 15 minutos, agrupa por `shop_id` y calcula
`payment_success / checkout_started`. Genera una alerta independiente por cada
tienda con conversión menor al 20%; no mantiene estado en memoria.

El adaptador conserva `/webhooks/shopify`, valida HMAC con un secreto por tienda
cuando `SHOPIFY_SECRETS_FILE` está configurado y traduce cada webhook a un `Signal` con
`source=shopify`, `tenant_id=shop_id`, `entity_id=shop_id`, `kind=event` y
`name=topic`.

`SHOPIFY_SECRETS_FILE` apunta a un JSON local con formato
`{"shop-a.myshopify.com":"secret-a"}`. Las tiendas sin entrada reciben `401`.
`SHOPIFY_SECRET` se conserva únicamente como fallback para desarrollo local;
cuando existe el archivo por tienda nunca se utiliza el secreto global.

El evaluator consulta los últimos 15 minutos de señales Shopify, agrupa por
tienda y calcula `payment_success / checkout_started`. Genera una alerta
independiente por cada tienda con conversión menor al 20%; no mantiene estado
de negocio en memoria.

## Reglas genéricas de umbral

Las señales de sistemas internos pueden activar reglas source-neutral desde un
archivo JSON local. Configura `RULES_FILE` con la ruta al archivo; una ruta
vacía desactiva las reglas genéricas. El evaluador consulta ClickHouse en cada
tick, calcula el `aggregate` configurado para cada grupo y dispara una alerta
por grupo que cruza el umbral. La agrupación acepta `tenant_id` o `tenant_id` + `entity_id`;
siempre se exige `tenant_id` para evitar mezclar clientes. Todos los grupos se
derivan nuevamente desde la ventana de ClickHouse después de reinicios.

El formato está ejemplificado en [`rules.example.json`](rules.example.json):

```json
{
  "rules": [{
    "rule_id": "crane.load.high",
    "source": "crane-monitor",
    "kind": "measurement",
    "name": "load",
    "aggregate": "max",
    "operator": "gte",
    "threshold": 10,
    "window_seconds": 300,
    "group_by": ["tenant_id", "entity_id"]
  }]
}
```

Los agregados permitidos son `avg` (default), `sum`, `min`, `max`, `p50`, `p90`,
`p95` y `p99`. Los percentiles usan `quantileTDigest`, una estimación eficiente
para alto volumen. Los operadores permitidos son `gt`, `gte`, `lt` y `lte`.
La configuración es estricta: máximo 100 reglas, ventanas entre 1 segundo y 30
días, identificador seguro, campos desconocidos rechazados y valores enviados a
ClickHouse como parámetros. El agregado default conserva la semántica histórica
de `avg(value)`; las reglas compuestas quedan para una extensión posterior.

La alerta genérica contiene `rule_id`, `source`, `tenant_id`, `entity_id`,
`observed_value`, `operator`, `threshold` y `window_seconds`. Usa el mismo
`ALERT_WEBHOOK_URL` que la regla Shopify. Si está vacío, ambas clases de alerta
se escriben en el log.

Para generar tráfico saludable para `shop_id_1` y `shop_id_2`:

```bash
go run ./traffic-generator -target http://localhost:8080/webhooks/shopify
```

El generador expone `http://localhost:9090` en el entorno local configurado. Para detener de golpe los pagos
exitosos de `shop_id_1`, dejando llegar sus checkouts, ejecuta:

```bash
curl -X POST http://localhost:9090/incident
curl -X POST http://localhost:9090/resume
```

El endpoint `/healthz` del generador permite comprobar que está activo. Cambia
`-target` a otro endpoint si la app usa un puerto distinto.

Para una prueba de volumen, `-rate` indica ciclos por segundo. Cada ciclo intenta
enviar un checkout por tienda y, mientras no haya incidente, un pago con la
probabilidad de `-success-rate`. Por ejemplo, esta configuración genera cerca de
340 webhooks/s con dos tiendas y 70% de pagos exitosos:

```bash
go run ./traffic-generator \
  -target http://localhost:18080/webhooks/shopify \
  -listen :9092 \
  -rate 100 \
  -workers 32 \
  -queue-size 10000
```

La cola es acotada: si la app no alcanza la tasa, el generador aplica backpressure
y lo muestra en `/stats`, sin crecer indefinidamente en memoria:

```bash
curl http://localhost:9092/stats
```

Para medir la ingesta genérica con una prueba finita y comparable, usa el
benchmark acotado. Por defecto ejecuta 10 segundos por cada objetivo de 100,
1.000 y 10.000 señales/s, con batches de 100, y devuelve JSON con throughput,
errores, p50/p95/p99 y el máximo de heap del proceso cliente:

```bash
go run ./benchmark \
  -base-url http://127.0.0.1:18080 \
  -token "$SIGNAL_INGEST_TOKEN" \
  -duration 10s \
  -rates 100,1000,10000
```

`client_heap_max_bytes` mide únicamente la memoria del benchmark; para medir
el límite real del appliance, combina el resultado con los límites y métricas
del pod (`kubectl top pod`, `kubectl describe pod`). Cada ejecución usa
`external_id` únicos, por lo que no depende de limpiar ClickHouse y no borra
datos. El benchmark nunca mantiene una cola sin límite: `-queue-size` controla
la cantidad máxima de batches pendientes.

Para comparar el comportamiento antes y durante el incidente:

```bash
curl -X POST http://localhost:9092/incident
curl http://localhost:18080/api/overview
curl -X POST http://localhost:9092/resume
```

La operación se inspecciona sin una plataforma externa:

```bash
curl http://127.0.0.1:8080/status
curl -H "X-Canary-API-Key: $CANARY_NODE_API_TOKEN" \
  http://127.0.0.1:8080/api/diagnostics
```

`/status` es seguro para probes y devuelve solo el nombre del proceso, versión,
estado de dependencias, uptime y contadores agregados. `/api/diagnostics` es
la vista de operador y requiere el token configurado en producción. Los logs
JSON permiten seguir un request con `request_id` sin almacenar su payload.

## Consola nativa

La interfaz de operador de Cell es una aplicación de escritorio nativa en
[`../tucunar-cell-console`](../tucunar-cell-console/), hecha en Rust con Raven.
Su vista principal es un HMI de proceso: esquema de la estación, instrumentos,
estado de dispositivos, tendencia y límites de seguridad. No usa WebView ni la
pantalla HTML histórica de Swift. La primera vertical slice consulta solo las
APIs locales de lectura y reproduce señales sintéticas; no manda órdenes a PLC
ni a equipos.

Raven se construye desde el commit fijado `7f4411c376cec8fad2d071d63696b8444ec2640d`:

```bash
git clone https://github.com/oscar-soirey/Raven.git /tmp/Raven
git -C /tmp/Raven checkout 7f4411c376cec8fad2d071d63696b8444ec2640d
cmake -S /tmp/Raven -B /tmp/Raven/build -G Ninja \
  -DCMAKE_BUILD_TYPE=Release -DRAVEN_BUILD_SHARED=ON \
  -DRAVEN_BUILD_EXAMPLES=OFF -DRAVEN_BUILD_TESTS=OFF \
  -DRAVEN_BUILD_TOOLS=OFF -DRAVEN_BUILD_WEB=OFF -DRAVEN_WAYLAND=ON
cmake --build /tmp/Raven/build --parallel
sudo cmake --install /tmp/Raven/build --prefix /usr/local
sudo ldconfig
```

Con la celda local arriba, abre el túnel al API de Swift en una terminal:

```bash
k0s kubectl -n tucunar-cell port-forward --address=127.0.0.1 \
  service/swift 18080:8080
```

En otra terminal, compila y abre el panel nativo:

```bash
cargo run --manifest-path tools/tucunar-cell-console/Cargo.toml
```

El botón `Replay water scenario` solo aparece habilitado funcionalmente cuando
se proporciona `TUCUNAR_SIMULATION_TOKEN` del perfil local de desarrollo. Sin
ese token, la app permanece de solo lectura. El servidor conserva `/` por
compatibilidad y exploración local; las superficies de datos son
`/api/overview` y `/api/timeline?scenario=...`. En desarrollo,
`/api/query` permite SQL `SELECT`/`WITH` read-only con límite de 200 filas y 5
segundos; mantén `CANARY_QUERY_ENABLED=0` fuera de una consola local protegida.
`/api/dashboard` sigue como compatibilidad del resumen Shopify antiguo.

## Logs operativos

El node escribe registros JSON a `stderr`. Cada request recibe o conserva un
`X-Request-ID` (también devuelto en la respuesta) y los registros HTTP incluyen
`request_id`, `organization_id`, `canary_id`, ruta, estado y duración cuando el
contexto está configurado. Los logs no incluyen cuerpos, payloads, tokens ni
secretos.

## DDL del ledger canónico

Aplicado automáticamente por la app; también en [`schema.sql`](schema.sql):

```sql
CREATE TABLE IF NOT EXISTS canary_signal_events (
    signal_id      UUID,
    external_id    String DEFAULT '',
    organization_id LowCardinality(String) DEFAULT '',
    canary_id      String DEFAULT '',
    site_id         String DEFAULT '',
    dedup_key       String,
    dedup_version   UInt64,
    source          LowCardinality(String),
    tenant_id       LowCardinality(String),
    entity_id       String,
    kind            LowCardinality(String),
    name            LowCardinality(String),
    value           Float64 DEFAULT 0,
    unit            LowCardinality(String) DEFAULT '',
    dimensions      String DEFAULT '{}',
    payload         String DEFAULT '{}' TTL toDateTime(occurred_at) + INTERVAL 395 DAY,
    occurred_at     DateTime64(3, 'UTC'),
    ingested_at     DateTime64(3, 'UTC') DEFAULT now64(3)
) ENGINE = ReplacingMergeTree(dedup_version)
PARTITION BY toYYYYMM(occurred_at)
ORDER BY (organization_id, canary_id, source, kind, name, occurred_at, tenant_id, entity_id, dedup_key)
TTL toDateTime(occurred_at) + INTERVAL 395 DAY DELETE
```

La tabla es append-only, usa tipos nativos para UUID, timestamp y valor,
`LowCardinality` para dimensiones repetidas y particiones mensuales para el
ciclo de vida. El payload se elimina por TTL de columna sin borrar la señal;
configura `CANARY_RAW_PAYLOAD_TTL_DAYS` (1–3650, default 395) para ajustar esa
retención. Las claves sensibles se redactan antes de insertar con
`CANARY_REDACT_PAYLOAD_KEYS=token,email,card_number` (lista separada por comas).
La instalación es greenfield: la app crea este esquema inicial al arrancar y no
ejecuta migraciones ni mantiene un ledger de señales duplicado. Si se cambia el
modelo antes de una release estable, se recrea el entorno de desarrollo y se
actualiza [`schema.sql`](schema.sql) junto con el binario.

## Compilar y ejecutar

```bash
# 1. Instalar k0s y nerdctl con BuildKit en el host
# 2. Construir las imágenes en el containerd de k0s
sudo make images CONTAINERD_ADDRESS=/run/k0s/containerd.sock
# 3. Crear el secreto fuera de Git y aplicar la celda
cp deploy/k0s/overlays/single-node/secret.env.example deploy/k0s/overlays/single-node/secret.env
kubectl apply -k deploy/k0s/overlays/single-node

# 4. Compilar todos los procesos, sin producir un binario monolítico
go build ./...

# 5. Ejecutar un proceso manualmente si ClickHouse ya está disponible
go run ./cmd/swift
```

La app reintenta el ping inicial a ClickHouse durante 30 segundos, por lo que
puedes ejecutar ambos pasos seguidos sin esperar manualmente al healthcheck.

> En producción configura `SHOPIFY_SECRETS_FILE`. `SHOPIFY_SECRET` vacío deja el
> adaptador sin firma solo para pruebas locales.

## Probar el flujo

```bash
# Enviar un webhook de prueba (HMAC válido)
./scripts/send_test_webhook.sh

# Generar tráfico multi-tenant y consultar el dashboard:
go run ./traffic-generator -target http://localhost:8080/webhooks/shopify -listen :9090 -rate 10
```

Abrir **http://localhost:18080/** para el dashboard de intake. Condor queda en
**http://localhost:18081/**. Con `ALERT_WEBHOOK_URL` vacío, las alertas se
escriben en el log del evaluator; configurado, se envían al webhook externo.

## Variables de entorno

| Variable | Default | Descripción |
|---|---|---|
| `PORT` | `8080` | Puerto HTTP de ingesta |
| `CLICKHOUSE_ADDR` | `localhost:19000` | Dirección nativa TCP de ClickHouse (host) |
| `CLICKHOUSE_USER` | `default` | Usuario de ClickHouse |
| `CLICKHOUSE_PASSWORD` | `clickhouse` | Contraseña de ClickHouse |
| `CLICKHOUSE_DB` | `default` | Base de datos de ClickHouse |
| `CONDOR_CONNECT_URL` | `http://condor:8080` | Servicio Connect interno para Append/Read |
| `TUCUNAR_INTERNAL_TOKEN` | *(vacío)* | Bearer compartido para llamadas internas |
| `ALERT_WEBHOOK_URL` | *(vacío)* | Webhook externo opcional para alertas |
| `ALERT_WEBHOOK_SECRET` | *(vacío)* | Secreto HMAC opcional para firmar alertas salientes |
| `SHOPIFY_SECRET` | *(vacío)* | Secreto del adaptador Shopify para validar HMAC |
| `SHOPIFY_SECRETS_FILE` | *(vacío)* | JSON local de secretos HMAC por dominio Shopify |
| `MERCADOPAGO_ACCESS_TOKEN` | *(vacío)* | Token server-side para leer pagos (sandbox o producción según la cuenta) |
| `MERCADOPAGO_API_BASE_URL` | `https://api.mercadopago.com` | Endpoint de búsqueda de pagos |
| `MERCADOPAGO_TENANT_ID` | `mercadolibre-test` | Tenant al que se asignan las señales |
| `MERCADOPAGO_POLL_INTERVAL_SECONDS` | `60` | Frecuencia de sondeo |
| `MERCADOPAGO_LOOKBACK_MINUTES` | `1440` | Ventana de relectura para capturar pagos tardíos |
| `MERCADOPAGO_PAGE_SIZE` | `50` | Pagos por página (máximo soportado por el proveedor) |
| `SIGNAL_INGEST_TOKEN` | *(vacío)* | Bearer/API token para `/v1/signals*`; vacío solo en desarrollo local |
| `CANARY_NODE_API_TOKEN` | *(vacío)* | Protege dashboard y diagnostics cuando el bind no es loopback |
| `CANARY_DASHBOARD_PUBLIC` | `0` | Solo desarrollo local: permite leer `/` y `/api/overview` sin token |
| `CANARY_QUERY_ENABLED` | `0` | Habilita el explorador SQL read-only en `/api/query`; úsalo solo en desarrollo o detrás de auth |
| `RULES_FILE` | *(vacío)* | Archivo JSON local de reglas genéricas; vacío desactiva estas reglas |
| `CANARY_RAW_PAYLOAD_TTL_DAYS` | `395` | Retención del payload crudo antes de eliminarlo por TTL de columna (1–3650 días) |
| `CANARY_REDACT_PAYLOAD_KEYS` | *(vacío)* | Claves JSON sensibles a reemplazar por `[REDACTED]`, separadas por comas |
| `TUCUNAR_API_BASE_URL` | *(vacío)* | Control API saliente; vacío desactiva el reporte cloud |
| `TUCUNAR_ENROLLMENT_TOKEN` | *(vacío)* | Token de enrollment de un solo uso entregado por la app |
| `CANARY_ORGANIZATION_ID` | *(requerido con API)* | UUID de la organización autorizada |
| `CANARY_ID` | *(requerido con API)* | UUID del canary autorizado |
| `CANARY_DEPLOYMENT_MODE` | `on_premise` | Perfil `cloud` u `on_premise` |
| `CANARY_NODE_INSTANCE_ID` | *(auto-generado)* | UUID estable de esta instalación |
| `CANARY_NODE_ID` | `CANARY_NODE_INSTANCE_ID` o `CANARY_ID` | Identidad del caller en `TenantContext` |
| `TUCUNAR_CELL_STATE_FILE` | `./tucunar-cell-state.json` | Archivo local 0600 para credenciales emitidas |
| `CANARY_NODE_VERSION` | `dev` | Versión visible en `/version`, diagnostics y heartbeat |
| `CANARY_HEARTBEAT_INTERVAL_SECONDS` | `60` | Intervalo de heartbeats salientes |
| `CANARY_ROLLUP_INTERVAL_SECONDS` | `60` | Intervalo solicitado para rollups |
| `CANARY_OUTBOX_FLUSH_INTERVAL_SECONDS` | `5` | Frecuencia de entrega de pendientes |
| `CANARY_MAX_PENDING_REPORTS` | `10000` | Límite de reportes pendientes en ClickHouse |

El dashboard local vive en `/` y su endpoint de diagnóstico en
`/api/diagnostics`. La respuesta incluye volumen reciente, señales/s, p95
acotado de inserts/evaluator, partes activas y bytes ocupados en ClickHouse,
memoria, límites y estado de integraciones; no incluye payloads ni secretos. Si
`CANARY_NODE_BIND` no es loopback, configura
`CANARY_NODE_API_TOKEN` y deja el servicio detrás de TLS o un reverse proxy
autenticado.

## Regla Shopify de compatibilidad

- Una evaluación inicial pide a Condor un snapshot Connect al arrancar y luego
  un `time.Ticker` lo vuelve a pedir cada **1 minuto**.
- Lee una **lookback window de 15 minutos**, agrupando las señales por tienda.
- Si `payment_success / checkout_started < 0.2` en los últimos 15 minutos, dispara un `HTTP POST` por tienda al webhook de alerta.
- No se mantiene ningún estado de negocio en memoria; si el proceso se
  reinicia, la siguiente evaluación reconstruye el contexto desde Condor y
  ClickHouse solo conserva el lifecycle de incidentes.

### Signal identity lookup maintenance

The signal ledger uses a Bloom skip index (`signal_dedup_lookup`) for exact
identity existence checks. Those checks do not require `FINAL`: any persisted
version establishes that an identity already exists. New installations include
the index; startup adds its definition to older ledgers without rewriting history.
After upgrading an older ledger, an operator can materialize it once:

```sql
ALTER TABLE canary_signal_events MATERIALIZE INDEX signal_dedup_lookup;
```

This builds index data asynchronously without deleting events. Check
`system.mutations` for completion and failures before comparing query latency.
