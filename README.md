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

Propiedades implementadas hoy: `profile_sources`, `classification`, `backoff_expiry`, `receiver_limits` (en modo simulado), `determinism` y `resources`. Qué decide cada una: sección 4 de [vault/CONTRACT.md](vault/CONTRACT.md).

## Estado

| Fase | Contenido | Estado |
| --- | --- | --- |
| K0 | Contrato, investigación y plan | hecho |
| K1 | Simulador, motor, clasificación, backoff, DNS, determinismo, no pérdida bajo `kill -9` | en curso |
| K2 | Pools y calentamiento, tenants, aislamiento, TLS, autenticación, depuración | pendiente |
| K3 | Mutantes, validación externa con Haraka, licencias | pendiente |

## Resultados

Todavía no hay resultados: se publican aquí al cerrar cada fase.

## Documentos

- [vault/CONTRACT.md](vault/CONTRACT.md): qué debe hacer el sistema, formatos y decisiones.
- [vault/RESEARCH.md](vault/RESEARCH.md): fuentes con revisión fijada y mediciones.
- [LIMITATIONS.md](LIMITATIONS.md): lo que el proyecto no demuestra.
- [CHANGELOG.md](CHANGELOG.md): cambios y decisiones por versión.
