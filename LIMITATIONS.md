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
- Los límites aprendidos del motor viven en memoria: solo bajan durante una corrida y se pierden al reiniciar. Sus factores (la mitad de la tasa, una conexión menos) son supuestos.
- El motor no distingue el cierre silencioso de Yahoo al llegar a su límite por conexión de un servidor que cierra una conexión ociosa justo cuando llega el `MAIL`; en el segundo caso aprendería un límite menor que el real.
- La resolución del motor no sigue CNAME ni usa IPv6 (solo registros A): la zona de laboratorio no tiene ninguno de los dos.
- Los límites de tasa del motor cuentan intentos iniciados, no mensajes aceptados por el receptor.
- DANE con DNSSEC, BIMI con certificado comercial y MTA-STS con dominios públicos quedan fuera.
- Se implementa sobre RFC 5321, RFC 5322 y los RFC vigentes citados; no sobre borradores (RFC 5321bis, DKIM2, APRF).
- La validación externa observa al MTA externo solo desde el receptor; las propiedades que necesitan su estado interno quedan sin evaluar.
- El determinismo bit a bit vale para el modo simulado (reloj virtual); las corridas reales con fallas se verifican por propiedades, no por hash.
