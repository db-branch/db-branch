# Branch のアップグレード（データベースのバージョンを上げる）

1 つの Branch だけを新しいバージョンのランタイムに上げ、動作を試し、必要なら元のバージョンに戻せる。Base と、ほかの Branch は何も変わらない。主な使い道は MySQL 8.0 → 8.4 の移行テストで、同じ Base から「8.0 のまま」の Branch と「8.4 に上げた」Branch を 1 手ずつで作って比べられる。

OSS（Apache-2.0）の機能で、有償モジュールは要らない。

## 1. 何ができるか

| 操作 | CLI | API | MCP |
|---|---|---|---|
| 上げる | `branch upgrade <Branch> --runtime <engine@version>` | `POST /v1/branches/{name}/upgrade` | `upgrade_branch` |
| 確認だけ | `branch upgrade <Branch> --runtime … --check-only` | 同上（`check_only: true`） | `check_upgrade` |
| 作って上げる | `branch create <name> --base <base> --runtime <engine@version>` | `POST /v1/branches`（`runtime`） | `create_branch`（`runtime`） |
| 戻す | `branch restore <Branch> before-upgrade-<時刻>` | Checkpoint の復元 | `restore_checkpoint` |

コンソールでは Branch のカードと詳細のエンジンの横に **Upgrade** ボタンがある。移行先のバージョンを選び、確認ダイアログ（"A checkpoint is saved first; on failure it rolls back automatically"）で進める。結果の画面に、事前確認の指摘とサーバーログの警告が並ぶ。

使えるのは Branch の所有者と管理者（Checkpoint の復元と同じ。`branch.upgrade` アクション）。ログインなしの呼び出しや、`open` の Branch は対象外。

## 2. 許される移行

| エンジン | 許す | 断る |
|---|---|---|
| MySQL | 5.6 → 5.7、5.7 → 8.0、8.0 → 8.4、8.4 → 9.x、同じシリーズ内のパッチ更新（8.4.9 → 8.4.13）、8.0 → 8.1〜8.3 のようなシリーズ内の前進 | シリーズ（5.7、8.0、8.4、9.7）を飛び越す移行（5.6 → 8.0、5.7 → 8.4 など。5.7、8.0 を経由する）（例: 8.0 → 9.x は「先に mysql@8.4.x へ」）、ダウングレード、同じバージョン、別のエンジン |
| PostgreSQL | 同じメジャー内のマイナー更新（17.2 → 17.5。バイナリの差し替えだけ） | メジャーの更新（`pg_upgrade`）は **not supported yet**、ダウングレード |

- **ダウングレードはない**。戻すときは、上げる前に自動で取った Checkpoint を復元する（§5）
- 移行先のランタイムは、その Branch のノードに入っていること。なければ `404 runtime_not_installed` で、`db-branch runtime install mysql@8.4.13` のように入れ方を添えて断る。クラスタでは Branch のあるノードへ転送して、そのノードで確かめる
- クラスタの全ノードがこの機能を知る版（プロトコル 16）になるまで、`503 cluster_upgrading` で断る（確認だけは使える）
- Branch が `ready` のときだけ。Checkpoint を使うので、ノード自身の ZFS が要る（確認だけは要らない）

## 3. 流れ

1. 検証して、Branch を `upgrading` にする
2. **事前確認**（サーバーは動いたまま。変更しない）。指摘は `{level: error|warning|info, code, message}` の一覧で返る。`error` があれば止まる（`--force` で進める）
3. サーバーを**きれいに停止**する（MySQL は `innodb_fast_shutdown=0`）。次に自動の Checkpoint `before-upgrade-<時刻>` を取る。これは古いランタイムを記録する自動の Checkpoint で、新しい 5 件を残し、`branch.max_checkpoints` に数えない
4. Branch のランタイムを切り替え、**新しいバイナリで起動**する。MySQL 8.0.16 以降は起動時にデータ辞書とシステムテーブルを自分で更新する（`upgrade=AUTO`）。5.7 へ上げるときは、起動のあとに `mysql_upgrade` を流してシステムテーブルを更新し、サーバーを再起動する（5.6 のシステムテーブルのままでは `account_locked` 列や `sys` スキーマがない）。待つ上限は 15 分
5. 起動中にエラーログへ出た警告・エラー・アップグレードの進行を集めて結果に入れる
6. 失敗したら（起動しない、アップグレードのエラー、時間切れ）**自動で Checkpoint を復元して古いランタイムで起動し直す**。結果にエラーログの抜粋が残る。`--no-rollback` なら失敗したまま残す（状態 `failed`。Checkpoint を復元するか Branch を削除する）

Checkpoint はきれいに停止した**あと**に取る。そうすると、復元して古いランタイムで起動し直すときにクラッシュリカバリが要らない。エージェントの再起動で中断した場合は、Checkpoint が取れていれば戻し、取れていなければ何も変えずに元の状態へ戻す。

結果（オペレーションの `result`。失敗したときも付く）:

