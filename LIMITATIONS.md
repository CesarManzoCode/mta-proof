# Límites de mta-proof

Fuente única de lo que este proyecto no demuestra. Ningún otro documento repite estos límites.

## No demostrable localmente

- **Reputación real** de IPs y dominios, y colocación en bandeja de entrada o spam: depende de receptores reales.
- **Límites reales de proveedores** (Gmail, Yahoo, Microsoft, Apple): no están publicados y cambian. Los perfiles del simulador son modelos de comportamientos documentados; toda cifra de límite es un supuesto marcado como tal en el perfil.
- **Calentamiento real de IPs**: toma semanas o meses frente a proveedores reales. Aquí solo se demuestra que el motor respeta un plan declarado frente a un receptor modelado.
- **PTR y DNS inverso en IPs públicas**: solo se modela en una zona de laboratorio.
- **FBL real** (bucles de quejas) y reportes de postmaster: exigen inscripción con proveedores y dominios reales.
- **Escala de producción**: una máquina de 16 GB prueba comportamiento, no volumen ni multirregión.

## Otros límites

- Las directrices oficiales de remitentes de Gmail no pudieron leerse; el perfil `gmail-model` se apoya en la página de errores SMTP de Google y lo declara.
- `550 5.7.509` y los textos de throttling de Yahoo provienen de fuentes secundarias.
- El clasificador solo conoce las respuestas de la tabla 3.2 del contrato y unos pocos códigos extendidos de RFC 3463. Cualquier otro 5xx real queda `undetermined` y se reintenta hasta expirar: no se pierde ni se suprime, pero tarda la vida completa del mensaje en terminar. Los subtipos de las respuestas de proveedor son diseño del proyecto, no categorías publicadas.
- Las muertes forzadas son de proceso (`kill -9`); no se prueban cortes de energía ni fallas de disco.
- Los límites aprendidos del motor solo bajan y nunca caducan: se reconstruyen al arrancar desde el registro de eventos, así que un límite aprendido en un mal momento queda para siempre en esa base. Sus factores (la mitad de la tasa, una conexión menos) son supuestos.
- El motor no distingue el cierre silencioso de Yahoo al llegar a su límite por conexión de un servidor que cierra una conexión ociosa justo cuando llega el `MAIL`; en el segundo caso aprendería un límite menor que el real.
- La resolución del motor no sigue CNAME ni usa IPv6 (solo registros A): la zona de laboratorio no tiene ninguno de los dos.
- Los límites de tasa del motor cuentan intentos iniciados, no mensajes aceptados por el receptor.
- DANE con DNSSEC, BIMI con certificado comercial y MTA-STS con dominios públicos quedan fuera.
- Se implementa sobre RFC 5321, RFC 5322 y los RFC vigentes citados; no sobre borradores (RFC 5321bis, DKIM2, APRF).
- La validación externa observa al MTA externo solo desde el receptor; las propiedades que necesitan su estado interno quedan sin evaluar.
- El determinismo bit a bit vale para el modo simulado (reloj virtual); las corridas reales con fallas se verifican por propiedades, no por hash.
- El determinismo se midió con el mismo binario (Go 1.27.3, amd64) en esta máquina. No se afirma entre versiones de Go ni arquitecturas: las llegadas usan aritmética de punto flotante y los generadores de `math/rand/v2`, y las trazas usan el codificador JSON de Go.
- En modo simulado la red y el DNS no consumen tiempo virtual; solo pasa el tiempo que un perfil declara con `latency`. Los perfiles actuales no declaran latencia, así que en ellos toda una conversación SMTP ocurre en un instante virtual. Cuando una latencia supera el plazo del motor, el receptor anota el cierre al vencer el plazo, no cuando habría intentado responder.
- Las propiedades de modo simulado (`backoff_expiry`, `receiver_limits`, `determinism`) se deciden sobre una semilla por escenario (y una segunda para `determinism`): muestran el comportamiento en esas trazas, no en todas las semillas posibles. La campaña real también es una sola semilla, y no es reproducible byte a byte: los instantes de las muertes dependen del planificador del sistema operativo.
- La ventana de un duplicado empieza en el `MAIL` de la transacción de su primer 2xx, no en el 2xx: si el motor muere tras enviar el punto final de DATA, el receptor termina sus `fsync` y acepta después de la muerte, y el motor reenvía. Medido en la campaña de la semilla 1 sobre `fa3eedd`: 2 de 7 duplicados (`m00004540`, `m00006973`) tienen su `kill` anotado 1 ms antes del `data_end` 250 de su primera copia y 2 ms después de su `MAIL`. Una muerte en ese tramo cuenta como explicación del duplicado aunque el 250 aún no existiera.
- En la campaña real solo muere el motor; el simulador y el DNS de laboratorio no fallan. Las muertes son `kill -9` del proceso, no del sistema.
- La campaña real dura unos 333 s y el periodo de adaptación de los perfiles es de 300 s: `receiver_limits` sobre la corrida real solo puede contar violaciones en los últimos ~33 s de carga y en el drenaje. Las violaciones de antes se registran como no contadas en `details.runs.real.uncounted`.
- La campaña real manda a cada modelo de proveedor un 5 % de los mensajes (unos 1,5 por segundo), por debajo de sus tasas supuestas; la presión sobre las tasas se prueba en el escenario simulado `limits`, no en la corrida real.
- El CI solo corre `make check`. Las propiedades de `make verify` (modo simulado y campaña real) necesitan espacios de nombres de usuario y una sesión de systemd de usuario, y solo se han corrido en la máquina de desarrollo. Los binarios `cmd/mtadst`, `cmd/mtaengine` y `cmd/mtasim` no tienen pruebas propias en `make check`: solo los ejercita `make verify`.
- El número de duplicados de la campaña real y cuáles `msg_id` los tienen cambian entre corridas de la misma semilla: 8 en la corrida de K1 publicada en el README y 7 en otra sobre el mismo código (`fa3eedd`). Entre corridas reales solo se comparan veredictos, no cifras.
