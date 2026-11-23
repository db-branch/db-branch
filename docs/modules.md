# 拡張モジュールの作り方

有償モジュール（`auth`・`audit` など）や独自の拡張は、db-branch 本体を変えずに別のバイナリとして追加する。公開 API は `pkg/ext`（ポート）と `pkg/ext/plugin`（プロセス間通信）。

## ポート

| ポート | 呼ばれるとき | OSS の既定 |
|---|---|---|
| `ext.Authenticator` | コンソールのログイン（ユーザー名とパスワード）、API の `Authorization: Bearer <token>` | admin 1 人のパスワード（トークンは扱わない） |
| `ext.Authorizer` | すべての API 呼び出しの前（`branch.create`、`branch.read`、`base.delete` などのアクション名と対象） | ログインしていれば許可 |
| `ext.AuditSink` | 変更を伴う呼び出し・拒否・ログインの後 | agent の構造化ログに 1 行 |
| `ext.Router` | 振り分けの設定（ブランチごとの hostgroup・ユーザー・コメントの規則）が変わったとき | なし（振り分けをしない）。`db-branch-proxysql` モジュールが ProxySQL を設定する |

- 認証は「モジュール → admin パスワード」の順に試す。扱わない資格情報には `ext.ErrUnauthenticated` を返す
- 認可は全モジュールが許可したときだけ通る。拒否は `ext.ErrForbidden` を包んだエラー
- 監査は全モジュールに届く。既定では失敗しても API は止めず、agent のログに残す（設定 `audit.fail_closed` で、記録できない変更を拒否できる）
- 振り分けは最初に見つかった Router だけを使う（ノードあたりプロキシは 1 つ）。`Module.Ports`（`mysql`・`postgres`）が接続先 URL のポートになる
- ノード間で転送された要求は、最初に受けたノードで認可・監査し、転送先では元の利用者として認可だけを行う（二重に記録しない）
- ローカルソケット（root と db-branch-admin グループ）からの呼び出しの利用者は `local`

アクション名の一覧は `internal/interface/httpapi/server.go` の `routes`。

## 書き方

```go
package main

import (
	"github.com/db-branch/db-branch/pkg/ext"
	"github.com/db-branch/db-branch/pkg/ext/plugin"
)

func main() {
	plugin.Serve(ext.Module{Name: "audit-file", Version: "0.1.0", AuditSink: mySink{}})
}
```

例: `examples/module-audit-file`（監査イベントを JSON Lines で書き出す）、`cmd/db-branch-proxysql`（ProxySQL を設定する Router）。

## クラスタ全体への導入（2026-11-10）

- `db-branch module install <名前>`（またはコンソールの Settings → Modules → Enable）で、パッケージ `db-branch-<名前>` を全ノードに入れる。版は実行したノードで apt が入れられる最新版（`--version` で指定可）を Raft に記録し、全ノードがその版に揃える。後から参加したノードにも入る
- 各ノードの agent は記録と手元を 30 秒ごと（記録が変わったときはすぐ）に照合し、`apt-get install/remove` を `systemd-run` の一時ユニットで実行する（パッケージの postinst が agent を再起動するため、apt を agent の子プロセスにしない）。エージェント内の `apt-get`（`install`・`remove`・`upgrade`・`update`）はすべて直列化し、同時に複数の `apt-get` を走らせない（外部の `apt` / `dpkg` ロックも `DPkg::Lock::Timeout=60` で待つ）
- **導入済みが記録より新しい場合（ダウングレードしない）**: `apt upgrade` などでコアと一緒にモジュールが新しい版へ上がったとき、手元の版がクラスターの記録より新しければダウングレードは行わない。1 台だけ先に上げた途中状態で他ノードへ旧コア非対応の版を強制したり、W-18 のローリング更新（キャンセル後の状態を含む）や Raft の互換性を壊したりしないためである。警告ログ、`module.version_ahead` イベント（SSE `/v1/events` および通知）、`GET /v1/modules` の `problem`、`dbbranch_module_problem` メトリクスで知らせる。全ノードを上げ終えたあと、`db-branch module install <名前>`（または `--version <版>`）でクラスターの記録を揃えると解消する
- **失敗時の指数バックオフと観測**: パッケージのインストール・削除が失敗したときは指数バックオフ（1 分 → 2 分 → 4 分 → …、上限 30 分）で間隔を延ばし、クラスターのモジュール記録が変わったときだけ即座にリセットする。連続して失敗する（2 回以上）と、`module.sync_failed` イベント（SSE・通知）、`GET /v1/modules` の `problem`、`dbbranch_module_problem` メトリクスに表れる（復旧すると消える）
- `db-branch module remove <名前>`（Disable）で全ノードから削除する
- 削除済みの記録を一覧から消すには `db-branch module forget <名前>`（API: `POST /v1/modules/{name}/forget`、アクション名 `module.forget`、Raft プロトコル 22）
- 記録されたモジュールを読み込めていないノードは、コンソールと API の操作を `503 modules_pending` で断る（認証・認可のモジュールが一部のノードにだけない状態を作らないため）。状態の確認・ログイン・モジュールと設定の操作・ローカルソケットからの操作は通す
- 以前から手で入れていたモジュールのパッケージは、起動時にクラスタのものとして記録する
- 各ノードに db-branch の APT リポジトリが設定されていること（docs/distribution.md）

