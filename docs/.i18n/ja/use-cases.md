<!-- source: use-cases.md; sha256: 769c5e58a1b095392245d30f0377bf87b025fa28c3eeca64f9337340d43cd3d7 -->
# ユースケース {#use-cases}

コーディングエージェントごとに、テストとビルド用のオンデマンドコンピューターを用意します。エージェントがローカルのチェックアウトを編集し続ける間、Crabbox は未コミットの変更を同期し、リモートでコマンドを実行して出力と終了コードを返します。個別のリモートリースにより、並列作業がノート PC の CPU、メモリ、ポートを奪い合うのを防げます。

プラットフォーム固有の動作を確認するには Linux、macOS、ネイティブ Windows、WSL2 を選びます。繰り返し実行にはウォーム状態のマシンを再利用し、結果を確認するにはデスクトップやブラウザーのエディターを開けます。対話アクセスと引き継ぎは[デスクトップ、VNC、ブラウザー、Code、ポータルのガイド](features/interactive-desktop-vnc.md)を参照してください。

ベンダーではなく、ワークロードから選びます。

```sh
crabbox providers recommend
crabbox providers recommend fast-feedback
crabbox providers recommend agent-sandbox --json
```

用途を省略すると、`crabbox providers recommend` は対応する推奨名をすべて表示します。推奨はリポジトリ内の機能メタデータに基づきます。プロバイダーへの接続、認証情報やクォータの確認、起動時間の測定、安全性の認証は行いません。

## テストとビルドの反復を高速化する {#speed-up-test-and-build-loops}

ローカルでは遅い、メモリを消費しすぎる、ほかの作業を妨げるコマンドに使います。

```sh
crabbox providers recommend fast-feedback
crabbox providers recommend linux-vm
crabbox run --provider hetzner -- pnpm test
```

ローカルランタイム、再利用可能なキャッシュ、短い編集・実行サイクルには `fast-feedback` を優先します。処理をワークステーションから移す場合や、完全な VM と SSH の動作が必要なら `linux-vm` を優先します。

## コーディングエージェントに使い捨て環境を用意する {#give-a-coding-agent-a-disposable-environment}

ワークステーション外でリポジトリのコマンドを実行するには、リモート SSH リースか委譲型サンドボックスを使います。ローカルサンドボックスは作業領域を分離しますが、PC のリソースを使います。Crabbox は実行と後片付けを担当し、呼び出すエージェントやハーネスがオーケストレーションを担当します。

独立したエージェントには、それぞれのチェックアウトから個別のリースを作成します。

```sh
crabbox warmup --provider hetzner --class standard --slug agent-tests
crabbox run --id agent-tests -- pnpm test
crabbox stop agent-tests
```

リースごとに異なる slug を使います。ウォームリースはコマンド間で環境を保持します。テスト前にリポジトリ設定を確認し、ツールと依存関係をインストールしてください。

```sh
crabbox providers recommend agent-sandbox
crabbox providers recommend disposable-execution
crabbox providers recommend isolated-execution
```

この 3 つは異なる目的の推奨です。

- `agent-sandbox` はエージェントや開発環境向けのプロバイダーを優先します。
- `disposable-execution` は後片付け可能な一時ランタイムを必要とします。
- `isolated-execution` は委譲型およびローカルのサンドボックス境界を優先します。

最後の推奨はルーティングの目安であり、安全性の認証ではありません。Crabbox は運用者、リポジトリ設定、ワークロードが信頼できることを前提とします。使い捨てマシンでも悪意あるコードは安全になりません。シークレットを渡す前に、プロバイダーの信頼・ネットワークモデルを確認してください。

## Linux、Windows、WSL2、macOS を検証する {#validate-linux-windows-wsl2-or-macos}

動作が対象 OS に依存する場合は、プラットフォーム別の推奨を使います。

```sh
crabbox providers recommend linux-vm
crabbox providers recommend windows
crabbox providers recommend macos

crabbox run --provider aws --target windows --windows-mode wsl2 -- uname -a
```

ネイティブ Windows は PowerShell と Windows ツールを使います。WSL2 はネスト対応 Windows VM 内で POSIX 契約を実行します。macOS の対象はプロバイダーに応じてローカルの Apple Silicon VM またはクラウドの Mac です。

## ブラウザーや画面付きデスクトップをテストする {#test-a-browser-or-visible-desktop}

ブラウザー、スクリーンショット、入力、チームメンバーへの引き継ぎが必要なら、デスクトップ対応の SSH リースを使います。

```sh
crabbox providers recommend desktop
crabbox warmup --provider azure --target windows --windows-mode normal --desktop --slug ui-check
crabbox webvnc --id ui-check --open --take-control
```

