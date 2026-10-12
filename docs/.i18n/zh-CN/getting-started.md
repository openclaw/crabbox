<!-- source: getting-started.md; sha256: 6f010425dbbffe15196dfbc9cbfce66ea14783090c7babf231211bf13239fc09 -->
# 入门 {#getting-started}

Crabbox 为编程智能体和人类提供按需计算机。继续编辑本地工作副本；Crabbox 租用机器、同步包含未提交修改的工作树、执行命令、流式传回输出并返回退出码。单次运行结束后释放机器；预热租约则保留供下次使用。

远程测试和构建释放笔记本的 CPU、内存及端口，让多个智能体并行工作。选择支持目标系统的提供商，运行 Linux、macOS、原生 Windows 或 WSL2。可视化任务见[桌面、VNC、浏览器、Code 和门户指南](features/interactive-desktop-vnc.md)。

本教程从安装开始，在可信仓库中运行 `crabbox run -- pnpm test`。执行前检查仓库配置与安装命令。无需账户的首次体验可用 `--provider local-container` 在本机 Docker 或 Podman 中执行；[`crabbox-quickstart` 技能](integrations/agents.md#install-through-ecosystem-skill-managers)涵盖整个无凭据流程。

## 步骤 1：安装 {#step-1-install}

```sh
brew install openclaw/tap/crabbox
```

验证安装：

```sh
crabbox --version
crabbox doctor
```

`crabbox doctor` 每项检查输出一行。全新安装时，提供商无关的 `git` 检查应返回 `ok`；选定提供商后才检查其工具。未选择时报告 `no provider selected`，保留兼容元数据 `source=compiled_default selected=false`，跳过凭据就绪检查但命令不失败。生命周期命令前，通过配置、`CRABBOX_PROVIDER` 或 `--provider` 选择提供商。选择会严格校验，未配好凭据可能失败；运行 `crabbox providers recommend` 比较选项。

不用 Homebrew 时，可从 <https://github.com/openclaw/crabbox/releases> 下载 macOS、Linux 或 Windows 对应归档；macOS 可执行文件经过签名和公证。

将附带的 `crabbox-runtime` 目录保留在实际 CLI 可执行文件旁。两种 Linux 运行时架构属于匹配的控制器；只复制 CLI 或混用版本无法保留完整运行时包。

Go 用户可指定发行版仅安装 CLI。请从 [GitHub Releases](https://github.com/openclaw/crabbox/releases) 选择标签；此渠道从 v0.44.0 开始支持。例如：

```sh
go install github.com/openclaw/crabbox/cmd/crabbox@v0.70.0
```

模块要求 Go 1.26，首选工具链为 go1.26.5。请使用 Go 1.26.5 或更高版本，或启用自动工具链选择。此命令仅在本地编译 Go CLI，不安装 `crabbox-apple-vm-helper`、运行时包等配套可执行文件或资源，也不是签名并公证的预编译发行包。完整发行包，尤其 Apple Silicon 的 Apple VM 支持，请使用 Homebrew 或发行归档。

WSL2 与 x64 无 WSL 的 Windows 传输选择需要当前 `main` 构建或 Crabbox v0.42.1 及以上。完整步骤见[支持的 Windows 安装方式](windows-install.md)。

## 步骤 2：登录 {#step-2-log-in}

共享机群使用团队的协调器 URL。它保管提供商凭据，实施租约限制、费用上限与到期策略。直接云和本地提供商跳过登录，见[选择访问路径](#choosing-an-access-path)。

```sh
crabbox login --url https://broker.example.com
```

`login` 在浏览器中打开 GitHub OAuth；使用 `--no-browser` 可打印 URL，供同一设备上的浏览器访问。代理交换 OAuth 代码、验证 GitHub 组织成员资格，将一次性确认重定向到 CLI 回环监听器，然后把签名令牌写入用户配置：

```text
logged in broker=https://broker.example.com provider=hetzner user=github:12345 org=example-org config=/Users/alice/Library/Application Support/crabbox/config.yaml
```

之后每个 `crabbox` 命令自动认证。随时查看身份：

```sh
crabbox whoami
```

```text
user=github:12345 org=example-org auth=user broker=https://broker.example.com
```

### 选择访问路径 {#choosing-an-access-path}

代理访问由部署决定。使用团队提供的协调器 URL 与 GitHub 组织/团队允许列表。即使 OAuth 完成，不在允许列表中的账户仍会被拒绝。

个人或第三方安装可选：

- **直接提供商模式：**自带本地云凭据，适合私人快速测试；接受本地清理与状态管理，代替代理用量历史和共享费用上限。
- **自托管协调器：**部署 Cloudflare Workers 或 Node.js/PostgreSQL 服务，获得集中凭据、活跃租约限制、月度费用上限、`crabbox usage`、持久化清理和共享端点。
- **申请访问：**仅当代理运营者为你的组织提供明确的接入流程。团队端点不自动成为开放代理。

直接模式完全跳过 `login`：

```sh
crabbox doctor --provider hetzner
crabbox run --provider hetzner -- pnpm test
```

自托管先选择运行时：

- **Cloudflare Workers：**Durable Object 状态、闹钟、计划清理及可选 Cloudflare Access。
- **Node.js/PostgreSQL：**普通 HTTP/WebSocket 服务、PostgreSQL 13+ 状态、pg-boss 维护任务和单个常驻副本。视为初始可移植运行时，生产切换前完成部署验证。

二者使用相同的提供商机密、认证配置、预算限制、API 和门户。见[基础设施](infrastructure.md#choose-a-coordinator-runtime)。浏览器登录需要 GitHub OAuth 应用和至少一个允许的组织/团队，共享令牌自动化不需要。

无法打开浏览器的 CI 可用共享令牌：

```sh
printf '%s' "$TOKEN" | crabbox login \
  --url https://broker.example.com \
  --provider aws \
  --token-stdin
```

完整身份模型见[认证与管理](features/auth-admin.md)。

## 步骤 3：初始化仓库 {#step-3-onboard-a-repo}

在仓库内运行：

```sh
crabbox init
```

`init` 写入三个文件；用 `--config`、`--workflow` 或一个/多个 `--skill` 覆盖路径，用 `--force` 覆盖已有文件：

```text
.crabbox.yaml                          repo defaults (profile, class, sync, env)
.github/workflows/crabbox.yml          Actions hydration workflow (optional)
.agents/skills/crabbox/SKILL.md        agent-facing skill instructions
```

生成的 `.crabbox.yaml` 有合理默认值。按仓库需要调整：

- `profile`：执行通道名称，模板使用 `<repo>-check`；
- `class`：`tiny`、`small`、`standard`、`fast`、`large` 或 `beast`，模板使用 `beast`；
- `sync.exclude`：绝不能发送到执行器的目录；
- `sync.include`：可选、相对仓库根目录的允许列表；设置后排除规则先应用，然后**仅**同步这些路径，方便从大型仓库只传少量文件；
- `env.allow`：远程命令允许读取的环境变量。

使用 `--detect` 扫描测试命令并写入 `jobs.detected`，随后可运行 `crabbox job run detected`。

预览同步内容：

```sh
crabbox sync-plan
```

`sync-plan` 输出文件数、总字节数和最大文件。若发现 `dist/`、遗忘的 `.cache/` 或数 GB 资源，收紧 `sync.exclude` 后重试。新执行器首次同步的速度受此体积限制。

安装 `jj` 后可使用原生 Git-backed Jujutsu 工作区：Crabbox 捕获工作副本编辑并同步精确的 `@` 树，包括多父提交。`sync-plan --json` 显示选择的修订版；普通 SSH 提供商使用 `run --no-hydrate`。共置 Git/Jujutsu 工作区沿用 Git 路径。前提条件与安全拒绝规则见 [Jujutsu 同步](features/sync.md#jujutsu-workspaces)。

## 步骤 4：预热机器 {#step-4-warm-a-box}

```sh
crabbox warmup --class standard --slug first-tests
```

`warmup` 获取租约、创建执行器、等待 SSH 和工具就绪，保留租约（`--keep` 默认开启），并显示摘要、就绪端点及完成时间：

```text
leased cbx_abcdef123456 slug=first-tests provider=hetzner server=... type=... ip=203.0.113.10 idle_timeout=30m0s expires=...
ready ssh=crabbox@203.0.113.10:2222 network=public workroot=/work/crabbox
warmup complete total=...
```

协调器将租约保持可用。空闲超时默认 30 分钟，TTL 默认 90 分钟，共同限制寿命。直接提供商自行执行到期策略；用完务必明确停止。机器规格因提供商而异，选择前检查 `crabbox providers --json`。

可通过友好的 `slug` 或 `id`（`cbx_...` 标识）复用租约，后续命令均用 `--id`。

## 步骤 5：执行命令 {#step-5-run-a-command}

```sh
crabbox run --id first-tests -- pnpm test
```

执行过程：

1. CLI 验证租约上的 SSH 就绪状态。
2. 从 origin 和基准引用初始化远程 Git，再用 rsync 叠加未提交工作树；指纹未变化则跳过同步。
3. 通过 SSH 执行命令，流式返回 stdout 和 stderr。
4. 向代理发送心跳，防止运行中因空闲到期。
5. 记录 `run_...` 历史项，包含同步时间、命令时间、退出码和 Linux 上有界的遥测采样。

机器只提供执行机制；仓库负责工具和依赖安装。必要时先执行安装脚本或[从 Actions 准备环境](features/actions-hydration.md)。

单次运行可省略 `--id`：

```sh
crabbox run -- pnpm test
```

这会获取新租约、执行命令并在退出时释放。临时测试用单次运行；同机迭代用 `warmup` + `--id`。

## 步骤 6：查看历史 {#step-6-inspect-history}

```sh
crabbox history
crabbox events run_abcdef123456
crabbox logs run_abcdef123456
crabbox results run_abcdef123456
```

`history` 列出近期运行；`events` 显示有序事件（租用、同步、命令、输出块、结束）；`logs` 返回保留输出；`results` 解析附加的 JUnit 报告。

启用门户时，`/portal/runs/run_abcdef123456` 以网页展示相同数据。

## 步骤 7：停止租约 {#step-7-stop-the-lease}

完成后：

```sh
crabbox stop first-tests
```

`stop` 按提供商清理协议释放租约并解除代理预留费用。多数临时提供商删除机器；静态 SSH 主机保留，其他保留资源遵循提供商策略。清理必须验证所有权。本地可能保留恢复声明或固定 ID 的终态凭证。遗忘的代理机器由协调器安排到期清理。

```sh
crabbox cleanup --dry-run
```

`cleanup` 扫除直接提供商残留资源与本地状态；代理资源清理由代理闹钟负责。

## 常见变体 {#common-variations}

较长会话中保持租约：

```sh
crabbox warmup --idle-timeout 4h --ttl 8h --slug swift-crab
crabbox run --id swift-crab -- pnpm test
crabbox run --id swift-crab -- pnpm bench
crabbox stop swift-crab
```

打开桌面：

```sh
crabbox warmup --desktop --browser --slug ui-check
crabbox webvnc --id ui-check --open --take-control
```

打开 code-server 标签页：

```sh
crabbox warmup --code --slug code-check
crabbox run --id code-check --sync-only
crabbox code --id code-check --open
```

使用浏览器时保持桥接命令运行。桌面和 Code 支持取决于提供商与目标；浏览器 Code 还需要已配置的协调器和隔离的 Code origin。[综合访问指南](features/interactive-desktop-vnc.md)解释原生 VNC、本地 WebVNC、门户、共享和清理。

在 Zed Remote Projects 中打开同步的工作副本：

```sh
crabbox run --id swift-crab --sync-only
crabbox open --editor=zed --id swift-crab
```

将输出的 SSH 命令粘贴到 Zed 的 **Connect New Server** 对话框，打开输出目录，并在工作期间保持 `crabbox open` 运行。

使用已有 Mac（静态 SSH，不创建机器）：

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

按命令覆盖默认提供商和规格：

```sh
crabbox run --provider aws --class beast -- pnpm test
```

## 后续阅读 {#where-to-go-next}

- [Crabbox 工作原理](how-it-works.md) — 心智模型。
- [CLI](cli.md) — 完整命令与退出码。
- [命令](commands/README.md) — 每个命令的参考。
- [功能](features/README.md) — 每项功能的指南。
- [配置](features/configuration.md) — YAML 结构与优先级。
- [提供商](providers/README.md) — 选择执行后端。
- [提供商开发](features/provider-authoring.md) — 添加提供商。
- [故障排除](troubleshooting.md) — 步骤失败时如何处理。
