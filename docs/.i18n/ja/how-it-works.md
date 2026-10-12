<!-- source: how-it-works.md; sha256: 2e0288463ff74a878d06a6226e5924a2ce5426c54796c463626ae14219ab37b5 -->
# Crabbox の仕組み {#how-crabbox-works}

初めて使う方、全体像を知りたい方、変更前に責任範囲を確認したい方のための説明です。

## 概要 {#tl-dr}

Crabbox はエージェントと人にオンデマンドマシンを提供します。ローカルで編集し、Linux、macOS、ネイティブ Windows、WSL2 の別マシンでテストやビルドを動かせます。共有フリートでは次が協力します。

- **CLI**（`cmd/crabbox`、`internal/cli`）：取得、変更済みツリーの同期、実行、出力、解放。
- **コーディネーター**（`worker/src`）：認証情報、リース状態、期限、清掃、使用量、費用制限。Cloudflare Workers + Durable Object または Node.js + PostgreSQL + pg-boss で稼働します。

ランナーはブローカーのシークレットを保持しません。セットアップ、依存関係、仕事はリポジトリの責任です。コーディネーターは記録・ログ・テレメトリーを保持しますが、画像などの成果物は解放前に取得・公開してください。直接プロバイダーはコーディネーター不要です。

## 構成要素 {#the-pieces}

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

通常の CLI は HTTPS で取得後、SSH/rsync でランナーへ**直接**接続します。通常の実行でランナーからコーディネーターへの呼び出しはなく、実行経路は一方向です。コーディネーターはリースを管理し、チェックアウト転送や実行はしません。CLI は別途メタデータ、プレビュー、ログ、結果を送信し、ポータルの対話ブリッジも経由します。[非公開 AWS ワークスペース](features/aws-private-workspaces.md)は SSH データプレーンのない独立した SSM-only API です。

長時間処理は認証済み `/v1/control` WebSocket でイベント購読とハートビートを送り、遅延の大きい回線でのポーリングを減らせます。HTTPS が正規の保存先兼互換フォールバックなので古い組み合わせも動作します。

## 責任範囲 {#ownership}

| 層 | 担当 |
| :--- | :--- |
| **CLI** | 設定・フラグ、リース専用 SSH 鍵、準備確認、Git/rsync、指紋・整合性確認、コマンド・出力、制御 WebSocket と HTTP 代替、解放 |
| **Broker** | 認証・ID、直列化した状態、認証情報、作成削除、期限、照会、記録・ログ・イベント・テレメトリー、使用量・上限 |
| **Provider** | 計算資源。アダプターが Hetzner、AWS EC2、Azure VM、GCP などを作成削除 |
| **Runner** | 永続的責務なし。Linux の SSH/Git/rsync/作業ルート、他 OS の個別初期化、既存の静的 SSH。プロジェクトのランタイムはリポジトリで準備 |

## `crabbox run` の動作 {#what-crabbox-run-does}

通常のブローカー付き SSH 実行は 5 段階です。直接プロバイダーはローカルでライフサイクルを管理し、委譲型は通信・実行を所有します。

**1. 計画。**フラグ → 環境 → リポジトリ → ユーザー → 既定の順に設定を読みます。`cbx_` と 12 桁の 16 進数の一時 ID、および `<user-config>/crabbox/testboxes/<lease>/id_ed25519` に鍵を作ります（AWS/Azure Windows は RSA）。

**2. 取得。**クラス、プロバイダー、対象、TTL、アイドル期限、slug、機能、公開鍵を `POST /v1/leases` へ送ります。コーディネーターが認証・変更直列化・同時数と月額制限・実価格取得・最大 TTL 費用予約を行い、地域や市場の代替を使って作成します。ホスト、ユーザー、ポート、作業ルート、期限、確定 ID、slug を返します。ID が変われば CLI の鍵ディレクトリも移します。

**3. 同期。**SSH と準備プローブを待ち、origin/ベース参照から Git を準備します。指紋一致なら省略。不一致なら POSIX は rsync（Linux は `/work/crabbox/<lease>/<repo>`）、ネイティブ Windows はマニフェスト tar で転送し、整合性チェックと対応する基準履歴の準備を行います。

**4. 実行。**裏でハートビートを送り、SSH 実行の stdout/stderr を返します。ブローカーには段階別イベントを記録します。

**5. 解放。**新規リースは `--keep` がなければ解放、明示的な `--id` 再利用は保持します。`--stop-after` で上書き可能です。プロバイダーが清掃し予約費用を戻します。新規・非保持で SSH 準備前に失敗した場合、`crabbox run` は一度だけ交換します。保持・明示再利用でコマンドを重複しません。

## ウォームマシンと再利用 {#warm-boxes-and-reuse}

`crabbox warmup` は同じ作成処理で、実行せず待機させます。slug/ID で明示再利用します。

```sh
crabbox warmup --profile project-check --slug blue-lobster
crabbox run --id blue-lobster -- pnpm test:changed
crabbox ssh --id blue-lobster
crabbox stop blue-lobster
```

