<!-- source: concepts.md; sha256: 27d2835bb5b9d8ffdb9a39d681b5f2c01215e10c33a0199b1b1e2dd3347e29c8 -->
# 概念 {#concepts}

Crabbox 为智能体和人类提供按需计算机。本地编辑；**租约**预留机器，**运行**同步工作树并执行命令。独立机器支持并行测试与构建，避免竞争笔记本的 CPU、内存和端口。

提供商支持 Linux、macOS、原生 Windows 和 WSL2。预热租约保留环境，Desktop、Browser 和 Code 提供交互入口，协调器门户显示共享租约与证据，见[访问指南](features/interactive-desktop-vnc.md)。

本词汇表用于 CLI、协调器和文档；同义词中首选形式用**粗体**标出。

## 计算资源术语 {#compute-vocabulary}

**Lease（租约）**：对 Crabbox 创建或解析的执行器的限时预留。有规范 ID（`cbx_...`）、slug、空闲超时、TTL 和状态（`active`、`released`、`expired`、`failed`）。它是计费与清理单位。

**Runner（执行器）**：提供商创建、初始化后执行一次或多次任务的机器。Hetzner、AWS EC2 和静态 SSH 主机均按后端报告视为执行器。

**Box** / **testbox**：执行器的非正式称呼。新文档优先使用 runner，面向租约产品体验时可用 box，如预热机器。

**Pool**：用户、组织或全机群可见的活跃执行器集合，见 `crabbox list` 和管理端点 `GET /v1/pool`。

**Pond**：共享提供商标签 `pond=<name>` 的租约群，无中央集群对象，至少一个活跃成员存在时才存在。用 `crabbox pond peers`、`crabbox pond
connect`/`disconnect`、`crabbox pond release` 操作，见 [Pond](features/pond.md)。

**Lease ID**：`cbx_` 加 12 位小写十六进制，共 16 字符，如 `cbx_abcdef123456`。用于标签、日志、声明和 API，见[标识符](features/identifiers.md)。

**Slug**：友好名称。自动名称组合词对和八位指纹，如 `swift-crab-1f3a9c2b`，冲突时加四位后缀。可用 `--slug swift-crab` 自定义。`--slug` 归一化到 `[a-z0-9-]`，最长 41 字符。多数命令用 `--id` 接受 ID 或 slug。

**Run（运行）**：一次 `crabbox run`，有 `run_` ID、租约、命令和退出码。代理运行有持久历史；直接运行在本地计时、证明和故障产物中使用 ID。见[历史与日志](features/history-logs.md)。

**Workspace（工作区）**：租约或委托沙箱中的工作副本与运行状态，包括同步文件、依赖、提供商工作目录、输出和可选桌面/浏览器状态。用户体验超出单一执行器时首选此词。

## 角色 {#roles}

**CLI**：本地 Go 程序 `crabbox`，负责配置、同步、执行、输出和租约 SSH 密钥，见[架构](architecture.md)。

**Broker** / **coordinator**：经认证的共享控制平面，负责凭据、租约状态、到期、清理调度、历史、用量与费用上限。运行于 Cloudflare Workers 的 Fleet Durable Object 或 Node.js/PostgreSQL/pg-boss。强调服务时称 coordinator，强调 CLI 与提供商信任边界时称 broker。