WebVNC では許可されたメンバーがデスクトップを見たり操作を引き継いだりできます。ネイティブ VNC は SSH トンネルを使い、`crabbox code` は対応 Linux リースで code-server を開きます。コーディネーターのポータルにはリース、ブリッジの状態、保存済みの実行証跡が表示されます。ブラウザー使用中はローカルブリッジを動かし続けてください。前提条件は[統合アクセスガイド](features/interactive-desktop-vnc.md)を参照してください。

マシン解放後も結果を残すには、証跡用ルートも選びます。

```sh
crabbox providers recommend run-evidence
crabbox providers recommend failure-diagnostics
```

## ウォーム状態を再利用する {#reuse-warm-state}

セットアップの繰り返しより、準備済みの状態を保持・復元する方が安い場合に使います。

```sh
crabbox providers recommend warm-start
crabbox providers recommend versioned-workspace

crabbox warmup --slug warm-tests
crabbox run --id warm-tests -- pnpm test:changed
```

`warm-start` はローカルランタイム、キャッシュボリューム、保持セッション、一時停止・再開、ワークスペース状態を考慮します。すべてのプロバイダーで同じスナップショットやウォームプール機構が使えるという保証ではありません。

## 実験を並列に展開する {#fan-out-parallel-experiments}

並列ブランチ、エージェントの複数候補、準備済み状態から始めるテスト分割には、fork 対応ワークスペースを使います。

```sh
crabbox providers recommend fanout-testing
crabbox checkpoint fork <checkpoint-id> --count 4
```

これはプロバイダーに依存しないチェックポイントと fork のルーティングです。microVM の実行中メモリの fork を保証しません。

## GPU ワークロードを実行する {#run-gpu-workloads}

モデルテスト、CUDA ビルド、レンダリングなどのアクセラレーター処理には GPU 推奨を使います。

```sh
crabbox providers recommend gpu
crabbox run --provider runpod -- nvidia-smi
```

対話的なデバッグには通常 SSH リース型 GPU が適しています。プロバイダーに実行と結果収集を任せたい場合は委譲型 GPU が適することが多いです。

## クラウド認証情報なしでローカル実行する {#run-locally-without-cloud-credentials}

ローカルコンテナー、VM、ポリシーサンドボックスで、ワークステーション上の簡単なスモークテストを実行します。

```sh
crabbox providers recommend local
crabbox providers recommend offline-validation
crabbox run --provider local-container -- pnpm test
```

ローカルでも隔離モデルは同じではありません。Docker 互換コンテナー、Apple VM、Multipass VM、Windows Sandbox、ローカルポリシーランタイムでは、ホストアクセス、永続性、後片付けの契約が異なります。未知のコードを動かす前にプロバイダーのページを読んでください。

## 既存のインフラを使う {#use-infrastructure-you-already-own}

共有コーディネーターなしで、常設 SSH ホスト、プライベート仮想化基盤、直接のクラウドアカウントを使えます。

```sh
crabbox providers recommend byo-ssh
crabbox providers recommend self-hosted
crabbox providers recommend linux-vm
```

Crabbox がライフサイクルを所有してはいけないホストには `ssh` を使います。ランタイムの作成と後片付けを任せる場合は Proxmox、XCP-ng、Incus、KubeVirt、Firecracker などを使います。

## 管理されたチームのフリートを共有する {#share-a-governed-team-fleet}

認証情報、リース所有権、後片付け、使用記録、同時リース数、月額上限を共有するにはコーディネーターを使います。

```sh
crabbox providers recommend team-cloud
crabbox login --url https://broker.example.com
crabbox usage --scope user
```

コーディネーターは制御プレーンです。CLI は通常の SSH ランナーに直接接続して同期・実行し、コーディネーターには証跡を記録します。`/portal` でリース一覧、デスクトップ、エディター、実行詳細を確認できます。直接プロバイダーの登録は、認証情報や後片付けの所有権を移さずにポータルへ表示を追加します。

## より具体的なルートが必要ですか？ {#need-a-more-specific-route}

CLI は成果物、CI 証明、コードインタープリター、障害診断、MCP 接続、ネットワーク制限、一時停止・再開、プレビュー URL、到達性、リモート開発、リソース監視、再利用セッション、Web アプリのスモークテスト、Worker/モジュール実行にも対応する推奨を提供します。

現在の一覧、別名、フィルター、スコア、注意点は [`crabbox providers recommend`](commands/providers.md#providers-recommend) を参照してください。

## 関連ドキュメント {#related-docs}

- [プロバイダーの選択](features/provider-selection.md)
- [プロバイダーリファレンス](providers/README.md)
- [ネストした実行](features/nested-execution.md)
- [料金とコスト](pricing.md)
