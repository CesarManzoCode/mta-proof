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
- Las muertes forzadas son de proceso (`kill -9`); no se prueban cortes de energía ni fallas de disco.
- DANE con DNSSEC, BIMI con certificado comercial y MTA-STS con dominios públicos quedan fuera.
- Se implementa sobre RFC 5321, RFC 5322 y los RFC vigentes citados; no sobre borradores (RFC 5321bis, DKIM2, APRF).
- La validación externa observa al MTA externo solo desde el receptor; las propiedades que necesitan su estado interno quedan sin evaluar.
- El determinismo bit a bit vale para el modo simulado (reloj virtual); las corridas reales con fallas se verifican por propiedades, no por hash.
