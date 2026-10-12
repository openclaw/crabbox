<!-- source: README.md; sha256: 008297f255e89d26769a8517dd1c425b15462564563cce18859b56b7640a570e -->
# Crabbox {#crabbox}

**Ordenadores bajo demanda para agentes.**

## Qué es Crabbox {#what-crabbox-is}

Crabbox proporciona ordenadores para que agentes y personas ejecuten pruebas y compilaciones. Sigue editando localmente mientras una máquina remota ejecuta tu árbol de trabajo, incluidos los cambios sin confirmar. Crabbox transmite la salida, devuelve el código de salida y limpia la reserva según el contrato del proveedor.

Con un proveedor configurado, ejecuta el comando de pruebas habitual:

```sh
crabbox run -- pnpm test
```

## Un ciclo, muchos tipos de máquina {#one-loop-many-kinds-of-box}

El ciclo de ejecución remota estándar:

1. **Edita localmente** tu copia existente.
2. **Reserva una máquina** o reutiliza capacidad preparada.
3. **Sincroniza** el árbol, incluidos los cambios sin confirmar.
4. **Ejecuta** el comando en la máquina.
5. **Recibe la salida y el código de salida**.
6. **Limpia** la capacidad propia o consérvala para otra ejecución.

Los proveedores delegados pueden gestionar ejecución y sincronización. La reutilización, las evidencias y la limpieza siguen las capacidades del proveedor y la política de ejecución.

La infraestructura puede cambiar sin convertir el flujo en un script específico. Empieza por [Casos de uso](use-cases.md) si conoces la tarea, o por la [Referencia de proveedores](providers/README.md) si conoces el destino.

## Más agentes, menos espera {#more-agents-less-waiting}

- **Agentes en paralelo:** separa pruebas y compilaciones para no competir por CPU, RAM y puertos del portátil.
- **Máquinas preparadas:** reutiliza entornos para acortar los ciclos.
- **Pruebas multiplataforma:** Linux, macOS, Windows nativo o WSL2 desde la misma copia local, según el proveedor.
- **Ver y controlar:** usa [VNC o WebVNC](features/interactive-desktop-vnc.md), solicita un navegador o usa VS Code web con `--code` en Linux gestionado. El [portal](features/portal.md) autenticado muestra reservas y registros; WebVNC permite observar o controlar mientras funciona el puente local.
- **Capacidad de equipo:** un coordinador opcional guarda credenciales, comparte reservas e impone caducidad y topes de gasto para proveedores intermediados.

El [catálogo](providers/README.md) incluye nubes, VM locales, hosts SSH y entornos delegados. El [contrato de plugins externos](providers/external.md) permite conectar tu backend. La web genera la lista y los recuentos desde los metadatos del catálogo.

## Empieza aquí {#start-here}

- [Primeros pasos](getting-started.md) — instalación y primera ejecución.
- [Casos de uso](use-cases.md) — respuesta rápida, agentes, validación de plataformas, QA web, paralelismo y GPU.
- [Precios y costes](pricing.md) — código abierto, capacidad propia y límites.
- [Cómo funciona Crabbox](how-it-works.md) — el recorrido por CLI, coordinador y ejecutor.
- [Visión de Crabbox](../VISION.md) — alcance, objetivos excluidos y seguridad del ciclo de vida.

## Elige un modelo operativo {#pick-an-operating-model}

| Ruta | Uso ideal | Propiedad |
| --- | --- | --- |
| Entorno local | Comprobaciones rápidas sin credenciales | Tu estación y entorno |
| Nube directa o SSH | Cuentas personales, hosts privados, virtualización propia | Tu cuenta o infraestructura |
| Coordinador de equipo | Credenciales, reservas, limpieza, uso y topes compartidos | Tu despliegue Cloudflare o Node.js/PostgreSQL |
| Ejecución delegada | Agentes, CI, navegador o GPU gestionados por el proveedor | Proveedor u operador del entorno |

Crabbox tiene licencia MIT y actualmente no ofrece un plan de control alojado. Cada operador factura el cómputo y la infraestructura del coordinador. Consulta [Precios y costes](pricing.md).

## Límite de confianza {#trust-boundary}

Ejecuta repositorios fiables con compañeros fiables. El aislamiento depende del entorno. Reutilizar y limpiar de forma destructiva exige verificar la propiedad del recurso y de su declaración exactos. Si es ambigua, la operación se rechaza.

Usa [Selección de proveedores](features/provider-selection.md) y lee la documentación del proveedor y el [modelo de seguridad](security.md). Recomendar una ruta no certifica su seguridad.

## Profundiza {#go-deeper}

- **CLI y configuración:** [CLI](cli.md), [Comandos](commands/README.md), [Configuración](features/configuration.md), [Incorporación de repositorios](features/repository-onboarding.md).
- **Flota y operaciones:** [Arquitectura](architecture.md), [Infraestructura](infrastructure.md), [Operaciones](operations.md), [Observabilidad](observability.md), [Emparejamiento de solo lectura](features/device-pairing.md), [Seguridad](security.md).
- **Ejecuciones y evidencias:** [Trabajos](features/jobs.md), [Preparación desde Actions](features/actions-hydration.md), [Artefactos](features/artifacts.md), [Puntos de control](features/checkpoints.md), [Escritorio interactivo y VNC](features/interactive-desktop-vnc.md).
- **Límites de plataforma:** [Ejecución anidada](features/nested-execution.md) distingue WSL2, motores de contenedores, hosts KVM preparados y entornos locales.
- **Extensión:** [Integraciones](integrations/README.md), [Creación de proveedores](features/provider-authoring.md), [Mapa del código](source-map.md).

## Sobre esta documentación {#about-these-docs}

El Markdown de `docs/` es la fuente de [crabbox.sh](https://crabbox.sh/). La implementación es la referencia; el [Mapa del código](source-map.md) relaciona el comportamiento con su responsable.

Construye y valida la web localmente:

```sh
scripts/check-docs.sh
open dist/docs-site/index.html
```

En `docs/`, usa encabezados de nivel 1–4 y bloques delimitados por tres acentos graves. Los enlaces a encabezados se comprueban con los mismos identificadores que genera el renderizador; ejemplos y comentarios no reservan anclas. Conserva los ID publicados. El Markdown exclusivo del repositorio mantiene las reglas previas de anclas estilo GitHub, sin prometer compatibilidad completa con GitHub Markdown.

Consulta [Traducir la documentación](translations.md) para añadir o actualizar un idioma.
