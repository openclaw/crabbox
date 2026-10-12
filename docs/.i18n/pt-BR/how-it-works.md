<!-- source: how-it-works.md; sha256: 2e0288463ff74a878d06a6226e5924a2ce5426c54796c463626ae14219ab37b5 -->
# Como o Crabbox funciona {#how-crabbox-works}

Para entender o fluxo completo ou identificar o componente responsável antes de mudar o código.

## Resumo {#tl-dr}

O Crabbox oferece máquinas sob demanda. Edite localmente enquanto testes e builds rodam em Linux, macOS, Windows nativo ou WSL2. Em uma frota compartilhada:

- **CLI** (`cmd/crabbox`, `internal/cli`): alocação, sincronização de alterações, execução, saída e limpeza.
- **Coordenador** (`worker/src`): credenciais, estado, expiração, limpeza, uso e custos. Roda em Cloudflare Workers/Durable Object ou Node.js/PostgreSQL/pg-boss.

Executores não guardam segredos do intermediário. Preparação, dependências e trabalho pertencem ao repositório. O coordenador preserva registros, logs e telemetria; baixe ou publique capturas e artefatos antes de liberar. Provedores diretos dispensam coordenador.

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

A CLI fala HTTPS com o coordenador e depois **diretamente** SSH/rsync com o executor. Ele não chama o coordenador para executar comandos comuns; esse caminho é unidirecional. O coordenador gerencia locações, não transfere a cópia nem executa comandos. A CLI envia metadados, prévias, logs e resultados separadamente; pontes interativas também passam por ele. O [serviço privado AWS](features/aws-private-workspaces.md) é uma API SSM-only separada, sem plano SSH.

Interações longas podem usar o WebSocket autenticado `/v1/control` para eventos e heartbeats, reduzindo consultas em conexões lentas. HTTPS continua o armazenamento canônico e a alternativa compatível com versões antigas.

## Responsabilidades {#ownership}

| Camada | Responsável por |
| :--- | :--- |
| **CLI** | Configuração/flags, chaves SSH por locação, prontidão, Git/rsync, fingerprints/integridade, comando/saída, WebSocket com alternativa HTTP, liberação |
| **Broker** | Autenticação/identidade, estado serializado, credenciais, criação/exclusão, expiração, consultas, registros/logs/eventos/telemetria, uso/limites |
| **Provider** | Computação bruta: adaptadores criam/excluem Hetzner, AWS EC2, Azure VM, GCP e outros |
| **Runner** | Nada durável: Linux com SSH/Git/rsync/raiz de trabalho, preparação específica em outros destinos, hosts SSH existentes; runtimes do projeto vêm do repositório |

## O que `crabbox run` faz {#what-crabbox-run-does}

Cinco fases no caminho SSH intermediado. Provedores diretos controlam o ciclo localmente; delegados controlam o transporte da execução.

**1. Planejar.** Precedência flags → ambiente → repositório → usuário → padrões. Gera ID temporário (`cbx_` + 12 dígitos hexadecimais) e chave em `<user-config>/crabbox/testboxes/<lease>/id_ed25519` (RSA no Windows AWS/Azure).

**2. Alocar.** Envia classe, provedor, destino, TTL, inatividade, slug, capacidades e chave pública para `POST /v1/leases`. O coordenador autentica, serializa mudanças, verifica limites ativos/mensais, consulta preços, reserva custo máximo do TTL e provisiona com alternativas de região/mercado. Retorna host, acesso SSH, raiz, expiração, ID final e slug. Se o ID mudar, a CLI renomeia o diretório de chaves.

**3. Sincronizar.** Espera SSH e sonda, prepara Git de origin/ref base. Fingerprints iguais evitam transferência. Senão usa rsync POSIX (`/work/crabbox/<lease>/<repo>` no Linux) ou tar de manifesto no Windows nativo, seguido de verificações e histórico base onde houver suporte.

**4. Executar.** Heartbeats em segundo plano, comando SSH e saída stdout/stderr. Quando intermediado, registra eventos por fase.

**5. Liberar.** Locações novas são liberadas salvo `--keep`; reutilizações explícitas com `--id` são mantidas. `--stop-after` pode substituir a regra. O provedor limpa e libera custo reservado. Se uma máquina nova não mantida falhar antes do SSH, `crabbox run` a substitui uma vez. Não duplica comandos em locações mantidas ou reutilizadas explicitamente.

## Máquinas preparadas e reutilização {#warm-boxes-and-reuse}

`crabbox warmup` usa a mesma criação, mas conserva a máquina sem comando. Reutilize por slug ou ID:

```sh
crabbox warmup --profile project-check --slug blue-lobster
crabbox run --id blue-lobster -- pnpm test:changed
crabbox ssh --id blue-lobster
crabbox stop blue-lobster
```

