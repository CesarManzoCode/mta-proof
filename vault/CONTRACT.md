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

Diseño del ejecutor simulado (k1-05):

- Es el `Stepper` del motor: una cola de eventos ordenada por (`at_ms` virtual, secuencia de inserción). Un evento es una pasada del programador (`Dispatch`), una acción del núcleo (resolver, abrir, una transacción, cerrar), una entrada externa (llegada de un mensaje) o la reanudación de una conexión que esperaba. Tras cada evento se confirman las escrituras del núcleo y se encolan, en orden, las acciones que devolvió y una pasada del programador en el mismo instante.
- La conversación de cada conexión (`openConn`, `transact`, `quit`, el mismo código del ejecutor real) corre como corrutina (`iter.Pull` de la biblioteca estándar): solo avanza cuando el `Stepper` la reanuda y le devuelve el control cuando el transporte espera. Nunca corren dos piezas de código a la vez, así que ninguna goroutine decide el orden de la salida.
- El transporte en proceso (`internal/dst`) abre una sesión de `internal/sim` en el host dueño de la dirección destino según la zona, como `mtasim` escucha en ella; las órdenes y los datos pasan por la máquina de sesión, los datos se codifican y decodifican con los mismos códecs de los dos lados del socket, y la respuesta vuelve como bytes de la red leídos por el lector de respuestas del motor. Una dirección sin host servido rechaza la conexión.
- Tiempo: la transferencia por la red y el DNS (la zona de laboratorio contestada en proceso por el mismo código del servidor DNS) no consumen tiempo virtual; la latencia `latency` del perfil sí: la conexión que espera la respuesta duerme ese tiempo y las demás siguen. Una latencia mayor que el plazo del motor (`timeouts.command_s`, `data_end_s`) corta la conexión al vencer el plazo.
- Flujos de azar: `engine.core` y `engine.mx` del motor y `dst.arrivals` de las llegadas, todos derivados de la semilla de la corrida (`--seed` reemplaza `seed` de la configuración).
- Fin: la corrida termina cuando no queda ningún evento o al llegar a `duration_s`. Las sesiones aún abiertas reciben un `drop` con regla `shutdown`, como al detener `mtasim`, y un intento a medio camino queda `inflight`, como tras una muerte.

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
- `mtasim serve --out <dir> [--dns 127.0.0.1:53] [--smtp-port 25] [--profiles profiles] [--zone lab/zone.json] [--control <dir>/control.sock]`: cada host de cada perfil escucha en sus direcciones A de la zona (3.9) y en el puerto indicado; imprime `ready` en la salida estándar cuando todo escucha. Fuera del espacio de nombres se prueba con puertos altos, porque Linux enruta todo `127.0.0.0/8` por `lo`.
- `mtaengine serve --db <engine.db> [--http 127.0.0.1:8025] [--seed 1] [--max-batch 256]` (diseño, k1-02): recupera el almacén, confirma los eventos `recovered`, abre la API de ingreso (3.10) e imprime `ready` cuando escucha.
- `mtaengine run --config <engine.json> --db <engine.db>` (diseño, k1-04): lo mismo que `serve` más la entrega (2.6), con la configuración del archivo; la del laboratorio es `lab/engine.json`.
- `mtadst run --scenario <archivo> --seed <n> --out <dir> [--root .] [--research vault/RESEARCH.md]` (diseño, k1-05): ejecuta un escenario (3.11) en modo simulado y escribe en `<dir>`, que debe no existir o estar vacío, los archivos de 3.6; imprime un resumen JSON (estados finales, eventos, tiempo virtual y de pared, `trace_sha256`). No abre sockets.
- `mtadst classify --corpus corpus/classification.json --out <archivo>` (diseño, k1-05): clasifica cada caso del corpus (3.8) con la tabla del motor y escribe una línea JSON por caso, en el orden del corpus, con `id`, `class`, `subtype` y `rule_id`; no copia lo esperado.

### 2.4 Almacén

SQLite mediante `modernc.org/sqlite` (Go puro, sin CGO), en modo WAL con `synchronous=FULL`. El motor responde 202 al ingreso solo después del commit; registra el resultado de un intento en una transacción antes de considerarlo hecho. Se permite agrupar commits (group commit) de peticiones concurrentes.

En modo simulado (`mtadst`, k1-05) el almacén usa WAL con `synchronous=OFF`: allí no muere ningún proceso, así que la durabilidad no cambia ninguna salida, y el orden y el contenido de los commits son los mismos. Medido: el escenario `limits` tarda 54,8 s con `FULL` y 5,0 s con `OFF` en el disco de esta máquina, con el mismo `trace.sha256` (RESEARCH, «Mediciones»).

Diseño (k1-02):

