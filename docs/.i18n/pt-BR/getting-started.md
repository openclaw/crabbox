<!-- source: getting-started.md; sha256: 6f010425dbbffe15196dfbc9cbfce66ea14783090c7babf231211bf13239fc09 -->
# Primeiros passos {#getting-started}

O Crabbox fornece computadores sob demanda a agentes e pessoas. Continue editando localmente: ele aloca uma máquina, sincroniza alterações não commitadas, executa o comando e retorna a saída e o código de saída. Execuções avulsas liberam a máquina; locações preparadas continuam disponíveis.

Testes remotos liberam CPU, memória e portas para agentes em paralelo. Escolha um provedor compatível com Linux, macOS, Windows nativo ou WSL2. Para trabalho visual, veja o [guia de desktop, VNC, navegador, Code e portal](features/interactive-desktop-vnc.md).

Este roteiro leva um repositório confiável até `crabbox run -- pnpm test`. Revise antes a configuração e os comandos de preparação. Sem conta, `--provider local-container` usa Docker ou Podman local; a [habilidade `crabbox-quickstart`](integrations/agents.md#install-through-ecosystem-skill-managers) guia todo o caminho sem credenciais.

## Etapa 1. Instalar {#step-1-install}

```sh
brew install openclaw/tap/crabbox
```

Verifique:

```sh
crabbox --version
crabbox doctor
```

`crabbox doctor` imprime uma linha por verificação. Na instalação nova, o teste neutro `git` deve mostrar `ok`; testes específicos começam após escolher o provedor. Sem seleção, mostra `no provider selected`, mantém `source=compiled_default selected=false` e ignora a prontidão das credenciais sem falhar. Antes de comandos de ciclo de vida, selecione pela configuração, `CRABBOX_PROVIDER` ou `--provider`. A escolha é validada estritamente e pode falhar sem credenciais; compare com `crabbox providers recommend`.

Sem Homebrew, baixe o arquivo de macOS, Linux ou Windows em <https://github.com/openclaw/crabbox/releases>. Os executáveis macOS são assinados e notarizados.

Mantenha `crabbox-runtime` junto do executável real da CLI. As duas arquiteturas Linux pertencem ao controlador correspondente; copiar só a CLI ou misturar versões não preserva o pacote completo.

Usuários Go podem instalar só a CLI de uma versão explícita. Escolha uma tag no [GitHub Releases](https://github.com/openclaw/crabbox/releases), suportado desde v0.44.0:

```sh
go install github.com/openclaw/crabbox/cmd/crabbox@v0.70.0
```

O módulo exige Go 1.26 e prefere go1.26.5. Use Go 1.26.5 ou superior ou seleção automática. Isso compila a CLI sem instalar `crabbox-apple-vm-helper`, pacote de runtime ou auxiliares, e não é a distribuição assinada/notarizada. Para a distribuição completa, especialmente Apple VM em Apple Silicon, use Homebrew ou o arquivo da versão.

Transferências WSL2 e Windows x64 sem WSL exigem `main` atual ou Crabbox v0.42.1 em diante. Veja a [instalação Windows](windows-install.md).

## Etapa 2. Entrar {#step-2-log-in}

Para uma frota compartilhada, use a URL do coordenador da equipe. Ele guarda credenciais e aplica limites, tetos e expiração. Provedores diretos e locais não precisam de login; veja [Escolher acesso](#choosing-an-access-path).

```sh
crabbox login --url https://broker.example.com
```

`login` abre GitHub OAuth; `--no-browser` imprime a URL para um navegador do mesmo dispositivo. O intermediário troca o código, verifica a organização e redireciona uma confirmação de uso único ao listener local antes de salvar o token assinado:

```text
logged in broker=https://broker.example.com provider=hetzner user=github:12345 org=example-org config=/Users/alice/Library/Application Support/crabbox/config.yaml
```

Depois, cada comando `crabbox` autentica automaticamente. Confira a identidade:

```sh
crabbox whoami
```

```text
user=github:12345 org=example-org auth=user broker=https://broker.example.com
```

### Escolher acesso {#choosing-an-access-path}

O acesso depende da implantação. Use a URL e a lista de organizações/equipes permitidas fornecidas pela equipe. OAuth concluído ainda pode ser rejeitado fora da lista.

Para instalações pessoais ou de terceiros:

- **Provedor direto:** credenciais locais para testes privados rápidos; limpeza e estado locais em vez de histórico central e tetos compartilhados.
- **Coordenador próprio:** Cloudflare Workers ou Node.js/PostgreSQL para credenciais centrais, limites ativos/mensais, `crabbox usage`, limpeza durável e endpoint comum.
- **Solicitar acesso:** apenas se o operador definir entrada para sua organização. Endpoint de equipe não é automaticamente público.

O modo direto pula `login`:

```sh
crabbox doctor --provider hetzner
crabbox run --provider hetzner -- pnpm test
```

Escolha um runtime para hospedar:

- **Cloudflare Workers:** estado Durable Object, alarmes, limpeza agendada e Cloudflare Access opcional.
- **Node.js/PostgreSQL:** HTTP/WebSocket, PostgreSQL 13+, tarefas pg-boss e uma réplica permanente. Trate como runtime portátil inicial e valide a implantação antes de migrar produção.

Ambos compartilham segredos, autenticação, orçamentos, API e portal. Veja [Infraestrutura](infrastructure.md#choose-a-coordinator-runtime). Login web exige app OAuth e organização/equipe permitida; automação com token compartilhado não.

CI sem navegador usa token compartilhado:

```sh
printf '%s' "$TOKEN" | crabbox login \
  --url https://broker.example.com \
  --provider aws \
  --token-stdin
```

Veja [Autenticação e administração](features/auth-admin.md).

## Etapa 3. Preparar um repositório {#step-3-onboard-a-repo}

Dentro do repositório:

```sh
crabbox init
```

`init` escreve três arquivos. Ajuste caminhos com `--config`, `--workflow` ou um ou mais `--skill`; `--force` sobrescreve existentes:

```text
.crabbox.yaml                          repo defaults (profile, class, sync, env)
.github/workflows/crabbox.yml          Actions hydration workflow (optional)
.agents/skills/crabbox/SKILL.md        agent-facing skill instructions
```

Ajuste `.crabbox.yaml` conforme necessário:

- `profile`: nome da execução; modelo `<repo>-check`.
- `class`: `tiny`, `small`, `standard`, `fast`, `large` ou `beast`; modelo `beast`.
- `sync.exclude`: diretórios que nunca devem ser enviados.
- `sync.include`: lista permitida relativa à raiz; após exclusões, **somente** esses caminhos são sincronizados.
- `env.allow`: variáveis que o comando remoto pode ler.

`--detect` identifica testes e escreve `jobs.detected`, executável com `crabbox job run detected`.

Confira a sincronização:

```sh
crabbox sync-plan
```

`sync-plan` mostra quantidade, bytes e maiores arquivos. Se aparecerem `dist/`, `.cache/` ou ativos enormes inesperados, restrinja `sync.exclude` e repita. Esse tamanho limita a primeira sincronização.

Workspaces nativos Jujutsu com backend Git funcionam com `jj` instalado. Crabbox captura edições e sincroniza exatamente a árvore `@`, incluindo merges com vários pais. `sync-plan --json` mostra a revisão; use `run --no-hydrate` com SSH comum. Workspaces Git/Jujutsu colocados usam o caminho Git. Veja [Sync Jujutsu](features/sync.md#jujutsu-workspaces) para requisitos e recusas seguras.

## Etapa 4. Preparar uma máquina {#step-4-warm-a-box}

```sh
crabbox warmup --class standard --slug first-tests
```

`warmup` aloca, provisiona, espera SSH/ferramentas e mantém a locação (`--keep` ativo por padrão). Imprime resumo, endpoint e duração:

```text
leased cbx_abcdef123456 slug=first-tests provider=hetzner server=... type=... ip=203.0.113.10 idle_timeout=30m0s expires=...
ready ssh=crabbox@203.0.113.10:2222 network=public workroot=/work/crabbox
warmup complete total=...
```

O coordenador mantém a máquina pronta. Os padrões de inatividade de 30 minutos e TTL de 90 minutos limitam sua vida. Provedores diretos têm regras próprias; sempre pare ao terminar. Consulte `crabbox providers --json` antes de escolher classes específicas.

Reutilize por `slug` ou `id` (`cbx_...`), ambos aceitos em `--id`.

## Etapa 5. Executar um comando {#step-5-run-a-command}

```sh
crabbox run --id first-tests -- pnpm test
```

O processo:

1. Verifica SSH.
2. Prepara Git remoto de origin e ref base, sobrepondo alterações por rsync. Fingerprints iguais evitam nova sincronização.
3. Executa por SSH e transmite stdout/stderr.
4. Envia heartbeats para impedir expiração durante a execução.
5. Registra `run_...` com tempos, código de saída e telemetria limitada em Linux.

O repositório instala ferramentas e dependências. Execute a preparação ou [hidrate com Actions](features/actions-hydration.md) antes dos testes.

Omita `--id` para execução avulsa:

```sh
crabbox run -- pnpm test
```

Uma nova locação é adquirida e liberada ao sair. Para iterações na mesma máquina, use `warmup` + `--id`.

## Etapa 6. Inspecionar histórico {#step-6-inspect-history}

```sh
crabbox history
crabbox events run_abcdef123456
crabbox logs run_abcdef123456
crabbox results run_abcdef123456
```

`history` lista execuções; `events` mostra locação, sincronização, comando, saída e fim em ordem; `logs` retorna saída preservada; `results` interpreta relatórios JUnit anexados.

Com portal habilitado, `/portal/runs/run_abcdef123456` mostra os mesmos dados.

## Etapa 7. Parar a locação {#step-7-stop-the-lease}

Ao terminar:

```sh
crabbox stop first-tests
```

`stop` aplica o contrato de limpeza e libera custo reservado. Máquinas descartáveis geralmente são apagadas; hosts SSH estáticos e recursos retidos seguem suas políticas. A propriedade precisa ser verificada. Claims de recuperação e recibos terminais de ID fixo podem permanecer localmente. O coordenador agenda limpeza de máquinas esquecidas.

```sh
crabbox cleanup --dry-run
```

`cleanup` varre resíduos diretos e estado local. Limpeza intermediada pertence ao alarme do coordenador.

## Variações comuns {#common-variations}

Manter uma sessão longa:

```sh
crabbox warmup --idle-timeout 4h --ttl 8h --slug swift-crab
crabbox run --id swift-crab -- pnpm test
crabbox run --id swift-crab -- pnpm bench
crabbox stop swift-crab
```

Abrir desktop:

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

Mantenha as pontes ativas enquanto usa as abas. Desktop e Code dependem do provedor e destino; Code web precisa também de coordenador e origem isolada. O [guia combinado](features/interactive-desktop-vnc.md) explica VNC, WebVNC, portal, compartilhamento e limpeza.

Abrir a cópia sincronizada no Zed Remote Projects:

```sh
crabbox run --id swift-crab --sync-only
crabbox open --editor=zed --id swift-crab
```

Cole o comando SSH em **Connect New Server**, abra a pasta indicada e mantenha `crabbox open` ativo.

Usar um Mac existente (SSH estático, sem provisionar):

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

Substituir provedor e classe por comando:

```sh
crabbox run --provider aws --class beast -- pnpm test
```

## Próximos passos {#where-to-go-next}

- [Funcionamento](how-it-works.md) — modelo mental.
- [CLI](cli.md) — comandos e códigos.
- [Comandos](commands/README.md) — referência individual.
- [Recursos](features/README.md) — por capacidade.
- [Configuração](features/configuration.md) — YAML e precedência.
- [Provedores](providers/README.md) — escolha de destino.
- [Criar provedores](features/provider-authoring.md) — extensão.
- [Solução de problemas](troubleshooting.md) — falhas nos passos.
