<!-- source: concepts.md; sha256: 27d2835bb5b9d8ffdb9a39d681b5f2c01215e10c33a0199b1b1e2dd3347e29c8 -->
# 概念 {#concepts}

Crabbox はエージェントと人にマシンを提供します。ローカルで編集し、**リース**で確保し、**実行**で同期とコマンド起動を行います。独立マシンにより CPU、メモリ、ポートを競合せず並列テストできます。

Linux、macOS、ネイティブ Windows、WSL2 が対象です。ウォームリースは環境を保持し、Desktop、Browser、Code とポータルが対話操作と証跡を提供します。[アクセスガイド](features/interactive-desktop-vnc.md)を参照してください。

CLI、コーディネーター、文書共通の用語です。同義語の推奨形は**太字**です。

## 計算資源の用語 {#compute-vocabulary}

**Lease**：作成・解決したマシンの期限付き予約。`cbx_...`、slug、アイドル期限、TTL、状態 `active`、`released`、`expired`、`failed` を持ち、課金と清掃の単位です。

**Runner**：プロバイダーが用意し初期化した実行マシン。Hetzner、AWS EC2、静的 SSH も同じ分類です。

**Box** / **testbox**：非公式な呼称。新文書は runner を優先し、ウォームマシンなど製品的な説明では box も使います。

**Pool**：ユーザー・組織・フリートに見える稼働中集合。`crabbox list` と管理用 `GET /v1/pool` が公開します。

**Pond**：`pond=<name>` ラベルを共有するグループ。中央オブジェクトはなく、稼働メンバーがいる間だけ存在します。`crabbox pond peers`、`crabbox pond
connect`/`disconnect`、`crabbox pond release`。[Pond](features/pond.md)を参照。

**Lease ID**：`cbx_` と小文字 16 進数 12 桁、計 16 文字。例 `cbx_abcdef123456`。ラベル、ログ、claim、API で使います。[識別子](features/identifiers.md)。

**Slug**：読みやすい名前。単語の組と 8 桁指紋、例 `swift-crab-1f3a9c2b`。競合時はさらに 4 桁追加。`--slug swift-crab` で指定可能。`--slug` は `[a-z0-9-]`、最大 41 文字に正規化し、`--id` は ID と名前の両方を受け付けます。

**Run**：`crabbox run` 一回分。`run_` ID、リース、コマンド、終了コード。コーディネーターには永続履歴、直接実行にはローカル計測・証跡・失敗成果物があります。[履歴とログ](features/history-logs.md)。

**Workspace**：チェックアウトと実行状態。同期ファイル、依存関係、作業ディレクトリ、出力、任意の画面状態を含み、単なるマシン以上を指します。

## 役割 {#roles}

**CLI**：ローカル Go バイナリ `crabbox`。設定、同期、実行、出力、リース鍵を管理。[アーキテクチャ](architecture.md)。

**Broker** / **coordinator**：認証済み共有制御プレーン。認証情報、リース、期限、清掃、履歴、使用量、上限。Cloudflare Workers/Fleet Durable Object または Node.js/PostgreSQL/pg-boss。サービス強調は coordinator、信頼境界強調は broker。

