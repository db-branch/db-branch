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
- 監査は全モジュールに届く。失敗しても API は止めず、agent のログに残す
- 振り分けは最初に見つかった Router だけを使う（ノードあたりプロキシは 1 つ）。`Module.Ports`（`mysql`・`postgres`）が接続先 URL のポートになる
- ノード間で転送された要求は、最初に受けたノードで認可・監査し、転送先では元の利用者として認可だけを行う（二重に記録しない）
- ローカルソケット（root と db-branch グループ）からの呼び出しの利用者は `local`

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
- 各ノードの agent は記録と手元を 30 秒ごと（記録が変わったときはすぐ）に照合し、`apt-get install/remove` を `systemd-run` の一時ユニットで実行する（パッケージの postinst が agent を再起動するため、apt を agent の子プロセスにしない）
- `db-branch module remove <名前>`（Disable）で全ノードから削除する
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
