<!-- source: use-cases.md; sha256: 769c5e58a1b095392245d30f0377bf87b025fa28c3eeca64f9337340d43cd3d7 -->
# 使用场景 {#use-cases}

为每个编程智能体提供按需计算机，运行测试与构建。智能体继续编辑本地工作副本，Crabbox 同步未提交的修改、远程执行命令并返回输出与退出码。独立的远程租约避免并行任务争抢笔记本的 CPU、内存和端口。

需要验证平台行为时，选择 Linux、macOS、原生 Windows 或 WSL2。重复运行可复用预热机器；需要检查结果时可打开桌面或浏览器编辑器。[桌面、VNC、浏览器、Code 和门户指南](features/interactive-desktop-vnc.md)介绍交互访问和交接。

从工作负载出发选择，而非先选厂商：

```sh
crabbox providers recommend
crabbox providers recommend fast-feedback
crabbox providers recommend agent-sandbox --json
```

不指定场景时，`crabbox providers recommend` 列出全部支持的推荐名称。推荐依据仓库中的能力元数据，不会联系提供商、验证凭据或配额、测量启动时间，也不构成安全边界认证。

## 加快测试与构建循环 {#speed-up-test-and-build-loops}

适用于在本地运行太慢、占用内存过多或干扰其他工作的仓库命令。

```sh
crabbox providers recommend fast-feedback
crabbox providers recommend linux-vm
crabbox run --provider hetzner -- pnpm test
```

本地运行时、可复用缓存和快速编辑运行循环优先考虑 `fast-feedback`。需要将执行移出工作站，或需要完整 VM 和 SSH 语义时，优先考虑 `linux-vm`。

## 为编程智能体提供临时环境 {#give-a-coding-agent-a-disposable-environment}

智能体需要在工作站之外执行仓库命令时，使用远程 SSH 租约或委托沙箱。本地沙箱提供独立工作区，但仍消耗笔记本资源。Crabbox 负责执行和清理；调用它的智能体或运行框架负责调度。

为独立智能体从各自工作副本创建独立租约：

```sh
crabbox warmup --provider hetzner --class standard --slug agent-tests
crabbox run --id agent-tests -- pnpm test
crabbox stop agent-tests
```

每个租约使用不同的 slug。预热租约会在命令之间保留环境；测试前请检查仓库配置并安装工具和依赖。

```sh
crabbox providers recommend agent-sandbox
crabbox providers recommend disposable-execution
crabbox providers recommend isolated-execution
```

这三种推荐回答不同的问题：

- `agent-sandbox` 偏重适合智能体和开发机执行的提供商；
- `disposable-execution` 要求运行时是可清理的临时环境；
- `isolated-execution` 偏重委托沙箱和本地沙箱边界。

最后一种推荐仅为路由建议，不是安全认证。Crabbox 假定操作者、仓库配置和工作负载可信；临时机器不会让恶意代码变安全。转发机密前，请阅读所选提供商的信任与网络模型。

## 验证 Linux、Windows、WSL2 或 macOS {#validate-linux-windows-wsl2-or-macos}

行为依赖目标操作系统时，使用平台推荐：

```sh
crabbox providers recommend linux-vm
crabbox providers recommend windows
crabbox providers recommend macos

crabbox run --provider aws --target windows --windows-mode wsl2 -- uname -a
```

原生 Windows 使用 PowerShell 和 Windows 工具。WSL2 在支持嵌套虚拟化的 Windows VM 内执行 POSIX 协议。macOS 目标可为本地 Apple Silicon VM 或云端 Mac，取决于提供商。

## 测试浏览器或可视桌面 {#test-a-browser-or-visible-desktop}

测试需要浏览器、截图、输入或交接给同事时，使用支持桌面的 SSH 租约：

```sh
crabbox providers recommend desktop
crabbox warmup --provider azure --target windows --windows-mode normal --desktop --slug ui-check
crabbox webvnc --id ui-check --open --take-control
```

