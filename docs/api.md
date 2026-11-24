# db-branch REST API 設計

ステータス: 1.0 (GA) 基準 / 最終更新: 2026-11-24

対応する機械可読な定義: [`api/openapi.yaml`](../api/openapi.yaml)（OpenAPI 3.1、version `1.0.0`）。全体設計は [design.md](design.md) を参照。

`api/openapi.yaml` は、エージェントが実際に返すコア API の全ルート（メソッド・パス・スキーマ・エラーコード・認証・`x-dbb-action`・トークンのスコープ）を記述する公開 API リファレンスで、Redoc で表示できる（`npx @redocly/cli lint api/openapi.yaml` で検証）。`internal/interface/httpapi/openapi_test.go` が、サーバーが登録したルートとこのファイルのオペレーションが過不足なく一致すること、`operationId` が一意でタグとエラー応答があること、`$ref` がすべて解決できることを検査する。ルートを足したら同じ変更で `openapi.yaml` も更新する（さもないと `make test` が失敗する）。有償モジュールの `/v1/ext/<module>/...` はモジュールが自分で文書化し、`openapi.yaml` には載せない。

この文書は REST API の設計ガイドおよび運用上の概念解説であり、スキーマやパラメータの厳密な定義は [`api/openapi.yaml`](../api/openapi.yaml) を正とする。

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

すべてのエラーは同じ形で返す（`code` と `message` のみ）。

```json
{
  "error": {
    "code": "not_found",
    "message": "branch feature-x: not found"
  }
}
```

- `code`: 機械向けの安定した値（下表は主なエラーコード。エンドポイント固有の詳細は `api/openapi.yaml` を参照）。クライアントはこれで分岐する
- `message`: 人間向けの英文。内容は変わりうるので、プログラムで解釈しない

| HTTP | code | 意味 |
|---|---|---|
| 400 | `invalid_request` | JSON が壊れている、未知のフィールド、必須項目不足、不正な名前・ランタイム・TTL・設定値 |
| 400 | `weak_password` | 新しいパスワードが強度要件を満たさない |
| 401 | `unauthenticated` | セッション・トークンがない、または無効・失効 |
| 401 | `invalid_credentials` | ユーザー名またはパスワード誤り |
| 403 | `csrf_failed` | セッション Cookie 使用時に有効な `X-CSRF-Token` がない |
| 403 | `forbidden` | 呼び出し元またはトークンスコープに権限がない |
| 403 | `password_change_required` | 評価用初期パスワードの変更が完了するまで他操作不可 |
| 403 | `socket_only` | ローカル UNIX ソケット経由でのみ呼べる操作 |
| 403 | `managed_elsewhere` | 外部認証プロバイダ管理のパスワードのため変更不可 |
| 404 | `not_found` | リソースが存在しない |
| 404 | `runtime_not_installed` | 指定のランタイムが対象ノードに入っていない |
| 409 | `already_exists` | 同名リソースまたは同ランタイムが既に存在する |
| 409 | `conflict` | 状態や前提条件が合わない（例: `ready` でないブランチの操作、OS 不一致、`cluster remove` のクォーラム割れ） |
| 409 | `in_use` | 参照中のため削除できない（例: ベースが使用中のランタイム、ブランチが残るノード） |
| 409 | `unclean_shutdown` | DB サーバーの正常停止を確認できず、スナップショットを取れなかった |
| 409 | `feature_not_supported` | エンジンのランタイム契約がその機能に対応していない |
| 409 | `other_node` | 担当ノードではない（内部エラー／構成不整合） |
| 409 | `rollout_running` | ローリングアップデート実行中 |
| 409 | `checkpoints_unavailable` | モジュール／共有ストレージ上のためチェックポイント非対応 |
| 409 | `not_routable` / `no_proxy` | ProxySQL ルーティング未導入、またはブランチがプロキシ経由で到達不能 |
| 409 | `not_clustered` | クラスタ未構成のノードでクラスタ専用操作を実行 |
| 409 | `auto_renew_off` / `not_renewable` / `in_progress` | ライセンス自動更新無効、更新非対応キー、または更新実行中 |
| 411 | `length_required` | `POST /v1/uploads` で `Content-Length` ヘッダが欠落 |
| 413 | `too_large` | アップロード・JSON・MCP 本文の上限超過 |
| 422 | `runtime_unavailable` | カタログにない・このアーキテクチャ向けがない |
| 422 | `upgrade_not_allowed` | 許可されないアップグレード経路（ダウングレード、LTS 飛び越し等） |
| 422 | `invalid_license` / `license_unusable` | ライセンスキーの署名不正、または期限切れ・ノード上限超過 |
| 422 | `query_failed` / `restore_failed` | `base sql` のクエリ失敗、またはバックアップ復元の失敗 |
| 429 | `rate_limited` | ログイン試行や匿名作成等の回数制限超過（`Retry-After` ヘッダ付き） |
| 429 | `quota_exceeded` | ユーザー別最大ブランチ数 (`branch.max_per_user`) 超過 |
| 500 | `internal` | 想定外の内部エラー（詳細はノードのログのみに出力） |
| 501 | `cannot_verify` / `not_implemented` | ライセンスルート公開鍵のないビルド、または単一ノード未対応機能（バックアップ等） |
| 503 | `no_leader` | Raft リーダー不在（選出中・過半数割れ） |
| 503 | `no_capacity` | 配置・再開に必要なメモリ予算に空きがない |
| 503 | `not_initialized` | `db-branch init` 未実行 |
| 503 | `modules_pending` | クラスタ記録済みの拡張モジュールがノードで未読み込み |
| 503 | `cluster_upgrading` | クラスタ内の全ノードが必要なプロトコルに達していない |
| 503 | `audit_unavailable` | `audit.fail_closed = true` 時に監査ログの記録に失敗 |
| 507 | `no_space` | ZFS プール空き容量が最小ガード（10% または 1 GiB）未満 |

