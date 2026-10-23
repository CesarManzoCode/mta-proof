# Investigación de mta-proof

Fuentes primarias con su revisión fijada. Los RFC son inmutables; se fija el sha256 del texto descargado de `https://www.rfc-editor.org/rfc/rfcNNNN.txt`. Las páginas web cambian: se fija la fecha de consulta y el texto literal que el proyecto usa. El software se fija por versión y hash del gestor de paquetes. Toda consulta: 2026-10-22.

Etiquetas: **primaria** (emisor oficial del dato) y **secundaria** (blog, agregador).

## Estándares (primarias)

| Id | Fuente | Revisión fijada | Secciones leídas en K0 | Lo que usa el proyecto |
| --- | --- | --- | --- | --- |
| R-5321 | RFC 5321, SMTP | sha256 `7d560eddff1b213c17d4e1e5f95005728b2f05bc2b3947f0fcc91717c6707634` | §4.5.4.1, §4.5.5, §5.1 (releídas en k1-04, sha256 verificado de nuevo); §4.2.3 (k1-03, sha256 verificado de nuevo) | `451 Requested action aborted: local error in processing` y `554 Transaction failed` (§4.2.3). El 250 tras DATA transfiere la responsabilidad. Reintento ≥ 30 min «en general», dos intentos en la primera hora y luego cada 2–3 h, abandono a 4–5 días, parámetros configurables. 5yz a MAIL no se cachea. MX ordenados por preferencia, aleatorios a igual preferencia, probar al menos dos direcciones; MX implícito si no hay MX. DSN con reverse-path nulo |
| R-5322 | RFC 5322, formato de mensaje | sha256 `cb459acca15a2aa895863eaeff6f1d5b9c4ca15578ae3e3706f757787a385969` (verificado de nuevo en k1-02) | §2.1.1, §3.3, §3.6, §3.6.1, §3.6.2, §3.6.4 (k1-02); el resto lo lee k2-04 | Formato de los mensajes generados. Solo `Date` (una vez) y `From` (una vez) son obligatorios; `To`, `Subject` y `Message-ID` como máximo una vez y `Message-ID` «SHOULD»; `msg-id` = `<` id-left `@` id-right `>` con dot-atom-text y dominio recomendado a la derecha; fecha con día de la semana coherente, `+0000` para UTC; líneas de 998 caracteres como máximo |
| R-7505 | RFC 7505, null MX | sha256 `c746618ea6f26fab7828075696be275d42f4aa134a3e9853cbc6f7ed4d859eb9` | §3, §4.1, §4.2 (releídas en k1-04, sha256 verificado de nuevo) | Un único MX de preferencia 0 y exchange `.`; el emisor no encola ni reintenta; `556 5.1.10` |
| R-3463 | RFC 3463, códigos de estado extendidos | sha256 `ccfbf657f4d3ac7c89cca03bd1abed355efe3f4b170162e32a6ff344cebfe851` | §2; §3 (k1-03, sha256 verificado de nuevo); §3.5 X.4.7 (k1-04, sha256 verificado de nuevo) | Clase 4 = transitoria persistente, 5 = permanente; ante detalle desconocido, usar el sujeto. §3: X.1.1 «only useful for permanent failures»; X.2.2 «should be used as a persistent transient failure»; X.2.3 «should be used as a permanent failure»; X.3.4 «useful only as a permanent error»; X.7.1 «useful only as a permanent error». El motor no sube a permanente por el sujeto ni por códigos sin regla (D14). §3.5: X.4.7 «Delivery time expired»; «If possible, the code for the actual problem found when delivery was attempted should be returned rather than this code» (Status del DSN de expiración) |
| R-3464 | RFC 3464, DSN | sha256 `8d213b662d9c40a7a729db598beb1d3f0139b7532b3c9a7be06ef220c84c397c` | §2 (estructura); §2.1–§2.3 leídas en k1-04, sha256 verificado de nuevo | `multipart/report; report-type=delivery-status`, segunda parte `message/delivery-status`, dirigido al reverse-path original. §2: `MAIL FROM:<>` obligatorio; `message/delivery-status` en 7bit; campos por mensaje (`Reporting-MTA` obligatorio, `Original-Envelope-Id` permitido al MTA del remitente, `Arrival-Date`) y por destinatario (`Final-Recipient`, `Action`, `Status` obligatorios; `Remote-MTA`, `Diagnostic-Code`, `Last-Attempt-Date` opcionales); tercera parte con el original opcional |
| R-3207 | RFC 3207, STARTTLS | sha256 `b004a203791a1ca664d39a49d73369f88f5f3870486a6639e3f833df2118e4ac` | — (lo lee k2-03) | Negociación TLS y política local ante fallas |
| R-6376 | RFC 6376, DKIM | sha256 `00ae1e26aa19e0b8a6087435a524d23e824e2c31ebd62544dd714923bf8553f0` | — (lo lee k2-04) | Firma y verificación |
| R-7208 | RFC 7208, SPF | sha256 `3cca3f2551f50d24a7b6423a7451d285df077863dab4bb261080ee87844f2d5b` | — (lo lee k2-04) | Evaluación `check_host()` |
| R-9989 | RFC 9989, DMARC (mayo 2026; obsoleta 7489 y 9091) | sha256 `cdd78e2918a210ca0f1bcd167dfd9e5555a9331506bcea988432793b71fd0c1e` | §4.10 | Búsqueda por árbol DNS (máximo ocho consultas), descubrimiento de política, alineamiento |
| R-6761 | RFC 6761, nombres de uso especial | sha256 `549f6635f34eb0da627568657401f0115e8e33c2ba1ba7e37bf14ab27651339e` | §6.2 | `.test` reservado para pruebas |

