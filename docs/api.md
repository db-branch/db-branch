# db-branch REST API 設計

ステータス: Draft / 最終更新: 2026-11-09

対応する機械可読な定義: [`api/openapi.yaml`](../api/openapi.yaml)（OpenAPI 3.1）。全体設計は [design.md](design.md) を参照。

`api/openapi.yaml` は、エージェントが実際に返すコア API の全ルート（メソッド・パス・スキーマ・エラーコード・認証・`x-dbb-action`・トークンのスコープ）を記述する公開 API リファレンスで、Redoc で表示できる（`npx @redocly/cli lint api/openapi.yaml` で検証）。`internal/interface/httpapi/openapi_test.go` が、サーバーが登録したルートとこのファイルのオペレーションが過不足なく一致すること、`operationId` が一意でタグとエラー応答があること、`$ref` がすべて解決できることを検査する。ルートを足したら同じ変更で `openapi.yaml` も更新する（さもないと `make test` が失敗する）。有償モジュールの `/v1/ext/<module>/...` はモジュールが自分で文書化し、`openapi.yaml` には載せない。

この文書は設計の記録で、実装と食い違う箇所がある（`Idempotency-Key`、`ETag` / `If-Match`、カーソル式ページング、`X-DB-Branch-Index`、`?min_index` は未実装）。実際に返すものは `openapi.yaml` を正とする。

## 1. 方針

- CLI・Web コンソール・外部連携のすべてが同じ API を使う。CLI だけの裏口は作らない（ノード上でしかできない操作は §3.2 のソケット限定エンドポイントにする）
- 必要最小限にする。design.md にない機能は足さない
- 時間のかかる操作は **202 + オペレーション** で返し、進み具合は SSE で流す
- 返す項目は、コンソールの「視覚の語彙」（design.md §11）をそのまま描けるようにする

## 2. 共通の約束

### 2.1 URL・形式

- 接頭辞は `/v1`。互換性を壊す変更をするときは `/v2` を作る
- リクエストとレスポンスは JSON（`Content-Type: application/json; charset=utf-8`）。SSE だけ `text/event-stream`
- フィールド名は `snake_case`
- 時刻は RFC 3339 の UTC（`2026-11-09T12:34:56Z`）。秒未満は付けない
- 容量はバイト単位の整数。人間向けの単位への変換はコンソール側で行う
- 期間の入力は Go の duration 表記（`72h`、`30m`）。出力は期限の時刻（`expires_at`）で返す

### 2.2 識別子

| 種類 | 形式 | 例 |
|---|---|---|
| ブランチ名・ベース名・スナップショット名 | `domain.Name`（英小文字・数字・単独のハイフン、先頭は英字、最大 32 文字） | `feature-x` |
| ランタイム | `engine@version` | `mysql@8.4.13` |
| ノード名 | ホスト名（`cluster join` で登録した名前） | `node1` |
| オペレーション ID | `op_` + ULID | `op_01JC8Z6Q4M7V3X2N9K5R8T1W0B` |

名前はリソースの ID を兼ねる（URL に直接入る）。名前の変更はできない。

### 2.3 エラー

すべてのエラーは同じ形で返す。

```json
{
  "error": {
    "code": "not_found",
    "message": "branch \"feature-x\" not found",
    "details": { "resource": "branch", "name": "feature-x" }
  }
}
```

- `code`: 機械向けの安定した値（下表）。クライアントはこれで分岐する
- `message`: 人間向けの英文。内容は変わりうるので、プログラムで解釈しない
- `details`: 任意。`code` ごとに中身が決まっている

| HTTP | code | 意味 | details |
|---|---|---|---|
| 400 | `invalid_request` | JSON が壊れている、必須項目がない、型が違う | `field` |
| 400 | `invalid_name` | 名前の規則に合わない | `field`, `value` |
| 400 | `invalid_runtime` | `engine@version` の形でない | `value` |
| 401 | `unauthenticated` | セッションがない・切れた | — |
| 403 | `csrf_failed` | CSRF トークンがない・違う | — |
| 403 | `forbidden` | 認可で拒否された（有償 `auth` モジュール導入時） | `action` |
| 403 | `socket_only` | UNIX ソケット経由でしか呼べない操作 | — |
| 404 | `not_found` | リソースがない | `resource`, `name` |
| 409 | `already_exists` | 同じ名前のリソースがある | `resource`, `name` |
| 409 | `conflict` | 状態が合わない（例: 準備中のブランチをリセット） | `status` |
| 409 | `in_use` | 参照されているので消せない（例: ベースが使っているランタイム） | `used_by` |
| 409 | `unclean_shutdown` | DB サーバーの正常停止を確認できず、スナップショットを取れなかった | `base`, `log` |
| 412 | `precondition_failed` | `If-Match` の ETag が古い | `current_etag` |
| 422 | `idempotency_key_reused` | 同じ冪等キーで違う内容が送られた | — |
| 422 | `runtime_unavailable` | カタログにない・このアーキテクチャ向けがない | `runtime`, `arch` |
| 429 | `rate_limited` | ログイン試行の上限 | — （`Retry-After` ヘッダー） |
| 503 | `no_leader` | リーダーがいない（選出中・過半数割れ） | — （`Retry-After` ヘッダー） |
| 507 | `insufficient_capacity` | 配置できるノードに空き容量がない | `required`, `available` |
| 500 | `internal` | 想定外のエラー | — |

### 2.4 一覧とページング

一覧はカーソル方式。

```
GET /v1/branches?limit=50&cursor=eyJu...
```

```json
{ "items": [ ... ], "next_cursor": "eyJu..." }
```

- `limit` の既定は 100、上限は 500
- `next_cursor` が `null` なら最後のページ
- 並び順はエンドポイントごとに固定（ブランチは名前順、オペレーションは新しい順）

### 2.5 冪等キー

