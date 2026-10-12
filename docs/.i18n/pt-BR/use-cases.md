<!-- source: use-cases.md; sha256: 769c5e58a1b095392245d30f0377bf87b025fa28c3eeca64f9337340d43cd3d7 -->
# Casos de uso {#use-cases}

Dê a cada agente de programação um computador sob demanda para testes e builds. Os agentes continuam editando suas cópias locais enquanto o Crabbox sincroniza alterações não commitadas, executa comandos remotamente e retorna a saída e os códigos de saída. Locações remotas separadas evitam disputa por CPU, memória e portas do notebook.

Escolha Linux, macOS, Windows nativo ou WSL2 quando o comportamento da plataforma importar. Reutilize uma máquina preparada para execuções repetidas e abra um desktop ou editor no navegador para inspecionar resultados. O [guia de desktop, VNC, navegador, Code e portal](features/interactive-desktop-vnc.md) explica o acesso interativo e as transferências de controle.

Comece pela carga de trabalho, não pelo fornecedor:

```sh
crabbox providers recommend
crabbox providers recommend fast-feedback
crabbox providers recommend agent-sandbox --json
```

Sem caso de uso, `crabbox providers recommend` lista todos os nomes disponíveis. As recomendações usam metadados de capacidades versionados no repositório. Não consultam provedores, verificam credenciais ou cotas, medem inicialização nem certificam limites de segurança.

## Acelerar testes e builds {#speed-up-test-and-build-loops}

Use quando um comando for lento, consumir muita memória ou atrapalhar o trabalho local.

```sh
crabbox providers recommend fast-feedback
crabbox providers recommend linux-vm
crabbox run --provider hetzner -- pnpm test
```

Prefira `fast-feedback` para ambientes locais, caches reutilizáveis e ciclos rápidos. Prefira `linux-vm` para tirar a execução da estação ou quando precisar de VM completa e semântica SSH.

## Dar um ambiente descartável a um agente {#give-a-coding-agent-a-disposable-environment}

Use uma locação SSH remota ou sandbox delegado para executar comandos fora da estação. Um sandbox local separa o espaço de trabalho, mas continua usando os recursos do notebook. O Crabbox cuida da execução e limpeza; o agente ou harness chamador cuida da orquestração.

Para agentes independentes, crie uma locação a partir de cada cópia de trabalho:

```sh
crabbox warmup --provider hetzner --class standard --slug agent-tests
crabbox run --id agent-tests -- pnpm test
crabbox stop agent-tests
```

Use um slug diferente para cada locação. Locações preparadas preservam a configuração entre comandos; revise a configuração e instale ferramentas e dependências antes de testar.

```sh
crabbox providers recommend agent-sandbox
crabbox providers recommend disposable-execution
crabbox providers recommend isolated-execution
```

São três perguntas diferentes:

- `agent-sandbox` prioriza provedores feitos para agentes e ambientes de desenvolvimento;
- `disposable-execution` exige um ambiente temporário que possa ser limpo;
- `isolated-execution` prioriza limites de sandbox locais e delegados.

A última recomendação orienta o roteamento, não certifica segurança. O Crabbox pressupõe operadores, configuração e cargas confiáveis. Uma máquina descartável não torna código hostil seguro. Leia o modelo de confiança e rede antes de encaminhar segredos.

## Validar Linux, Windows, WSL2 ou macOS {#validate-linux-windows-wsl2-or-macos}

Use recomendações de plataforma quando o comportamento depender do sistema operacional:

```sh
crabbox providers recommend linux-vm
crabbox providers recommend windows
crabbox providers recommend macos

crabbox run --provider aws --target windows --windows-mode wsl2 -- uname -a
```

Windows nativo executa PowerShell e ferramentas Windows. WSL2 executa o contrato POSIX dentro de uma VM Windows compatível com virtualização aninhada. Os destinos macOS podem ser VMs Apple Silicon locais ou Macs na nuvem, conforme o provedor.

## Testar um navegador ou desktop visível {#test-a-browser-or-visible-desktop}

Use uma locação SSH com desktop quando precisar de navegador, capturas, entrada ou transferência para um colega:

```sh
crabbox providers recommend desktop
crabbox warmup --provider azure --target windows --windows-mode normal --desktop --slug ui-check
crabbox webvnc --id ui-check --open --take-control
```

