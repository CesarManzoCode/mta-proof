# Contrato de mta-proof

Fuente única de qué debe hacer el sistema y de las decisiones cerradas. Los límites de lo que se demuestra viven solo en `LIMITATIONS.md`; las fuentes, con su revisión fijada, en `vault/RESEARCH.md`. Cuando una sección cita una fuente, lo hace por su identificador en RESEARCH (por ejemplo `[R-5321]`).

Convención: **hecho** es lo observado o leído en una fuente; **diseño** es lo que este contrato decide; **supuesto** es una cifra o regla sin fuente que el proyecto aporta como parámetro.

## 1. Qué es

mta-proof es un banco de pruebas al estilo Jepsen para el lado saliente de un MTA transaccional, en una sola máquina. Tres piezas:

1. **Simulador** (`mtasim`): receptor SMTP programable con perfiles declarativos de proveedores de buzones y un DNS de laboratorio. Solo reproduce comportamientos documentados; lo que no tiene fuente se marca como supuesto.
2. **Motor** (`mtaengine`): motor de entrega saliente de referencia, código propio. Cola durable por destino, reintentos con backoff y vida máxima, límites por dominio, proveedor, tenant e IP de origen, pools de IP con calentamiento, clasificación de respuestas, supresión, idempotencia en el ingreso y registro de eventos consultable (`mtaq`).
3. **Verificador** (`verifier/`): independiente del motor. Ejecuta cargas con fallas inyectadas y decide propiedades de seguridad y de vivacidad sobre la traza del receptor y el registro de eventos. Emite un reporte JSON con veredicto por propiedad.

Lo que se demuestra es el comportamiento del emisor frente a un receptor modelado; nunca entregabilidad real.

## 2. Arquitectura y fronteras (diseño)

### 2.1 Lenguajes y piezas

| Pieza | Lenguaje | Binario o entrada | Comparte código con |
| --- | --- | --- | --- |
| Motor | Go 1.27 | `cmd/mtaengine` | `mtadst`, `mtaq` |
| Simulador y DNS de laboratorio | Go 1.27 | `cmd/mtasim` | `mtadst` |
| Simulación determinista (motor + simulador en un proceso, reloj virtual) | Go 1.27 | `cmd/mtadst` | motor y simulador |
| Herramienta de depuración | Go 1.27 | `cmd/mtaq` | solo el esquema del almacén |
| Verificador, orquestador de fallas, reporte | Python 3 (solo biblioteca estándar) | `python3 -m verifier` | ninguno |

El verificador no importa nada del motor: lee archivos crudos (registro JSONL del receptor, base SQLite de eventos del motor en solo lectura, registro de fallas del orquestador, mensajes `.eml` capturados).

Módulo Go: `github.com/CesarManzoCode/mta-proof`.

### 2.2 Núcleo determinista y ejecutores

- El núcleo del motor (cola, programador por destino, límites, calentamiento, tenants, reintentos, clasificación) es una máquina de estados de un solo hilo. No lee el reloj ni el azar directamente: recibe un `Clock` y un generador `math/rand/v2` sembrado. Los mapas nunca se iteran sin ordenar cuando el orden afecta la salida.
- Dos ejecutores: **real** (goroutines, sockets TCP, reloj de pared) y **simulado** (cola de eventos con reloj virtual; la conversación SMTP se ejecuta en proceso contra la máquina de estados de sesión del simulador; la latencia la decide el perfil y avanza el reloj virtual).
- En modo simulado, misma semilla y mismo escenario producen la misma traza byte a byte.

### 2.3 Procesos en modo real y plan de direcciones

Toda corrida real ocurre dentro de un espacio de nombres de usuario, red y montaje sin privilegios (`unshare --user --map-root-user --net --mount`), con solo la interfaz `lo`. Así el tráfico no puede salir de la máquina y los puertos 25 y 53 se pueden abrir sin root (medido en esta máquina, ver RESEARCH, «Mediciones»).

| Rango | Uso |
| --- | --- |
| `127.0.0.1:53` UDP/TCP | DNS autoritativo de laboratorio (`mtasim`), zona `.test` |
| `127.0.0.1:8025` | API HTTP de ingreso del motor |
| `127.0.0.1:2525` | Entrada SMTP del MTA externo (solo validación externa) |
| `127.0.1.0/24:25` | Hosts MX simulados (uno o más por perfil) |
| `127.0.2.0/24` | IPs de origen del motor (pools) y del MTA externo |

