<!-- source: concepts.md; sha256: 27d2835bb5b9d8ffdb9a39d681b5f2c01215e10c33a0199b1b1e2dd3347e29c8 -->
# Conceptos {#concepts}

Crabbox proporciona ordenadores a agentes y personas. Editas localmente; una **reserva** aparta la máquina y una **ejecución** sincroniza y ejecuta un comando. Las máquinas independientes permiten pruebas paralelas sin disputar CPU, memoria ni puertos.

Hay destinos Linux, macOS, Windows nativo y WSL2. Una reserva preparada mantiene el entorno; Desktop, Browser y Code añaden interacción y el portal muestra reservas y pruebas. Consulta la [guía de acceso](features/interactive-desktop-vnc.md).

Este glosario define términos compartidos. La forma preferida aparece en **negrita**.

## Vocabulario de cómputo {#compute-vocabulary}

**Lease (reserva):** asignación temporal de un ejecutor creado o resuelto por Crabbox, con ID `cbx_...`, slug, inactividad, TTL y estado `active`, `released`, `expired` o `failed`. Unidad de coste y limpieza.

**Runner (ejecutor):** máquina preparada por el proveedor para una o varias ejecuciones. Hetzner, AWS EC2 y SSH estático son ejecutores según el backend.

**Box** / **testbox:** sinónimo informal; prefiere runner salvo contextos de producto como máquina preparada.

**Pool:** ejecutores activos visibles para usuario, organización o flota, expuestos por `crabbox list` y `GET /v1/pool` administrativo.

**Pond:** grupo espontáneo con etiqueta `pond=<name>`, sin objeto central; existe mientras quede un miembro activo. Usa `crabbox pond peers`, `crabbox pond
connect`/`disconnect`, `crabbox pond release`. Véase [Pond](features/pond.md).

**Lease ID:** `cbx_` y 12 caracteres hexadecimales minúsculos, 16 en total, como `cbx_abcdef123456`; aparece en etiquetas, logs, declaraciones y API. [Identificadores](features/identifiers.md).

**Slug:** nombre amigable. Los automáticos combinan palabras y huella de ocho dígitos, como `swift-crab-1f3a9c2b`, con sufijo de cuatro ante colisión. `--slug swift-crab` personaliza; `--slug` normaliza a `[a-z0-9-]`, máximo 41 caracteres. `--id` acepta ambos.

**Run:** una llamada `crabbox run`, con ID `run_`, reserva, comando y código. El coordinador conserva historia; el modo directo usa ID en tiempos, pruebas y fallos locales. [Historial y logs](features/history-logs.md).

**Workspace:** copia y estado del entorno: archivos, dependencias, directorio del proveedor, salida y escritorio/navegador opcional. Úsalo si el producto va más allá de la máquina.

## Roles {#roles}

**CLI:** binario Go local `crabbox`; configuración, sincronización, ejecución, salida y claves SSH. [Arquitectura](architecture.md).

**Broker** / **coordinator:** plano compartido autenticado; credenciales, estado, caducidad, limpieza, historia, uso y topes. Cloudflare Workers/Fleet Durable Object o Node.js/PostgreSQL/pg-boss. Coordinator enfatiza servicio; broker, frontera de confianza.