- Tablas `messages` (una fila por mensaje con su estado, número de intentos, próximo intento y el mensaje RFC 5322 ya construido, que se envía igual en cada intento), `idempotency_keys` y `events` (3.3). Estados: `queued`, `inflight` y los terminales de 3.3.
- El núcleo guarda el estado en memoria y produce una lista ordenada de escrituras; el ejecutor real ejecuta en una goroutine las operaciones que llegaron mientras se confirmaba el grupo anterior (hasta `--max-batch`), confirma sus escrituras en una sola transacción y solo entonces responde a cada una. Si un commit falla, el motor se detiene: la memoria quedó por delante del disco y se reconstruye al arrancar.
- Recuperación al arrancar: cada mensaje `inflight` (intento con `attempt_started` sin resultado) vuelve a `queued`, vencido en el instante del arranque, con un evento `recovered` cuyo `attempt` es el del intento abandonado. Los eventos se confirman antes de abrir la API.

### 2.5 Comandos

| Comando | Qué hace | Red |
| --- | --- | --- |
| `make deps` | `go mod download` y `npm ci` en `external/haraka` | Sí, una vez |
| `make build` | Compila `bin/` y los mutantes en `bin/mut/<nombre>/` | No |
| `make check` | `gofmt -l`, `go vet ./...`, `go test -count=1 ./...` y `python3 -m unittest` del verificador. Es el `check_command` del plan y lo corre el CI | No |
| `make verify` | El comando único de verificación del README. Corre `make build`, entra en un scope de systemd de usuario y en el espacio de nombres, ejecuta todas las propiedades y escribe `out/report.json`. Sale con 0 solo si todas pasan; si no, imprime cuáles fallaron | No |

`make check` no requiere espacios de nombres ni red: las pruebas de integración viven en `make verify`.

### 2.6 Entrega (diseño, k1-04)

**Configuración** (`lab/engine.json`; el cargador rechaza campos desconocidos): `http`, `dns` (dirección `ip:puerto` a la que el motor consulta; nunca lee `resolv.conf`), `smtp_port`, `hostname` (EHLO y `Reporting-MTA`), `postmaster` (`From` de los DSN), `seed`, `max_batch`, `src_ips` (IPs de origen, en orden), `route_ttl_s` (300, como la zona), `idle_s` (cierre de conexiones ociosas), `timeouts` (`connect_s`, `command_s`, `data_end_s`; supuesto), `retry_policy` (sección 5), `providers` (`name` y `mx_suffixes`), `limits` y `adapt.rate_window_s`.

**Resolución** [R-5321 §5.1, R-7505]: consulta MX por UDP (TCP si la respuesta viene truncada) a la dirección configurada. `NXDOMAIN` → `bounced` permanente sin intento (`rfc5321.nxdomain`); cualquier otra falla del DNS → `deferred` sin intento con el calendario de reintentos (`rfc5321.dns_temporary`); sin registros MX → MX implícito (el dominio, preferencia 0); un único MX `.` de preferencia 0 → null MX: eventos `routed` (`null_mx`), `classified` con `556 5.1.10` (regla `rfc7505.null_mx`) y `bounced` `NullMX`, sin conexión ni reintento; MX presentes sin ningún host con dirección A → `bounced` permanente (`rfc5321.mx_unusable`). El núcleo ordena por preferencia y baraja los de igual preferencia con el flujo `engine.mx` de la semilla, después de ordenarlos por nombre, para que el orden no dependa del orden de la respuesta DNS. No se siguen CNAME. La ruta se reusa `route_ttl_s`; el proveedor es el del primer `mx_suffixes` que coincide con el MX más preferido.

**Transporte y conversación**: interfaz `Transport` (`Dial` desde una IP de origen) y `Conn` a nivel de respuestas; el ejecutor real usa TCP con la IP de origen enlazada. Al abrir prueba los MX en orden y las direcciones de cada uno en orden, y pasa al siguiente ante falla de conexión sin respuesta. Un saludo o un HELO rechazado con respuesta termina el intento con esa respuesta (el receptor contestó; se clasifica). Si ninguna dirección sirve, `deferred` contado (`rfc5321.mx_unreachable`). Transacción: `MAIL FROM`, `RCPT TO` (un destinatario, D13), `DATA`; tras un rechazo a `MAIL`, `RCPT` o `DATA` se envía `RSET` y la conexión sigue; un 421 la cierra.

**Programador**: una cola por destino, clave (tenant, dominio), ordenada por (`next_at_ms`, secuencia). `Dispatch` recorre las colas por turnos rotativos, un mensaje por cola por vuelta, hasta que ninguna avanza; una cola bloqueada por sus límites, su ruta o su calendario no frena a las demás. Una conexión pertenece a una cola y, al terminar una transacción, toma el siguiente mensaje de su cola; se cierra al llegar a su límite de mensajes, tras `idle_s` ociosa o, para no acaparar, si otra cola no pudo abrir por un límite de conexiones que esta conexión ocupa.

**Límites** (0 o ausente = sin límite; `"*"` es el valor por defecto de cada ámbito): `connections` (conexiones abiertas), `msgs_per_conn` (transacciones por conexión) y `rate` (`limit` intentos iniciados por ventana deslizante de `per_s` segundos). Ámbitos: `domain` (todo lo que va al dominio), `provider` (por par proveedor e IP de origen, porque los proveedores limitan por IP emisora), `tenant` e `src_ip`. Se elige la primera IP de origen, por turnos, que admite una conexión más en los cuatro ámbitos.

