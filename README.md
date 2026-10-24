# mta-proof

Banco de pruebas al estilo Jepsen para el lado saliente de un MTA transaccional, en una sola máquina:

1. **Simulador** de proveedores de buzones: receptor SMTP con perfiles declarativos cuya cada regla cita su fuente pública, y DNS de laboratorio.
2. **Motor de entrega de referencia**, escrito aquí: cola durable por destino, reintentos con backoff y expiración, límites por dominio, proveedor, tenant e IP, pools de IP con calentamiento, clasificación de rebotes y diferimientos, supresión, idempotencia y registro de eventos consultable.
3. **Verificador independiente**: inyecta fallas (`kill -9`, cortes, cambios de perfil) y decide propiedades de seguridad y vivacidad sobre la traza del receptor y el registro de eventos.

No demuestra entregabilidad real: ver [LIMITATIONS.md](LIMITATIONS.md).

## Requisitos

Linux con espacios de nombres de usuario sin privilegios y sesión de systemd de usuario, Go 1.27, Python 3.14, Node.js 26 (solo para la validación externa) y `make`. Todo el tráfico de prueba queda en la interfaz `lo` de un espacio de nombres de red propio.

## Uso

```sh
make deps     # una vez, con Internet: módulos Go y Haraka
make verify   # toda la verificación, sin Internet
```

`make verify` compila `bin/` (`make build`) y luego corre, dentro de un scope de systemd de usuario y de un espacio de nombres de usuario, red y montaje propio:

```sh
systemd-run --user --scope --quiet -- unshare --user --map-root-user --net --mount python3 -m verifier verify --out out
```

Dentro, el verificador levanta `lo`, comprueba que es la única interfaz (`offline`), corre cada propiedad implementada, escribe `out/report.json` e imprime el veredicto de cada una. Sale con 0 solo si todas pasan; si alguna falla, sale con 1 e imprime cuáles y por qué. Para correr solo algunas: `python3 -m verifier verify --out out --only determinism,classification` (las de modo simulado no necesitan el espacio de nombres).

`make build` compila además los seis mutantes del contrato (4.3), cada uno con su etiqueta de compilación, en `bin/mut/<nombre>/`. La propiedad `mutation_detection` vuelve a correr la suite con los binarios originales y con los de cada mutante, y deja cada reporte en `out/runs/mutation/<nombre>/report.json`; pasa solo si el original pasa y cada mutante hace fallar la suite.

`licenses/inventory.json` es el inventario de dependencias Go y npm con su licencia; lo genera `python3 -m verifier inventory` y la propiedad `licenses` lo regenera en cada `make verify` y falla si cambió o si alguna licencia no es abierta.

`make check` corre formato, `go vet` y las pruebas unitarias de Go (también las de cada mutante con su etiqueta) y del verificador; es lo que corre el CI.

### Cómo leer `out/report.json`

```json
{
  "format": "mta-proof-report/1",
  "git_head": "<commit verificado>",
  "started_at": "2026-10-23T16:37:05+00:00",
  "duration_s": 7.1,
  "max_memory_bytes": 215048192,
  "memory_limit_bytes": 8589934592,
  "verdict": "pass",
  "properties": [
    {"name": "backoff_expiry", "verdict": "pass", "seed": 1, "trace": "out/runs/backoff-1",
     "details": {"messages": 3, "max_interval_s": 10800.0, "expired": 3, "dsn_null_sender": 3}}
  ]
}
```

- `verdict` global es `pass` solo si todas las propiedades pasan. Cada propiedad tiene su `verdict`, la `seed` de la corrida (`null` si no usa semilla) y `trace`: el directorio con los archivos crudos sobre los que se decidió (`receiver.jsonl`, `engine.db`, `events.jsonl`, `trace.sha256`, `messages/`), para repetir la decisión a mano.
- `details` trae las cifras que respaldan el veredicto (claves en la sección 4.1 del contrato). Si la propiedad falla, `details.failures` lista los motivos.
- `max_memory_bytes` es el `memory.peak` del scope de toda la corrida y `duration_s` su duración; la propiedad `resources` falla si la memoria supera `memory_limit_bytes`.