- Todos los nombres de laboratorio cuelgan de `.test` (reservado por RFC 6761). Las direcciones de correo son sintéticas.
- El orquestador monta un `resolv.conf` propio (`nameserver 127.0.0.1`) solo dentro del espacio de nombres; el motor no depende de él (consulta a la dirección configurada), el MTA externo sí.
- `mtasim` abre un socket de control Unix por corrida para: cambiar de perfil en caliente, cortar conexiones y retener la respuesta 250 de un mensaje (ver 4.4).

### 2.4 Almacén

SQLite mediante `modernc.org/sqlite` (Go puro, sin CGO), en modo WAL con `synchronous=FULL`. El motor responde 202 al ingreso solo después del commit; registra el resultado de un intento en una transacción antes de considerarlo hecho. Se permite agrupar commits (group commit) de peticiones concurrentes.

### 2.5 Comandos

| Comando | Qué hace | Red |
| --- | --- | --- |
| `make deps` | `go mod download` y `npm ci` en `external/haraka` | Sí, una vez |
| `make build` | Compila `bin/` y los mutantes en `bin/mut/<nombre>/` | No |
| `make check` | `gofmt -l`, `go vet ./...`, `go test -count=1 ./...` y `python3 -m unittest` del verificador. Es el `check_command` del plan y lo corre el CI | No |
| `make verify` | El comando único de verificación del README. Corre `make build`, entra en un scope de systemd de usuario y en el espacio de nombres, ejecuta todas las propiedades y escribe `out/report.json`. Sale con 0 solo si todas pasan; si no, imprime cuáles fallaron | No |

`make check` no requiere espacios de nombres ni red: las pruebas de integración viven en `make verify`.

## 3. Formatos

### 3.1 Perfil del simulador (`profiles/<nombre>.json`)

```json
{
  "name": "yahoo-model",
  "sources": ["R-YAHOO", "R-SUPED-YAHOO"],
  "limitations": "texto libre; obligatorio si alguna regla viene de fuente secundaria",
  "adaptation_period_s": 300,
  "hosts": ["mx1.yahoo-model.test"],
  "rules": [
    {"id": "yahoo.max_msgs_per_conn", "kind": "max_msgs_per_conn", "params": {"limit": 20},
     "on_violation": {"action": "close_silent"},
     "source": "R-YAHOO", "assumption": true, "note": "Yahoo publica el comportamiento, no la cifra"}
  ]
}
```

- Cada regla lleva `source` (identificador de RESEARCH) y `assumption` (`true` si la cifra o el texto no salen de la fuente). Una regla sin `source` debe tener `assumption: true`.
- Tipos de regla mínimos: `max_msgs_per_conn`, `rate` (por IP de origen y por perfil), `concurrency` (por IP de origen), `unknown_ip` (límites más estrictos para IPs fuera de su lista de conocidas), `respond` (respuesta fija por condición), `drop_mid_data`, `latency`, `auth` (SPF/DKIM/DMARC).
- Las respuestas usan solo los códigos y textos de la tabla 3.2. Perfiles mínimos: `gmail-model`, `yahoo-model`, `outlook-model`, `baseline` (acepta todo, RFC 5321) y `always-defer` (responde `451 4.3.0` al final de DATA, para que el receptor vea el `msg_id` de cada intento).
- Una violación es un comando aceptado por encima de un límite del perfil, ocurrido después de `adaptation_period_s` contados desde la primera conexión de esa IP de origen a ese perfil. El simulador responde a la violación según `on_violation` y la anota en su registro con el id de la regla.

### 3.2 Respuestas documentadas

| Código | Texto (inicio literal de la fuente) | Fuente | Nota |
| --- | --- | --- | --- |
| `421 4.7.28` | `Gmail has detected an unusual rate of email originating from your IP address` | R-GOOGLE-ERR | Variantes por bloque de IP, dominio DKIM y SPF en la misma página |
| `421 4.7.0` | `Try again later, closing connection.` | R-GOOGLE-ERR | |
| `550 5.7.26` | `This email has been blocked because the sender is unauthenticated.` | R-GOOGLE-ERR | |
| `550 5.7.26` | `Unauthenticated email from domain-name is not accepted due to domain's DMARC policy.` | R-GOOGLE-ERR | `domain-name` se sustituye |
| `450 4.2.1` | `The user you are trying to contact is receiving email too quickly.` | R-GOOGLE-ERR | |
| `550 5.1.1` | `The email account that you tried to reach does not exist.` | R-GOOGLE-ERR | |
| `421 4.7.0 [TSS04]` | `Message temporarily deferred by Yahoo` | R-SUPED-YAHOO | Fuente secundaria |
| `421 [IPTS04]` | `Message deferred for IP policy reasons` | R-SUPED-YAHOO | Fuente secundaria; sin código extendido en la fuente |
| `550 5.7.515` | `Access denied, sending domain <domain> does not meet the required authentication level.` | R-MS-515 | |
| `550 5.7.509` | `Access denied, sending domain <domain> does not pass DMARC verification and has a DMARC policy of reject.` | R-MS-509 | Fuentes secundarias |
| `451 4.3.0` | `Requested action aborted: local error in processing` | R-5321 §4.2.3, R-3463 | Perfil `always-defer` |
| `556 5.1.10` | `Requested action not taken: domain does not accept mail` | R-7505 §4.1 | Lo genera el motor ante null MX, no el simulador |

