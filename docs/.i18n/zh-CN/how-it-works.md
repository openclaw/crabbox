<!-- source: how-it-works.md; sha256: 2e0288463ff74a878d06a6226e5924a2ce5426c54796c463626ae14219ab37b5 -->
# Crabbox 工作原理 {#how-crabbox-works}

适合初次了解 Crabbox、需要端到端模型，或修改代码前确认行为归属的读者。

## 概览 {#tl-dr}

Crabbox 为智能体和人类提供按需计算机。在本地编辑，让独立机器执行测试与构建，目标可为 Linux、macOS、原生 Windows 和 WSL2。共享机群包含两部分：

- **CLI**（`cmd/crabbox`、`internal/cli`）提供租用、同步未提交工作树、执行、输出流和清理。
- **协调器**（`worker/src`）持有提供商凭据，负责租约状态、到期、清理、用量和成本限制，可运行在 Cloudflare Workers + Durable Object 或 Node.js + PostgreSQL + pg-boss。

受管执行器不持有代理机密。仓库负责初始化、依赖和工作负载。协调器保留运行记录、日志与遥测；释放机器前需下载或发布截图等产物。直接提供商无需协调器。

## 组成 {#the-pieces}

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

普通 CLI 租约先通过 HTTPS 联系协调器，再通过 SSH 和 rsync **直接**连接执行器。执行器不为普通命令回调协调器，执行路径是单向的。协调器管理租约，不传输工作树或执行命令。CLI 另行上传运行元数据、输出预览、日志和结果；交互门户桥接也经过协调器。[私有 AWS 工作区服务](features/aws-private-workspaces.md)是独立的 SSM-only API 路径，没有 SSH 数据平面。

长时交互可使用经过认证的 `/v1/control` WebSocket，传输事件订阅和心跳，减少高延迟网络轮询。HTTPS 端点仍是权威存储与兼容回退，旧 CLI 与协调器可互通。

## 责任划分 {#ownership}

| 层 | 负责内容 |
| :--- | :--- |
| **CLI** | 配置和参数、每租约 SSH 密钥、SSH 就绪、Git 初始化与 rsync、指纹和健全性检查、远程命令与输出、控制 WebSocket 与 HTTP 回退、释放 |
| **Broker** | 身份认证、串行租约状态、提供商凭据、创建/删除、到期、机群查询、运行/日志/事件/遥测、用量和费用上限 |
| **Provider** | 原始计算资源；适配器创建和删除 Hetzner 服务器、AWS EC2、Azure VM、GCP 实例等 |
| **Runner** | 代理机器不保存持久状态；Linux 准备 SSH、Git、rsync 和工作根目录，其他目标按提供商初始化；静态目标为已有 SSH 主机；项目运行时由仓库安装 |

## `crabbox run` 的流程 {#what-crabbox-run-does}

普通协调器 SSH 运行有五阶段。直接提供商在本地管理生命周期，委托提供商拥有执行传输。

**1. 规划。**配置优先级为参数 → 环境 → 仓库 → 用户 → 默认。生成临时租约 ID（`cbx_` 加 12 位十六进制）及 `<user-config>/crabbox/testboxes/<lease>/id_ed25519` 下的独立 SSH 密钥；AWS/Azure Windows 使用 RSA。

**2. 租用。**向代理 `POST /v1/leases`，发送规格、提供商、目标、TTL、空闲超时、slug、能力和公钥。协调器认证、串行变更、执行活跃数与月度上限、获取实时价格、预留最坏 TTL 成本，按区域和市场回退创建机器，返回连接信息、工作目录、到期时间、最终 ID 与 slug。最终 ID 不同时，CLI 重命名本地密钥目录。

**3. 同步。**等待 SSH 探针，通过配置的 origin 与基准引用初始化 Git。指纹一致则跳过；否则 POSIX 用 rsync 将未提交树传到工作根目录（Linux 为 `/work/crabbox/<lease>/<repo>`），原生 Windows 用清单 tar 包。随后执行健全性检查并在支持时准备基准历史。

**4. 执行。**后台发送心跳，通过 SSH 执行并传回 stdout/stderr。代理模式还镜像分阶段运行事件。

**5. 释放。**新租约默认释放，设置 `--keep` 则保留；显式 `--id` 复用的租约保留。`--stop-after` 可覆盖。提供商执行清理协议，代理释放预留费用。新建且不保留的机器若在 SSH 就绪前初始化失败，`crabbox run` 最多替换一次；保留或显式复用的租约不会重复命令。

## 预热与复用 {#warm-boxes-and-reuse}

`crabbox warmup` 使用相同创建流程但不执行命令，保留机器待用。通过 slug 或 ID 明确复用：

```sh
crabbox warmup --profile project-check --slug blue-lobster
crabbox run --id blue-lobster -- pnpm test:changed
crabbox ssh --id blue-lobster
crabbox stop blue-lobster
```

