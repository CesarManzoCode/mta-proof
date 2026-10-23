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

`make verify` sale con 0 solo si todas las propiedades pasan; si alguna falla, sale con otro código e imprime cuál. Escribe `out/report.json`: una entrada por propiedad con nombre, veredicto, semilla y ruta de la traza que la respalda, más la memoria máxima y la duración de la suite.

`make check` corre formato, `go vet` y las pruebas unitarias; es lo que corre el CI.

## Estado

| Fase | Contenido | Estado |
| --- | --- | --- |
| K0 | Contrato, investigación y plan | hecho |
| K1 | Simulador, motor, clasificación, backoff, DNS, determinismo, no pérdida bajo `kill -9` | pendiente |
| K2 | Pools y calentamiento, tenants, aislamiento, TLS, autenticación, depuración | pendiente |
| K3 | Mutantes, validación externa con Haraka, licencias | pendiente |

## Resultados

Todavía no hay resultados: se publican aquí al cerrar cada fase.

## Documentos

- [vault/CONTRACT.md](vault/CONTRACT.md): qué debe hacer el sistema, formatos y decisiones.
- [vault/RESEARCH.md](vault/RESEARCH.md): fuentes con revisión fijada y mediciones.
- [LIMITATIONS.md](LIMITATIONS.md): lo que el proyecto no demuestra.
- [CHANGELOG.md](CHANGELOG.md): cambios y decisiones por versión.