### 3.3 Registro de eventos del motor (SQLite, tabla `events`)

| Columna | Tipo | Contenido |
| --- | --- | --- |
| `seq` | INTEGER PRIMARY KEY | Orden total de escritura |
| `at_ms` | INTEGER | Milisegundos del reloj del motor (virtual en `mtadst`) |
| `msg_id` | TEXT | Id del mensaje (nulo en eventos de tenant) |
| `tenant` | TEXT | |
| `kind` | TEXT | Ver lista |
| `domain`, `provider`, `mx_host`, `src_ip` | TEXT | Ruta del intento, cuando aplica |
| `attempt` | INTEGER | Número de intento, desde 1 |
| `code`, `enhanced`, `text` | INTEGER, TEXT, TEXT | Respuesta del receptor |
| `class`, `subtype`, `rule_id` | TEXT | Clasificación y regla que decidió |
| `msg_type` | TEXT | `transactional` o `marketing` |
| `detail` | TEXT | JSON con datos propios del evento (métrica de pausa, TLS, etc.) |

`kind` ∈ `accepted`, `idempotent_replay`, `idempotency_conflict`, `routed`, `attempt_started`, `response`, `classified`, `deferred`, `delivered`, `bounced`, `expired`, `suppressed`, `dsn_enqueued`, `tenant_paused`, `tenant_resumed`, `tls_negotiated`, `recovered`.

Estados terminales: `delivered`, `bounced`, `expired`, `suppressed`. Cada mensaje aceptado tiene un único evento `accepted`.

### 3.4 Registro del receptor (`receiver.jsonl`)

Una línea JSON por evento, en orden de escritura, con `fsync` al cerrar cada respuesta a `DATA`:

`seq`, `at_ms`, `conn`, `src_ip`, `dst_ip`, `host`, `profile`, `ev` (`connect`, `ehlo`, `starttls`, `mail`, `rcpt`, `data_end`, `reply`, `close`, `drop`, `violation`), y según el evento `mail_from` (cadena vacía para el reverse-path nulo), `rcpt`, `msg_id` (cabecera `X-Mtaproof-Id`), `sha256` del mensaje, `code`, `enhanced`, `text`, `rule`, `tls` y `auth` (`spf`, `dkim`, `dmarc`, cada uno `pass` o `fail`).

- `data_end` es una línea por cada DATA terminado y lleva la respuesta final a DATA (`code`, `enhanced`, `text`) junto con `msg_id` y `sha256`. `reply` es la respuesta a cualquier otro comando.
- `violation` lleva `rule` y `counted` (`false` dentro del periodo de adaptación, `true` después).
- El mensaje recibido se guarda además como `messages/<sha256>.eml`.

### 3.5 Registro del orquestador (`harness.jsonl`)

`at_ms`, `action` (`start`, `kill`, `profile_switch`, `cut`, `hold_250`, `release_250`), `pid`, `window` (`random` o `post_250`), `msg_id` cuando aplica.

### 3.6 Corrida

En modo real, `at_ms` de los tres registros son milisegundos de reloj de pared (época Unix) de la misma máquina, así que se pueden ordenar entre sí; en modo simulado son milisegundos virtuales desde el inicio del escenario.

Cada escenario escribe `out/runs/<escenario>-<semilla>/` con `receiver.jsonl`, `engine.db`, `harness.jsonl` (si hubo fallas), `messages/` y, en modo simulado, `events.jsonl` (exportación canónica de `events`: una fila por línea, ordenada por `seq`, objeto JSON con claves ordenadas y separadores `,` y `:` sin espacios) y `trace.sha256` (hex del sha256 de los bytes de `receiver.jsonl` seguidos de los de `events.jsonl`). `out/` no se versiona.

### 3.7 Reporte (`out/report.json`)

