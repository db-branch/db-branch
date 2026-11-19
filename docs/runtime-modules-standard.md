# Runtime モジュール標準化仕様（サードパーティ DB エンジン拡張設計）

MySQL / PostgreSQL に続く新しいデータベースエンジン（ClickHouse 等）を、コアを変更せずに独立したモジュール（サードパーティ製を含む）として配布・実行できるようにするための標準契約、依存機能の標準化、配布・信頼モデル、および移行計画を定義する。

> [!NOTE]
> **main v0.2.7 での再照合（2026-11-19）**
> `v0.2.6` / `v0.2.7` で導入されたセキュリティ強化（管理ソケット・設定の `db-branch-admin` グループ分離、`InaccessiblePaths`、DB ログインの非スーパーユーザー化、`init_sql` の `ExecAs` 実行）および `docs/security-review.md`（SEC-22）の指摘を踏まえ、現行コードベースと再照合した。主な変更点は以下の通り。
>
> | 項目 | 変更前 | 変更後（v0.2.7 照合後） | 理由 |
> |---|---|---|---|
> | **systemd パス隔離と権限分離の前提** | `db-branch` ユーザーと `root` の分離のみを前提とし、`db-branch-rt@.service` に `InaccessiblePaths` 未指定 | `db-branch-rt@.service` にも `InaccessiblePaths=/run/db-branch /etc/db-branch` を必須化。`db-branch-admin` 分離後も既存 DB の `DataDir`・ソケットは `db-branch` 所有のため、`db-branch-rt-<engine>` 分離結論は維持 | `v0.2.7` で `agent_http.go` / `postinst` / `db-branch-mysqld@.service` / `db-branch-postgres@.service` に導入された多層防御と整合させるため |
> | **`RuntimeProvider` の `ExecAs` と非特権 `CreateUser`** | `CreateUser` と `Query` のみで、`init_sql` の実行権限やログイン権限上限が未規定 | `RuntimeProvider` に `ExecAs` を追加し、`CreateUser` に非スーパーユーザー・OS ファイル/プログラム実行不可（および起動・復旧・復元時の再適用）を契約化 | `v0.2.7` で `DBServer.ExecAs` と非スーパーユーザー `CreateUser`（`mysql` `clientPrivileges`+`partial_revokes`, `postgres` `NOSUPERUSER`+`OwnerRole`）が導入されたため |
> | **`LaunchPlan` / `Catalog()` の境界防御** | `override.conf` 書き出し時のサニタイズや `Catalog()` URL 検証が未規定 | `LaunchPlan` → `override.conf` の改行・ディレクティブインジェクション拒否、`Catalog()` URL の SSRF 検証、同一エンジン内影響範囲の明記を追加 | `docs/security-review.md`（SEC-22）の指摘を設計契約として取り込むため |

---

## 1. 目的と前提

- **背景**: 現行の db-branch は、認証・認可・監査・プロキシ・マスキング・履歴・ストレージを `pkg/ext` 経由の外部モジュールとして分離できている一方、DB エンジン本体（runtime：`mysql`, `postgres`）はコアにハードコードされている。
- **目標**:
  1. サードパーティを含む外部開発者が新しい DB エンジン（例: ClickHouse）を runtime モジュールとして実装・配布できる標準契約を定める。
  2. `history`・`masking`・`branch merge`・`MCP`・`Web console`・`proxy`・監視などの runtime 依存機能を「コア共通部分」と「runtime adapter 部分」に分離し、Capability 宣言によって非対応機能を安全かつ明示的に無効化できるようにする。
  3. ZFS スナップショット／クローンの整合性と、root 権限で動作するエージェントにおけるセキュリティ境界（権限分離・署名検証）を両立する。
- **非目標**: 本仕様書は設計標準であり、既存コードの変更や個別エンジンの実装コードは含まない。

---

## 2. 現状分析：現在の「モジュール」とコア埋め込みの runtime 責務

### 2.1 現状の拡張モジュール（`pkg/ext` / `pkg/ext/plugin`）と外部バイナリ連携

db-branch における現行の「モジュール」は、コア本体を変えずに別バイナリとして機能を追加する仕組みを指す（[docs/modules.md](modules.md)）。

