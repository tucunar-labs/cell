# Contrato mínimo `tucunar-app` ↔ `canary-node`

**Estado:** implementado en el node para el envío saliente. La Control API
receptora vive en `tucunar-app` y debe aplicar la autorización/idempotencia
descrita aquí.

**Versión del contrato:** `1.0.0` (wire major: `/v1`).

Este documento define la frontera mínima entre el runtime portable y el futuro
control plane de `tucunar-app` (Next.js + Supabase). El nodo sigue siendo
responsable de recibir webhooks Shopify, persistirlos en ClickHouse y calcular
salud operacional. La app recibe solamente identidad del nodo, heartbeats,
rollups e incidentes resumidos.

Las palabras **MUST**, **MUST NOT**, **SHOULD** y **MAY** son normativas.

## 1. Límites y principios

El tráfico de datos queda separado del tráfico de control:

```text
Shopify ──POST /webhooks/shopify──> canary-node ──> ClickHouse elegido
                                      │
                                      └─ HTTPS saliente /v1 ──> Control API
                                                                  │
                                             Next.js ─────────────┴─> Supabase
```

- El contrato de este documento es entre el nodo y una **Control API** del
  producto. El navegador y un JWT de Supabase no llaman directamente a estos
  endpoints.
- La URL lógica de producción es `https://api.tucunar.com`; el nodo la recibe
  como configuración (`TUCUNAR_API_BASE_URL`).
  El host puede ser distinto por despliegue, pero siempre debe usar HTTPS.
- Todas las peticiones de este contrato usan JSON UTF-8, `Content-Type:
  application/json` y rutas bajo `/v1`.
- Cada body lleva `contract_version: "1.0"`. El major de la URL y el major del
  campo deben coincidir.
- Los timestamps son RFC 3339 en UTC. Los contadores son enteros no negativos;
  los rates son números entre `0` y `1`.
- Las respuestas del control plane nunca contienen eventos Shopify crudos,
  credenciales ni secretos del nodo.

### 1.1 Qué no forma parte del contrato

No se exponen desde el nodo hacia Vercel/Supabase:

- el body crudo de un webhook;
- `X-Shopify-Hmac-Sha256`, otros headers Shopify o secretos;
- emails, nombres, direcciones, tokens de pago, números de orden o IDs de
  clientes;
- consultas arbitrarias a ClickHouse;
- un JWT de usuario de Supabase.

El control plane persiste únicamente el estado de control y los resúmenes que
se describen aquí. **Los eventos crudos Shopify permanecen en el ClickHouse del
perfil elegido: el ClickHouse gestionado por Tucunar en `cloud`, o el
ClickHouse operado por el cliente en `on_premise`. No pasan por Vercel ni por
Supabase.** La replicación futura de eventos seleccionados sería otro contrato,
otra decisión de consentimiento y otra versión mayor.

## 2. Identidad, tenancy y autorización

| Campo | Tipo | Significado y autoridad |
|---|---|---|
| `organization_id` | UUID opaco | Organización de Supabase a la que pertenece el canary. Lo crea y lo autoriza el control plane; no se deriva de `shop_id`. |
| `canary_id` | UUID opaco | Unidad de despliegue/configuración y límite de tenancy. Pertenece a exactamente una organización. Es el `canary` que la app muestra al usuario. |
| `node_id` | UUID opaco | Instancia de runtime registrada por el control plane durante enrollment. El nodo no lo elige. |
| `node_instance_id` | UUID | Identificador local generado por una instalación antes del enrollment. Sirve para detectar reinstalaciones; no autoriza nada. |
| `shop_id` | string | Dimensión de una tienda Shopify dentro de un canary. Es un identificador de fuente, no un límite de seguridad. Puede ser un dominio Shopify o el identificador opaco que configure el runtime. |
| `deployment_mode` | enum | `cloud` significa cloud-managed; `on_premise` significa nodo y ClickHouse operados por el cliente. |

Reglas de autorización:

1. La app autentica usuarios con Supabase Auth y comprueba membership de
   `organization_id` en el control plane. Un usuario nunca entrega su sesión al
   nodo.
2. El control plane crea el canary y emite un token de enrollment de un solo
   uso, corto y limitado a `(organization_id, canary_id, deployment_mode)`.
3. `POST /v1/nodes/enroll` acepta ese token y devuelve un token opaco de nodo.
   El token de enrollment se usa solo en el header `Authorization`, nunca en
   query parameters, logs o JSON.
4. Los demás endpoints aceptan únicamente el token de nodo como `Bearer`. El
   token está vinculado server-side a `node_id`, `canary_id` y
   `organization_id`; no es un JWT de Supabase ni un secreto del usuario.
5. En cada reporte el servidor compara el token, el `canary_id` de la ruta y
   los tres IDs del body. Cualquier discrepancia, canary eliminado o token
   revocado se rechaza. El `organization_id` del body nunca puede ampliar el
   alcance del token.
6. La autorización falla cerrada: no hay endpoint anónimo ni fallback por
   `shop_id`. Los errores de autorización no revelan si existe otra
   organización o canary.

Política inicial recomendada: token de enrollment válido durante 15 minutos y
token de nodo válido durante 90 días, con `access_token_expires_at` como fuente
de verdad. La rotación se hace emitiendo un nuevo token de enrollment desde la
app y revocando el anterior; un endpoint de rotación dedicado queda fuera
de v1.

## 3. Convenciones HTTP comunes

### Headers

Todos los `POST` requieren:

```http
Authorization: Bearer <token>
Content-Type: application/json
Accept: application/json
Idempotency-Key: <stable-key-for-this-logical-operation>
```

`Idempotency-Key` es ASCII, de 1 a 128 caracteres. El nodo MUST conservar la
misma clave al reintentar la misma operación; no puede reutilizarla con otro
body. El servidor devuelve un `X-Request-Id` para soporte y no debe loggear el
valor de `Authorization`.

`Idempotency-Key` tiene estas formas recomendadas:

| Operación | Clave recomendada |
|---|---|
| Enrollment | `enroll:<node_instance_id>` |
| Heartbeat | `heartbeat:<node_id>:<sent_at>` |
| Rollup | `rollup:<rollup_id>` |
| Incidente | `incident:<emission_id>` |

### Respuestas de error

Todos los errores usan la misma forma y no incluyen tokens, payloads ni
detalles de ClickHouse:

```json
{
  "error": {
    "code": "scope_mismatch",
    "message": "request is not authorized for this canary",
    "request_id": "8e2d3ac7-7cb7-4c91-8ce8-f11d7f2db0f4"
  }
}
```

| HTTP | `error.code` típico | Acción del nodo |
|---:|---|---|
| `400` | `invalid_json`, `invalid_request` | Corregir; no reintentar. |
| `401` | `missing_token`, `invalid_token`, `token_expired` | Detener reportes; hacer re-enrollment solo con una nueva instrucción de la app. |
| `403` | `scope_mismatch`, `token_revoked` | No reintentar automáticamente. |
| `404` | `not_found` | No reintentar sin revisar base URL o configuración del canary. |
| `409` | `idempotency_conflict`, `enrollment_token_used` | No reintentar con otra clave; conservar la respuesta o pedir nuevo enrollment. |
| `413` | `body_too_large` | Reducir el reporte; nunca enviar el evento crudo. |
| `415` | `unsupported_media_type` | Corregir el cliente. |
| `422` | `validation_failed`, `stale_report` | Corregir o descartar el reporte; no reintentar igual. |
| `429` | `rate_limited` | Respetar `Retry-After` y aplicar backoff. |
| `500`, `502`, `503`, `504` | `server_error`, `temporarily_unavailable` | Reintentar con la política de la sección 8. |

En `429` y `503` el servidor SHOULD incluir `Retry-After` en segundos. Un
`202` ya es aceptación del reporte; el nodo no debe esperar una lectura desde
Supabase para considerarlo exitoso.

## 4. Enrollment de node/canary