### 2.4 一覧とページング

すべての一覧エンドポイントは統一形式で全件を返します。

```json
{ "items": [ ... ], "next_cursor": null }
```

- 1.0 では `limit` や `cursor` クエリパラメータはなく、`next_cursor` は常に `null` です
- 並び順はエンドポイントごとに固定（ブランチとベースは名前順、オペレーションは新しい順）

### 2.5 長時間の操作（オペレーション）

次の操作は数秒〜数十分かかるため、受け付けた時点で `202 Accepted` を返します。

| 操作 | kind |
|---|---|
| ランタイムのインストール（全ノードへの配布を含む） | `runtime.install` |
| ベースの作成（ダンプの流し込み） | `base.create` |
| ベースのダンプ再読込 | `base.load` |
| ベースの定期／手動リフレッシュ | `base.refresh` |
| ベーススナップショットの作成と伝搬 | `base.snapshot` |
| ブランチの作成 | `branch.create` |
| ブランチのフォーク | `branch.fork` |
| ブランチの履歴ラインマージ | `branch.merge` |
| ブランチのリセット | `branch.reset` |
| ブランチの一時停止／再開 | `branch.pause` / `branch.resume` |
| ブランチのチェックポイント復元 | `branch.restore` |
| ブランチのアップグレード | `branch.upgrade` |
| ブランチの別ノード再作成 | `branch.recreate` |
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
- `phase`: 実行中の段階（`running` のときだけ。例: `base.snapshot` は `stopping` → `snapshotting` → `restarting`、`branch.create` は `cloning` → `starting` → `init_sql` → `stopping` → `snapshotting` → `starting`）
- `progress`: コンソールの `2/3` 表示にそのまま使う。`unit` は `nodes`（配布・伝搬）か `bytes`（ダウンロード・流し込み）。分からないときは `null`
- `error`: 失敗時にメッセージ文字列またはエラー情報が入る
- 対象リソース（ブランチなど）自体も作成直後から `GET` で見え、`status` で状態を表す。オペレーションは「その操作の経過」、リソースは「今の状態」を表す
- 完了したオペレーションは最新 200 件を保持する

### 2.6 イベント（SSE）

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