- **プロセス間通信と配置**:
  - `hashicorp/go-plugin` の `net/rpc`（`ProtocolVersion: 1`, `MagicCookieKey: "DB_BRANCH_MODULE"`）を用いて、エージェントの子プロセスとして stdio 経由で起動・通信する（[pkg/ext/plugin/plugin.go](../pkg/ext/plugin/plugin.go#L36-L40), [L691-L764](../pkg/ext/plugin/plugin.go#L691-L764)）。
  - バイナリは `/usr/lib/db-branch/modules/<name>` に配置し、実行権限あり・group/others 書き込み不可（`perm & 0o022 == 0`）のものだけを名前順にロードする（[internal/app/agent.go](../internal/app/agent.go#L41), [pkg/ext/plugin/plugin.go](../pkg/ext/plugin/plugin.go#L659-L689)）。
- **提供ポート（[pkg/ext/ext.go](../pkg/ext/ext.go#L513-L554)）**:
  - `Authenticator`, `Authorizer`, `AuditSink`, `BranchPolicy`, `Router`（`cmd/db-branch-proxysql`）, `Storage`, `Masker`（`cmd/db-branch-greenmask`, `cmd/db-branch-greenmask-mysql`, `cmd/db-branch-anon`）, `MaskPolicy`, `RefreshFollower`, `ServerOptioner` / `RestoreFinisher`（有償 `history` モジュール向け）, `Handler` / `Pages`（モジュール独自 API・コンソール画面）, `Starter`（`Host.Store`, `Host.DataDir`, `Host.Branches`, `Host.API` の注入）。
- **APT によるクラスタ同期（[internal/usecase/module/module.go](../internal/usecase/module/module.go#L17-L161), [internal/app/agent_modules.go](../internal/app/agent_modules.go#L208-L273)）**:
  - Raft の `Modules` に `ModuleSpec{Name, Version, Wanted}` を記録し、各ノードが 30 秒周期（変更時は即座）に `systemd-run --quiet --wait --collect` 経由で `apt-get install/remove db-branch-<name>` を実行する（[internal/infrastructure/apt/apt.go](../internal/infrastructure/apt/apt.go#L118-L129)）。
  - 未ロードのモジュールがあるノードは `503 modules_pending` で API を拒否する。
- **既存外部バイナリ連携のパターン**:
  - `db-branch-proxysql`（[cmd/db-branch-proxysql/main.go](../cmd/db-branch-proxysql/main.go#L16-L25)）: 上流 `proxysql`（`.deb`）に依存し、Admin API (`127.0.0.1:6032`) 経由で設定を投入する。
  - `db-branch-greenmask` / `greenmask-mysql` / `anon`（[internal/infrastructure/greenmask/greenmask.go](../internal/infrastructure/greenmask/greenmask.go), [mysql.go](../internal/infrastructure/greenmask/mysql.go), [internal/infrastructure/anon/anon.go](../internal/infrastructure/anon/anon.go)）: コアから `ext.DBAccess`（`Engine`, `Host`, `Socket`, `Port`, `User`, `BinDir`, `LibDir`）を受け取り、モジュール自身が外部 CLI（`greenmask`, `psql`, `mysql`, `pg_dump` 等）を実行する。

### 2.2 コアにハードコードされている runtime 責務一覧

現状、`mysql` と `postgres` の 2 エンジンはドメイン層から UI 層までコアに深く埋め込まれている。

| 責務領域 | 現状の実装箇所（ファイル・行） | ハードコードされている内容 |
|---|---|---|
| **エンジン定義・検証** | [internal/domain/runtime.go](../internal/domain/runtime.go#L14-L39) | `Engine` 定数 `MySQL`, `Postgres` のみ許可（`ParseRuntimeRef`） |
| **DSN 生成・Server 契約** | [internal/domain/branch.go](../internal/domain/branch.go#L325-L336), [L375-L431](../internal/domain/branch.go#L375-L431) | `Branch.DSN` が `Postgres` のみ `/postgres` パス付与。`Server` / `StatsUpdater` / `DBServer`（`Exec`, `ExecAs`, 非特権 `CreateUser` を含む）定義 |
| **Upgrade パス判定** | [internal/domain/upgrade.go](../internal/domain/upgrade.go#L117-L201) | `mysqlLTS`（5.7/8.0/8.4/9.7）と `pgMajor` 一致制約がドメイン関数 `CheckUpgradePath` に固定 |
| **OS/Collation 配置制約** | [internal/domain/placement.go](../internal/domain/placement.go#L37), [internal/usecase/dbbranch/distro.go](../internal/usecase/dbbranch/distro.go#L7-L22) | `CollationBound` が `Postgres` のみ `true` |
| **定期リフレッシュ検証・ダンプ** | [internal/domain/refresh.go](../internal/domain/refresh.go#L61), [internal/infrastructure/srcdump/srcdump.go](../internal/infrastructure/srcdump/srcdump.go#L34-L105) | `pg_dump` / `mysqldump` 呼び出しとエンジン分岐をコアが保持 |
| **ランタイムカタログ・展開** | [internal/infrastructure/catalog/runtimes.json](../internal/infrastructure/catalog/runtimes.json), [catalog.go](../internal/infrastructure/catalog/catalog.go), [internal/infrastructure/runtimestore/store.go](../internal/infrastructure/runtimestore/store.go#L136-L154), [internal/app/app.go](../internal/app/app.go#L113-L120) | 組み込み JSON カタログ、`.deb`/`tar.xz`/`tar.gz` 展開、`mysql.Fixup` / `postgres.Fixup`（`libaio`/`ncurses`/`lib` シンボリックリンク）の固定注入 |
| **インスタンス管理・起動/停止** | [internal/infrastructure/mysql/server.go](../internal/infrastructure/mysql/server.go), [upgrade.go](../internal/infrastructure/mysql/upgrade.go), [internal/infrastructure/postgres/server.go](../internal/infrastructure/postgres/server.go), [upgrade.go](../internal/infrastructure/postgres/upgrade.go), [internal/app/app.go](../internal/app/app.go#L142-L166) | `my.cnf` / `env` 生成、systemd テンプレートユニット（`db-branch-mysqld@.service`, `db-branch-postgres@.service`）、`mysqladmin ping` / `pg_isready`、`innodb_fast_shutdown=0` / `SIGINT` + `pg_controldata`、`mysqlcheck` / `vacuumdb` |
| **サービス ID 同期** | [internal/infrastructure/svcids/svcids.go](../internal/infrastructure/svcids/svcids.go#L107-L108) | UID/GID 変更時の停止対象に `db-branch-mysqld@*.service`, `db-branch-postgres@*.service` を列挙 |
| **ZFS dataset プロパティ** | [internal/usecase/dbbranch/service.go](../internal/usecase/dbbranch/service.go#L264-L281), [base.go](../internal/usecase/dbbranch/base.go#L169) | `Init()` が `<pool>/db-branch` に `recordsize=16K`（InnoDB 向け）等を設定し、ベース作成時は `props=nil` で親から継承 |
| **クエリ実行（API / MCP）** | [internal/infrastructure/sqlquery/sqlquery.go](../internal/infrastructure/sqlquery/sqlquery.go#L49-L66) | `go-sql-driver/mysql` と `pgx` の `switch` 固定 |
| **ProxySQL ルーティング** | [internal/usecase/routing/routing.go](../internal/usecase/routing/routing.go#L63-L97), [internal/infrastructure/proxysql/proxysql.go](../internal/infrastructure/proxysql/proxysql.go#L187-L246) | `RouteConfig` が `MySQL` / `Postgres` の 2 フィールド固定 |
| **HTTP API / Capabilities** | [internal/interface/httpapi/misc.go](../internal/interface/httpapi/misc.go#L47-L50), [record.go](../internal/interface/httpapi/record.go#L52-L58) | `availableRuntimes` が MySQL+Postgres 固定、`recordAvailable` が `domain.MySQL` を決め打ちでプローブ |
| **Web コンソール** | `web/src/components/newbranch.tsx#L70`, `web/src/components/basepicker.tsx#L9-L10`, `web/src/components/connect.tsx#L99`, `web/src/lib/bases.ts#L16,22`, `web/src/styles.css#L20-L21`, `web/src/demo/state.ts`, `web/src/demo/backend.ts` | `mysql` / `postgres` の文字列判定・色・接続ヒント・Record 表示条件がハードコード |

### 2.3 runtime 依存機能の現状実装と課題

1. **History モジュール（`ServerOptioner`, `RestoreFinisher`, `Host.Branches`）**:
   - [docs/modules.md](modules.md#L73-L90) および [pkg/ext/ext.go](../pkg/ext/ext.go#L293-L476) により `history` モジュールは抽象化されているが、実態として `ServerOptions.Config` は MySQL の `my.cnf` `[mysqld]` にのみ適用され、PostgreSQL は無視している（[postgres/server.go](../internal/infrastructure/postgres/server.go#L70-L94)）。
   - `RestoreFinisher` は `Maintenance: true`（`skip-networking`）を前提とするが、PostgreSQL 実装は `s.Maintenance` を参照せず TCP ポートで待ち受けてしまう（[docs/branch-merge.md](branch-merge.md#L98) でも指摘済み）。
2. **Branch Merge（[docs/branch-merge.md](branch-merge.md)）**:
   - 同一ブランチ内の退避ラインとライブ間の 3-way 論理マージ仕様。一時クローン 3 台を `Maintenance: true` で起動し、`BASE TABLE` 抽出、PK 昇順ストリーミング比較、`AUTO_INCREMENT` / `SEQUENCE` 同期、FK 孤児検証、トリガ抑止を行う設計であり、SQL 方言・システムカタログに強く依存する。
3. **Masking / Diff / MCP / Monitoring**:
   - `Masker` は `DBAccess` を受け取るが、対象エンジンとの整合チェックがマスク実行時まで遅延している。
   - MCP の `query`・`check_upgrade`・`upgrade_branch`・（将来の）`merge_line` は全エンジン共通で露出しており、非対応エンジンに対する事前抑制の仕組みがない。

---

## 3. Runtime が満たすべき標準インターフェース（契約）

コアが DB エンジンの具体知識を持たずに全ライフサイクルを統制できるよう、新たに **`RuntimeProvider`**（プロセス境界では gRPC サービス、コア内部では `domain.RuntimeProvider` ポート）を定義する。

### 3.1 エンジン識別・インスタンス仕様・メタデータ

```go
// EngineSpec はモジュールが提供するエンジンの静的能力と要件を宣言する。
type EngineSpec struct {
    Engine         string            // `^[a-z][a-z0-9-]{0,30}$` (例: "mysql", "postgres", "clickhouse")
    DisplayName    string            // コンソール表示名 (例: "ClickHouse")
    VersionPattern string            // 受理するバージョン正規表現 (空なら標準パターン)
    CollationBound bool              // true なら OS/glibc ロケール一致ノードのみに複製・配置 (PostgreSQL 等)
    RunAsUser      string            // 実行ユーザー (外部モジュールは "db-branch-rt-" + Engine に強制検証)
    ZFSProps       map[string]string // ベース・ブランチ作成時の ZFS プロパティ推奨値 (例: recordsize, compression)
    Protocols      []ProtocolPort    // 必要な TCP ポート群 (Primary: true が 1 つ必須)
    Capabilities   RuntimeCaps       // サポート機能の宣言 (§4.4)
}

// InstanceSpec はコアからモジュールへ渡される 1 インスタンスの実行仕様。
type InstanceSpec struct {
    ID          string            // 一意なインスタンス ID (例: "base-prod", "branch-x")
    Runtime     RuntimeRef        // engine@version
    RuntimeDir  string            // 展開済みランタイムディレクトリ (<DataDir>/runtimes/<engine>/<version>)
    DataDir     string            // ZFS dataset のマウントポイント (永続データ唯一の配置先)
    StateDir    string            // インスタンス固有の設定・ログ格納先 (<CoreDataDir>/instances/<id>)
    RunDir      string            // ソケット・PID ファイル格納先 (/run/db-branch-rt/<engine>/<id>)
    Bind        string            // リッスンアドレス (Maintenance 時は強制ローカル)
    Ports       map[string]int    // Protocol 名 -> 割り当て済み TCP ポート
    Limits      ResourceLimits    // MemoryMB, MaxConnections, NoFile, CPUQuota
    Maintenance bool              // true: 外部ネットワーク遮断・ローカル管理のみ許可
    LegacyAuth  bool              // エンジン固有の互換フラグ等
    Extra       []ServerOption    // ServerOptioner 等から注入された追加設定
}
```

- **ハードコード撤廃**: コアは `domain.MySQL` / `domain.Postgres` の固定許可を廃止し、登録済み `EngineSpec` に存在するエンジン名のみを `ParseRuntimeRef` で受理する。

### 3.2 ライフサイクル契約（systemd 権限分離との両立）

非特権ユーザーで動作するサードパーティモジュールと、root 権限で systemd / ZFS を統制するコアとの責務境界が矛盾しないよう、起動・停止を **「モジュールによる準備/検証」と「コアによる `systemctl` 実行」** に明確に分離する。

```go
type RuntimeProvider interface {
    Describe(ctx context.Context) (EngineSpec, error)
    Catalog(ctx context.Context) ([]CatalogEntry, error) // 取得可能バージョン・プラットフォーム別 SHA-256

    // ランタイムバイナリ展開後処理 (アーカイブ取得・検証・展開はコアの ArtifactCache/RuntimeStore が実施)
    PrepareRuntime(ctx context.Context, version, stagingDir string) error

    // インスタンス準備・起動・停止・状態確認 (コアが事前に DataDir/StateDir/RunDir を RunAsUser へ chown する)
    Initialize(ctx context.Context, s InstanceSpec) error
    PrepareClone(ctx context.Context, s InstanceSpec) error
    PrepareStart(ctx context.Context, s InstanceSpec) (LaunchPlan, error)
    Health(ctx context.Context, s InstanceSpec) (HealthState, error)
    Quiesce(ctx context.Context, s InstanceSpec, mode StopMode) error
    VerifyCleanShutdown(ctx context.Context, s InstanceSpec) error
    RemoveInstance(ctx context.Context, s InstanceSpec) error

    // ユーザー・接続情報・クエリ・ログ
    CreateUser(ctx context.Context, s InstanceSpec, user, password string) error // 非スーパーユーザー（OS ファイル/プログラム実行不可）かつ冪等に権限を是正
    ExecAs(ctx context.Context, s InstanceSpec, user, password, sql string) error // init_sql 用: ブランチログイン権限で実行し、クライアントコマンド（\!, system 等）を無効化
    AdminAccess(ctx context.Context, s InstanceSpec) (domain.AdminAccess, error)
    FormatDSN(ctx context.Context, s InstanceSpec, host, user, password string) (string, error)
    Query(ctx context.Context, s InstanceSpec, q QueryRequest) (QueryResult, error)
    TailLog(ctx context.Context, s InstanceSpec, lines int) (string, error)

    // オプショナル契約 (Capabilities が true の場合のみ実装)
    PreFreeze(ctx context.Context, s InstanceSpec) (FreezeToken, error)
    PostFreeze(ctx context.Context, s InstanceSpec, tok FreezeToken) error
    UpdateStats(ctx context.Context, s InstanceSpec) error
    CheckUpgradePath(ctx context.Context, fromVersion, toVersion string) error
    CheckUpgrade(ctx context.Context, s InstanceSpec, toVersion string, own Login) (UpgradePlan, error)
    StartUpgraded(ctx context.Context, s InstanceSpec, own Login) (LaunchPlan, UpgradeLog, error)
    DumpSource(ctx context.Context, job SourceDumpJob, outPath string) error
    Load(ctx context.Context, s InstanceSpec, dumpPath string) error
    InstanceMetrics(ctx context.Context, s InstanceSpec) ([]MetricSample, error)
}
```

| ステップ | 実行主体 | 契約（満たすべき不変条件） |
|---|---|---|
| **事前所有権設定** | **コア (root)** | `Initialize`, `PrepareClone`, `PrepareStart` の前に、`DataDir`, `StateDir`, `RunDir` を当該エンジンの `RunAsUser` へ `chown` する（モジュールは `chown` を行わない）。 |
| `PrepareRuntime` | モジュール | コアがダウンロード・SHA-256 検証・展開（`tar`/`dpkg-deb`）した `stagingDir` に対し、ライブラリ互換リンク等の Fixup と実行可能性検証を行う。 |
| `Initialize` | モジュール | `DataDir` 内に空のデータベースクラスタを初期化し、ローカル管理者認証を設定する。非特権 `RunAsUser` で動作すること。 |
| `PrepareClone` | モジュール | クローン元から引き継いだ固有ファイル（PID、サーバー UUID、ロック、事前生成設定等）を除去・再生成する。 |
| `PrepareStart` → `systemctl start` → `Health` | モジュール → **コア (root)** → モジュール | モジュールが `StateDir` に設定ファイルを書き出し、`LaunchPlan`（実行コマンド、引数、環境変数、systemd `Type`/`KillSignal`/`TimeoutStopSec` 指定）を返す。コアは改行・systemd ディレクティブインジェクションを拒否した上で `/run/systemd/system/db-branch-rt@<id>.service.d/override.conf`（`InaccessiblePaths=/run/db-branch /etc/db-branch` を含む）と `env` を書いて `systemctl start` を実行し、`Health == Ready` までポーリングする。 |
| `Quiesce` → `systemctl stop` → `VerifyCleanShutdown` | モジュール → **コア (root)** → モジュール | `StopClean` 時: モジュールがメモリ上のダーティデータ・WAL/ログをフラッシュ（例: `innodb_fast_shutdown=0`、`SYSTEM STOP MERGES` + `SYSTEM FLUSH LOGS`）→ コアが `systemctl stop` 実行 → モジュールが `VerifyCleanShutdown` でクリーン停止完了を検証（失敗時は `ErrUncleanShutdown`）。<br>`StopImmediate` 時: コアが `systemctl kill -s SIGKILL` + `stop` + `reset-failed` を実行。 |
| `RemoveInstance` | モジュール＋コア | モジュールがインスタンス設定・ログを整理し、コアが `StateDir`・`RunDir`・systemd drop-in と ZFS dataset を破棄する。 |

### 3.3 データディレクトリと ZFS dataset 配置契約

ZFS のスナップショット・クローン・ロールバック・`zfs send/recv` が破綻しないよう、配置境界を厳格に分離する。

```text
<pool>/db-branch/
├── bases/<name>              -> DataDir (ZFS dataset; コアがマウント・スナップショット管理)
├── replicas/<name>           -> レプリカ受信先 (ZFS dataset; base replicate で配置)
├── branches/<name>           -> DataDir (ZFS clone; ライブ稼働中)
├── history/<name>/<line>     -> 退避ライン (ZFS; マウントしない)
└── work/<op_id>              -> 一時クローン作業領域 (ZFS)

/var/lib/db-branch/           -> コアの StateDir（ZFS ブランチデータセット外）
├── runtimes/<engine>/<ver>/  -> 展開済みランタイムバイナリ
├── instances/<id>/           -> インスタンス別の設定ファイル・環境変数・エラーログ
└── modules/<name>/           -> モジュール固有のローカルデータ

/run/db-branch-rt/<engine>/<id>/ -> RunDir（ソケット・PID ファイル）
```

- **単一 DataDir 強制（必須要件）**: runtime モジュールは、データベースの永続データ・テーブルスペース・WAL/redo ログ・パーツ・スキーマメタデータ・アクセス制御データの**すべてを `InstanceSpec.DataDir` 配下に配置**しなければならない。`/var/lib/<engine>` 等への書き出しは厳禁（ブランチ間で衝突し、スナップショットに含まれないため）。
- **一時ファイル・ログの分離**: ソケット・PID・エラーログは `RunDir` または `StateDir` に置き、ZFS スナップショット・クローンに含めない。
- **ZFS 操作の一元所有とレプリケーション時のプロパティ維持**:
  - モジュール自身は `zfs` コマンドを一切実行しない。
  - `<pool>/db-branch` には既定の `DatasetProperties`（`recordsize=16K` 等、[service.go](../internal/usecase/dbbranch/service.go#L266-L271)）が設定されているため、ベース作成時だけでなく **`base replicate`（`zfs send/recv`）による受信側データセットにも、`EngineSpec.ZFSProps`（例: ClickHouse 向け `recordsize=128K`）を明示適用**（`zfs recv -o ...` または受信後 `zfs set`）し、親データセットからの `16K` 継承による性能劣化を防ぐ。

### 3.4 整合的なスナップショットの取り方（Quiesce / Flush / Freeze フック）

1. **Clean Snapshot（Base 作成・Snapshot・Upgrade 前・Merge 前）**:
   - コアが任意で `UpdateStats`（`Capabilities.UpdateStats == true` 時）を呼んだ後、`Quiesce(StopClean)` → `systemctl stop` → `VerifyCleanShutdown` → `zfs snapshot` を実行する。
2. **Live Snapshot（稼働中 `branch checkpoint`）の `PreFreeze` / `PostFreeze` フック**:
   - 原則サーバー無停止で `zfs snapshot` を取得する。補助的なフラッシュ/凍結が必要なエンジンのみ `PreFreeze` / `PostFreeze` を実装する。
   - **クラッシュ・タイムアウト時の安全策（`STOP MERGES` 放置防止）**:
     - `PreFreeze` は原則として**タイムアウト上限（例: 5 秒）付きの冪等なフラッシュ**（例: ClickHouse の `SYSTEM FLUSH LOGS`）に留めることを推奨する。
     - もし `PreFreeze` 内でインスタンスのバックグラウンド処理停止（例: `SYSTEM STOP MERGES`）等の状態変更を行う場合は、**① コアが `PreFreeze` 直前に ZFS 意図プロパティ `db-branch:freeze=1` を書き込み、`PostFreeze` 成功後に消去する**、**② `PreFreeze` 失敗時・エラー時も `PostFreeze` をベストエフォートで必ず呼び出す**、**③ エージェント起動時の `Recover` で `db-branch:freeze` が残っていれば `PostFreeze` を実行して解除する**ことを義務付ける。

### 3.5 ポート・接続情報・認証・DSN 契約

- **複数ポートへの対応**:
  - `EngineSpec.Protocols` で必要ポートを宣言し（例: ClickHouse は `[{Name: "http", Primary: true}, {Name: "native"}]`）、コアは `ports.min`〜`ports.max` から必要数を割り当てて `InstanceSpec.Ports` で渡す。`Primary: true` のポートが `Branch.Port` 互換の代表ポートとなる。
- **`AdminAccess` の汎用化と `pkg/ext.DBAccess` 連携**:
  - `domain.AdminAccess`（[internal/domain/mask.go](../internal/domain/mask.go#L7-L20)）および `ext.DBAccess`（[pkg/ext/ext.go](../pkg/ext/ext.go#L558-L571)）に `Endpoints map[string]Endpoint`（プロトコル別 `Host`, `Port`, `Socket`）と `Env map[string]string` を後方互換で追加し、`RuntimeProvider.AdminAccess()` から生成する。これにより `Masker` や `history` モジュールが任意のエンジンのクライアント接続情報を受け取れる。
- **ユーザー作成・`init_sql` 実行と DSN**:
  - `CreateUser(ctx, spec, user, password)`、`ExecAs(ctx, spec, user, password, sql)`、`FormatDSN(ctx, spec, host, user, password)` をモジュール契約とし、`Branch.DSN` の `if engine == Postgres` ハードコードを廃止する。
  - `CreateUser` はデータ・オブジェクトの読み書き・変更のみを許可する非スーパーユーザー（OS ファイル読み書き・プログラム実行不可。[internal/domain/branch.go](../internal/domain/branch.go#L420-L424) 準拠）を冪等に作成・再設定する契約とし、コアは起動・リセット・復元・エージェント復旧（`putLoginRight` / `hardenRunning`）時にも呼び出す。
  - `init_sql` は管理者権限ではなく `ExecAs` によりブランチログイン権限で実行し、クライアントコマンド（`psql` の `\!`、`mysql` の `system` 等）がノード上で実行されないよう無効化する（[internal/domain/branch.go](../internal/domain/branch.go#L416-L419), [internal/usecase/dbbranch/branch.go](../internal/usecase/dbbranch/branch.go#L194-L197) 準拠）。

### 3.6 バージョン管理・並存・Upgrade 契約

- **並存配置**: `<CoreDataDir>/runtimes/<engine>/<version>/` にバージョン単位で配置し、同一ノード上で複数バージョンが並存する。
- **Upgrade 契約（[docs/upgrade.md](upgrade.md) 準拠）**:
  - `CheckUpgradePath(ctx, fromVersion, toVersion) error`: シリーズ飛び越し禁止やメジャー更新不可を判定。
  - `CheckUpgrade(ctx, spec, toVersion, ownLogin) (UpgradePlan, error)`: `--check-only` 検証と設定妥当性チェック。
  - `StartUpgraded(ctx, spec, ownLogin) (LaunchPlan, UpgradeLog, error)`: 新バージョンでの起動プラン生成とログ警告抽出。失敗時はコアが `before-upgrade-*` ZFS チェックポイント復元で自動ロールバックする。

### 3.7 Base 同期とリソース制限

- **Base 同期（レプリケーション / Refresh）**:
  - `zfs send/recv` 自体はエンジン非依存。`EngineSpec.CollationBound` により OS/Distro 制約を判定し、受信時に `EngineSpec.ZFSProps` を維持する。
  - 定期リフレッシュ（`base refresh`）は `Capabilities.SourceDump == true` の場合のみ `DumpSource` と `Load` を呼ぶ。
- **リソース制限**:
  - コアは `InstanceSpec.Limits`（`MemoryMB`, `NoFile`, `CPUQuota`）を渡し、モジュールはエンジン設定（`innodb_buffer_pool_size`, `shared_buffers`, `max_server_memory_usage` 等）へ反映する。
  - 同時にコア側の systemd drop-in で `Slice=db-branch.slice`・`LimitNOFILE`・`MemoryMax`（フェイルセーフ上限）を設定し、設定漏れによるノード OOM を防止する。

### 3.8 クエリ実行・スキーマ Introspection・ログ・メトリクス

- `Query(ctx, spec, req) (QueryResult, error)`: API / MCP の `query` 用。ドライバ非依存化。
- `IntrospectSchema(ctx, spec) (SchemaInfo, error)`（任意）: テーブル・列・型・PK・FK・ユニーク制約を標準形式で返却。
- `TailLog(ctx, spec, lines)` / `InstanceMetrics(ctx, spec)`: エラー抜粋とインスタンスメトリクス収集。

---

## 4. runtime 依存機能の標準化と Capability 設計

### 4.1 「コア共通部分」と「runtime モジュール Adapter 部分」の責務分離

| 機能領域 | コアが持つ共通部分 | モジュール側 Adapter 部分 | Capability キー |
|---|---|---|---|
| **History（変更履歴・時点復元）** | 作成時 `Record` 固定判定、`CheckpointGraph` 管理、ZFS `OpenLine`/`CloseLine`、`Maintenance` 起動、`AfterRestore` 呼び出し | 変更ログ有効化（`ServerOptions`）、ログ解析・再生（`AfterRestore`）。PITR 不可エンジンは `false` | `history_record` |
| **マスキング（Base Mask）** | `base mask` 制御、一時ソース起動、ターゲットクリーン停止・スナップショット・失敗時ロールバック、`MaskInfo` 記録 | 各エンジン向け `Masker` モジュールが `SupportedEngines` を宣言し、対象エンジンを事前検証 | `masking`（Masker 側宣言） |
| **Checkpoint 整合性** | ZFS スナップショット・リネーム・`promote`・ライン木・GC・意図プロパティ復旧 | `Quiesce` / `VerifyCleanShutdown`、`PreFreeze` / `PostFreeze`、`PrepareClone` | `checkpoints` / `live_freeze` |
| **Diff / Merge（[branch-merge.md](branch-merge.md)）** | LCA 解決、`BranchMerging` 排他、一時クローン 3 台の確保・ZFS GC、3-way 比較エンジン、自動チェックポイント `before-merge-*` とロールバック | `LogicalMergeAdapter`: `BASE TABLE`、PK 昇順カーソル、DDL 適用、FK 孤児検証、`AUTO_INCREMENT`/シーケンス同期、トリガ抑止 | `branch_merge` / `diff` |
| **監視（Monitoring）** | ノード死活、Raft リーダー、ZFS プール、`dbbranch_branches`（`engine` ラベル追加）、`dbbranch_module_problem` | `Health` 判定、任意でインスタンスメトリクス返却 | `instance_metrics` |
| **MCP（[mcp.md](mcp.md)）** | `/v1/mcp`、トークン認可・監査、共通ツール（`list_*`, `create_branch`, `reset_branch`, `delete_branch`, `create_checkpoint`, `restore_checkpoint`） | `query`, `check_upgrade` / `upgrade_branch`, `merge_line` 実行時にブランチのエンジンの Capability を検証 | `query`, `upgrade`, `branch_merge` |
| **Web コンソール** | 共通レイアウト、ZFS グラフ、設定画面、操作進捗表示 | `/v1/capabilities`（エンジン別マップ）に基づき、バッジ色・接続ガイド・Record/Upgrade/Merge/Query UI を動的制御 | 全 Capability 参照 |
| **Proxy（Router）** | ホストグループ採番、`RouteUser` 管理、コメントルール生成、`Routing` 同期ループ | `Router` モジュールが `SupportedEngines` を宣言し、`RouteConfig.Routes[engine]` を適用 | `proxy_routing`（Router 側宣言） |

### 4.2 `branch merge`（[docs/branch-merge.md](branch-merge.md)）との整合

- [docs/branch-merge.md](branch-merge.md) の「LCA 探索・`BranchMerging` 排他・一時クローン 3 台・`StopClean`・`before-merge-*` 自動ロールバック・`Recover`」はコアの共通基盤として維持する。
- MySQL / PostgreSQL 固有の SQL 比較・適用・FK/トリガ制御部分を `LogicalMergeAdapter` として切り出し、`Capabilities.BranchMerge == false` のエンジンに対しては受付時に新規エラーコード `409 feature_not_supported`（既存の機能未実装 `501 not_implemented` とは区別し、対象エンジンの構成非対応を示す）を返し、Web コンソールの「Merge into live」ボタンも無効化する。
- ClickHouse は MergeTree の PK が一意制約ではなくソートキーであり、`UPDATE`/`DELETE` が非同期ミューテーション・FK 無しのため、初期は `branch_merge: false` として安全に拒否する。

### 4.3 `history` / `masking` / `Proxy` との整合

- **`history` モジュールと `ServerOptioner`**:
  - [internal/interface/httpapi/record.go](../internal/interface/httpapi/record.go#L56) の `domain.MySQL` 決め打ちプローブを廃止し、選択ベースの `Engine` が `history_record: true` かつ `history` モジュールが受理する場合のみ有効化する。
  - `ServerOptioner` が返す `ServerOptions.Config`（`[]string`）は `InstanceSpec.Extra` として `PrepareStart` へ渡す。モジュールが解釈できないオプションが含まれる場合（および `Record: true` のブランチで設定適用に失敗した場合）は明確なエラーを返し、`Record: true` ブランチの無記録起動を禁止する（[docs/modules.md](modules.md#L86) 準拠）。
  - **`OpenLine` の読み取り権限**: `OpenLine` でマウントされたスナップショットを別ユーザーで動く `history` / `diff` モジュールが読めるよう、コアは読み取り専用グループ（例: `db-branch-ro`）またはコア経由の読み取りストリームでアクセス境界を担保する。
- **`Router`（`db-branch-proxysql` 等）の後方互換**:
  - `ext.RouteConfig` に `Routes map[string]RouteTables` を新設し（`MySQL`/`Postgres` フィールドは後方互換のため維持）、`Router` モジュールに `SupportedEngines []string` を追加する。未宣言の既存 `Router` モジュール（現行 `db-branch-proxysql` 等）については `ext.Module.Ports` のマップキーを対応エンジンとして扱う。

### 4.4 Capability 宣言方式の比較と推奨案

| 方式 | メリット | デメリット | 評価 |
|---|---|---|---|
| **案 A: 静的マニフェスト宣言のみ** | 起動前から能力が分かり、UI/API 検証が高速・決定的 | ライセンス切れ等の動的無効化を表現できない | △ |
| **案 B: 動的プローブのみ** | 実行時状態を反映できる | RPC 遅延・障害点が増え、`recordAvailable` のような決め打ちバグの温床になる | × |
| **案 C（推奨）: 静的マニフェスト + 動的状態のハイブリッド** | エンジンの構造的能力（`branch_merge`, `upgrade`, `source_dump` 等）はマニフェスト＋`Describe()` で静的に確定し、外部モジュール依存能力（`history_record`, `proxy_routing`）のみ設定・ライセンス変更時にコアが結合してキャッシュする | マニフェストと実装の一致を適合性テストで検証する必要がある | **◎** |

非対応機能は `GET /v1/capabilities` の `engines.<engine>.features` に明示的に `false` として返却し、暗黙のフォールバックを行わない。

---

## 5. 配布・実行方式とセキュリティ・運用設計

### 5.1 配布・実行方式の比較と推奨案

| 方式 | 概要 | メリット | デメリット / リスク | 評価 |
|---|---|---|---|---|
| **(a) Go plugin (`.so`)** | `plugin.Open` で動的リンク | 同一プロセス内で最速 | Linux 限定。Go 版・依存版・ビルドタグ完全一致が必須。unload 不可。パニック波及。他言語不可 | **×（不採用）** |
| **(b) 常駐別プロセス + gRPC over `go-plugin`** | `hashicorp/go-plugin` 上で **gRPC (Protobuf)** を採用 | [docs/modules.md](modules.md#L59) の既定方針に合致。Protobuf による後方互換、他言語実装可、クラッシュ隔離、ストリーム対応 | Protobuf 生成が必要。OS 権限分離は別途プロセス起動側で制御が必要 | **◎（制御プレーン推奨）** |
| **(b') 別プロセス + JSON-RPC over stdio** | 独自 JSON-RPC | Protobuf 不要で軽量 | `go-plugin` のヘルスチェック・多重化・TLS を再実装する必要がある | △ |
| **(c) コンテナ / OCI** | Docker / containerd | 強力な隔離 | ZFS マウント伝播が複雑。起動遅延で数秒ブランチ作成を損なう。APT 単体導入要件に反する | × |
| **(d) 単発外部バイナリ + マニフェスト** | 操作ごとに CLI 起動 | プロセス常駐不要 | 頻繁な `Health` / `Query` ごとに起動コストが発生 | △ |

#### 推奨アーキテクチャ：ハイブリッド分離モデル

1. **制御プレーン（Runtime Adapter）**: **(b) 常駐別プロセス + gRPC over `hashicorp/go-plugin`** と **静的マニフェスト（`manifest.json`）**。
2. **データプレーン（DB サーバー本体）**: コア（root）が生成するインスタンス別 drop-in 付き **汎用 systemd テンプレートユニット `db-branch-rt@.service`** 経由で起動する。
   - **動的ユニット設定**: `Type=`（`simple` vs PostgreSQL の `notify`）、`KillSignal=`（`SIGTERM` vs `SIGINT`）、`User=`、`ReadWritePaths=` は `EnvironmentFile` で切り替えられないため、コアが `/run/systemd/system/db-branch-rt@<id>.service.d/override.conf` を生成して `systemctl daemon-reload` → `start` する。
   - **サービス ID 同期との整合**: `internal/infrastructure/svcids/svcids.go#L107-L108` の停止対象に `db-branch-rt@*.service` を追加する。

### 5.2 契約のバージョニングとマニフェスト形式

- **契約の Semver / ProtocolVersion**:
  - `go-plugin` の `HandshakeConfig.ProtocolVersion` を破壊的変更時にインクリメントし、`VersionedPlugins` で過渡期の複数バージョン並存を許容する。モジュール自身は Semver と `api_version` を持つ。
- **マニフェスト形式（`/usr/lib/db-branch/runtimes/<engine>/manifest.json`）**:

```json
{
  "schema_version": 1,
  "engine": "clickhouse",
  "display_name": "ClickHouse",
  "module_version": "0.1.0",
  "api_version": 1,
  "binary": "/usr/lib/db-branch/runtimes/clickhouse/db-branch-rt-clickhouse",
  "catalog": "/usr/lib/db-branch/runtimes/clickhouse/runtimes.json",
  "run_as_user": "db-branch-rt-clickhouse",
  "collation_bound": false,
  "zfs_props": { "recordsize": "128K", "compression": "lz4" },
  "protocols": [
    { "name": "http", "primary": true },
    { "name": "native", "primary": false }
  ],
  "capabilities": {
    "query": true,
    "checkpoints": true,
    "live_freeze": true,
    "upgrade": true,
    "source_dump": false,
    "branch_merge": false,
    "history_record": false,
    "update_stats": false,
    "instance_metrics": true
  },
  "systemd": {
    "service_type": "simple",
    "kill_signal": "SIGTERM",
    "timeout_stop_sec": 300
  }
}
```

### 5.3 署名・検証・信頼モデル（サードパーティ対応の最重要点）

現状の `db-branch.service` は `root` 権限で動作し（[packaging/db-branch.service](../packaging/db-branch.service#L9-L10)）、既存モジュールも `root` のまま起動されている（[pkg/ext/plugin/plugin.go](../pkg/ext/plugin/plugin.go#L657)）。`v0.2.6` / `v0.2.7` により、管理ソケット（`/run/db-branch/agent.sock`, [internal/app/agent_http.go](../internal/app/agent_http.go#L22-L50)）と設定ファイル（`/etc/db-branch/config.toml`, [packaging/scripts/postinst](../packaging/scripts/postinst#L14-L21)）のアクセス権は `db-branch-admin` グループへ分離され、既存 DB ユニットには `InaccessiblePaths=/run/db-branch /etc/db-branch`（[db-branch-mysqld@.service](../packaging/db-branch-mysqld@.service#L19), [db-branch-postgres@.service](../packaging/db-branch-postgres@.service#L21)）が導入され、DB ログインも非スーパーユーザー化（`NOSUPERUSER` + `OwnerRole` / `clientPrivileges` + `partial_revokes`）および `init_sql` の `ExecAs` 実行へ強化された。しかし、既存 MySQL/PostgreSQL のプロセス・`DataDir`・ローカルソケットは引き続き `db-branch` ユーザー所有であり、ローカル管理者接続がパスワードレス許可されている（[postgres/server.go](../internal/infrastructure/postgres/server.go#L117-L124)）。**そのため、サードパーティ製モジュールを `root` はもちろん、既存の `db-branch` ユーザーで動かすことも、他 DB のデータ窃取・改ざんを許すため危険である。**

1. **ユーザー分離と最小権限サンドボックス**:
   - サードパーティ（および外部）runtime モジュールと DB インスタンスは、既存 `db-branch` ユーザーおよび `db-branch-admin` グループとは分離した**エンジン専用非特権ユーザー**（`db-branch-rt-<engine>`、APT パッケージ `postinst` で `--system` 作成、またはコアが動的に割り当てる専用 UID）で実行する。
   - **`run_as_user` の厳格検証（特権・他ユーザーなりすまし防止）**: コアは外部 runtime モジュールの `manifest.json` ロード時および `Describe()` 検証時に、`run_as_user` が空または `db-branch-rt-<engine>` と一致すること（かつ OS 上の UID が `0` でないこと、`db-branch` や他エンジンのユーザーでないこと）を検証し、`"root"` や `"db-branch"` 等を指定した不正なマニフェストは `allow_unsigned_runtimes = true` 環境であっても無条件にロードを拒否する。
   - ZFS 操作・`chown`・`systemctl` はすべてコア（root）が代行する。
   - `db-branch-rt@<id>.service.d/override.conf` には `User=<run_as_user>`, `NoNewPrivileges=yes`, `ProtectSystem=strict`, **`InaccessiblePaths=/run/db-branch /etc/db-branch`** を設定し、`ReadWritePaths=` を**当該インスタンスのパスのみ**（`<DataDir>`, `/var/lib/db-branch/instances/<id>`, `/run/db-branch-rt/<engine>/<id>`）に厳格限定する。
   - **`LaunchPlan` のサニタイズ（systemd ディレクティブインジェクション防止）**: コアが `override.conf` を生成する際、モジュールから受け取ったコマンド・引数・環境変数・`Type`/`KillSignal`/`TimeoutStopSec` に対して改行文字（`\r`, `\n`）・NUL バイト・制御文字を厳格に拒否し、許可リスト以外の systemd ディレクティブを書き出さない。
   - **影響範囲の境界**: エンジン単位のコントロールプレーンプロセス（`db-branch-rt-<engine>`）が侵害された場合でも、`db-branch-admin` グループ分離と `InaccessiblePaths` によりエージェントソケット・設定・他エンジン・`db-branch` 所有データには波及しない（ただし同一エンジン内の他ブランチには影響し得るため、署名検証と併用する）。
2. **配布・署名検証モデル**:
   - **APT 署名済みパッケージ**: APT リポジトリの GPG 署名（[docs/distribution.md](distribution.md)）により検証され、`/usr/lib/db-branch/runtimes/<engine>/` に `root:root` 所有・`0755`（`perm & 0o022 == 0`）で配置されたものを信頼する。
   - **サードパーティ外部配布**: `manifest.json.sig`（Ed25519 / Cosign 等、バイナリとカタログの SHA-256 を含む）を `/etc/db-branch/trusted-keys/` の公開鍵で検証できた場合のみロードする。未署名は既定で拒否し、検証環境向けオプトイン（`config.toml` の `allow_unsigned_runtimes = true`）でのみ明確な警告ログ（および `/v1/capabilities` / コンソールへの警告表示）付きで許可する。
3. **カタログ（上流 DB バイナリ）の検証と SSRF 防止**:
   - マニフェストが指す `catalog`（または `Catalog()` RPC）の全アーティファクトに **SHA-256 ダイジェスト**を必須とし、コアの `ArtifactCache` がダウンロード時に検証を強制する。
   - 外部モジュールが `Catalog()` で返すダウンロード URL に対しては `https://` スキームのみを許可し、ループバック・リンクローカル（`169.254.0.0/16` 等）・プライベート IP アドレスへのリダイレクト／アクセスを拒否して SSRF を防止する。

### 5.4 検出・インストール・更新・削除 UX とクラスタ（Raft）整合

- **既存 `503 modules_pending` との分離**:
  - 現行の `ModuleSpec` / `PendingModules`（[internal/domain/module.go](../internal/domain/module.go#L44-L55), [internal/app/agent_modules.go](../internal/app/agent_modules.go#L232-L238)）は `/usr/lib/db-branch/modules/` の `loadedNames` を照合するため、`/usr/lib/db-branch/runtimes/` 配下の runtime モジュールをそのまま `ModuleSpec` に入れると**永久に `503 modules_pending` で API がロックアウトされる**。
  - 対策として、`ModuleSpec` に `Kind`（`"module"` | `"runtime"`）および配布元（`"apt"` | `"local"`）を持たせる（または独立した `RuntimeModules` マップを Raft State に設ける）。`Kind == "runtime"` は `RuntimeRegistry` のロード済みリストと照合し、非 APT 配布（`"local"`）は `apt` 同期ループの対象外としてハートビートで全ノードの一致（バージョン・バイナリダイジェスト）を検証する。
- **ノード障害・未導入時の安全網**:
  - ブランチ保持ノードで起動時に該当 runtime モジュールが未ロードの場合、ブランチを削除・再作成せず `status: "failed"`, `status_reason: "runtime module <engine> not loaded"` として保護する。
- **Raft `Protocol` 予約**:
  - 現行 `Protocol = 18`（[internal/infrastructure/raftstore/fsm.go](../internal/infrastructure/raftstore/fsm.go#L113)）に対し、並行策定中の `branch-fork`（`19`）および `branch-merge`（`20`、[docs/branch-merge.md](branch-merge.md#L28)）との衝突を避けるため、本仕様に伴う Raft コマンド・状態拡張は **`ProtocolRuntimeModules = 21` 以降** として予約し、`opProtocol` でゲートする。

### 5.5 適合性テストスイート（Conformance Test Suite）

サードパーティ作者が契約遵守を自己検証できる CLI テストハーネス **`db-branch-conformance`** を提供する。

- **実行形態**: `db-branch-conformance --manifest /path/to/manifest.json [--zfs-pool tank-test]`
- **検証項目**:
  1. **Handshake & Manifest 整合**: `manifest.json` と gRPC `Describe()` の完全一致。
  2. **ライフサイクルと冪等性**: `Initialize` → `CreateUser` → `PrepareStart` → systemd 起動 → `Health(Ready)` → `Quiesce` → 停止 → `VerifyCleanShutdown` → `PrepareClone` → 再起動の成功、および冪等性。
  3. **DataDir 自己完結性**: `DataDir` 外への永続ファイル漏洩がないことの検査。
  4. **Crash Consistency**: `SIGKILL` 後のクローン起動とクエリ成功。
  5. **Maintenance 隔離**: `Maintenance: true` 時に外部ネットワーク接続が拒否されること。
  6. **Capability 契約整合**: `false` と宣言した操作が `ErrFeatureNotSupported` を返し、`true` と宣言した操作が成功すること。

---

## 6. 移行計画と ClickHouse 検証ポイント

### 6.1 既存 MySQL / PostgreSQL の移行計画（後方互換維持）

1. **Step 1（コア内部の抽象化）**:
   - `domain.RuntimeRegistry` と `domain.RuntimeProvider` を導入し、既存の `mysql` / `postgres` を **組み込み（built-in）プロバイダ** としてインプロセスで登録する。
   - `sqlquery`, `srcdump`, `Branch.DSN`, `CheckUpgradePath`, `CollationBound`, `availableRuntimes`, `recordAvailable` の分岐をすべて `RuntimeProvider` / `EngineSpec` 呼び出しへ置き換える。
2. **Step 2（gRPC 境界と Capability 駆動 API/MCP/UI）**:
   - `pkg/ext/proto/runtime/v1` と `pkg/ext/plugin` の gRPC 対応を追加し、外部 runtime モジュールをロード可能にする。
   - `GET /v1/capabilities` にエンジン別マップを追加し、Web コンソール・MCP を Capability 駆動へ移行する。
3. **Step 3（権限分離・署名検証・Conformance Test）**:
   - エンジン別非特権ユーザー実行・systemd drop-in・署名検証・`db-branch-conformance` を実装する。既存の `mysql` / `postgres` は `apt install db-branch` 単体導入の利便性を維持するため built-in として残しつつ、同一の `RuntimeProvider` 契約で統一する。

### 6.2 ClickHouse を最初のサードパーティ相当モジュールとして載せる場合の検証ポイント

1. **大容量単一バイナリの配布・転送**: ClickHouse は単一バイナリ（`clickhouse server` / `client`）で共有ライブラリ Fixup はほぼ不要だがサイズが数百 MB あるため、`ArtifactCache` とリーダーからのピア転送のタイムアウト・ディスク容量を検証する。
2. **DataDir 完全閉じ込め**: `config.xml` の `<path>`, `<tmp_path>`, `<user_files_path>`, `<format_schema_path>`, `<access_control_path>` をすべて `InstanceSpec.DataDir` 配下に強制し、`/var/lib/clickhouse` への漏洩を防ぐ。
3. **クローン時の固有 ID 除去（`PrepareClone`）**: `<DataDir>/uuid` および `preprocessed_configs/` を除去・再生成し、クローン間でのサーバー UUID 衝突や設定キャッシュ残留を防ぐ。
4. **複数ポートと Maintenance 隔離**: `http` (`8123` 相当) と `native` (`9000` 相当) の 2 ポートを `InstanceSpec.Ports` から割り当て、`Maintenance: true` 時は `<listen_host>127.0.0.1</listen_host>` に限定して外部接続を遮断する。
5. **メモリ上限の強制（OOM 防止）**: ClickHouse の `<max_server_memory_usage>` 既定値 `0`（物理メモリの 90% 自動確保）による複数ブランチ同時起動時の OOM を防ぐため、`Limits.MemoryMB` を `<max_server_memory_usage>` および `<mark_cache_size>` に必ず反映する。
6. **ZFS `recordsize` の適合**: `EngineSpec.ZFSProps` で `recordsize=128K` を指定し、親データセット（`16K`）からの継承を抑止（作成時・レプリケーション受信時とも）した上で性能・圧縮率を検証する。
7. **整合的スナップショット（`Quiesce` / `PreFreeze`）**:
   - `StopClean`: `SYSTEM STOP MERGES` → `SYSTEM FLUSH LOGS` → `systemctl stop`（`SIGTERM` グレースフル終了）を検証する。
   - `PreFreeze`（ライブチェックポイント）: 原則 `SYSTEM FLUSH LOGS` のみを実行し、`SYSTEM STOP MERGES` を併用する場合は §3.4 の `db-branch:freeze` + `Recover` 解除の安全性を実機検証する。
8. **Capability 割り切り**: `query`, `checkpoints`, `upgrade`, `instance_metrics` を `true` とし、`history_record`, `branch_merge`, `proxy_routing`, `source_dump` は `false` として安全に無効化する。

---

## 7. 結論：推奨アーキテクチャ・リスク・実装 TODO

### 7.1 推奨アーキテクチャ

- **エンジン配置方針（決定済み）**: 既存の MySQL / PostgreSQL はコアに **built-in** として維持しつつ `RuntimeProvider` インターフェースに統一し、その他の DB エンジン（ClickHouse 等）は外部モジュールとして差し込める構成とする。
- **制御プレーン**: `hashicorp/go-plugin` 上の **gRPC (Protobuf) 常駐アダプタ** ＋ **静的 `manifest.json`**。
- **データプレーン**: コア（root）がインスタンス別 drop-in（`override.conf`）で制御する **汎用 systemd テンプレート `db-branch-rt@.service`**。
- **信頼境界**: エンジン別非特権ユーザー実行、インスタンス単位の `ReadWritePaths` 制限、APT GPG 署名または公開鍵署名検証、`PendingModules` と分離された Raft 整合。

### 7.2 リスクと緩和策

| リスク | 影響 | 緩和策 |
|---|---|---|
| サードパーティモジュールによる他 DB 侵入・ZFS 破壊 | データ漏洩・プール破壊 | エンジン別非特権ユーザー分離、`ReadWritePaths` 限定、ZFS のコア一元化、署名検証 |
| `DataDir` 外へのファイル書き出し | クローン・復元破綻 | `db-branch-conformance` での独立性テスト義務化 |
| `PreFreeze` 凍結後のエージェントクラッシュ | `STOP MERGES` 放置によるコンパクション停止 | `SYSTEM FLUSH LOGS` 中心の設計、ZFS `db-branch:freeze` 意図記録＋`Recover` 解除 |
| `PendingModules` 誤判定や Raft 番号衝突 | 全ノード API `503` ロックアウト・ログ破損 | `ModuleSpec.Kind` 分離、`ProtocolRuntimeModules = 21` 予約 |

### 7.3 実装用 TODO リスト（t-wada 式 TDD）

実装時は `CLAUDE.md` の原則（TODO リスト → Red → Green → Refactor、テストより先にプロダクションコードを書かない、カバレッジ 95% 以上維持）に従う。

#### 最初に書くべきテスト（Walking Skeleton）

外部依存ゼロの `internal/domain` で、ハードコードされた `MySQL`/`Postgres` を外し、動的に登録されたエンジンを解釈する最小テストから着手する。

- **`TestRuntimeRegistry_登録された任意のエンジンをParseRuntimeRefで受理し未登録エンジンを拒否できる`**
  - `Registry` に `"clickhouse"` を登録した状態で `Parse("clickhouse@25.3.1")` が `RuntimeRef{Engine: "clickhouse", Version: "25.3.1"}` を返し、未登録の `"oracle@19.0"` が `ErrInvalidRuntime` になることを検証する。

#### 分解された実装 TODO リスト

##### Phase 1: `internal/domain` とコア抽象化（インプロセス）
- [ ] **Walking Skeleton**: `domain.RuntimeRegistry` と `EngineSpec`（`Capabilities`, `ZFSProps`, `Protocols`, `CollationBound`, `RunAsUser`）のテストと実装。
- [ ] `Branch.DSN` を `EngineSpec` / `FormatDSN` 委譲へ変更するテストと実装。
- [ ] `domain.RuntimeProvider`（`ExecAs` と非特権 `CreateUser` 契約を含む）・`InstanceSpec`・`LaunchPlan`・`ErrFeatureNotSupported` の定義と単体テスト。
- [ ] `domain.CheckUpgradePath` をエンジン別バリデータ登録方式へ移譲するテストと実装。
- [ ] `mysql` / `postgres` の既存実装を `RuntimeProvider` アダプタとしてラップし、全既存テストがグリーンであることを確認（Refactor）。

##### Phase 2: `internal/usecase` / `internal/infrastructure` の脱ハードコード
- [ ] `dbbranch.Service`: 複数ポート割り当て（`Protocols`）、`EngineSpec.ZFSProps` のベース作成時および `base replicate` 受信時適用テスト。
- [ ] `dbbranch.Service`: `PreFreeze` / `PostFreeze` フック（`db-branch:freeze` 意図記録、`defer PostFreeze`、`Recover` 解除）の `CreateCheckpoint` テスト。
- [ ] `dbbranch.Service`: `RunAsUser` への事前 `chown` と `PrepareStart` → systemd 起動 → `Health` / `Quiesce` → systemd 停止 → `VerifyCleanShutdown`、および `ExecAs`（`init_sql`）・`putLoginRight`（起動・復旧・リセット・復元時の `CreateUser` 再適用）オーケストレーションテスト。
- [ ] `dbbranch.Service`: `CollationBound` と `SourceDump` capability による配置・リフレッシュ制御テスト。
- [ ] `postgres/server.go`: `Maintenance: true` 時の `listen_addresses = ''` 対応テスト（[branch-merge.md](branch-merge.md#L98) 整合）。
- [ ] `sqlquery` / `srcdump`: `RuntimeProvider` 経由のクエリ実行・ダンプ実行への置換テスト。
- [ ] `routing.Plan` / `ext.RouteConfig`: `Routes map[string]RouteTables` 汎用化と `SupportedEngines`（未宣言時は `Ports` キーフォールバック）テスト。
- [ ] `svcids.go#L107-L108`: 停止対象ユニットへの `db-branch-rt@*.service` 追加と専用サービスユーザー考慮テスト。

##### Phase 3: `pkg/ext` gRPC 境界・セキュリティ・API/MCP/UI
- [ ] `pkg/ext/proto/runtime/v1` の Protobuf 定義と `pkg/ext/plugin` の gRPC トランスポート対応テスト。
- [ ] `pkg/ext.DBAccess`: `Endpoints` / `Env` 追加の後方互換テスト。
- [ ] `manifest.json` パーサ（`run_as_user` の `root`/`db-branch`/他エンジン拒否検証含む）・署名検証（Ed25519 / SHA-256）・`allow_unsigned_runtimes` ガード・`Catalog()` URL の SSRF 検証のユニットテスト。
- [ ] エンジン別非特権ユーザー起動（`Credential` 分離）と `/run/systemd/system/db-branch-rt@<id>.service.d/override.conf` 生成テスト（`InaccessiblePaths=/run/db-branch /etc/db-branch` 付与、改行・systemd ディレクティブインジェクション拒否を含む）。
- [ ] `httpapi`: `/v1/capabilities` のエンジン別 capability 出力、`recordAvailable` の `MySQL` プローブ解消、`availableRuntimes` の動的列挙テスト。
- [ ] `mcp`: ツール呼び出し時の capability 検証（非対応時の明示的エラー返却）テスト。
- [ ] `raftstore` / `module`: `ModuleSpec.Kind` 分離（`PendingModules` ロックアウト防止）と `ProtocolRuntimeModules = 21` ゲートテスト。
- [ ] `web/`: Capability 駆動のエンジンバッジ・操作ボタン制御のテスト。

##### Phase 4: Conformance Test と ClickHouse モジュール検証
- [ ] `cmd/db-branch-conformance`: 契約検証テストスイートの実装。
- [ ] ClickHouse runtime モジュールプロトタイプによる Conformance Test 全項目パス確認。

### 7.4 決定事項・未決事項

#### 決定済み
1. **既存 MySQL / PostgreSQL の扱い**: コアに **built-in** として残し（`apt install db-branch` 単体で即利用可能な利便性を維持）、設計上は他エンジンと同じ `RuntimeProvider` インターフェースを実装する。その他の DB エンジン（ClickHouse 等）は外部モジュールとして差し込む。

#### 未決事項（人間に確認したい点）
1. **サードパーティモジュールの署名検証のオプトイン UI**:
   - 未署名バイナリを開発・検証環境で許可する場合、`config.toml` の設定キー（`allow_unsigned_runtimes = true`）と CLI フラグのどちら（または両方）を採用するか？
3. **ClickHouse における Proxy（Router）の扱い**:
   - ClickHouse は `proxy_routing: false`（ダイレクトポート接続のみ）とする方針でよいか？MySQL 互換ポート（`9004`）経由の実験的 ProxySQL ルーティングを視野に入れるか？
4. **Live Checkpoint 時の `PreFreeze` ロック時間許容値**:
   - タイムアウト上限（例: 5 秒）を超過した場合、「フックを諦めてクラッシュコンシステントで撮る」か「チェックポイント自体を失敗させる」か？

---

## 8. 根拠（参照ファイル・公式ドキュメント）

### リポジトリ内ファイル
- モジュール・設計仕様: [docs/modules.md](modules.md), [docs/branch-merge.md](branch-merge.md), [docs/checkpoints.md](checkpoints.md), [docs/upgrade.md](upgrade.md), [docs/masking.md](masking.md), [docs/mcp.md](mcp.md), [docs/monitoring.md](monitoring.md), [docs/distribution.md](distribution.md), [docs/base-refresh.md](base-refresh.md)
- 公開拡張 API: [pkg/ext/ext.go](../pkg/ext/ext.go), [pkg/ext/plugin/plugin.go](../pkg/ext/plugin/plugin.go)
- ドメイン層: [internal/domain/runtime.go](../internal/domain/runtime.go), [branch.go](../internal/domain/branch.go), [module.go](../internal/domain/module.go), [upgrade.go](../internal/domain/upgrade.go), [mask.go](../internal/domain/mask.go), [storage.go](../internal/domain/storage.go), [placement.go](../internal/domain/placement.go), [refresh.go](../internal/domain/refresh.go)
- ユースケース層: [internal/usecase/dbbranch/service.go](../internal/usecase/dbbranch/service.go), [base.go](../internal/usecase/dbbranch/base.go), [distro.go](../internal/usecase/dbbranch/distro.go), [runtime/runtime.go](../internal/usecase/runtime/runtime.go), [module/module.go](../internal/usecase/module/module.go), [routing/routing.go](../internal/usecase/routing/routing.go)
- インフラ層: [internal/infrastructure/mysql/server.go](../internal/infrastructure/mysql/server.go), [upgrade.go](../internal/infrastructure/mysql/upgrade.go), [fixup.go](../internal/infrastructure/mysql/fixup.go), [internal/infrastructure/postgres/server.go](../internal/infrastructure/postgres/server.go), [execas.go](../internal/infrastructure/postgres/execas.go), [upgrade.go](../internal/infrastructure/postgres/upgrade.go), [fixup.go](../internal/infrastructure/postgres/fixup.go), [runtimestore/store.go](../internal/infrastructure/runtimestore/store.go), [catalog/catalog.go](../internal/infrastructure/catalog/catalog.go), [sqlquery/sqlquery.go](../internal/infrastructure/sqlquery/sqlquery.go), [srcdump/srcdump.go](../internal/infrastructure/srcdump/srcdump.go), [raftstore/fsm.go](../internal/infrastructure/raftstore/fsm.go), [svcids/svcids.go](../internal/infrastructure/svcids/svcids.go), [apt/apt.go](../internal/infrastructure/apt/apt.go), [greenmask/](../internal/infrastructure/greenmask/), [anon/anon.go](../internal/infrastructure/anon/anon.go), [proxysql/proxysql.go](../internal/infrastructure/proxysql/proxysql.go)
- アプリ・API・UI・パッケージング: [internal/app/app.go](../internal/app/app.go), [agent.go](../internal/app/agent.go), [agent_http.go](../internal/app/agent_http.go), [agent_modules.go](../internal/app/agent_modules.go), [internal/interface/httpapi/server.go](../internal/interface/httpapi/server.go), [misc.go](../internal/interface/httpapi/misc.go), [record.go](../internal/interface/httpapi/record.go), [mcp.go](../internal/interface/httpapi/mcp.go), [metrics.go](../internal/interface/httpapi/metrics.go), [web/src/components/newbranch.tsx](../web/src/components/newbranch.tsx), [packaging/nfpm.yaml](../packaging/nfpm.yaml), [scripts/postinst](../packaging/scripts/postinst), [db-branch.service](../packaging/db-branch.service), [db-branch-mysqld@.service](../packaging/db-branch-mysqld@.service), [db-branch-postgres@.service](../packaging/db-branch-postgres@.service)

### 外部公式ドキュメント
- HashiCorp `go-plugin`（gRPC プロトコル・ハンドシェイク・非 Go プラグイン仕様）:
  - https://github.com/hashicorp/go-plugin
  - https://github.com/hashicorp/go-plugin/blob/main/docs/guide-plugin-write-non-go.md
- ClickHouse 公式ドキュメント（サーバー設定・`max_server_memory_usage`・`SYSTEM STOP MERGES`・`SYSTEM FLUSH LOGS`・`SYSTEM SHUTDOWN`・`ALTER TABLE ... FREEZE`）:
  - https://clickhouse.com/docs/operations/server-configuration-parameters/settings
  - https://clickhouse.com/docs/sql-reference/statements/system
  - https://clickhouse.com/docs/operations/backup
