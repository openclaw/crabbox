<!-- source: use-cases.md; sha256: 769c5e58a1b095392245d30f0377bf87b025fa28c3eeca64f9337340d43cd3d7 -->
# Casos de uso {#use-cases}

Da a cada agente de programación un ordenador bajo demanda para sus pruebas y compilaciones. Los agentes siguen editando sus copias locales mientras Crabbox sincroniza cambios sin confirmar, ejecuta comandos a distancia y devuelve la salida y los códigos de salida. Las reservas remotas independientes evitan competir por la CPU, la memoria y los puertos del portátil.

Elige Linux, macOS, Windows nativo o WSL2 cuando importe el comportamiento de la plataforma. Reutiliza una máquina preparada para ejecuciones repetidas y abre un escritorio o editor web para inspeccionar el resultado. La [guía de escritorio, VNC, navegador, Code y portal](features/interactive-desktop-vnc.md) explica el acceso interactivo y los traspasos.

Empieza por la carga de trabajo, no por el proveedor:

```sh
crabbox providers recommend
crabbox providers recommend fast-feedback
crabbox providers recommend agent-sandbox --json
```

Sin un caso de uso, `crabbox providers recommend` muestra todos los nombres admitidos. Las recomendaciones usan metadatos de capacidades del repositorio: no contactan con proveedores, comprueban credenciales o cuotas, miden tiempos de inicio ni certifican límites de seguridad.

## Acelerar pruebas y compilaciones {#speed-up-test-and-build-loops}

Úsalo cuando un comando del repositorio sea demasiado lento, consuma mucha memoria o interfiera con el trabajo local.

```sh
crabbox providers recommend fast-feedback
crabbox providers recommend linux-vm
crabbox run --provider hetzner -- pnpm test
```

Prefiere `fast-feedback` para entornos locales, cachés reutilizables y ciclos cortos de edición y ejecución. Prefiere `linux-vm` para sacar el trabajo del equipo o cuando necesites una VM completa y semántica SSH.

## Dar a un agente un entorno desechable {#give-a-coding-agent-a-disposable-environment}

Usa una reserva SSH remota o un entorno aislado delegado para ejecutar comandos fuera del equipo. Un entorno aislado local separa el espacio de trabajo, pero sigue usando recursos del portátil. Crabbox se ocupa de ejecutar y limpiar; el agente o arnés que lo llama gestiona la orquestación.

Para agentes independientes, crea una reserva desde cada copia de trabajo:

```sh
crabbox warmup --provider hetzner --class standard --slug agent-tests
crabbox run --id agent-tests -- pnpm test
crabbox stop agent-tests
```

Usa un slug distinto por reserva. Las reservas preparadas conservan la configuración entre comandos; revisa la configuración del repositorio e instala sus herramientas y dependencias antes de probar.

```sh
crabbox providers recommend agent-sandbox
crabbox providers recommend disposable-execution
crabbox providers recommend isolated-execution
```

Son tres preguntas diferentes:

- `agent-sandbox` prioriza proveedores diseñados para agentes y entornos de desarrollo;
- `disposable-execution` exige un entorno temporal que se pueda limpiar;
- `isolated-execution` prioriza límites de aislamiento locales y delegados.

La última recomendación orienta el enrutamiento, no certifica seguridad. Crabbox presupone operadores, configuración y cargas fiables; una máquina desechable no vuelve seguro el código hostil. Lee el modelo de confianza y red del proveedor antes de enviar secretos.

## Validar Linux, Windows, WSL2 o macOS {#validate-linux-windows-wsl2-or-macos}

Usa recomendaciones de plataforma cuando el comportamiento dependa del sistema operativo:

```sh
crabbox providers recommend linux-vm
crabbox providers recommend windows
crabbox providers recommend macos

crabbox run --provider aws --target windows --windows-mode wsl2 -- uname -a
```

Windows nativo ejecuta PowerShell y herramientas de Windows. WSL2 ejecuta el contrato POSIX dentro de una VM Windows con virtualización anidada. Los destinos macOS pueden ser VM Apple Silicon locales o capacidad Mac en la nube, según el proveedor.

## Probar un navegador o escritorio visible {#test-a-browser-or-visible-desktop}

Usa una reserva SSH con escritorio cuando una prueba necesite navegador, capturas, entrada o traspaso a un compañero:

```sh
crabbox providers recommend desktop
crabbox warmup --provider azure --target windows --windows-mode normal --desktop --slug ui-check
crabbox webvnc --id ui-check --open --take-control
```

