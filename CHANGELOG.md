# Changelog

## [Sin publicar]

### Añadido

- Simulador `mtasim serve`: receptor SMTP con perfiles declarativos (`gmail-model`, `yahoo-model`, `outlook-model`, `baseline`, `always-defer`), DNS autoritativo de laboratorio sobre `lab/zone.json`, registro `receiver.jsonl` y mensaje `messages/<sha256>.eml` sincronizados con `fsync` antes de cada respuesta a DATA y socket de control (cambio de perfil, cortes, retención del 250).
- Máquina de sesión SMTP sin sockets en `internal/sim`, para que `mtadst` la use en proceso.
- Núcleo del motor (`internal/engine`): máquina de estados de un hilo con reloj y azar inyectados (`internal/clock`: reloj real, reloj virtual y flujos de `math/rand/v2` derivados por componente), estados del mensaje, eventos de 3.3 y recuperación al arrancar con eventos `recovered`.
- Almacén `internal/store` en SQLite (`modernc.org/sqlite` v1.61.0, WAL, `synchronous=FULL`) con commits agrupados.
- Clasificador de respuestas (`internal/classify`): tabla de reglas con id, fuente, nivel (proveedor, código extendido, clase 4) y resultado; el núcleo registra `class`, `subtype` y `rule_id` en el evento `classified`.
- Corpus `corpus/classification.json` con las doce filas de 3.2, sus variantes con el dominio sustituido, códigos extendidos de RFC 3463 y casos sin regla.
- `mtaengine serve` con la API de ingreso `POST /v1/messages`: un destinatario por mensaje, `Idempotency-Key` con ventana de 24 h, 202 solo tras el commit y mensaje RFC 5322 con `Message-ID` y `X-Mtaproof-Id` derivados del `msg_id`.
- Entrega del motor y `mtaengine run --config lab/engine.json --db <engine.db>`: resolución MX contra el DNS configurado (MX implícito, null MX sin conexión, NXDOMAIN permanente, error temporal diferido), cliente SMTP detrás de una interfaz de transporte (TCP con IP de origen elegida), paso al siguiente MX ante falla de conexión, programador con una cola por (tenant, dominio) y turnos rotativos, límites de conexiones, mensajes por conexión y tasa por dominio, proveedor e IP, tenant e IP de origen, límites aprendidos ante throttling y ante el cierre silencioso de Yahoo, reintentos con calendario no decreciente y vida máxima, DSN `multipart/report` con reverse-path nulo al expirar y supresión por `NoEmail`.
- Ejecutor real (`Deliverer`) y ejecutor de un hilo con reloj virtual (`Stepper`); en ambos el intento se confirma antes de tocar la red y su resultado antes de la siguiente acción sobre el mensaje.
- Almacén: columna `messages.retries` (con migración de bases anteriores) y tabla `suppressions`.
- Simulación determinista `mtadst run --scenario <archivo> --seed <n> --out <dir>`: motor y máquina de sesión del simulador en un proceso con reloj virtual; escribe `receiver.jsonl`, `messages/`, `engine.db`, `events.jsonl` canónico y `trace.sha256`. Misma semilla, misma traza byte a byte.
- El `Stepper` del motor pasa a ser una cola de eventos ordenada por (tiempo virtual, secuencia); la conversación de cada conexión corre como corrutina y la latencia del perfil avanza el reloj virtual solo para esa conexión.
- Formato de escenario `lab/scenarios/<nombre>.json` (tenants, dominios, perfiles, número de mensajes, llegadas de Poisson sembradas, configuración del motor y duración virtual) y escenarios `backoff`, `limits` y `determinism`.
- `mtadst classify --corpus corpus/classification.json --out <archivo>`: una decisión del clasificador por caso del corpus (`id`, `class`, `subtype`, `rule_id`).
- Verificador `python3 -m verifier verify --out out [--only ...]` (Python, solo biblioteca estándar, sin código del motor): lectores de `receiver.jsonl`, `engine.db` en solo lectura, `harness.jsonl` y `.eml`, reporte `out/report.json` y las propiedades `classification`, `backoff_expiry`, `receiver_limits` (modo simulado), `determinism`, `profile_sources` y `resources`.
- `make build` (binarios en `bin/`) y `make verify`, el comando único: scope de systemd de usuario y espacio de nombres de usuario, red y montaje, donde el verificador levanta `lo`.
- `lab/params.json` con los parámetros de la sección 5 del contrato.
- Laboratorio real y campaña de muertes forzadas (K1): orquestador en `verifier/lab.py` y `verifier/campaign.py` que, dentro del espacio de nombres, monta su `resolv.conf`, arranca `mtasim serve` y `mtaengine run`, genera 10 000 mensajes sembrados por HTTP con reenvíos idempotentes y conflictos, mata el motor 30 veces con `kill -9` (5 con el protocolo `post_250`: retención del 250, `SIGSTOP`, liberación, `SIGKILL`), corta conexiones, cambia un perfil en caliente, rearranca el motor y espera a que drene. Escribe `harness.jsonl`, `ingest.jsonl` y el resumen `campaign.json` en `out/runs/lab-1/campaign/`.
- Definición de la campaña en `lab/campaign.json` (contrato 3.12).
- Propiedades `no_loss`, `bounded_duplicates`, `idempotency` y `dns_mx` sobre la corrida real; `receiver_limits` decide ahora sobre el escenario simulado `limits` y sobre la corrida real. Con ellas, el reporte trae las diez propiedades de K1.
- Zona de laboratorio: `fallback.test`, cuyo MX preferido no escucha.

### Decisiones