- `event`: `<resource>.updated` / `<resource>.deleted`（`branch`, `base`, `runtime`, `node`, `operation`, `setting`, `module`, `update`）
- `data`: `updated` は単体取得と同じ表現。`deleted` は `{"name": ...}`（オペレーションは `{"id": ...}`）
- `id`: イベントバスの連番。`Last-Event-ID` を付けて再接続すれば取りこぼした分から再送する。古すぎて再送できないときは `event: resync` を送るので、クライアントは一覧を取り直す
- 15 秒ごとにコメント行（`: ping`）を送って接続を保つ
- 受け取れるイベントは呼び出し元の権限で絞る。管理者は全部。それ以外は、自分（と SharesBranches で共有する相手）のブランチ・オペレーション、open なブランチ、`base.read` などのアクションが許された種類のイベントだけ。宛先を持たないイベントは管理者だけに送る（既定は閉じる）。絞った分の ID は飛ぶ

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
- ログアウト（`POST /v1/auth/logout`）の失効記録は全ノードがプロトコル 24 以上である必要がある。混在クラスター（更新中）では、受けたノードの手元でのみ失効して保留キューに積み、`503 cluster_upgrading` を返す（利用者に成功とは見せず、更新完了後に再ログアウトできる案内を返す）。全ノードの更新が完了すると保留分は自動で Raft に記録されるが、**既知の制約として、更新完了前にそのノードを再起動するとメモリ上の失効・保留が失われ、他ノードと同様に期限切れまたは更新後の再ログアウトまでセッションが有効のまま残る**
- ログインの失敗が続いたら `429 rate_limited`（`Retry-After` 付き）
- パスワードを変更すると、今のセッション以外はすべて無効になる
- パスワードのリセット（忘れたとき）は API にしない。ノード上で `sudo db-branch admin reset-password` を実行する
- OSS 版の `username` は `admin` だけ

### 3.2 ローカル CLI（UNIX ソケット）

- `/run/db-branch/agent.sock` で同じ API を提供する。HTTP をソケット上で話す
- 接続元のプロセスの UID/GID を `SO_PEERCRED` で確かめ、root か `db-branch-admin` グループなら認証済みとして扱う（DB サーバーが動く `db-branch` グループは含めない）。Cookie・CSRF は不要
- 主体（principal）は `local:<ユーザー名>` として監査イベントに記録する
- ソケット限定の操作: `POST /v1/auth/reset`、`POST /v1/backups/restore`、`POST /v1/cluster/token`。TCP（HTTPS）およびノード間 API（`/internal/v1/api/` 経由の転送）から呼ぶと `403 socket_only`
- クラスタの初期化・参加（`cluster init` / `join`）は API にせず CLI だけで行う（参加トークンと証明書の発行を伴うため）。`POST /v1/cluster/token` は 1 回限り・有効期限付き（省略時は設定 `cluster.join_token_ttl`、既定 `24h`。任意で `{"ttl": "1h"}` を指定可）のトークンを発行し、`token` と `expires_at` を返す

### 3.3 拡張ポイントとの関係

すべてのリクエストは次の順に通る。OSS 版でもこの経路は省略しない。

```
リクエスト → Authenticator → Authorizer → ハンドラ → (変更系なら) AuditSink
```

| ポート | OSS 版 | 有償 `auth` / `audit` モジュール |
|---|---|---|
| `Authenticator` | セッション Cookie（admin のみ）、ソケットの UID 確認、API トークン（`Authorization: Bearer dbb_...`） | 複数ユーザー・SSO（OIDC / SAML） |
| `Authorizer` | 認証済みならすべて許可 | RBAC。拒否時は `403 forbidden` |
| `AuditSink` | 構造化ログに出すだけ | 永続化・検索・外部転送 |

- **OSS 版の認可設計境界**: 有償 `auth` モジュール（`ext.Authorizer`）を導入しない OSS 版単独では、匿名（`anonymous`）・`branches` スコープのトークン制限・ブランチ所有判定・ローカルソケット限定操作・初期パスワード変更前制限を除き、認証済みの主体はロールを問わず管理者相当の操作（`cluster.remove`、`setting.write`、`base.delete` 等）を実行できる設計です。ロールに基づく RBAC は有償モジュール側の責務です。

- 認可の単位は `action`（例: `branch.create`、`branch.delete`、`runtime.install`）と対象リソース。各エンドポイントの `action` は OpenAPI の `x-dbb-action` に書く
- **変更系の呼び出しはすべて**監査イベントを出す。成功・失敗（認可拒否を含む）の両方を出す
- 認証に失敗した要求（無効なトークン `auth.token`、期限切れのセッション `auth.session`、CSRF トークンの欠落 `auth.csrf`）も `denied` として記録する。対象のリソースは、パスが指すもの（ブランチ・ベース名、トークン ID、設定のスコープ、バックアップのファイル名、`<engine>@<version>`）。利用者が打った名前に所有者の接頭辞が付く場合は、付いたあとの名前を記録する
- 監査の記録に失敗しても、既定では変更を止めずエージェントのログに残す（fail-open）。設定 `audit.fail_closed` を `true` にすると、変更の前に「`attempt`」を記録し、記録できなければ `503 audit_unavailable` で変更を行わない（記録対象でない読み取りには影響しない）。ローカルソケット（`db-branch` CLI）は常に fail-open で、監査先が止まっても `db-branch settings` でこの設定を戻せる。対象は API のルート経由の変更で、モジュールの公開パス（`/v1/ext/...` の SSO など）とクラスタ内部の mask 開始は含まない。`attempt` の対象名は、利用者が打った名前（所有者の接頭辞が付く前）
- 認可（`ext.Authorizer`）に渡る `Resource.Name` は、トークン ID・設定のスコープ・バックアップのファイル名・`<engine>@<version>` も持つようになった（以前は空）。Cookie も Bearer もない 401 は、何も提示されていないので記録しない

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