Están implementadas las veintidós propiedades de la sección 4 de [vault/CONTRACT.md](vault/CONTRACT.md), que dice qué decide cada una.

La corrida real es una campaña de muertes forzadas (`lab/campaign.json`, contrato 3.12): dentro del espacio de nombres, el verificador arranca el simulador y el motor, manda 10 000 mensajes por HTTP con reenvíos idempotentes y conflictos, mata el motor 30 veces con `kill -9` (5 en la ventana entre el 250 del receptor y su registro), corta conexiones y cambia un perfil en caliente; tras cada muerte lo rearranca y al final espera a que drene. Deja todo en `out/runs/lab-1/campaign/`. Su duración medida está en «Resultados».

## Guía de lectura de la evidencia

Cada fila es una propiedad de `out/report.json`: el criterio de la idea al que responde, el modo, y `trace`, el directorio con los archivos crudos sobre los que se decidió. Las claves de `details` que respaldan el veredicto están en la sección 4.1 del contrato.

| Criterio | Propiedad | Modo | Traza (`trace`) |
| --- | --- | --- | --- |
| No pérdida | `no_loss` | real | `out/runs/lab-1/campaign/` (`ingest.jsonl`, `engine.db`, `harness.jsonl`) |
| Duplicación acotada | `bounded_duplicates` | real | `out/runs/lab-1/campaign/` (`receiver.jsonl`, `harness.jsonl`) |
| Duplicación acotada (ingreso) | `idempotency` | real | `out/runs/lab-1/campaign/` (`ingest.jsonl`, `engine.db`) |
| Límites del receptor | `receiver_limits` | real y simulado | `out/runs/lab-1/` (`campaign/` y `limits/`) |
| Backoff y expiración | `backoff_expiry` | simulado | `out/runs/backoff-1/` |
| Clasificación | `classification` | corpus | `out/runs/classification/decisions.jsonl` frente a `corpus/classification.json` |
| DNS y TLS (MX) | `dns_mx` | real | `out/runs/lab-1/campaign/` |
| Determinismo | `determinism` | simulado | `out/runs/determinism-1/{a,b,seed-2}/trace.sha256` |
| Recursos | `resources` | todo | `out/` (cgroup y `memory.peak` en `details`) |
| Honestidad sobre el alcance | `profile_sources` | estático | `profiles/` frente a `vault/RESEARCH.md` |
| Pools y calentamiento | `ip_warmup` | simulado | `out/runs/warmup-1/` |
| Control por tenant | `tenant_pause` | simulado | `out/runs/tenant-pause-1/{paused,baseline}/` |
| Aislamiento | `destination_isolation` | simulado | `out/runs/isolation-1/{baseline,throttled}/` |
| DNS y TLS (TLS) | `tls_policy` | real | `out/runs/tls-1/{other-ca,wrong-name,valid}/` |
| Autenticación saliente | `outbound_auth` | real | `out/runs/auth-1/` |
| Autenticación saliente (formato) | `rfc5322_format` | capturas | `out/runs/**/messages/*.eml` |
| Depuración | `debug_timeline`, `debug_aggregates` | real | `out/runs/lab-1/campaign/engine.db` y `out/runs/lab-1/debug/` |
| Poder de detección | `mutation_detection` | real y simulado | `out/runs/mutation/<nombre>/report.json` y `verify.log` |
| Validación externa | `external_validation` | real | `out/runs/external-1/` (`receiver.jsonl`, `inject.jsonl`, `haraka.log`) |
| Licencias | `licenses` | estático | `licenses/inventory.json` (regenerado en `out/runs/licenses/inventory.json`) |
| Licencias (sin Internet) | `offline` | todo | `out/runs/offline/interfaces.json`: `interfaces` e `if_nameindex` |

Para repetir una decisión a mano basta la traza: los registros tienen los formatos de la sección 3 del contrato y `engine.db` se puede abrir con `sqlite3` en solo lectura o con `bin/mtaq`.