## Proveedores y Resend

| Id | Fuente | Tipo | Texto o dato fijado |
| --- | --- | --- | --- |
| R-GOOGLE-ERR | https://knowledge.workspace.google.com/admin/support/troubleshooting/gmail-smtp-errors-and-codes | primaria | Inicios literales: `421 4.7.28` «Gmail has detected an unusual rate of email originating from your IP address» (y variantes por bloque de IP, dominio DKIM y SPF); `421 4.7.0` «Try again later, closing connection.»; `550 5.7.26` «This email has been blocked because the sender is unauthenticated.» y «Unauthenticated email from domain-name is not accepted due to domain's DMARC policy.»; `450 4.2.1` «The user you are trying to contact is receiving email too quickly.»; `550 5.1.1` «The email account that you tried to reach does not exist.» La herramienta de lectura entregó como máximo 125 caracteres por fila |
| R-GMAIL-GUIDE | https://support.google.com/a/answer/81126 | primaria | No legible (página vacía). Sin cifras primarias de Gmail |
| R-YAHOO | https://senders.yahooinc.com/best-practices/ | primaria | «Yahoo accepts a limited number of messages per SMTP connection»; al alcanzarlo cierra la conexión «without giving an error message»; se puede reconectar «immediately thereafter»; concurrencia permitida sin cifra publicada; requisitos: SPF o DKIM, spam < 0.3 %, DNS directo e inverso, RFC 5321 y 5322 |
| R-SUPED-YAHOO | https://www.suped.com/learn/email-deliverability/what-do-yahoo-421-errors-mean-and-how-to-resolve-them | secundaria | «421 4.7.0 [TSS04] Message temporarily deferred by Yahoo»; «421 [IPTS04] Message deferred for IP policy reasons» |
| R-MS-515 | https://support.microsoft.com/en-au/topic/fix-ndr-error-550-5-7-515-in-outlook-com-34cfe8f8-6fbf-457e-9e8b-9e4dbaf4e0ef | primaria | «550 5.7.515 Access denied, sending domain <domain> does not meet the required authentication level.»; umbral 5 000 mensajes/día con el mismo dominio From; SPF y DKIM deben pasar y DMARC alineado con al menos uno |
| R-MS-509 | https://dmarcian.com/microsoft-dmarc-error-codes-troubleshoot/ y https://www.palisade.email/learning/fix-microsoft-365-550-5-7-x-access-denied | secundaria | «550 5.7.509 Access denied, sending domain <domain> does not pass DMARC verification and has a DMARC policy of reject.» La página de Microsoft de 5.7.515 no menciona 5.7.509 |
| R-RESEND-WARMUP | https://resend.com/docs/knowledge-base/warming-up | primaria | Dominio nuevo: días 1–7 hasta 150, 250, 400, 700, 1 000, 1 500 y 2 000 correos; por hora: 50, 75, 100 y 150 como máximo en los días 4–7. Dominio existente: 1 000/100, 2 500/300, 5 000/600, 5 000/800, 7 500/1 000, 7 500/1 500, 10 000/2 000. «keep your bounce rate below 4% and your spam rate below 0.08%» |
| R-RESEND-BOUNCES | https://resend.com/docs/dashboard/emails/email-bounces | primaria (vía investigación) | `Permanent` (General, NoEmail), `Transient` (General, MailboxFull, MessageTooLarge, ContentRejected, AttachmentRejected), `Undetermined` |
| R-RESEND-IDEMP | https://resend.com/blog/engineering-idempotency-keys | primaria (vía investigación) | `Idempotency-Key` ≤ 256 caracteres, ventana de 24 h, se compara el payload |
| R-RESEND-FORGE | https://resend.com/features/forge | primaria (vía investigación) | Configuración por receptor; límites no publicados y cambiantes; los proveedores limitan IPs desconocidas |
| R-VACANTE | https://resend.com/careers/053bde8f-294e-4cce-9d62-2301282120a2 | primaria (vía investigación) | Responsabilidades del rol |