### 3.4 API トークン（Bearer）

スクリプト・CI・MCP クライアント向けの認証手段。`Authorization: Bearer dbb_<id>_<secret>` ヘッダで渡す（Cookie や CSRF トークンは不要）。秘密は発行時に 1 度だけ返し、ストア（単一ノードは `state.json`、クラスタは Raft）には SHA-256 ハッシュのみを保存する。無効・期限切れ・取り消し済みのトークンは `401 unauthenticated` を返す。

- **種別**:
  - **サービス用**（`POST /v1/tokens`、管理者のみ）: 人に紐付かず、主体 `token:<id>` として動く。スコープは `branches`（既定）または `full`
  - **個人用**（`POST /v1/account/tokens`、ログイン中の本人）: 作った本人として動き（発行時の `admin` 以外のロールも引き継ぐ）、本人の所有ブランチや `auth` モジュールのロールの範囲で操作できる。スコープは `branches` 固定（`admin` ロールは引き継がない）。トークンから別のトークンを発行・変更・削除することはできない（`403 forbidden`）
- **スコープと認可（ホワイトリスト）**:
  - `full`: 管理者と同等（ソケット限定操作を除く）
  - `branches`: 明示的なホワイトリストにある操作のみを許可し、それ以外はすべて `403 forbidden` で拒否する
    - ブランチ操作: `branch.read`, `branch.create`, `branch.extend`, `branch.delete`, `branch.reset`, `branch.pause`, `branch.resume`, `branch.fork`, `branch.recreate`, `branch.checkpoint.read`, `branch.checkpoint`, `branch.restore`, `branch.checkpoint.delete`, `branch.upgrade`, `branch.merge`, `branch.connect`, `branch.query`（互換のため旧名別名 `branch.update`, `branch.checkpoint_delete` も同様に許可）
    - 参照系・MCP: `base.read`, `base.public`, `runtime.read`, `operation.read`, `event.read`, `capability.read`, `metrics.read`, `account.preference.read`, `account.token.read`, `mcp.use`
    - 拒否対象の例: 拡張モジュール API（`ext.<module>.read` / `ext.<module>.write`）、ライセンス（`license.*`）、クラスタ構成・ノード情報（`cluster.*`）、モジュール管理（`module.*`）、更新管理（`update.*`）、設定・トークン・ルートユーザー・バックアップ・ベース変更・リフレッシュ設定（`setting.*`, `token.*`, `route-user.*`, `backup.*`, `base.refresh*` 等）
- **失効と期限切れ GC**:
  - 有償 `auth` モジュールで利用者（`user:<email>`）を削除すると、その利用者に紐付く個人用トークンは即座にすべて失効・削除される（サービス用トークンは影響を受けない）
  - `expires_at` を過ぎた期限切れトークンは、定期スイープ（GC。クラスタでは Raft リーダーが実行）でストアから自動的に削除される

## 4. クラスタでの振る舞い

- **どのノードでも受け付ける**。変更系のリクエストやリソース担当ノードに依存する操作は、受けたノードがリーダーまたは担当ノードへ転送する（クライアントからは見えない）
- リーダーがいないときは `503 no_leader` と `Retry-After: 2` を返す。既存ブランチへの DB 接続（ProxySQL → DB サーバー）はこの間も止まらない
- **読み取りは受けたノードの Raft 状態から返す**（書き込み後に他ノードへ即座にフォールバックせずとも、書き込み応答を受けた後に最新状態を参照できるよう、状態更新の適用後に応答する）
- 削除・状態遷移は SSE にも流れるので、コンソールは SSE を正として表示を更新する

## 5. リソース

### 5.1 ブランチ