**Provider**：取得・解決・一覧・解放を実装するアダプター。AWS、Azure、Google Cloud、Hetzner、Proxmox、Parallels、Local Container、静的 SSH、SSH サンドボックス Daytona、KubeVirt、External、Namespace Devbox、Semaphore、Sprites、exe.dev、RunPod、Railway、委譲型 Blacksmith Testbox、E2B、Modal、Islo、Tensorlake、[Upstash Box](https://upstash.com/docs/box/overall/quickstart)、Cloudflare、Azure Dynamic Sessions、W&B。[リファレンス](providers/README.md)。

**Backend**：Go インターフェース。`SSHLeaseBackend` は実際の SSH 対象、`DelegatedRunBackend` は提供元による実行。[バックエンド](provider-backends.md)。

**Operator**：管理権限を持ち、`crabbox admin` やイメージ作成・昇格を実行する人。

**Agent**：CLI を呼ぶ LLM プロセス。Crabbox は実行と証跡、エージェントやハーネスはプロンプト、判断、調整を担当します。信頼する設定・仕事が前提です。

## モード {#modes}

**Brokered mode** / **coordinator mode**：作成・状態・清掃はコーディネーター、SSH/rsync/実行は CLI が直接行います。秘密は中央に保持。AWS、Azure、Daytona、Google Cloud、Hetzner で `CRABBOX_COORDINATOR` または `config set-broker` を設定します。

**Direct mode** / **direct-provider mode**：直接 API、中央履歴や費用上限なし。ほかの提供元では必須、この 5 種でも URL がなければ使用します。

**Static mode**：`provider: ssh` の既存ホスト。作成・削除せず、常に直接接続。

**Delegated mode**：Blacksmith、E2B、Modal、Islo などが実行し出力を返します。ローカルの `--sync-only`、`--checksum` は拒否され、時間は `sync=delegated`。

## コマンド {#commands}

**warmup**：確保・準備して保持。`--keep` は既定 true。まだ実行しません。

**run**：取得/再利用、同期、実行、出力、終了コード。新規は解放、再利用は保持。`--keep` と `--stop-after` で制御。

**stop** / **release**：契約に沿って終了。通常は一時資源を削除、静的・保持資源は例外。

**cleanup**：正確な所有確認後、対象の直接資源を清掃。ラベル、名前、ID だけでは削除不可。

**reuse**：`--id`/slug で既存のリースを `run` に選択。`warmup` は作成、`--lease-id` は対応元での冪等作成であり一般的再利用ではありません。

**reclaim**：claim をチェックアウト間で移す `--reclaim`。通常リースはリポジトリに結び付くため必要。

**hydrate**：リポジトリの Actions から依存関係を準備。対応する手順を実行し、完全な意味が必要なら `--github-runner` で登録・本物のジョブを起動。[Actions 準備](features/actions-hydration.md)。

## 状態 {#state}

**Idle timeout**：心拍なしで解放する時間。既定 1800 秒/30 分。心拍か touch でリセット。

**TTL**：絶対寿命 5400 秒/90 分、最大 86400 秒/24 時間。心拍で延長不可。`expiresAt = min(createdAt + ttl, lastTouchedAt + idleTimeout)`。

**Heartbeat**：`POST /v1/leases/{id}/heartbeat` で `lastTouchedAt` を更新し、任意のテレメトリーや明示したアイドル変更を送信。`expiresAt` 以降は拒否され清掃が担当。

**Touch**：状態・アイドル更新。直接提供元は `Touch`、ブローカーでは心拍。

**Reserved cost**：`hourlyRate × ttl` の最大費用を月額枠から予約し、解放で返却。

**Estimated cost**：`active` 時間と単価から推定し `crabbox usage` に表示。[費用](features/cost-usage.md)。

## 同期 {#sync}

**Manifest**：`git ls-files --cached` と `git ls-files --others --exclude-standard` による NUL 区切りパス。削除は別記録。

**Fingerprint**：コミット、変更メタデータ、マニフェストのハッシュ。一致なら rsync 省略。

**Git seeding**：初回に origin/base からリモート Git を準備し差分利用を可能にする任意手順。

**Base ref**：準備・履歴復元の基準。`sync.baseRef` で設定。

**Sanity check**：rsync 後の大量削除、項目欠落、疑わしい結果を検査。[同期](features/sync.md)。

## 機能 {#capabilities}

**Desktop**：`--desktop` によるサイズ変更可能な TigerVNC/XFCE（管理 Linux/新規コンテナー）または Wayland/GNOME。`crabbox
vnc`、`crabbox webvnc`、多くの画面付き `--browser` で必要。

**Browser**：`--browser` がブラウザーを導入・検出しパスを公開。ヘッドレス可、`--desktop` で表示。エンジンは提供元/イメージ次第、ログイン済みプロファイルなし。

**Code**：`--code` が loopback 8080 の code-server を導入。`crabbox code` と `/code/` 用、管理 Linux のみ。

**Tailscale**：管理 Linux に tailnet で到達し公開 IP を不要にする機能。経路選択の `--network tailscale` とは別。[機能](features/capabilities.md)。

## コーディネーターのランタイム {#coordinator-runtime}

**FleetCoordinator**：両ランタイム共通の TypeScript 制御。ルート、認証、アダプター、記録、費用、清掃、ポータル、ブリッジ。

**Durable Object**：Cloudflare の保存・直列化機構。Fleet を `idFromName("default")` で 1 個使用。

**PostgreSQL runtime**：移植可能な Node。`crabbox` スキーマに状態、pg-boss の `crabbox_jobs` に正確な保守と周期照合。

**Alarm**：将来の清掃期限。Durable Object/pg-boss でアイドル、TTL、再試行、保守を管理。

**Portal**：`/portal/...` の認証済みサーバー描画 UI。リース、ログ、イベント、VNC/Code。[ポータル](features/portal.md)。

**Bridge**：ローカル CLI リレーへの接続。WebVNC/Code は loopback VNC 5900/code-server 8080 へ SSH トンネルを保ち、利用中は起動が必要。Egress は別のチケット付きリレー。コーディネーター自身は SSH を開きません。

## ID {#identity}

**Owner**：所有者。GitHub は不変 `github:<numeric-id>` に結び付け、共有自動化は検証した設定または Git メールを使用。[運用セキュリティ](security.md#authentication)。

**Org**：署名トークンまたは `CRABBOX_ORG` の組織。使用量範囲とマルチテナント制限。

**Allowed org**：ログイントークン発行前に必要な組織所属。Worker ごとに設定。

**Admin token**：`GET /v1/pool`、管理ルート、全体一覧用の別権限。共有自動化 token より厳重に管理。

**Cloudflare Access**：任意の前段保護。検証済み `cf-access-jwt-assertion` を信頼し、生ヘッダーは信頼しません。

## 保存 {#storage}

**State directory**：claim、履歴、チェックポイント。`XDG_STATE_HOME` 指定なら `$XDG_STATE_HOME/crabbox` に鍵・ホスト信頼も統一。未指定なら `<os-user-config-dir>/crabbox/state`、鍵は `<os-user-config-dir>/crabbox/testboxes`。変更しても旧状態を移行・探索しません。[SSH 鍵](features/ssh-keys.md)。

**Claim**：リースとチェックアウトを結ぶ JSON。`crabbox run --id` の slug 解決と、`--reclaim` なしの他リポジトリ利用拒否に必要。

**Workdir** / **work root**：同期先。Linux は `/work/crabbox`、通常 Windows は `C:\crabbox`、ほかは提供元/対象ごと。

## 証跡と成果物 {#evidence-and-artifacts}

**Run history**：`crabbox history`、`logs`、`events`、`results` の過去記録。現在の状態とは別。[履歴](features/history-logs.md)。

**Test results**：`crabbox results <run-id>` の JUnit 集計。スイート、テスト、失敗、エラー、省略。[結果](features/test-results.md)。

**Telemetry**：Linux の負荷、メモリ、ディスク、稼働時間の軽量サンプル。製品分析ではなく実行時資源。[テレメトリー](features/telemetry.md)。

**Artifacts**：`crabbox artifacts` の画像、動画、GIF、ログ、状態、メタデータ。PR に公開可能。[成果物](features/artifacts.md)。

**Egress**：`crabbox egress` が WebSocket で運用者マシン経由の外向き通信を提供。[Egress](features/egress.md)。

**Checkpoint**：復元・fork 可能な VM/作業状態。`chk_...`、アーカイブ、レシピ、ネイティブスナップショット/イメージ。[チェックポイント](features/checkpoints.md)。

**Cache volume**：リポジトリ/ランタイム/OS ごとの永続キャッシュを絶対パスへ配置。速度専用で fork 可能シナリオではなく、作業ツリーが基準です。[キャッシュ](features/cache.md)。

**Capsule**：`crabbox capsule from-actions` で取得し `crabbox capsule
replay` で再現する持ち運べる失敗記録。[カプセル](features/capsules.md)。

## ドキュメント {#documentation}

**Source map**：動作と実装の対応。[ソースマップ](source-map.md)。

**Feature page**：`docs/features/<name>.md` の概念説明。コマンドや提供元から参照。

**Command page**：`docs/commands/<name>.md` にフラグ、動作、終了コードを記載し `--help` と同期。

**Provider page**：`docs/providers/<name>.md` に対象、設定、環境変数、同期、失敗を記載。

関連文書：

- [仕組み](how-it-works.md)
- [アーキテクチャ](architecture.md)
- [CLI](cli.md)
- [設定](features/configuration.md)
- [バックエンド](provider-backends.md)
