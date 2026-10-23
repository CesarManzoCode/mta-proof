# Changelog

## [Sin publicar]

### Añadido

- Simulador `mtasim serve`: receptor SMTP con perfiles declarativos (`gmail-model`, `yahoo-model`, `outlook-model`, `baseline`, `always-defer`), DNS autoritativo de laboratorio sobre `lab/zone.json`, registro `receiver.jsonl` y mensaje `messages/<sha256>.eml` sincronizados con `fsync` antes de cada respuesta a DATA y socket de control (cambio de perfil, cortes, retención del 250).
- Máquina de sesión SMTP sin sockets en `internal/sim`, para que `mtadst` la use en proceso.

### Decisiones

- Motor, simulador y herramienta de depuración en Go; verificador en Python sin dependencias, sin código compartido con el motor.
- Cola y registro de eventos en SQLite (WAL, `synchronous=FULL`), elegido tras medir cero pérdidas en 30 `kill -9`.
- Toda corrida ocurre en un espacio de nombres de red sin privilegios con solo `lo`: el tráfico no puede salir de la máquina.
- Haraka 3.3.5 como MTA externo para la validación de caja negra.
- Licencia MIT.
- Parámetros de cada tipo de regla, zona DNS y protocolo del socket de control fijados en el contrato (3.1, 3.4, 3.9 y 4.4). El cambio de perfil en caliente vale para conexiones nuevas: cambiar de perfil a mitad de una conexión mezclaría contadores de dos perfiles; quien necesite forzarlo corta las conexiones.
- Un perfil solo puede usar respuestas idénticas a la tabla 3.2 y debe declarar en `sources` la fuente de cada una, para que una respuesta de fuente secundaria no quede escondida tras una regla con fuente primaria.