**Adaptación** (los factores son supuesto): los límites aprendidos viven por par proveedor e IP de origen (o dominio e IP si no hay proveedor) y solo bajan. (1) Respuesta de throttling de 3.2 (regla de proveedor con resultado transitorio): tasa aprendida = la mitad de los intentos iniciados en la última ventana `adapt.rate_window_s` (mínimo 1) y conexiones aprendidas = abiertas − 1 (mínimo 1); la conexión se cierra. (2) Cierre sin respuesta al `MAIL` de una conexión que ya llevó `n` ≥ 1 transacciones [R-YAHOO]: `msgs_per_conn` aprendido = `n`; el mensaje vuelve a la cola al instante sin avanzar el calendario (`yahoo.silent_close`), porque R-YAHOO permite reconectar enseguida. Lo aprendido va en `detail.learned` del evento `deferred` (`key` y los valores nuevos). Otra pérdida de conexión sin respuesta es `deferred` contado (`engine.no_reply`).

**Reintentos y expiración** (sección 5): el k-ésimo diferimiento contado espera `min(initial_s · factor^(k−1), max_s)`; `messages.retries` guarda k. Si el siguiente intento caería después de `aceptado + lifetime_s`, el mensaje expira en ese momento (evento `expired` con la última respuesta); un mensaje que llega a su turno con la vida agotada expira sin intento (`detail.reason` `lifetime`). Así ningún intento ocurre pasada la vida y los intervalos programados no decrecen.

**DSN** [R-3464 §2, R-5321 §4.5.5]: al expirar, el motor crea un mensaje nuevo (su propio `msg_id`, evento `accepted` con `detail.dsn_for`) con reverse-path nulo dirigido al reverse-path original, y registra `dsn_enqueued` en el original con `detail.dsn_msg_id`. Es `multipart/report; report-type=delivery-status` con una parte de texto y una `message/delivery-status` en 7bit: `Original-Envelope-Id` (el `msg_id`, que el motor entregó al remitente al ingresar, §2.2.1), `Reporting-MTA`, `Arrival-Date`; `Final-Recipient`, `Action: failed`, `Status` (el código extendido de la última respuesta, o `4.4.7` si no hay [R-3463 §3.5]), `Remote-MTA` y `Diagnostic-Code` si hubo respuesta, y `Last-Attempt-Date` si el último intento terminó al expirar. No devuelve el mensaje original. Un mensaje con reverse-path nulo nunca genera DSN. Los rebotes permanentes no generan DSN: quedan en los eventos.

**Supresión**: un `bounced` con subtipo `NoEmail` suprime el par (tenant, dirección) (parte local tal cual, dominio en minúsculas) en la tabla `suppressions`; un mensaje posterior a esa dirección del mismo tenant termina `suppressed` al llegar su turno, sin intento, con `detail.suppressed_by`.

**Durabilidad y orden**: el núcleo inicia el intento (`attempt_started`) dentro de la misma operación que decide enviarlo, y el ejecutor no toca la red hasta que esa operación está confirmada; el resultado de la transacción (`delivered`, `deferred`, `bounced`, `expired`) se confirma antes de que la conexión o el programador hagan otra cosa con ese mensaje. Un 250 recibido y no confirmado deja el mensaje `inflight`: la recuperación lo reencola y el reenvío es el duplicado acotado de 4. Ejecutores: `Deliverer` (real: una goroutine por conexión y por resolución, todo a través del `Runner` de 2.4) y `Stepper` (simulado: un hilo lógico, reloj virtual, cola de eventos ordenada por tiempo y secuencia, un evento a la vez y commit tras cada uno; ver 2.2).

**Eventos de un intento**: `attempt_started` (en conexión nueva sin `mx_host`), `routed` (`mx_host` y `detail`: `conn`, y `mx` en orden, `addr` y `tried` con las direcciones fallidas en conexión nueva, o `reused` en conexión reusada), `classified` si hubo rechazo, y el resultado. `detail` del resultado: `conn` y `stage` (`connect`, `ehlo`, `mail`, `rcpt`, `data`, `data_end`), `error` si no hubo respuesta, `learned`, `suppressed`.

| `rule_id` sin respuesta del receptor | Clase / subtipo | Fuente |
| --- | --- | --- |
| `rfc5321.nxdomain` | `permanent` / `General` | R-5321 §5.1 |
| `rfc5321.mx_unusable` | `permanent` / `General` | R-5321 §5.1 |
| `rfc5321.dns_temporary` | `transient` / `Connection` | R-5321 §5.1 |
| `rfc5321.mx_unreachable` | `transient` / `Connection` | R-5321 §5.1 |
| `yahoo.silent_close` | `transient` / `Connection` (no avanza el calendario) | R-YAHOO |
| `engine.no_reply` | `transient` / `Connection` | supuesto |

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

Parámetros por tipo (diseño, k1-01). Los límites cuentan solo comandos aceptados; el comando que excede el límite es la violación.