作成系の `POST`（ブランチ・ベース・スナップショット・ランタイムのインストール）は `Idempotency-Key` ヘッダーを受け付ける。

- 同じキーで同じ内容が再送されたら、最初の結果（同じオペレーション）をそのまま返す
- 同じキーで違う内容なら `422 idempotency_key_reused`
- キーは Raft に 24 時間保存する。リーダーが替わっても有効
- 省略可。CLI は毎回ランダムなキーを付ける（ネットワークの再試行で二重作成しないため）

### 2.6 ETag と If-Match

- 単体取得（`GET /v1/branches/{name}` など）は `ETag` ヘッダーを返す。値は Raft の更新インデックスから作る
- 更新（`PATCH /v1/branches/{name}`）は `If-Match` を必須にする。古ければ `412 precondition_failed`
- 削除・リセットでは `If-Match` を任意とする（付ければ検査する）。コンソールは付ける

### 2.7 長時間の操作（オペレーション）

次の操作は数秒〜数十分かかるので、受け付けた時点で `202 Accepted` を返す。

| 操作 | kind |
|---|---|
| ランタイムのインストール（全ノードへの配布を含む） | `runtime.install` |
| ベースの作成（ダンプの流し込み） | `base.create` |
| ベーススナップショットの作成と伝搬 | `base.snapshot` |
| ブランチの作成 | `branch.create` |
| ブランチのリセット | `branch.reset` |
| ブランチのチェックポイントの復元 | `branch.restore` |
| ブランチの削除 | `branch.delete` |

レスポンス:

```
HTTP/1.1 202 Accepted
Location: /v1/operations/op_01JC8Z6Q4M7V3X2N9K5R8T1W0B
```

```json
{
  "id": "op_01JC8Z6Q4M7V3X2N9K5R8T1W0B",
  "kind": "base.snapshot",
  "target": { "resource": "base", "name": "prod-masked", "snapshot": "s20261109" },
  "state": "running",
  "progress": { "done": 2, "total": 3, "unit": "nodes" },
  "error": null,
  "created_at": "2026-11-09T12:00:00Z",
  "updated_at": "2026-11-09T12:00:41Z",
  "finished_at": null
}
```

- `state`: `pending` → `running` → `succeeded` / `failed`
- `phase`: 実行中の段階（`running` のときだけ）。`base.snapshot` は `quiescing` → `stopping` → `verifying` → `snapshotting` → `restarting` → `propagating`、`branch.create` は `cloning` → `starting` → `init_sql` → `stopping` → `snapshotting` → `starting`（design.md §5・§6。スナップショットは DB サーバーを正常停止してから取る）
- `progress`: コンソールの `2/3` 表示にそのまま使う。`unit` は `nodes`（配布・伝搬）か `bytes`（ダウンロード・流し込み）。分からないときは `null`
- `error`: 失敗時に §2.3 の `error` と同じ形
- 対象リソース（ブランチなど）自体も作成直後から `GET` で見え、`status` で状態を表す。オペレーションは「その操作の経過」、リソースは「今の状態」を表す
- 完了したオペレーションは 7 日間保存する

### 2.8 イベント（SSE）

```
GET /v1/events
Accept: text/event-stream
Last-Event-ID: 18342
```

```
id: 18343
event: branch.updated
data: {"name":"feature-x","status":"ready", ... }

id: 18344
event: operation.updated
data: {"id":"op_...","state":"running","progress":{"done":2,"total":3,"unit":"nodes"}, ...}
```

- `event`: `<resource>.updated` / `<resource>.deleted`。resource は `branch`・`base`・`runtime`・`node`・`operation`
- `data`: `updated` は単体取得と同じ完全な表現。`deleted` は `{"name": ...}`（オペレーションは `{"id": ...}`）
- `id`: Raft のインデックス。`Last-Event-ID` を付けて再接続すれば取りこぼした分から再送する。古すぎて再送できないときは `event: resync` を送るので、クライアントは一覧を取り直す
- 15 秒ごとにコメント行（`: ping`）を送って接続を保つ
- 絞り込み: `?types=branch,operation`

## 3. 認証

### 3.1 Web コンソール（セッション Cookie + CSRF）

画面は 2 つある。`/` はブランチを使う人向け（ベースを選んでブランチを作り、接続情報を得て、自分のブランチを管理する。公開スナップショットはログインなしで使える）。`/admin` は管理者向けで、管理者以外がログインしても中は見えない。旧 URL（`/branches` など）は `/admin/...` に転送する。

```
POST /v1/auth/login        {"username":"admin","password":"..."}
→ 200 Set-Cookie: dbb_session=...; HttpOnly; Secure; SameSite=Strict; Path=/
  {"user":{"name":"admin"},"csrf_token":"...","expires_at":"..."}

GET  /v1/auth/session      → 200 {"user":{"name":"admin"},"csrf_token":"...","expires_at":"..."}
POST /v1/auth/logout       → 204
PUT  /v1/auth/password     {"current_password":"...","new_password":"..."} → 204
```

- 状態を変えるリクエスト（`GET` 以外）には `X-CSRF-Token` ヘッダーが必要。ない・違えば `403 csrf_failed`
- セッションは Raft に保存し、どのノードでも有効。有効期限は 12 時間、操作のたびに延長する
- ログインの失敗が続いたら `429 rate_limited`（`Retry-After` 付き）
- パスワードを変更すると、今のセッション以外はすべて無効になる
- パスワードのリセット（忘れたとき）は API にしない。ノード上で `sudo db-branch admin reset-password` を実行する
- OSS 版の `username` は `admin` だけ

### 3.2 ローカル CLI（UNIX ソケット）