WebVNC permite observar y tomar el control a compañeros autorizados. VNC nativo usa un túnel SSH; `crabbox code` abre code-server en reservas Linux compatibles. El portal muestra reservas, estado de los puentes y evidencias conservadas. Mantén activo el puente local mientras usas el navegador. Consulta los requisitos en la [guía de acceso combinada](features/interactive-desktop-vnc.md).

Combínalo con rutas de evidencia si el resultado debe sobrevivir a la máquina:

```sh
crabbox providers recommend run-evidence
crabbox providers recommend failure-diagnostics
```

## Reutilizar el estado preparado {#reuse-warm-state}

Usa una máquina preparada cuando repetir la instalación cueste más que conservar o restaurar el estado:

```sh
crabbox providers recommend warm-start
crabbox providers recommend versioned-workspace

crabbox warmup --slug warm-tests
crabbox run --id warm-tests -- pnpm test:changed
```

`warm-start` considera entornos locales, volúmenes de caché, sesiones conservadas, pausa/reanudación y estado del espacio de trabajo. No garantiza las mismas primitivas de instantáneas o grupos preparados en todos los proveedores.

## Distribuir experimentos en paralelo {#fan-out-parallel-experiments}

Usa espacios de trabajo con fork para ramas paralelas, varios intentos de agentes o fragmentos de prueba que partan de un estado preparado:

```sh
crabbox providers recommend fanout-testing
crabbox checkpoint fork <checkpoint-id> --count 4
```

Es enrutamiento neutral de puntos de control y bifurcaciones, no una garantía de fork de la memoria en vivo de una microVM.

## Ejecutar cargas GPU {#run-gpu-workloads}

Usa la recomendación GPU para pruebas de modelos, compilaciones CUDA, renderizado y otros trabajos con aceleradores:

```sh
crabbox providers recommend gpu
crabbox run --provider runpod -- nvidia-smi
```

Los proveedores GPU con reserva SSH suelen encajar mejor en depuración interactiva. Los delegados suelen ser preferibles cuando el proveedor deba gestionar la ejecución y recopilar resultados.

## Ejecutar localmente sin credenciales de nube {#run-locally-without-cloud-credentials}

Usa un contenedor, VM o entorno aislado por políticas local para una prueba rápida en tu equipo:

```sh
crabbox providers recommend local
crabbox providers recommend offline-validation
crabbox run --provider local-container -- pnpm test
```

Local no implica un único modelo de aislamiento. Un contenedor compatible con Docker, Apple VM, Multipass VM, Windows Sandbox y un entorno local por políticas tienen contratos distintos de acceso al host, persistencia y limpieza. Lee la página del proveedor antes de ejecutar código desconocido.

## Usar infraestructura existente {#use-infrastructure-you-already-own}

Crabbox puede usar un host SSH permanente, virtualización privada o una cuenta de nube directa sin coordinador compartido:

```sh
crabbox providers recommend byo-ssh
crabbox providers recommend self-hosted
crabbox providers recommend linux-vm
```

Usa `ssh` si Crabbox no debe controlar el ciclo de vida del host. Usa proveedores autohospedados como Proxmox, XCP-ng, Incus, KubeVirt o Firecracker si debe crear y limpiar el entorno.

## Compartir una flota de equipo gobernada {#share-a-governed-team-fleet}

Usa el coordinador para compartir credenciales, propiedad de reservas, limpieza, registros, límites de reservas activas y topes mensuales:

```sh
crabbox providers recommend team-cloud
crabbox login --url https://broker.example.com
crabbox usage --scope user
```

El coordinador es un plano de control. La CLI sigue conectándose directamente a los ejecutores SSH normales para sincronizar y ejecutar, mientras registra evidencias en el coordinador. Abre `/portal` para ver reservas y enlaces a escritorios, editores y detalles. Registrar proveedores directos añade visibilidad sin transferir credenciales ni la responsabilidad de limpieza.

## ¿Necesitas una ruta más específica? {#need-a-more-specific-route}

La CLI también recomienda proveedores para artefactos, pruebas de CI, intérpretes, diagnóstico, conexión MCP, contención de red, pausa/reanudación, URL de vista previa, conectividad, desarrollo remoto, observabilidad, sesiones reutilizables, pruebas web y ejecución de Worker/módulos.

Consulta [`crabbox providers recommend`](commands/providers.md#providers-recommend) para la lista actual, alias, filtros, puntuaciones y advertencias.

## Documentación relacionada {#related-docs}

- [Selección de proveedores](features/provider-selection.md)
- [Referencia de proveedores](providers/README.md)
- [Ejecución anidada](features/nested-execution.md)
- [Precios y costes](pricing.md)
