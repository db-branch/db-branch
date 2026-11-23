# Branch のフォーク（既存ブランチからの分岐）

稼働中または一時停止中の既存ブランチ（親ブランチ）の現在の状態、または親ブランチの既存のチェックポイントから、独立した新しいブランチ（子ブランチ）を切り出す（Fork）。

元のブランチには影響を与えずに、ある作業の途中状態から並行して別の実験・検証・デバッグを行ったり、AI エージェントに安全な分岐環境を渡したりする用途を想定している。

## GA (`origin/main`) での再照合

本仕様書は `origin/main`（`9f83d51`、GA 修正およびセキュリティ強化反映後）に対して全根拠を再照合し、仕様・前提を更新した。主な反映点は以下のとおり。

| 項目 | 旧仕様（`v0.2.7` 時点） | 現行仕様（GA / 1.0） | 理由 |
|---|---|---|---|
| Raft プロトコル番号（§4.5） | `ProtocolBranchFork = 19` | `ProtocolBranchFork = 25`（`raftstore.Protocol = 25`） | W-17 でプロトコル予約が撤廃され、`origin/main` は `Protocol = 24`（`ProtocolBranchOwnerID = 23`, `ProtocolSessionRevocation = 24`）まで進んでいるため。`Command.Needs()` でも `insert_branch` / `put_branch` が `ParentBranch != ""` または `BranchForking` を運ぶ場合に `ProtocolBranchFork` を要求する |
| 所有権と可視性（W-03 / W-04、§3.2, §5） | `owner` を単一文字列で扱う前提 | `Branch.Owner` に不変 ID（`p.ID`）、`Branch.OwnerName` に表示名を分離。一覧・参照・SSE イベントは `canReadBranch` / `canManageBranch`（自分・`open`・`SharesBranches`・管理者）に従う | GA 修正（W-03, W-04）に揃え、Fork で作られた子ブランチにも同一の所有権・可視性・プレフィックス付与を適用する |
| リソース上限・空き容量ガード（W-15、§4.1, §5） | メモリ予算（`Placement`）とポートのみ | `branch.max_ttl`（`checkTTL`）・`branch.max_per_user`（`checkBranchCount` → `429 quota_exceeded`）・プール残量 10% / 1GiB ガード（`checkPoolRoom` → `507 no_space`）・`refquota`（`applyQuota`）・エンジン別 ZFS プロパティ（`applyEngineProps`）・`Placement`（メモリ予算・`IDProblem`）をすべて適用 | GA 修正（W-15）に揃え、Fork が既存の容量・クォータ保護を迂回しないようにする |
| 起動停止・復旧（W-13 / W-14、§4.5） | 単純な `ensureRunning` のみ | `Recover` の `BranchForking` 復旧で `startServerIfDown` → `releaseFreeze` → `putLoginRight`（失敗時は `stopBranchImmediate`、`ErrRecordRequired` 時は `markFailed`）を実行。`ForkBranch` 完了時の `saveBranch` 失敗時も `stopBranchImmediate` | GA 修正（W-13, W-14）の安全停止・権限維持・記録必須ブランチ保護に合わせる |
| MySQL `CreateUser` とバイナリログ（§4.2, §4.6） | `createUser` に `ALTER USER` を追加する前提 | `mysql/server.go` の `createUser` は GA 修正で `ALTER USER`（8.4 非 LegacyAuth / 9.x は `caching_sha2_password`）対応済み。`child.Record == true` のフォーク時は `CloneHistoryResetter`（MySQL 8.4+ は `RESET BINARY LOGS AND GTIDS`、それ未満は `RESET MASTER`）で親由来の binlog / GTID をリセットする | 親の履歴が子の history モジュールへ混入するのを防ぐ |
| 外部ランタイムと LiveFreeze（DECISIONS 8-b、§4.1, §5） | 組み込みエンジンのみ想定 | API 入口で `requireFeature(engine, "checkpoints")` を検査（非対応エンジンは `409 feature_not_supported`）。無停止ライブフォークは `withLiveFreeze` で囲み、`LiveFreeze` 宣言エンジンの静止点を保つ | 外部ランタイムモジュール（単一ノード Experimental）との整合性を保つ |