- `/run/db-branch/agent.sock` で同じ API を提供する。HTTP をソケット上で話す
- 接続元のプロセスの UID/GID を `SO_PEERCRED` で確かめ、root か `db-branch` グループなら認証済みとして扱う。Cookie・CSRF は不要
- 主体（principal）は `local:<ユーザー名>` として監査イベントに記録する
- ソケット限定の操作: `POST /v1/runtimes/import`（ノード上のパスを読むため）。TCP から呼ぶと `403 socket_only`
- クラスタの初期化・参加（`cluster init` / `join`）は API にせず CLI だけで行う（参加トークンと証明書の発行を伴うため）

### 3.3 拡張ポイントとの関係

すべてのリクエストは次の順に通る。OSS 版でもこの経路は省略しない。

```
リクエスト → Authenticator → Authorizer → ハンドラ → (変更系なら) AuditSink
```

| ポート | OSS 版 | 有償 `auth` / `audit` モジュール |
|---|---|---|
| `Authenticator` | セッション Cookie（admin のみ）とソケットの UID 確認 | 複数ユーザー・SSO（OIDC / SAML）・API トークン（`Authorization: Bearer`） |
| `Authorizer` | 認証済みならすべて許可 | RBAC。拒否時は `403 forbidden` |
| `AuditSink` | 構造化ログに出すだけ | 永続化・検索・外部転送 |

- 認可の単位は `action`（例: `branch.create`、`branch.delete`、`runtime.install`）と対象リソース。各エンドポイントの `action` は OpenAPI の `x-dbb-action` に書く
- **変更系の呼び出しはすべて**監査イベントを出す。成功・失敗（認可拒否を含む）の両方を出す

```json
{
  "time": "2026-11-09T12:00:00Z",
  "principal": "admin",
  "via": "console",
  "action": "branch.delete",
  "target": { "resource": "branch", "name": "feature-x" },
  "result": "succeeded",
  "operation_id": "op_...",
  "source_ip": "192.0.2.20",
  "node": "node1"
}
```

- 有償モジュール向けのエンドポイント（ユーザー管理・監査ログの検索など）は、モジュールが `/v1/ext/<module>/...` の下に追加する。OSS の API には含めない

## 4. クラスタでの振る舞い

- **どのノードでも受け付ける**。変更系のリクエストは、受けたノードがリーダーへ転送する（クライアントからは見えない）
- リーダーがいないときは `503 no_leader` と `Retry-After: 2` を返す。既存ブランチへの DB 接続（ProxySQL → DB サーバー）はこの間も止まらない
- **読み取りは受けたノードの手元の状態から返す**。リーダーより少し遅れている可能性がある（通常は数百ミリ秒以内）
  - 全レスポンスに `X-DB-Branch-Index`（その状態の Raft インデックス）と `X-DB-Branch-Node`（応答したノード）を付ける
  - 直前の自分の書き込みを確実に読みたいときは `?min_index=<書き込み応答のインデックス>` を付ける。手元が追いつくまで最大 5 秒待ち、追いつかなければリーダーへ転送する
  - `?consistent=true` なら常にリーダーで読む
- 変更系の応答（202 / 200）にも `X-DB-Branch-Index` を付ける。CLI は続けて読むときにこれを `min_index` に使う
- 削除・状態遷移は SSE にも同じ順で流れるので、コンソールは SSE を正として表示を更新する

## 5. リソース

### 5.1 ブランチ

| メソッド | パス | 応答 | action |
|---|---|---|---|
| `GET` | `/v1/branches` | 200 一覧 | `branch.list` |
| `POST` | `/v1/branches` | 202 オペレーション | `branch.create` |
| `GET` | `/v1/branches/{name}` | 200 | `branch.get` |
| `PATCH` | `/v1/branches/{name}` | 200（TTL の変更のみ） | `branch.update` |
| `DELETE` | `/v1/branches/{name}` | 202 オペレーション | `branch.delete` |
| `POST` | `/v1/branches/{name}/reset` | 202 オペレーション | `branch.reset` |
| `GET` | `/v1/branches/{name}/dsn` | 200 接続文字列（パスワード入り） | `branch.dsn` |
| `GET` | `/v1/branches/{name}/checkpoints` | 200 一覧（全ての線のチェックポイント。古い順） | `branch.checkpoint.read` |
| `POST` | `/v1/branches/{name}/checkpoints` | 201 チェックポイント（スナップショットは一瞬なので同期） | `branch.checkpoint` |
| `POST` | `/v1/branches/{name}/checkpoints/{checkpoint}/restore` | 202 オペレーション | `branch.restore` |
| `DELETE` | `/v1/branches/{name}/checkpoints/{checkpoint}` | 204 | `branch.checkpoint_delete` |
| `POST` | `/v1/branches/{name}/upgrade` | 202 オペレーション（`result` に結果） | `branch.upgrade` |

一覧の絞り込み: `?base=prod-masked`、`?node=node1`、`?status=ready`。

チェックポイント（[checkpoints.md](checkpoints.md)）:

```json
POST /v1/branches/feature-x/checkpoints
{ "name": "before-migration" }          // 省略可。既定は UTC の時刻（20261112-153000）

201 Created
{ "name": "before-migration", "created_at": "2026-11-12T15:30:00Z", "size": 98304, "line": 0, "auto": false, "current": true }
```