| メソッド | パス | 応答 | action |
|---|---|---|---|
| `GET` | `/v1/branches` | 200 一覧 | `branch.read` |
| `POST` | `/v1/branches` | 202 オペレーション | `branch.create` |
| `GET` | `/v1/branches/{name}` | 200 | `branch.read` |
| `PATCH` | `/v1/branches/{name}` | 200（TTL の変更のみ） | `branch.extend` |
| `DELETE` | `/v1/branches/{name}` | 202 オペレーション | `branch.delete` |
| `POST` | `/v1/branches/{name}/reset` | 202 オペレーション | `branch.reset` |
| `POST` | `/v1/branches/{name}/pause` | 202 オペレーション | `branch.pause` |
| `POST` | `/v1/branches/{name}/resume` | 202 オペレーション | `branch.resume` |
| `POST` | `/v1/branches/{name}/fork` | 202 オペレーション | `branch.fork` |
| `POST` | `/v1/branches/{name}/recreate` | 202 オペレーション | `branch.recreate` |
| `GET` | `/v1/branches/{name}/dsn` | 200 接続文字列（パスワード入り） | `branch.connect` |
| `POST` | `/v1/branches/{name}/query` | 200 クエリ結果 | `branch.query` |
| `GET` | `/v1/branches/{name}/checkpoints` | 200 一覧（全ての線のチェックポイント。古い順） | `branch.checkpoint.read` |
| `POST` | `/v1/branches/{name}/checkpoints` | 201 チェックポイント（スナップショットは一瞬なので同期） | `branch.checkpoint` |
| `POST` | `/v1/branches/{name}/checkpoints/{checkpoint}/restore` | 202 オペレーション | `branch.restore` |
| `DELETE` | `/v1/branches/{name}/checkpoints/{checkpoint}` | 204 | `branch.checkpoint.delete` |
| `POST` | `/v1/branches/{name}/upgrade` | 202 オペレーション（`result` に結果） | `branch.upgrade` |
| `POST` | `/v1/branches/{name}/merge` | 202 オペレーション（`result` に結果） | `branch.merge` |

チェックポイント（[checkpoints.md](checkpoints.md)）:

```json
POST /v1/branches/feature-x/checkpoints
{ "name": "before-migration" }          // 省略可。既定は UTC の時刻（20261112-153000）

201 Created
{ "name": "before-migration", "created_at": "2026-11-12T15:30:00Z", "size": 98304, "line": 0, "auto": false, "current": true }
```

- 一覧は ZFS から読む（Raft には置かない）。担当ノード以外に届いた要求は担当ノードへ転送する
- `size` はそのチェックポイントだけが持つ容量、`line` は履歴の線の番号（復元のたびに新しい線ができる）、`auto` はシステムが自動取得したもの（アップグレード前の `before-upgrade-*`、または旧版が復元前に取った `before-restore-*`）、`current` は現在のデータの元になったもの
- 上限は設定 `branch.max_checkpoints`（既定 20）。数えるのは手動のチェックポイントだけで、超える作成は `409 conflict`。自動のもの（アップグレードの前の `before-upgrade-<時刻>`、以前の版が復元の前に取った `before-restore-<時刻>`）は数えず、復元を断る理由にもならない。自動のものは新しい 5 件だけ残り、6 件目ができるアップグレードは一番古いものを通常の削除と同じ手順で消す
- 作成（`201`）の応答には、ブランチの使用量（ライブ + 履歴）がプールの空きの半分を超えるとき `warning`（英文の文字列）が付く。作成は断らない。空きが不明なら付かない
- 復元（`branch.restore`）はブランチが `ready` のときだけ。いまの状態は置き換わり、最後のチェックポイントより後の変更は復元で失われる（自動では保存されない。残したい場合は先に `POST …/checkpoints` で取る。取った他のチェックポイントは全て残る）。進捗は `cloning` `stopping` `swapping` `promoting` `starting`
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
- `init_sql` はリクエストに直接入れる（リクエスト JSON 本文全体で上限 2 MiB）。適用後にリセットポイントを取る。**ブランチのログインとして**実行するので、管理者の権限が要る SQL（`CREATE USER`、`GRANT`、`COPY ... PROGRAM` など）は使えない。PostgreSQL は平文の SQL と `\connect` だけを受け付け、MySQL はクライアントのコマンド（`system` など）を無効にして流す。ログインなしの呼び出しは `init_sql` を渡せない（403）
- `ttl` を省略すると期限なし（ただし `branch.max_ttl` 設定時はその値が適用される）。`"0"` は不可（省略で表す）