| Tipo | `params` | Dónde se evalúa | Violación |
| --- | --- | --- | --- |
| `max_msgs_per_conn` | `limit` | `MAIL` con `limit` mensajes ya aceptados en la conexión | sí |
| `rate` | `scope` (`src_ip`, `profile` o `rcpt`), `limit`, `per_s` | Ventana deslizante de `per_s` segundos: `MAIL` para `src_ip` y `profile`, `RCPT` (por destinatario) para `rcpt` | sí |
| `concurrency` | `limit` | Conexión nueva con `limit` conexiones ya abiertas desde la misma IP de origen | sí |
| `unknown_ip` | `known_ips` (direcciones o prefijos) y al menos uno de `max_msgs_per_conn`, `rate` (`limit`, `per_s`, `scope` opcional, por defecto `src_ip`) y `concurrency` | Como cada límite, solo para IPs de origen fuera de `known_ips` | sí, con el id de la regla `unknown_ip` |
| `respond` | `stage` (`connect`, `mail`, `rcpt`, `data_end`), `match` (`rcpt_local_prefix`, `rcpt_domain`, `mail_from_domain`, `src_ips`; vacío coincide siempre), `response`, `close` | Primera regla del perfil que coincide | no |
| `drop_mid_data` | `every_n` (por defecto 1; contador de `DATA` por perfil), `after_bytes` | Tras el 354 lee como máximo `after_bytes` de datos y cierra sin respuesta ni `data_end` | no |
| `latency` | `stage` (`connect`, `ehlo`, `mail`, `rcpt`, `data`, `data_end`, `any`), `ms` | Se suma antes de enviar la respuesta; en `mtadst` avanza el reloj virtual | no |
| `auth` | `require` (`spf`, `dkim`, `dmarc`), `fail_response`, `dmarc_reject_response` | Se acepta y valida; se aplica desde k2-04 | no |

- `on_violation.action` ∈ `close_silent` (cierra sin respuesta), `respond` (responde y sigue) y `respond_close` (responde y cierra), con `response` en los dos últimos. Un 421 siempre cierra [R-5321 §3.8] y una violación al conectar también.
- El cargador rechaza campos desconocidos, una respuesta que no sea exactamente una fila de 3.2 (código, código extendido y texto; `556 5.1.10` es solo del motor), una regla cuyo `source` no esté en `sources` del perfil y una respuesta cuya fuente en 3.2 no esté en `sources`. Exige `limitations` si alguna fuente del perfil es secundaria en RESEARCH o alguna respuesta viene de una fuente secundaria. Los marcadores `domain-name` y `<domain>` se sustituyen al responder por el dominio del `MAIL FROM`.
- El nombre del perfil es el nombre del archivo. Un host pertenece a un solo perfil; un perfil sin `hosts` solo se usa por cambio en caliente.

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

`detail` de los eventos de entrega (diseño, k1-04): ver 2.6.

`detail` de los eventos de ingreso (diseño, k1-02): `accepted` lleva `payload_sha256` e `idempotency_key` si hubo clave; `idempotent_replay` e `idempotency_conflict` llevan `idempotency_key` y el `payload_sha256` de la petición, y su `msg_id` es el del mensaje que creó la clave; `recovered` lleva `from_state`. Las claves de `detail` van ordenadas.

### 3.4 Registro del receptor (`receiver.jsonl`)

Una línea JSON por evento, en orden de escritura, con `fsync` al cerrar cada respuesta a `DATA`:

`seq`, `at_ms`, `conn`, `src_ip`, `dst_ip`, `host`, `profile`, `ev` (`connect`, `ehlo`, `starttls`, `mail`, `rcpt`, `data_end`, `reply`, `close`, `drop`, `violation`), y según el evento `mail_from` (cadena vacía para el reverse-path nulo), `rcpt`, `msg_id` (cabecera `X-Mtaproof-Id`), `sha256` del mensaje, `code`, `enhanced`, `text`, `rule`, `tls` y `auth` (`spf`, `dkim`, `dmarc`, cada uno `pass` o `fail`).

- `data_end` es una línea por cada DATA terminado y lleva la respuesta final a DATA (`code`, `enhanced`, `text`) junto con `msg_id` y `sha256`, además de `mail_from` y `rcpt` (destinatarios unidos por comas). Se escribe y se sincroniza antes de enviar la respuesta. `reply` es la respuesta a cualquier otro comando y al saludo (220 o la respuesta de una regla al conectar).
- `ehlo`, `mail` y `rcpt` se anotan solo para comandos bien formados y en secuencia; un comando rechazado por sintaxis o secuencia deja solo su `reply`.
- `close`: la conexión terminó tras una respuesta (`QUIT`, 421, `close`) o la cerró el cliente. `drop`: el simulador la terminó sin respuesta; `rule` es el id de la regla, `control.cut` (corte por el socket de control) o `shutdown`.
- `violation` lleva `rule` y `counted` (`false` dentro del periodo de adaptación, `true` después).
- El mensaje recibido se guarda además como `messages/<sha256>.eml`.

### 3.5 Registro del orquestador (`harness.jsonl`)

`at_ms`, `action` (`start`, `kill`, `profile_switch`, `cut`, `hold_250`, `release_250`), `pid`, `window` (`random` o `post_250`), `msg_id` cuando aplica.

### 3.6 Corrida

