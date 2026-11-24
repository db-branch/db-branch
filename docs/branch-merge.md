# Branch 内の Line マージ（履歴ライン間の変更統合）

1 つのブランチ内部において、`branch restore` によって分岐した過去の履歴ライン（`line`）で行われたデータベースの変更を、現在稼働中のライブライン（`live`）へ統合（マージ）する。

`branch restore` で過去のチェックポイントへ戻して作業を続けた後、「退避された元のラインに残っていた変更」を現在のライブ状態へ取り込みたい用途を想定している。データのコンフリクトやスキーマの不整合を検出した場合は、ライブラインを一切変更せずにエラーを表示して失敗する（自動解決や対話的な手動解決 UI は扱わない）。

## GA (`origin/main`) での再照合

本仕様書は `v0.2.7` 時点の再照合版をもとに、GA 修正および F-01（[branch-fork.md](branch-fork.md)）マージ後の `origin/main`（`07a6a33`）に対して全根拠を再照合し、仕様・前提を更新した。主な更新点は以下のとおり。

| 項目 | 旧仕様（`v0.2.7` 時点） | 現行仕様（GA / 1.0） | 理由 |
|---|---|---|---|
| **Raft プロトコル番号** | `ProtocolBranchMerge = 20` | `ProtocolBranchMerge = 26`（`raftstore.Protocol = 26`） | GA 修正（W-17, W-03, R-01）と F-01（`ProtocolBranchFork = 25`）により `origin/main` の [raftstore/fsm.go](../internal/infrastructure/raftstore/fsm.go#L161) が `Protocol = 25` まで進んでいるため。`Command.Needs()` でも `insert_branch` / `put_branch` が `Status == BranchMerging` を運ぶ場合に `ProtocolBranchMerge` を要求する |
| **所有権・認可・結果閲覧（W-03 / W-04 / SEC-05）** | 所有者なしレガシーブランチも一般ユーザー可と記載 | `Branch.Owner` に不変 ID（`p.ID`）、`Branch.OwnerName` に表示名を分離（[branch.go](../internal/domain/branch.go#L243-L246)）。`canManageBranch`（[ownership.go](../internal/interface/httpapi/ownership.go#L90-L103)）に従い、所有者・`SharesBranches`・管理者のみ `branch.merge` を許可（所有者なしレガシーブランチは管理者限定、他人の `open` ブランチおよび `Anonymous` は `403 forbidden`）。`Operation.Result`（`MergeResult`）は `StartResultAs` により `canSeeOperation`（[ownership.go](../internal/interface/httpapi/ownership.go#L121-L133)）で所有者スコープに保護される | GA 修正（W-03, W-04, SEC-05）の所有権・認可モデルに完全準拠するため |
| **PostgreSQL メンテナンス起動と非スーパーユーザー所有権** | `postgres/server.go` に `Maintenance` 分岐を新規追加と記載 | [postgres/server.go](../internal/infrastructure/postgres/server.go#L118-L123) の `listen(s)` で `s.Maintenance == true` 時の `PG_BIND=""` は実装済み。適用時は `p.Admin(s)`（`Superuser = "postgres"`）で DDL/DML を実行し、`StopClean` 前に `putLoginRight`（`CreateUser` / `ownObjects`、[execas.go](../internal/infrastructure/postgres/execas.go#L103-L115)）で新設オブジェクトの所有権を `db_branch_owner` へ移譲する | GA 修正後の `postgres/server.go`・`execas.go` の実装と整合させるため |
| **CLI 終了コード制御** | `ExitCodeError` を新規導入と記載 | [cli.go](../internal/interface/cli/cli.go#L21-L28) に導入済みの `ExitError{Code: 2, Err: ...}` を利用する | GA 修正で `ExitError` が実装済みのため |
| **リソース上限・容量ガード（W-15）と混在クラスタ転送** | 容量・メモリ検査の抽象記載 | `checkPoolRoom`（[limits.go](../internal/usecase/dbbranch/limits.go#L39-L56)、プール残量 10% または 1GiB 未満で `507 no_space`）および一時クローン用メモリ予算検査（`503 no_capacity`）を適用。HTTP API 入口ではリモートノード転送前にクラスタプロトコル（`ProtocolBranchMerge = 26`）を検査し、旧ノードへの転送による `404` 化を防いで `503 cluster_upgrading` を返す | W-15 および F-01 のクラスタ転送ガードと統一するため |
| **ブランチ間マージ非目標との区別（`README.md` / `docs/design.md`）** | 文書間の関係未整理 | `README.md` と `docs/design.md` の「ブランチ間のマージ・リベースは行わない」記述を維持しつつ、本機能は「1 つのブランチ内部で `branch restore` により分岐した履歴ライン（`line`）間の論理 3-way マージ」であることを両文書に明記 | 利用者がブランチ間マージと混同しないようにするため |

## 1. 既存モデルとの整合と前提

### 現行コードから確認した前提

1. **1 ブランチ内の履歴ライン（`line`）構造**
   - ブランチ `x` は、現在稼働中のライブデータセット `<pool>/db-branch/branches/x` と、過去の `branch restore` によって退避されたライン `<pool>/db-branch/history/x/<n>` を持つ（[checkpoints.md §3](checkpoints.md#L29-L43), [checkpoint.go](../internal/usecase/dbbranch/checkpoint.go#L12-L30)）。
   - 各ラインとチェックポイントは ZFS ユーザープロパティとして以下を保持する（[checkpoint.go](../internal/domain/checkpoint.go#L48-L78)）:
     - `db-branch:line`（ライン番号。初期ラインは `0`、復元のたびに `NextLine` で単調増加）
     - `db-branch:parent`（分岐元の親ライン番号）
     - `db-branch:from`（分岐元のチェックポイント名 `ckpt-<name>` または `reset`）
     - `db-branch:runtime`（チェックポイント取得時のランタイム）
   - `domain.LineViews` により、ブランチ内の全ラインは `Parent` と `From` で結ばれた木構造（履歴グラフ）を形成する（[checkpoint.go](../internal/domain/checkpoint.go#L455-L499)）。
2. **退避ラインのライフサイクルとデータセットの所在**
   - `restore` 実行時、それまでライブだったデータセットは `history/x/<n>` へリネーム・`canmount=off` で退避される（[restore.go](../internal/usecase/dbbranch/restore.go#L14-L30)）。
   - 後続の `restore` での `zfs promote` により、あるラインのデータセット自体は消滅し、そのライン上で取られたチェックポイントスナップショットのみが別データセット上に残る場合がある（[checkpoint.go](../internal/domain/checkpoint.go#L503-L517) の `NewestCheckpointOfLine`）。
   - チェックポイントも `reset` もクローンも持たないラインは `domain.Garbage` により自動削除される（[checkpoint.go](../internal/domain/checkpoint.go#L300-L329)）。
3. **局所性と `Lineage` 依存**
   - 1 つのブランチのライブデータセットと `history/x/*` はすべて**同一ノード・同一 ZFS プール**に存在する。
   - 履歴ラインの操作はノード自身の ZFS（`s.Lineage != nil`）を前提とし、共有ストレージ等では `ErrCheckpointsUnavailable`（`409 checkpoints_unavailable`）となる（[checkpoint.go](../internal/usecase/dbbranch/checkpoint.go#L44-L49)）。
4. **`branch fork` 仕様（[branch-fork.md](branch-fork.md)）との整合**
   - `branch fork` は「別ブランチの作成」であり、本仕様書は「同一ブランチ内のライン間マージ」であるため機能として直交する。`branch fork` で作成された子ブランチの内部でも、自身の `history/<child>/` 内のライン間マージは同様に動作する。
   - 子ブランチ削除時の `cleanupParentFork` / `collectIfSettled`（[fork.go](../internal/usecase/dbbranch/fork.go#L422-L478)）が親ブランチのマージ中と競合しないよう、`isLineageTransition` に `BranchMerging` を含め、`dropEmptyRoot`（[checkpoint.go](../internal/usecase/dbbranch/checkpoint.go#L382-L400)）でも `PropMerge` を保護する。

### マージの方向と対象（前提の明記）

「ブランチの中でのマージ」を **同一ブランチ内の `source_line`（マージ元ライン）→ 現在の `live` ライン（マージ先）** への一方向統合と定義する。

| 論点 | 採用方針 | 理由 |
|---|---|---|
| **マージ先（Target）** | 常に**現在のライブライン**（`branches/<name>`） | DB サーバーが稼働し接続先となるのはライブラインのみであるため。過去の退避ラインを直接書き換える必要がない |
| **マージ元（Source）** | 同一ブランチ内の**別のライン番号**（`source_line != live_line`）、または任意指定のチェックポイント（`--checkpoint`） | `restore` で退避した元のライン（例: `line 0`）の変更を現在の `line 1` へ統合する基本ユースケースに合致する |
| **Source の時点** | `history/<name>/<source_line>` データセットが現存する場合（別のチェックポイントが残り `domain.Garbage` に破棄されなかった場合）はその**先端（退避直前の状態）**。`promote` や `Garbage` でデータセットが消えている場合は `NewestCheckpointOfLine` で見つかる**最新チェックポイント**（ただしそれが共通祖先と同一なら `invalid_request` エラー） | 復元先に選んだチェックポイントより新しいチェックポイントが元の線にない場合、旧線は `restore` 完了時に `domain.Garbage` によって片付き、最後のチェックポイントより後の変更は失われる。マージできるのは `restore` 後に残っている旧線の先端か、そのチェックポイントまでとなるため、残したい場合は `restore` の前にチェックポイントを取る必要がある |
| **共通祖先（Ancestor）** | 履歴木（`PropParent` / `PropFrom`）を遡って求める **LCA（最小共通祖先）スナップショット** | 3-way マージにより「Source で削除された行」と「Target で新規挿入された行」を正確に区別できる |

#### 共通祖先（Ancestor）の特定ルール

`live` ライン（番号 $L$）と `source_line`（番号 $S$）の履歴木を `PropParent` / `PropFrom` に沿ってルート方向へ辿り、以下のルールで共通祖先スナップショット $A$ を決定する。

1. $L$ が $S$ から分岐した子孫（例: $L=1$ が $S=0$ の `ckpt-c1` から `restore` で分岐）の場合: 共通祖先 $A$ は分岐点の `ckpt-c1`（または `reset`）。
   - **削除済みチェックポイント（`gone-*`）への追従**: ユーザーが `ckpt-c1` を削除した場合でも、クローンが存在する間は ZFS 上に `gone-<ts>` として残っている（[checkpoints.md §6](checkpoints.md#L78)）。`PropFrom` で名前が見つからない場合は、`promoteOwn`（[restore.go](../internal/usecase/dbbranch/restore.go#L591-L608)）による親子逆転後でも特定できるよう、`family` 全体の `SnapshotInfo.Clones` の逆引きと `PropLine`（元のチェックポイントが属していたライン番号）を用いて該当する `gone-*` スナップショットを探索する。
2. 共通の親ライン $P$ から $S$ と $L$ の両方が分岐した兄弟・従兄弟ライン（例: $S=1$ が `line 0` の `ckpt-c1` から、$L=2$ が `line 0` の `ckpt-c2` から分岐）の場合:
   - `ckpt-c1` と `ckpt-c2` のどちらが先かを ZFS の `createtxg`（トランザクショングループ番号。`CreatedAt` は 1 秒解像度のため、`SnapshotInfo` に `CreateTXG uint64` を追加して比較）で判定し、**古い方のスナップショット**を共通祖先 $A$ とする。
3. **共通祖先を特定できない場合（`no_common_ancestor`）**:
   - `PropParent` / `PropFrom` が記録されていないライン（[checkpoints.md §10](checkpoints.md#L106)）。
   - 共通祖先となるスナップショットが完全に削除・GC 済みで ZFS 上に存在しない場合。
   - `source_line == live_line`、または解決した Source スナップショットが共通祖先スナップショットと同一の場合は `invalid_request`。

## 2. 目的・ユースケース / 非目標

### 目的とユースケース

1. **別アプローチの統合**: `line 0` で機能 A のマイグレーションとデータ投入を行い、途中で `cp1` に `restore` して `line 1` で機能 B を進めた後、`line 0` の変更も `line 1` に統合する。
2. **退避ラインからの救出**: チェックポイントを取ってから `restore` で過去のチェックポイントに戻した後、退避された元のラインに残っていたデータ変更を、現在のライブ状態とコンフリクトしない範囲で取り込む。
3. **AI エージェントの試行結果統合（MCP）**: LLM エージェントが `restore_checkpoint` で別ラインを試した結果を、`merge_line`（`dry_run: true` で事前検証）によりライブラインへ統合する。

### 非目標（v1 スコープ外）

- **別ブランチ間のマージ**: 異なるブランチ（`branch-a` → `branch-b`）やベースへのマージは対象外（本仕様は 1 ブランチ内の `line` 単位に限定する）。
- **コンフリクトの自動解決・対話的解決 UI**: `--ours` / `--theirs` や行単位の選択 UI は提供しない。
- **マージの継続・中断状態（Git の `MERGING` 状態）**: コンフリクト時は何も変更せずエラーで終了する。
- **Source ラインの自動削除**: マージ成功後も、マージ元となった退避ラインやチェックポイントは自動的に消さない。
- **共有ストレージ（`s.Lineage == nil`）での利用**: `409 checkpoints_unavailable` を返す。

## 3. マージ方式

### ZFS ブロック単位でのマージが不可能な理由

ZFS スナップショット間の差分（`zfs send -i` / `zfs diff`）はファイル・ブロック単位である。MySQL（InnoDB の `.ibd` テーブルスペース、redo ログ、データディクショナリ）および PostgreSQL（ヒープファイル、B-tree ページ、WAL、システムカタログ、`xmin`/`xmax`）では、2 つのラインで異なる行を変更しても同一の 16 KiB / 8 KiB ページや B-tree 内部ノード、LSN が書き換わるため、ブロック単位の統合は確実にデータ破損を引き起こす。

したがって、**データベースの論理レベル（テーブル・スキーマ・行単位）の 3-way マージ**を行う。

### 論理レベルのマージ方式の比較と採用方針

| 案 | 方式 | メリット | デメリット / リスク |
|---|---|---|---|
| **案 1（採用）** | **一時クローン起動による 3-way 論理比較・SQL 適用**（Ancestor / Source / Target の ZFS スナップショットをメンテナンスモードで一時クローン起動し、Go エンジンが 3-way 比較してライブラインへ SQL 適用） | ZFS クローンによりディスクコピー 0 バイトで任意の過去ライン・スナップショットを SQL 読み取り可能にできる。`Record: false`（既定）のブランチでも動作する。テーブル・主キー・行単位で正確なコンフリクト情報を生成できる。ライン間でランタイムが異なっていても SQL 経由で比較可能 | 一時サーバー起動に数秒（PostgreSQL ~0.5s, MySQL ~3s）かかる。巨大テーブルの全行走査に時間を要する |
| 案 2 | **クエリ履歴のタイムスタンプ順合体・再実行方式** | 変更クエリだけを再実行するため全行スキャン不要。`UPDATE ... SET x = x + 1` のような相対更新を文レベルで保持できる | クエリ記録（`Record: true` / 有償 `history` モジュール）が必須となり既定ブランチで使えない。文だけでは衝突した主キー行を特定できず、`NOW()` / `UUID()` 等の非決定的関数や並行ラインの時刻交差で破綻しうる |
| 案 3 | バイナリログ / WAL（行ベース変更ログ）の 3-way 合成 | 変更行だけを抽出できるため高速かつ行特定も可能 | `Record: true`（かつ RBR 有効化）が必須となり、OSS コアの既定ブランチで動作しない |
| 案 4 | `mysqldump` / `pg_dump` のテキスト diff | 実装が単純に見える | 巨大 DB で大量のディスク・時間を消費し、型・NULL・バイナリの厳密比較や行単位のコンフリクト特定が困難 |

事前設定（`--record` や有償 `history` モジュール）なしで**すべてのブランチで動作し、かつ「どのテーブル・行・キーでコンフリクトしたか」を確実に報告できる案 1（3-way 論理比較）**を採用する。

### 一時クローンとメンテナンス起動の実装要件

- Ancestor スナップショット、Source ライン（データセット現存時は一時スナップショット `merge-src-<short_id>` を切ってから、消滅時または `--checkpoint` 指定時はそのスナップショットから）、および Target（ライブ）ライン（`merge-live-<short_id>` スナップショットから）を `<pool>/db-branch/work/merge-<short_id>/{a,s,t}`（[mask.go](../internal/usecase/dbbranch/mask.go#L25) の `workDataset` を再利用）へ ZFS クローンする。
  - Target も一時クローン `t` から読み取ることで、`--dry-run` 時も稼働中のライブ DB サーバーへ直接接続・ロック競合させることなく安全に 3-way 比較を実行できる（本実行時と `--dry-run` 時の比較ロジックも共通化できる）。
  - **親データセットの事前作成**: `<pool>/db-branch/work` および `<pool>/db-branch/work/merge-<short_id>` を `canmount=off` で事前作成する。
- 一時インスタンスは稼働中ブランチの `Server.ID`（`branch-<name>`）や設定ディレクトリと衝突しないよう、短い専用 ID（`m-<short_id>-a`, `m-<short_id>-s`, `m-<short_id>-t`。UNIX ソケットパス長 107 bytes 上限内に収める）で `DBServer.PrepareClone` + `Start(Maintenance: true)` を実行する。本実行時の Target のメンテナンス起動は `s.branchSpec(ctx, br, s.branchDataset(br.Name), true, false)`（[service.go](../internal/usecase/dbbranch/service.go#L196-L224)）を用い、`br.Record` / `br.LegacyAuth` / `ServerOptions` を維持する。
- **UNIX ソケット経由のローカル管理者接続**:
  - MySQL: `Maintenance: true` で `skip-networking` が付与され、`AdminAccess.Socket`（`/run/db-branch-mysqld/<id>/mysqld.sock`）に `go-sql-driver/mysql`（`Net: "unix"`）で `root` 接続する。
  - PostgreSQL: `Maintenance: true` で `PG_BIND=""`（`listen_addresses=""`）が出力され、`AdminAccess.Socket`（`/run/db-branch-postgres/<id>`）に `jackc/pgx/v5/stdlib` で `postgres`（`Superuser`）接続する。
- 各一時インスタンスは**そのスナップショット/ラインに記録されたランタイム（`PropRuntime`、未記録時は `runtimeOf`）**で起動する（未インストールなら `runtime_not_installed`）。
- 処理完了後（成功・失敗・キャンセルを問わず `defer` で）、一時サーバーを `StopImmediate` + `Remove` し、一時 ZFS クローンと一時スナップショットを `Destroy` する。

### 3-way 比較の基本アルゴリズム

テーブルごとに、Ancestor $A$、Source $S$、Target（現在のライブライン）$T$ の行を主キー（PK）昇順のストリーミングカーソルで同時に走査（マージソート走査）し、メモリ消費を $O(1)$ に抑える。

| $A$ (祖先) | $S$ (Source) | $T$ (Target/Live) | 判定と Target へのアクション |
|---|---|---|---|
| なし | なし | あり | Target のみで挿入 → **維持（何もしない）** |
| なし | あり | なし | Source のみで挿入 → **`INSERT`** |
| なし | あり | あり | 値が完全一致なら維持、異なれば **重複挿入コンフリクト（`insert_insert`）** |
| あり | $= A$ | $= A$ | 変更なし → **何もしない** |
| あり | $= A$ | $\neq A$ / 削除 | Target のみ変更/削除 → **維持（何もしない）** |
| あり | $\neq A$ | $= A$ | Source のみ更新 → **`UPDATE`** |
| あり | 削除 | $= A$ | Source のみ削除 → **`DELETE`** |
| あり | $\neq A$ | $\neq A$ | 同一行の両側更新 → 変更後の値が完全一致なら維持、異なれば **更新コンフリクト（`update_update`）** |
| あり | 削除 | $\neq A$ | Source 削除・Target 更新 → **削除/更新コンフリクト（`delete_update`）** |
| あり | $\neq A$ | 削除 | Source 更新・Target 削除 → **更新/削除コンフリクト（`update_delete`）** |
| あり | 削除 | 削除 | 両側で同じ行を削除 → **維持（何もしない）** |

- **NULL・型・照合順序の厳密比較**:
  - `NULL` 同士は一致（SQL の `IS NOT DISTINCT FROM` / `<=>` 相当）とし、`NULL` と空文字 `""`・ゼロ値 `0` は厳密に区別する。
  - `DECIMAL` や高精度数値・`JSON` は正規化された文字列表現で比較し、`float64` への丸めによる精度落ちや `NaN != NaN`（PostgreSQL では `NaN = NaN` が成立し PK になりうる）による偽コンフリクトを防ぐ。
  - 文字列 PK で大文字小文字を区別しない照合順序（MySQL `utf8mb4_0900_ai_ci` や `PAD SPACE`）が使われている場合でも DB の `ORDER BY` 順と Go 側の比較が食い違わないよう、文字列 PK 列はバイナリ/コードポイント順（MySQL: `CAST(col AS BINARY)`、PostgreSQL: `col COLLATE "C"`）でソートして走査する。

### DB エンジン別の課題と扱い（MySQL / PostgreSQL）

#### 1. スキーマ変更（DDL）・ビュー・生成列・パーティション
- **MySQL と PostgreSQL の違い**: PostgreSQL は DDL もトランザクション内でロールバック可能だが、MySQL は DDL（`CREATE`/`ALTER`/`DROP TABLE`）が**暗黙のコミット（Implicit Commit）**を引き起こす。そのため **§5 の自動チェックポイントと失敗時自動復元が原子性の要** となる。
- **対象オブジェクトの絞り込み**:
  - `information_schema.TABLES` は `TABLE_TYPE = 'BASE TABLE'` のみを対象とし、ビュー（`VIEW`）を除外する。
  - PostgreSQL の宣言的パーティションテーブル（`pg_class.relkind = 'p'`）は親テーブルのみを対象（子パーティション `relispartition` を重複走査しない）とする。
  - 生成列（`GENERATED ALWAYS AS ...`）は行比較および `INSERT`/`UPDATE` の列リストから除外する。
  - システム DB / スキーマ（MySQL: `mysql`, `information_schema`, `performance_schema`, `sys`、PostgreSQL: `pg_catalog`, `information_schema`, `pg_toast`, `pg_temp_*`）は除外する。
- **DDL 扱い方針**:
  - **新規テーブル（$A$ に無く $S$ のみに存在）**: PostgreSQL の `ENUM` 型（`CREATE TYPE ... AS ENUM`）等があれば先に作成し、構造化メタデータから構築・検証した `CREATE TABLE` を Target へ適用後に行を `INSERT` する。双方で同名テーブルが新規作成された場合は定義が完全一致しない限り **`schema_conflict`**。
  - **PostgreSQL での所有者移譲（必須）**: PostgreSQL のブランチログインは `NOSUPERUSER` かつ `OwnerRole`（`db_branch_owner`、[execas.go](../internal/infrastructure/postgres/execas.go#L17)）のメンバーである。Target への DDL 適用はエージェントの `Superuser`（`p.Admin(s)`）で行うため、新規作成したテーブル・型・シーケンスの所有権をブランチログインが操作できるよう、適用後（`Maintenance: true` の終了前）に `putLoginRight`（`CreateUser` / `ownObjects`、[execas.go](../internal/infrastructure/postgres/execas.go#L103-L115)）を実行して `db_branch_owner` へ移譲する。
  - **テーブル削除（$A$ にあり $S$ で削除）**: $T$ でそのテーブルにスキーマ変更・行変更がなければ `DROP TABLE`。変更があれば `schema_conflict`。
  - **既存テーブルの `ALTER TABLE`**: 双方で異なる変更があれば `schema_conflict`。$S$ のみの場合は、互換性のある列追加（`ADD COLUMN` で `NULL` 許容または `DEFAULT` あり）のみ `--allow-ddl` 指定時に許可する（既定は `schema_conflict`）。

#### 2. 主キー無しテーブル（PK-less tables）
- 主キー（`PRIMARY KEY`）または全列 `NOT NULL` の一意制約（代替キー）がないテーブルは行を一意に識別できない。
- **扱い**: $S$ と $T$ で $A$ からの差分がある主キー無しテーブルは、**エラー（`schema_no_primary_key`）としてマージを拒否**する（変更がないテーブルはスキップする）。

#### 3. シーケンス / `AUTO_INCREMENT` と `IDENTITY`
- **競合**: 双方で同じ連番の主キーが新規挿入された場合は `insert_insert` コンフリクトとしてエラーにする。
- **PostgreSQL `GENERATED ALWAYS AS IDENTITY`**: 明示的な主キー値を `INSERT` するため、`OVERRIDING SYSTEM VALUE` 句を付与して実行する。
- **マージ後のカウンタ同期（必須）**:
  - MySQL: `SET SESSION information_schema_stats_expiry = 0` を実行した上で、`MAX(col) + 1` と $S$・$T$ の `AUTO_INCREMENT` 値を比較し、最大値へ `ALTER TABLE ... AUTO_INCREMENT = <next>` で進める。
  - PostgreSQL: `pg_sequences` および `SELECT last_value, is_called FROM <seq>` から $S$ と $T$ の状態を取得し、増分方向（昇順/降順）に応じて `setval` で $S$・$T$・`MAX(col)`（降順は `MIN(col)`）の極値以上に進める。

#### 4. 外部キー（Foreign Keys）とユニーク制約の適用順序
- **適用順序（値スワップ・削除後挿入対策）**:
  - 同一テーブル内のユニーク列の入れ替え（`id=1 ('a'), id=2 ('b')` のスワップ）や削除後挿入が即時制約チェックで落ちないよう、適用順を **DDL → `DELETE`（子→親の逆トポロジカル順）→ `UPDATE`（ユニーク列スワップ時は削除＋再挿入へ展開）→ `INSERT`（親→子のトポロジカル順）→ `DROP TABLE`** とする。PostgreSQL で遅延可能な制約は `SET CONSTRAINTS ALL DEFERRED` を併用する。
- **自己参照・循環参照**:
  - 適用セッション中は PostgreSQL で `SET session_replication_role = 'replica'`、MySQL で `SET FOREIGN_KEY_CHECKS = 0` を用いて一時的に制約チェックを迂回する。
- **事後検証（必須）**:
  - 差分計算時および変更適用完了前（制約チェックを有効に戻した状態）に、**「`DELETE` または PK `UPDATE` が発生した親テーブルを参照するすべての子テーブル」**（$S$ 側で子テーブルに変更が無くても $T$ 側で新規子行が挿入されている可能性があるため）および「子テーブル側で `INSERT` / FK `UPDATE` が発生したテーブル」に対して `LEFT JOIN` による孤児レコード検証クエリを実行する。
  - 複合外部キー（`MATCH SIMPLE`）で FK 列の一部が `NULL` の行は合法であるため検証から除外する。孤児が 1 件でもあれば **`foreign_key_conflict`** として失敗（適用時はロールバック）する。
- **ユニーク制約の事前・適用時検証**:
  - 標準 `UNIQUE` 制約の `NULL` 複数許容、PostgreSQL 15+ の `UNIQUE NULLS NOT DISTINCT` を区別して事前検証する。PostgreSQL の部分ユニークインデックス・式インデックスや照合順序依存の一意制約は、適用時のユニーク違反エラー（PostgreSQL `23505`、MySQL `1062`）も捕捉して **`unique_violation`** としてロールバックする。

#### 5. トリガ（Triggers）
- $S$ 側で既にトリガが発火した結果の行変更もマージ対象に含まれるため、$T$ 側への適用時にトリガが再発火すると二重計上等が起きる。
- **PostgreSQL**: マージ適用はエージェント自身のローカル管理者（`Superuser`、`p.Admin(s)`）接続で実行するため、`SET session_replication_role = 'replica'` により通常トリガの発火を抑止する。
- **MySQL**: セッション単位でトリガを無効化する標準機能がないため、変更対象テーブルにトリガが存在する場合は安全のため**エラー（`triggers_not_supported`）として拒否し、`--ignore-triggers` 指定時のみトリガが発火する前提で適用を許可**する。

#### 6. 巨大テーブルの扱い
- 全行をメモリに読み込まず、`ORDER BY <pk>` のストリーミングカーソルで比較する（メモリ使用量 $O(1)$）。
- **変更のないテーブルのスキップ（v0.3.1 高速化）**: スキーマ（列定義・主キー・一意制約）が Ancestor / Source / Target で一致し、さらに Ancestor と Source のテーブル単位ダイジェスト（MySQL は `CHECKSUM TABLE` と空テーブル確認、PostgreSQL は $O(1)$ メモリの `sha256` 集約ハッシュ＋件数）が一致するテーブルは行の走査をスキップする（偽陽性を避けるため、条件を満たさない場合やダイジェスト取得不可時は従来どおり全行走査へフォールバックする）。外部キー事前検証はスキップしたテーブルに対しても変更テーブルの参照キーのみ対象インスタンスへ問い合わせて維持する。
- `--tables` フラグによる対象テーブルの絞り込みをサポートする（対象テーブルと外部キー依存関係にあるテーブルが指定から欠けている場合はエラーとする）。
- **今後の拡張案**: (b) 有償 `history` モジュールの記録があるブランチで変更テーブルや変更範囲を絞り込む方式、(c) 記録済みクエリ履歴を再実行するマージモードは将来検討とする。

## 4. コンフリクトの定義とエラー表示仕様

### コンフリクトの種類

| `type` | 説明 |
|---|---|
| `update_update` | 同一行（同一 PK）の 1 つ以上の列が $S$ ラインと $T$（ライブ）ラインの両方で異なる値に更新された |
| `delete_update` | $S$ ラインで削除された行が、$T$ ラインで更新された |
| `update_delete` | $S$ ラインで更新された行が、$T$ ラインで削除された |
| `insert_insert` | 共通祖先 $A$ に存在しない同一 PK の行が、$S$ と $T$ の両方で異なる値として挿入された |
| `unique_violation` | PK は異なるが、`UNIQUE` 制約列（またはユニークインデックス）の値が $S$ の挿入/更新と $T$ の行で衝突した |
| `foreign_key_conflict` | 親行の削除と子行の挿入などにより、マージ後に外部キー制約違反が発生する |
| `schema_conflict` | 同一テーブルに対する両側の DDL 競合、または未許可の DDL と行変更の競合 |
| `schema_no_primary_key` | 差分が存在するテーブルに主キーおよび `NOT NULL` 一意制約がない |

### エラー表示仕様（Operation Result / CLI）

コンフリクト検出時は最初の 1 件で止めず、最大 50 件（`MaxMergeConflicts = 50`）のコンフリクトを収集して提示する。

#### REST API（非同期オペレーション `GET /v1/operations/{id}`）

`POST /v1/branches/{name}/merge` は非同期オペレーション（`202 Accepted`）を返すため、実行フェーズで検出されたコンフリクト・エラーは `upgrade.go`（`UpgradeResult`）と同様に `Operation.State = "failed"` かつ `Operation.Result`（`MergeResult`）として格納する（[api.md](api.md#L132-L150) 準拠）。

```json
{
  "id": "op_01JC8Z6Q4M7V3X2N9K5R8T1W0B",
  "kind": "branch.merge",
  "state": "failed",
  "error": "merge aborted: 2 conflict(s) detected in branch \"feature-x\" (line 0 -> live line 1)",
  "result": {
    "branch": "feature-x",
    "source_line": 0,
    "target_line": 1,
    "ancestor": "before-migration",
    "status": "conflicted",
    "code": "merge_conflict",
    "total_conflicts": 2,
    "truncated": false,
    "conflicts": [
      {
        "type": "update_update",
        "database": "shop",
        "table": "orders",
        "key": { "id": 1042 },
        "columns": ["status"],
        "ancestor_values": { "status": "pending" },
        "source_values":   { "status": "shipped" },
        "target_values":   { "status": "cancelled" }
      },
      {
        "type": "unique_violation",
        "database": "shop",
        "table": "users",
        "constraint": "users_email_key",
        "key": { "id": 205 },
        "conflicting_target_key": { "id": 198 },
        "columns": ["email"],
        "source_values": { "email": "alice@example.com" },
        "target_values": { "email": "alice@example.com" }
      }
    ]
  }
}
```

#### CLI 表示と終了コード

- **終了コード**: `0` = 成功、`1` = その他のエラー、`2` = コンフリクト（`merge_conflict`）。既存の [cli.go](../internal/interface/cli/cli.go#L21-L28) の `ExitError` を用いる。
- **`-o json` 出力**: 上記 `result`（`MergeResult`）をそのまま標準出力へ JSON 出力する。
- **人間向け表示（Stderr）**:

```text
Error: merge aborted: 2 conflict(s) detected in branch "feature-x" (line 0 -> live line 1; live data was not modified)

  1. [update_update] shop.orders (id=1042)
     column "status": ancestor="pending", source="shipped", target="cancelled"

  2. [unique_violation] shop.users (id=205, constraint=users_email_key)
     column "email": value="alice@example.com" conflicts with target row (id=198)
```

## 5. 原子性・稼働中ブランチの扱い・ロック・並行制御

### 稼働中ブランチの扱いと TOCTOU 防止

- **状態遷移と排他（本実行）**:
  - `s.mu` のロック下で `br.CanMerge()`（`ready` または `paused` のみ許可）を確認し、ブランチを **`BranchMerging`**（`BranchStatus = "merging"`）に遷移させる。
  - `BranchMerging` 中は ProxySQL の `routable`（[routing.go](../internal/usecase/routing/routing.go#L47-L49)）から外れ、同じブランチへの他操作（`DeleteBranch`, `ResetBranch`, `RestoreCheckpoint`, `PauseBranch`, `ResumeBranch`, `UpgradeBranch`, `ForkBranch`, `CreateCheckpoint`, `DeleteCheckpoint`, 別の `MergeBranch`）は **`409 conflict`** で拒否され、TTL 失効（`Expire`）もスキップされる。
  - **ライブデータの一貫性保護**: `BranchMerging` 遷移直後に（元々 `ready` だった場合は）**`StopClean` して既存の直接接続・ProxySQL 接続を完全に遮断した上で**、ライブの一時スナップショット取得・比較・適用を実行する。これにより、比較中に外部クライアントが書き込む TOCTOU レースを完全に排除する。
- **`--dry-run` の扱い**:
  - `--dry-run` は DB サーバーを停止せず接続断を起こさない。ただし、一時クローン（`work/merge-<short_id>`）および一時スナップショットが存在する間に `DeleteBranch` / `Expire` や `RestoreCheckpoint` / `ResetBranch` / `UpgradeBranch` / `ForkBranch` / `DeleteCheckpoint`（データセットのリネームや破棄を伴う操作）が競合すると、`DestroyOrder` の `ErrInUse` によるブランチ破損や `DestroySnapshot` の失敗が起きる。そのため、`--dry-run` 実行中は `Service` 内のインメモリ排他（`mergingDryRun[name]`）により対象ブランチへのこれら状態変更系操作および `Expire` を `409 conflict` で一時的に排他し、`defer` の一時スナップショット削除も `domain.FindSnapshot(family, ...)` で探索して確実に回収する。

### 原子性の保証（失敗時はライブラインを一切変更しない）

1. **検証・差分計算フェーズ**:
   - ライブデータセットには一切書き込まない。コンフリクトやスキーマ不整合があればここでエラー終了し、元々 `ready` だった場合はサーバーを再起動して `BranchReady`（`paused` だった場合は `BranchPaused`）に戻す（自動チェックポイントは作らない）。
2. **適用フェーズ（専用意図プロパティ + 自動チェックポイント + ロールバック）**:
   - **専用意図プロパティ `db-branch:merge`（`PropMerge`）**: `restore.go` の `readIntent`（`PropOp` / `PropSource` / `PropNewLine` 必須）と衝突しないよう、`upgrade.go` の `PropUpgrade` と同様に `history/<name>` へ専用プロパティ `db-branch:merge`（開始前のステータス `ready` / `paused`、自動チェックポイント名 `before-merge-<時刻>`、ロールバック用の新しいライン番号とランタイムを記録）を書き込む。
   - **適用前の自動チェックポイント**: `StopClean` 済みの状態で自動チェックポイント **`before-merge-<時刻>`**（`PropAuto: "on"`、`MaxAutoCheckpoints = 5` の回転対象、`branch.max_checkpoints` に数えない）を取得する。
   - **メンテナンスモードでの SQL 適用**: 外部接続不可の `Maintenance: true` でサーバーを起動して SQL を適用・事後検証（FK・カウンタ同期）し、PostgreSQL の新設 DDL オブジェクトも含め `putLoginRight`（`CreateUser` / `ownObjects`）でログイン権限・所有権を整合させてから `StopClean` する。
   - **完了時の順序**: 成功時は `db-branch:merge` プロパティを消去（`Unset`）し、元々 `ready` ならサーバーを起動して `BranchReady`（`paused` なら停止のまま `BranchPaused`）へ戻す。
   - **失敗時の自動ロールバック**: SQL 適用中や事後検証でエラーが起きた場合は、直ちに `before-merge-<時刻>` をスワップ復元し（`paused` だった場合は起動せず `BranchPaused`、元々 `ready` なら起動＋`putLoginRight` して `BranchReady`）、`db-branch:merge` を消去する。
   - **`Recover`（エージェント再起動時の復旧）**:
     1. 起動時に `<pool>/db-branch/work/merge-*` の残留クローンおよび各ブランチの `@merge-*` 一時スナップショットを探索し、存在すれば一時サーバー停止・破棄（GC）する。
     2. ステータスが `BranchMerging` のブランチについて `db-branch:merge` を読み、自動チェックポイント取得前なら開始前の状態（`ready` なら `recoverRunning`、[branch.go](../internal/usecase/dbbranch/branch.go#L544-L575)、`paused` なら停止維持）に戻す。取得後（またはロールバック途中で二重クラッシュした場合）なら、既存の `runRestore`（無条件でサーバーを起動して `BranchReady` にしてしまう [restore.go](../internal/usecase/dbbranch/restore.go#L232)）へ丸投げせず、`BranchMerging` のまま `before-merge-<時刻>` からのスワップ復元を冪等に完了させ、開始前の状態（`ready` なら起動＋`putLoginRight` して `BranchReady`、`paused` なら停止のまま `BranchPaused`）へ戻した後に `db-branch:merge` を消去する。

## 6. インターフェース仕様

### 6.1 CLI

```sh
# ブランチ feature-x の line 0 の変更を現在のライブラインへマージ
db-branch branch merge <branch> <source-line> [flags]

# 例: ドライランでコンフリクト確認（JSON 出力）
db-branch branch merge feature-x 0 --dry-run -o json

# 例: 特定のチェックポイント時点をマージ元として指定
db-branch branch merge feature-x --checkpoint old-work
```

| フラグ | 説明 | 既定値 |
|---|---|---|
| `--checkpoint <name>` | `<source-line>` の代わりに特定のチェックポイントをマージ元として指定する | なし |
| `--dry-run` | ライブラインを変更せず、コンフリクト検査と変更サマリのみを返す | `false` |
| `--tables <list>` | 対象テーブルをカンマ区切りで限定する | 全テーブル |
| `--allow-ddl` | 既存テーブルへの互換性のある `ALTER TABLE` を許可する | `false` |
| `--ignore-triggers` | MySQL でトリガが存在するテーブルへの適用を許可する | `false` |
| `--no-wait` | オペレーション ID だけを表示して即座に戻る | `false` |
| `-o, --output` | `table` または `json` | `table` |

### 6.2 REST API

| メソッド | パス | 応答 | `x-dbb-action` | 概要 |
|---|---|---|---|---|
| `POST` | `/v1/branches/{name}/merge` | `202 Accepted`（オペレーション `branch.merge`） | `branch.merge` | `{name}` 内の指定ライン（またはチェックポイント）を現在のライブラインへマージ |

リクエストボディ（`source_line` または `checkpoint` のいずれか一方のみ必須）:

```json
POST /v1/branches/feature-x/merge
{
  "source_line": 0,
  "dry_run": false,
  "tables": ["users", "orders"],
  "allow_ddl": false,
  "ignore_triggers": false
}
```

- `phase` 遷移: `stopping`（`dry_run` 以外）→ `snapshotting` → `starting_clones` → `diffing` →（`dry_run` なら終了）→ `checkpointing` → `applying` → `verifying` → `starting`（失敗時は `rolling_back`）。
- 担当ノード以外に届いた場合は、クラスタプロトコル（`ProtocolBranchMerge = 26`）を検証した上で `br.Node` へ転送（`s.forward`）して実行する。

### 6.3 MCP ツール

[mcp.md](mcp.md) および [mcp.go](../internal/interface/httpapi/mcp.go) に `merge_line` ツールを追加する。

| ツール | 入力パラメータ | 動作 | 戻り値 |
|---|---|---|---|
| `merge_line` | `branch`（必須）, `source_line`（任意）, `checkpoint`（任意）, `dry_run`（任意）, `tables`（任意）, `allow_ddl`（任意）, `ignore_triggers`（任意） | `POST /v1/branches/{branch}/merge` を呼び出し、完了まで待機する（`startAndCollect`） | `MergeResult`（変更サマリまたはコンフリクト詳細） |

### 6.4 Web コンソール

[design.md §11](design.md#L352-L366) および [checkpoints.md §10](checkpoints.md#L110-L127) に準拠する。

- **Checkpoints パネル（`checkpoints.tsx`）**:
  - 現在のライブラインとは異なるライン上のチェックポイントを選択した詳細欄に **Merge into live** ボタンを配置する（`status` が `ready` または `paused` かつ `checkpoints === true` かつ `branch.merge` 権限があるとき有効）。
  - クリックするとダイアログが開き、まず自動的に Dry run（プレビュー）を実行して変更サマリ（`+15 / ~3 / -1 rows`）またはコンフリクト詳細を表で表示し、問題なければ **Apply Merge** で本実行する。

## 7. クラスタ・マスキング・権限・リソース上限

- **クラスタ（Raft）での扱い**:
  - 1 つのブランチの全ラインは常に同一ノード（`br.Node`）に存在するため、**クロスノードのデータ転送は不要**。API リクエストが別ノードに届いた場合のみ `br.Node` へ転送する。
  - [fsm.go](../internal/infrastructure/raftstore/fsm.go#L114-L162) の規約に従い、`BranchMerging` ステータスを全ノードが認識できるよう `ProtocolBranchMerge = 26` を要求し、全ノードが上がるまで `503 cluster_upgrading` を返す。
- **マスキングとの関係**: 同一ブランチ内のライン間の統合であるため、ベース作成時のマスキング（[masking.md](masking.md)）の境界を越えることはない。
- **権限（[ownership.go](../internal/interface/httpapi/ownership.go#L90-L193)）**:
  - 認可アクション名は `branch.merge`。`canManageBranch` に従い、所有者・管理者（`admin` / `scope:full`）・チーム共有（`SharesBranches`）、および所有者の権限範囲で動く `branches` スコープトークンのみ実行可能。所有者なしレガシーブランチ（`Owner == ""`）は管理者のみ。ログインなし（`Anonymous`）や他人の `open` ブランチでは `403 forbidden`。
- **セキュリティ要件（`SEC-22` / `SEC-05` 準拠）**:
  - **SQL インジェクション防止（`SEC-22`）**: Source から抽出した DDL/DML を Target へ管理者（`Superuser` / `root`）権限で適用する際、テーブル名・列名・制約名・型名は厳格にエスケープ/クォートし、DML の行値はすべてパラメータバインドで渡す。`DEFAULT` 式・生成列式・`CREATE TABLE` は構造化メタデータからの再構築と式構文検証（許可された演算子・リテラル・純粋関数のみ、複文 `;`・コメント・ドルクォート・サーバーファイル/プログラム操作関数の拒否）を行い、任意の複文やサーバー側ファイル/プログラム操作を含めさせない。
  - **コンフリクト詳細の閲覧認可（`SEC-05`）**: `Operation.Result`（`MergeResult`）には生行データが含まれるため、`StartResultAs` により記録された操作所有者に対して `GET /v1/operations/{id}` の `canSeeOperation` スコープ制限を適用する。
- **リソース上限（W-15 整合）**: 一時クローン起動（Ancestor / Source / Target 最大 3 インスタンス、最小メモリ `64MB` 等）のためノードのメモリ予算（不足時は `503 no_capacity`）と ZFS プール空き容量（`checkPoolRoom`、残量 10% または 1GiB 未満で `507 no_space`）を確認する。比較・適用にはタイムアウト（既定 15 分）を設ける。

## 8. エラーケース・境界条件

### 受付時（同期 HTTP レスポンス）

| 条件 | HTTP | エラー `code` | 備考 |
|---|---|---|---|
| ブランチが存在しない | 404 | `not_found` | ブランチ未存在 |
| `source_line` と `checkpoint` の両方指定または両方未指定 / 負のライン番号 / 不正なチェックポイント名 | 400 | `invalid_request` | リクエスト不正 |
| ブランチが `ready` / `paused` 以外（または `--dry-run` 実行中） | 409 | `conflict` | 現在の `status` を明示 |
| エンジンが `checkpoints` 非対応 | 409 | `feature_not_supported` | `domain.ErrFeatureNotSupported` |
| ストレージが ZFS チェックポイント非対応（`Lineage == nil`） | 409 | `checkpoints_unavailable` | `domain.ErrCheckpointsUnavailable` |
| 権限不足（他人のブランチ・`open` ブランチ・Anonymous） | 403 | `forbidden` | `action: "branch.merge"` |
| クラスタに Protocol 26 未満のノードがある | 503 | `cluster_upgrading` | 全ノード更新後に利用可能 |
| 一時サーバー起動のためのメモリ不足 | 503 | `no_capacity` | `domain.ErrNoCapacity` |
| プール空き容量不足（残量 10% または 1GiB 未満） | 507 | `no_space` | `domain.ErrNoSpace` |

### 実行中（オペレーション `state: "failed"` + `result.code`）

| `result.code` | 説明 |
|---|---|
| `not_found` | 指定 `source_line` または `checkpoint` が存在しない |
| `invalid_request` | `source_line == live_line`、Source が共通祖先と同一で変更なし、または `--tables` で FK 依存テーブルが欠落 |
| `no_common_ancestor` | 共通祖先を特定できない（履歴記録なし、祖先スナップショット完全削除済み） |
| `merge_conflict` | データ・スキーマ・FK・ユニーク制約のコンフリクト（`conflicts` に詳細） |
| `schema_no_primary_key` | 変更のあるテーブルに主キー / `NOT NULL` 一意制約がない |
| `triggers_not_supported` | MySQL でトリガが存在するテーブルへの変更がある（`--ignore-triggers` なし） |
| `runtime_not_installed` | 必要なランタイム（過去ラインの `PropRuntime`）が未インストール |

## 9. 受け入れ条件

1. **ライン間の基本マージ（MySQL / PostgreSQL）**: `line 0` で行変更後に `cp1` を取り、さらに `line 0` で変更 A を加えて `cp2` を取った後、`cp1` へ `restore` して `line 1` で変更 B を加えた状態で `branch merge <br> 0` を実行すると、変更 A と B がライブラインに統合され、`line 0` のチェックポイントは壊れず残ること。
2. **3-way 削除の正確な反映**: 共通祖先 `cp1` に存在し `line 1` で未変更の行を `line 0` で `DELETE` した場合、マージ後にライブラインからも該当行が削除されること。
3. **コンフリクト検出と無変更保証**: 同一主キーの両側更新、更新と削除、重複挿入、ユニーク制約違反、FK 参照整合性違反が発生した場合、`merge_conflict`（CLI 終了コード `2`、`Operation.Result.Code == "merge_conflict"`）で失敗し、テーブル・主キー・値がエラーに表示され、ライブラインが一切変更されないこと。
4. **`--dry-run`**: `--dry-run` 指定時は DB サーバーを停止せず（接続断なし）、サマリまたはコンフリクトのみを返すこと。
5. **原子性と自動ロールバック（`ready` / `paused` 両方）**: 適用フェーズの途中でエラーが起きた場合、自動チェックポイント `before-merge-<時刻>` へ自動ロールバックされ、ライブラインがマージ前のデータ・ステータス（`ready` または `paused`）に完全に復帰すること。
6. **`AUTO_INCREMENT` / シーケンス・`IDENTITY` 同期**: マージ後にライブラインで主キー省略 `INSERT` を行っても重複エラーにならず、`GENERATED ALWAYS AS IDENTITY` テーブルでもマージできること。
7. **テストカバレッジ**: `CLAUDE.md` の基準どおり、全パッケージのユニットテストカバレッジが 95% 以上を維持すること（`make vet cover` が通ること）。

## 10. 実装用 TODO リスト（t-wada 式 TDD）

### 1. `internal/domain`（外部依存なし）
- [x] **Walking Skeleton**: 3-way 行比較・コンフリクト判定モデル（`MergeAction`, `MergeConflict`, `DiffTableRows`、NULL セーフ比較・精度保証比較）のテストと実装。
- [x] `SnapshotInfo` への `CreateTXG uint64` 追加と、`ResolveLineAncestor`（親子ライン・兄弟ライン・`gone-*` の `SnapshotInfo.Clones` 逆引きおよび `PropLine` 追従・`ErrNoCommonAncestor`）のテストと実装。
- [x] `BranchMerging BranchStatus = "merging"`、`PropMerge = "db-branch:merge"`、`ProtocolBranchMerge = 26`、`Branch.CanMerge() error` のテストと実装。
- [x] 外部キー依存グラフのトポロジカルソート（`DELETE` 逆順・`INSERT` 順・循環参照検出・`--tables` FK 依存検証）のテストと実装。

### 2. `internal/infrastructure`
- [x] `zfs/checkpoint.go`: `createtxg` の `SnapshotInfo.CreateTXG` への読み取り追加。
- [x] `postgres/server.go`: `Server.Maintenance == true` 時の `PG_BIND=""` 起動テスト確認。
- [x] `raftstore/fsm.go`: `Protocol` 更新（`25` → `26`、`ProtocolBranchMerge = 26`）と `BranchMerging` の `Command.Needs()`・シリアライズテスト。
- [x] `sqlmerge`: 論理マージアダプタ（MySQL / PostgreSQL それぞれで `BASE TABLE` / 宣言的パーティション / 生成列 / `ENUM` 対応、PK 昇順ストリーミング走査、`OVERRIDING SYSTEM VALUE`、ユニーク・FK 孤児検証〔`MATCH SIMPLE` 対応〕、トリガ検査、`AUTO_INCREMENT` / `SEQUENCE` 同期、識別子クォート・パラメータバインド・DDL 式検証〔`SEC-22`〕）。

### 3. `internal/usecase`（`routing` / `dbbranch/merge.go`）
- [x] `internal/usecase/routing/routing.go`: `routable` が `BranchMerging` を除外することのテスト。
- [x] `MergeBranch` の状態遷移・排他テスト（`BranchMerging` 遷移、`StopClean` による TOCTOU 防止、並行操作 `409 conflict`、`ready` / `paused` 復帰）。
- [x] 一時クローン作成（`workDataset` 再利用、親データセット事前作成、短い `Server.ID`）・Target の `branchSpec(..., true, false)` メンテナンス起動・`defer` クリーンアップテスト。
- [x] `--dry-run` モードのテスト（サーバー無停止、`DeleteBranch` / `Expire` 等の状態変更操作との排他保護）。
- [x] 適用失敗時の自動チェックポイント `before-merge-<時刻>` 作成と自動ロールバックテスト（`Recover` での `work/merge-*` 掃除、二重クラッシュ復帰、`putLoginRight` による非スーパーユーザー権限維持、`paused` 状態維持含む）。
- [x] `ErrClusterUpgrading` と `ErrCheckpointsUnavailable` テスト。

### 4. `internal/interface/httpapi` / `mcp` / `cli` / `web`
- [x] `POST /v1/branches/{name}/merge` ハンドラと `Operation.Result`（`MergeResult`）返却、混在クラスタ `503 cluster_upgrading` ガード、`br.Node` への転送テスト。
- [x] 認可テスト（`ownership.go`: 所有者・管理者・共有・`branches` トークンのみ許可、`open` / 匿名 / 所有者なしレガシーは `403`、`Operation.Result` の閲覧制限）。
- [x] `api/openapi.yaml` 更新と `openapi_test.go` パス確認。
- [x] MCP ツール `merge_line` の登録・実行テスト。
- [x] CLI `db-branch branch merge` のフラグ・終了コード `2` テスト。
- [x] Web コンソール（Checkpoints パネルの「Merge into live」ボタン・Dry run プレビューダイアログ・`DemoBackend` 対応）。

## 11. 将来の拡張・人間判断の余地（v1 は安全な既定動作を採用）

v1 ではデータ破壊や意図しない副作用を防ぐ安全な既定動作を採用しており、以下は運用実績に応じて将来の拡張として検討できる。

1. **マージ方式（3-way 状態比較 vs クエリ履歴のタイムスタンプ順合体）**:
   - v1 は `Record: false`（既定）のすべてのブランチで動作し、主キー単位のコンフリクトを正確に検出できる **案 1（3-way 論理比較）** を採用。将来、有償 `history` モジュール有効ブランチ向けの高速パスとして案 2/案 3 を併用する選択肢がある。
2. **既存テーブルへの DDL（`ALTER TABLE`）の既定挙動**:
   - v1 は安全のため既存テーブルのスキーマ不一致を既定で `schema_conflict` とし、`--allow-ddl` 指定時のみ互換性のある `ADD COLUMN`（`NULL` 許容または `DEFAULT` あり）を許可する。
3. **MySQL トリガ存在テーブルの扱い**:
   - v1 は変更対象テーブルにトリガが存在する場合、既定で `triggers_not_supported` として拒否し、`--ignore-triggers` 指定時のみ適用を許可する（一時的な `DROP TRIGGER` → `CREATE TRIGGER` は行わない）。
4. **退避ラインにチェックポイントが無い場合のライフサイクル**:
   - v1 は既存の `restore.go` / `Garbage` のライフサイクルを維持する。復元先に選んだチェックポイントより新しいチェックポイントが元の線にない場合、旧線は `restore` の最後に `Garbage` で破棄され、最後のチェックポイントより後の変更は失われる。マージしたい変更がある場合は `restore` の前にチェックポイントを取る必要があり、現存する退避ラインまたはチェックポイントがあるラインをマージ元として指定できる。
5. **履歴グラフ上のマージ矢印描画**:
   - v1 は純粋な 3-way 比較（冪等）を行い、ZFS 上への特別なマージ済みメタデータ記録や履歴グラフ上のマージ矢印描画は行わない。