### `POST /v1/nodes/enroll`

El canary se crea primero en la app y el token resultante se entrega al
proceso que arranca el nodo. El endpoint no crea una organización a partir de
input del nodo. El token ya limita el canary; los IDs en el body son una
comprobación redundante y deben coincidir.

Request:

```json
{
  "contract_version": "1.0",
  "organization_id": "9b6f8e2f-5fb6-4ef1-9818-0f0b6d5d8f22",
  "canary_id": "1b3c29d1-e8d7-4d1e-8a0c-2f1ecf2cb7b3",
  "deployment_mode": "on_premise",
  "node_instance_id": "e0e2b063-4a65-47b3-a8d6-887e57ef2aa1",
  "node_version": "0.1.0",
  "capabilities": [
    "shopify_webhooks",
    "clickhouse_rollups",
    "incidents"
  ]
}
```

Headers relevantes:

```http
Authorization: Bearer <one-time-enrollment-token>
Idempotency-Key: enroll:e0e2b063-4a65-47b3-a8d6-887e57ef2aa1
```

Response inicial: `201 Created`.

```json
{
  "contract_version": "1.0",
  "organization_id": "9b6f8e2f-5fb6-4ef1-9818-0f0b6d5d8f22",
  "canary_id": "1b3c29d1-e8d7-4d1e-8a0c-2f1ecf2cb7b3",
  "node_id": "f3a4dd93-9ff4-4f6d-9e14-1e2f7ea0c4b0",
  "deployment_mode": "on_premise",
  "access_token": "<opaque-node-token>",
  "access_token_expires_at": "2026-11-28T12:00:00Z",
  "reporting": {
    "heartbeat_interval_seconds": 60,
    "rollup_interval_seconds": 60
  },
  "raw_event_forwarding": false
}
```

Si se pierde la respuesta, repetir **con el mismo** `Idempotency-Key` y el
mismo body devuelve `200 OK` y la misma respuesta semántica. El token de
enrollment sigue siendo de un solo uso para cualquier otra clave. Una clave
distinta después de un enrollment exitoso obtiene `409 enrollment_token_used`.
El control plane MUST conservar el resultado idempotente el tiempo suficiente
para recuperar una respuesta perdida, como mínimo 24 horas.

## 5. Heartbeat

### `POST /v1/canaries/{canary_id}/heartbeat`

El heartbeat indica disponibilidad del proceso, ClickHouse y evaluator. No es
un mecanismo de ingestión ni envía métricas de sistema de alta cardinalidad.

Request:

```json
{
  "contract_version": "1.0",
  "organization_id": "9b6f8e2f-5fb6-4ef1-9818-0f0b6d5d8f22",
  "canary_id": "1b3c29d1-e8d7-4d1e-8a0c-2f1ecf2cb7b3",
  "node_id": "f3a4dd93-9ff4-4f6d-9e14-1e2f7ea0c4b0",
  "sent_at": "2026-08-30T16:00:00Z",
  "node_version": "0.1.0",
  "status": "ready",
  "uptime_seconds": 86400,
  "components": {
    "http_ingest": "ready",
    "clickhouse": "ready",
    "evaluator": "running"
  },
  "data": {
    "lookback_seconds": 900,
    "last_event_at": "2026-08-30T15:59:58.431Z"
  }
}
```

`status` es `ready`, `degraded` o `unhealthy`; los valores de `components`
son `ready`, `running`, `degraded` o `unhealthy`. `last_event_at` puede omitirse
si no hubo eventos y nunca debe sustituirse por un payload.

Response: `202 Accepted`.

```json
{
  "contract_version": "1.0",
  "status": "accepted",
  "duplicate": false,
  "request_id": "c8f18af7-9b9b-4a0d-aee8-4888b4e0dd32"
}
```

Un heartbeat repetido con la misma clave y body también responde `202`, con
`duplicate: true` cuando el servidor ya lo había aceptado.

## 6. Rollups operacionales

### `POST /v1/canaries/{canary_id}/rollups`