En modo real, `at_ms` de los tres registros son milisegundos de reloj de pared (época Unix) de la misma máquina, así que se pueden ordenar entre sí; en modo simulado son milisegundos virtuales desde el inicio del escenario.

Cada escenario escribe `out/runs/<escenario>-<semilla>/` con `receiver.jsonl`, `engine.db`, `harness.jsonl` (si hubo fallas), `messages/` y, en modo simulado, `events.jsonl` (exportación canónica de `events`: una fila por línea, ordenada por `seq`, objeto JSON con claves ordenadas y separadores `,` y `:` sin espacios) y `trace.sha256` (hex del sha256 de los bytes de `receiver.jsonl` seguidos de los de `events.jsonl`). `out/` no se versiona.

Diseño (k1-05): cada línea de `events.jsonl` tiene todas las columnas de 3.3, `NULL` como `null`, enteros sin decimales y cadenas con los escapes de `json.dumps` de Python con `ensure_ascii` (`\"`, `\\`, `\n`, `\r`, `\t`, `\b`, `\f` y todo otro carácter fuera de espacio..`~` como `\uXXXX`, con pares sustitutos sobre U+FFFF); es decir, los bytes de `json.dumps(dict(fila), sort_keys=True, separators=(",", ":"))` de cada fila leída con `sqlite3`, así el verificador la reconstruye desde `engine.db` sin código del motor. `detail` va como la cadena guardada. `trace.sha256` contiene el hex seguido de un salto de línea.

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

Diseño (k1-03):

- Campos del caso: `provider` es el nombre del perfil del destino (`gmail-model`, …) o vacío si la respuesta no es de un proveedor; `source` es exactamente un identificador de RESEARCH; un caso `undetermined` espera `expected_subtype` vacío.
- El clasificador (`internal/classify`) recibe proveedor, código, código extendido y texto, y evalúa una tabla de reglas en tres niveles: (1) reglas de proveedor, que coinciden con el inicio literal de un texto de 3.2 o con su etiqueta (`[TSS04]`, `[IPTS04]`); (2) reglas por código extendido [R-3463 §3, R-7505 §4.1]; (3) la regla genérica de clase 4. Dentro de un nivel gana la primera en orden de tabla. Sin coincidencia, `undetermined` con `rule_id` `d14.undetermined` (D14). Toda decisión trae `rule_id`.
- Un código extendido solo coincide si su clase es la del código de respuesta. En los textos con marcador (`domain-name`, `<domain>`) el marcador vale por un dominio sin espacios o por sí mismo, como lo imprime la fuente.
- La tabla rechaza al cargar: reglas sin fuente, regla genérica que no sea la de clase 4 transitoria, y reglas permanentes que puedan coincidir con algo que no sea un 5xx.
- El núcleo registra cada clasificación en un evento `classified` con la respuesta (`code`, `enhanced`, `text`), `class`, `subtype` y `rule_id`.

| `rule_id` | Nivel | Condición | Clase / subtipo | Fuente |
| --- | --- | --- | --- | --- |
| `google.rate_ip` | proveedor | `421 4.7.28` + texto de 3.2 | `transient` / `RateLimited` | R-GOOGLE-ERR |
| `google.try_later` | proveedor | `421 4.7.0` + texto de 3.2 | `transient` / `General` | R-GOOGLE-ERR |
| `google.unauthenticated` | proveedor | `550 5.7.26` + texto de 3.2 | `permanent` / `AuthConfig` | R-GOOGLE-ERR |
| `google.dmarc_policy` | proveedor | `550 5.7.26` + texto de 3.2 con `domain-name` | `permanent` / `AuthConfig` | R-GOOGLE-ERR |
| `google.rcpt_rate` | proveedor | `450 4.2.1` + texto de 3.2 | `transient` / `RateLimited` | R-GOOGLE-ERR |
| `google.no_account` | proveedor | `550 5.1.1` + texto de 3.2 | `permanent` / `NoEmail` | R-GOOGLE-ERR |
| `yahoo.tss04` | proveedor | `421` + `[TSS04]` | `transient` / `General` | R-SUPED-YAHOO |
| `yahoo.ipts04` | proveedor | `421` + `[IPTS04]` | `transient` / `Reputation` | R-SUPED-YAHOO |
| `microsoft.auth_level` | proveedor | `550 5.7.515` + texto de 3.2 con `<domain>` | `permanent` / `AuthConfig` | R-MS-515 |
| `microsoft.dmarc_reject` | proveedor | `550 5.7.509` + texto de 3.2 con `<domain>` | `permanent` / `AuthConfig` | R-MS-509 |
| `rfc7505.null_mx` | extendido | `556 5.1.10` | `permanent` / `NullMX` | R-7505 §4.1 |
| `rfc3463.bad_mailbox` | extendido | `5.1.1` («only useful for permanent failures») | `permanent` / `NoEmail` | R-3463 §3.2 |
| `rfc3463.mailbox_full` | extendido | `4.2.2` o `5.2.2` («should be used as a persistent transient failure») | `transient` / `MailboxFull` | R-3463 §3.3 |
| `rfc3463.mailbox_length` | extendido | `5.2.3` («should be used as a permanent failure») | `permanent` / `MessageTooLarge` | R-3463 §3.3 |
| `rfc3463.too_big` | extendido | `5.3.4` («useful only as a permanent error») | `permanent` / `MessageTooLarge` | R-3463 §3.4 |
| `rfc3463.class4` | genérica | cualquier 4xx | `transient` / `General` | R-3463 §2 |