**Provider**：获取、解析、列出和释放执行器的适配器。包括 AWS、Azure、Google Cloud、Hetzner；自托管/本地 Proxmox、Parallels、Local Container、静态 SSH；SSH 开发沙箱 Daytona、KubeVirt、External、Namespace Devbox、Semaphore、Sprites、exe.dev、RunPod、Railway；委托执行 Blacksmith Testbox、E2B、Modal、Islo、Tensorlake、[Upstash Box](https://upstash.com/docs/box/overall/quickstart)、Cloudflare、Azure Dynamic Sessions、W&B。见[提供商参考](providers/README.md)。

**Backend**：提供商的 Go 接口：`SSHLeaseBackend` 提供真实 SSH 目标，`DelegatedRunBackend` 由提供商执行命令，见[后端](provider-backends.md)。

**Operator**：有代理端管理权限的人，运行 `crabbox admin` 及镜像构建/提升流程。

**Agent**：通过 CLI 调用的 LLM 进程。Crabbox 提供执行与证据，智能体或框架负责提示、决策与调度。配置及负载必须可信。

## 模式 {#modes}

**Brokered mode** / **coordinator mode**：协调器负责创建、状态和清理，CLI 直接 SSH/rsync/执行。凭据不离开协调器。AWS、Azure、Daytona、Google Cloud、Hetzner 支持，需通过 `CRABBOX_COORDINATOR` 或 `config set-broker` 配置 URL。

**Direct mode** / **direct-provider mode**：CLI 直连提供商 API，无代理、中央历史或费用上限。上述五种以外的提供商仅支持此模式；上述五种无 URL 时也使用此模式。

**Static mode**：`provider: ssh`，主机属于操作者，Crabbox 不创建也不删除，始终直接模式。

**Delegated mode**：Blacksmith、E2B、Modal、Islo 等自行执行并回传输出。拒绝 Crabbox 本地同步参数 `--sync-only`、`--checksum`，计时显示 `sync=delegated`。

## 命令 {#commands}

**warmup**：获取并准备租约，`--keep` 默认 true，尚不执行命令。

**run**：获取或复用、同步、执行、输出及返回退出码。新租约默认释放，复用租约保留；由 `--keep` 和 `--stop-after` 控制。

**stop** / **release**：按提供商协议结束租约。通常删除临时资源，静态主机和特定保留资源除外。

**cleanup**：验证精确所有权后清理符合条件的直接资源。标签、名称或 ID 单独不能授权删除。

**reuse**：用 `--id` 或 slug 选择现有租约，而非为 `run` 创建新的。`warmup` 创建租约；`--lease-id` 在支持的提供商上是幂等创建协议，不是通用复用参数。

**reclaim**：将声明移到另一个工作副本，允许仓库 B 复用仓库 A 的租约（`--reclaim`）。默认租约绑定仓库。

**hydrate**：通过仓库 GitHub Actions 工作流准备依赖。通常执行支持的设置步骤；需要完整语义时，`--github-runner` 注册自托管运行器并触发真实任务。见 [Actions 环境准备](features/actions-hydration.md)。

## 状态 {#state}

**Idle timeout**：无心跳多久后代理释放租约；默认 1800 秒（30 分钟），每次心跳或 touch 重置。

**TTL**：绝对最长寿命，默认 5400 秒（90 分钟），上限 86400 秒（24 小时），心跳不能延长。`expiresAt = min(createdAt + ttl, lastTouchedAt + idleTimeout)`。

**Heartbeat**：长任务发送 `POST /v1/leases/{id}/heartbeat`，更新 `lastTouchedAt`、可携遥测并按明确请求更新空闲超时。达到 `expiresAt` 后拒绝，清理流程接管。

**Touch**：更新状态与空闲的底层术语。提供商 `Touch` 更新直接状态，代理对应心跳。

**Reserved cost**：创建前预留的最坏 TTL 成本（`hourlyRate × ttl`），占用月度额度直到释放。

**Estimated cost**：按小时价和 `active` 时长计算的估计费用，由 `crabbox usage` 展示，见[费用与用量](features/cost-usage.md)。

## 同步 {#sync}

**Manifest**：以 NUL 分隔的路径清单，由 `git ls-files --cached` 和 `git ls-files --others --exclude-standard` 生成；删除另记。

**Fingerprint**：提交、未提交文件元数据与清单的哈希，本地与远端相同则跳过 rsync。

**Git seeding**：首次同步可选步骤，从配置的 origin/base ref 初始化远端 Git，以便差异可用。

**Base ref**：初始化与环境准备的 Git 基准，仓库用 `sync.baseRef` 配置。

**Sanity check**：同步后检测大量受控文件删除、清单缺失和可疑结果，见[同步](features/sync.md)。

## 能力 {#capabilities}

**Desktop**：`--desktop` 增加可见会话：受管 Linux/新本地容器使用可调整大小的 TigerVNC + XFCE，或 Wayland/GNOME。`crabbox
vnc`、`crabbox webvnc` 和多数 `--browser` UI 运行需要它。

**Browser**：`--browser` 安装或发现浏览器并导出路径。可无头运行，加 `--desktop` 可显示。引擎由提供商/镜像决定，不提供已登录配置。

**Code**：`--code` 安装监听回环 8080 的 code-server，供 `crabbox code` 和门户 `/code/` 桥接使用，仅受管 Linux。

**Tailscale**：让受管 Linux 加入指定 tailnet，无需公网 IP 即可访问。不同于选择 CLI 网络平面的 `--network tailscale`，见[能力](features/capabilities.md)。

## 协调器运行时 {#coordinator-runtime}

**FleetCoordinator**：运行时无关的 TypeScript 控制逻辑；两种部署共享路由、认证、适配器、记录、费用、清理、门户及桥接协议。

**Durable Object**：存储状态并串行决策的 Cloudflare 原语，使用一个 Fleet 对象 `idFromName("default")`。

**PostgreSQL runtime**：Node.js 可移植部署，`crabbox` schema 保存键值，pg-boss 使用 `crabbox_jobs` 执行精确维护与周期对账。

**Alarm**：未来清理期限，由 Durable Object 或 pg-boss 处理空闲、TTL、重试和提供商维护。

**Portal**：协调器 `/portal/...` 下服务端渲染且需认证的 UI，显示租约、日志、事件及 VNC/Code，见[浏览器门户](features/portal.md)。

**Bridge**：协调器连接本地 CLI 中继。WebVNC/Code 中继通过 SSH 隧道连接回环 VNC 5900、code-server 8080，浏览器连接时必须持续运行。出口使用单独带票据中继。协调器认证访问，不直接建立 SSH。

## 身份 {#identity}

**Owner**：租约所有者。GitHub 登录绑定 `github:<numeric-id>`；共享自动化按已验证身份路由使用配置所有者或 Git 邮件，见[运行安全](security.md#authentication)。

**Org**：GitHub 风格组织命名空间，从签名 token 或 `CRABBOX_ORG` 解析，控制用量范围和多租户额度。

**Allowed org**：签发登录 token 前必须具备的组织成员身份，按 Worker 配置。

**Admin token**：用于 `GET /v1/pool`、管理租约和全机群列表的独立权限 token，比共享自动化 token 更严格保管。

**Cloudflare Access**：Worker 前的可选保护层，仅信任验证后的 `cf-access-jwt-assertion` 而非原始客户端头。

## 存储 {#storage}

**State directory**：本地声明、历史、检查点。明确设置 `XDG_STATE_HOME` 时用 `$XDG_STATE_HOME/crabbox`，密钥和主机信任也切换。否则状态用 `<os-user-config-dir>/crabbox/state`，密钥用 `<os-user-config-dir>/crabbox/testboxes`。更改不迁移或搜索旧状态，见 [SSH 密钥](features/ssh-keys.md)。

**Claim**：状态目录内将租约绑定工作副本的 JSON 文件。`crabbox run --id` 用它解析 slug，并拒绝无 `--reclaim` 的跨仓库复用。

**Workdir** / **work root**：同步目的地。Linux 默认 `/work/crabbox`，Windows 正常模式为 `C:\crabbox`，macOS 及其他目标由提供商决定。

## 证据与产物 {#evidence-and-artifacts}

**Run history**：代理的历史记录（`crabbox history`、`logs`、`events`、`results`），不同于租约当前状态，见[历史和日志](features/history-logs.md)。

**Test results**：记录运行的 JUnit 结果，由 `crabbox results <run-id>` 展示套件、测试、失败、错误和跳过，见[测试结果](features/test-results.md)。

**Telemetry**：运行前后轻量 Linux 健康采样，包含负载、内存、磁盘、运行时间；是资源快照，不是产品分析，见[遥测](features/telemetry.md)。

**Artifacts**：`crabbox artifacts` 生成截图、视频、GIF、日志、状态、元数据，可发布并评论到 PR，见[产物](features/artifacts.md)。

**Egress**：`crabbox egress` 经 WebSocket 将租约出站网络通过操作者机器转发，见[出口](features/egress.md)。

**Checkpoint**：可恢复或 fork 的 VM/工作区状态，ID 为 `chk_...`，含归档、配方、原生快照/镜像，见[检查点](features/checkpoints.md)。

**Cache volume**：由仓库/运行时/平台决定键、挂在绝对路径的持久缓存，仅用于加速。它不是可 fork 的场景，工作树仍为真源，见[缓存](features/cache.md)。

**Capsule**：从 Actions 以 `crabbox capsule from-actions` 捕获并用 `crabbox capsule
replay` 重放的便携故障包，见[胶囊](features/capsules.md)。

## 文档 {#documentation}

**Source map**：将行为映射至实现，见[源码地图](source-map.md)。

**Feature page**：`docs/features/<name>.md` 定义能力概念，命令和提供商文档引用它。

**Command page**：`docs/commands/<name>.md` 描述参数、行为、退出码，与 `--help` 同步。

**Provider page**：`docs/providers/<name>.md` 描述目标、配置、环境变量、同步与预期失败。

相关文档：

- [工作原理](how-it-works.md)
- [架构](architecture.md)
- [CLI](cli.md)
- [配置](features/configuration.md)
- [提供商后端](provider-backends.md)