```json
{
  "format": "mta-proof-report/1",
  "git_head": "<sha>",
  "started_at": "<ISO 8601>",
  "duration_s": 0.0,
  "max_memory_bytes": 0,
  "memory_limit_bytes": 8589934592,
  "verdict": "pass",
  "properties": [
    {"name": "no_loss", "verdict": "pass", "seed": 1, "trace": "out/runs/kill-campaign-1", "details": {}}
  ]
}
```

`verdict` de propiedad ∈ `pass`, `fail`, `not_evaluated` (este último solo dentro de `external_validation`). El `verdict` global es `pass` solo si todas las propiedades pasan.

### 3.8 Corpus de clasificación (`corpus/classification.json`)

Lista de casos `{"id", "provider", "code", "enhanced", "text", "expected_class", "expected_subtype", "source"}`. Incluye como mínimo cada fila de 3.2 y un caso sin regla que espera `undetermined`.

Clases: `permanent`, `transient`, `undetermined`. Subtipos: los de Resend (`General`, `NoEmail`, `MailboxFull`, `MessageTooLarge`, `ContentRejected`, `AttachmentRejected`) [R-RESEND-BOUNCES] más los del proyecto: `RateLimited`, `Reputation`, `AuthConfig`, `TLSPolicy`, `NullMX`, `Connection`.

## 4. Propiedades

Cada propiedad tiene un nombre fijo, que es su clave en el reporte. El verificador registra solo las propiedades ya implementadas; los checkers de cada K exigen las suyas.

| Propiedad | Criterio de la idea | Cómo se decide | Modo | K |
| --- | --- | --- | --- | --- |
| `no_loss` | No pérdida | Todo `msg_id` con evento `accepted` tiene un estado terminal tras reiniciar y drenar. `details`: `messages`, `kills`, `kills_post_250`, `lost` | real | k1 |
| `bounded_duplicates` | Duplicación acotada | Un `msg_id` con más de un `data_end` aceptado (2xx) en el receptor es duplicado; cada duplicado necesita un `kill` del orquestador posterior a su primer 2xx y anterior a su evento `delivered`. `details`: `duplicates`, `duplicates_outside_window` (debe ser 0) | real | k1 |
| `idempotency` | Duplicación acotada (ingreso) | Misma clave y mismo payload → un solo `accepted`; misma clave y otro payload → respuesta 409 y evento `idempotency_conflict`. `details`: `replays`, `conflicts_rejected` | real | k1 |
| `receiver_limits` | Límites del receptor | Cero violaciones (3.1) por perfil y por IP de origen. `details.violations`: `{perfil: {ip: n}}` | real y simulado | k1 |
| `backoff_expiry` | Backoff y expiración | Contra `always-defer`: intervalos entre intentos de un mismo mensaje no decrecientes y ≤ máximo; `expired` al agotar la vida; DSN con `MAIL FROM:<>` recibido por el remitente. Reloj virtual | simulado | k1 |
| `classification` | Clasificación | 100 % del corpus coincide; cada decisión registra `rule_id`; ninguna respuesta sin regla es `permanent` | unidad sobre el corpus | k1 |
| `dns_mx` | DNS y TLS (MX) | MX probados en orden de preferencia con paso al siguiente ante falla; null MX → `bounced`/`NullMX` sin conexiones al receptor ni reintentos | real | k1 |
| `determinism` | Determinismo | Dos corridas con la misma semilla dan el mismo `trace.sha256`; otra semilla da otro. `details`: `hash_a`, `hash_b`, `hash_other` | simulado | k1 |
| `resources` | Recursos | `memory.peak` del scope de la corrida ≤ 8 GiB; registra `duration_s` | todo | k1 |
| `profile_sources` | Honestidad sobre el alcance | Cada regla de cada perfil tiene fuente o `assumption: true`; perfiles con fuentes secundarias declaran `limitations` | estático | k1 |
| `ip_warmup` | Pools y calentamiento | Por IP, nunca se supera el tope diario ni horario del plan; las IPs desconocidas reciben más estrangulamiento que las calentadas; el reparto de origen sigue el plan | simulado | k2 |
| `tenant_pause` | Control por tenant | Tenant con tasa de rebote > umbral se pausa dentro del plazo declarado, con evento `tenant_paused` y la métrica; los demás mantienen su tasa dentro del margen de aislamiento | simulado | k2 |
| `destination_isolation` | Aislamiento | p50 y p99 del tiempo hasta aceptación de los demás destinos, con un destino casi estrangulado, no empeoran más del margen respecto a la corrida base con la misma semilla. `details`: ambas distribuciones y el margen | simulado | k2 |
| `tls_policy` | DNS y TLS (TLS) | Con certificado inválido de la CA de prueba: `opportunistic` entrega, `required` difiere con subtipo `TLSPolicy`; `tls_negotiated` y el receptor lo respaldan | real | k2 |
| `outbound_auth` | Autenticación saliente | Mensajes bien configurados pasan SPF, DKIM y DMARC en el receptor; con configuración rota, `outlook-model` responde `550 5.7.515`, el motor clasifica `permanent`/`AuthConfig` y el receptor ve un solo intento | real | k2 |
| `rfc5322_format` | Autenticación saliente (formato) | Todo `.eml` capturado se analiza sin defectos con `email.policy.strict` de Python | sobre capturas | k2 |
| `debug_timeline` | Depuración | Para el 100 % de los mensajes de una corrida, `mtaq timeline` coincide con la traza del receptor | real | k2 |
| `debug_aggregates` | Depuración | `mtaq agg` por (tenant, dominio, proveedor, IP de origen, tipo de evento) suma exactamente lo que cuenta el verificador en `events` | real | k2 |
| `mutation_detection` | Poder de detección | Con cada mutante, la suite sale con código ≠ 0; con el motor original, 0 | real y simulado | k3 |
| `external_validation` | Validación externa | Haraka sin modificar contra el simulador: propiedades observables desde el receptor evaluadas; el resto `not_evaluated` con motivo | real | k3 |
| `licenses` | Licencias | Inventario automático (`licenses/inventory.json`) de dependencias Go y npm con licencia; falla si alguna no está en la lista de licencias abiertas permitidas | estático | k3 |
| `offline` | Licencias (sin Internet) | La corrida ocurre en un espacio de nombres de red cuya única interfaz es `lo` | todo | k3 |