Los subtipos de las reglas de proveedor son diseño: las fuentes dan el texto, no una categoría. `MessageTooLarge` es permanente aquí aunque Resend lo agrupe con los transitorios [R-RESEND-BOUNCES], porque R-3463 §3.3–3.4 dice que 5.2.3 y 5.3.4 son permanentes y reenviar el mismo mensaje no cambia el resultado. `5.2.2` es transitorio aunque llegue con un 5xx, como pide R-3463 §3.3.

### 3.9 Zona de laboratorio (`lab/zone.json`)

```json
{"ttl": 300, "records": [
  {"name": "gmail-model.test", "type": "MX", "pref": 10, "host": "mx1.gmail-model.test"},
  {"name": "mx1.gmail-model.test", "type": "A", "addr": "127.0.1.1"},
  {"name": "nullmx.test", "type": "MX", "pref": 0, "host": "."},
  {"name": "sender.test", "type": "TXT", "text": "v=spf1 ..."}
]}
```

- Tipos `A`, `MX` y `TXT`. Todo nombre cuelga de `.test` y toda dirección es IPv4 de loopback; el cargador rechaza lo demás. Un null MX es el único MX de su dominio y tiene preferencia 0 [R-7505 §3].
- El DNS de `mtasim` es autoritativo para `test.` por UDP y TCP: `NXDOMAIN` para nombres inexistentes y respuesta vacía para tipos ausentes, ambos con un SOA sintético; `REFUSED` fuera de `.test`. Los TXT de más de 255 octetos se parten en cadenas de 255.

### 3.10 API de ingreso y mensaje generado (diseño, k1-02)

`POST /v1/messages` con un objeto JSON de exactamente estos campos, todos obligatorios: `tenant` (1–64 de `[A-Za-z0-9._-]`), `from` y `to` (un addr-spec ASCII cada uno, sin nombre ni lista: un destinatario por mensaje, D13), `subject` (1–900 caracteres ASCII imprimibles, para no superar 998 por línea [R-5322 §2.1.1]), `text` (UTF-8, hasta 512 KiB, puede ser vacío) y `msg_type` (`transactional` o `marketing`). Un campo desconocido, un JSON inválido o datos tras el objeto dan 400 sin evento.

- `Idempotency-Key` opcional, 1–256 caracteres ASCII imprimibles, ventana de 24 h [R-RESEND-IDEMP]. La clave es global (no por tenant): sin autenticación el `tenant` es parte del payload, y otro tenant con la misma clave es otro payload. El payload es la petición ya analizada (el orden de las claves JSON y los espacios no cuentan). Dentro de la ventana, mismo payload → 202 con el mismo `id`, cabecera `Idempotent-Replayed: true` y evento `idempotent_replay`; otro payload → 409 con el `id` original y evento `idempotency_conflict`. Una clave con 24 h o más se olvida: la petición crea un mensaje nuevo y la clave pasa a él.
- Respuestas: 202 `{"id": "<msg_id>"}` después del commit; 400 y 409 `{"error": ...}`; 503 si el motor se detuvo.
- `msg_id`: `m` seguido del número secuencial del almacén con al menos ocho dígitos (`m00000001`); sigue tras reiniciar sin huecos (D15).
- Mensaje: cabeceras `Date` (instante del ingreso en UTC, `Thu, 22 Oct 2026 13:05:09 +0000`) [R-5322 §3.3, §3.6.1], `From`, `To`, `Subject`, `Message-ID: <msg_id.mtaproof@dominio-del-from>` [R-5322 §3.6.4], `X-Mtaproof-Id: <msg_id>`, `MIME-Version: 1.0`, `Content-Type: text/plain; charset=utf-8` y `Content-Transfer-Encoding: quoted-printable`; líneas CRLF.

### 3.11 Escenario del modo simulado (`lab/scenarios/<nombre>.json`, diseño k1-05)

```json
{
  "name": "backoff",
  "description": "texto libre",
  "engine": {"base": "lab/engine.json", "set": {"retry_policy": {"lifetime_s": 3600}}},
  "zone": "lab/zone.json",
  "profiles_dir": "profiles",
  "profiles": ["always-defer", "baseline"],
  "tenants": [{"name": "acme", "from": "notify@baseline.test", "msg_type": "transactional", "weight": 1}],
  "domains": [{"domain": "always-defer.test", "local_prefix": "user", "recipients": 0, "weight": 1}],
  "messages": 3,
  "arrival": {"process": "poisson", "rate_per_s": 0.01, "start_s": 0},
  "duration_s": 518400
}
```