- 一覧は ZFS から読む（Raft には置かない）。担当ノード以外に届いた要求は担当ノードへ転送する
- `size` はそのチェックポイントだけが持つ容量、`line` は履歴の線の番号（復元のたびに新しい線ができる）、`auto` は復元の前にシステムが取ったもの、`current` は現在のデータの元になったもの
- 上限は設定 `branch.max_checkpoints`（既定 20）。数えるのは手動のチェックポイントだけで、超える作成は `409 conflict`。自動のもの（アップグレードの前の `before-upgrade-<時刻>`、以前の版が復元の前に取った `before-restore-<時刻>`）は数えず、復元を断る理由にもならない。自動のものは新しい 5 件だけ残り、6 件目ができるアップグレードは一番古いものを通常の削除と同じ手順で消す
- 作成（`201`）の応答には、ブランチの使用量（ライブ + 履歴）がプールの空きの半分を超えるとき `warning`（英文の文字列）が付く。作成は断らない。空きが不明なら付かない
- 復元（`branch.restore`）はブランチが `ready` のときだけ。今の状態は置き換わり、自動では保存されない（残したいときは先に `POST …/checkpoints` で取る。取った他のチェックポイントは全て残る）。進捗は `cloning` `stopping` `swapping` `promoting` `starting`
- 名前の重複は `409 already_exists`、存在しないチェックポイントは `404`、不正な名前は `400`
- 所有者と管理者だけが使える。ログインなしの呼び出しや `open` のブランチは対象外

Branch のアップグレード（[upgrade.md](upgrade.md)）:

```json
POST /v1/branches/feature-x/upgrade
{ "runtime": "mysql@8.4.13", "check_only": false, "no_rollback": false, "force": false }

202 Accepted        // オペレーション branch.upgrade。GET /v1/operations/{id} の result が UpgradeResult
```

- `runtime` だけが必須。`check_only` は事前確認だけ（何も変えない）、`no_rollback` は失敗を戻さず残す、`force` は事前確認のエラーを無視して進める
- 同期で断るもの: `400`（形式の誤り）、`422 upgrade_not_allowed`（ダウングレード、LTS の飛び越し、別のエンジン、PostgreSQL のメジャー更新）、`404 runtime_not_installed`（入れ方を添える）、`409`（`ready` でない）、`503 cluster_upgrading`（全ノードが新しい版になるまで）
- 進捗は `checking` `stopping` `checkpointing` `upgrading`、失敗して戻すときは `rolling_back` と復元の進捗。オペレーションが `failed` でも `result` は付く（`status` が `rolled_back`、`failed`、`refused`、`aborted`）
- Branch の JSON の `runtime` はその Branch のもの、`base_runtime` は Base のもの。Checkpoint の JSON の `runtime` は取ったときのランタイム
- `POST /v1/branches` に `runtime` を付けると、作成のあとすぐに同じ手順で上げる（`kind` は `branch.create` のまま、`result` に結果）。失敗しても Branch は残り、戻された Base のランタイムで動く
- Branch の状態に `upgrading` が加わる。`no_rollback` で残した失敗は `failed` で、`status_reason` が `upgrade failed:` で始まる。このときも Checkpoint の復元は使える

作成:

```json
POST /v1/branches
{
  "name": "feature-x",
  "base": "prod-masked",
  "snapshot": null,
  "init_sql": "CREATE TABLE ...;",
  "ttl": "72h"
}
```

- `snapshot` を省略するとベースの最新スナップショットを使う
- `init_sql` はリクエストに直接入れる（上限 1 MiB）。適用後にリセットポイントを取る
- `ttl` を省略すると期限なし。`"0"` は不可（省略で表す）

表現:

```json
{
  "name": "feature-x",
  "status": "ready",
  "status_reason": null,
  "base": "prod-masked",
  "snapshot": "s20261109",
  "runtime": "mysql@8.4.13",
  "node": "node2",
  "created_at": "2026-11-09T12:00:00Z",
  "expires_at": "2026-11-12T12:00:00Z",
  "reset_at": "2026-11-09T12:00:08Z",
  "storage": { "used": 1048576, "referenced": 524288000 },
  "checkpoints": true,
  "connection": {
    "host": "node2",
    "port": 33061,
    "user": "app.feature-x",
    "database": "app"
  },
  "routing": {
    "proxysql_user": "app.feature-x",
    "comment": "/*db_branch='feature-x'*/"
  },
  "operation": null
}
```

- `status`: `provisioning`（作成中・リセット中）/ `ready` / `failed` / `lost`（ノード喪失）/ `deleting`
- `status_reason`: `failed`・`lost` のときに短い英文
- `expires_at`: 期限なしなら `null`。コンソールは `created_at` から `expires_at` までの残りで TTL バーを描く
- `storage.used` はこのブランチ固有の差分、`storage.referenced` はベースと共有している分を含む全体（ZFS の値そのまま）
- `checkpoints`: ブランチを持つノードのストレージがチェックポイントに対応しているか（そのノード自身の ZFS なら `true`。モジュール／共有ストレージでは `false`。クラスタではブランチのノードが報告した値で、未報告の古いノードも `false`）。`false` のとき、チェックポイントの API は `409`（エラーコード `checkpoints_unavailable`）を返し、コンソールはチェックポイントの操作を出さない
- `connection`: 直接つなぐ場合。ProxySQL を使う場合は `routing` を見る（フェーズ 3 以降。それまでは `null`）
- `operation`: 進行中のオペレーションがあればその ID とkind。なければ `null`
- `GET /v1/branches/{name}/dsn` は `{"dsn": "mysql://app.feature-x:<password>@node2:33061/app"}` を返す。パスワードは一覧・単体取得には含めない。この呼び出しは読み取りだが監査イベントを出す

### 5.2 ベースとスナップショット

