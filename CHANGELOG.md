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
- `events.jsonl` usa los escapes de `json.dumps` de Python (`ensure_ascii`), para que el verificador lo reconstruya desde `engine.db` con la biblioteca estándar y compare bytes.