## 1. 既存機能との区別と前提

db-branch にはデータセットを扱う類似の操作がすでに存在する。`branch fork` はこれらと役割を明確に分ける。

| 操作 | 起点 | 結果 | 元のリソースへの影響 | 根拠 |
|---|---|---|---|---|
| `branch create` | ベース（`BaseSnapshot`） | 新規ブランチ | なし（リードオンリーのベーススナップショットを参照） | [branch.go](../internal/usecase/dbbranch/branch.go) |
| `branch checkpoint` / `restore` | 自ブランチのライブ状態 | 同一ブランチ内の復元（ポート・DSN 不変） | 自ブランチの現在状態が置き換わる | [checkpoints.md](checkpoints.md) |
| **`branch fork`** | **既存ブランチ（ライブ状態またはチェックポイント）** | **別の新規ブランチ（独自のデータセット・ポート・DSN）** | **なし（親ブランチは独立して稼働継続）** | 本仕様書 |

### 前提（現行コードからの導出）

- **1 ブランチ = 1 データセット + 1 DB サーバープロセス**: ブランチは `<pool>/db-branch/branches/<name>` と専用の systemd ユニット（`branch-<name>`）およびポートを持つ（[service.go](../internal/usecase/dbbranch/service.go), [branch.go](../internal/domain/branch.go)）。子ブランチも同じく独立したブランチとして振る舞う。
- **ZFS クローンの局所性と `Lineage` 依存**: ZFS の `zfs clone` は同一プール・同一ノード内でのみ完結する（[zfs.go](../internal/infrastructure/zfs/zfs.go)）。ベーススナップショットと異なり、ブランチのデータセットはクラスタ内の全ノードへ伝搬されない（[design.md](design.md)）。また、親の履歴ライン探索やスワップリセットにはノード自身の ZFS（`s.Lineage != nil`、[checkpoint.go](../internal/usecase/dbbranch/checkpoint.go)）を前提とする。
- **ブランチ単位のランタイム**: ブランチは `branch upgrade` によりベースとは異なるランタイムを持ちうる（[upgrade.md](upgrade.md)）。フォーク時はベースのランタイムではなく、親ブランチ（または指定チェックポイント）のランタイムを引き継ぐ。

## 2. 目的・ユースケース / 非目標

### 目的とユースケース

1. **並行実験と A/B 比較**: マイグレーションや大量データ投入を済ませたブランチ `feature-a` を壊さずに、そこから `feature-a-exp1` と `feature-a-exp2` をフォークし、異なるクエリやインデックス案を同時に試す。
2. **障害調査の切り出し**: 検証中に再現した不具合の状態をそのまま残しつつ、調査用の子ブランチを切ってデバッグや破壊的な検証を行う。
3. **AI エージェントの試行錯誤（MCP）**: LLM エージェントが自分の作業ブランチの途中状態から安全に別案を試す（`fork_branch`）。
4. **チェックポイントからの別ブランチ化**: 過去のチェックポイント（`before-upgrade-*` を含む）を「今のブランチを上書きする `restore`」ではなく「新しい別ブランチ」として起動し、新旧のデータを並べて比較する。

### 非目標（v1 スコープ外）

- **別ノードへのクロスノード・フォーク（ZFS send/recv による転送）**: 親ブランチが存在するノードとは別のノードへのフォークは行わない。
- **共有ストレージ（`s.Lineage == nil`）でのフォーク**: `family` 探索およびスワップリセットが使えないため、v1 ではノード自身の ZFS（`s.Lineage != nil`）のみを対象とし、共有ストレージやモジュールストレージでは `409 checkpoints_unavailable` を返す。
- **ブランチ間の双方向マージ / 差分反映**: フォーク後の親と子は完全に独立して書き換えられる（マージ機能は別仕様で扱う）。
- **ログインなし（Anonymous）ユーザーによるフォーク**: 公開ベースからの匿名作成と異なり、既存ブランチのデータを複製する操作であるため、認証済みユーザーのみを対象とする。

## 3. インターフェース

### 3.1 CLI