## 設定

- モジュールは `Module.Settings`（項目の定義：キー・表示名・説明・型 string/int/bool/duration・既定値）と `Configurable` を持てる。値はクラスタで 1 つ（Raft）で、コンソールの Settings 画面に項目が出る。起動時と変更時に全ノードのモジュールへ `Configure` で渡る
- proxysql モジュールの設定: コメントによる振り分けの有効・無効、クライアントに見せる MySQL の版

## 配置

- バイナリを `/usr/lib/db-branch/modules/` に置き、agent を再起動する。root 所有で、グループやその他が書き換えられないこと（そうでなければ読み込まない）
- agent は起動時にディレクトリ内の実行ファイルを名前順に起動し、ハンドシェイク（`plugin.Handshake`）に応えたものだけを使う
- 読み込んだモジュールは `GET /v1/capabilities` の `modules` に出る
- 通信は hashicorp/go-plugin の net/rpc。design.md §12 では gRPC としていたが、protobuf の生成なしで済む net/rpc から始めた。ポートの形は同じなので、他言語のモジュールが必要になったら gRPC を足す

## 未実装

- ライセンスキー（ECDSA P-256 署名のオフライン検証。pkg/ext/license）は有償モジュール側で行う
- ProxySQL の接続記録を AuditSink に流す処理

## モジュールの API・画面・クラスタ共通データ（プロトコル 9）

- **API:** `ext.Module.Handler` を持つモジュールは `/v1/ext/<モジュール>/...` で自前の API を出せる。GET は `ext.<モジュール>.read`、それ以外は `ext.<モジュール>.write` として認可し、書き込みは監査する。`?node=<ノード>` を付けると、そのノードのモジュールが答える（ノードごとのファイルを読むときなど）
- **画面:** `Pages` に挙げたページ（HTML、スクリプトとスタイルは別ファイル）は、コンソールのナビに並び、枠の中に表示される（`/m/<モジュール><パス>`）。書き込みには `GET /v1/auth/session` の `csrf_token` を `X-CSRF-Token` に付ける
- **クラスタ共通データ:** `Starter.Start` で受け取る `Host.Store` に、ユーザーや方針などの小さな記録を置ける（Raft で全ノードに複製。キー 256 バイト・値 64 KiB・1 モジュール 1 万件・8 MiB まで）。ログのような大量のデータは `Host.DataDir`（ノードごと）に置く
- **ライセンス:** 有償モジュールは `Configure` で受け取る `ext.LicenseKey` を `pkg/ext/license` で確かめる。エラーを返すとコンソールのモジュール欄に表示される

## ブランチの操作・サーバー設定・復元後の処理（history モジュール向け）

ブランチの変更を記録し、任意の時点へ戻す有償モジュール `history`（`db-branch-modules` の `docs/history.md`）のために足した拡張ポイント。どれも既存のモジュールには影響しない（追加のみ。古いモジュールは何も変わらず動き、新しいモジュールが古い agent で動くときは `Host.Branches` が nil になる）。

| 拡張点 | 内容 |
|---|---|
| `ext.ServerOptioner`（`Module.ServerOptioner`） | ブランチのサーバーの設定ファイルに行を足す。`ServerOptions(ctx, ServerBranch) (ServerOptions{Config []string}, error)`。agent がブランチのサーバー設定を書くたび（作成・再作成・再起動・復元）に呼ぶ。MySQL は `my.cnf` の `[mysqld]` に足す。PostgreSQL は未対応（無視） |
| `ext.RestoreFinisher`（`Module.RestoreFinisher`） | 復元（チェックポイントの復元・リセット）の直後、サーバーが**保守モード**（`skip-networking`。誰も接続できない）で動いている間に呼ぶ。`AfterRestore(ctx, ServerBranch, note)`。モジュールが復元を頼んだときに渡した `note` がある復元でだけ呼ぶ。中断後はやり直すので、冪等に書く。失敗しても復元は戻さない（チェックポイントのデータのまま起動し、agent のログに残る） |
| `ext.Host.Branches`（`ext.BranchHost`） | この**ノードの**ブランチを扱う口: `Branch` / `Branches` / `Graph`（チェックポイントと線の構造）/ `CreateCheckpoint` / `RestoreCheckpoint(name, checkpoint, note)`（完了まで待つ。`@reset` は作成時の状態。どちらも今の状態を自動では保存しない（モジュールが必要なら、先に `CreateCheckpoint` で取る））/ `DB`（管理者としてサーバーに入る口: ソケットとクライアントツール）/ `OpenLine`・`CloseLine`（ある線、またはあるチェックポイント（`@reset` は作成時の状態）の時点のデータを**読み取り専用**でマウント）/ `StartOperation`・`ReportOperation`・`EndOperation`（モジュールの操作をコンソールや `GET /v1/operations/{id}` に出す。1 時間で閉じなければ失敗） |
| `/v1/ext/<モジュール>/branches/<名前>/...` | この形のパスは、そのブランチを持つノードのモジュールに自動で転送される（`?node=` を付けなくてよい） |