```json
{ "branch": "x", "from": "mysql@8.0.46", "to": "mysql@8.4.13", "status": "upgraded",
  "checkpoint": "before-upgrade-20261113-120000", "check_only": false,
  "pre_check": [ { "level": "warning", "code": "native_password_users", "message": "1 account(s) use mysql_native_password, …" } ],
  "log": [ { "level": "warning", "code": "log_warning", "message": "MY-010068: CA certificate ca.pem is self signed." } ] }
```

`status` は `checked`（確認だけ）、`upgraded`、`rolled_back`（失敗して戻した）、`failed`（失敗したまま残した）、`refused`（事前確認のエラーで止めた）、`aborted`（何も変える前に失敗した）。

## 4. 事前確認

基本の確認だけを行う。

- **MySQL 5.x から**:
  - 5.6 → 5.7: 旧パスワード方式（`mysql_old_password`）のユーザーは `error`（5.7 で削除された。`SET PASSWORD FOR ... = PASSWORD('...')` で直す）。
  - 5.7 → 8.0: InnoDB 以外のエンジンでパーティションしているテーブルは `error`（8.0 は対応しない。`ALTER TABLE ... ENGINE = InnoDB` で直す）。
  - どちらも、移行先の `mysqld` に、db-branch が書く設定を読ませて確かめる（8.0 未満は `--validate-config` がないので、`--help --verbose` で読ませる）。
  - ここで見ているのは、上の2つと設定だけで、予約語の衝突や文字コードの違いなどは見ない。サーバーのエラーログの警告・エラーが、結果に入る。

- **`mysql_native_password` のユーザー**（8.4 以上へ）: 8.4 では既定で無効になり、9.0 で削除された。
  - 8.4 へ: `warning`。db-branch はこの Branch の設定に `mysql_native_password=ON` を足して、既存のユーザーがそのまま使えるようにする（Branch に記録され、再起動後も続く）。9.0 より前に `caching_sha2_password` へ移すよう案内する
  - 9.x へ: db-branch 自身の Branch ユーザーは起動後に同じパスワードのまま `caching_sha2_password` へ自動で直す（`info`）。ほかのユーザーが使っていれば `error`
- **my.cnf のオプション**: 新しいバージョンのランタイムに合う設定を生成する。db-branch が書くオプションと、モジュールが足すオプションのうち、新しいバージョンで消えたもの（`default_authentication_plugin`、`expire_logs_days`（`binlog_expire_logs_seconds` に書き換える）、`master_info_repository`、`relay_log_info_repository`、`transaction_write_set_extraction`、9.6 の `innodb_log_file_size` など）は落とすか書き換え、`info` で知らせる。さらに、**新しい `mysqld --validate-config`** に生成した設定を読ませ、受け付けなければ `error`（`config_invalid`）
- **空き容量の見積もり**: Branch のデータの 1/20 と 512 MiB ほどを目安に、プールの空きと比べる（足りなければ `error`、2 倍に満たなければ `warning`）。Checkpoint は取った時点では容量を使わない
- PostgreSQL: データディレクトリのメジャーが移行先と同じかを確かめる

## 5. 戻す・リセット・再作成

- **Checkpoint は取ったときのランタイムを記録する**（一覧の `runtime`）。復元すると Branch はそのランタイムに戻る。ランタイムが入っていなければ、`runtime install` を案内して断る。ランタイムを記録していない古い Checkpoint は Base のランタイムに戻す
- **リセット**は Base の状態と **Base のランタイム**に戻す
- ノード喪失で別のノードに作り直した Branch は、Base のスナップショットから作り直すので Base のランタイムで動く
- Branch のランタイムは Raft の Branch の記録に入るので、エージェントの再起動・ノードの再起動・フェイルオーバーのあとも保たれる。`branch show` と `branch list` に出る（`show` は Base と違うときに Base のランタイムも添える）。API の Branch の `runtime` と `base_runtime`、コンソール、MCP も同じ
- 使われているランタイムを消さないチェック（`runtime remove`）は Base と Branch を見る。Checkpoint だけが使っているランタイムは守らない。消してしまった場合、その Checkpoint の復元は入れ方を案内して断る

## 6. 履歴モジュール（有償）との関係

- 記録している Branch のバイナリログは、上げたあとも続く。履歴モジュールは新しいランタイムのバージョンに合うオプションを足す（9.x では `binlog_format` を足さない）
- 履歴モジュールの時点復元は、復元元の Checkpoint を復元してから、**そのときのランタイム**のクライアント（`mysqlbinlog` と `mysql`）でログを再生する。Checkpoint の復元がランタイムを切り替えてから再生を始めるため

## 7. ProxySQL

ProxySQL が接続元に見せるサーバーバージョン（`mysql-server_version`）は変えない。上げた Branch でも、ProxySQL 経由の接続は今までの値を見せる。実際のバージョンは `SELECT VERSION()` を Branch へ直接つないで確かめるか、`branch show` を見る。

## 8. 制限

- PostgreSQL のメジャーアップグレードは未対応
- ダウングレードはない（Checkpoint の復元で戻す）
- 事前確認は基本だけ（`mysqlsh` の `checkForServerUpgrade` ほどは調べない）。テーブルの照合順序や廃止された機能の利用は調べない