每个租约都有友好 slug 和规范 `cbx_…` ID。自动 slug 带八位指纹，如 `blue-lobster-1f3a9c2b`；`--slug` 自定义，多数命令的 `--id` 接受二者。CLI 使用时发送心跳，更新 `lastTouchedAt` 并重算空闲到期。超过空闲期限后，持久调度器释放租约。

## 代理模式与直接模式 {#brokered-vs-direct-provider}

**代理路径**是常规运行方式：

```text
CLI -> coordinator (Cloudflare or Node/PostgreSQL) -> provider API
CLI -> runner over SSH/rsync
```

适合共享基础设施，机密留在代理端，清理、用量和费用统一管理。受管云（`hetzner`、`aws`、`azure`、`daytona`、`gcp`）仅在设置代理 URL（`CRABBOX_COORDINATOR` 或 `config set-broker`）时启用。

未配置代理时使用**直接路径**：

```text
CLI -> provider API
CLI -> runner over SSH/rsync
```

需要本地提供商凭据，例如 AWS SDK 凭据链或 Hetzner token，无中央历史或代理心跳。适用于诊断代理，也是沙箱、静态 SSH、本地容器等直接适配器的唯一模式。

`broker.mode: registered` 仍直接执行，但向协调器镜像按所有者隔离的租约元数据和心跳，增加门户清单、可选共享和出站 WebVNC，不转移凭据或清理权限。释放默认仅移除元数据；显式绑定的出站适配器可在用户确认后删除工作区。注册记录不参与受管配额、费用、镜像、池或维护。

静态 SSH（`provider: ssh`）指向现有主机，即使配置代理也绕过。macOS 与 Windows WSL2 使用 POSIX rsync；原生 Windows 用 PowerShell 和 tar 同步。

## 认证与身份 {#auth-and-identity}

普通用户用签名 GitHub token，可信自动化用共享 bearer token。Cloudflare 可另加 Access；Node 仅信任配置的反向代理 CIDR 传入的身份头。CLI bearer 请求发送：

```text
Authorization: Bearer <token>
X-Crabbox-Owner: <email>
X-Crabbox-Org: <org>
```

`crabbox login` 的所有者为签名 token 的不可变 `github:<numeric-id>`。共享模式取 `CRABBOX_OWNER`、Git 邮件环境或 `git config user.email`；`CRABBOX_ORG` 指定组织。原始 Access 头被忽略，仅已验证 Access JWT 邮件可成为 bearer 所有者。完整模型见[安全](security.md)。

## 同步模型 {#sync-model}

同步以本地为准，不要求干净工作副本：

- 尽可能用 origin 与基准引用初始化 Git；
- rsync 叠加未提交文件，可选校验和；
- 指纹跳过无变化同步；
- 配置排除大目录；
- 防止可疑的大量受控文件删除；
- 为变更测试准备基准历史。

智能体和人都采用本地编辑、远程运行。先用 `crabbox sync-plan` 只读预览清单。

## 成本与用量 {#cost-and-usage}

代理为每个租约记录两种费用：

```text
estimatedUSD   elapsed runtime cost so far
reservedUSD    worst-case TTL cost reserved before provisioning
```

小时价格来源优先级：

```text
1. CRABBOX_COST_RATES_JSON   explicit override
2. provider live pricing     e.g. EC2 spot history, Hetzner server-type prices
3. built-in fallback rates   last-resort defaults
```

Hetzner 通过 `CRABBOX_EUR_TO_USD` 换算美元。`CRABBOX_MAX_*` 设置全局和每所有者/组织的活跃数与月度美元上限，超限创建返回 HTTP 429。`crabbox usage` 查询 `GET /v1/usage`，按用户、组织、提供商和机器类型分组。

## 故障与清理 {#failure-and-cleanup}

CLI 崩溃、SSH 断开、cloud-init 失败、部分创建、重试及机器存活时间超过本地进程都属正常故障模型：

- 运行时串行化机群决策；
- 尽可能幂等创建；
- 资源带 Crabbox 标签；
- 释放可安全重复调用；
- Durable Object 闹钟或 pg-boss 加定期对账使过期租约释放；
- 直接清理保守执行；
- 代理清理由代理负责。

配置协调器时，`crabbox cleanup` 拒绝清扫提供商资源，因为权限属于协调器。

## 后续阅读 {#where-to-go-next}

- [架构](architecture.md)：组件、API、状态与故障模型。
- [协调器](orchestrator.md)：职责、生命周期、清理、费用和用量。
- [CLI](cli.md)：命令、配置、输出与退出码。
- [功能](features/README.md)：按功能组织。
- [命令](commands/README.md)：按命令组织。
- [基础设施](infrastructure.md)：运行时、入口、机密、DNS、提供商设置。
- [安全](security.md)：认证、机密、SSH、清理与信任边界。