### 4.1 Claves de `details` que leen los checkers

| Propiedad | Claves obligatorias |
| --- | --- |
| `no_loss` | `messages`, `kills`, `kills_post_250`, `lost` |
| `bounded_duplicates` | `duplicates`, `duplicates_outside_window` |
| `idempotency` | `replays`, `conflicts_rejected` |
| `receiver_limits` | `violations` (`{perfil: {ip: n}}`) |
| `backoff_expiry` | `max_interval_s`, `expired`, `dsn_null_sender` |
| `classification` | `cases`, `matched`, `decisions` (ruta a un JSONL con `id`, `class`, `subtype`, `rule_id` por caso del corpus) |
| `dns_mx` | `null_mx_domain`, `fallback_domain` |
| `determinism` | `runs` (tres directorios: A, B con la misma semilla, y otra semilla), `hash_a`, `hash_b`, `hash_other` |
| `ip_warmup` | `plan` (`{ip: {"daily": [..], "hourly": [..], "start_day": n}}`: listas por día del plan, `null` sin tope; `start_day` es el día del plan en que está la IP al empezar la corrida, desde 1; el día `d` de la corrida, contado desde 0 en `at_ms // 86400000`, usa el índice `start_day - 1 + d` y, pasado el final, el último valor), `known_ips`, `throttle_rate_known`, `throttle_rate_unknown`. Los topes se cuentan como eventos `data_end` del receptor por `src_ip`; la tasa de estrangulamiento es la fracción de respuestas 4xx (`reply` y `data_end`) por grupo de IPs |
| `tenant_pause` | `tenant`, `threshold`, `window`, `pause_bound_s`, `rate_ratio_others`. El `detail` de `tenant_paused` lleva `bounce_rate`, `window` y `threshold`. El cruce ocurre en el primer resultado terminal del tenant (`delivered` o `bounced`) en que los últimos `window` resultados están completos y su fracción de `bounced` supera `threshold`. `rate_ratio_others` es la tasa de entrega de los demás tenants con el tenant malo dividida por la de la corrida base; debe ser ≥ 1 / margen de aislamiento |
| `destination_isolation` | `runs` (`{"baseline": dir, "throttled": dir}`), `throttled_domain`, `margin` (igual al de `lab/params.json`), `baseline`, `throttled` (cada uno `{"p50_ms", "p99_ms"}`, percentil por rango más cercano del tiempo entre `accepted` y el primer `delivered` de cada mensaje de otro dominio) |
| `tls_policy` | `required_domain`, `opportunistic_domain` |
| `outbound_auth` | `good_domain`, `broken_domain` |
| `external_validation` | `mta`, `version`, `evaluated` (lista de nombres), `not_evaluated` (`{nombre: motivo}`) |
| `offline` | `interfaces` |

Cuando `trace` apunta a un directorio con varias corridas, cada una vive en un subdirectorio con el formato 3.6.

