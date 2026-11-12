# db-branch 開発ガイド

設計は [docs/design.md](docs/design.md) を参照。

## 開発プロセス: t-wada 式 TDD

Red → Green → Refactor のサイクルを厳守する。

1. **Red** — 失敗するテストを 1 つだけ書く
2. **Green** — そのテストを通す最小限のコードを書く
3. **Refactor** — テストがグリーンのまま、重複・命名・構造を整理する（テストコードも対象）

- テストより先にプロダクションコードを書かない
- テスト名は「何を達成するか」を表現する
- **カバレッジは常に 95% 以上**（`make cover` で確認。下回るパッケージがあれば先にテストを足す）
  - 例外: `internal/app`（配線だけを行うコンポジションルート。`cmd/` と同じ扱い）は対象外とし、`make integration`（実機の ZFS・systemd・mysqld を使う結合テスト）で確認する

## アーキテクチャ: クリーンアーキテクチャ

依存は常に外側から内側へ。

```
internal/
├── domain/          エンティティ・値オブジェクト・ポート。外部依存ゼロ
├── usecase/         ユースケース。domain のみに依存
├── interface/       CLI・HTTP・Web コンソールなどの入出力アダプタ
└── infrastructure/  ZFS・DB エンジン・ProxySQL・Raft・設定ファイルなどの外部実装
internal/app/        コンポジションルート（実装を組み立てる。ロジックを置かない）
pkg/ext/             有償モジュール向けの公開拡張ポイント（後のフェーズ）
web/                 Web コンソール（React + Vite。ビルド結果をバイナリに埋め込む）
cmd/db-branch/       エントリポイント
```

- `domain` は外部ライブラリに依存しない
- `usecase` はインフラの具体型を import しない。必ずポートを介する
- 副作用（ZFS・プロセス起動・ネットワーク）は `infrastructure` に閉じ込める
- テストで差し替えられないコードはアーキテクチャ違反のサイン

## クラスタの互換性

- Raft のコマンド（`internal/infrastructure/raftstore/fsm.go`）を足す・意味を変えるときは、`Protocol` を 1 上げ、`opProtocol` に必要なプロトコルを登録する。全ノードが新版になるまで、そのコマンドはログに書かれない
- 状態に足すフィールドは、古いノードが読み飛ばしても壊れないようにする（古いノードは知らないフィールドを落とす）

## パッケージング

- `apt install db-branch` で入ることがゴール。FHS に従う（`/usr/bin`、`/etc/db-branch`、`/var/lib/db-branch`）
- パッケージのインストール時にディスクやネットワークに触れない
- 変更したら `make deb` で .deb を作り、検証 VM にインストールして確認する

## よく使うコマンド

```
make test    # テスト
make cover   # カバレッジ（95% 未満で失敗）
make build   # bin/db-branch
make deb     # dist/*.deb
```