Un rollup es una fotografía de una ventana ya calculada desde ClickHouse. En
v1 el nodo usa la ventana de 15 minutos y publica tanto la vista Shopify
(`shops`) como grupos source-neutral (`signals`). El control plane no vuelve a
consultar ClickHouse ni recalcula el payload.

Request:

```json
{
  "contract_version": "1.0",
  "organization_id": "9b6f8e2f-5fb6-4ef1-9818-0f0b6d5d8f22",
  "canary_id": "1b3c29d1-e8d7-4d1e-8a0c-2f1ecf2cb7b3",
  "node_id": "f3a4dd93-9ff4-4f6d-9e14-1e2f7ea0c4b0",
  "rollup_id": "52fbec18-e8e3-4bb0-a9d7-2b42f918f75c",
  "generated_at": "2026-08-30T16:00:02Z",
  "window": {
    "start": "2026-08-30T15:45:02Z",
    "end": "2026-08-30T16:00:02Z",
    "seconds": 900
  },
  "rule_set_version": "conversion-v1",
  "summary": {
    "total_shops": 2,
    "at_risk_shops": 1,
    "checkout_started": 200,
    "payment_success": 35,
    "conversion_rate": 0.175
  },
  "shops": [
    {
      "shop_id": "shop_id_1",
      "checkout_started": 100,
      "payment_success": 10,
      "conversion_rate": 0.1,
      "status": "at_risk"
    },
    {
      "shop_id": "shop_id_2",
      "checkout_started": 100,
      "payment_success": 25,
      "conversion_rate": 0.25,
      "status": "healthy"
    }
  ],
  "signals": [
    {
      "source": "crane-monitor",
      "tenant_id": "acme",
      "entity_id": "crane-17",
      "kind": "measurement",
      "name": "load",
      "unit": "tons",
      "samples": 30,
      "avg_value": 8.4,
      "last_occurred_at": "2026-08-30T16:00:02Z"
    }
  ]
}
```

Invariantes del payload:

- `summary.total_shops` es `shops.length`;
- los totales de `summary` son la suma de `shops`;
- `conversion_rate` es `payment_success / checkout_started` cuando el
  denominador es mayor que cero, y `0` cuando es cero;
- `at_risk` significa `conversion_rate < 0.2` en la regla actual;
- `window.end` identifica la posición lógica del snapshot; el orden de llegada
  HTTP no es el orden de negocio.
- `signals` contiene agregados acotados por `source`, `tenant_id`, `entity_id`,
  `kind`, `name` y `unit`; nunca contiene payloads crudos.

Response: `202 Accepted` con la forma común de aceptación. La deduplicación
primaria es `(canary_id, rollup_id)` y la de transporte es la
`Idempotency-Key`. Un mismo rollup repetido nunca debe crear una segunda fila de
resumen. El control plane puede aceptar ventanas que llegan fuera de orden,
pero la app debe mostrar la versión más reciente por `window.end`.

## 7. Incidentes

### `POST /v1/canaries/{canary_id}/incidents`

Cada incidente representa una regla y una tienda. `incident_key` es estable
para esa combinación y permite que el control plane haga upsert sin que el
nodo mantenga estado de negocio en memoria. `emission_id` identifica una
emisión concreta y se usa para reintentarla de forma idempotente.

Request de apertura:

```json
{
  "contract_version": "1.0",
  "organization_id": "9b6f8e2f-5fb6-4ef1-9818-0f0b6d5d8f22",
  "canary_id": "1b3c29d1-e8d7-4d1e-8a0c-2f1ecf2cb7b3",
  "node_id": "f3a4dd93-9ff4-4f6d-9e14-1e2f7ea0c4b0",
  "emission_id": "d7f8fda4-4be9-47fb-a8cd-ae7e8e72b956",
  "incident_key": "checkout_conversion_below_threshold:shop_id_1",
  "rule_id": "checkout_conversion_below_threshold",
  "lifecycle_event": "opened",
  "status": "open",
  "occurred_at": "2026-08-30T16:00:03Z",
  "shop_id": "shop_id_1",
  "window": {
    "start": "2026-08-30T15:45:03Z",
    "end": "2026-08-30T16:00:03Z",
    "seconds": 900
  },
  "threshold": {
    "operator": "lt",
    "value": 0.2
  },
  "metrics": {
    "checkout_started": 100,
    "payment_success": 10,
    "conversion_rate": 0.1
  }
}
```