| メソッド | パス | 応答 | action |
|---|---|---|---|
| `GET` | `/v1/bases` | 200 一覧 | `base.list` |
| `POST` | `/v1/bases` | 202 オペレーション | `base.create` |
| `GET` | `/v1/bases/{name}` | 200 | `base.get` |
| `GET` | `/v1/bases/{name}/snapshots` | 200 一覧（新しい順） | `base.get` |
| `POST` | `/v1/bases/{name}/snapshots` | 202 オペレーション（作成と伝搬） | `base.snapshot` |
| `GET` | `/v1/bases/{name}/snapshots/{snapshot}` | 200 | `base.get` |
| `PATCH` | `/v1/bases/{name}/snapshots/{snapshot}` | 200 ベース（`{"public": true}` で公開） | `base.publish` |
| `DELETE` | `/v1/bases/{name}/snapshots/{snapshot}` | 204 | `base.snapshot_delete` |
| `PATCH` | `/v1/bases/{name}` | 200 ベース（`{"description": "..."}`、1000 バイトまで） | `base.describe` |
| `PUT` | `/v1/bases/{name}/replicas` | 200 ベース（`{"replica_nodes": null}` で全ノード、`[]` で起点ノードだけ、`["n2","n3"]` でそれらと起点）。存在しないノードは 400 | `base.replicas` |
| `PATCH` | `/v1/bases/{name}/anonymous` | 200 ベース（ログインなしの上限。`{"max_branches": 3, "max_ttl": "4h"}`。書いたキーだけ変わり、`null` は継承に戻す。負の数・0 以下の期間・型違いは 400） | `base.publish` |
| `POST` | `/v1/bases/{name}/load` | 202 オペレーション（`{"dump": "upload:<id>" か ノード上のパス, "snapshot": true}`） | `base.load` |
| `POST` | `/v1/uploads?name=x.sql.gz&node=N` | 201 `{"id":"upload:<id>","size":...,"node":...}`（本文はファイルそのもの） | `base.upload` |
| `GET` | `/v1/uploads/room?node=N` | 200 `{"max_bytes":...,"reason":"limit"/"pool"}` | `base.upload` |
| `GET` | `/v1/bases/{name}/refresh` | 200 定期更新の設定（パスワードは返さず `password_set` だけ）。無ければ 404 | `base.refresh.read` |
| `PUT` | `/v1/bases/{name}/refresh` | 200 設定（下記）。パスワードを省くと、保存済みのものを使い続ける | `base.refresh` |
| `DELETE` | `/v1/bases/{name}/refresh` | 204（定期更新をやめ、接続先とパスワードを忘れる） | `base.refresh` |
| `POST` | `/v1/bases/{name}/refresh/run` | 202 オペレーション（`base.refresh`。今すぐ更新する） | `base.refresh` |
| `POST` | `/v1/bases/{name}/sql` | 204（`{"sql": "..."}` をベースのサーバーで管理者として実行） | `base.sql` |

作成:

```json
POST /v1/bases
{
  "name": "prod-masked",
  "runtime": "mysql@8.4.13",
  "source": { "type": "dump", "path": "/srv/dumps/prod-masked.sql.gz" }
}
```

- `source.path` はベースオリジンになるノード（リクエストを受けたノード）上のパス。agent が読める場所に置く
- ダンプの形式（mysqldump / mydumper など）は design.md §15 の未決事項に従う

ベースの表現:

```json
{
  "name": "prod-masked",
  "runtime": "mysql@8.4.13",
  "origin_node": "node1",
  "status": "ready",
  "created_at": "2026-11-09T10:00:00Z",
  "latest_snapshot": "s20261109",
  "branch_count": 4,
  "storage": { "used": 524288000, "referenced": 524288000 }
}
```

スナップショットの表現:

```json
{
  "name": "s20261109",
  "base": "prod-masked",
  "created_at": "2026-11-09T11:00:00Z",
  "propagation": {
    "done": 2,
    "total": 3,
    "nodes": [
      { "node": "node1", "state": "present" },
      { "node": "node2", "state": "present" },
      { "node": "node3", "state": "receiving", "progress": { "done": 104857600, "total": 524288000, "unit": "bytes" } }
    ]
  },
  "branch_count": 2
}
```

- `propagation.nodes[].state`: `present` / `receiving` / `pending` / `failed`
- `GET /v1/bases` と `GET /v1/bases/{name}` の各ベースには `can`（action 名から true/false への対応）が付く。呼び出した人がそのベースに対してできる操作で、認可の拡張点に `Resource{Kind:"base", Name:<ベース名>}` を渡して調べる（ベースごとのメンテナーの権限が反映される）。対象は `base.snapshot` / `base.load` / `base.delete` / `base.describe` / `base.mask` / `base.publish` / `base.sql` / `base.start` / `base.stop` / `base.refresh` / `base.refresh.read` / `base.snapshot_delete` / `base.replicas`。イベント（`base.updated`）には付かない。コンソールは `can` を見てボタンを出し、無ければセッションの全体の権限（`can`）を使う
- レプリカ先（design.md §6「コピーを置くノードを選ぶ」）: `POST /v1/bases` は `replica_nodes`（省略か `null` で全ノード、`[]` で起点ノードだけ、名前の配列でそれらと起点）を受け取る。ベースの表現には `replica_nodes`（`null` は全ノード）と、ノードごとの保持状況 `holders`（`[{"node": "n2", "state": "held"|"pulling"|"draining", "origin": false}]`）が付く。`pulling` はコピーを取得中（まだ最新を持たない）、`skipped` はコピーの対象だが別の OS のため作らない（PostgreSQL のみ。`reason` は `other OS: ubuntu-26.04`、`distro` はそのノードのディストリビューション。どちらも `skipped` のときだけ付く。[mixed-os.md](mixed-os.md)）、`draining` は一覧から外れたが、そのノードのブランチがまだ使っていて消せない。コピーのサイズはまだ返さない。全ノードが新しいプロトコル（13）になるまで、`replica_nodes` を指定した作成と `PUT .../replicas` は `503 cluster_upgrading` で断る
- ログインなしの上限（design.md §6「ログインなしの作成の上限」）: `GET /v1/bases` と `GET /v1/bases/{name}` の各ベースには `anonymous` が付く。`{"max_branches": 3, "max_ttl": "4h0m0s"}` の形で、**`null` は「継承」**（`max_branches` が `null` なら全体の上限 `branch.anonymous_max` だけが効き、`max_ttl` が `null` なら `branch.anonymous_ttl` が効く）。`max_branches: 0` は、スナップショットが公開でもこのベースからはログインなしで作らせない。どのベースにも到達できるかは、これまでどおり公開スナップショットだけで決まる（この設定は公開を増やさない）。変更は `PATCH .../anonymous`（権限 `base.publish`）。全ノードが新しいプロトコル（14）になるまで、`503 cluster_upgrading` で断る
- `POST .../snapshots` の `name` は省略可（省略時は `sYYYYMMDDhhmm`）
- スナップショットはベースの DB サーバーを正常停止してから取る（design.md §6）。その間、ベースの `status` は `maintenance` になり、ベースへの接続はできない。正常停止を確認できなければ、スナップショットを取らずに `409 unclean_shutdown` で失敗する

