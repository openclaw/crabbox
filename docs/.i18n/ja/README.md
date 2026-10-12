<!-- source: README.md; sha256: 008297f255e89d26769a8517dd1c425b15462564563cce18859b56b7640a570e -->
# Crabbox {#crabbox}

**エージェントのためのオンデマンドコンピューター。**

## Crabbox とは {#what-crabbox-is}

Crabbox はエージェントと人に、テストやビルド用コンピューターを提供します。ローカルで編集を続けながら、未コミットの変更を含む作業ツリーをリモートで実行できます。出力をストリーミングし、終了コードを返し、プロバイダーの契約に従ってリースを解放します。

プロバイダーを設定したら、いつものテストコマンドを実行します。

```sh
crabbox run -- pnpm test
```

## ひとつのサイクル、多様なマシン {#one-loop-many-kinds-of-box}

標準的なリモート実行の流れです。

1. 既存のチェックアウトを**ローカルで編集**します。
2. **マシンを借りる**か、準備済みの容量を再利用します。
3. 未コミットの変更も含めて作業ツリーを**同期**します。
4. マシンでコマンドを**実行**します。
5. 呼び出し元に**出力と終了コードを返し**ます。
6. 所有する容量を**解放**するか、次回に備えて保持します。

委譲型プロバイダーは実行と同期を担当できます。再利用、証跡、後片付けは、選択したプロバイダーの機能と実行ポリシーに従います。

基盤が変わっても、作業フローをプロバイダー専用スクリプトにする必要はありません。仕事は決まっているが実行先は未定なら[ユースケース](use-cases.md)、実行先が決まっていれば[プロバイダーリファレンス](providers/README.md)から始めてください。

## エージェントを増やし、待ち時間を減らす {#more-agents-less-waiting}

- **エージェントを並列化：**別々のマシンでテストとビルドを動かし、ノート PC の CPU、RAM、ポートの競合を防ぎます。
- **ウォームマシン：**準備済み環境を再利用し、反復時間を短くします。
- **クロスプラットフォーム：**同じチェックアウトから Linux、macOS、ネイティブ Windows、WSL2 を対象にします。対応範囲はプロバイダー次第です。
- **見て操作する：**対応デスクトップで [VNC や WebVNC](features/interactive-desktop-vnc.md)、ブラウザー、管理対象 Linux では `--code` によるブラウザー版 VS Code を使えます。認証付き[ポータル](features/portal.md)にはリースとログが表示され、ローカルブリッジ稼働中は WebVNC で閲覧や操作の引き継ぎができます。
- **チーム容量を共有：**任意のコーディネーターがクラウド認証情報、リース共有、有効期限、設定した費用上限を管理します。

[プロバイダーカタログ](providers/README.md)はクラウド、ローカル VM、SSH ホスト、委譲型サンドボックスを扱います。[外部プラグイン契約](providers/external.md)で独自バックエンドを接続できます。サイトのプロバイダー一覧と分類件数はカタログメタデータから生成します。

## ここから始める {#start-here}

- [はじめに](getting-started.md) — CLI のインストールと最初のリモート実行。
- [ユースケース](use-cases.md) — 高速反復、エージェント、OS 検証、ブラウザー QA、並列展開、GPU。
- [料金とコスト](pricing.md) — オープンソースと持ち込み計算資源の費用モデルと制限。
- [Crabbox の仕組み](how-it-works.md) — CLI、コーディネーター、ランナーを通る実行。
- [Crabbox のビジョン](../VISION.md) — 製品の範囲、対象外、安全なライフサイクル。

## 運用モデルを選ぶ {#pick-an-operating-model}

| 経路 | 用途 | 所有者 |
| --- | --- | --- |
| ローカルランタイム | 認証不要の高速な開発チェック | 自分の端末とランタイム |
| 直接クラウドまたは SSH | 個人アカウント、専用ホスト、自前仮想化 | 自分のアカウントやインフラ |
| チームコーディネーター | 認証、リース、清掃、使用量、費用上限の共有 | 自分の Cloudflare または Node.js/PostgreSQL |
| 委譲実行 | プロバイダー主導のエージェント、CI、ブラウザー、GPU | 選択したプロバイダーまたは運用者 |

Crabbox は MIT ライセンスです。現在ホスト型制御プレーンのプランはなく、計算資源とコーディネーターのインフラは各運用者が課金します。境界は[料金とコスト](pricing.md)を参照してください。

## 信頼の境界 {#trust-boundary}

信頼できる仲間と信頼できるリポジトリを実行してください。隔離はランタイムに依存します。再利用と破壊的な後片付けには、対象リソースと claim の所有権確認が必要です。不明な場合は処理を拒否します。

[プロバイダー選択](features/provider-selection.md)で振り分け、プロバイダーの文書と[セキュリティモデル](security.md)を読んでください。推奨は作業の案内であり、セキュリティ認証ではありません。

## さらに詳しく {#go-deeper}

- **CLI と設定：**[CLI](cli.md)、[コマンド](commands/README.md)、[設定](features/configuration.md)、[リポジトリ導入](features/repository-onboarding.md)。
- **フリートと運用：**[アーキテクチャ](architecture.md)、[インフラ](infrastructure.md)、[運用](operations.md)、[観測](observability.md)、[読み取り専用デバイス連携](features/device-pairing.md)、[セキュリティ](security.md)。
- **実行と証跡：**[ジョブ](features/jobs.md)、[Actions による環境準備](features/actions-hydration.md)、[成果物](features/artifacts.md)、[チェックポイント](features/checkpoints.md)、[対話デスクトップと VNC](features/interactive-desktop-vnc.md)。
- **プラットフォーム境界：**[ネストした実行](features/nested-execution.md)では WSL2、コンテナー、準備済み KVM ホスト、ローカルサンドボックスを区別します。
- **拡張：**[連携一覧](integrations/README.md)、[プロバイダー開発](features/provider-authoring.md)、[ソースマップ](source-map.md)。

## このドキュメントについて {#about-these-docs}

`docs/` の Markdown が [crabbox.sh](https://crabbox.sh/) の原稿です。実装の基準はコードにあり、[ソースマップ](source-map.md)で担当箇所を確認できます。

ローカルでサイトを構築・検証します。

```sh
scripts/check-docs.sh
open dist/docs-site/index.html
```

`docs/` ではレベル 1–4 の見出しとバッククォート 3 つのコードフェンスを使用します。サイト内の見出しリンクはレンダラーと同じ ID で検証され、例やコメントはアンカーを予約しません。公開済み ID を維持してください。リポジトリ専用 Markdown には従来の GitHub 風アンカー規則を使い、GitHub Markdown 全体への対応を保証するものではありません。

言語の追加・更新は[翻訳ガイド](translations.md)を参照してください。