`lifecycle_event` es `opened`, `updated` o `recovered`. La combinación válida
es `opened/open`, `updated/open` y `recovered/recovered`. La misma
`incident_key` se reutiliza para actualizaciones y recuperación; el control
plane asigna y devuelve el `incident_id` durable.

Para reglas genéricas o analíticas, `metrics.observed_value` contiene el
agregado que cruzó el umbral y `metrics.baseline_value` contiene la referencia
cuando aplica. `metrics.conversion_rate` solo representa conversión para la
familia `conversion`; no se reutiliza para transportar valores de sensores.

Response: `202 Accepted`.

```json
{
  "contract_version": "1.0",
  "status": "accepted",
  "duplicate": false,
  "incident_id": "a3de692a-3ad6-49bf-a2cb-f2b4a54e3f2a",
  "incident_key": "checkout_conversion_below_threshold:shop_id_1",
  "request_id": "3b2e4ce4-8d25-4266-a4af-c0c9b5ba2b77"
}
```

Un update atrasado no debe reabrir ni sobrescribir un estado más nuevo: el
servidor lo puede aceptar como `stale: true` sin cambiar la vista materializada.
La comparación usa `occurred_at` y, en empate, la precedencia
`opened < updated < recovered`. Una nueva apertura después de una recuperación
se considera un nuevo episodio en el control plane, aunque conserve la misma
clave de regla/tienda.

El node produce los incidentes de conversión y genéricos desde la evaluación y
persiste el ciclo de vida local en ClickHouse. El payload conserva
`family`, `source`, `tenant_id` y `entity_id`; `shop_id` queda como alias de
compatibilidad para la integración Shopify.

## 8. Idempotencia, retries y timeouts

- La entrega es **at-least-once** desde el punto de vista del nodo. Todo POST
  tiene una `Idempotency-Key`; el control plane guarda el hash del body y el
  resultado. Misma clave + mismo body es un duplicate aceptable; misma clave +
  body distinto es `409 idempotency_conflict`.
- La retención mínima de claves de transporte es 7 días para rollups,
  incidentes y heartbeats. Para enrollment es 24 horas. La deduplicación de
  rollups por `rollup_id` y de incidentes por `emission_id` debe durar al menos
  tanto como pueda repetirse el reporte desde ClickHouse.
- El timeout es un deadline total de la request, incluyendo conexión y lectura:
  10 s para enrollment/heartbeat y 15 s para rollup/incident. El cliente debe
  cancelar la request al vencer el deadline.
- Se reintenta solamente ante error de red, timeout, `408`, `429`, `500`,
  `502`, `503` y `504`. Se usa backoff con jitter, por ejemplo 5 intentos como
  máximo con esperas aproximadas de 1 s, 2 s, 4 s y 8 s, sin superar 30 s de
  tiempo total de reintentos. `Retry-After` puede alargar la próxima espera,
  pero el cliente mantiene un límite razonable.
- No se reintentan `400`, `401`, `403`, `404`, `409`, `413`, `415` ni `422` sin
  cambiar la causa. En `401 token_expired`, la recuperación requiere un nuevo
  enrollment emitido por la app; no se adivinan credenciales.
- La ingesta Shopify y la evaluación local no deben bloquearse esperando al
  control plane. Si la nube no está disponible, el nodo on-premise continúa
  aceptando eventos, consultando ClickHouse y enviando sus alertas locales. Los
  rollups se pueden reconstruir desde la ventana de ClickHouse al reconectar;
  no es necesario encolar eventos crudos.
- Si se implementa una cola local de reportes pendientes, debe ser acotada y
  contener solo envelopes ya redactados. Nunca puede ser una vía de salida para
  `payload` crudo ni crecer sin límite.

