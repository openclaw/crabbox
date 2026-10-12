<!-- source: README.md; sha256: 008297f255e89d26769a8517dd1c425b15462564563cce18859b56b7640a570e -->
# Crabbox {#crabbox}

**Computadores sob demanda para agentes.**

## O que é o Crabbox {#what-crabbox-is}

O Crabbox fornece computadores a agentes e pessoas para testes e builds. Continue editando localmente enquanto uma máquina remota executa sua árvore de trabalho, incluindo alterações não commitadas. O Crabbox transmite a saída, retorna o código de saída e limpa a locação conforme o contrato do provedor.

Com um provedor configurado, execute o comando de testes habitual:

```sh
crabbox run -- pnpm test
```

## Um ciclo, vários tipos de máquina {#one-loop-many-kinds-of-box}

O ciclo remoto padrão:

1. **Edite localmente** na cópia existente.
2. **Aloque uma máquina** ou reutilize capacidade preparada.
3. **Sincronize** a árvore, incluindo alterações não commitadas.
4. **Execute** o comando na máquina.
5. **Receba a saída e o código de saída**.
6. **Limpe** a capacidade própria ou mantenha-a preparada.

Provedores delegados podem gerenciar execução e sincronização. Reutilização, evidências e limpeza seguem as capacidades do provedor e a política da execução.

A infraestrutura pode mudar sem transformar o fluxo em um script específico. Comece por [Casos de uso](use-cases.md) se souber a tarefa ou pela [Referência de provedores](providers/README.md) se souber o destino.

## Mais agentes, menos espera {#more-agents-less-waiting}

- **Agentes em paralelo:** separe testes e builds para evitar disputa por CPU, RAM e portas.
- **Máquinas preparadas:** reutilize ambientes e reduza o ciclo de edição e execução.
- **Multiplataforma:** Linux, macOS, Windows nativo e WSL2 a partir da mesma cópia, conforme o provedor.
- **Veja e controle:** use [VNC ou WebVNC](features/interactive-desktop-vnc.md), solicite um navegador ou use VS Code web com `--code` no Linux gerenciado. O [portal](features/portal.md) autenticado mostra locações e logs; WebVNC permite observar e controlar com a ponte local em execução.
- **Capacidade compartilhada:** um coordenador opcional guarda credenciais, compartilha locações e aplica expiração e limites de gastos nos provedores intermediados.

O [catálogo](providers/README.md) cobre nuvens, VMs locais, hosts SSH e sandboxes delegados. O [contrato de plugin externo](providers/external.md) conecta seu backend. A lista e as contagens por categoria vêm dos metadados do catálogo.

## Comece aqui {#start-here}

- [Primeiros passos](getting-started.md) — instalação e primeira execução remota.
- [Casos de uso](use-cases.md) — feedback rápido, agentes, plataformas, QA web, paralelismo e GPU.
- [Preços e custos](pricing.md) — código aberto, computação própria e limites.
- [Como o Crabbox funciona](how-it-works.md) — CLI, coordenador e executor.
- [Visão do Crabbox](../VISION.md) — escopo, não objetivos e segurança do ciclo de vida.

## Escolha o modelo operacional {#pick-an-operating-model}

| Caminho | Melhor uso | Propriedade |
| --- | --- | --- |
| Ambiente local | Verificações rápidas sem credenciais | Sua estação e ambiente |
| Nuvem direta ou SSH | Contas pessoais, hosts privados, virtualização própria | Sua conta ou infraestrutura |
| Coordenador de equipe | Credenciais, locações, limpeza, uso e limites compartilhados | Seu Cloudflare ou Node.js/PostgreSQL |
| Execução delegada | Agentes, CI, navegador e GPU gerenciados pelo provedor | Provedor ou operador escolhido |

O Crabbox usa licença MIT. Atualmente não oferece um plano de controle hospedado; computação e infraestrutura do coordenador são cobradas pelos operadores. Veja [Preços e custos](pricing.md).

## Limite de confiança {#trust-boundary}

Execute repositórios confiáveis com colegas confiáveis. O isolamento depende do ambiente. Reutilização e limpeza destrutiva exigem propriedade verificada do recurso e do registro exatos. Propriedade ambígua bloqueia a operação.

Use [Seleção de provedores](features/provider-selection.md) e leia a documentação do provedor e o [modelo de segurança](security.md). Recomendações orientam o fluxo; não certificam segurança.

## Saiba mais {#go-deeper}

- **CLI e configuração:** [CLI](cli.md), [Comandos](commands/README.md), [Configuração](features/configuration.md), [Preparação de repositórios](features/repository-onboarding.md).
- **Frota e operação:** [Arquitetura](architecture.md), [Infraestrutura](infrastructure.md), [Operações](operations.md), [Observabilidade](observability.md), [Pareamento somente leitura](features/device-pairing.md), [Segurança](security.md).
- **Execuções e evidências:** [Tarefas](features/jobs.md), [Preparação com Actions](features/actions-hydration.md), [Artefatos](features/artifacts.md), [Checkpoints](features/checkpoints.md), [Desktop interativo e VNC](features/interactive-desktop-vnc.md).
- **Limites de plataforma:** [Execução aninhada](features/nested-execution.md) distingue WSL2, mecanismos de contêiner, hosts KVM preparados e sandboxes locais.
- **Extensibilidade:** [Integrações](integrations/README.md), [Criação de provedores](features/provider-authoring.md), [Mapa do código](source-map.md).

## Sobre a documentação {#about-these-docs}

O Markdown em `docs/` é a fonte de [crabbox.sh](https://crabbox.sh/). A implementação é a referência; o [Mapa do código](source-map.md) relaciona comportamentos aos responsáveis.

Compile e valide o site localmente:

```sh
scripts/check-docs.sh
open dist/docs-site/index.html
```

Em `docs/`, use títulos de nível 1–4 e blocos com três crases. Links de títulos são verificados pelos mesmos IDs do renderizador; exemplos e comentários não reservam âncoras. Preserve IDs publicados. Markdown exclusivo do repositório mantém regras próprias inspiradas no GitHub, sem prometer compatibilidade completa com GitHub Markdown.

Veja [Traduzir a documentação](translations.md) para adicionar ou atualizar um idioma.