表現:

```json
{
  "name": "feature-x",
  "status": "ready",
  "status_reason": null,
  "base": "prod-masked",
  "snapshot": "s20261109",
  "runtime": "mysql@8.4.13",
  "base_runtime": "mysql@8.4.13",
  "node": "node2",
  "created_at": "2026-11-09T12:00:00Z",
  "expires_at": "2026-11-12T12:00:00Z",
  "reset_at": "2026-11-09T12:00:08Z",
  "storage": { "used": 1048576, "referenced": 524288000 },
  "connection": {
    "host": "node2",
    "port": 40001,
    "user": "feature-x"
  },
  "proxy": {
    "host": "node2",
    "port": 6033,
    "user": "feature-x"
  },
  "operation": null,
  "owner": "admin",
  "owner_name": "admin",
  "id": "admin/feature-x",
  "access": "private",
  "record": false,
  "checkpoints": true
}
```

- `status`: `provisioning` / `ready` / `resetting` / `restoring` / `upgrading` / `pausing` / `paused` / `resuming` / `forking` / `merging` / `failed` / `lost` / `deleting`
- `status_reason`: `failed`・`lost` 等での短い英文
- `expires_at`: 期限なしなら `null`。コンソールは `created_at` から `expires_at` までの残りで TTL バーを描く
- `storage.used` はこのブランチ固有の差分、`storage.referenced` はベースと共有している分を含む全体（ZFS の値そのまま）
- `checkpoints`: ブランチを持つノードのストレージがチェックポイントに対応しているか（そのノード自身の ZFS なら `true`。モジュール／共有ストレージでは `false`。クラスタではブランチのノードが報告した値で、未報告の古いノードも `false`）。`false` のとき、チェックポイントの API は `409`（エラーコード `checkpoints_unavailable`）を返し、コンソールはチェックポイントの操作を出さない
- `connection`: 直接つなぐ場合。ProxySQL 経由で到達可能な場合は `proxy` が付く（そうでなければ `null`）
- `operation`: 進行中のオペレーションがあればその ID と kind。なければ `null`
- `GET /v1/branches/{name}/dsn` は `{"dsn": "mysql://feature-x:<password>@node2:40001/"}`（`?via=proxy` 指定時はプロキシ経由）を返す。パスワードは一覧・単体取得には含めない。この呼び出しは読み取りだが監査イベントを出す

### 5.2 ベースとスナップショット

| メソッド | パス | 応答 | action |
|---|---|---|---|
| `GET` | `/v1/bases` | 200 一覧 | `base.read` |
| `GET` | `/v1/public/bases` | 200 公開ベース一覧（認証不要） | `base.public` |
| `POST` | `/v1/bases` | 202 オペレーション | `base.create` |
| `GET` | `/v1/bases/{name}` | 200 | `base.read` |
| `DELETE` | `/v1/bases/{name}` | 202 オペレーション | `base.delete` |
| `POST` | `/v1/bases/{name}/snapshots` | 202 オペレーション（作成と伝搬） | `base.snapshot` |
| `PATCH` | `/v1/bases/{name}/snapshots/{snapshot}` | 200 ベース（`{"public": true}` で公開） | `base.publish` |
| `DELETE` | `/v1/bases/{name}/snapshots/{snapshot}` | 204 | `base.snapshot.delete` |
| `PATCH` | `/v1/bases/{name}` | 200 ベース（`{"description": "..."}`、1000 バイトまで） | `base.describe` |
| `PUT` | `/v1/bases/{name}/replicas` | 200 ベース（`{"replica_nodes": null}` で全ノード、`[]` で起点ノードだけ、`["n2","n3"]` でそれらと起点）。存在しないノードは 400 | `base.replicas` |
| `PATCH` | `/v1/bases/{name}/anonymous` | 200 ベース（ログインなしの上限。`{"max_branches": 3, "max_ttl": "4h"}`。書いたキーだけ変わり、`null` は継承に戻す。負の数・0 以下の期間・型違いは 400） | `base.publish` |
| `POST` | `/v1/bases/{name}/start` | 200 ベース（ベースの DB サーバーを起動） | `base.start` |
| `POST` | `/v1/bases/{name}/stop` | 200 ベース（ベースの DB サーバーを停止） | `base.stop` |
| `POST` | `/v1/bases/{name}/load` | 202 オペレーション（`{"dump": "upload:<id>" か ノード上のパス, "snapshot": true}`） | `base.load` |
| `POST` | `/v1/uploads?name=x.sql.gz&node=N` | 201 `{"id":"upload:<id>","size":...,"node":...}`（本文はファイルそのもの） | `base.upload` |
| `GET` | `/v1/uploads/room?node=N` | 200 `{"max_bytes":...,"reason":"limit"/"pool"}` | `base.upload` |
| `GET` | `/v1/bases/{name}/refresh` | 200 定期更新の設定（パスワードは返さず `password_set` だけ）。無ければ 404 | `base.refresh.read` |
| `PUT` | `/v1/bases/{name}/refresh` | 200 設定（下記）。パスワードを省くと、保存済みのものを使い続ける | `base.refresh` |
| `DELETE` | `/v1/bases/{name}/refresh` | 204（定期更新をやめ、接続先とパスワードを忘れる） | `base.refresh` |
| `POST` | `/v1/bases/{name}/refresh/run` | 202 オペレーション（`base.refresh`。今すぐ更新する） | `base.refresh` |
| `POST` | `/v1/bases/{name}/sql` | 204（`{"sql": "..."}` をベースのサーバーで管理者として実行） | `base.sql` |

