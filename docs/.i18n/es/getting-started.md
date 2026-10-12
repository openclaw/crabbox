<!-- source: getting-started.md; sha256: 6f010425dbbffe15196dfbc9cbfce66ea14783090c7babf231211bf13239fc09 -->
# Primeros pasos {#getting-started}

Crabbox ofrece ordenadores bajo demanda a agentes y personas. Sigue editando localmente: reserva una máquina, sincroniza cambios sin confirmar, ejecuta el comando, transmite la salida y devuelve el código de salida. Las ejecuciones puntuales liberan la máquina; las reservas preparadas permanecen disponibles.

Las pruebas remotas liberan CPU, memoria y puertos para trabajar en paralelo. Elige un proveedor compatible con Linux, macOS, Windows nativo o WSL2. Para tareas visuales, consulta la [guía de escritorio, VNC, navegador, Code y portal](features/interactive-desktop-vnc.md).

Esta guía lleva un repositorio fiable desde la instalación hasta `crabbox run -- pnpm test`. Revisa primero la configuración y los comandos de preparación. Sin cuenta, `--provider local-container` ejecuta en Docker o Podman local; la [habilidad `crabbox-quickstart`](integrations/agents.md#install-through-ecosystem-skill-managers) guía todo el recorrido sin credenciales.

## Paso 1. Instalar {#step-1-install}

```sh
brew install openclaw/tap/crabbox
```

Verifica la instalación:

```sh
crabbox --version
crabbox doctor
```

`crabbox doctor` imprime una línea por comprobación. Recién instalado, el control neutral `git` debería indicar `ok`; los controles de proveedor empiezan al seleccionarlo. Sin selección muestra `no provider selected`, conserva `source=compiled_default selected=false` y omite la preparación de credenciales sin fallar. Antes de gestionar máquinas, selecciona mediante configuración, `CRABBOX_PROVIDER` o `--provider`. La selección se valida estrictamente y puede fallar sin credenciales; compara con `crabbox providers recommend`.

Sin Homebrew, descarga el archivo adecuado para macOS, Linux o Windows en <https://github.com/openclaw/crabbox/releases>. Los ejecutables macOS están firmados y notarizados.

Conserva `crabbox-runtime` junto al ejecutable real. Ambas arquitecturas Linux pertenecen al controlador correspondiente; copiar solo la CLI o mezclar versiones no conserva el paquete completo.

Los usuarios de Go pueden instalar solo la CLI de una versión explícita. Elige una etiqueta en [GitHub Releases](https://github.com/openclaw/crabbox/releases); se admite desde v0.44.0:

```sh
go install github.com/openclaw/crabbox/cmd/crabbox@v0.70.0
```

El módulo exige Go 1.26 y prefiere go1.26.5. Usa Go 1.26.5 o posterior, o selección automática. Esto compila solo la CLI, sin auxiliares como `crabbox-apple-vm-helper` ni paquete de ejecución; tampoco es la distribución precompilada firmada y notarizada. Para la distribución completa, especialmente Apple VM en Apple Silicon, usa Homebrew o el archivo de versión.

La selección de transferencias WSL2 y Windows x64 sin WSL exige `main` actual o Crabbox v0.42.1 o posterior. Consulta la [instalación de Windows](windows-install.md).

## Paso 2. Iniciar sesión {#step-2-log-in}

Para una flota compartida, usa la URL del coordinador del equipo. Guarda credenciales y aplica límites de reservas, gastos y caducidad. Proveedores directos y locales no necesitan login; consulta [Elegir una ruta de acceso](#choosing-an-access-path).

```sh
crabbox login --url https://broker.example.com
```

`login` abre GitHub OAuth en el navegador; `--no-browser` imprime la URL para otro navegador del mismo dispositivo. El intermediario intercambia el código, verifica la pertenencia a la organización y devuelve una confirmación de un uso al listener local antes de guardar el token firmado:

```text
logged in broker=https://broker.example.com provider=hetzner user=github:12345 org=example-org config=/Users/alice/Library/Application Support/crabbox/config.yaml
```

Después, cada comando `crabbox` autentica automáticamente. Comprueba tu identidad:

```sh
crabbox whoami
```

```text
user=github:12345 org=example-org auth=user broker=https://broker.example.com
```

### Elegir una ruta de acceso {#choosing-an-access-path}

El acceso depende del despliegue. Usa la URL y la lista de organizaciones/equipos permitidos de tu equipo. Completar OAuth no evita el rechazo de cuentas fuera de la lista.

Para instalaciones personales o de terceros:

- **Proveedor directo:** credenciales locales para pruebas privadas rápidas; estado y limpieza locales en lugar de historial central y topes compartidos.
- **Coordinador propio:** Cloudflare Workers o Node.js/PostgreSQL para credenciales centrales, límites activos y mensuales, `crabbox usage`, limpieza duradera y endpoint común.
- **Solicitar acceso:** solo si el operador define un proceso para tu organización. Un endpoint de equipo no es automáticamente público.

El modo directo omite `login`:

```sh
crabbox doctor --provider hetzner
crabbox run --provider hetzner -- pnpm test
```

Para autohospedar, elige entorno:

- **Cloudflare Workers:** estado Durable Object, alarmas, limpieza programada y Cloudflare Access opcional.
- **Node.js/PostgreSQL:** servicio HTTP/WebSocket, PostgreSQL 13+, mantenimiento pg-boss y una réplica permanente. Es el entorno portable inicial; completa la prueba de despliegue antes de pasar producción.

Comparten secretos, autenticación, presupuestos, API y portal. Consulta [Infraestructura](infrastructure.md#choose-a-coordinator-runtime). Login web requiere una app OAuth y al menos una organización/equipo permitido; automatización con token compartido no.

Para CI sin navegador, usa un token compartido:

```sh
printf '%s' "$TOKEN" | crabbox login \
  --url https://broker.example.com \
  --provider aws \
  --token-stdin
```

Consulta [Autenticación y administración](features/auth-admin.md).

## Paso 3. Incorporar un repositorio {#step-3-onboard-a-repo}

Dentro del repositorio:

```sh
crabbox init
```

`init` escribe tres archivos. Cambia rutas con `--config`, `--workflow` o uno o más `--skill`; `--force` sobrescribe archivos existentes:

```text
.crabbox.yaml                          repo defaults (profile, class, sync, env)
.github/workflows/crabbox.yml          Actions hydration workflow (optional)
.agents/skills/crabbox/SKILL.md        agent-facing skill instructions
```

Ajusta los valores de `.crabbox.yaml`:

- `profile`: nombre del entorno; la plantilla usa `<repo>-check`.
- `class`: `tiny`, `small`, `standard`, `fast`, `large` o `beast`; la plantilla usa `beast`.
- `sync.exclude`: directorios que nunca se envían.
- `sync.include`: lista permitida relativa a la raíz; tras exclusiones, sincroniza **solo** esas rutas.
- `env.allow`: variables visibles para el comando remoto.

`--detect` detecta pruebas y escribe `jobs.detected`, ejecutable con `crabbox job run detected`.

Previsualiza la sincronización:

```sh
crabbox sync-plan
```

`sync-plan` muestra número de archivos, bytes y archivos mayores. Si aparecen `dist/`, `.cache/` o recursos enormes inesperados, restringe `sync.exclude` y repite. El tamaño limita la primera sincronización.

Los espacios nativos de Jujutsu con backend Git funcionan si está instalado `jj`. Crabbox captura cambios y sincroniza el árbol `@` exacto, incluidas fusiones con varios padres. `sync-plan --json` muestra la revisión; usa `run --no-hydrate` con SSH ordinario. Los espacios Git/Jujutsu colocados usan la ruta Git. Consulta [Sincronización Jujutsu](features/sync.md#jujutsu-workspaces) para requisitos y rechazos seguros.

## Paso 4. Preparar una máquina {#step-4-warm-a-box}

```sh
crabbox warmup --class standard --slug first-tests
```

`warmup` reserva y prepara la máquina, espera SSH y herramientas, conserva la reserva (`--keep` por defecto) e imprime resumen, endpoint y tiempo:

```text
leased cbx_abcdef123456 slug=first-tests provider=hetzner server=... type=... ip=203.0.113.10 idle_timeout=30m0s expires=...
ready ssh=crabbox@203.0.113.10:2222 network=public workroot=/work/crabbox
warmup complete total=...
```

El coordinador la mantiene lista. Inactividad de 30 minutos y TTL de 90 minutos por defecto acotan su vida. Los proveedores directos aplican sus propias reglas; detén explícitamente al terminar. Las clases varían: consulta `crabbox providers --json` antes de elegir.

Reutiliza con `slug` o `id` (`cbx_...`); ambos funcionan en `--id`.

## Paso 5. Ejecutar un comando {#step-5-run-a-command}

```sh
crabbox run --id first-tests -- pnpm test
```

El proceso:

1. Verifica SSH.
2. Prepara Git remoto desde origin y referencia base; superpone cambios con rsync. La huella evita sincronizar si no hay cambios.
3. Ejecuta por SSH y transmite stdout/stderr.
4. Envía latidos para evitar caducidad durante el trabajo.
5. Registra `run_...` con tiempos, código de salida y telemetría acotada en Linux.

El repositorio instala herramientas y dependencias. Ejecuta su preparación o [hidrata desde Actions](features/actions-hydration.md) antes de probar.

Omite `--id` para una ejecución puntual:

```sh
crabbox run -- pnpm test
```

Obtiene una reserva nueva y la libera al salir. Para iteraciones usa `warmup` + `--id`.

## Paso 6. Examinar el historial {#step-6-inspect-history}

```sh
crabbox history
crabbox events run_abcdef123456
crabbox logs run_abcdef123456
crabbox results run_abcdef123456
```

`history` enumera ejecuciones; `events` muestra eventos ordenados de reserva, sincronización, comando, salida y fin; `logs` devuelve salida conservada; `results` interpreta JUnit adjunto.

Con portal habilitado, `/portal/runs/run_abcdef123456` muestra los mismos datos.

## Paso 7. Detener la reserva {#step-7-stop-the-lease}

Al terminar:

```sh
crabbox stop first-tests
```

`stop` aplica la limpieza del proveedor y libera coste reservado. Normalmente elimina máquinas desechables; conserva hosts SSH estáticos y respeta políticas de recursos retenidos. Exige propiedad verificada. Pueden quedar declaraciones de recuperación o recibos terminales de ID fijo locales. El coordinador programa la limpieza de reservas olvidadas.

```sh
crabbox cleanup --dry-run
```

`cleanup` barre restos directos y estado local. La limpieza intermediada corresponde a las alarmas del coordinador.

## Variaciones habituales {#common-variations}

Mantener una sesión larga:

```sh
crabbox warmup --idle-timeout 4h --ttl 8h --slug swift-crab
crabbox run --id swift-crab -- pnpm test
crabbox run --id swift-crab -- pnpm bench
crabbox stop swift-crab
```

Abrir un escritorio:

```sh
crabbox warmup --desktop --browser --slug ui-check
crabbox webvnc --id ui-check --open --take-control
```

Abrir code-server:

```sh
crabbox warmup --code --slug code-check
crabbox run --id code-check --sync-only
crabbox code --id code-check --open
```

Mantén los puentes activos mientras usas las pestañas. Desktop y Code dependen del destino/proveedor; Code web requiere coordinador y origen Code aislado. La [guía conjunta](features/interactive-desktop-vnc.md) explica VNC, WebVNC, portal, compartir y limpiar.

Abrir en Zed Remote Projects:

```sh
crabbox run --id swift-crab --sync-only
crabbox open --editor=zed --id swift-crab
```

Pega el comando SSH en **Connect New Server**, abre la carpeta indicada y mantén `crabbox open` activo.

Usar un Mac existente (SSH estático, sin aprovisionar):

```yaml
# .crabbox.yaml
provider: ssh
target: macos
static:
  host: mac-studio.local
  user: alice
  port: "22"
  workRoot: /Users/alice/crabbox
```

```sh
crabbox run -- xcodebuild test
```

Cambiar proveedor y clase por comando:

```sh
crabbox run --provider aws --class beast -- pnpm test
```

## Próximos pasos {#where-to-go-next}

- [Cómo funciona](how-it-works.md) — modelo mental.
- [CLI](cli.md) — comandos y códigos.
- [Comandos](commands/README.md) — referencia individual.
- [Funciones](features/README.md) — guía por función.
- [Configuración](features/configuration.md) — YAML y precedencia.
- [Proveedores](providers/README.md) — elegir destino.
- [Crear proveedores](features/provider-authoring.md) — añadir uno.
- [Solución de problemas](troubleshooting.md) — pasos fallidos.