## 9. Redaction y observabilidad

El contrato usa allowlist. Los únicos datos de una tienda en v1 son su
`shop_id`, contadores, rates, estado y ventanas. El nodo MUST redactar o
descartar cualquier campo adicional de Shopify antes de construir un rollup o
incidente.

Logs y spans del cliente:

- pueden incluir `organization_id`, `canary_id`, `node_id`, endpoint, status
  HTTP, `request_id` y latencia;
- pueden incluir `shop_id` solo como dimensión operacional de baja sensibilidad;
- no pueden incluir `Authorization`, enrollment token, access token, body JSON
  completo, HMAC, `payload` ni URLs con secretos;
- deben usar el mismo `request_id` del servidor para correlación, sin convertirlo
  en una credencial.

El servidor debe aplicar límites por organización/canary y registrar auditoría
de enrollment, revocación y errores de scope sin persistir secretos. El node no
requiere un collector ni una plataforma de observabilidad: cada proceso expone
`/healthz`, `/readyz`, `/version` y `/status`, y registra requests como JSON
estructurado sin payloads crudos, tokens o credenciales.

## 10. Diferencias entre los perfiles

Ambos perfiles usan las mismas rutas, JSON, IDs y reglas de autorización. Solo
cambian el operador, el lugar del dato y la topología de red.

| Aspecto | `cloud` (cloud-managed) | `on_premise` |
|---|---|---|
| Operador del nodo/ClickHouse | Tucunar | Cliente |
| Shopify → nodo | Endpoint cloud operado por Tucunar | Endpoint local expuesto por el cliente |
| ClickHouse de eventos | Gestionado por Tucunar | Gestionado y respaldado por el cliente |
| Nodo → Control API | Puede usar red interna o HTTPS público administrado por Tucunar | Solo conexión HTTPS saliente; no requiere inbound desde la nube |
| Enrollment | Automatizado por el provisionamiento de Tucunar | Token entregado al instalador o administrador |
| Dashboard | `tucunar-app` consume resúmenes autorizados en Supabase | Dashboard local actual sigue disponible como fallback; la app consume resúmenes |
| Caída de la Control API | El servicio cloud sigue con su operación local y reintenta | La ingestión, evaluación y alertas locales siguen; los resúmenes se reenvían luego |
| Residencia de payload crudo | ClickHouse cloud | ClickHouse del cliente |

`deployment_mode` no cambia el alcance de `organization_id` ni permite que un
nodo on-premise escriba directamente en Supabase. Un canary on-premise no
requiere conexiones entrantes, túneles ni acceso de la app al ClickHouse
del cliente.

## 11. Mapping del dashboard actual

El dashboard actual es local y no es aún una API de tenancy. `GET
/api/dashboard` consulta directamente ClickHouse con una ventana fija de 15
minutos y responde, en esencia:

```json
{
  "generated_at": "2026-08-30T16:00:02Z",
  "window_minutes": 15,
  "total_shops": 2,
  "at_risk_shops": 1,
  "checkout_started": 200,
  "payment_success": 35,
  "conversion_rate": 0.175,
  "shops": [
    {
      "shop_id": "shop_id_1",
      "checkout_started": 100,
      "payment_success": 10,
      "conversion_rate": 0.1,
      "status": "at_risk"
    }
  ]
}
```

Mapping al rollup v1:

| Dashboard actual | Rollup v1 | Cambio posterior |
|---|---|---|
| `generated_at` | `generated_at` | Mantener UTC y añadir `window.end` para orden lógico. |
| `window_minutes` | `window.seconds` | El contrato usa segundos; la UI puede seguir mostrando minutos. |
| `total_shops` | `summary.total_shops` | Añadir `organization_id`, `canary_id` y `node_id` al envelope. |
| `at_risk_shops` | `summary.at_risk_shops` | Persistir el snapshot autorizado en Supabase. |
| `checkout_started`, `payment_success`, `conversion_rate` | mismos nombres dentro de `summary` | Validar invariantes y versionar reglas. |
| `shops[]` | `shops[]` | Mantener una fila por `shop_id`; no incluir payload ni PII. |
| `shops[].status` | `shops[].status` | Seguir derivando `healthy`/`at_risk` desde la regla publicada. |