Cada locação tem ID `cbx_…` e slug. Os automáticos incluem oito dígitos de fingerprint, como `blue-lobster-1f3a9c2b`. `--slug` personaliza; `--id` aceita ambos. Heartbeats atualizam `lastTouchedAt` e expiração ociosa. O agendador durável libera locações abandonadas.

## Provedor intermediado ou direto {#brokered-vs-direct-provider}

O caminho **intermediado** é o usual:

```text
CLI -> coordinator (Cloudflare or Node/PostgreSQL) -> provider API
CLI -> runner over SSH/rsync
```

Compartilha infraestrutura sem segredos locais, centralizando limpeza, uso e custos. Ativo para `hetzner`, `aws`, `azure`, `daytona`, `gcp` apenas com URL em `CRABBOX_COORDINATOR` ou `config set-broker`.

Sem URL, usa o caminho **direto**:

```text
CLI -> provider API
CLI -> runner over SSH/rsync
```

Exige credenciais locais, como cadeia AWS SDK ou token Hetzner. Não há histórico central nem heartbeat intermediado. Serve para diagnosticar o intermediário e para adaptadores apenas diretos, como sandboxes, SSH estático e contêineres locais.

`broker.mode: registered` continua direto, mas replica metadados por proprietário e heartbeats. Acrescenta inventário, compartilhamento opcional e WebVNC de saída sem transferir credenciais nem autoridade de limpeza. Liberar remove só metadados por padrão; adaptador de saída explicitamente vinculado pode apagar o workspace após confirmação. Registros não contam nas cotas, custos, imagens, pools ou manutenção gerenciados.

SSH estático (`provider: ssh`) ignora o intermediário mesmo com URL. macOS e WSL2 usam rsync POSIX, Windows nativo PowerShell/tar.

## Autenticação e identidade {#auth-and-identity}

Usuários usam tokens GitHub assinados; automação confiável usa bearer compartilhado. Cloudflare Access é opcional; Node só confia em cabeçalhos de identidade dos CIDRs de proxy configurados.

```text
Authorization: Bearer <token>
X-Crabbox-Owner: <email>
X-Crabbox-Org: <org>
```

Em `crabbox login`, o proprietário é o imutável `github:<numeric-id>`. No modo compartilhado vem de `CRABBOX_OWNER`, ambiente de email Git ou `git config user.email`; `CRABBOX_ORG` define organização. Cabeçalhos Access brutos são ignorados; só email JWT verificado é aceito. Veja [Segurança](security.md).

## Modelo de sincronização {#sync-model}

Prioriza o local e não exige cópia limpa:

- prepara Git de origin/base;
- sobrepõe alterações por rsync, com checksum opcional;
- evita transferências vazias por fingerprints;
- exclui diretórios pesados;
- protege contra exclusões em massa suspeitas;
- prepara histórico base para testes de mudanças.

Pessoas e agentes editam localmente e executam remotamente. `crabbox sync-plan` oferece prévia somente leitura.

## Custo e uso {#cost-and-usage}

Dois custos por locação:

```text
estimatedUSD   elapsed runtime cost so far
reservedUSD    worst-case TTL cost reserved before provisioning
```

Prioridade do preço por hora:

```text
1. CRABBOX_COST_RATES_JSON   explicit override
2. provider live pricing     e.g. EC2 spot history, Hetzner server-type prices
3. built-in fallback rates   last-resort defaults
```

Hetzner converte para USD com `CRABBOX_EUR_TO_USD`. `CRABBOX_MAX_*` limita locações e USD mensais globalmente e por proprietário/organização; exceder retorna HTTP 429. `crabbox usage` consulta `GET /v1/usage` e agrupa por usuário, organização, provedor e tipo.

## Falha e limpeza {#failure-and-cleanup}

Quedas, SSH desconectado, cloud-init falho, sucesso parcial, repetições e máquinas que sobrevivem ao processo são previstos:

- decisões serializadas;
- criação idempotente quando possível;
- rótulos Crabbox;
- liberação repetível e segura;
- alarmes Durable Object ou pg-boss com reconciliação;
- limpeza direta conservadora;
- limpeza intermediada pertencente ao intermediário.

Com coordenador configurado, `crabbox cleanup` recusa varrer recursos: essa responsabilidade é do coordenador.

## Para continuar {#where-to-go-next}

- [Arquitetura](architecture.md): componentes, API, estado, falhas.
- [Orquestrador](orchestrator.md): responsabilidades, ciclo, limpeza, custos.
- [CLI](cli.md): comandos, configuração, saída, códigos.
- [Recursos](features/README.md): por área.
- [Comandos](commands/README.md): por comando.
- [Infraestrutura](infrastructure.md): runtimes, entrada, segredos, DNS, configuração.
- [Segurança](security.md): autenticação, SSH, limpeza, confiança.
