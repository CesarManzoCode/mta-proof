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

Dentro, el verificador levanta `lo`, corre cada propiedad implementada, escribe `out/report.json` e imprime el veredicto de cada una. Sale con 0 solo si todas pasan; si alguna falla, sale con 1 e imprime cuáles y por qué. Para correr solo algunas: `python3 -m verifier verify --out out --only determinism,classification` (las de modo simulado no necesitan el espacio de nombres).

`make check` corre formato, `go vet` y las pruebas unitarias de Go y del verificador; es lo que corre el CI.

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

Propiedades implementadas hoy, las diez de K1: `profile_sources`, `classification`, `backoff_expiry`, `determinism`, `no_loss`, `bounded_duplicates`, `idempotency`, `dns_mx`, `receiver_limits` (sobre el escenario simulado `limits` y sobre la corrida real) y `resources`. Qué decide cada una: sección 4 de [vault/CONTRACT.md](vault/CONTRACT.md).

La corrida real es una campaña de muertes forzadas (`lab/campaign.json`, contrato 3.12): dentro del espacio de nombres, el verificador arranca el simulador y el motor, manda 10 000 mensajes por HTTP con reenvíos idempotentes y conflictos, mata el motor 30 veces con `kill -9` (5 en la ventana entre el 250 del receptor y su registro), corta conexiones y cambia un perfil en caliente; tras cada muerte lo rearranca y al final espera a que drene. Deja todo en `out/runs/lab-1/campaign/`. Su duración medida está en «Resultados».

## Estado

| Fase | Contenido | Estado |
| --- | --- | --- |
| K0 | Contrato, investigación y plan | hecho |
| K1 | Simulador, motor, clasificación, backoff, DNS, determinismo, no pérdida bajo `kill -9` | Cerrada |
| K2 | Pools y calentamiento, tenants, aislamiento, TLS, autenticación, depuración | pendiente |
| K3 | Mutantes, validación externa con Haraka, licencias | pendiente |

## Resultados

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

Son una semilla en una máquina: ver [LIMITATIONS.md](LIMITATIONS.md).

## Documentos

- [vault/CONTRACT.md](vault/CONTRACT.md): qué debe hacer el sistema, formatos y decisiones.
- [vault/RESEARCH.md](vault/RESEARCH.md): fuentes con revisión fijada y mediciones.
- [LIMITATIONS.md](LIMITATIONS.md): lo que el proyecto no demuestra.
- [CHANGELOG.md](CHANGELOG.md): cambios y decisiones por versión.