（`base mask` はマスカーモジュールの別リポジトリ分離に伴い、1.0 の公開 API からは非露出としています。）

作成:

```json
POST /v1/bases
{
  "name": "prod-masked",
  "runtime": "mysql@8.4.13",
  "dump": "/srv/dumps/prod-masked.sql.gz"
}
```

- `dump` はベースオリジンになるノード（`node` 指定先、省略時はリクエストを受けたノード）上のパス、または `POST /v1/uploads` で得た `upload:<id>`。HTTPS 経由ではローカルパスを指定できない（`upload:<id>` またはローカルソケットが必要）

ベースの表現（主要フィールド。全フィールドは `api/openapi.yaml` の `Base` スキーマ参照）:

```json
{
  "name": "prod-masked",
  "runtime": "mysql@8.4.13",
  "status": "ready",
  "status_reason": null,
  "node": "node1",
  "port": 40000,
  "created_at": "2026-11-09T10:00:00Z",
  "latest_snapshot": "s20261109",
  "snapshots": [ ... ],
  "replicas": { "s20261109": ["node2", "node3"] },
  "replica_nodes": null,
  "holders": [
    { "node": "node1", "state": "held", "origin": true },
    { "node": "node2", "state": "held", "origin": false },
    { "node": "node3", "state": "pulling", "origin": false }
  ],
  "operation": null,
  "description": "",
  "anonymous": { "max_branches": null, "max_ttl": null },
  "can": { ... }
}
```

スナップショットの表現（`Base.snapshots` の要素）:

```json
{
  "name": "s20261109",
  "created_at": "2026-11-09T11:00:00Z",
  "public": false
}
```

- `holders[].state`: `held` / `pulling` / `draining` / `skipped`
- `GET /v1/bases` と `GET /v1/bases/{name}` の各ベースには `can`（action 名から true/false への対応）が付く。呼び出した人がそのベースに対してできる操作で、認可の拡張点に `Resource{Kind:"base", Name:<ベース名>}` を渡して調べる（ベースごとのメンテナーの権限が反映される）。対象は `base.snapshot` / `base.load` / `base.delete` / `base.describe` / `base.publish` / `base.sql` / `base.start` / `base.stop` / `base.refresh` / `base.refresh.read` / `base.snapshot.delete` / `base.replicas`（互換のため `base.snapshot_delete` 別名も許容）。イベント（`base.updated`）には付かない。コンソールは `can` を見てボタンを出し、無ければセッションの全体の権限（`can`）を使う
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
| `GET` | `/v1/runtimes` | 200 一覧（`?node=` でノード指定可） | `runtime.read` |
| `GET` | `/v1/runtimes/available` | 200 カタログ | `runtime.read` |
| `POST` | `/v1/runtimes` | 202 オペレーション | `runtime.install` |
| `DELETE` | `/v1/runtimes/{engine}/{version}` | 204 | `runtime.remove` |

```json
POST /v1/runtimes          {"runtime": "mysql@8.4.13"}
GET  /v1/runtimes/available?engine=mysql
→ {"items": [{"runtime": "mysql@8.4.13", "engine": "mysql", "version": "8.4.13"}], "next_cursor": null}
```