- `ServerBranch` はブランチ名・所有者・エンジンと版・`Record`（このブランチは記録する）・`Creating`・`Anonymous`（ログインなしで作られたブランチ。記録しない）・`Maintenance`。作成時にエラーを返すと作成が失敗する（`ErrForbidden` を包めば拒否として 403）
- **記録するかどうかは、ブランチを作るときに一度だけ決まり、あとから変わらない**。決まり方は 2 つ。作成の要求が `record: true` を持つか、持たないブランチについて agent が作成の直前に一度だけ `ServerOptions` を `Creating: true`（`Record` は false）で呼び、モジュールが返す `ServerOptions.Record` が true か（history モジュールの `record_all` がオンのときの答え）。決まった値はブランチの `Record` として保存し、それ以降の呼び出しでは `Record` だけを見る。モジュールは `Record` のブランチにだけ記録の設定を返し、自分の設定（`record_all`）を後から見直してはならない。`Creating` はそれ以外の呼び出しでは false
- 記録するブランチ（`Record`）は、起動のたびに（作成・作り直し・リセット・チェックポイントの復元・履歴の復元・agent やノードの再起動）モジュールのオプションが付かなければ**起動しない**。モジュールが無い、止めてある、ライセンスが無い、いずれでも同じ。作成と作り直しとリセットは失敗にして理由を `status_reason` に出す。再起動で起動できなかったブランチも `failed` と理由にし、モジュールが戻ったあとの次の起動で `ready` に戻る。記録しないブランチには、`record_all` がオンになっていても記録のオプションは付かない
- ストアは、既存のブランチを書き換えるとき `Record` を保存済みの値のまま変えない（`PutBranch`）。API・CLI・コンソールに記録を切り替える手段は無い。Raft のコマンドの意味は変わらない（プロトコルは 15 のまま）
- `POST /v1/branches` の `record: true` は `ServerOptioner` が受け付けたときだけ通る。`GET /v1/capabilities` の `record` が true のとき、コンソールは作成画面にだけチェックボックスを出す（作ったあとは「Recording: on / off」と表示するだけ）
- `GET /v1/branches/{name}/checkpoints` は `lines`（線の構造: 番号・現在の線か・親の線・元のチェックポイント）、`live`、`head` を足して返す。コンソールのグラフはこれで描く
- ライセンス: `license.FeatureHistory`（`history`）。どのプランに入れるかは `pkg/ext/license` の `planFeatures` の 1 か所

## 自分のモジュールを他のノードで呼ぶ（diff モジュール向け）

2 つのブランチのデータとスキーマを比べる有償モジュール `diff`（`db-branch-modules` の `docs/diff.md`）のために足した拡張点。追加のみ。古い agent では `Host.API` が nil になる。

| 拡張点 | 内容 |
|---|---|
| `ext.Host.API`（`ext.ModuleAPI`） | `Call(ctx, ext.Request) (ext.Response, error)`。モジュールが**自分自身の API**（`Handler`）を、クライアントが呼ぶのと同じ経路で呼ぶ。`/branches/<名前>/...` のパスはそのブランチを持つノードのモジュールが、`RawQuery` に `node=<ノード>` を付ければそのノードのモジュールが答える。モジュールが `Request.Principal` に渡した利用者として `ext.<モジュール>.read`（GET）・`.write`（それ以外）を認可し、書き込みは監査する |
| `ext.BranchState.Base` / `Snapshot` | ブランチの作成元のベースとスナップショット |

- 用途: ブランチ A のノードで動くモジュールが、別のノードにあるブランチ B の中身（チェックサムや行）を、B のノードのモジュールに頼む。データベースの認証情報はノードの外に出ない（各ノードのモジュールがローカルのソケットで自分のブランチだけを読み、結果だけが agent 間の相互 TLS を通る）
- `Principal` は呼び出し元のモジュールが責任を持つ（モジュールは agent と同じ信頼の内側で動く）。受けた要求の利用者をそのまま渡すこと
- ライセンス（masking）: `license.FeatureMasking` はどのプランにも入れない（トライアルにも入らない）。`Modules` に `masking` を名指しで書いたライセンスだけが `Has` で真になる（`*` は含まない）
- ライセンス: `license.FeatureDiff`（`diff`）。Lite・Team・Business のすべて（`planFeatures` の 1 か所）。Lite は history と diff だけで 3 ノードまで