## Depurar con `mtaq`

`mtaq` lee el registro de eventos (`engine.db`) en solo lectura; sirve sobre cualquier corrida, también con el motor en marcha. Interfaz completa: sección 4.2 del contrato.

```sh
# Línea de tiempo causal de un mensaje: aceptación, cada intento con su ruta, IP,
# conexión y TLS, la respuesta, su clasificación y el estado final, más sus eventos.
bin/mtaq timeline --db out/runs/lab-1/campaign/engine.db --msg m00001230

# Todos los mensajes rebotados desde una IP, uno por línea.
bin/mtaq timeline --db out/runs/lab-1/campaign/engine.db --all --src-ip 127.0.2.1 --kind bounced

# Diferimientos por proveedor.
bin/mtaq agg --db out/runs/lab-1/campaign/engine.db --by provider,kind --kind deferred
```

En la corrida de K2, `m00001230` tiene dos intentos: el primero `interrupted` (el motor murió durante él y la recuperación lo cerró con `recovered`) y el segundo `delivered` con `250 2.0.0` en `mx1.baseline.test` desde `127.0.2.1`. La agregación devuelve un objeto por grupo:

```json
[
{"count":17,"kind":"deferred","provider":null},
{"count":261,"kind":"deferred","provider":"baseline"},
{"count":3,"kind":"deferred","provider":"gmail-model"},
{"count":1,"kind":"deferred","provider":"outlook-model"},
{"count":2,"kind":"deferred","provider":"yahoo-model"}
]
```

`provider` nulo es el grupo de los eventos sin proveedor: en esta corrida, los 17 diferimientos son de `fallback.test`, cuyo MX no coincide con los `mx_suffixes` de ningún proveedor de la configuración (14 `451 4.3.0` al final de DATA y 3 cierres silenciosos al `MAIL`).

## Estado

| Fase | Contenido | Estado |
| --- | --- | --- |
| K0 | Contrato, investigación y plan | hecho |
| K1 | Simulador, motor, clasificación, backoff, DNS, determinismo, no pérdida bajo `kill -9` | cerrada |
| K2 | Pools y calentamiento, tenants, aislamiento, TLS, autenticación, depuración | cerrada |
| K3 | Mutantes, validación externa con Haraka, licencias y modo sin red | sus tres mini sprints hechos (k3-01, k3-02, k3-03); cierre pendiente |

## Resultados

### Resultados finales (versión 1.0.0)

`make verify` de k3-03, 2026-10-23 (de 18:35:09 a 19:34:07, −06:00), sobre `40979e3` con el árbol de trabajo de k3-03 (el `git_head` del reporte), en la máquina de desarrollo (Arch Linux, kernel 7.2.11, 12 CPU, 14 GiB visibles, disco SSD). Sale con 0: veredicto global `pass` en las veintidós propiedades. `make deps` y `make check` también salen con 0 (171 pruebas del verificador).