Las filas «vía investigación» se apoyan en los archivos de investigación de la idea (consulta del 2026-10-22), no en una lectura propia de K0.

## Software (revisiones fijadas)

| Id | Software | Versión fijada | Hash o verificación | Licencia |
| --- | --- | --- | --- | --- |
| S-GO | Go | 1.27.3 (`go version`) | instalado en la máquina | BSD-3 |
| S-SQLITE | `modernc.org/sqlite` | v1.61.0 | go.sum `h1:sQi63oLQ9uquAUA4CzOisH35Q3pEszkPBuHM0tFL8CY=` | BSD-3 (SQLite: dominio público). Dependencia directa en `go.mod` desde k1-02: hash del go.sum igual al fijado, `LICENSE` BSD-3 en la caché de módulos y «License: BSD-3-Clause» en pkg.go.dev para v1.61.0. Pragmas por DSN (`_pragma=journal_mode(WAL)`, `_pragma=synchronous(FULL)`, `_txlock=immediate`), leídos en su `sqlite.go`. Arrastra, verificados por el `LICENSE` de cada módulo en la caché: `modernc.org/libc` v1.79.0 (BSD-3; incluye musl, go-netdb y nixpkgs, MIT, en `LICENSE-3RD-PARTY.md`), `modernc.org/mathutil` v1.7.1 (BSD-3), `modernc.org/memory` v1.12.1 (BSD-3, con avisos BSD de Go y mmap-go), `github.com/dustin/go-humanize` v1.1.0 (MIT), `github.com/google/uuid` v1.6.0 (BSD-3), `github.com/mattn/go-isatty` v0.0.24 (MIT), `github.com/ncruces/go-strftime` v1.1.0 (MIT), `github.com/remyoudompheng/bigfft` (BSD-3) y sube `golang.org/x/sys` a v0.49.0 (BSD-3) |
| S-DNS | `github.com/miekg/dns` | v1.1.73 | go.sum `h1:uhT8nJxmTrPJYClxVxTCX+CVn6qnzSiybRk72Z6DgrE=` | BSD-3. La v2 vive en codeberg.org/miekg/dns; se usa v1. Dependencia directa en `go.mod` desde k1-01 (hash y `LICENSE` verificados en la caché de módulos); arrastra `golang.org/x/net` v0.57.0 y `golang.org/x/sys` v0.47.0, BSD-3 (`LICENSE` de cada módulo) |
| S-MSGAUTH | `github.com/emersion/go-msgauth` | v0.7.0 | go.sum `h1:vj2hMn6KhFtW41kshIBTXvp6KgYSqpA/ZN9Pv4g1INc=` | MIT. `dkim.Sign`, `dkim.VerifyWithOptions` con `LookupTXT` propio |
| S-SPF | `blitiri.com.ar/go/spf` | v1.6.0 | go.sum `h1:TK91HOya1R2J5b+x+NZfdYTqDqbr+Q+hil5gy8WzLDQ=` | MIT. `CheckHostWithSender` con `WithResolver` |
| S-HARAKA | Haraka | 3.3.5 | npm `sha512-nAO+V/N/YfTHg8Dul2gY3RTt0+ErnqkUAcvsEVDBr7I1yGOTAvjqG+6PT1RRY999a924gpEWJ/tPx+CahZLivw==`, gitHead `dc8d154d1b4ec96d6dfd22a67d87c22b48527651` | MIT. `outbound.ini`: `local_mx_ok`, `temp_fail_intervals` (unidades s/m/h/d, mínimo 5 s), `enable_tls`, `connect_timeout`; `notes.outbound_ip` vía hook `get_mx` |
| S-NODE | Node.js | 26.12.0 | instalado | MIT |
| S-PY | Python | 3.14.8 | instalado; `email.policy.strict` tiene `raise_on_defect=True` (probado) | PSF |
| S-UNSHARE | util-linux `unshare` | 2.42.4 | `--user --map-root-user --net --mount` | GPL-2.0 (herramienta del sistema, no dependencia) |
| S-SYSTEMD | systemd `systemd-run` | 262 | `--user --scope -p` | herramienta del sistema |
| S-IPROUTE | iproute2 `ip`, `tc` | 7.3.0 | `netem delay` probado en el espacio de nombres | herramienta del sistema |
| S-ACTIONS | `actions/checkout`, `actions/setup-go`, `actions/setup-python` | v7.0.1, v7.0.0, v7.1.0 | entradas `go-version` y `python-version` leídas de sus `action.yml` | MIT |