### 4.2 Herramienta de depuración

- `mtaq timeline --db <engine.db> --msg <msg_id>` imprime `{"msg_id": ..., "events": [...]}` con los eventos del mensaje en orden de `seq`; `--all` imprime una línea JSON por mensaje.
- `mtaq agg --db <engine.db> --by <dim,...>` imprime una lista JSON de objetos con las dimensiones pedidas y `count`. Dimensiones: `tenant`, `domain`, `provider`, `mx_host`, `src_ip`, `msg_type`, `kind`.
- Ambos aceptan filtros `--tenant`, `--domain`, `--provider`, `--src-ip`, `--msg-type`, `--kind`.

### 4.3 Mutantes

Variantes por etiqueta de compilación (`go build -tags <nombre>`, binario en `bin/mut/<nombre>/mtaengine`); el binario original no contiene su código. Mínimo seis, con estos nombres:

| Mutante | Falla sembrada | Propiedad que debe caer |
| --- | --- | --- |
| `mut_no_backoff` | Reintenta con intervalo constante mínimo | `backoff_expiry` |
| `mut_retry_permanent` | Trata los 5xx como transitorios | `outbound_auth` |
| `mut_ignore_conn_limit` | Ignora el límite de mensajes por conexión aprendido | `receiver_limits` |
| `mut_ack_before_persist` | Responde 202 antes del commit | `no_loss` |
| `mut_shared_queue` | Una sola cola para todos los destinos | `destination_isolation` |
| `mut_exceed_warmup` | Ignora el tope horario del plan | `ip_warmup` |

`details` de `mutation_detection`: `original_exit_code` y una entrada por mutante con `name`, `exit_code` y `failed_properties`. Cada corrida de mutante deja su reporte en `out/runs/mutation/<nombre>/report.json`.

### 4.4 Inyección de fallas

- `kill -9` del motor en instantes aleatorios sembrados.
- Ventana entre 250 y registro durable (`post_250`): el simulador, por el socket de control, avisa al orquestador antes de enviar el 250 de un mensaje marcado; el orquestador detiene el motor con `SIGSTOP`, autoriza el 250, y lo mata con `SIGKILL`. El receptor aceptó y el motor no pudo registrarlo.
- Corte de conexiones y cambio de perfil en caliente por el socket de control.
- Latencia en `lo` con `tc qdisc ... netem` solo si un escenario la necesita (medido: funciona dentro del espacio de nombres).

## 5. Parámetros del proyecto

Valores iniciales en `lab/params.json`. Un sprint puede endurecerlos (corridas más grandes, márgenes o plazos menores); aflojar uno exige una medición documentada en el contrato y el CHANGELOG. Los mínimos de la idea no se aflojan.

| Clave en `lab/params.json` | Valor inicial | Mínimo de la idea | Base |
| --- | --- | --- | --- |
| `campaign_messages` | 10 000 | 10 000 | idea |
| `campaign_kills`, `campaign_kills_post_250` | 30 y 5 | 25 en total | idea |
| `retry_policy` (`initial_s`, `factor`, `max_s`, `lifetime_s`) | 1 800, 2, 10 800, 432 000: primer reintento a 30 min, máximo 3 h, vida 5 días | — | R-5321 §4.5.4.1 (≥ 30 min, dos intentos en la primera hora, luego cada 2–3 h, abandono a 4–5 días) |
| `campaign_retry_policy` | 1, 2, 8, 600 | — | supuesto: solo para que la corrida real drene; los parámetros deben ser configurables (R-5321 §4.5.4.1) |
| `isolation_margin` | 1.10 (p50 y p99 ≤ 1.10 × base) | — | supuesto |
| `tenant_bounce_threshold`, `tenant_window`, `tenant_pause_bound_s` | 0.04, 200 resultados, 60 s virtuales | — | 4 %: R-RESEND-WARMUP; ventana y plazo: supuesto |
| `warmup_plan` | `daily` 150/250/400/700/1000/1500/2000; `hourly` sin tope los días 1–3 y 50/75/100/150 los días 4–7 | — | R-RESEND-WARMUP |
| `warmup_share_tolerance` | 0.05 (diferencia absoluta entre la fracción enviada por IP y la del plan) | — | supuesto |
| `memory_limit_bytes` | 8 589 934 592 (falla si se supera) | 8 GiB | idea |
| Periodo de adaptación | declarado por perfil (`adaptation_period_s`) | — | supuesto por perfil |

## 6. Decisiones cerradas

Las decisiones D01 a D12 son las delegadas por la idea, en su orden. Desde D13 son las que abrió el diseño.