ダンプのアップロード:

- 本文はダンプファイルそのもの（`.sql` か `.sql.gz`）。`Content-Length` が必須で、受け取る前に、設定 `upload.max_mb`（既定 20 GiB）、置き場所のディスクの空き（1 GiB は残す）、ZFS プールの空きと比べ、超えれば読まずに `413`/`507` で断る
- `node` には、そのダンプを使うノード（ベースを作る・持つノード）を指定する。アップロードはそのノードに置かれ、別のノードからは使えない
- 使われたアップロードはすぐ消え、24 時間使われなかったものも消える。`POST /v1/bases` の `dump` と `.../load` の `dump` に `upload:<id>` を渡す

定期更新（`/refresh`）:

```json
PUT /v1/bases/prod/refresh
{
  "source": { "host": "db.internal", "port": 5432, "user": "dumper", "password": "...", "databases": ["app", "billing"] },
  "every": "24h"
}
```

```json
200
{
  "base": "prod", "engine": "postgres",
  "source": { "host": "db.internal", "port": 5432, "user": "dumper", "password_set": true, "databases": ["app", "billing"] },
  "every": "24h0m0s",
  "updated_at": "2026-11-09T12:00:00Z",
  "last_run": { "started_at": "...", "finished_at": "...", "ok": false, "error": "pg_dump app: ..." },
  "next_run": "2026-11-10T12:00:00Z",
  "operation": null
}
```

- `every` は Go の期間（`24h`、`90m`）で、1 時間以上。`source` の接続先の種類は、ベースのランタイムのエンジン（`postgres` / `mysql`）で決まる
- `last_run` は最後の実行の結果（失敗なら `error` に理由）。`next_run` は最後の実行が終わった時刻（まだ無ければ設定した時刻）に `every` を足した時刻。失敗した実行は、次の `next_run` に再試行する
- 実行はベースを持つノードで 1 ベースにつき 1 つずつ。オペレーションは `base.refresh` として、ほかのベースの操作と同じようにイベントに出る
- パスワードは応答にも監査ログにもオペレーションの出力にも出ない。クラスタの状態（Raft のログとスナップショット）には平文で保存される

スナップショットの管理:

- **データの更新**: `POST .../sql` はベース自身のサーバーで SQL を実行する。止まっていれば起動する。既存のブランチには影響しない。新しいブランチに反映するには、続けてスナップショットを取る
- **削除**: 最新のスナップショットと、ブランチが使っているスナップショットは消せない（`409`）。ほかのノードのコピーは、次の複製で消える
- **自動削除**（設定 `base.prune_snapshots`、既定で有効）: 新しいスナップショットがあり、どのブランチも使っておらず、コピーを持つ全ノードが最新まで追いついた古いスナップショットを、定期処理（30 秒ごと）で消す。公開だったスナップショットが消えると、最新のスナップショットが公開になる
- **公開**（`public`）: 公開スナップショットからは、ログインなしで `POST /v1/branches` できる（下記）

### 5.2.1 ブランチの所有者と公開範囲

- ブランチは作成者が所有者（`owner`）になり、`id` は `owner/name`。他人の名義（`owner` 指定）で作れるのは管理者（role `admin` か、full スコープのトークン）だけ
- 管理者以外が変更・削除・接続情報の取得をできるのは、自分のブランチと `access: "open"` のブランチ（接続情報のみ）だけ。所有者のない既存ブランチは従来どおり
- ユーザー画面（ログインセッション）から管理者以外が `POST /v1/branches` すると、名前の前に所有者の前置き `<owner>-` が付く（例: `alice` が `test` と入力すると `alice-test`）。前置きは `GET /v1/auth/session` の `branch_prefix` で画面に伝わり、管理者では空。メールアドレスは `@` より前だけを使い、小文字にして、英小文字・数字以外の連続を 1 つのハイフンにし、前後のハイフンを除く。空か数字で始まるときは先頭に `u` を付ける。すでに前置きで始まる名前は重ねない。名前全体が 32 文字を超えるときは切り詰めずに 400 を返し、入力できる最大の長さを伝える。API トークン・CLI・MCP・管理者・ログインなしの作成は、名前をそのまま使う
- `access`: `private`（既定。作成ごとにランダムなパスワード）か `open`（設定 `branch.open_password` の共通パスワード。誰でも接続できる）
- `GET /v1/public/bases`（ログイン不要）: 公開スナップショットのある ready のベースと、その公開スナップショットだけ（ログインなしの作成を 0 にしたベースは載せない）。各ベースの `anonymous` は、いま効いている値と残り: `max_branches`（全体の上限とベースの上限の小さいほう）、`max_ttl`（`branch.anonymous_ttl` とベースの `max_ttl` の短いほう）、`branches`（このベースからログインなしで作られたブランチの数）、`remaining`（あと何個作れるか。全体の残りとベースの残りの小さいほう）。トップレベルの `anonymous` は全体の値: `max_ttl`・`max_branches`・`branches`（ログインなしのブランチの合計）・`rate_per_hour`（`branch.anonymous_rate`）
- ログインなしの作成: 公開スナップショットからだけ。所有者は `anonymous`、`access` は常に `open`、TTL は `branch.anonymous_ttl`（既定 24h）とベースの `max_ttl` の短いほう以下。数は、全体で `branch.anonymous_max`（既定 10）個まで、かつそのベースから作ったものがベースの `max_branches` 個まで。超えたときは `409 conflict` で、いまの数を本文に書く。ベースが 0 のときは `403`。ログインなしで読めるのは、そのブランチ・その接続情報・そのオペレーションだけ
- ログインなしの作成の回数制限: 設定 `branch.anonymous_rate`（既定 5、0 で無制限）。接続元のアドレス 1 つあたり 1 時間にこの数まで。超えると `429 rate_limited` に `Retry-After`（秒）を付けて返す。数えるのは各ノードのメモリの中だけで、アドレスは保存も記録もしない（再起動で忘れる。クラスタでは受け付けたノードごとに数えるので、全体ではノード数倍まで通りうる）。IPv6 は上位 64 ビットで 1 つとみなす。`X-Forwarded-For` などのヘッダは信用せず、接続そのものの相手だけを見る（リバースプロキシの背後では全員がプロキシのアドレスになるので、プロキシ側で制限するか、この設定を 0 にして別に制限する）。断られた作成（上限・名前の重複・配置できないなど）は数えない。ログインしている人には適用しない