**Provider:** adaptador que adquiere, resuelve, lista y libera máquinas. Nubes AWS, Azure, Google Cloud, Hetzner; Proxmox, Parallels, Local Container y SSH; entornos SSH Daytona, KubeVirt, External, Namespace Devbox, Semaphore, Sprites, exe.dev, RunPod, Railway; delegados Blacksmith Testbox, E2B, Modal, Islo, Tensorlake, [Upstash Box](https://upstash.com/docs/box/overall/quickstart), Cloudflare, Azure Dynamic Sessions, W&B. [Referencia](providers/README.md).

**Backend:** interfaz Go: `SSHLeaseBackend` entrega un destino SSH real; `DelegatedRunBackend` controla la ejecución. [Backends](provider-backends.md).

**Operator:** persona con acceso administrativo que ejecuta `crabbox admin` y creación/promoción de imágenes.

**Agent:** proceso LLM que llama a la CLI. Crabbox aporta ejecución y evidencias; agente/arnés controla prompts, decisiones y orquestación. Configuración y cargas deben ser fiables.

## Modos {#modes}

**Brokered mode** / **coordinator mode:** el coordinador gestiona creación, estado y limpieza; la CLI ejecuta SSH/rsync directamente. Secretos centrales. Para AWS, Azure, Daytona, Google Cloud, Hetzner con `CRABBOX_COORDINATOR` o `config set-broker`.

**Direct mode** / **direct-provider mode:** API directa sin intermediario, historia central ni topes; único modo fuera de esos cinco y alternativa de ellos sin URL.

**Static mode:** `provider: ssh`; host del operador, nunca creado ni borrado por Crabbox. Siempre directo.

**Delegated mode:** Blacksmith, E2B, Modal, Islo y otros ejecutan y devuelven salida. Rechaza opciones locales `--sync-only`, `--checksum`; tiempo `sync=delegated`.

## Comandos {#commands}

**warmup:** adquiere y prepara, con `--keep` activado; aún no ejecuta comandos.

**run:** adquiere/reutiliza, sincroniza, ejecuta y devuelve salida/código. Nuevas reservas se liberan y reutilizadas se conservan; `--keep` y `--stop-after` lo controlan.

**stop** / **release:** termina según contrato; normalmente borra recursos desechables, salvo hosts estáticos y recursos retenidos.

**cleanup:** limpia restos directos elegibles tras verificar propiedad exacta. Una etiqueta, nombre o ID no basta.

**reuse:** `--id` o slug elige una reserva existente para `run`. `warmup` crea; `--lease-id` es creación idempotente donde se admite, no reutilización genérica.

**reclaim:** mueve la declaración entre copias para reutilizar desde otro repositorio (`--reclaim`); por defecto la reserva está vinculada al repositorio.

**hydrate:** instala dependencias desde Actions del repositorio. Ejecuta pasos compatibles; `--github-runner` registra un ejecutor propio y lanza una tarea real si se necesita toda la semántica. [Preparación Actions](features/actions-hydration.md).

## Estado {#state}

**Idle timeout:** máximo sin latido, por defecto 1800 s/30 min; se reinicia con latido o toque.

**TTL:** duración absoluta, 5400 s/90 min por defecto, máximo 86400 s/24 h; no se prolonga con latidos. `expiresAt = min(createdAt + ttl, lastTouchedAt + idleTimeout)`.

**Heartbeat:** `POST /v1/leases/{id}/heartbeat` durante comandos largos; actualiza `lastTouchedAt`, puede transmitir telemetría y cambiar inactividad explícitamente. Se rechaza al alcanzar `expiresAt`; entonces manda la limpieza.

**Touch:** actualización de estado/inactividad. `Touch` del proveedor es directo; latidos son el equivalente intermediado.

**Reserved cost:** coste máximo `hourlyRate × ttl`, reservado contra el tope mensual hasta liberar.

**Estimated cost:** precio por tiempo `active`; estimación de `crabbox usage`. [Coste y uso](features/cost-usage.md).

## Sincronización {#sync}

**Manifest:** lista de rutas separadas por NUL desde `git ls-files --cached` y `git ls-files --others --exclude-standard`; borrados aparte.

**Fingerprint:** hash de commit, metadatos modificados y manifiesto. Coincidencia evita rsync.

**Git seeding:** preparación inicial opcional de Git remoto desde origin/base para disponer de diferencias.

**Base ref:** referencia para preparar e hidratar, configurada con `sync.baseRef`.

**Sanity check:** detecta borrados masivos, entradas ausentes y resultados sospechosos después de rsync. [Sincronización](features/sync.md).

## Capacidades {#capabilities}

**Desktop:** `--desktop` añade TigerVNC/XFCE redimensionable en Linux gestionado y nuevos contenedores, o Wayland/GNOME. Requerido para `crabbox
vnc`, `crabbox webvnc` y la mayoría de pruebas visuales `--browser`.

**Browser:** `--browser` instala o descubre un navegador y exporta su ruta. Puede ser headless; `--desktop` lo hace visible. Motor según imagen/proveedor, sin perfil autenticado.

**Code:** `--code` instala code-server en loopback 8080 para `crabbox code` y puente `/code/`; solo Linux gestionado.

**Tailscale:** acceso opcional a Linux gestionado mediante tailnet sin IP pública. Distinto de elegir plano con `--network tailscale`. [Capacidades](features/capabilities.md).

## Entorno del coordinador {#coordinator-runtime}

**FleetCoordinator:** lógica TypeScript común de rutas, autenticación, adaptadores, registros, costes, limpieza, portal y puentes.

**Durable Object:** estado y decisiones serializadas Cloudflare; un Fleet con `idFromName("default")`.

**PostgreSQL runtime:** despliegue Node; esquema `crabbox` para registros y `crabbox_jobs` de pg-boss para mantenimiento preciso y periódico.

**Alarm:** fecha futura de limpieza; Durable Object o pg-boss programan inactividad, TTL, reintentos y mantenimiento.

**Portal:** interfaz autenticada renderizada en servidor en `/portal/...`, con reservas, logs, eventos y VNC/Code. [Portal](features/portal.md).

**Bridge:** enlace del coordinador al relay CLI. WebVNC/Code mantienen túneles SSH a VNC 5900/code-server 8080 y deben seguir activos. Egress tiene relay con tickets independiente. El coordinador autentica, no abre SSH.

## Identidad {#identity}

**Owner:** dueño de la reserva; GitHub vincula `github:<numeric-id>` inmutable. Automatización usa dueño configurado o email Git bajo validación de identidad. [Seguridad operativa](security.md#authentication).

**Org:** espacio de organización obtenido del token o `CRABBOX_ORG`, para uso y topes multitenant.

**Allowed org:** pertenencia exigida para emitir login firmado, configurada por Worker.

**Admin token:** ámbito separado para `GET /v1/pool`, rutas administrativas y listado global; más restringido que automatización compartida.

**Cloudflare Access:** capa opcional; confía en `cf-access-jwt-assertion` verificado, no cabeceras sin validar.

## Almacenamiento {#storage}

**State directory:** declaraciones, historia y checkpoints. `XDG_STATE_HOME` explícito selecciona `$XDG_STATE_HOME/crabbox` también para claves/confianza. Sin él usa `<os-user-config-dir>/crabbox/state`, y claves en `<os-user-config-dir>/crabbox/testboxes`. Cambiar raíz no migra ni busca el estado anterior. [Claves SSH](features/ssh-keys.md).

**Claim:** JSON que vincula reserva y copia local; necesario en `crabbox run --id` para slugs y rechazar reutilización entre repositorios sin `--reclaim`.

**Workdir** / **work root:** destino de sincronización. `/work/crabbox` en Linux, `C:\crabbox` en Windows normal; otros dependen del proveedor/destino.

## Evidencias y artefactos {#evidence-and-artifacts}

**Run history:** registro pasado del intermediario (`crabbox history`, `logs`, `events`, `results`), distinto del estado actual. [Historial](features/history-logs.md).

**Test results:** JUnit interpretado por `crabbox results <run-id>`: suites, pruebas, fallos, errores, omitidos. [Resultados](features/test-results.md).

**Telemetry:** muestras Linux de carga, memoria, disco y uptime por ejecución; no analítica de producto. [Telemetría](features/telemetry.md).

**Artifacts:** paquetes de `crabbox artifacts` con capturas, vídeo, GIF, logs, estado y metadatos, publicables en PR. [Artefactos](features/artifacts.md).

**Egress:** `crabbox egress` dirige tráfico saliente a través del equipo operador por WebSocket. [Egress](features/egress.md).

**Checkpoint:** estado restaurable/bifurcable de VM o workspace, ID `chk_...`, archivos, recetas o snapshots/imágenes nativas. [Checkpoints](features/checkpoints.md).

**Cache volume:** almacenamiento persistente por repositorio/runtime/plataforma en ruta absoluta, solo para acelerar. No es escenario bifurcable; el árbol sigue siendo la referencia. [Cachés](features/cache.md).

**Capsule:** fallo portable capturado por `crabbox capsule from-actions` y repetido por `crabbox capsule
replay`. [Cápsulas](features/capsules.md).

## Documentación {#documentation}

**Source map:** enlaza comportamiento e implementación. [Mapa](source-map.md).

**Feature page:** `docs/features/<name>.md`, explicación conceptual enlazada desde comandos/proveedores.

**Command page:** `docs/commands/<name>.md`, flags, comportamiento y códigos coherentes con `--help`.

**Provider page:** `docs/providers/<name>.md`, destinos, configuración, variables, sincronización y fallos.

Documentos relacionados:

- [Funcionamiento](how-it-works.md)
- [Arquitectura](architecture.md)
- [CLI](cli.md)
- [Configuración](features/configuration.md)
- [Backends](provider-backends.md)
