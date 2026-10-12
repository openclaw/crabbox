<!-- source: how-it-works.md; sha256: 2e0288463ff74a878d06a6226e5924a2ce5426c54796c463626ae14219ab37b5 -->
# Cómo funciona Crabbox {#how-crabbox-works}

Para conocer el modelo completo o identificar qué componente controla un comportamiento antes de cambiarlo.

## Resumen {#tl-dr}

Crabbox ofrece máquinas bajo demanda. Edita localmente mientras ejecutas pruebas y builds en Linux, macOS, Windows nativo o WSL2. En una flota compartida cooperan:

- **CLI** (`cmd/crabbox`, `internal/cli`): reserva, sincronización de cambios, ejecución, salida y limpieza.
- **Coordinador** (`worker/src`): credenciales, estado, caducidad, limpieza, uso y costes. Usa Cloudflare Workers/Durable Object o Node.js/PostgreSQL/pg-boss.

Los ejecutores no guardan secretos del intermediario. El repositorio prepara herramientas, dependencias y trabajo. El coordinador conserva registros, logs y telemetría; descarga o publica capturas y artefactos antes de liberar la máquina. También hay proveedores directos sin coordinador.

## Componentes {#the-pieces}

```text
┌──────────────────────┐   HTTPS / JSON   ┌───────────────────────────┐
│ your machine         │ ───────────────► │ coordinator runtime       │
│ crabbox CLI          │  bearer + owner  │ Cloudflare + Durable Obj. │
│ repo checkout        │                  │ or Node.js + PostgreSQL   │
│ per-lease SSH key    │                  │ provider creds + state    │
└──────────┬───────────┘                  └────────────┬──────────────┘
           │                                         │ provider API
           │                                         ▼
           │                          ┌──────────────────────────────┐
           │ SSH (primary + fallback) │ managed cloud or sandbox     │
           └────────── rsync ────────►│ runner host                   │
                                      │ /work/crabbox/<lease>/<repo> │
                                      └──────────────────────────────┘
```

La CLI contacta por HTTPS al coordinador y después **directamente** por SSH/rsync al ejecutor. El ejecutor no llama al coordinador para comandos ordinarios; ese camino es unidireccional. El coordinador administra reservas, no transporta el repositorio ni ejecuta comandos. La CLI sube metadatos, previsualizaciones, logs y resultados aparte; los puentes interactivos sí lo atraviesan. El [servicio privado AWS](features/aws-private-workspaces.md) es una API separada SSM-only sin plano SSH.

Para interacciones largas, `/v1/control` ofrece un WebSocket autenticado de eventos y latidos, reduciendo consultas con alta latencia. HTTPS sigue siendo almacenamiento canónico y alternativa compatible con versiones anteriores.

## Responsabilidades {#ownership}

| Capa | Controla |
| :--- | :--- |
| **CLI** | Configuración, flags, claves SSH por reserva, preparación, Git/rsync, huellas y comprobaciones, comandos, salida, WebSocket con alternativa HTTP, liberación |
| **Broker** | Autenticación, identidad, estado serializado, credenciales, creación/borrado, caducidad, consultas, registros/logs/eventos/telemetría, uso y topes |
| **Provider** | Cómputo: adaptadores crean y borran Hetzner, AWS EC2, Azure VM, GCP y otros |
| **Runner** | Nada duradero: SSH/Git/rsync y raíz de trabajo en Linux, preparación específica en otros sistemas, hosts SSH estáticos existentes; el repositorio instala sus entornos |

## Qué hace `crabbox run` {#what-crabbox-run-does}

Un recorrido SSH intermediado tiene cinco fases. Los proveedores directos gestionan el ciclo localmente y los delegados gestionan el transporte de ejecución.

**1. Planificar.** Configuración en orden flags → entorno → repositorio → usuario → valores por defecto. Genera ID temporal (`cbx_` + 12 dígitos hexadecimales) y clave en `<user-config>/crabbox/testboxes/<lease>/id_ed25519` (RSA en Windows AWS/Azure).

**2. Reservar.** Envía clase, proveedor, destino, TTL, inactividad, slug, capacidades y clave pública a `POST /v1/leases`. El coordinador autentica, serializa cambios, limita reservas y gasto mensual, consulta precios, reserva el coste máximo TTL y crea con alternativas de región/mercado. Devuelve host, usuario/puerto, directorio, caducidad, ID final y slug. Si cambia el ID, la CLI renombra el directorio de claves.

**3. Sincronizar.** Espera SSH y preparación, inicia Git desde origin/referencia base. Huellas iguales evitan transferencia; si cambian usa rsync POSIX (`/work/crabbox/<lease>/<repo>` en Linux) o tar de manifiesto en Windows nativo. Comprueba integridad y prepara historia base donde se admita.

**4. Ejecutar.** Envía latidos en segundo plano, ejecuta por SSH y transmite stdout/stderr. En modo intermediado replica eventos de fases.

**5. Liberar.** Libera reservas nuevas salvo `--keep`; conserva las reutilizadas con `--id`. `--stop-after` puede cambiarlo. El proveedor limpia y el intermediario libera coste. Si una nueva máquina no retenida falla antes de SSH, `crabbox run` la sustituye una vez. Nunca duplica comandos en reservas retenidas o reutilizadas explícitamente.

