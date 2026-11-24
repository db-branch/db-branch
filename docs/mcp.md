# MCP（LLM エージェントからブランチを使う）

開発者の LLM エージェント（Claude Code など）が、自分でブランチを発行し、その中で SQL を実行して検証できるようにする。各ノードのコンソールと同じ HTTPS ポートで MCP サーバー（Streamable HTTP、ステートレス）を提供する。

- エンドポイント: `https://<どのノードでも>:7080/v1/mcp`
- 認証: API トークン（`Authorization: Bearer dbb_...`）。秘密は発行時に 1 度だけ表示し、Raft にはハッシュだけを置く。トークンは 2 種類:
  - **個人用**（コンソールの Account → MCP access、`/v1/account/tokens`）: 作った本人として動く（`admin` 以外のロールも保持し、`admin` ロールは引き継がない）。作ったブランチは本人のもの、できることは本人の権限の範囲内のブランチ操作だけ（`branches` スコープ固定）。本人を無効化すると即座に拒否され、`auth` モジュールから削除すると個人トークンもすべて失効・削除される。トークンから別のトークンは作れない
  - **サービス用**（Settings → Service tokens、`db-branch token create <名前> [--scope branches|full] [--ttl 720h]`、管理者のみ）: CI や外部連携向け。人に結び付かず、トークン自身として動く
  - `branches` スコープのトークンはホワイトリスト方式で認可され、ブランチ操作・ベース一覧/公開ベース・ランタイム一覧・オペレーション・イベント・機能情報・メトリクス・自身のトークン/設定参照・MCP のみを許可する。拡張モジュール（`ext.<module>.*`）、ライセンス（`license.*`）、クラスタ構成（`cluster.*`）、モジュール/更新管理（`module.*`, `update.*`）、設定・トークン・ルートユーザー・バックアップ・ベース定期更新などはすべて `403 forbidden` で拒否される
  - 期限切れのトークンは定期処理（GC）で自動的に削除される
- 範囲（scope）: `branches`（既定）はブランチの作成・利用・削除とホワイトリスト内の限定読み取りだけ。ベースやランタイム、設定、トークンは変えられない。`full` は admin と同じ
- ツールの呼び出しは API と同じ経路（認可・監査）を通る。監査ログの利用者は `token:<id>`。`POST /v1/mcp` 自体も `mcp.use` として認可・監査する（拒否も記録）。本文は 1 MiB まで（超えると `413 too_large`）

## セキュリティ上の注意（接続情報の返却）

`create_branch` と `get_branch` は、パスワード入りの接続文字列（`dsn`）を返す。これは LLM のコンテキスト（会話履歴、ログ、ツールの記録）に平文で残りうる。次の点に注意する。

- パスワードはそのブランチ専用で、ブランチを削除すれば無効になる。本番のパスワードではない
- 短い `ttl` を付け、機密を含む環境では、ブランチを `open`（共有パスワード）にしない
- MCP のトークンは `branches` スコープで足りる。LLM の記録が外部に残る環境では、`dsn` を返す呼び出しの記録を見直す（監査ログには `dsn` を取り出した呼び出しが残る）

## セキュリティ上の注意（データマスキング）

ブランチは本番環境への破壊的な書き込み（Write）を防ぐが、未マスクの本番データをそのままベースに渡すと、AI エージェントが `query` 等で読み取り（Read）を行うことによる機密情報アクセスリスクが残る。機密データを含む環境では、ブランチ作成後（`init_sql` 等）ではなく、**ベーススナップショットを作成する段階でマスク済みのデータを用意する**ことを推奨する。

## ツール

| ツール | 内容 |
|---|---|
| `list_bases` | ベースとスナップショット |
| `list_branches` | ブランチの一覧 |
| `create_branch` | ベースからブランチを作り（完了まで待つ）、接続先（dsn）を返す。`ttl`・`init_sql`・`snapshot`・`access`・`record`・`runtime` を指定できる |
| `get_branch` | ブランチと接続先（引数 `branch`、旧 `name` も互換受理） |
| `query` | ブランチで SQL を 1 文実行（`database` で接続先 DB 指定可）。行（文字列）か影響行数を返す。最大 1000 行、30 秒 |
| `create_checkpoint` | 稼働中のブランチの今の状態をチェックポイントとして保存する（一瞬。`name` は省略可）。危ない作業の前に取る |
| `list_checkpoints` | ブランチのチェックポイント（時刻・容量・`current`・`auto`） |
| `restore_checkpoint` | チェックポイントの状態に戻す（完了まで待つ）。今の状態は置き換わり、最後のチェックポイントより後の変更は復元で失われる（残したい場合は先に `create_checkpoint` を呼ぶ）。取ってあるチェックポイントはすべて残る |
| `fork_branch` | 既存のブランチ（現在の状態またはチェックポイント）から独立した新しいブランチをフォークし（完了まで待つ）、接続先（dsn）を返す。[branch-fork.md](branch-fork.md) |
| `merge_line` | 同一ブランチ内で `restore_checkpoint` により分岐した履歴ライン（`source_line` または `checkpoint`）の変更を現在のライブラインへ 3-way マージする（`dry_run: true` で事前検証可、コンフリクト時はライブを変更せず失敗）。[branch-merge.md](branch-merge.md) |
| `check_upgrade` | ブランチを新しいバージョンのランタイムに上げると何に当たるかを調べる（何も変えない）。指摘は `error` `warning` `info` |
| `upgrade_branch` | そのブランチだけを新しいバージョンに上げる（完了まで待つ）。先に Checkpoint を取り、失敗したら自動で戻る。結果に事前確認の指摘とサーバーログの警告が付く。`create_branch` の `runtime` で、作ってすぐ上げることもできる。[upgrade.md](upgrade.md) |
| `reset_branch` | 作成直後（init SQL の後）に戻す（引数 `branch`、旧 `name` も互換受理）。チェックポイントは残る |
| `extend_branch` | 期限を延ばす（引数 `branch` と `ttl`、旧 `name` も互換受理） |
| `delete_branch` | 削除する（引数 `branch`、旧 `name` も互換受理） |

## つなぎ方（Claude Code）

```sh
curl -sk https://<node>:7080/tls.pem -o db-branch.pem     # ノードの自己署名証明書
export NODE_EXTRA_CA_CERTS=$PWD/db-branch.pem
claude mcp add --transport http db-branch https://<node>:7080/v1/mcp --header "Authorization: Bearer <token>"
```

## 確認（2026-11-10）

node1 でトークンを発行し、node3 の MCP に curl で JSON-RPC を送った。create_branch（init SQL でマスキング）→ ブランチは node2 に配置 → query でマスク済みの行を取得 → DELETE で 2 行 → reset_branch で 2 行に戻る → delete_branch。branches のトークンでベースを消そうとすると 403、トークンなしは 401。