## Mediciones en la máquina objetivo (2026-10-22)

Arch Linux, kernel 7.2.10, 12 CPU, 14 GiB visibles, disco `/home` SSD.

| Medición | Resultado | Usada en |
| --- | --- | --- |
| SQLite WAL + `synchronous=FULL`, 30 `kill -9` aleatorios durante commits confirmados | 3 635 confirmados, 0 perdidos, `integrity_check` ok | D03 |
| Commits de una fila por segundo | FULL ≈ 709/s; NORMAL ≈ 32 207/s | D03 (commits agrupados) |
| `unshare --user --map-root-user --net --mount` sin root | `lo` arriba, bind a 127.0.0.7:25 y 127.0.0.1:53/udp, origen 127.0.0.42, 1.1.1.1 inalcanzable, `mount --bind` de `resolv.conf` | D06 |
| `tc qdisc add dev lo root netem delay 5ms` en el espacio de nombres | RTT 10 ms | D06 |
| `systemd-run --user --scope` y `memory.peak` | cuenta 300 MiB asignados por un hijo | D17 |
| Ingreso de k1-02 (`go test -run TestKillDuringIngest -v`): 16 clientes concurrentes, 5 `SIGKILL` sembrados (semilla 1) sobre el mismo `engine.db` | 15 569 respuestas 202, 15 578 mensajes confirmados, 0 perdidos, `integrity_check` ok, ~1,4 s en total; con el 202 enviado antes del commit (cambio temporal, no versionado) la misma prueba pierde 16 | 2.4, D03 |
| Commits agrupados (`TestAPIGroupCommit`, 32 clientes) | 640 operaciones en 42 commits | 2.4 |
| Mensaje generado leído con `email.message_from_bytes(..., policy=email.policy.strict)` de Python 3.14.8 | sin defectos ni excepción | 3.10 |
| `npm install Haraka@3.3.5` y `haraka --version` | funciona con Node 26.12.0; el script de instalación de `modern-syslog` queda bloqueado y no se usa | D08 |
| `mtadst run` del escenario `limits` (10 800 mensajes, 51 592 eventos, semilla 1) en `/home`, 2026-10-23, kernel 7.2.11 | `synchronous=FULL`: 54,8 s; `synchronous=OFF`: 5,0 s; mismo `trace.sha256` (`12fe285e…`) con los dos | 2.4 (modo simulado) |
| `mtadst run` del escenario `backoff` (semilla 1) en `/home` | 3 mensajes, 4,9 días virtuales (`virtual_ms` 423 315 474), 41 reintentos por mensaje, expiración y DSN: 63 ms de reloj | 2.2, D05 |
| Diez corridas `mtadst run` por escenario (`determinism` y `backoff`, semilla 7) con `GOMAXPROCS` 1, 8 y 15, y tres de `limits` (semilla 3) con 5, 10 y 15 | un solo `trace.sha256` por escenario | 2.2, D05 |
| `events.jsonl` de `determinism` contra `json.dumps(dict(fila), sort_keys=True, separators=(",", ":"))` de Python 3.14.8 sobre `engine.db` | idénticos byte a byte (también lo comprueba `TestEventsMatchPython`) | 3.6 |
| `make verify` de k1-06 (seis propiedades: `profile_sources`, `classification`, `backoff_expiry`, `receiver_limits`, `determinism`, `resources`), 2026-10-23, kernel 7.2.11 | todas `pass`; `memory.peak` del scope 215 048 192 bytes; 7,1 s. Dentro de `unshare --net`, `/sys/class/net` lista las interfaces del anfitrión (`docker0`, `enp5s0`, `lo`), así que `lo` se sube con `ioctl` | 4, D17 |

## Huecos que siguen abiertos

- Directrices oficiales de Gmail: sin fuente primaria. Afecta a `gmail-model`.
- `550 5.7.509` y los textos de Yahoo: solo fuentes secundarias.
- Cifras de límites de cualquier proveedor: no publicadas (R-YAHOO, R-RESEND-FORGE). Toda cifra de perfil es supuesto.
- Rendimiento del motor en esta máquina: no medible antes de que exista; lo mide K1 y ajusta los parámetros de la sección 5 del contrato.
