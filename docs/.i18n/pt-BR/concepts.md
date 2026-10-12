<!-- source: concepts.md; sha256: 27d2835bb5b9d8ffdb9a39d681b5f2c01215e10c33a0199b1b1e2dd3347e29c8 -->
# Conceitos {#concepts}

O Crabbox fornece computadores a agentes e pessoas. Você edita localmente; uma **locação** reserva a máquina e uma **execução** sincroniza e roda um comando. Máquinas separadas permitem testes paralelos sem disputar CPU, memória e portas.

Há destinos Linux, macOS, Windows nativo e WSL2. Locações preparadas preservam a configuração; Desktop, Browser, Code e portal oferecem interação e evidências. Veja o [guia de acesso](features/interactive-desktop-vnc.md).

O glossário é comum à CLI, ao coordenador e à documentação. Sinônimos preferidos estão em **negrito**.

## Vocabulário de computação {#compute-vocabulary}

**Lease (locação):** reserva temporária de executor criado/resolvido, com `cbx_...`, slug, inatividade, TTL e estado `active`, `released`, `expired`, `failed`. Unidade de custo e limpeza.

**Runner (executor):** máquina preparada pelo provedor para uma ou várias execuções, seja Hetzner, AWS EC2 ou SSH estático.

**Box** / **testbox:** sinônimo informal. Prefira runner em novos documentos, exceto linguagem de produto como máquina preparada.

**Pool:** executores ativos visíveis para usuário, organização ou frota; `crabbox list` e `GET /v1/pool` administrativo.

**Pond:** grupo espontâneo com rótulo `pond=<name>`, sem objeto central, existente enquanto houver membro ativo. `crabbox pond peers`, `crabbox pond
connect`/`disconnect`, `crabbox pond release`. Veja [Pond](features/pond.md).

**Lease ID:** `cbx_` e 12 caracteres hexadecimais minúsculos, 16 no total, como `cbx_abcdef123456`; usado em rótulos, logs, claims e APIs. [Identificadores](features/identifiers.md).

**Slug:** nome amigável com par de palavras e oito dígitos de fingerprint, como `swift-crab-1f3a9c2b`, mais quatro em colisões. `--slug swift-crab` personaliza; `--slug` normaliza em `[a-z0-9-]`, máximo 41 caracteres. `--id` aceita nome ou ID.

**Run:** chamada `crabbox run` com ID `run_`, locação, comando e código. Histórico durável com coordenador; IDs locais em tempos, provas e falhas no modo direto. [Histórico e logs](features/history-logs.md).

**Workspace:** cópia e estado: arquivos, dependências, diretório do provedor, saída e desktop/navegador opcional. Use quando o produto for além da máquina bruta.

## Papéis {#roles}

**CLI:** binário Go local `crabbox`; configuração, sincronização, execução, saída e chaves SSH. [Arquitetura](architecture.md).

**Broker** / **coordinator:** plano autenticado compartilhado, credenciais, estado, expiração, limpeza, histórico, uso e limites. Cloudflare Workers/Fleet Durable Object ou Node.js/PostgreSQL/pg-boss. Coordinator enfatiza serviço; broker, limite de confiança.