WebVNC permite que colegas autorizados observem e assumam o controle. VNC nativo usa um túnel SSH; `crabbox code` abre code-server em locações Linux compatíveis. O portal mostra locações, saúde das pontes e evidências preservadas. Mantenha a ponte local ativa enquanto usa o navegador. Consulte os pré-requisitos no [guia de acesso combinado](features/interactive-desktop-vnc.md).

Combine com roteamento de evidências se o resultado precisar sobreviver à máquina:

```sh
crabbox providers recommend run-evidence
crabbox providers recommend failure-diagnostics
```

## Reutilizar estado preparado {#reuse-warm-state}

Use uma máquina preparada quando repetir a instalação custar mais que preservar ou restaurar o estado:

```sh
crabbox providers recommend warm-start
crabbox providers recommend versioned-workspace

crabbox warmup --slug warm-tests
crabbox run --id warm-tests -- pnpm test:changed
```

`warm-start` considera ambiente local, volumes de cache, sessões mantidas, pausa/retomada e estado do espaço de trabalho. Não promete o mesmo mecanismo de snapshot ou pool preparado em todos os provedores.

## Distribuir experimentos paralelos {#fan-out-parallel-experiments}

Use espaços de trabalho com fork para branches paralelas, várias tentativas de agentes ou partes de testes iniciadas com estado preparado:

```sh
crabbox providers recommend fanout-testing
crabbox checkpoint fork <checkpoint-id> --count 4
```

É roteamento neutro de checkpoints e forks, não garantia de fork da memória em execução de uma microVM.

## Executar cargas GPU {#run-gpu-workloads}

Use a recomendação GPU para testes de modelos, builds CUDA, renderização e outras tarefas aceleradas:

```sh
crabbox providers recommend gpu
crabbox run --provider runpod -- nvidia-smi
```

Provedores GPU com SSH costumam ser melhores para depuração interativa. Provedores delegados costumam ser melhores quando o provedor deve executar e coletar resultados.

## Executar localmente sem credenciais de nuvem {#run-locally-without-cloud-credentials}

Use contêiner, VM ou sandbox de políticas local para um teste rápido na estação:

```sh
crabbox providers recommend local
crabbox providers recommend offline-validation
crabbox run --provider local-container -- pnpm test
```

Local não significa um único modelo de isolamento. Contêineres compatíveis com Docker, Apple VM, Multipass VM, Windows Sandbox e ambientes de políticas têm contratos diferentes de acesso ao host, persistência e limpeza. Leia a página do provedor antes de executar código desconhecido.

## Usar infraestrutura existente {#use-infrastructure-you-already-own}

O Crabbox pode usar um host SSH permanente, virtualização privada ou conta direta de nuvem sem coordenador compartilhado:

```sh
crabbox providers recommend byo-ssh
crabbox providers recommend self-hosted
crabbox providers recommend linux-vm
```

Use `ssh` quando o Crabbox não deva controlar o ciclo de vida do host. Use Proxmox, XCP-ng, Incus, KubeVirt ou Firecracker quando ele deva criar e limpar o ambiente.

## Compartilhar uma frota de equipe com governança {#share-a-governed-team-fleet}

Use o coordenador para compartilhar credenciais, propriedade das locações, limpeza, histórico, limites de locações ativas e tetos mensais:

```sh
crabbox providers recommend team-cloud
crabbox login --url https://broker.example.com
crabbox usage --scope user
```

O coordenador é um plano de controle. A CLI ainda se conecta diretamente aos executores SSH normais para sincronizar e executar, registrando evidências no coordenador. Acesse `/portal` para ver o inventário e os links de desktops, editores e detalhes. Registros de provedores diretos acrescentam visibilidade sem transferir credenciais nem a responsabilidade de limpeza.

## Precisa de uma rota mais específica? {#need-a-more-specific-route}

A CLI também recomenda provedores para artefatos, evidências CI, interpretadores, diagnóstico, conexão MCP, contenção de rede, pausa/retomada, URLs de prévia, conectividade, desenvolvimento remoto, observabilidade, sessões reutilizáveis, testes web e execução de Worker/módulos.

Veja [`crabbox providers recommend`](commands/providers.md#providers-recommend) para a lista atual, aliases, filtros, pontuações e ressalvas.

## Documentação relacionada {#related-docs}

- [Seleção de provedores](features/provider-selection.md)
- [Referência de provedores](providers/README.md)
- [Execução aninhada](features/nested-execution.md)
- [Preços e custos](pricing.md)