La primera implementación del corte Node↔Cloud ya cubre el envío saliente. La
app debe persistir los envelopes recibidos con RLS y deduplicación por los
identificadores indicados en este documento; eso pertenece al control plane y
no hace que el nodo dependa de Supabase para ingerir o evaluar señales.

### Correspondencia con el código actual

- `ingest.go` valida HMAC opcional para Shopify y expone la ingesta genérica
  autenticable `/v1/signals` y `/v1/signals/batch`; el contexto de cloud se
  valida durante enrollment y se mantiene fuera del payload crudo.
- `store.go` persiste el contrato `Signal` en ClickHouse. La tabla
  `canary_signal_events` es el único ledger raw y canónico; el adaptador
  Shopify solo transforma webhooks antes de escribirlo, usando
  `tenant_id=shop_id` y `name` para conservar la conversión.
- `rules.go` ejecuta cada minuto, usa lookback de 15 minutos, alerta bajo 20%
  y reconcilia el lifecycle de incidentes desde ClickHouse sin estado de
  alertas en memoria.
- `cloud_client.go` implementa enrollment, Bearer, HTTPS, timeout, retries y
  backoff; `cloud_store.go` mantiene el outbox durable y acotado.
- `cloud_reporter.go` publica heartbeat y rollups; `incident_cloud.go` publica
  los incidentes de conversión y source-neutral con la misma forma versionada.
- `dashboard.go` y `dashboard.html` son la vista local/fallback; no son la
  superficie de reporting cloud.

Las rutas `/v1/nodes/*` son rutas de la Control API receptora: el node no las
expone localmente, sino que las invoca por HTTPS saliente. Las rutas de datos
`/v1/signals*` siguen siendo locales al node.

## 12. Evolución del contrato

- Cambios aditivos compatibles (campos opcionales, nuevos valores que el
  receptor pueda ignorar, nuevas capacidades) permanecen bajo `/v1` y suben el
  minor de `contract_version`, por ejemplo `1.1`.
- El receptor debe ignorar campos desconocidos no sensibles para permitir una
  actualización gradual; el nodo no debe enviar campos que no estén publicados
  en una versión. La OpenAPI es la allowlist documentada de v1.
- Cambiar el significado de un campo, una unidad, el cálculo de una métrica,
  la semántica de tenancy, el mecanismo de autenticación o enviar eventos
  crudos requiere `/v2`, migración explícita y coexistencia temporal.
- `rule_set_version` identifica la semántica que generó un rollup. La app
  no debe comparar tasas de reglas incompatibles sin mostrar esa versión.
- Las migraciones de Supabase son responsabilidad del control plane. Deben ser
  versionadas, reejecutables de forma segura y verificadas por organización,
  tomando del starter kit únicamente la idea de estado explícito por target,
  aislamiento fail-closed y tests de aislamiento; no se adopta su tenancy por
  schemas ni su modelo de negocio ERP.

## 13. Códigos y superficies de v1

| Método | Ruta | Éxito | Auth |
|---|---|---:|---|
| `POST` | `/v1/nodes/enroll` | `201` nuevo; `200` replay idempotente | Bearer enrollment de un solo uso |
| `POST` | `/v1/canaries/{canary_id}/heartbeat` | `202` | Bearer token de nodo |
| `POST` | `/v1/canaries/{canary_id}/rollups` | `202` | Bearer token de nodo |
| `POST` | `/v1/canaries/{canary_id}/incidents` | `202` | Bearer token de nodo |

La especificación OpenAPI mínima equivalente está en
[`tucunar-app-canary-node.openapi.yaml`](tucunar-app-canary-node.openapi.yaml). Es un
artefacto de contrato, no una indicación de que las rutas ya existan.