- El cargador rechaza campos desconocidos. Las rutas son relativas a la raíz del repositorio (`--root`). Por defecto: `engine.base` `lab/engine.json`, `zone` `lab/zone.json`, `profiles_dir` `profiles`, `msg_type` `transactional`, `local_prefix` `user`, pesos 1.
- `engine`: la configuración del motor (2.6) es `base` con `set` encima: los objetos se mezclan clave a clave y cualquier otro valor reemplaza al de la base; el resultado pasa por el cargador del motor.
- `profiles`: los perfiles que sirve el receptor; los hosts de los demás rechazan la conexión.
- Llegadas: proceso de Poisson sembrado con el flujo `dst.arrivals`: huecos exponenciales de media `1/rate_per_s` segundos desde `start_s`. Por mensaje, en este orden: el hueco, el tenant y el dominio (por peso) y, si el dominio tiene `recipients` > 0, el número del destinatario en `[0, recipients)`; con 0, el número es el del mensaje y cada destinatario es nuevo. Destinatario `<local_prefix><n>@<dominio>`, asunto `Scenario <nombre> message <i>` y texto `Message <i> of scenario <nombre>.`.
- `duration_s`: tiempo virtual máximo; el reloj empieza en 0.

Escenarios iniciales: `backoff` (un dominio con `always-defer` y el calendario de `lab/engine.json` hasta expirar, con remitente en `baseline.test` para recibir los DSN), `limits` (un dominio por perfil de proveedor con llegadas por encima de sus tasas, que siguen después del periodo de adaptación) y `determinism` (mezcla de casi todos los caminos del motor en una hora virtual).

## 4. Propiedades

Cada propiedad tiene un nombre fijo, que es su clave en el reporte. El verificador registra solo las propiedades ya implementadas; los checkers de cada K exigen las suyas.

| Propiedad | Criterio de la idea | Cómo se decide | Modo | K |
| --- | --- | --- | --- | --- |
| `no_loss` | No pérdida | Todo `msg_id` con evento `accepted` tiene un estado terminal tras reiniciar y drenar. `details`: `messages`, `kills`, `kills_post_250`, `lost` | real | k1 |
| `bounded_duplicates` | Duplicación acotada | Un `msg_id` con más de un `data_end` aceptado (2xx) en el receptor es duplicado; cada duplicado necesita un `kill` del orquestador posterior a su primer 2xx y anterior a su evento `delivered`. `details`: `duplicates`, `duplicates_outside_window` (debe ser 0) | real | k1 |
| `idempotency` | Duplicación acotada (ingreso) | Misma clave y mismo payload → un solo `accepted`; misma clave y otro payload → respuesta 409 y evento `idempotency_conflict`. `details`: `replays`, `conflicts_rejected` | real | k1 |
| `receiver_limits` | Límites del receptor | Cero violaciones (3.1) por perfil y por IP de origen. `details.violations`: `{perfil: {ip: n}}` | real y simulado | k1 |
| `backoff_expiry` | Backoff y expiración | Contra `always-defer`: intervalos entre intentos de un mismo mensaje no decrecientes, ≤ máximo y ≥ la espera del calendario de `lab/params.json` para ese reintento; ningún intento pasada la vida; `expired` al agotar la vida (no antes); DSN con `MAIL FROM:<>` recibido por el remitente. Reloj virtual | simulado | k1 |
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

Diseño del verificador (k1-06):

