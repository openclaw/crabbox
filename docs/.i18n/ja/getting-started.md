<!-- source: getting-started.md; sha256: 6f010425dbbffe15196dfbc9cbfce66ea14783090c7babf231211bf13239fc09 -->
# はじめに {#getting-started}

Crabbox はエージェントと人にオンデマンドコンピューターを提供します。ローカルで編集を続ける間にマシンを借り、未コミットの変更も同期し、コマンドを実行して出力と終了コードを返します。単発実行はマシンを解放し、ウォームリースは次回まで保持します。

リモートのテストとビルドはノート PC の CPU、メモリ、ポートを解放し、複数エージェントの並行作業を可能にします。Linux、macOS、ネイティブ Windows、WSL2 に対応したプロバイダーを選びます。画面付きの作業は[デスクトップ、VNC、ブラウザー、Code、ポータルのガイド](features/interactive-desktop-vnc.md)を参照してください。

この手順では、信頼できるリポジトリでインストールから `crabbox run -- pnpm test` まで進めます。設定とセットアップコマンドを実行前に確認してください。アカウント不要の初回実行には `--provider local-container` でローカルの Docker または Podman を使えます。[`crabbox-quickstart` スキル](integrations/agents.md#install-through-ecosystem-skill-managers)が全手順を案内します。

## 手順 1：インストール {#step-1-install}

```sh
brew install openclaw/tap/crabbox
```

確認します。

```sh
crabbox --version
crabbox doctor
```

`crabbox doctor` はチェックごとに 1 行表示します。新規インストールでは共通の `git` チェックが `ok` になり、プロバイダー固有のチェックは選択後に始まります。未選択なら `no provider selected` と表示し、互換メタデータ `source=compiled_default selected=false` を保持し、認証準備確認を省略して正常終了します。ライフサイクル操作の前に、設定、`CRABBOX_PROVIDER`、`--provider` で選択してください。選択後の検証は厳格なので認証情報がなければ失敗し得ます。比較には `crabbox providers recommend` を使います。

Homebrew を使わない場合は <https://github.com/openclaw/crabbox/releases> から macOS、Linux、Windows 用アーカイブを取得できます。macOS 実行ファイルは署名・公証済みです。

付属の `crabbox-runtime` ディレクトリは実際の CLI 実行ファイルの隣に置いてください。両方の Linux アーキテクチャのランタイムは対応するコントローラーと一組です。CLI だけのコピーやバージョン混在では完全なパックになりません。

Go ユーザーは明示したリリースの CLI だけをインストールできます。[GitHub Releases](https://github.com/openclaw/crabbox/releases) からタグを選びます。v0.44.0 以降に対応しています。

```sh
go install github.com/openclaw/crabbox/cmd/crabbox@v0.70.0
```

Go 1.26 が必要で、推奨ツールチェーンは go1.26.5 です。Go 1.26.5 以降か自動選択を使用してください。この方法はローカルで Go CLI をコンパイルするだけで、`crabbox-apple-vm-helper` やランタイムパックなどの付属品、署名・公証済み配布物は含みません。特に Apple Silicon の Apple VM には Homebrew または完全なリリースアーカイブを使ってください。

WSL2 と x64 の WSL なし Windows 転送選択には現在の `main` または Crabbox v0.42.1 以降が必要です。[Windows インストール](windows-install.md)を参照してください。

## 手順 2：ログイン {#step-2-log-in}

共有フリートではチームのコーディネーター URL を使います。認証情報とリース制限、費用上限、有効期限を管理します。直接クラウドとローカルのプロバイダーはログイン不要です。[アクセス経路の選択](#choosing-an-access-path)を参照してください。

```sh
crabbox login --url https://broker.example.com
```

`login` は GitHub OAuth をブラウザーで開きます。`--no-browser` は同じ端末のブラウザー向けに URL を表示します。ブローカーがコードを交換して組織所属を確認し、使い捨て確認を CLI のループバックリスナーへ戻してから、署名付きトークンを設定に保存します。

```text
logged in broker=https://broker.example.com provider=hetzner user=github:12345 org=example-org config=/Users/alice/Library/Application Support/crabbox/config.yaml
```

以後の `crabbox` コマンドは自動認証します。現在の ID を確認できます。

```sh
crabbox whoami
```

```text
user=github:12345 org=example-org auth=user broker=https://broker.example.com
```

### アクセス経路の選択 {#choosing-an-access-path}

アクセス条件はデプロイごとに異なります。チームの URL と GitHub 組織・チーム許可リストを使用します。OAuth が完了しても許可リスト外のアカウントは拒否されます。

個人や第三者の導入では次を選びます。

- **直接プロバイダー：**ローカルのクラウド認証情報を使う簡単な専用テスト環境。中央の履歴や共有費用制限の代わりにローカル状態と清掃を受け入れます。
- **自前コーディネーター：**Cloudflare Workers または Node.js/PostgreSQL を配置し、認証情報、同時リース数、月額上限、`crabbox usage`、永続的な清掃、共通エンドポイントを管理します。
- **アクセス申請：**運用者が組織向けの導入手順を提供している場合のみ。チームの URL は公開ブローカーとは限りません。

直接モードでは `login` を使いません。

```sh
crabbox doctor --provider hetzner
crabbox run --provider hetzner -- pnpm test
```

自前運用ではランタイムを選びます。

- **Cloudflare Workers：**Durable Object の状態、アラーム、定期清掃、任意の Cloudflare Access。
- **Node.js/PostgreSQL：**通常の HTTP/WebSocket サービス、PostgreSQL 13+、pg-boss の保守ジョブ、常時稼働する単一レプリカ。初期の移植可能ランタイムとして扱い、本番切り替え前に配置検証を完了します。

両方とも同じシークレット、認証設定、予算、API、ポータルを使います。[インフラ](infrastructure.md#choose-a-coordinator-runtime)を参照してください。ブラウザーログインには OAuth アプリと許可組織・チームが最低 1 つ必要ですが、共有トークン自動化には不要です。

ブラウザーを開けない CI では共有トークンを使います。

```sh
printf '%s' "$TOKEN" | crabbox login \
  --url https://broker.example.com \
  --provider aws \
  --token-stdin
```

全体の ID モデルは[認証と管理](features/auth-admin.md)を参照してください。

## 手順 3：リポジトリを導入 {#step-3-onboard-a-repo}

リポジトリ内で実行します。

```sh
crabbox init
```

`init` は 3 ファイルを作ります。`--config`、`--workflow`、1 個以上の `--skill` でパスを変更でき、`--force` で既存ファイルを上書きします。

```text
.crabbox.yaml                          repo defaults (profile, class, sync, env)
.github/workflows/crabbox.yml          Actions hydration workflow (optional)
.agents/skills/crabbox/SKILL.md        agent-facing skill instructions
```

生成した `.crabbox.yaml` の既定値を必要に応じて調整します。

- `profile`：レーン名。テンプレートは `<repo>-check`。
- `class`：`tiny`、`small`、`standard`、`fast`、`large`、`beast`。既定テンプレートは `beast`。
- `sync.exclude`：送信しないディレクトリ。
- `sync.include`：任意のルート相対許可リスト。除外の後に適用し、指定パス**だけ**を同期します。
- `env.allow`：リモートコマンドに公開する環境変数。

`--detect` はテストを検出して `jobs.detected` を生成します。`crabbox job run detected` で実行できます。

同期内容をプレビューします。

```sh
crabbox sync-plan
```

`sync-plan` はファイル数、総バイト数、大きいファイルを表示します。`dist/`、忘れた `.cache/`、巨大なアセットがあれば `sync.exclude` を絞って再確認します。初回同期はこのサイズに左右されます。

`jj` をインストールすれば、ネイティブ Git-backed Jujutsu ワークスペースを利用できます。作業中の変更と複数親マージを含む正確な `@` ツリーを同期します。`sync-plan --json` が選択リビジョンを表示し、通常の SSH プロバイダーでは `run --no-hydrate` を使います。同居 Git/Jujutsu は Git 経路です。前提と拒否条件は [Jujutsu 同期](features/sync.md#jujutsu-workspaces)を参照してください。

## 手順 4：マシンを準備 {#step-4-warm-a-box}

```sh
crabbox warmup --class standard --slug first-tests
```

`warmup` はリース取得、プロビジョニング、SSH とツールの準備待ちを行い、`--keep`（既定有効）で保持して概要、接続先、所要時間を表示します。

```text
leased cbx_abcdef123456 slug=first-tests provider=hetzner server=... type=... ip=203.0.113.10 idle_timeout=30m0s expires=...
ready ssh=crabbox@203.0.113.10:2222 network=public workroot=/work/crabbox
warmup complete total=...
```

コーディネーターはリースを準備済みで保持します。既定のアイドル 30 分と TTL 90 分が寿命を制限します。直接プロバイダーは独自に期限を実装するため、完了したら明示的に停止してください。サイズの選択前に `crabbox providers --json` を確認します。

`slug` または `id`（`cbx_...`）で再利用でき、以後はどちらも `--id` に渡します。

## 手順 5：コマンドを実行 {#step-5-run-a-command}

```sh
crabbox run --id first-tests -- pnpm test
```

実行時の流れ：

1. CLI が SSH の準備を確認します。
2. origin とベース参照から Git を準備し、変更済みツリーを rsync で重ねます。指紋が同じなら同期を省略します。
3. SSH で実行し、stdout と stderr をストリーミングします。
4. 実行中にアイドル期限が切れないようブローカーにハートビートを送ります。
5. 同期時間、実行時間、終了コード、Linux では上限付きのテレメトリーを `run_...` の履歴に記録します。

ツールと依存関係のインストールはリポジトリが担当します。必要ならテスト前にセットアップスクリプトや [Actions の環境準備](features/actions-hydration.md)を実行してください。

単発実行では `--id` を省略できます。

```sh
crabbox run -- pnpm test
```

新しいリースを取得して実行後に解放します。臨時テストは単発、同じマシンでの反復は `warmup` + `--id` を使います。

## 手順 6：履歴を確認 {#step-6-inspect-history}

```sh
crabbox history
crabbox events run_abcdef123456
crabbox logs run_abcdef123456
crabbox results run_abcdef123456
```

`history` は最近の実行、`events` は順序付きイベント（取得、同期、コマンド、出力、終了）、`logs` は保存出力、`results` は添付 JUnit レポートを表示します。

ポータルが有効なら `/portal/runs/run_abcdef123456` で同じ内容を見られます。

## 手順 7：リースを停止 {#step-7-stop-the-lease}

作業後に実行します。

```sh
crabbox stop first-tests
```

`stop` はプロバイダー契約に従って解放し、予約費用を戻します。通常の一時マシンは削除されますが、静的 SSH ホストや保留リソースは各規則に従います。所有権確認が必要で、復旧 claim や固定 ID の終端レシートはローカルに残る場合があります。忘れたブローカーのマシンはコーディネーターが期限清掃します。

```sh
crabbox cleanup --dry-run
```

`cleanup` は直接プロバイダーの残留物とローカル状態向けです。ブローカー側の清掃はそのアラームが担当します。

## よくある使い方 {#common-variations}

長いセッションで保持する：

```sh
crabbox warmup --idle-timeout 4h --ttl 8h --slug swift-crab
crabbox run --id swift-crab -- pnpm test
crabbox run --id swift-crab -- pnpm bench
crabbox stop swift-crab
```

デスクトップを開く：

```sh
crabbox warmup --desktop --browser --slug ui-check
crabbox webvnc --id ui-check --open --take-control
```

code-server を開く：

```sh
crabbox warmup --code --slug code-check
crabbox run --id code-check --sync-only
crabbox code --id code-check --open
```

ブラウザー利用中はブリッジコマンドを動かし続けます。Desktop と Code はプロバイダーと対象次第で、ブラウザー Code にはコーディネーターと独立した Code origin も必要です。[統合ガイド](features/interactive-desktop-vnc.md)で VNC、WebVNC、ポータル、共有、清掃を確認できます。

Zed Remote Projects で同期済みチェックアウトを開く：

```sh
crabbox run --id swift-crab --sync-only
crabbox open --editor=zed --id swift-crab
```

表示された SSH コマンドを Zed の **Connect New Server** に貼り、表示フォルダーを開いて、作業中は `crabbox open` を起動したままにします。

所有する Mac を使う（静的 SSH、作成なし）：

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

コマンドごとにプロバイダーとクラスを変更する：

```sh
crabbox run --provider aws --class beast -- pnpm test
```

## 次に読む {#where-to-go-next}

- [Crabbox の仕組み](how-it-works.md) — 基本モデル。
- [CLI](cli.md) — コマンドと終了コード。
- [コマンド](commands/README.md) — 個別の説明。
- [機能](features/README.md) — 機能ごとの説明。
- [設定](features/configuration.md) — YAML と優先順位。
- [プロバイダー](providers/README.md) — 実行先の選択。
- [プロバイダー開発](features/provider-authoring.md) — 拡張方法。
- [トラブルシューティング](troubleshooting.md) — 手順が失敗した場合。