**D01. Lenguajes.**
Decisión: motor, simulador, simulación determinista y `mtaq` en Go 1.27; verificador y orquestador en Python 3 con solo la biblioteca estándar.
Razón: Go es el stack de la vacante, da concurrencia, TLS y red en la biblioteca estándar y etiquetas de compilación para los mutantes; el reloj virtual se logra inyectando `Clock` en un núcleo de un hilo. Un verificador en otro lenguaje no puede compartir código con el motor por accidente, y Python trae JSON, SQLite, `hashlib` y un analizador estricto de RFC 5322 sin dependencias.

**D02. Arquitectura y fronteras.**
Decisión: procesos separados en modo real (`mtasim`, `mtaengine`, orquestador Python) unidos por SMTP, HTTP y archivos crudos con los formatos de la sección 3; en modo simulado, `mtadst` une núcleo y simulador en un proceso.
Razón: el verificador observa solo desde el receptor y el registro, como pide la idea, y los formatos fijos permiten decidir cada propiedad sobre bytes crudos.

**D03. Almacén durable.**
Decisión: SQLite (`modernc.org/sqlite` v1.61.0) en WAL con `synchronous=FULL` y commits agrupados.
Razón: medido en esta máquina: 30 `kill -9` aleatorios durante escrituras confirmadas, 0 pérdidas e integridad correcta; ~709 commits/s con FULL (por eso se agrupan). Postgres no está instalado y no hace falta para una sola máquina.

**D04. Orden de las fases.**
Decisión: K1 núcleo (simulador, motor, clasificación, backoff, DNS, determinismo, no pérdida y el comando único); K2 rutas, tenants, TLS, autenticación y depuración; K3 mutantes, validación externa, licencias y entrega.
Razón: cada K deja `make verify` pasando con más propiedades; los mutantes van al final porque necesitan todas las propiedades que deben hacer caer.

**D05. Reloj virtual y determinismo.**
Decisión: núcleo de un hilo con `Clock` y `math/rand/v2` sembrado inyectados; ejecutor simulado con cola de eventos ordenada por (tiempo, secuencia); flujos de azar derivados por componente desde la semilla.
Razón: elimina el no determinismo del planificador de goroutines y permite probar días de backoff en segundos.

**D06. Inyección de fallas y aislamiento de red.**
Decisión: `unshare --user --map-root-user --net --mount` para cada corrida; señales `SIGKILL`/`SIGSTOP` para muertes; socket de control del simulador para cortes, cambios de perfil y la ventana `post_250`; `tc netem` solo si un escenario lo pide.
Razón: medido sin root en esta máquina: puertos 25 y 53, IPs de origen 127.0.x.x, Internet inalcanzable y netem funcionan dentro del espacio de nombres.

**D07. DNS, CA de prueba, DKIM/SPF/DMARC y analizador RFC 5322.**
Decisión: DNS autoritativo propio con `github.com/miekg/dns` v1.1.73 dentro de `mtasim`; CA de prueba generada en Go con `crypto/x509`; firma y verificación DKIM con `github.com/emersion/go-msgauth` v0.7.0; SPF con `blitiri.com.ar/go/spf` v1.6.0 (`WithResolver`); DMARC evaluado por el simulador según RFC 9989 (búsqueda por árbol DNS, `p`, `sp`, `np`, `adkim`, `aspf`); analizador RFC 5322: `email` de Python con `policy.strict`.
Razón: unbound, knot, nsd, opendkim y swaks no están instalados y K0 no instala paquetes del sistema; estas bibliotecas son de código abierto (BSD-3 y MIT, verificado) y no requieren root. Ninguna biblioteca Go revisada implementa RFC 9989, y el subconjunto que usa la zona de laboratorio es pequeño.

**D08. MTA externo para la validación.**
Decisión: Haraka 3.3.5 (MIT), instalado con `npm ci` en `external/haraka/`, configurado con `local_mx_ok=true`, `temp_fail_intervals` en segundos (mínimo 5 s según su código) y el plugin `relay` para aceptar desde `127.0.0.1`; resuelve MX con el `resolv.conf` del espacio de nombres.
Razón: probado en esta máquina: instala y arranca con Node 26.12.0 sin root. Postfix exige root y usuarios propios (inviable en un espacio de nombres sin privilegios), KumoMTA exige compilar un proyecto Rust grande y mox es un servidor completo cuyo envío directo depende del puerto 25 sin intervalos configurables por archivo.