- `python3 -m verifier verify --out out [--only <propiedad,...>] [--root .] [--bin bin]`. Corre las propiedades implementadas en un orden fijo (`profile_sources`, `classification`, `backoff_expiry`, `receiver_limits`, `determinism`, `resources`; `resources` al final porque mide todo lo anterior), escribe `out/report.json` aunque alguna falle, imprime el veredicto de cada una y los motivos de las que fallan, y sale con 0 solo si todas pasan (1 si alguna falla). Una excepción dentro de una propiedad la hace fallar con `details.error`; no aborta la suite. `--only` con un nombre no implementado es un error de uso.
- El motor solo aparece como subproceso (`bin/mtadst`). Cada corrida simulada va a `out/runs/<escenario>-<semilla>/`; el verificador borra solo los directorios que va a escribir. `determinism` deja sus tres corridas en `out/runs/determinism-1/{a,b,seed-2}` (semillas 1, 1 y 2); `classification` deja `out/runs/classification/decisions.jsonl`. Semilla 1 en las corridas de una sola semilla; `seed` es `null` en las propiedades sin semilla (`classification`, `profile_sources`, `resources`).
- Lo que se decide sobre cada fuente: `backoff_expiry` toma los intentos y el DSN del receptor (`data_end` en `always-defer`, `data_end` 2xx con `mail_from` vacío dirigido al remitente original y su `.eml`, que debe ser `multipart/report` `delivery-status` con `Original-Envelope-Id` del original y `Action: failed`) y del registro del motor solo lo que el receptor no ve: el instante de `accepted`, el evento `expired` y qué mensaje es el DSN (`detail.dsn_for`). `receiver_limits` cuenta solo líneas `violation` con `counted: true`. `determinism` recalcula el hash desde `receiver.jsonl` y `engine.db` (3.6) y exige además que `trace.sha256` y `events.jsonl` coincidan con lo recalculado. `profile_sources` lee los perfiles y `vault/RESEARCH.md` (secundaria es la fila cuyo tipo empieza con «secundaria»). `resources` lee su propio cgroup en `/proc/self/cgroup` y su `memory.peak` en `/sys/fs/cgroup`; sin `memory.peak` legible falla.
- El calendario de `backoff_expiry` exige también que cada intervalo sea al menos la espera `min(initial_s · factor^(k−1), max_s)` de `lab/params.json`: un intervalo constante mínimo (el mutante `mut_no_backoff`, 4.3) no decrece ni supera el máximo, así que sin esta condición pasaría.
- `lo`: dentro del espacio de nombres el verificador lee y sube la bandera `IFF_UP` de `lo` con `ioctl` sobre un socket del propio espacio de nombres; `/sys/class/net` no sirve porque sigue mostrando las interfaces del anfitrión (medido en k1-06: dentro de `unshare --net` lista `docker0`, `enp5s0` y `lo` del anfitrión).
- `details` incluye, además de las claves de 4.1, `failures` (hasta 50 motivos) y `failures_total` cuando la propiedad falla, y claves propias de cada propiedad (`uncounted` en `receiver_limits`, `attempts` y `min_interval_s` en `backoff_expiry`, `memory_peak_bytes` y `cgroup` en `resources`).

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
- Protocolo del socket de control (diseño, k1-01): socket Unix de flujo; una línea JSON por petición y una línea JSON de respuesta, `{"ok": true, ...}` o `{"ok": false, "error": "..."}`. Comandos: `ping`; `switch_profile` con `host` y `profile` (o `from` y `profile` para todos los hosts de un perfil), que vale para las conexiones nuevas (las abiertas conservan su perfil; para forzarlo se combina con `cut`); `cut` con `host` o `src_ip` opcionales, que responde `cut` (número de conexiones cortadas); `hold_250` con `msg_id` opcional (vacío: el próximo mensaje aceptado); `release_250` con `msg_id` opcional, que responde `released`. Cuando un mensaje retenido termina DATA con 2xx, el simulador sincroniza su `data_end`, envía `{"ev": "held_250", "msg_id": ..., "conn": ...}` por la conexión de control que pidió la retención y no envía el 250 hasta `release_250`. Si esa conexión de control se cierra, sus retenciones se liberan; si la conexión SMTP se corta mientras espera, el 250 no se envía.
- Latencia en `lo` con `tc qdisc ... netem` solo si un escenario la necesita (medido: funciona dentro del espacio de nombres).

## 5. Parámetros del proyecto

Valores iniciales en `lab/params.json`. Un sprint puede endurecerlos (corridas más grandes, márgenes o plazos menores); aflojar uno exige una medición documentada en el contrato y el CHANGELOG. Los mínimos de la idea no se aflojan.

| Clave en `lab/params.json` | Valor inicial | Mínimo de la idea | Base |
| --- | --- | --- | --- |
| `campaign_messages` | 10 000 | 10 000 | idea |
| `campaign_kills`, `campaign_kills_post_250` | 30 y 5 | 25 en total | idea |
| `retry_policy` (`initial_s`, `factor`, `max_s`, `lifetime_s`) | 1 800, 2, 10 800, 432 000: primer reintento a 30 min, máximo 3 h, vida 5 días. El motor lo lee de su configuración (`lab/engine.json`, 2.6), que trae estos valores | — | R-5321 §4.5.4.1 (≥ 30 min, dos intentos en la primera hora, luego cada 2–3 h, abandono a 4–5 días) |
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
Decisión: SQLite (`modernc.org/sqlite` v1.61.0) en WAL con `synchronous=FULL` y commits agrupados. En modo simulado, `synchronous=OFF` (k1-05, 2.4).
Razón: medido en esta máquina: 30 `kill -9` aleatorios durante escrituras confirmadas, 0 pérdidas e integridad correcta; ~709 commits/s con FULL (por eso se agrupan). Postgres no está instalado y no hace falta para una sola máquina.

**D04. Orden de las fases.**
Decisión: K1 núcleo (simulador, motor, clasificación, backoff, DNS, determinismo, no pérdida y el comando único); K2 rutas, tenants, TLS, autenticación y depuración; K3 mutantes, validación externa, licencias y entrega.
Razón: cada K deja `make verify` pasando con más propiedades; los mutantes van al final porque necesitan todas las propiedades que deben hacer caer.

**D05. Reloj virtual y determinismo.**
Decisión: núcleo de un hilo con `Clock` y `math/rand/v2` sembrado inyectados; ejecutor simulado con cola de eventos ordenada por (tiempo, secuencia); flujos de azar derivados por componente desde la semilla. La conversación SMTP de cada conexión corre como corrutina del ejecutor (k1-05, 2.2), para que la latencia de una conexión no detenga a las demás sin introducir concurrencia real.
Razón: elimina el no determinismo del planificador de goroutines y permite probar días de backoff en segundos. Medido en k1-05: el escenario `backoff` simula 4,9 días en 63 ms de reloj, y diez corridas por escenario con `GOMAXPROCS` distinto dan un solo `trace.sha256` (RESEARCH, «Mediciones»).

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