- インストールはリーダーが外部から取得し、他のノードはリーダーから受け取る（design.md §4）
- 削除は全ノードから消す。ベースやブランチが使っていれば `409 in_use`

### 5.4 クラスタとノード

| メソッド | パス | 応答 | action |
|---|---|---|---|
| `GET` | `/v1/cluster` | 200 | `cluster.read` |
| `POST` | `/v1/cluster/token` | 200（ソケット限定、1 回限り・期限付き） | `cluster.token` |
| `DELETE` | `/v1/cluster/nodes/{name}` | 200 外した内訳（`?force=true` で、残る記録を忘れて外す） | `cluster.remove` |
| `GET` | `/v1/nodes/{name}/metrics` | 200 ノードの最新メトリクスサンプル | `cluster.read` |
| `GET` | `/v1/nodes/{name}/storage` | 200 ノードの ZFS データセット／スナップショット一覧 | `cluster.read` |

```json
GET /v1/cluster
{
  "mode": "cluster",
  "self": "node1",
  "leader": "node1",
  "protocol": 28,
  "service_ids": { "uid": 109, "gid": 112 },
  "nodes": [ ... ]
}
```

ノードの表現（`nodes[]` の要素）:

```json
{
  "name": "node1",
  "host": "node1",
  "address": "192.0.2.10",
  "version": "1.0.0",
  "leader": true,
  "voter": true,
  "down": false,
  "pool_size": 53150220288,
  "pool_free": 52101644288,
  "runtimes": ["mysql@8.4.13"],
  "protocol": 28,
  "modules": [],
  "metrics": null,
  "arc_max_mb": 1024,
  "system": { "distro": "ubuntu-26.04", "os": "Ubuntu 26.04.1 LTS", "kernel": "7.0.0-14-generic", "zfs": "2.4.0-1ubuntu1" },
  "service_ids": { "uid": 109, "gid": 112 },
  "last_seen": "2026-11-24T12:00:00Z"
}
```

- `system`: ノードの OS（ディストリビューションのキー `distro`、表示名 `os`）、カーネル（`uname -r`）、ZFS の版（ユーザーランド。カーネルモジュールの版が違えば `2.3.1-1 (kmod 2.2.2-0ubuntu9)` のように続ける）。ハートビートで運ばれ、古いノードが報告しない項目は `"unknown"`。ノードごとに OS が違うクラスタでの PostgreSQL の扱いは [mixed-os.md](mixed-os.md)。PostgreSQL のベースを別ディストリビューションのノードに置く指定（`node`、`replica_nodes`）は `409 conflict` で断られる
- `service_ids` / `id_problem`: ノードの `db-branch` ユーザーの uid/gid と、クラスタのものと食い違うときの理由（[service-user-ids.md](service-user-ids.md)）。`GET /v1/cluster` のトップレベルの `service_ids` はクラスタの値。食い違うノードには新しいブランチもレプリカも置かれず、`node` で指定すると `409 conflict`
- 単一ノード構成でも `mode: "single"` として同じ形で返す
- コンソールは `(pool_size - pool_free) / pool_size` で容量バーを描き、80% で琥珀色、90% で赤にする

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
| `GET` | `/v1/operations` | 200 一覧（新しい順） | `operation.read` |
| `GET` | `/v1/operations/{id}` | 200 | `operation.read` |

見えるのは、自分が始めたオペレーション、SharesBranches で共有する相手のもの、管理者は全部。システムが始めたもの（ランタイムの導入、定期リフレッシュ）は管理者だけ。見えないオペレーションは存在しないときと同じ 404 を返す。

### 5.6 その他

```
GET /v1/capabilities
{
  "version": "1.0.0",
  "node": "node1",
  "modules": [],
  "pages": [],
  "record": false,
  "engines": [ ... ]
}
```

- `modules`: 有効なモジュール一覧（`"name version"` の文字列配列）
- コンソールはここを見て、表示する画面や機能を切り替える
- 認証なしで呼べるのは `/healthz`、`/readyz`、`/tls.pem`、`GET /v1/auth/methods`、`POST /v1/auth/login`、`GET /v1/public/bases`、匿名作成可能な `POST /v1/branches` およびその匿名ブランチの参照・`dsn`・オペレーション参照だけ

```
GET /healthz  → 200 "ok\n"
GET /readyz   → 200 "ready\n" / 503 "not ready\n"
```