```sh
# 親ブランチの現在のライブ状態からフォーク
db-branch branch fork <source-branch> <new-name> [flags]

# 例: チェックポイントを指定してフォーク、TTL と init SQL を付与
db-branch branch fork feature-x feature-x-try --checkpoint before-migration --ttl 24h --init-file extra.sql
```

既存の `branch create`（[branch.go](../internal/interface/cli/branch.go)）とフラグ体系を揃える。

| フラグ | 説明 | 既定値 |
|---|---|---|
| `--checkpoint <name>` | 親ブランチの指定チェックポイント（または `@reset`）からフォークする。省略時は親の現在状態から新規スナップショットを取ってフォーク | なし（現在状態） |
| `--clean` | `--checkpoint` 省略かつ親が `ready` のとき、親 DB サーバーを一時的に正常停止してからスナップショットを取る（[§4.2](#42-整合性の取り方)） | `false`（無停止・クラッシュコンシステント） |
| `--init-sql <sql>` | フォーク直後の子ブランチで 1 回だけ実行する SQL（この後の状態が子の `@reset` になる） | なし |
| `--init-file <path>` | `--init-sql` をファイルから読み込む | なし |
| `--ttl <duration>` | 自動削除までの期間（例: `24h`）。未指定時はクラスタの既定 TTL（設定がある場合） | 既定 TTL / 無期限 |
| `--node <node>` | 配置先ノードの明示（親と異なるノードを指定した場合は `409 conflict`） | 親と同じノード |
| `--owner <user>` | 子ブランチの所有者（管理者のみ他者を指定可） | 実行ユーザー |
| `--access <mode>` | `private` または `open` | `private` |
| `--record` | history モジュールで変更を記録するか（[§4.6](#46-ポートランタイム設定proxysql-の引き継ぎ)） | `false`（または `record_all` 設定） |
| `--no-wait` | オペレーション ID だけを表示して即座に戻る | `false` |

出力形式は `branch create` と同一（完了まで待機してブランチ情報を表示、`--json` 対応）。`branch show` では `parent_branch` がある場合に親ブランチ情報を表示する。

### 3.2 REST API

既存の `/v1/branches` 体系（[server.go](../internal/interface/httpapi/server.go), [openapi.yaml](../api/openapi.yaml)）に追加する。

| メソッド | パス | 応答 | `x-dbb-action` | 概要 |
|---|---|---|---|---|
| `POST` | `/v1/branches/{name}/fork` | `202 Accepted`（オペレーション `branch.fork`） | `branch.fork` | `{name}` を親として新しいブランチを作成 |

リクエストボディ:

```json
POST /v1/branches/feature-x/fork
{
  "name": "feature-x-try",
  "checkpoint": "before-migration",
  "clean": false,
  "init_sql": "DELETE FROM audit_tmp;",
  "ttl": "24h",
  "node": "node1",
  "owner": "oidc:alice@example.com",
  "owner_name": "alice@example.com",
  "access": "private",
  "record": false
}
```

- `name` のみが必須。
- レスポンスヘッダ・ボディは `202 Accepted` + `Location: /v1/operations/<id>`（`kind: "branch.fork"`, `target: {"resource": "branch", "name": "feature-x-try"}`）。
- 転送時の注意: 受け付けたノードが親ノードと異なる場合、転送元ノードで `owner` / `owner_name` と `userBranchName`（セッション利用の非管理者に `<owner>-` プレフィックス付与）を確定してボディを再マーシャルしてから親ノードへ `s.forward` する。
- オペレーションの `phase` 遷移:
  - 無停止ライブフォーク: `snapshotting` → `cloning` → `starting` → `init_sql` → `stopping` → `snapshotting`（子の `@reset`）→ `starting`
  - `--clean` フォーク: `stopping`（親）→ `snapshotting` → `cloning` → `starting` → `init_sql` → `stopping` → `snapshotting` → `starting`
  - `--checkpoint` フォーク: `cloning` → `starting` → `init_sql` → `stopping` → `snapshotting` → `starting`
- `GET /v1/branches` / `GET /v1/branches/{name}` のブランチ表現に親参照フィールド（`parent_branch`, `parent_snapshot`、非フォークブランチでは省略）を追加する（既存の `base` / `snapshot` は祖先ベース情報として維持し、互換性を壊さない）:

```json
{
  "name": "feature-x-try",
  "status": "ready",
  "base": "prod-masked",
  "snapshot": "s20261109",
  "parent_branch": "feature-x",
  "parent_snapshot": "fork-feature-x-try",
  "runtime": "mysql@8.4.13",
  "base_runtime": "mysql@8.0.46"
}
```

### 3.3 MCP ツール

[mcp.go](../internal/interface/httpapi/mcp.go) に `fork_branch` ツールを追加する。

| ツール | 入力パラメータ | 動作 | 戻り値 |
|---|---|---|---|
| `fork_branch` | `source`（必須）, `name`（必須）, `checkpoint`（任意）, `clean`（任意）, `init_sql`（任意）, `ttl`（任意）, `access`（任意）, `record`（任意） | `POST /v1/branches/{source}/fork` を呼び出し、完了まで待機する | 作成された子ブランチ情報と接続先（`dsn`） |

`branches` スコープの API トークンで利用可能とする。

### 3.4 Web コンソール

- **ブランチ詳細画面**（管理画面 `/admin/branches/<name>` およびポータル `/branch/<name>`）:
  - 操作ボタン群に **Fork** ボタンを追加する（`status` が `ready` または `paused` かつ `checkpoints === true` かつ `branch.fork` 権限ありのとき表示・有効）。
  - クリックするとダイアログを開き、新しいブランチ名（ポータル非管理者は `<owner>-` プレフィックスをプレビュー表示）、起点（Current state / 指定 Checkpoint）、TTL、Access を入力して実行する。
  - 詳細メタデータ欄に、フォークされたブランチの場合は `Forked from: <parent_branch>`（クリックで親ブランチ詳細へ遷移）を表示する。
  - `lost` 状態のフォーク済みブランチでは `Recreate` ボタンを表示しない。
- **チェックポイントグラフ**（`checkpoints.tsx`）:
  - チェックポイント選択詳細欄から「Fork」アクションでそのチェックポイントを初期選択した Fork ダイアログを開けるようにする。
- **Branches 一覧**（`/admin/branches`）:
  - 一覧テーブルの `Base` 列に、フォークされたブランチでは親ブランチの補助表示を行い、検索対象（`filterBranches`）に `parent_branch` を含める。

## 4. 動作仕様

### 4.1 親ブランチの状態と実行可能条件・並行制御

| 親ブランチの `status` | フォーク可否 | 挙動 |
|---|---|---|
| `ready` | **可** | 既定はライブデータセットを無停止（必要なら `withLiveFreeze`）でスナップショット取得。`--clean` 指定時（かつ `--checkpoint` 省略時）は親を一時的に `forking` へ遷移させ正常停止して取得後すぐ再開 |
| `paused` | **可** | すでに正常停止済みのため、`--clean` の有無にかかわらず停止のままスナップショットを取得し、親は `paused` のまま維持する |
| `provisioning`, `resetting`, `restoring`, `upgrading`, `pausing`, `resuming`, `forking`, `deleting`, `failed`, `lost` | **不可**（`409 conflict`） | 親ブランチのデータセット・サーバー状態が不安定、喪失、または他操作中のため拒否 |

#### 親ブランチとの並行操作レースコンディション防止

1. **無停止フォーク / `--checkpoint` フォーク / `paused` 状態からのフォーク**:
   - `paused` 状態の親はすでに正常停止済みのため、`--clean` フラグの有無にかかわらず本経路で処理する（`BranchForking` へは遷移させない）。`--checkpoint` 指定時も既存スナップショットを参照するため `BranchForking` へ遷移させない。
   - `s.mu` のロック下で以下を連続実行する:
     1. `requireLineage()`、`clusterAllowsBranchFork()`、`parent.CanFork()`、子ブランチ名重複、リソース上限（`checkTTL`, `checkBranchCount`, `checkPoolRoom`）、`Placement`（親ノード単体のメモリ予算・`IDProblem`）、ランタイム導入済み、ポート割り当てを確認。
     2. **`Store.InsertBranch(child)` を先に実行**（`child.Status = BranchProvisioning`, `child.ParentBranch = parent.Name`, `child.ParentSnapshot = snapName`）。これにより、他リクエストや別ノードの `DeleteBranch(parent)` は即座に `child` の存在を検知して `409 in_use` で拒否でき、TOCTOU レースを防ぐ。
     3. `--checkpoint` 未指定なら `Storage.Snapshot(parent_live@fork-<child>)`（親が `ready` なら `withLiveFreeze` で囲む）を取得し、続けて `Storage.Clone(snap, child_ds)` を実行する。
     4. **失敗時の予約取り消し**: `Snapshot` または `Clone` に失敗した場合、新規作成した `fork-<child>` スナップショットがあれば `DestroySnapshot` で消し、`Store.DeleteBranch(child.Name)` で予約を取り消す（取り消しに失敗した場合は `s.fail(ctx, child, err)` で `BranchFailed` に遷移させ、手動削除可能にする）。
   - `s.mu` を解放した後、`applyEngineProps` → `applyQuota` → `PrepareClone` → `Start` → `CreateUser` → `dropInheritedUsers`（親ブランチ名およびレガシー `"branch"` の DB ログインを除去）→ `InitSQL`（`ExecAs`）→（`child.Record` 時 `ResetCloneHistory`）→ `StopClean` → `@reset`（`PropRuntime` 記録）→ `Start` を実行し、`saveBranch` で `child.Status = BranchReady` にする（`saveBranch` 失敗時は `stopBranchImmediate`）。失敗時は `s.fail(ctx, child, err)` で `BranchFailed` にする。
2. **`--clean` 指定フォーク（`--checkpoint` 未指定かつ親が `ready` のときのみ）**:
   - `s.mu` 下で検証・ポート確保後、`parent.Status = BranchForking` に遷移させ、同時に `Store.InsertBranch(child)` を行う（`InsertBranch` 失敗時は親を `BranchReady` に戻す）。
   - `BranchForking` 中、親に対する `DeleteBranch` / `ResetBranch` / `RestoreCheckpoint` / `PauseBranch` / `UpgradeBranch` / 別の `ForkBranch` は `409 conflict` で拒否され、ProxySQL ルーティング（`routable`）からも一時的に外れる。
   - `s.mu` を解放し、`StopClean` → `Storage.Snapshot` → `Storage.Clone` → 親の再起動（`ensureRunning`）と `BranchReady` 復帰を完了してから、子の起動・初期化を続行する。
   - `StopClean` / `Snapshot` / `Clone` 失敗時は、新規スナップショット（あれば）と子の予約を取り消した上で親を `ensureRunning` して `BranchReady` に戻す。
   - 親が `BranchForking` のままエージェントが再起動した場合は、`Recover` で `startServerIfDown` → `releaseFreeze` → `putLoginRight` を行い `BranchReady` に戻す（`ErrRecordRequired` 時は `markFailed`）。

### 4.2 整合性の取り方（ZFS スナップショットと DB エンジン）

- **既定は無停止（クラッシュコンシステント）、`--clean` で正常停止を選択可能**: MySQL（InnoDB redo ログ）および PostgreSQL（`pg_wal`）はデータディレクトリと同一の ZFS データセット内にログを保持するため、ZFS の原子的スナップショットから安全にクラッシュリカバリできる。`LiveFreeze` を宣言する外部エンジンでは `withLiveFreeze` で静止点を取る。
- **エンジン別のクローン後処理**:
  - **MySQL**: `PrepareClone` で `*.pid`・`auto.cnf`・古い `error.log` を削除。`CreateUser` は `CREATE USER IF NOT EXISTS` + `ALTER USER`（8.4 非 LegacyAuth / 9.x は `caching_sha2_password`）+ `clientPrivileges` + `partial_revokes`（8.0.16+ で `mysql.*` 剥奪）を適用。続けて `DropUser` で親ブランチ名およびレガシー `'branch'@'%'` のログインを除去し、親の認証情報による `private` / 所有者境界の迂回を防ぐ。`child.Record == true` の場合は `@reset` 取得前に `ResetCloneHistory`（8.4+ は `RESET BINARY LOGS AND GTIDS`、それ未満は `RESET MASTER`）で親由来の binlog / GTID をクリアする。
  - **PostgreSQL**: `PrepareClone` で `postmaster.pid`・`postmaster.opts` を削除。`CreateUser` は `NOSUPERUSER` + `OwnerRole`（`db_branch_owner`）所属 + `ownObjects` を適用。続けて `DropUser` で親ブランチ名およびレガシー `branch` ロールの残存所有物を `db_branch_owner` へ移譲・権限剥奪した上で `DROP ROLE` する。
- **`--init-sql` の実行権限**: `srv.ExecAs` で子ブランチ自身の非スーパーユーザーログイン権限として実行する（MySQL は `--binary-mode`、PostgreSQL は `pgx` 経由）。
- **リセットポイント作成**: `StopClean` 後に `<pool>/db-branch/branches/<child>@reset` を作成し、`PropRuntime` にフォーク時のランタイムを記録してから再起動する。

### 4.3 命名規則

- **子ブランチ名**: 既存の `domain.ParseName` をそのまま適用（英小文字・数字・単独ハイフン、先頭英字、最大 32 文字）。クラスタ全体で一意。
- **ユーザー画面のプレフィックス**: 管理者以外のログインセッションから作成する場合は `createBranch` と同様に `<owner>-` プレフィックスを自動付与する。
- **フォーク用 ZFS スナップショット名**: `fork-<child>`（最大 37 文字）。`ckpt-`、`gone-`、`reset` と衝突せず、`ListCheckpoints` や `branch.max_checkpoints` に混入しない。

### 4.4 親子関係とライフサイクル（ZFS 依存関係・各操作との相互作用）

#### A. 親ブランチの削除（`branch delete` / TTL 失効 / `lost` 時の `abandonLost`）

- **v1 では `409 in_use` による削除保護を採用する**。
- `DeleteBranch(parent)` において、`abandonLost` 内および `transitionBranch` 内の `s.mu` ロック下（さらに HTTP API の `deleteBranch` 入口でも同期検査）で `b.ParentBranch == parent` となる子ブランチが存在しないかを検査する。存在する場合は **`srv.Remove` 等の破壊的処理を一切行わず `409 in_use`**（子ブランチ名をソートしてメッセージに列挙）を返す。
- `Service.Expire` は `ErrInUse` で拒否された親ブランチをスキップし、エラーに積まない。子がすべて削除された次の巡回で親も自動削除される（同じ巡回で子と親が両方失効している場合も子を先に消して親を片付ける）。

#### B. 子ブランチ削除時の後始末（`fork-<child>` スナップショットの回収）

- 子ブランチの `doDelete` で `destroyBranchData(child)` と `Store.DeleteBranch(child)` を完了した後、`s.mu` ロック下で以下を実行する:
  1. 親ブランチがリネージ遷移中（`upgrading` / `restoring` / `resetting` / `forking`）であれば競合を避けてここでは触らず、親の遷移完了時の `collectIfSettled`（未クローンかつストア不在の `fork-<child>` を回収）に委ねる。
  2. それ以外の状態では、`child.ParentBranch != ""` かつ `domain.IsForkSnapshot(child.ParentSnapshot)`（`fork-*`）の場合、親の `family` から `domain.FindSnapshot(parentLines, child.ParentSnapshot)` で該当スナップショットを特定し、`Storage.DestroySnapshot` で破棄する。
  3. `child.ParentSnapshot` が `ckpt-*` や `reset` の場合は親自身のスナップショットなので破棄しない。
  4. 最後に親の `s.collectLogged(ctx, child.ParentBranch)` を実行し、`gone-*` 化されていたチェックポイントや不要になった退避ラインを即時回収する（`dropEmptyRoot` は `PropOp` と `PropUpgrade` の両方を保護する）。
- エージェント再起動時の `Recover` では `BranchDeleting` の後始末を他ブランチの復旧完了後に実行し、親の `resumeUpgrade` / `resumeRestore` と競合させない。

#### C. 親ブランチの `reset` / `restore`

- 親のライブデータセットに `@fork-<child>` が存在すると `len(l.Snapshots) > 1` となり、`resetBySwap` が自動的にスワップ方式を選ぶ。旧ライブは `history/<parent>/<line>` へ退避され、`Garbage` は `len(s.Clones) > 0` を保持するため子は壊れない。子が削除されると §4.4.B の回収で退避ラインごと解放される。

#### D. 親ブランチのチェックポイント削除（`branch checkpoint delete`）

- 親のチェックポイントに子クローンがぶら下がっている間、`deleteCheckpointLocked` が `gone-<timestamp>` へリネームして隠し、子削除後の `collectLogged` で自動回収される。

#### E. 親ブランチのアップグレードと子の `reset`（`LegacyAuth` を含む）

- 子ブランチは `ForkRuntime` と `ForkLegacyAuth` を保持する。
- `runtimeOf` はスナップショットに `PropRuntime` がない場合、フォーク済みブランチなら `base.Runtime` ではなく `br.ForkRuntime` を返す。
- `doReset` と `switchRuntime` はリセット時に `br.LegacyAuth` を無条件に `false` にせず、`br.ForkLegacyAuth` に戻す。

#### F. 子ブランチの `recreate`（ノード喪失時）

- `Branch.CanRecreate()` は `b.ParentBranch != ""` の場合に `ErrConflict`（`forked branch cannot be recreated on another node`）を返す。コンソールでも `lost` 時の `Recreate` ボタンを表示しない。

#### G. 多段フォーク（孫ブランチ）

- 子ブランチ `child1` が `ready` または `paused` になれば、`child1` を親として `child2` をフォーク可能。削除はリーフ側（`child2` → `child1` → `parent`）の順になる。

### 4.5 クラスタ（Raft）での扱い

- **配置ノードの決定**: 親ブランチが存在するノード（`parent.Node`）にのみ配置可能。別ノード指定は `409 conflict`。別ノードへの API リクエストは親ノードへ転送する。
- **Raft プロトコルバージョン**:
  - `domain.ProtocolBranchFork = 25`、`raftstore.Protocol = 25`。
  - `raftstore.Command.Needs()` は `insert_branch` / `put_branch` が `ParentBranch != ""` または `Status == BranchForking` を含む場合に `ProtocolBranchFork` を要求する。
  - ユースケースの `clusterAllowsBranchFork` でもクラスタ全体が Protocol 25 に達するまで `503 cluster_upgrading` で拒否する。

### 4.6 ポート・ランタイム・設定・ProxySQL の引き継ぎ

| 項目 | 引き継ぎ元 / 決定方法 | 備考 |
|---|---|---|
| ランタイム | 親の現在ランタイム（`--checkpoint` 指定時はその `PropRuntime`、未記録なら `runtimeOf`） | 未インストールなら `404 runtime_not_installed` |
| ポート | `allocatePort` で新規割り当て | 親と別ポート |
| DB ユーザー・パスワード | `child` 名で新規作成（非スーパーユーザー、`ALTER USER` でパスワード同期） | `private` はランダム、`open` は `OpenPassword` |
| ProxySQL ルーティング | 子用に新規 hostgroup・ユーザー・コメントルールを登録 | `BranchForking` 中は親を一時的に `routable` から除外 |
| メモリ・クォータ・空き容量 | 現行設定の `MemoryMB`, `refquota`, `max_per_user`, `max_ttl`, プール残量 10%/1GiB ガード | 親作成時の値ではなく現行設定を適用 |
| `LegacyAuth`（MySQL 8.4） | 親から引き継ぎ、`ForkLegacyAuth` に保存 | `--checkpoint` が別ランタイムなら `false`、`@reset` は親の `ForkLegacyAuth` |
| `Record`（history モジュール） | `--record` 明示または `RecordNew`（`record_all`）に従う | `child.Record == true` 時は `ResetCloneHistory` で親の binlog / GTID をリセット |
| `Base` / `Snapshot` | 親の `Base` / `Snapshot` を引き継ぐ | 一覧・祖先参照の互換性維持 |

## 5. エラーケース・境界条件・権限・リソース上限

### 権限と認可（`ownership.go`）

- 認可アクション名: `branch.fork`（対象リソースは親ブランチ `Resource{Kind: "branch", Name: <source>}`）。
- 管理者（`admin` / `scope:full`）: 任意のブランチをフォーク可能。
- 一般ユーザー（`scope:branches` / ポータルユーザー）: 自分が所有するブランチ、または `SharesBranches` で共有されたブランチのみフォーク可能。他人の `open` ブランチ・所有者なしのレガシーブランチ・匿名ユーザー（`Anonymous`）は `403 forbidden`。
- `checkBranchPolicy` により有償モジュールのチーム制限等も子ブランチに対して適用する。

### エラーケース一覧

| 条件 | HTTP | エラー `code` | 備考 |
|---|---|---|---|
| 親ブランチが存在しない | 404 | `not_found` | 親ブランチ未存在 |
| 親ブランチが `ready` / `paused` 以外 | 409 | `conflict` | 親の現在状態をメッセージに含める |
| 指定 `--checkpoint` が親に存在しない | 404 | `not_found` | チェックポイント未存在 |
| 子ブランチ名・チェックポイント名・TTL・Access が不正 | 400 | `invalid_request` | `domain.ErrInvalidName` / `ErrInvalidTTL` 等 |
| 子ブランチ名がすでに存在する | 409 | `already_exists` | クラスタ全体で一意 |
| 権限がない / 匿名ユーザー / 他人の `open` ブランチ | 403 | `forbidden` | `action: "branch.fork"` |
| ユーザー別ブランチ上限超過（`branch.max_per_user`） | 429 | `quota_exceeded` | `domain.ErrQuotaExceeded` |
| プール空き容量不足（残量 10% または 1GiB 未満） | 507 | `no_space` | `domain.ErrNoSpace` |
| 親のノードでメモリ不足 | 503 | `no_capacity` | `domain.ErrNoCapacity` |
| 別 `--node` を指定（親と不一致） | 409 | `conflict` | 同一ノードのみ許可 |
| 親のランタイム（またはチェックポイントのランタイム）が未インストール | 404 | `runtime_not_installed` | インストールコマンドを案内 |
| エンジンが `checkpoints` 非対応 | 409 | `feature_not_supported` | `domain.ErrFeatureNotSupported` |
| ストレージがローカル ZFS 非対応（`Lineage == nil`） | 409 | `checkpoints_unavailable` | `domain.ErrCheckpointsUnavailable` |
| 子ブランチが存在する親ブランチの削除 | 409 | `in_use` | `domain.ErrInUse`（子ブランチ名を列挙） |
| クラスタに Protocol 25 未満のノードがある | 503 | `cluster_upgrading` | 全ノード更新後に利用可能 |
| `--clean` 指定時に親の正常停止に失敗 | 409 | `unclean_shutdown` | スナップショットを取らず親を再起動して失敗 |

## 6. 未決事項（将来の拡張・人間判断の余地）

v1 では安全で一貫した既定動作を採用しており、以下は必要に応じて人間の判断で拡張・変更できる。

1. **親ブランチ削除時の `zfs promote` 許可**: v1 は `409 in_use`（および `Expire` スキップ）で親を保護する。将来的に親を先に削除する需要が高い場合、`zfs promote` + 親スナップショット整理の設計を検討する。
2. **子の TTL と親の TTL の関係**: v1 は子の TTL を通常どおり独立して決定する（`branch.max_ttl` 等の既存上限のみ適用）。親の残り TTL 以下へ強制制限するかは運用方針次第。
3. **親の `Record` の自動引き継ぎ**: v1 は `branch create` と同様に `--record` 明示または `record_all` 設定に従う。親が `Record: true` のとき既定で引き継ぐ案もある。
4. **`failed` 状態の親からの `--checkpoint` フォーク**: v1 はシンプルさのため `ready` / `paused` のみ許可する。`upgrade --no-rollback` で `failed` になった親のチェックポイントからのフォークを許可するかは将来検討。