### 5.2.2 ライセンス

| メソッド | パス | 応答 | action |
|---|---|---|---|
| `GET` | `/v1/license` | 200 `{"configured":bool,"report":{...}}`（保存済みのキーを検証した結果。キー自体は返さない） | `license.read` |
| `POST` | `/v1/license/verify` | 200 レポート（`{"key":"dbbl1..."}` を検証するだけで保存しない） | `license.verify` |
| `PUT` | `/v1/license` | 200 レポート（検証して使えれば保存し、対象のモジュールを全ノードで有効にする） | `license.write` |
| `GET` | `/v1/license/renewal` | 200 更新の状態（リーダーのもの。リーダーでないノードはリーダーへ転送する） | `license.read` |
| `POST` | `/v1/license/renew` | 200 更新の状態（リーダーが今すぐライセンスサービスに問い合わせる。リーダーへ転送） | `license.write` |

- レポート: `license`（顧客・対象モジュール・期限・ノード上限）、`state`（`valid` / `grace` / `expired` / `over_nodes`）、`modules`（対象のうちパッケージがあるもの）、`unavailable`（ないもの）、`enabled`（`PUT` で有効にしたもの）、`all`（全モジュール対象。このときは自動では有効にしない）
- 署名の検証には、バイナリに組み込んだルート公開鍵を使う。鍵のないビルドでは `501 cannot_verify`。偽物・壊れたキーは `422 invalid_license`、期限切れ・ノード超過は `PUT` で `422 license_unusable`（保存しない）
- サブスクリプションのキー（`license.billing` / `license.subscription` / `license.renew_url`）の自動更新は [license-renewal.md](license-renewal.md)。`GET /v1/license` には `renewal`（`{"enabled":bool,"last_attempt":"…","last_result":"renewed|not_yet|inactive|error|unknown","message":"…","next_attempt":"…"}`）が付く。`last_result` は、リーダーに届かなかったときや一度も確認していないリーダーの直後は `unknown` / 空。`POST /v1/license/renew` は `409 auto_renew_off`（設定 `license.auto_renew` がオフ）・`409 not_renewable`（キーにサブスクリプションがない）・`409 in_progress`・`429 rate_limited`（30 秒に 1 回まで、`Retry-After` つき）を返す
- モジュールは `RequiresLicense` を申告でき、`GET /v1/modules` の `requires_license` に出る。コンソールはこれに「Paid」の印を付ける

### 5.3 ランタイム

| メソッド | パス | 応答 | action |
|---|---|---|---|
| `GET` | `/v1/runtimes` | 200 一覧（クラスタ全体） | `runtime.list` |
| `GET` | `/v1/runtimes/available` | 200 カタログ | `runtime.list` |
| `POST` | `/v1/runtimes` | 202 オペレーション | `runtime.install` |
| `POST` | `/v1/runtimes/import` | 202 オペレーション（**ソケット限定**） | `runtime.install` |
| `GET` | `/v1/runtimes/{engine}/{version}` | 200 | `runtime.get` |
| `DELETE` | `/v1/runtimes/{engine}/{version}` | 204 | `runtime.remove` |

```json
POST /v1/runtimes          {"runtime": "mysql@8.4.13"}
POST /v1/runtimes/import   {"runtime": "mysql@8.4.13-custom", "path": "/opt/mysql"}
GET  /v1/runtimes/available?engine=mysql
→ {"items": [{"runtime": "mysql@8.4.13", "engine": "mysql", "version": "8.4.13", "archs": ["amd64"], "size": 79753532}]}
```

ランタイムの表現:

```json
{
  "runtime": "mysql@8.4.13",
  "engine": "mysql",
  "version": "8.4.13",
  "source": "catalog",
  "sha256": "5dfbb764...",
  "distribution": {
    "done": 3,
    "total": 3,
    "nodes": [ { "node": "node1", "state": "present" } ]
  },
  "used_by": { "bases": ["prod-masked"] }
}
```

- インストールはリーダーが外部から取得し、他のノードはリーダーから受け取る（design.md §4）。その進み具合が `distribution`
- 削除は全ノードから消す。ベースが使っていれば `409 in_use`
- `source`: `catalog` / `import`

### 5.4 クラスタとノード

| メソッド | パス | 応答 | action |
|---|---|---|---|
| `GET` | `/v1/cluster` | 200 | `cluster.get` |
| `GET` | `/v1/cluster/nodes` | 200 一覧 | `cluster.get` |
| `GET` | `/v1/cluster/nodes/{node}` | 200 | `cluster.get` |

