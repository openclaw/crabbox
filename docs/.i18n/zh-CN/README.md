<!-- source: README.md; sha256: 008297f255e89d26769a8517dd1c425b15462564563cce18859b56b7640a570e -->
# Crabbox {#crabbox}

**为智能体提供按需计算机。**

## Crabbox 是什么 {#what-crabbox-is}

Crabbox 为编程智能体和人类提供按需计算机，用于测试与构建。你继续在本地编辑，远程机器执行包含未提交修改的工作树。Crabbox 流式传回输出、返回命令退出码，并按提供商协议清理租约。

配置好提供商后，运行仓库已有的测试命令：

```sh
crabbox run -- pnpm test
```

## 一个循环，多种机器 {#one-loop-many-kinds-of-box}

标准远程执行循环：

1. 在现有工作副本中**本地编辑**。
2. **租用机器**或复用预热容量。
3. **同步**工作树，包含未提交修改。
4. 在机器上**运行**命令。
5. 向调用者**流式传回输出和退出码**。
6. **清理**拥有的资源，或保持预热以供下次运行。

委托提供商可以负责执行与同步路径。复用、证据和清理取决于所选提供商的能力与运行策略。

执行基础设施可以更换，无需把工作流改写成特定提供商脚本。知道任务但还没选提供商时，从[使用场景](use-cases.md)开始；已知道运行位置时，查阅[提供商参考](providers/README.md)。

## 更多智能体，更少等待 {#more-agents-less-waiting}

- **并行智能体：**测试和构建使用独立机器，不争抢笔记本的 CPU、RAM 和端口。
- **预热机器：**复用准备好的环境，缩短编辑运行循环。
- **跨平台测试：**从同一本地工作副本面向 Linux、macOS、原生 Windows 或 WSL2；可用性取决于提供商。
- **查看与操控：**在支持的桌面上使用 [VNC 或 WebVNC](features/interactive-desktop-vnc.md)，请求浏览器，或在受管 Linux 上通过 `--code` 使用浏览器中的 VS Code。经身份验证的[门户](features/portal.md)显示租约和运行日志；本地桥接运行时，WebVNC 支持观察或接管。
- **共享团队容量：**可选协调器保管云凭据、共享租约，为代理提供商强制执行到期时间和配置的费用上限。

[提供商目录](providers/README.md)涵盖云、本地 VM、SSH 主机和委托沙箱。[外部插件协议](providers/external.md)可连接自有后端。网站的提供商展示和分类数量由目录元数据生成。

## 从这里开始 {#start-here}

- [入门](getting-started.md) — 安装 CLI 并完成首次远程运行。
- [使用场景](use-cases.md) — 选择快速反馈、智能体执行、跨平台验证、浏览器 QA、并行展开或 GPU 等工作流。
- [定价与成本](pricing.md) — 了解当前开源、自备计算资源的成本模型与保护措施。
- [Crabbox 工作原理](how-it-works.md) — 跟踪运行在 CLI、协调器和执行器间的过程。
- [Crabbox 愿景](../VISION.md) — 了解产品范围、非目标及生命周期安全规则。

## 选择运行模式 {#pick-an-operating-model}

| 路径 | 适用场景 | 所有权 |
| --- | --- | --- |
| 本地运行时 | 快速、无凭据的开发检查 | 你的工作站和本地运行时 |
| 直接云或 SSH | 个人云账户、私有主机、自托管虚拟化 | 你的提供商账户或基础设施 |
| 团队协调器 | 共享凭据、租约、清理、用量和费用上限 | 你的 Cloudflare 或 Node.js/PostgreSQL 部署 |
| 委托执行 | 提供商管理的智能体、CI、浏览器或 GPU 任务 | 所选提供商或自托管运行时运营者 |

Crabbox 使用 MIT 许可证，目前没有托管控制平面套餐；计算资源和协调器基础设施由各运营者计费。具体边界见[定价与成本](pricing.md)。

## 信任边界 {#trust-boundary}

与可信同事运行可信仓库。Crabbox 是开发执行工具；隔离取决于所选运行时。复用和破坏性清理必须验证具体资源与声明的所有权。所有权不明确时拒绝执行。

通过[提供商选择](features/provider-selection.md)路由任务，然后阅读提供商文档和[安全模型](security.md)。推荐命令是工作流建议，不是安全认证。

## 深入阅读 {#go-deeper}

- **CLI 与配置：**[CLI](cli.md)、[命令参考](commands/README.md)、[配置](features/configuration.md)和[仓库初始化](features/repository-onboarding.md)。
- **机群与运维：**[架构](architecture.md)、[基础设施](infrastructure.md)、[运维](operations.md)、[可观测性](observability.md)、[只读设备配对](features/device-pairing.md)和[安全](security.md)。
- **运行与证据：**[任务](features/jobs.md)、[Actions 环境准备](features/actions-hydration.md)、[产物](features/artifacts.md)、[检查点](features/checkpoints.md)和[交互式桌面与 VNC](features/interactive-desktop-vnc.md)。
- **平台边界：**[嵌套执行](features/nested-execution.md)区分 WSL2、容器引擎、准备好的 KVM 主机和本地沙箱。
- **扩展：**[集成目录](integrations/README.md)、[提供商开发](features/provider-authoring.md)和[源码地图](source-map.md)。

## 关于文档 {#about-these-docs}

`docs/` 中的 Markdown 是 [crabbox.sh](https://crabbox.sh/) 的用户文档源。实现以代码为准；[源码地图](source-map.md)将文档行为追溯到负责的实现。

本地构建并验证网站：

```sh
scripts/check-docs.sh
open dist/docs-site/index.html
```

编辑 `docs/` 时，使用网站支持的 1–4 级标题和三个反引号代码围栏。站内标题链接用渲染器实际生成的同一标题标识检查；示例和注释不占用锚点。请保持已发布标题 ID 稳定。仅用于仓库的 Markdown 目标仍使用检查器原有的 GitHub 风格锚点规则，并不代表完整支持 GitHub Markdown。

添加或更新语言请参阅[文档翻译指南](translations.md)。