| Medida | Valor |
| --- | --- |
| Duración de la suite | 3 533,7 s (unos 59 min); cada suite interna de `mutation_detection`, de 388,1 a 451,9 s |
| Memoria máxima (`memory.peak` del scope) | 5 809 086 464 bytes de 8 589 934 592 permitidos |
| `offline` | `lo` es la única interfaz según el `sysfs` del espacio de nombres y según `if_nameindex` |
| `licenses` | 35 módulos Go y 99 paquetes npm; 88 MIT, 26 BSD-3-Clause, 10 ISC, 5 BlueOak-1.0.0, 3 Apache-2.0, 1 BSD-2-Clause, 1 MPL-2.0; inventario sin cambios |
| `profile_sources` | 6 perfiles, 14 reglas, todas con fuente o supuesto declarado |
| `classification` | 24 de 24 casos del corpus |
| `backoff_expiry` | 3 mensajes, 126 intentos, intervalos de 1 800 a 10 800 s, 3 expirados con DSN `MAIL FROM:<>` |
| `determinism` | A y B con el mismo `trace.sha256` (`845f1455…`), la semilla 2 con otro (`42282bf8…`) |
| `ip_warmup` | 0 `data_end` sobre el tope diario u horario; 4xx: 0 de 118 229 en IPs conocidas, 2 de 34 044 en la desconocida; reparto a 0,00099 del plan |
| `tenant_pause` | `spamco` pausado 0 ms después de cada cruce (plazo 60 s); los demás entregan 2 787 mensajes en las pausas, igual que la base |
| `destination_isolation` | p50 y p99 de los otros destinos: 200 ms con y sin `gmail-model.test` estrangulado (su p99 pasa de 0 a 5 571 078 ms) |
| Campaña real (`no_loss`, `bounded_duplicates`, `idempotency`) | 10 000 aceptados, 30 `kill -9` (5 `post_250`, 10 `post_202`), 0 perdidos; 19 duplicados, todos con muerte en su ventana; 100 reenvíos idempotentes y 50 conflictos rechazados |
| `dns_mx` | 113 mensajes a null MX `bounced`/`NullMX` sin conexión; 422 de 422 a `fallback.test` entregados en el segundo MX |
| `receiver_limits` | 0 violaciones contadas en la corrida simulada `limits` y en la real |
| `tls_policy` | con certificado inválido, el dominio `opportunistic` se entrega y el `required` se difiere `TLSPolicy`; con el válido, los dos por TLS |
| `outbound_auth` | 10 de 10 de `auth-good.test` con SPF, DKIM y DMARC `pass`; 10 de 10 de `auth-broken.test` con un solo intento y `550 5.7.515` |
| `rfc5322_format` | 84 336 `.eml`, 0 con defectos |
| `debug_timeline`, `debug_aggregates` | 10 000 de 10 000 líneas de tiempo coinciden (10 183 `data_end` explicados); 15 consultas, 413 grupos sobre 41 358 eventos, 0 diferencias |
| `external_validation` | Haraka 3.3.5: 10 015 mensajes tomados, 0 violaciones, 5 de 5 mensajes a `always-defer` con el calendario de `temp_fail_intervals` y su DSN; 15 propiedades `not_evaluated` con motivo |
| `mutation_detection` | original 0; los seis mutantes salen con 1 y cae su propiedad: `mut_no_backoff` → `backoff_expiry`; `mut_retry_permanent` → `outbound_auth` (y `tenant_pause`); `mut_ignore_conn_limit` → `receiver_limits`; `mut_ack_before_persist` → `no_loss` (e `idempotency`); `mut_shared_queue` → `destination_isolation` (y `tenant_pause`); `mut_exceed_warmup` → `ip_warmup` |

Son una semilla en una máquina: ver [LIMITATIONS.md](LIMITATIONS.md).

### Resultados de K1

Cierre de K1, 2026-10-23, sobre el commit `b548f65`, en la máquina de desarrollo (Arch Linux, kernel 7.2.11, 12 CPU, 14 GiB, disco SSD):

- `make check` sale con 0: pasan los seis paquetes Go con pruebas (`classify`, `clock`, `dst`, `engine`, `sim`, `store`) y las 64 pruebas del verificador.
- Pasan los once criterios de salida de K1, recalculados sobre `out/report.json` de `make verify` y sus archivos crudos: comando único, no pérdida, duplicados acotados e idempotencia, límites del receptor, backoff y expiración, clasificación, DNS y MX, determinismo, recursos y perfiles.

El registro del cierre anota el veredicto de cada criterio, no sus cifras. Las de la tabla son del `out/report.json` de la corrida de K1 registrada en `fa3eedd`, con el mismo código del cierre (los commits posteriores solo cambian LIMITATIONS). Veredicto global `pass` en las diez propiedades.