**Provider:** adaptador para adquirir, resolver, listar e liberar. Nuvens AWS, Azure, Google Cloud, Hetzner; Proxmox, Parallels, Local Container, SSH; sandboxes SSH Daytona, KubeVirt, External, Namespace Devbox, Semaphore, Sprites, exe.dev, RunPod, Railway; delegados Blacksmith Testbox, E2B, Modal, Islo, Tensorlake, [Upstash Box](https://upstash.com/docs/box/overall/quickstart), Cloudflare, Azure Dynamic Sessions, W&B. [Referência](providers/README.md).

**Backend:** interface Go `SSHLeaseBackend` para destino SSH real ou `DelegatedRunBackend` para execução do provedor. [Backends](provider-backends.md).

**Operator:** pessoa com acesso administrativo, executa `crabbox admin` e preparação/promoção de imagens.

**Agent:** processo LLM chamando a CLI. Crabbox fornece execução/evidências; agente ou harness controla prompts, decisões e orquestração. Configuração e trabalho devem ser confiáveis.

## Modos {#modes}

**Brokered mode** / **coordinator mode:** coordenador cria, mantém estado e limpa; CLI usa SSH/rsync diretamente. Segredos ficam centrais. AWS, Azure, Daytona, Google Cloud, Hetzner com `CRABBOX_COORDINATOR` ou `config set-broker`.

**Direct mode** / **direct-provider mode:** API direta sem histórico central ou limites. Único modo fora desses cinco e alternativa deles sem URL.

**Static mode:** `provider: ssh`, host do operador, sem criar/excluir, sempre direto.

**Delegated mode:** Blacksmith, E2B, Modal, Islo e outros executam e retornam saída. Rejeita `--sync-only` e `--checksum`; tempo `sync=delegated`.

## Comandos {#commands}

**warmup:** aloca e prepara, `--keep` verdadeiro por padrão; ainda sem executar comando.

**run:** aloca/reutiliza, sincroniza, executa, retorna saída/código. Novas locações são liberadas, reutilizadas mantidas; controlado por `--keep` e `--stop-after`.

**stop** / **release:** termina pelo contrato, geralmente excluindo recursos descartáveis; hosts estáticos e recursos retidos são exceções.

**cleanup:** limpa resíduos diretos elegíveis após verificar propriedade exata. Rótulo, nome ou ID sozinho nunca basta.

**reuse:** `--id`/slug escolhe locação existente para `run`. `warmup` cria; `--lease-id` é criação idempotente, não reutilização genérica.

**reclaim:** move claim entre cópias (`--reclaim`) para reutilizar em outro repositório, pois há vínculo por padrão.

**hydrate:** prepara dependências com Actions do repositório. Executa passos compatíveis; `--github-runner` registra executor próprio e dispara job real para semântica completa. [Preparação Actions](features/actions-hydration.md).

## Estado {#state}

**Idle timeout:** prazo sem heartbeat, 1800 s/30 min por padrão, reiniciado por heartbeat ou touch.

**TTL:** duração absoluta, 5400 s/90 min, máximo 86400 s/24 h; heartbeat não estende. `expiresAt = min(createdAt + ttl, lastTouchedAt + idleTimeout)`.

**Heartbeat:** `POST /v1/leases/{id}/heartbeat` atualiza `lastTouchedAt`, pode enviar telemetria e alterar inatividade explicitamente. Rejeitado ao atingir `expiresAt`; limpeza passa a controlar.

**Touch:** atualização de estado/inatividade. `Touch` do provedor é direto, heartbeat é o equivalente intermediado.

**Reserved cost:** pior custo `hourlyRate × ttl`, reservado contra teto mensal até liberação.

**Estimated cost:** preço por tempo `active`, estimativa de `crabbox usage`. [Custo e uso](features/cost-usage.md).

## Sincronização {#sync}

**Manifest:** caminhos separados por NUL de `git ls-files --cached` e `git ls-files --others --exclude-standard`; exclusões separadas.

**Fingerprint:** hash do commit, metadados alterados e manifesto. Igualdade evita rsync.

**Git seeding:** preparação inicial opcional de Git remoto a partir de origin/base, habilitando diffs remotos.

**Base ref:** referência usada na preparação/hidratação, configurada em `sync.baseRef`.

**Sanity check:** após rsync detecta exclusões em massa, entradas ausentes e resultados suspeitos. [Sync](features/sync.md).

## Capacidades {#capabilities}

**Desktop:** `--desktop` adiciona TigerVNC/XFCE redimensionável no Linux gerenciado e novos contêineres, ou Wayland/GNOME. Necessário para `crabbox
vnc`, `crabbox webvnc` e maioria dos testes visuais `--browser`.

**Browser:** `--browser` instala/descobre navegador e exporta caminho. Pode ser headless; `--desktop` dá exibição. Motor depende da imagem/provedor, sem perfil autenticado.

**Code:** `--code` instala code-server em loopback 8080 para `crabbox code` e `/code/`, só Linux gerenciado.

**Tailscale:** acesso opcional ao Linux pelo tailnet sem IP público; diferente de escolher plano via `--network tailscale`. [Capacidades](features/capabilities.md).

## Runtime do coordenador {#coordinator-runtime}

**FleetCoordinator:** lógica TypeScript comum: rotas, autenticação, adaptadores, registros, custos, limpeza, portal, pontes.

**Durable Object:** estado Cloudflare e decisões serializadas; um Fleet com `idFromName("default")`.

**PostgreSQL runtime:** Node portátil, esquema `crabbox` de chave/valor, `crabbox_jobs` do pg-boss para manutenção exata e periódica.

**Alarm:** prazo futuro de limpeza, Durable Object ou pg-boss para inatividade, TTL, repetição e manutenção.

**Portal:** UI autenticada renderizada no servidor em `/portal/...`, locações, logs, eventos e VNC/Code. [Portal](features/portal.md).

**Bridge:** ligação ao relay CLI local. WebVNC/Code mantêm túneis SSH para VNC loopback 5900/code-server 8080 e precisam ficar ativos. Egress usa relay separado com tickets. Coordenador autentica, sem abrir SSH.

## Identidade {#identity}

**Owner:** dono da locação, GitHub fixa `github:<numeric-id>`. Automação usa proprietário configurado ou email Git com roteamento verificado. [Segurança operacional](security.md#authentication).

**Org:** organização do token ou `CRABBOX_ORG`, para escopo de uso e limites multitenant.

**Allowed org:** associação exigida antes do token assinado, configurada por Worker.

**Admin token:** escopo separado para `GET /v1/pool`, rotas admin e lista global, mais protegido que token compartilhado.

**Cloudflare Access:** proteção opcional, confia em `cf-access-jwt-assertion` verificado, não cabeçalhos brutos.

## Armazenamento {#storage}

**State directory:** claims, histórico, checkpoints. `XDG_STATE_HOME` seleciona `$XDG_STATE_HOME/crabbox` também para chaves/confiança. Caso ausente, `<os-user-config-dir>/crabbox/state`, chaves em `<os-user-config-dir>/crabbox/testboxes`. Mudar não migra nem busca estado antigo. [Chaves SSH](features/ssh-keys.md).

**Claim:** JSON vinculando locação e cópia, necessário em `crabbox run --id` para slugs e bloqueio entre repositórios sem `--reclaim`.

**Workdir** / **work root:** destino de sync, `/work/crabbox` Linux, `C:\crabbox` Windows normal, demais específicos do provedor/destino.

## Evidências e artefatos {#evidence-and-artifacts}

**Run history:** histórico (`crabbox history`, `logs`, `events`, `results`), diferente do estado vivo. [Histórico](features/history-logs.md).

**Test results:** JUnit de `crabbox results <run-id>`, suítes, testes, falhas, erros, ignorados. [Resultados](features/test-results.md).

**Telemetry:** amostras Linux de carga, memória, disco e uptime por execução; não análise de produto. [Telemetria](features/telemetry.md).

**Artifacts:** pacotes de `crabbox artifacts`, capturas, vídeo, GIF, logs, estado e metadados publicáveis em PR. [Artefatos](features/artifacts.md).

**Egress:** `crabbox egress` encaminha rede de saída via máquina do operador por WebSocket. [Egress](features/egress.md).

**Checkpoint:** estado restaurável/forkável de VM ou workspace, ID `chk_...`, arquivos, receitas, snapshots/imagens nativos. [Checkpoints](features/checkpoints.md).

**Cache volume:** armazenamento persistente por repositório/runtime/plataforma em caminho absoluto, só para acelerar. Não é cenário forkável; árvore continua fonte de verdade. [Cache](features/cache.md).

**Capsule:** falha portátil capturada por `crabbox capsule from-actions` e repetida por `crabbox capsule
replay`. [Cápsulas](features/capsules.md).

## Documentação {#documentation}

**Source map:** liga comportamento à implementação. [Mapa](source-map.md).

**Feature page:** `docs/features/<name>.md`, narrativa conceitual ligada por comandos/provedores.

**Command page:** `docs/commands/<name>.md`, flags, comportamento e códigos coerentes com `--help`.

**Provider page:** `docs/providers/<name>.md`, destinos, configuração, variáveis, sync e falhas.

Documentos relacionados:

- [Funcionamento](how-it-works.md)
- [Arquitetura](architecture.md)
- [CLI](cli.md)
- [Configuração](features/configuration.md)
- [Backends](provider-backends.md)
