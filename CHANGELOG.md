# Changelog

## [Sin publicar]

### Añadido

- Simulador `mtasim serve`: receptor SMTP con perfiles declarativos (`gmail-model`, `yahoo-model`, `outlook-model`, `baseline`, `always-defer`), DNS autoritativo de laboratorio sobre `lab/zone.json`, registro `receiver.jsonl` y mensaje `messages/<sha256>.eml` sincronizados con `fsync` antes de cada respuesta a DATA y socket de control (cambio de perfil, cortes, retención del 250).
- Máquina de sesión SMTP sin sockets en `internal/sim`, para que `mtadst` la use en proceso.
- Núcleo del motor (`internal/engine`): máquina de estados de un hilo con reloj y azar inyectados (`internal/clock`: reloj real, reloj virtual y flujos de `math/rand/v2` derivados por componente), estados del mensaje, eventos de 3.3 y recuperación al arrancar con eventos `recovered`.
- Almacén `internal/store` en SQLite (`modernc.org/sqlite` v1.61.0, WAL, `synchronous=FULL`) con commits agrupados.
- `mtaengine serve` con la API de ingreso `POST /v1/messages`: un destinatario por mensaje, `Idempotency-Key` con ventana de 24 h, 202 solo tras el commit y mensaje RFC 5322 con `Message-ID` y `X-Mtaproof-Id` derivados del `msg_id`.

### Decisiones

- Motor, simulador y herramienta de depuración en Go; verificador en Python sin dependencias, sin código compartido con el motor.
- Cola y registro de eventos en SQLite (WAL, `synchronous=FULL`), elegido tras medir cero pérdidas en 30 `kill -9`.
- Toda corrida ocurre en un espacio de nombres de red sin privilegios con solo `lo`: el tráfico no puede salir de la máquina.
- Haraka 3.3.5 como MTA externo para la validación de caja negra.
- Licencia MIT.
- Parámetros de cada tipo de regla, zona DNS y protocolo del socket de control fijados en el contrato (3.1, 3.4, 3.9 y 4.4). El cambio de perfil en caliente vale para conexiones nuevas: cambiar de perfil a mitad de una conexión mezclaría contadores de dos perfiles; quien necesite forzarlo corta las conexiones.
- La clave de idempotencia es global y no por tenant: sin autenticación, el tenant viaja en el cuerpo y forma parte del payload comparado; reusar una clave con otro tenant da 409.
- El motor guarda el mensaje RFC 5322 construido al ingresar y envía los mismos bytes en cada intento, así un duplicado tiene el mismo `sha256` en el receptor.
- Un perfil solo puede usar respuestas idénticas a la tabla 3.2 y debe declarar en `sources` la fuente de cada una, para que una respuesta de fuente secundaria no quede escondida tras una regla con fuente primaria.