| Medida | Valor |
| --- | --- |
| Mensajes aceptados en la corrida real | 10 000 (más 100 reenvíos iguales y 50 conflictos rechazados con 409) |
| `kill -9` del motor | 30, de ellos 5 en la ventana `post_250` |
| Pérdidas (`lost`) | 0 |
| Duplicados en el receptor | 8, todos con una muerte en su ventana (0 fuera de ventana) |
| Violaciones contadas de los límites del receptor | 0 (simulada y real) |
| Null MX (`nullmx.test`) | 113 mensajes, todos `bounced`/`NullMX`, sin conexión |
| MX preferido caído (`fallback.test`) | 422 mensajes, todos entregados en el segundo MX |
| Duración de la suite | 342,6 s (333,4 s de la campaña real, que va a 30 mensajes/s a propósito) |
| Memoria máxima (`memory.peak` del scope) | 332 771 328 bytes de 8 589 934 592 permitidos |

### Resultados de K2

Cierre de K2, 2026-10-23, sobre el commit `9faea41`, en la misma máquina:

- `make check` sale con 0: pasan los ocho paquetes Go con pruebas (`auth`, `classify`, `clock`, `dst`, `engine`, `mtaq`, `sim`, `store`) y las 126 pruebas del verificador.
- Pasan los ocho criterios de salida de K2, recalculados sobre `out/report.json` de `make verify` y sus archivos crudos: calentamiento de IPs, pausa por tenant, aislamiento entre destinos, TLS, autenticación, formato RFC 5322, depuración con `mtaq` y regresión de K1. Siguen pasando los once criterios de K1 y los nueve de K0.

El registro del cierre anota el veredicto de cada criterio, no sus cifras. Las de la tabla son del `out/report.json` del `make verify` del último mini sprint (k2-05), sobre `a9d1fe8` con el árbol de trabajo de k2-05 (el `git_head` del reporte; k2-05 se confirmó después como `9faea41`): veredicto global `pass` en las dieciocho propiedades.

| Medida | Valor |
| --- | --- |
| Campaña real | 10 000 aceptados, 30 `kill -9` (5 `post_250`), 0 perdidos, 7 duplicados todos con muerte en su ventana |
| `ip_warmup` (escenario `warmup`) | 0 `data_end` por encima del tope diario u horario; estrangulamiento 0 en las IPs conocidas y 2 de 34 044 respuestas en la desconocida; diferencia máxima de reparto 0,00099 (tolerancia 0,05) |
| `tenant_pause` (escenario `tenant-pause`) | `spamco` pausado 0 ms después de cada cruce (plazo 60 s), 2 400 s de pausa; los demás tenants entregan 2 787 mensajes en esas pausas, igual que en la corrida base (razón 1,0) |
| `destination_isolation` (escenario `isolation`) | p50 y p99 de los otros destinos sin cambio (margen 1,10) con `gmail-model.test` estrangulado: su p99 pasa de 0 a 5 571 078 ms |
| `tls_policy` | con certificado inválido el dominio `opportunistic` se entrega y el `required` se difiere `TLSPolicy`; con el válido, los dos se entregan por TLS |
| `outbound_auth` | 10 de 10 mensajes de `auth-good.test` con SPF, DKIM y DMARC `pass`; 10 de 10 de `auth-broken.test` con un solo intento, `550 5.7.515` y `fail` en los tres |
| `rfc5322_format` | 84 336 `.eml` capturados, 0 con defectos |
| `debug_timeline` | 10 000 de 10 000 líneas de tiempo coinciden con el receptor: 10 183 intentos, los 10 166 `data_end` explicados con su código y en orden |
| `debug_aggregates` | 15 consultas de `mtaq agg`, 411 grupos sobre 41 310 eventos, 0 diferencias con el `GROUP BY` del verificador |
| Duración de la suite | 393,9 s |
| Memoria máxima (`memory.peak` del scope) | 1 052 442 624 bytes de 8 589 934 592 permitidos |

## Documentos

- [vault/CONTRACT.md](vault/CONTRACT.md): qué debe hacer el sistema, formatos y decisiones.
- [vault/RESEARCH.md](vault/RESEARCH.md): fuentes con revisión fijada y mediciones.
- [LIMITATIONS.md](LIMITATIONS.md): lo que el proyecto no demuestra.
- [CHANGELOG.md](CHANGELOG.md): cambios y decisiones por versión.