各リースには `cbx_…` ID と slug があり、自動 slug は `blue-lobster-1f3a9c2b` のような 8 桁指紋付きです。`--slug` で指定し、多くは `--id` で両方使えます。ハートビートが `lastTouchedAt` とアイドル期限を更新します。放置すると永続スケジューラーが解放します。

## ブローカー経由と直接接続 {#brokered-vs-direct-provider}

通常は**ブローカー経由**です。

```text
CLI -> coordinator (Cloudflare or Node/PostgreSQL) -> provider API
CLI -> runner over SSH/rsync
```

共有インフラに適し、シークレットはローカルに渡さず清掃・費用・使用量を集中管理します。`hetzner`、`aws`、`azure`、`daytona`、`gcp` で、`CRABBOX_COORDINATOR` または `config set-broker` の URL があるときのみ有効です。

未設定なら**直接接続**します。

```text
CLI -> provider API
CLI -> runner over SSH/rsync
```

AWS SDK の認証チェーンや Hetzner token などローカル認証が必要です。中央履歴とブローカー心拍はありません。ブローカー診断や、サンドボックス・静的 SSH・ローカルコンテナーなど直接専用アダプターに使います。

`broker.mode: registered` は直接経路を維持し、所有者別メタデータと心拍だけをミラーします。ポータル一覧、任意共有、アウトバウンド WebVNC を追加し、鍵や清掃権限は移しません。解放は既定でメタデータ削除のみ。明示的に結び付けたアダプターはユーザー確認でワークスペースを削除できます。登録は管理対象のクォータ・費用・イメージ・プール・保守に入りません。

静的 SSH（`provider: ssh`）は URL があっても直接です。macOS と WSL2 は POSIX rsync、ネイティブ Windows は PowerShell/tar を使います。

## 認証と ID {#auth-and-identity}

ユーザーは署名済み GitHub token、信頼する自動化は共有 bearer token を使います。Cloudflare Access を追加でき、Node は指定したリバースプロキシ CIDR の ID ヘッダーだけ信頼します。

```text
Authorization: Bearer <token>
X-Crabbox-Owner: <email>
X-Crabbox-Org: <org>
```

`crabbox login` の所有者は不変の `github:<numeric-id>`。共有 token は `CRABBOX_OWNER`、Git メール環境、`git config user.email` から選び、`CRABBOX_ORG` が組織です。未検証 Access ヘッダーは無視し、検証済み JWT のメールだけを使います。[セキュリティ](security.md)を参照してください。

## 同期モデル {#sync-model}

クリーンなチェックアウトは不要で、ローカルを基準にします。

- origin/ベース参照から Git を準備；
- 変更ファイルを rsync、任意のチェックサム；
- 指紋で不要な同期を省略；
- 大きなディレクトリを設定で除外；
- 追跡ファイルの大量削除を警戒；
- 変更テスト向けに基準履歴を準備。

エージェントも人もローカル編集・遠隔実行です。`crabbox sync-plan` で読み取り専用プレビューできます。

## 費用と使用量 {#cost-and-usage}

リースには 2 種類の金額があります。

```text
estimatedUSD   elapsed runtime cost so far
reservedUSD    worst-case TTL cost reserved before provisioning
```

時間単価の優先順位：

```text
1. CRABBOX_COST_RATES_JSON   explicit override
2. provider live pricing     e.g. EC2 spot history, Hetzner server-type prices
3. built-in fallback rates   last-resort defaults
```

Hetzner の換算は `CRABBOX_EUR_TO_USD`。`CRABBOX_MAX_*` は全体・所有者・組織の同時数と月額ドル上限で、超過は HTTP 429 です。`crabbox usage` は `GET /v1/usage` をユーザー・組織・プロバイダー・タイプ別に表示します。

## 障害と清掃 {#failure-and-cleanup}

クラッシュ、切断、cloud-init 失敗、部分成功、再試行、プロセスより長く残るマシンを前提にします。

- フリート判断を直列化；
- 可能な限り冪等な作成；
- Crabbox タグ；
- 繰り返して安全な解放；
- Durable Object アラームまたは pg-boss と定期照合で期限清掃；
- 直接清掃は保守的；
- ブローカー清掃はブローカー所有。

コーディネーター設定時、`crabbox cleanup` は資源清掃を拒否します。その責任はコーディネーターにあります。

## 次に読む {#where-to-go-next}

- [アーキテクチャ](architecture.md)：構成、API、状態、障害。
- [オーケストレーター](orchestrator.md)：責任、寿命、清掃、費用。
- [CLI](cli.md)：コマンド、設定、出力、終了コード。
- [機能](features/README.md)：機能別。
- [コマンド](commands/README.md)：コマンド別。
- [インフラ](infrastructure.md)：ランタイム、入口、シークレット、DNS、設定。
- [セキュリティ](security.md)：認証、SSH、清掃、信頼境界。