## Máquinas preparadas y reutilización {#warm-boxes-and-reuse}

`crabbox warmup` sigue la creación normal, pero conserva la máquina sin ejecutar. Reutiliza explícitamente por slug o ID:

```sh
crabbox warmup --profile project-check --slug blue-lobster
crabbox run --id blue-lobster -- pnpm test:changed
crabbox ssh --id blue-lobster
crabbox stop blue-lobster
```

Cada reserva tiene ID `cbx_…` y slug. Los automáticos incluyen ocho dígitos de huella, como `blue-lobster-1f3a9c2b`. `--slug` personaliza; `--id` acepta ambos. Los latidos actualizan `lastTouchedAt` y la caducidad inactiva. El planificador duradero libera las abandonadas.

## Proveedor intermediado o directo {#brokered-vs-direct-provider}

El camino **intermediado** es habitual:

```text
CLI -> coordinator (Cloudflare or Node/PostgreSQL) -> provider API
CLI -> runner over SSH/rsync
```

Para infraestructura compartida, sin secretos locales y con limpieza, uso y costes centralizados. Solo se activa para `hetzner`, `aws`, `azure`, `daytona`, `gcp` al configurar URL mediante `CRABBOX_COORDINATOR` o `config set-broker`.

Sin URL, el camino es **directo**:

```text
CLI -> provider API
CLI -> runner over SSH/rsync
```

Necesita credenciales locales, como la cadena AWS SDK o token Hetzner. No mantiene historia central ni latidos de intermediario. Sirve para diagnosticarlo y para adaptadores solo directos: entornos aislados, SSH estático y contenedores locales.

`broker.mode: registered` conserva el acceso directo pero replica metadatos por propietario y latidos. Añade inventario, compartir opcional y WebVNC saliente sin transferir credenciales o autoridad de limpieza. Liberar elimina solo metadatos por defecto; un adaptador saliente vinculado explícitamente puede borrar con confirmación del usuario. Los registros no afectan cuotas, costes, imágenes, pools ni mantenimiento gestionados.

SSH estático (`provider: ssh`) siempre evita el intermediario. macOS y WSL2 usan rsync POSIX; Windows nativo usa PowerShell y tar.

## Autenticación e identidad {#auth-and-identity}

Usuarios usan tokens GitHub firmados; automatización fiable usa bearer compartido. Puede añadirse Cloudflare Access; Node solo confía en cabeceras de identidad de los CIDR de proxy configurados.

```text
Authorization: Bearer <token>
X-Crabbox-Owner: <email>
X-Crabbox-Org: <org>
```

Con `crabbox login`, el propietario es `github:<numeric-id>` inmutable. En modo compartido se obtiene de `CRABBOX_OWNER`, entorno de email Git o `git config user.email`; `CRABBOX_ORG` define organización. Se ignoran cabeceras Access sin verificar y solo se acepta el email JWT verificado. Consulta [Seguridad](security.md).

## Modelo de sincronización {#sync-model}

Prioriza lo local, sin exigir copia limpia:

- prepara Git desde origin/base;
- superpone cambios con rsync y checksum opcional;
- omite transferencias sin cambios por huellas;
- excluye directorios pesados;
- protege contra borrados masivos sospechosos;
- prepara historia base para pruebas de cambios.

Personas y agentes editan localmente y ejecutan fuera. `crabbox sync-plan` previsualiza sin modificar.

## Coste y uso {#cost-and-usage}

El intermediario registra dos costes:

```text
estimatedUSD   elapsed runtime cost so far
reservedUSD    worst-case TTL cost reserved before provisioning
```

Prioridad del precio por hora:

```text
1. CRABBOX_COST_RATES_JSON   explicit override
2. provider live pricing     e.g. EC2 spot history, Hetzner server-type prices
3. built-in fallback rates   last-resort defaults
```

Hetzner convierte a USD con `CRABBOX_EUR_TO_USD`. `CRABBOX_MAX_*` limita reservas y USD mensuales globales y por propietario/organización; superar límites devuelve HTTP 429. `crabbox usage` consulta `GET /v1/usage` y agrupa por usuario, organización, proveedor y tipo.

## Fallos y limpieza {#failure-and-cleanup}

Se esperan caídas, SSH desconectado, cloud-init fallido, éxito parcial, reintentos y máquinas que sobrevivan al proceso local:

- decisiones serializadas;
- creación idempotente cuando sea posible;
- etiquetas Crabbox;
- liberación repetible y segura;
- alarmas Durable Object o pg-boss con reconciliación periódica;
- limpieza directa conservadora;
- limpieza intermediada propiedad del intermediario.

Con coordinador configurado, `crabbox cleanup` rechaza barrer recursos: esa responsabilidad pertenece al coordinador.

## Qué leer después {#where-to-go-next}

- [Arquitectura](architecture.md): componentes, API, estado y fallos.
- [Orquestador](orchestrator.md): responsabilidad, ciclo, limpieza y costes.
- [CLI](cli.md): comandos, configuración, salida y códigos.
- [Funciones](features/README.md): por área.
- [Comandos](commands/README.md): por comando.
- [Infraestructura](infrastructure.md): entornos, entrada, secretos, DNS y proveedores.
- [Seguridad](security.md): autenticación, SSH, limpieza y confianza.