```json
GET /v1/cluster
{
  "id": "c_7f3a...",
  "leader": "node1",
  "quorum": { "voters": 3, "healthy": 3 },
  "index": 18344,
  "nodes": [ ... ]
}
```

ノードの表現:

```json
{
  "name": "node1",
  "address": "192.0.2.10",
  "leader": true,
  "status": "up",
  "last_seen_at": "2026-11-09T12:00:00Z",
  "version": "0.1.0",
  "arch": "amd64",
  "capacity": { "pool": "tank", "size": 53150220288, "allocated": 1048576000, "free": 52101644288 },
  "branch_count": 3,
  "branches": ["feature-x", "pr-1234", "qa"],
  "system": { "distro": "ubuntu-26.04", "os": "Ubuntu 26.04.1 LTS", "kernel": "7.0.0-14-generic", "zfs": "2.4.0-1ubuntu1" }
}
```

- `system`: ノードの OS（ディストリビューションのキー `distro`、表示名 `os`）、カーネル（`uname -r`）、ZFS の版（ユーザーランド。カーネルモジュールの版が違えば `2.3.1-1 (kmod 2.2.2-0ubuntu9)` のように続ける）。ハートビートで運ばれ、古いノードが報告しない項目は `"unknown"`。ノードごとに OS が違うクラスタでの PostgreSQL の扱いは [mixed-os.md](mixed-os.md)。PostgreSQL のベースを別ディストリビューションのノードに置く指定（`node`、`replica_nodes`）は `409 conflict` で断られる
- `service_ids` / `id_problem`: ノードの `db-branch` ユーザーの uid/gid と、クラスタのものと食い違うときの理由（[service-user-ids.md](service-user-ids.md)）。`GET /v1/cluster` のトップレベルの `service_ids` はクラスタの値。食い違うノードには新しいブランチもレプリカも置かれず、`node` で指定すると `409 conflict`
- `status`: `up` / `down` / `joining` / `leaving`
- 単一ノード構成でも同じ形で返す（`voters: 1`）
- コンソールは `capacity.allocated / capacity.size` で容量バーを描き、80% で琥珀色、90% で赤にする

#### 5.4.1 更新

マイナー更新の確認とロールアウト（[updates.md](updates.md)）。

| メソッド | パス | 応答 | action |
|---|---|---|---|
| `GET` | `/v1/updates` | 200 | `update.read` |
| `POST` | `/v1/updates/check` | 200（`apt-get update` を実行するノードで） | `update.check` |
| `POST` | `/v1/updates` | 202 ロールアウト（本文 `{"target": "1.2.0"}`） | `update.apply` |
| `DELETE` | `/v1/updates` | 204 | `update.apply` |

```json
GET /v1/updates
{
  "checked_at": "2026-11-11T10:00:00Z", "checked_by": "node1",
  "latest_minor": "1.2.0",
  "nodes": [{"name": "node1", "leader": true, "down": false, "version": "1.0.0",
             "modules": {"auth": "1.0.0"}, "update": {"state": "pending"}}],
  "packages": [{"name": "db-branch", "installed": "1.0.0",
                "available": [{"version": "2.0.0", "kind": "major"}, {"version": "1.2.0", "kind": "minor"}]}],
  "rollout": {"id": "…", "target": "1.2.0", "status": "running", "order": ["…"], "nodes": {"…": {"state": "updating"}}}
}
```

- `kind`: `minor`（同じ MAJOR の新しい版）か `major`（適用できない）
- `rollout.status`: `running` / `done` / `failed` / `canceled`。ノードの `state`: `pending` / `updating` / `done` / `failed`
- 開始できないとき: 400（メジャー更新・古い版・未確認の版）、409（ロールアウト実行中は `rollout_running`、ノードが落ちている）、503（リーダーなし、またはプロトコルが古いノードがいる）
- 変化はイベント `update.updated` で届く

### 5.5 オペレーション

| メソッド | パス | 応答 | action |
|---|---|---|---|
| `GET` | `/v1/operations` | 200 一覧（新しい順） | `operation.list` |
| `GET` | `/v1/operations/{id}` | 200 | `operation.get` |

絞り込み: `?state=running`、`?kind=branch.create`、`?target=branch/feature-x`。取り消しは当面提供しない。

### 5.6 その他

```
GET /v1/capabilities
{
  "version": "0.1.0",
  "engines": ["mysql"],
  "modules": [],
  "features": { "proxysql": false, "cluster": false }
}
```

- `modules`: 有効な有償モジュール。例: `[{"name": "audit", "version": "1.0.0", "licensed_until": "2027-11-09T00:00:00Z"}]`
- コンソールはここを見て、表示する画面を切り替える
- 認証なしで呼べるのは `GET /v1/capabilities`（`version` と `modules[].name` だけを返す縮小版）、`POST /v1/auth/login`、`GET /v1/healthz` だけ

```
GET /v1/healthz  → 200 {"status":"ok"} / 503 {"status":"degraded","reasons":["no_leader"]}
```

## 6. 未決事項

- ~~ダンプの受け渡し~~ → `POST /v1/uploads`（§5.2）で解決。ノード上のパスも引き続き使える
- `init_sql` の上限（1 MiB）と、ファイル参照（ノード上のパス）も許すか
- TTL の延長（`PATCH` の `ttl`）を OSS に含めるか
- リセット中の `status` を `provisioning` で表すか、`resetting` を追加するか
- ベースとスナップショットの削除 API（design.md では世代数による自動削除のみ）
- ブランチ用 DB ユーザーのパスワードの扱い（固定・作成時に生成・ローテーション）
- `GET /v1/events` を `Last-Event-ID` で再送できる期間（Raft ログの保持量に依存）
- オペレーションの取り消し（特に大きなランタイムのダウンロード）
- PostgreSQL 版の `connection` / `dsn` の形（`postgres://`、`sslmode`）