WebVNC 允许获授权的同事查看和接管桌面。原生 VNC 使用 SSH 隧道；`crabbox code` 在受支持的 Linux 租约上打开 code-server 编辑器。协调器门户显示租约、桥接健康状态和保留的运行证据。使用浏览器时请保持本地桥接运行。前提条件见[综合访问指南](features/interactive-desktop-vnc.md)。

需要在机器释放后保留结果时，同时选择证据路由：

```sh
crabbox providers recommend run-evidence
crabbox providers recommend failure-diagnostics
```

## 复用预热状态 {#reuse-warm-state}

重复初始化的成本高于保留或恢复准备好的状态时，使用预热机器：

```sh
crabbox providers recommend warm-start
crabbox providers recommend versioned-workspace

crabbox warmup --slug warm-tests
crabbox run --id warm-tests -- pnpm test:changed
```

`warm-start` 依据本地运行时、缓存卷、保留会话、暂停/恢复和工作区状态等信号推荐，不保证所有提供商都有相同的快照或预热池机制。

## 并行展开实验 {#fan-out-parallel-experiments}

并行分支、智能体多次候选尝试或从准备好的状态启动测试分片时，使用支持 fork 的工作区提供商：

```sh
crabbox providers recommend fanout-testing
crabbox checkpoint fork <checkpoint-id> --count 4
```

这是提供商无关的检查点与 fork 路由，不保证能对 microVM 的实时内存执行 fork。

## 运行 GPU 工作负载 {#run-gpu-workloads}

模型测试、CUDA 构建、渲染或其他加速器任务可使用 GPU 推荐：

```sh
crabbox providers recommend gpu
crabbox run --provider runpod -- nvidia-smi
```

SSH 租约 GPU 提供商通常更适合交互调试。需要提供商负责执行和收集结果时，委托 GPU 提供商通常更合适。

## 无云凭据的本地运行 {#run-locally-without-cloud-credentials}

使用本地容器、VM 或策略沙箱，在工作站上快速冒烟测试：

```sh
crabbox providers recommend local
crabbox providers recommend offline-validation
crabbox run --provider local-container -- pnpm test
```

本地并不意味着统一的隔离模型。Docker 兼容容器、Apple VM、Multipass VM、Windows Sandbox 和本地策略运行时的主机访问、持久化和清理协议不同。运行陌生代码前请阅读所选提供商页面。

## 使用已有基础设施 {#use-infrastructure-you-already-own}

无需共享协调器，Crabbox 即可使用长期 SSH 主机、私有虚拟化平台或直接云账户：

```sh
crabbox providers recommend byo-ssh
crabbox providers recommend self-hosted
crabbox providers recommend linux-vm
```

主机生命周期不应由 Crabbox 管理时使用 `ssh`。需要 Crabbox 创建和清理运行时时，使用 Proxmox、XCP-ng、Incus、KubeVirt 或 Firecracker 等自托管提供商。

## 共享受管团队机群 {#share-a-governed-team-fleet}

团队需要共享凭据、租约所有权、清理、使用记录、活跃租约限制和月度费用上限时，使用协调器路径：

```sh
crabbox providers recommend team-cloud
crabbox login --url https://broker.example.com
crabbox usage --scope user
```

协调器是控制平面。CLI 仍直接连接普通 SSH 运行器进行同步和命令执行，并向协调器记录运行证据。访问该协调器的 `/portal` 可查看租约清单及桌面、编辑器和运行详情。直接提供商注册可以增加门户可见性，不转移提供商凭据或清理所有权。

## 需要更具体的路由？ {#need-a-more-specific-route}

CLI 还可推荐适用于产物、CI 证明、代码解释器、故障诊断、MCP 连接、网络限制、暂停/恢复、预览 URL、可达性、远程开发、资源观测、可复用运行会话、Web 应用冒烟测试及 Worker/模块执行的提供商。

完整名称、别名、筛选、评分与限制见 [`crabbox providers recommend`](commands/providers.md#providers-recommend)。

## 相关文档 {#related-docs}

- [提供商选择](features/provider-selection.md)
- [提供商参考](providers/README.md)
- [嵌套执行](features/nested-execution.md)
- [定价与成本](pricing.md)