**D09. Pruebas, propiedades y mutación.**
Decisión: `testing` de Go y `unittest` de Python; las propiedades son corridas sembradas del verificador; los mutantes son etiquetas de compilación de la sección 4.1.
Razón: sin dependencias extra y con el binario original libre de código mutante.

**D10. Parámetros de las corridas y forma del reporte.**
Decisión: los de la sección 5 y el formato 3.7.
Razón: los mínimos son los de la idea; lo demás se marca supuesto y se puede subir tras medir. La única medición posible antes del motor (SQLite) ya está en D03.

**D11. Brechas de la investigación.**
Decisión: K0 leyó RFC 5321 §4.5.4, §4.5.5 y §5.1, RFC 7505 §3–4, RFC 9989 §4.10, RFC 3463 §2 y RFC 3464 §2, y las páginas de Google, Yahoo, Microsoft y Resend citadas en 3.2 y 5; cada prompt manda leer las secciones del RFC que implementa, en la revisión fijada en RESEARCH. Gmail sigue sin fuente primaria de directrices: `gmail-model` lo declara en `limitations`.
Razón: la idea exige leer el RFC antes de implementar su comportamiento; los RFC son inmutables y RESEARCH fija su sha256.

**D12. Presentación al revisor.**
Decisión: README con qué es, el comando único, cómo leer `out/report.json` y el estado por K; LIMITATIONS como fuente única de límites; CHANGELOG con cambios y decisiones de producto.
Razón: el revisor necesita un comando, un reporte y una lista honesta de lo que no se demuestra.

**D13. Un destinatario por mensaje.**
Decisión: cada petición de ingreso lleva exactamente un destinatario.
Razón: el estado por destinatario es la unidad de la no pérdida; agrupar RCPT (RFC 5321 §4.5.4.1) es una optimización que no cambia ninguna propiedad.

**D14. Respuestas sin regla.**
Decisión: una respuesta sin regla se clasifica `undetermined` y el motor la trata como transitoria hasta agotar la vida del mensaje.
Las reglas genéricas por clase solo existen para 4xx (transitoria, R-3463); un 5xx sin regla específica también es `undetermined`.
Razón: la idea pide no clasificarla nunca como permanente; reintentar hasta expirar no pierde el mensaje y termina en estado terminal. Un permanente erróneo suprime una dirección válida.

**D15. Correlación entre motor y receptor.**
Decisión: el motor agrega `X-Mtaproof-Id: <msg_id>` y un `Message-ID` derivado del `msg_id`; los `msg_id` son secuenciales por almacén.
Razón: el verificador correlaciona sin leer memoria del motor, y los ids secuenciales mantienen el determinismo.

**D16. Políticas TLS.**
Decisión: `opportunistic` usa STARTTLS si se anuncia, sin verificar el certificado, y cae a texto plano si el handshake falla; `required` exige STARTTLS con certificado válido para la CA de prueba y el nombre del MX, y si falla difiere con subtipo `TLSPolicy`.
Razón: es la distinción que pide el criterio y la que R-3207 deja a la política local.

**D17. Memoria y duración.**
Decisión: `make verify` corre dentro de `systemd-run --user --scope`; el verificador lee `memory.peak` de su cgroup al terminar.
Razón: medido: `memory.peak` cuenta todos los procesos del scope (motor, simulador, Haraka), no solo el mayor.

**D18. Licencia.**
Decisión: MIT.
Razón: el repositorio será público y todas las dependencias elegidas son MIT, BSD o Apache.

**D19. Conclusiones permitidas por comparación.**
Decisión: `destination_isolation` compara corridas equivalentes (mismo contrato y semilla): permite concluir una razón de degradación. Motor contra Haraka es distinto: no se concluye nada de velocidad, solo veredictos por propiedad.
Razón: método del proyecto para comparaciones.

## 7. Contradicciones resueltas

- **Respuesta IPTS04.** La idea agrupa `421 4.7.0 [TSS04]/[IPTS04]`; la única fuente muestra `421 [IPTS04] ...` sin código extendido. Se usa el texto de la fuente; el corpus incluye ambos casos tal como aparecen.
- **CI y comando de chequeo.** El método dice que el CI corre solo los tests; kiln exige que el CI corra el `check_command`. `make check` es formato, `vet` y tests, sin integración ni red, y el CI lo corre.
- **Mutantes.** El método deja fuera los mutantes como puerta de cierre; la idea los exige como producto (criterio de poder de detección). Son producto: los corre `make verify`, no un gate de K.
- **Contrato por K.** El método pide contrato al inicio de cada K; este contrato cubre todos los K desde K0. Un K solo agrega un commit de contrato si algo cambia.