- Motor, simulador y herramienta de depuración en Go; verificador en Python sin dependencias, sin código compartido con el motor.
- Cola y registro de eventos en SQLite (WAL, `synchronous=FULL`), elegido tras medir cero pérdidas en 30 `kill -9`.
- Toda corrida ocurre en un espacio de nombres de red sin privilegios con solo `lo`: el tráfico no puede salir de la máquina.
- Haraka 3.3.5 como MTA externo para la validación de caja negra.
- Licencia MIT.
- Parámetros de cada tipo de regla, zona DNS y protocolo del socket de control fijados en el contrato (3.1, 3.4, 3.9 y 4.4). El cambio de perfil en caliente vale para conexiones nuevas: cambiar de perfil a mitad de una conexión mezclaría contadores de dos perfiles; quien necesite forzarlo corta las conexiones.
- La clave de idempotencia es global y no por tenant: sin autenticación, el tenant viaja en el cuerpo y forma parte del payload comparado; reusar una clave con otro tenant da 409.
- El motor guarda el mensaje RFC 5322 construido al ingresar y envía los mismos bytes en cada intento, así un duplicado tiene el mismo `sha256` en el receptor.
- Reglas y subtipos del clasificador fijados en el contrato (3.8). `MessageTooLarge` es permanente (RFC 3463 §3.3–3.4) aunque Resend lo agrupe con los transitorios; `5.2.2` (buzón lleno) es transitorio aunque llegue con 5xx. Una regla permanente solo puede coincidir con un 5xx.
- Un perfil solo puede usar respuestas idénticas a la tabla 3.2 y debe declarar en `sources` la fuente de cada una, para que una respuesta de fuente secundaria no quede escondida tras una regla con fuente primaria.
- Diseño de la entrega fijado en el contrato (2.6). La cola es por (tenant, dominio) para que el control por tenant de K2 pause colas sin tocar las de otros. Los límites de proveedor cuentan por par proveedor e IP de origen, porque los proveedores limitan por IP emisora. Ante throttling la tasa aprendida es la mitad de lo iniciado en la última ventana y las conexiones bajan en una (supuesto); ante el cierre silencioso de Yahoo se aprende el número de transacciones que llevó la conexión y el mensaje vuelve a la cola sin avanzar el calendario, porque Yahoo permite reconectar enseguida.
- Un mensaje expira en el diferimiento cuyo siguiente intento caería después de su vida, no después: ningún intento ocurre pasada la vida y los intervalos programados no decrecen.
- Solo la expiración genera DSN; un rebote permanente queda en los eventos. La supresión es por tenant, como la lista de supresión de Resend por cuenta.
- Las decisiones sin respuesta del receptor (NXDOMAIN, MX inutilizable, error temporal de DNS, MX inalcanzable, cierre silencioso, pérdida de conexión) llevan su propio `rule_id`, fuera de la tabla de respuestas del clasificador.
- Modo simulado (contrato 2.2): el ejecutor reutiliza el cliente SMTP del motor, el analizador de respuestas, los códecs de DATA de los dos lados y el DNS de laboratorio contestado en proceso, para que lo simulado y lo real difieran solo en el transporte y el reloj. La red y el DNS no consumen tiempo virtual; solo la latencia declarada en un perfil.
- En modo simulado el almacén usa `synchronous=OFF` (cambia D03 solo para `mtadst`): no muere ningún proceso, así que la durabilidad no cambia ninguna salida. Medido: el escenario `limits` baja de 54,8 s a 5,0 s con el mismo `trace.sha256`.
- `backoff_expiry` exige además que cada intervalo entre intentos sea al menos la espera del calendario de `lab/params.json` para ese reintento. Sin esa condición, reintentar con un intervalo constante mínimo (el mutante `mut_no_backoff`) cumpliría «no decreciente y ≤ máximo» y la propiedad no lo detectaría.
- El verificador recalcula `trace.sha256` desde `receiver.jsonl` y `engine.db` en vez de leer el que escribe el motor, y falla si no coinciden.
- El motor reconstruye al arrancar los límites aprendidos desde los eventos `deferred` y `expired` que los registraron (contrato 2.6). Medido en la campaña real: con los límites solo en memoria, el motor volvía a violar el límite de mensajes por conexión de `yahoo-model` entre 13 y 21 s después de cada reinicio, y una de esas violaciones, pasado el periodo de adaptación, contó.
- `bounded_duplicates` acepta un duplicado si hay una falla inyectada en su ventana, con la ventana corregida (contrato 4): una muerte desde el `MAIL` de la transacción del primer 2xx (no desde el 2xx) hasta su `delivered`, o un corte del socket de control que terminó esa conexión justo después del 2xx. Medido: el receptor puede aceptar después de que el emisor murió (muerte en …223000 ms, 250 en …223001 ms) y un corte durante el `fsync` del `data_end` deja aceptado un mensaje cuyo 250 no sale (tres de seis cortes en una corrida de 2 000 mensajes). Ambos son el duplicado inevitable de RFC 5321 §6.1; sigue sin admitirse ninguno sin falla inyectada.
- `no_loss` cruza además cada 202 de la API (registrado por el orquestador en `ingest.jsonl`) con los eventos `accepted`: sin esa fuente, un 202 sin commit no dejaría rastro en `engine.db`.
- La campaña real va a 30 mensajes por segundo (supuesto) para que la carga dure más que el periodo de adaptación de 300 s de los perfiles y una violación en la corrida real pueda contar.
- `events.jsonl` usa los escapes de `json.dumps` de Python (`ensure_ascii`), para que el verificador lo reconstruya desde `engine.db` con la biblioteca estándar y compare bytes.
