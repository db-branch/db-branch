# db-branch 設計ドキュメント

ステータス: Draft / 最終更新: 2026-11-09

## 1. 目的

ZFS クローンを使い、本番相当のデータベースのブランチを数秒で発行・リセット・破棄できるようにする。

- **MySQL と PostgreSQL** に対応し、**複数バージョンを同時に**扱える。エンジンのバイナリは後からバージョンを指定して追加できる
- 単体の VM でも、複数 VM によるクラスタでも動く（Kubernetes は前提にしない）
- ProxySQL を前段に置き、**ユーザー名**または**クエリコメント**で接続先ブランチを振り分ける
- **Web コンソール**で操作できる
- 最終的に `apt install db-branch` でインストールできる

### 非目標（当面）

- 本番トラフィックの HA 構成（ブランチは開発・検証用途）
- ブランチ同士のマージやリベース（ZFS のブロックレベル CoW クローンであり DB エンジンの MVCC をフックしているわけではないため、並行書き込みしたブランチを Git のようにマージ・リベースすることはできない。AI エージェントや CI 向けの使い捨て（disposable）サンドボックスを前提とする）
- Kubernetes Operator
- 複数ユーザー・権限管理・監査ログ（有償モジュールで提供する。§12）

### 実装順

エンジンの抽象化は最初から入れる。実装は MySQL を先に完成させ、PostgreSQL を続けて追加する。

## 2. 用語

| 用語 | 意味 |
|---|---|
| ノード | db-branch を動かす VM 1 台。ZFS プール・DB サーバープロセス・ProxySQL を持つ |
| エンジン (engine) | DB の種類。`mysql` / `postgres` |
| ランタイム (runtime) | エンジンの特定バージョンのバイナリ一式。例: `mysql@8.4.3`、`postgres@17.2` |
| ベース (base) | ブランチの元になるデータセット。ランタイムが 1 つ紐づく。例: 本番のマスク済みダンプを流し込んだもの |
| ベーススナップショット | ベースの ZFS スナップショット。**全ノードへ伝搬**する |
| ブランチ (branch) | ベーススナップショットの ZFS クローンと、それを datadir とする DB サーバープロセス（mysqld / postgres） |
| リセットポイント | ブランチ作成直後、init SQL 適用後に取る ZFS スナップショット。**そのノードのみ**に置く |
| リーダー | Raft で自動選出されるノード。クラスタ状態の書き込みを担う |
| ベースオリジン | ベースを更新・送出するノード。リーダーとは独立した役割 |

## 3. 全体構成

```
              アプリ / 開発者
                    │  DNS ラウンドロビン or keepalived VIP
     ┌──────────────┼──────────────┐
   node1          node2          node3
  ┌────────┐    ┌────────┐    ┌────────┐
  │ProxySQL│    │ProxySQL│    │ProxySQL│   どのノードからも全ブランチへルーティング
  ├────────┤    ├────────┤    ├────────┤
  │db-branch│◀─Raft─▶db-branch│◀─Raft─▶db-branch│
  ├────────┤    ├────────┤    ├────────┤
  │ mysqld×N│   │ mysqld×N│   │ mysqld×N│   ブランチごとに 1 プロセス
  ├────────┤    ├────────┤    ├────────┤
  │ZFS tank│──send/recv──▶ZFS tank│──▶ │ZFS tank│  ベーススナップショットを伝搬
  └────────┘    └────────┘    └────────┘
```

### コンポーネント

`db-branch` は単一の Go バイナリで、サブコマンドで役割を切り替える。

- `db-branch agent` — 常駐デーモン（systemd）。以下を内包する
  - **Raft ストア**: クラスタ状態（ノード、ベース、ブランチ、割り当て）を保持
  - **ZFS ドライバ**: snapshot / clone / rollback / destroy / send / recv
  - **ランタイムマネージャ**: エンジンのバイナリをバージョン単位で取得・検証・配置（§4）
  - **エンジンランナー**: ブランチごとの DB サーバープロセス（mysqld / postgres）の起動・停止・ヘルスチェック
  - **ProxySQL リコンサイラ**: Raft 状態から自ノードの ProxySQL 設定を収束させる
  - **REST API**: CLI・Web コンソール・外部連携用
  - **Web コンソール**: バイナリに埋め込んだ管理画面（§11）
  - **拡張ポイント**: 認証・認可・監査を差し替えるためのインターフェース（§12）
- `db-branch <command>` — CLI（API クライアント）

### レイヤー構成

`domain → usecase → interface → infrastructure`（依存は内向き）。ZFS・mysqld・ProxySQL・Raft はすべて `infrastructure` に閉じ込め、usecase からはポート（interface）経由で使う。

## 4. エンジンとランタイム（複数バージョン）

DB サーバーのバイナリは OS のパッケージに依存せず、db-branch がバージョン単位で管理する。同じノードで `mysql@8.0.40`・`mysql@8.4.3`・`postgres@16.6`・`postgres@17.2` を同時に動かせる。

### 追加と削除

```
db-branch runtime available mysql            # 取得できるバージョンの一覧
db-branch runtime install mysql@8.4.3
db-branch runtime install postgres@17.2
db-branch runtime list
db-branch runtime remove mysql@8.0.40        # 参照するベースがあれば拒否
db-branch runtime import mysql@8.4.3-custom --path /opt/mysql   # 手元のビルドを登録
```

- 配置先: `/var/lib/db-branch/runtimes/<engine>/<version>/`（`bin/`, `lib/`, `share/`）
- 取得はユーザーの明示的な操作でのみ行う（パッケージのインストール時には何もダウンロードしない）
- **MySQL 8.0 以上の arm64 版**: Oracle が aarch64 の `-minimal` 版を出していないので、通常版（`glibc2.28-aarch64` の `.tar.xz`、800 MB〜1 GB）を使う。amd64 は `-minimal` 版。通常版に入っているデバッグ版のサーバーとプラグイン（`bin/*-debug`、`lib/plugin/debug`）は使わないので、導入時に消す（8.4.13 で 1.5 GB → 1.1 GB）
- **MySQL 5.7 と 5.6**（移行の検証用）: `mysql@5.7.44`、`mysql@5.6.51` を入れられる（amd64 のみ。Oracle が 5.x の arm64 版を出していない）。公式の汎用版（`glibc2.12` の `.tar.gz`）を使う。5.x のバイナリは RPATH を持たないので、`libaio` や ncurses 5 を使うものは、導入時に `lib/private` へ互換リンクを張り、それを見つける起動ラッパー（`<名前>.real` に本体を移す）に差し替える。サーバーの設定は、`mysqlx=OFF` を 8.0 以上だけに書き、5.6 の初期化は `mysql_install_db`（5.7 は `--initialize-insecure`）、ブランチのユーザーは `GRANT … IDENTIFIED BY …` で作る（5.6 は `CREATE USER IF NOT EXISTS` がない。匿名ユーザーと、ほかのホストの root も消す）。ブランチを作り、Checkpoint を使い、5.6 → 5.7 → 8.0 と上げられる（[upgrade.md](upgrade.md)）。**履歴（history）モジュールは 5.x では記録しない**（記録を求めると、理由を添えて断る）。マスキングは確かめていない。5.x は公式のサポートが終わっており、移行の検証のための機能として扱う

### クラスタでの配布

外部から取得するのはリーダー（マスター）だけで、他のノードはリーダーからダウンロードする。外部のネットワークに出られるノードが 1 台あれば足り、外部への通信と署名の検証も 1 回で済む。

1. `runtime install` を受けたリーダーが、取得元からアーカイブを取得し、署名を検証する
2. リーダーはアーカイブを自分のアーティファクト置き場（`/var/lib/db-branch/artifacts/`）に保存し、SHA256 とともに「必要なランタイム」として Raft に記録する
3. 各ノードの agent は不足に気づくと、ノード間 API（mTLS）でリーダーからアーカイブをダウンロードする
4. ダウンロードしたアーカイブの SHA256 を Raft の記録と照合してから展開する。一致しなければ破棄して再試行する
5. 全ノードへの配布が完了するまで、そのランタイムを使うベース・ブランチの配置は、配布済みのノードに限定する

- **リーダーが替わったとき**: 新しいリーダーがアーカイブを持っていなければ、外部ではなく、持っているノードから取得してから配布を続ける（外部の取得元が変わっていても、クラスタ内では同じアーカイブを使う）
- **リーダーの負荷**: 同時に配信するノード数に上限を設ける
- **ベースとの順序**: ベースを伝搬する前に、伝搬先ノードへそのランタイムを用意する

### 取得元（ランタイムプロバイダ）

エンジンごとにプロバイダを差し替えられるようにする。

| エンジン | 既定の取得元 | 検証 |
|---|---|---|
| MySQL | 公式の Linux Generic tarball（glibc 版） | 公式の GPG 署名と MD5/SHA |
| PostgreSQL | PGDG の apt リポジトリ（`apt.postgresql.org`、旧マイナー版は `apt-archive.postgresql.org`）の .deb を取得し、システムには入れずに展開する | リポジトリの GPG 署名 |
| 共通 | `import`（ローカルのパス）、ミラー URL（エアギャップ環境向け） | 任意で SHA256 |

PostgreSQL には公式の汎用バイナリ tarball がないため、PGDG の .deb を展開して使う。こうするとマイナーバージョンまで固定できる。

PGDG の .deb はディストリビューションごとにビルドされ、そのリリースのシステムライブラリ（ICU など）にリンクしている。カタログ（`runtimes.json`）は PostgreSQL のアーティファクトを `distros`（`ubuntu-24.04`、`ubuntu-26.04`）ごとに持ち、ノードは `/etc/os-release` の `ID` と `VERSION_ID` から自分のディストリビューションを決めて、合うビルド（`pgdg24.04+N` / `pgdg26.04+N`）を取得する。未対応のディストリビューションでは、そのOS名と対応するOSを挙げてエラーにし、別の版へ黙って切り替えることはしない。MySQL の tarball は glibc の汎用ビルドなので、ディストリビューションを問わない。クラスタのリーダーがほかのノードへ配るときも、要求したノードのディストリビューションのビルドを返す。カタログの PostgreSQL の項目は `scripts/pgdg-catalog.py` が PGDG のパッケージ索引から生成する。

### ベース・ブランチとの関係

- ベースは作成時に 1 つのランタイムに紐づく（datadir の形式がバージョンに依存するため）
- ブランチはベースのランタイムを引き継ぐ。ブランチのランタイムはブランチ自身の属性で、`branch upgrade` でそのブランチだけを上げられる（MySQL 8.0 → 8.4 など。ベースとほかのブランチは変わらない）。手順・許す移行・戻し方は [upgrade.md](upgrade.md)
- PostgreSQL のメジャーバージョンを上げるときは、新しいランタイムで新しいベースを作る（`pg_upgrade` は未対応）

### エンジンの抽象化

`domain` に `Engine` ポートを定義し、MySQL と PostgreSQL がそれぞれ実装する。

- datadir の初期化 / サーバーの起動と停止 / ヘルスチェック
- SQL の実行（init SQL、ユーザー作成）
- クローン後の固有処理（MySQL: `auto.cnf` の server UUID 再生成、PostgreSQL: `postmaster.pid` の除去など）
- 接続文字列（DSN）の生成

## 5. ブランチのライフサイクル

```
create ──▶ provisioning ──▶ ready ⇄ (reset) ──▶ deleting ──▶ (消滅)
                 │                     │
                 └──────▶ failed ◀─────┘
```

### 作成 `db-branch branch create feature-x --base prod-masked [--init init.sql] [--ttl 72h]`

1. リーダーが配置ノードを決定（ベーススナップショットを保持し、空き容量・ブランチ数に余裕があるノード）
2. 状態 `provisioning` を Raft に記録
3. 配置ノードの agent が実行
   - 注意: `--init` の SQL は呼び出し側の入力であり、管理者の SQL ではない。**ブランチのログインとして**実行する（管理者接続では流さない）。PostgreSQL は psql を通さずサーバーが直接読み（受け付けるのは平文の SQL と `\connect` だけ）、MySQL は `mysql --binary-mode`（クライアントのコマンド `system`・`\!`・`source` を無効にする）で流す。ログインなしの呼び出しは `init_sql` を渡せない（403）
   1. `zfs clone tank/bases/prod-masked@<snap> tank/branches/feature-x`
   2. ポートを払い出し、ベースのランタイムで DB サーバーを起動（datadir = クローン）
      - ブランチのポート範囲（既定 40000-40999）は Linux のエフェメラルポート（32768-60999）と重なるので、agent は起動時と範囲の変更時に `net.ipv4.ip_local_reserved_ports` へこの範囲を足す（既存の値は残す）。外向きの接続がブランチのポートを使ってしまい、サーバーが `Address already in use` で起動できなくなるのを防ぐ
   3. init SQL を適用（ブランチ用ユーザー作成を含む）
   4. DB サーバーを**正常停止**する（§6 の「一貫性のあるスナップショット」と同じ手順）
   5. `zfs snapshot tank/branches/feature-x@reset` — リセットポイント
   6. DB サーバーを起動
4. 状態 `ready` を記録 → 全ノードの ProxySQL リコンサイラがルーティングを追加

ベーススナップショットは正常停止した状態で取られているため、クローンからの起動でクラッシュリカバリは走らない。

### リセット `db-branch branch reset feature-x`

DB サーバー停止 → `zfs rollback tank/branches/feature-x@reset` → DB サーバー起動。`@reset` も正常停止した状態で取っているので、起動時のクラッシュリカバリは不要。init SQL の再実行もないので数秒で終わる。停止はロールバックで捨てる状態なので、即時停止（MySQL: `innodb_fast_shutdown=2` 相当、PostgreSQL: `immediate`）でよい。

### チェックポイント `db-branch branch checkpoint feature-x` / `branch restore feature-x <名前>`

稼働中のブランチのデータセットのスナップショットを取り、あとでその状態に戻せる。復元してもほかのチェックポイントは消えない（履歴の線を別のデータセットとして残す）。設計・データセットの配置・復元の手順・クラッシュ時の再開は [checkpoints.md](checkpoints.md)。

### アップグレード `db-branch branch upgrade feature-x --runtime mysql@8.4.13`

1 つのブランチだけを新しいランタイムに上げる。事前確認、きれいな停止、自動の Checkpoint `before-upgrade-<時刻>`、新しいバイナリでの起動、失敗時の自動復元。ブランチのランタイムは Checkpoint に記録され、Checkpoint の復元とリセットがランタイムを切り替える。設計は [upgrade.md](upgrade.md)。

### 削除 / TTL 失効

ProxySQL からルーティング削除 → DB サーバー停止 → `zfs destroy -r`。TTL はリーダーが監視して失効を発行する。

### ノード喪失時

リセットポイントはノードローカルのため、そのノード上のブランチは失われる（`lost` 状態にする）。ベースは全ノードにあるので、`db-branch branch recreate` で別ノードに作り直せる。

## 6. ベーススナップショットの取得と伝搬

### 一貫性のあるスナップショット（DB サーバーを止めてから取る）

PostgreSQL や MySQL（InnoDB）はクラッシュ整合性があるため、データディレクトリと WAL / redo ログが同一 ZFS データセット（同一ボリューム）上でアトミックにスナップショットできるなら、稼働中インスタンスのライブスナップショットから起動・動作させること自体は可能である（実際、稼働中ブランチの[チェックポイント](checkpoints.md)はライブスナップショットを使用する）。ただし、DB サーバーが動いたまま取ったスナップショットはエンジンから見ると「電源断の直後」の状態になるため、そこから作ったブランチは起動のたびに WAL / redo ログの適用や undo の巻き戻しといったクラッシュリカバリが走り、MySQL での検証では約 10 秒の起動遅延が生じた（データ量や未反映の変更が多いほど遅くなる）。

サブ秒〜数秒での高速なブランチ起動を優先するため、**ベーススナップショットとリセットポイントは、必ず DB サーバーを正常停止（クリーンシャットダウン）してから取る**。

`db-branch base snapshot prod-masked` の手順:

1. ベースへの新しい書き込みを止める（ベースの DB サーバーをメンテナンス状態にし、`base` に対する他の操作をロックする）
2. 停止前の準備
   - MySQL: `SET GLOBAL innodb_fast_shutdown = 0`（パージと change buffer のマージを済ませてから停止させ、ブランチ起動時の後処理をなくす）
   - PostgreSQL: `CHECKPOINT` を実行する
3. 正常停止
   - MySQL: `SHUTDOWN` を実行し、プロセスの終了を待つ
   - PostgreSQL: `pg_ctl stop -m fast`
4. 正常停止できたかを確認し、確認できなければスナップショットを取らずに失敗させる
   - MySQL: エラーログの `Shutdown complete` と、プロセスの終了コード
   - PostgreSQL: `pg_controldata` の `Database cluster state: shut down`
5. `zfs snapshot tank/bases/prod-masked@<世代>`
6. 停止前に動いていた場合は DB サーバーを再起動し、ロックを解除する
7. Raft に「最新スナップショット」として記録し、伝搬を始める

ブランチの起動時にも、クラッシュリカバリが走ったかどうかをエラーログで確認する。走っていたら、スナップショットの取り方に問題があるとして警告を出す。

### 伝搬

- ベースはベースオリジンノードで更新する（ダンプのロード、マスキング等）
- 各ノードの agent は自分の保持状況と比較し、不足していれば差分を受信する
  - `zfs send -i @prev @new | <TLS 経由> | zfs recv`
  - 初回はフル送信。送信元はオリジンに限らず、保持している任意のノード（負荷分散）
- 全ノードへの伝搬が完了するまで、新スナップショットからのブランチ作成は保持済みノードに限定する
- 古いスナップショットは、参照するブランチがなくなり、かつ保持世代数を超えたら削除する

### コピーを置くノードを選ぶ

既定では全ノードがベースのコピーを持つ。大きなベースを全ノードに置きたくないときは、ベースごとにコピーを置くノードを選べる（`db-branch base create … --replicas all|origin|n2,n3`、後から `db-branch base replicas <base> …`、API は `PUT /v1/bases/{name}/replicas`、権限は `base.replicas`）。

- 設定は「全ノード」（既定。既存のベースもこれ）か、ノードの一覧。起点ノードは常に持ち、一覧に書いても書かなくてもよい。空の一覧は「起点だけ」
- 受信するのは設定に含まれるノードだけ。一覧に加えたノードは次の巡回で取得を始める
- 一覧から外したノードは、コピーを他の古いコピーと同じように消す。ただしそのノードにこのベースを使うブランチが残る間は消さない（ZFS のクローンがスナップショットに依存するため）。この間そのノードは `draining` と表示され、新しいブランチの置き先にならない。ブランチがなくなった次の巡回で、コピーの記録を外してから消す
- ブランチは、要求したスナップショットを持ち、設定に含まれるノードにだけ置く。置き先がなければ、ベースを持つノードを挙げて断る
- トレードオフ: コピーを減らすとディスクを節約できるが、ブランチはそのベースを持つノードでしか動かせない。起点ノードを失ったときも、ベースを作り直せるのはコピーを持つノードだけで、コピーがなければベースは失われる
- Raft の `set_base_replica_nodes` と `remove_replica`（プロトコル 13）で記録する。全ノードが 13 になるまで書かれず、それまでは古いノードも含め全ノードに写す

### ログインなしの作成の上限

ログインなしで作れるのは、公開スナップショットからだけ（これが到達できるベースを決める。マスキングモジュールは検証済みのスナップショットしか公開させない）。全体の上限（設定 `branch.anonymous_max`・`branch.anonymous_ttl`）に加えて、ベースごと・接続元ごとの上限を持つ。

- ベースごと（`Base.Anonymous`）: `max_branches`（そのベースから作ったログインなしのブランチの上限）と `max_ttl`。**継承は明示する**: `max_branches` は `nil`（JSON では `null`）が継承で全体の上限だけが効き、`0` はそのベースからは作らせない。`max_ttl` は `0`（JSON では `null`）が継承で、設定があれば `branch.anonymous_ttl` との短いほうが効く。既存のベースと新しいベースは継承。全体の上限とベースの上限は両方が効く（厳しいほうが勝つ）
- `db-branch base anonymous <base> [--max N|inherit] [--ttl 4h|inherit]`（引数なしで現在値）、API は `PATCH /v1/bases/{name}/anonymous`、権限は `base.publish`（公開スナップショットを決める人と同じ）。コンソールは Bases の詳細の Without sign-in
- 接続元ごと: 設定 `branch.anonymous_rate`（既定 5、0 で無制限）。接続元アドレスごとの 1 時間の窓（スライディングウィンドウ）で、ノードのメモリの中だけで数える。アドレスは保存も記録もしない。IPv6 は /64 でまとめる。`X-Forwarded-For` は読まない
- Raft の `set_base_anonymous`（プロトコル 14）で記録する。`put_base` では上書きしない（レプリカ先と同じ）。全ノードが 14 になるまで書かれない

## 7. ZFS の設定（ARC とデータセット）

### ARC（ZFS の読み込みキャッシュ）

ZFS は空きメモリの大半を ARC に使おうとする。初期設定のままだと、InnoDB のバッファプールや PostgreSQL の shared_buffers とメモリを取り合い、ブランチの DB サーバーがメモリ不足で落ちることがある。

- **上限を必ず決める**: `db-branch init` で ARC の上限（`zfs_arc_max`）を設定する。既定は「物理メモリの 25%」で、`config.toml` の `[zfs] arc_max` で変えられる
  - 起動中のカーネルには `/sys/module/zfs/parameters/zfs_arc_max` で即時に反映する
  - 再起動後も残るよう `/etc/modprobe.d/db-branch-zfs.conf` に書く（パッケージのインストール時ではなく、`init` のときだけ。既存の設定があれば上書きせず警告する）
- **管理者が変える**: クラスタ設定 `zfs.arc_max`（コンソールの Settings、または `db-branch settings set core zfs.arc_max=50%`）で全ノードの上限を変えられる。物理メモリに対する割合（`50%`）か大きさ（`8GB`、`512MB`）で指定する。空なら `init` で決めた値のまま（既定）。各ノードは数秒以内に `zfs_arc_max` へ即時反映し、`db-branch-zfs.conf` にも書く（空に戻すと `init` の値に戻す）。その値は 64MB 以上、かつ物理メモリから予備（1GB と 10% の大きいほう）を引いた量以下でなければならず、満たさないノードはそのまま動き続け、クラスタ画面に理由を出す。別の modprobe ファイルが `zfs_arc_max` を決めているときも、再起動後に負ける可能性を同じ場所に出す。上げるとベースが大きいときのブランチ作成が速くなることがある。
  - 各ノードは実効値を `arc_max_mb` として報告し、配置スケジューラ（§9）は予算を「物理メモリ − ARC の上限」と「物理メモリの 75%」の小さいほうにする。上限を上げたノードには詰め込まれないが、下げても予算は 75% を超えない
- **ARC はブランチ間の共有キャッシュとして使う**: ブランチはベースのブロックの大半を共有している。ARC は同じブロックを 1 度だけキャッシュするので、ブランチがいくつあっても共有部分のキャッシュは 1 つで済む。一方、DB 側のバッファプールはブランチごとに別々に確保される。そのため次の方針にする
  - データセットは `primarycache=all` のままにする（一般的な「DB は metadata のみ」という設定は、ブランチ用途では逆効果）
  - ブランチの DB 側キャッシュは小さくする（既定: InnoDB バッファプール 128MB、shared_buffers 128MB。ブランチ作成時に変更できる）
- **メモリの予算**: ノードの物理メモリ = OS の予備 + ARC の上限 + ブランチごとの DB メモリの合計。配置スケジューラ（§9）はこの予算を超えるブランチを置かない

### データセットのプロパティ

| データセット | 設定 | 理由 |
|---|---|---|
| 全体（`<pool>/db-branch`） | `compression=lz4`, `atime=off`, `xattr=sa` | 容量削減と余計な書き込みの抑制 |
| MySQL のデータ | `recordsize=16K` | InnoDB のページサイズ（16K）に合わせ、二重 CoW / `fsync` 増幅とランダム I/O への悪影響を防ぐ |
| PostgreSQL のデータ | `recordsize=16K`（推奨の基本はページサイズに合わせた `8K` だが、`lz4` 圧縮効率との兼ね合いで既定は `16K`） | デフォルトの `128K` のままだと二重 CoW / `fsync` 増幅やランダム I/O 性能低下を招くため、ページサイズ（8K）または `16K` に抑える |

> ZFS のデフォルト `recordsize=128K` はデータベースのページサイズ（MySQL/InnoDB: 16K、PostgreSQL: 8K）と合わず、部分書き換えによる二重 CoW・`fsync` 増幅やランダム I/O 性能の悪化を招く。`db-branch` はベースデータセット作成時に `recordsize=16K` を自動設定する（ランダム I/O を最優先する PostgreSQL 環境では `8K` への調整も有効）。

1 つのブランチは 1 つのデータセットにまとめる（redo ログや WAL を別のデータセットに分けない）。分けると、スナップショットとクローンを複数のデータセットで揃えて扱う必要が出てくるため。

ZFS はコピーオンライトでページの書き込みが壊れないため、次の DB 側の設定を既定で無効にして書き込みを減らす。

- MySQL: `innodb_doublewrite=OFF`、`innodb_flush_neighbors=0`
- PostgreSQL: `full_page_writes=off`

ブランチは開発・検証用途なので、既定で `sync=standard` のまま、DB 側の永続性設定（`innodb_flush_log_at_trx_commit` など）も変えない。速さを優先したい場合はブランチ単位で緩められるようにする。

## 8. ルーティング（ProxySQL）

各ノードで ProxySQL（3.x）を動かし、どのノードに接続してもクラスタ内の全ブランチへ到達できるようにする。ProxySQL 3.x は MySQL と PostgreSQL の両方のプロトコルに対応しているため、エンジンが違っても同じ仕組みで振り分ける。

| | MySQL | PostgreSQL |
|---|---|---|
| クライアント向けポート | 6033 | 6133 |
| 管理インターフェース | 127.0.0.1:6032 | 127.0.0.1:6132 |
| 設定テーブル | `mysql_servers` / `mysql_users` / `mysql_query_rules` | `pgsql_servers` / `pgsql_users` / `pgsql_query_rules` |

ProxySQL が接続元に見せるサーバーバージョン（`mysql-server_version`）は、ブランチを上げても変えない。ブランチの実際のバージョンは `branch show` か、ブランチへ直接つないだ `SELECT VERSION()` で確かめる（[upgrade.md](upgrade.md) §7）。

PostgreSQL 側の機能の成熟度（特にクエリルールと拡張クエリプロトコルの扱い）は、フェーズ 6 の着手時に検証する。

### マッピング

- ブランチ 1 つ = ProxySQL の hostgroup 1 つ
- `*_servers`: hostgroup → `<配置ノード IP>:<ブランチのポート>`
- 各ノードの agent は、ブランチの変更（Raft で全ノードに届く）を受けるたびにすぐ設定を当て直す。5 秒ごとの見直しは取りこぼし対策として残す

### 8.1 ユーザー名による指定

ブランチ作成時にブランチ専用ユーザー（ブランチ名と同じ。例: `feature-x`）を発行し、`*_users.default_hostgroup` にそのブランチの hostgroup を設定する。アプリは接続文字列のユーザー名を変えるだけでよい。

### 8.2 クエリコメントによる指定

sqlcommenter 互換の形式を採用する。

```sql
SELECT * FROM users WHERE id = 1 /*db_branch='feature-x'*/;
```

HTTP ヘッダー（例: `X-DB-Branch`）をミドルウェアで読み、このコメントをクエリに付与すれば、リクエスト単位でブランチを切り替えられる。

クエリルールを 2 段で構成し、コメントのないクエリの評価コストを正規表現 1 回に抑える。

| rule | match_pattern | 動作 |
|---|---|---|
| 1 | `db_branch=` | `flagOUT=100`（コメントあり。2 段目へ） |
| 100+n | `db_branch='feature-x'` | `flagIN=100`, `destination_hostgroup=<feature-x>`, `apply=1` |

- 優先順位: コメント > ユーザー名 > ユーザーの既定ブランチ
- `transaction_persistent=1`: トランザクション途中ではブランチを切り替えない
- コメント指定は全ブランチ共通のユーザー（ベースから引き継がれる）で接続する前提。誰でも任意のブランチに入れるため、ユーザー単位で無効化できるようにする
- 注意: `mysql` CLI は初期設定でコメントを削る（`-c` が必要）

### リコンサイル

各 agent は Raft の状態を正として振り分けの設定を作り、ProxySQL モジュール（`db-branch-proxysql`、`ext.Router`）に渡す。モジュールが自ノードの ProxySQL 管理インターフェースで入れ替え、`LOAD ... TO RUNTIME` を実行する。モジュールがなければ振り分けは行わない。ProxySQL Cluster の設定同期は使わない（正となる情報源を 1 つにするため）。

## 9. クラスタ

- `hashicorp/raft` をバイナリに組み込み、外部の etcd 等は不要
- リーダーは自動選出。フェイルオーバーには 3 台以上（奇数）が必要
- リーダーが担うのは状態の書き込み（作成・削除・配置・TTL）だけ。既存ブランチへの接続はデータ経路（ProxySQL → DB サーバー）だけで完結し、リーダー不在でも止まらない
- 参加: `db-branch cluster init` / `db-branch cluster join <addr>`。参加したノードは非投票者で、リーダーが奇数台（最大 5）になるよう昇格・降格する（docs/cluster-plan.md C1）
- ノード間通信は mTLS（`cluster init` 時に CA を生成し、join トークンで証明書を発行）
- ノードの OS・カーネル・ZFS の版はハートビートで共有し、クラスタ表示に出す。ディストリビューションが混在するクラスタでは、PostgreSQL のブランチは元のスナップショットと同じディストリビューションのノードにだけ置く（照合順序ライブラリの違いによるインデックス破損を避ける）。詳細は [mixed-os.md](mixed-os.md)
- `db-branch` ユーザーの uid/gid はクラスタで 1 組に揃える。ベースのコピーは数値の所有者のまま運ばれるため、uid が違うノードではブランチのサーバーが起動できない。クラスタの値は Raft の状態に記録し（`init_service_ids`、Protocol 15）、`cluster join` はデータを受け取る前にローカルの `db-branch` をその値に付け替える（別のアカウントが使っていれば断る）。食い違うノードはハートビートで報告され、配置から外れる。パッケージで uid を固定しないのは、既存のシステムとの衝突を避けるため。詳細は [service-user-ids.md](service-user-ids.md)

## 10. パッケージング

| パッケージ | 内容 | 依存 |
|---|---|---|
| `db-branch` | `/usr/bin/db-branch`、systemd unit、`/etc/db-branch/config.toml` | Depends: `zfsutils-linux`, `tar`, `xz-utils`, `libaio1t64 \| libaio1`, `libnuma1`<br>Recommends: `db-branch-proxysql` |
| `db-branch-proxysql` | バージョン固定の ProxySQL（GPL-3.0）をリパッケージ | — |

- MySQL / PostgreSQL のバイナリはパッケージの依存にしない。ランタイムマネージャで後から追加する（§4）。公式の MySQL バイナリが使う共有ライブラリ（libaio、libnuma）だけは依存に入れる
- Ubuntu 24.04 / 26.04 と Debian 13 では libaio が `libaio.so.1t64` という名前になっているため、MySQL のランタイムを展開するときに、ランタイム内の `lib/private/` に `libaio.so.1` へのリンクを作る（システムには手を加えない）
- データ: `/var/lib/db-branch`、ログ: journald、専用システムユーザー `db-branch`
- インストールだけでディスクに触れず、何もダウンロードしない。ZFS プールの設定は `db-branch init` で明示的に行う
- `apt remove` ではデータを残し、`apt purge` で設定を削除する
- 対象: Ubuntu 24.04 LTS / 26.04 LTS（Debian 12+ は MySQL のみ）、amd64 / arm64。ICU と libxml2 はリリースごとにパッケージ名が違う（24.04: `libicu74` / `libxml2`、26.04: `libicu78` / `libxml2-16`）ので、依存は代替（`libicu74 | libicu76 | libicu78`、`libxml2 | libxml2-16`）で書き、同じ .deb を両方に入れられるようにする
- 配布: 自前の APT リポジトリ → PPA → Debian 公式の順に進める
- ProxySQL を同梱する際は、ライセンス文・対応するソースの入手先・変更点を `db-branch-proxysql` に含める
- db-branch 本体: Apache-2.0

## 11. Web コンソール

agent が管理画面を配信する。フロントエンド（React + Vite）はビルドして `go:embed` でバイナリに埋め込むので、追加のパッケージは不要。

- URL: `https://<任意のノード>:7080/`。書き込み操作はリーダーへ転送する
- TLS: 既定ではクラスタ CA が発行した証明書を使う。`config.toml` で任意の証明書に差し替えられる
- 状態の変化は Server-Sent Events で即時に反映する（再読み込み不要）
- **ブランチごとの画面**：Branches の一覧は、状態・名前・Base だけの軽い表で、名前を選ぶと、そのブランチの専用画面（`/admin/branches/<名前>`、リンクの共有・再読み込みができる）へ移る。専用画面には、Base とスナップショット・エンジン・接続先（コピー）・ノード（クラスタのとき）・所有者・作成・TTL・使用量、操作（Recreate〔lost のとき〕・Copy proxy URL・Extend 1h/24h/7d・Reset・Delete）、そして順に、Connect、Checkpoints（グラフ。Expand ボタンはそのまま）、Transactions（履歴モジュールがあり、記録するブランチで、読む権限があるとき。ライブの線のトランザクションを新しい順の表で。文・テーブルで絞り込み、古いものを続けて読める。読むだけで、復元はグラフで選ぶ）、Upgrade が並ぶ。一覧の操作メニューと、Connect・Checkpoints のダイアログは、この画面へ移した。新規作成は一覧に残る
- **SQL ランナー（ブランチのページ）**：管理画面のブランチのページと、ユーザーページのブランチのページに、SQL の欄がある（`ready` のブランチで、`branch.query` の権限があるとき）。`;` で区切った複数の文を、先頭から順に1文ずつ（API は1回に1文）、そのブランチ自身のユーザーとして実行し、途中で失敗したらそこで止め（文字列・コメント・`$$` の中の `;` では区切らない）、結果は文ごとに出る。1文ずつ（`POST /v1/branches/{name}/query`）、行は表（最大 500 行。超えると「(cut)」）、行を返さない文は影響行数と時間を出す。値は文字列で、NULL は NULL と出る。Ctrl / ⌘ + Enter で実行できる。変更は、そのブランチに残る（Reset で戻る）。履歴の保存や補完などはない\n- **ブランチの一時停止（Pause / Resume）**：`ready` のブランチのデータベースサーバーだけを、正常停止して、メモリを解放する（`POST /v1/branches/{name}/pause`、`branch pause <name>`）。データセット（差分）・ポート・認証情報は残り、状態は `pausing` → `paused`。`POST …/resume`（`branch resume <name>`）で、同じポート・同じ認証情報のまま、サーバーを起動して `ready` に戻す（`resuming`）。起動できなかったときは、`paused` のまま、理由を `status_reason` に残し、やり直せる。一時停止中は、接続できず（ProxySQL の振り分けにも入らない）、Reset・Checkpoint・アップグレードはできない（先に再開する）。削除と TTL の延長はできる。**TTL は止まらない**（期限が来れば、一時停止中でも削除される）。エージェントの再起動では、`paused` のブランチは起動しない（`pausing` は停止まで、`resuming` は起動まで進める）。権限は TTL の延長と同じ `branch.update`（所有者と管理者）。クラスターの全ノードがプロトコル 18 になるまでは、Pause を `cluster_upgrading` で断る（`paused` という状態を知らない古いノードが、起動や削除を誤るため）。コンソールでは、管理画面のブランチのページとユーザーページの行に、Extend と並んで Pause / Resume があり、状態は一時停止の印（2本の縦棒）と「Paused」で出る
- **Storage（管理画面）**：ノードごとに、プールのどれだけを、どの Base・Snapshot・ブランチが使っているかを、**木**で見るページ（`/admin/storage`。Branches・Bases と同じ権限。見るだけで、消す操作はない）。上にプール（db-branch・ほかの用途・空き）の帯、その下に、Base を根にした木が並ぶ。Base（使用量の大きい順）の下に、その Base 自身のデータ（`data`）と Snapshot（新しい順）、Snapshot の下に、その Snapshot から作ったブランチ（大きい順）。複製元の Snapshot がもう無いブランチは Base の直下に、Base がないブランチは「No base」のまとまりに入る。各行に、種類・名前（Base とブランチは、その専用画面へのリンク）・帯・大きさ・プールに占める割合が出る。大きさは、その行とその下をすべて足したもので、Snapshot は、それだけが持つ分も添える。▾ で、下の階層を畳める（Base は開いた状態から始まる）。期限切れのブランチと、他ノードの複製（replica）には印が付く。最後に、Base でもブランチでもないもの（復元で退けた線 `history/…`、作業用 `work/…`）の合計が出る。クラスタでは、見るノードを選ぶ（API は `GET /v1/nodes/{name}/storage`。ZFS の一覧を平らにして返し、木は画面で作る）。ZFS の `used` は、Snapshot では「その Snapshot だけが持つ分」、データセットでは「下にあるものすべて（Snapshot を含む）」。ブランチの大きさは、Base と共有している分を含まない、そのブランチ自身の変更分
- **一覧の検索と絞り込み**（管理画面の Branches と Bases）：一覧の上に、検索欄と絞り込みの選択肢が並ぶ。検索は空白で区切った語がすべて含まれるものを残し、Branches は名前・所有者・Base・ランタイム・状態・理由、Bases は名前・説明・ランタイム・状態・理由・ノードを見る（大文字小文字は区別しない）。絞り込みは、Branches が Status・Engine・Base・Owner、Bases が Status・Engine・Node（クラスタのときだけ）。選択肢は、一覧にある値だけ。何か指定しているあいだだけ「表示数 / 全体」と Clear が出る。条件は URL（`?q=…&status=…`）に残るので、再読み込みや共有で同じ一覧になる
- **diff（ブランチの比較）の画面**：diff モジュール（有償）の画面は、管理画面のサイドバーの Modules の Diff (beta)（`/admin/m/diff/ui/`）にある（`?a=<ブランチ>&b=<ブランチ>` で、A と B を入れた状態で開ける）。ブランチの画面にも、ユーザー側（ポータル）にも、入口は置いていない
- **管理画面の Base ごとの画面**：Bases の一覧は、タイムライン付きの一覧のままで、Base の名前を選ぶと、その Base の専用画面（`/admin/bases/<名前>`）へ移る。専用画面は、以前の詳細パネル（ドロワー）の中身（説明・複製先ノード・定期更新・Snapshot の一覧と公開設定・匿名の上限・使っているブランチ、Update data・Take snapshot・Delete base）を、そのまま置いたページで、今後ここに足していく
- **ユーザー側（ポータル）のブランチごとの画面**：「Your branches」の各行は、状態・名前・エンジン・Base・期限の要約と、コピー（接続 URL）・Extend +4h だけにし、行を選ぶと `/branch/<名前>`（リンクの共有・再読み込み・戻る進むができる）へ移る。行の展開はなくなり、展開で出していたもの（接続、Snapshot・Runtime・ノード・Recording・所有者・作成・期限・使用量、Checkpoints のグラフ、Keep for・Reset・Delete、Upgrade）は、この画面に最初から並ぶ。履歴モジュールがあり、記録するブランチで、読む権限があるときは、その下に Transactions（管理画面と同じ検索つきの表）が出る。ブランチを作って準備ができたときは、一覧にいる間に限り、その画面へ自動で移る（接続情報がすぐ見えるように）。`/branches/...` は、古い管理画面の URL の転送に使うので、ユーザー側は単数形

### デザイン方針

スタイリッシュで、文字は最小限。何を示しているかが形と色で直感的にわかることを最優先にする。

方向性は「よく作り込まれた計器・製図」。装飾ではなく、整列・余白・文字組みの階層で見せる。毎日使う道具として、落ち着いていて情報の密度が高いものにする。

**やらないこと（AI が作ったような見た目を避ける）**

- 紫や青のグラデーション、光彩、すりガラス（glassmorphism）、ぼかした色の塊
- 絵文字、キラキラ（✨）などの飾りアイコン
- 角の大きく丸いカードに影を付けて並べるだけのグリッド
- 「Welcome back!」「Get started」のような定型の文言や、宣伝調の言い回し
- 何にでも付けるピル型バッジ
- 弾む・ふわっと出るアニメーション。動きは状態が変わったときだけ、150ms 以内

**見た目の基本**

- 一覧は行（テーブル）で見せ、列を揃える。区切りは細い罫線。角丸は 4px 以下、影は使わない
- 書体: 本文は読みやすいサンセリフ、ブランチ名・接続文字列・バージョンなど識別子は等幅フォント（例: IBM Plex Sans / IBM Plex Mono）
- 色: 無彩色の基調に、アクセント 1 色（紫系は使わない）。状態の色（緑・琥珀・赤）は彩度を抑える

**原則**

1. **1 画面 1 目的**: 各画面の主役は 1 つ。主要な操作ボタンも 1 つだけ（例: ブランチ画面の「＋ Branch」）
2. **説明文を書かない**: 画面に説明の段落を置かない。必要な補足はツールチップに入れる。空の状態は「1 行 + ボタン 1 つ」だけ
3. **数値は人間の単位で**: `2h`、`1.2 GB`、`3 日前`。正確な値はツールチップで見せる
4. **色は意味にだけ使う**: 基調は落ち着いた無彩色 + アクセント 1 色。状態を表す色は状態以外に使わない
5. **色だけに頼らない**: 状態は色と形の両方で表す（色覚の違いに配慮）
6. **危険な操作は控えめに**: リセット・削除はメニューの中に置き、削除はブランチ名の入力で確定する

**視覚の語彙（全画面で共通）**

| 表すもの | 表現 |
|---|---|
| ブランチの状態 | ● 緑 = 稼働中 / ◌ 回転するリング = 準備中 / ▲ 赤 = 失敗 / ○ 灰の破線 = 消失 |
| 残り TTL | 行の中の細いバーが減っていく。残りわずかで琥珀色に変わる |
| エンジン | 小さなバッジ（MySQL / PostgreSQL で形と色を分ける）+ バージョン |
| ベースとブランチの関係 | ベースから枝分かれする線（ツリー） |
| ブランチの実容量 | 2 段のバー: 薄い部分 = ベースと共有（ZFS の referenced）、濃い部分 = このブランチ固有の差分（used）。コピーオンライトでほとんど容量を使っていないことが一目でわかる |
| ノードの空き容量 | ノードごとの横バー。80% で琥珀色、90% で赤 |
| リーダー | ノード名の横に `L` の小さな印 |
| 伝搬・配布の進み具合 | `2/3` の数字と細い進捗バー |

**画面構成**（左のナビは 3 つだけ）

- **Branches（ホーム）**: ブランチを 1 行ずつ並べる。列は、状態・名前・エンジン・TTL バー・容量バー・接続文字列のコピーボタンだけ
- **Bases**: ベースごとのツリー。スナップショットの世代と、そこから生えているブランチを線で示す
- **Cluster**: ノードを列として並べ、各列に容量バー・リーダーの印・載っているブランチの小さな四角を並べる。ランタイムもここで管理する
- 右上のアカウントメニュー: パスワードの変更、ログアウト

**技術**

- React + Vite。スタイルは CSS 変数（トークン）による素の CSS（細い罫線や等幅の数字を細かく制御するため）。ライトとダークの両テーマに対応
- 書体は IBM Plex Sans / Mono をパッケージに同梱する（ネットワークのない環境でも同じ見た目になる）
- アイコンは 1 つのアイコンセットに統一する
- 状態の変化は SSE（`/v1/events`）を受けて該当する一覧を取り直す

### 認証（OSS 版）

OSS 版のユーザーは `admin` の 1 人だけ。ユーザーの追加はできない（有償モジュールで提供。§12）。

- **初期パスワード**: `db-branch init`（クラスタでは `cluster init`）でランダムに生成し、1 回だけ表示する。あわせて `/etc/db-branch/initial-admin-password`（root のみ読み取り可、0600）に保存する。パスワードを変更したら、このファイルは削除する
- **パスワードの変更**: Web コンソールから（現在のパスワードの入力が必要）
- **評価用の初期パスワード**：`db-branch init --pool <pool> --evaluation` は、最初の管理者パスワードをランダムではなく `admin` にし、コンソールの最初のサインインで、新しいパスワード（12 文字以上）への変更を求める（Multipass の評価用 VM 向け。使い捨ての VM 以外では使わない）。変更するまでは、セッションの確認（`GET /v1/auth/session`、`must_change_password: true`）、サインアウト、パスワード変更の3つ以外の API は 403 `password_change_required` になる。変更が済めば通常どおり。すでに初期化したノードには何もしない。保存済みのランダムなパスワード（`/etc/db-branch/initial-admin-password`）は削除する。管理者のセッションにだけ付き、モジュールのユーザーには付かない
- **パスワードのリセット**: パスワードを忘れたときは、ノードの root が `sudo db-branch admin reset-password` を実行する。新しいランダムパスワードを表示し、既存のセッションはすべて無効にする
- **保存**: パスワードは argon2id でハッシュ化して Raft に保存する（どのノードでも同じパスワードでログインできる）
- **セッション**: Cookie（`HttpOnly`・`Secure`・`SameSite=Strict`）と CSRF トークン。ログインの試行回数には上限を設ける
- **CLI**: ノード上の CLI は UNIX ソケット経由で agent に接続し、root か `db-branch-admin` グループのメンバーなら操作できる（パスワード不要）。DB サーバーは `db-branch` ユーザーで動くので、このグループには入れない。DB サーバーを乗っ取った者が agent の管理者になれないようにするためである（ソケットは `db-branch-admin` がなければ root だけ）。DB サーバーの unit は `/run/db-branch` と `/etc/db-branch` を見られない
- **DB サーバーの設定と unit**: インスタンスのディレクトリ（`/var/lib/db-branch/instances/<id>`）と、その `env`・`my.cnf` は `root:db-branch`（0750/0640）で、DB サーバーのユーザーは読めるが書き換えられない（乗っ取った者が再起動後も居座るために `env` を書き換えるのを防ぐ）。MySQL のエラーログだけは、サービスユーザーの持ち物の `log/` に置く。DB サーバーの unit には `NoNewPrivileges`・`PrivateTmp`・`ProtectHome`・`ProtectSystem=full` などを付けてあるので、ブランチのデータ（ZFS のマウント先）を `/home`・`/usr`・`/etc` 以下に置くことはできない。稼働中のインスタンスは、次に再起動されるときに新しい所有者になる。
- **ブランチのログインの権限**: クライアントが使うログインは、データとその中のオブジェクトを読み書きできるだけで、サーバーのファイルやプログラムの実行には触れない。PostgreSQL ではスーパーユーザーにしない（`COPY ... TO PROGRAM` で OS のコマンドが走る）。代わりにベースの全オブジェクトを `db_branch_owner` ロールの持ち物にし（ベースのスナップショットを取る前と、ブランチのログインを作るとき）、ログインをそのメンバーにする。MySQL は `FILE`・`SUPER`・`SHUTDOWN`・`RELOAD`・`PROCESS`・`CREATE USER`・レプリケーション・`GRANT OPTION` を与えず、`my.cnf` で `secure_file_priv=NULL`・`local_infile=OFF` にする。以前に作ったブランチは、agent の起動時と DB サーバーの起動のたびに、この権限に直す

## 12. 拡張モジュール（有償）

商用版では、次の機能を有償モジュールとして提供する。

| モジュール | 内容 |
|---|---|
| `auth` | 複数ユーザー、ロールと権限（RBAC）、SSO（OIDC / SAML）、API トークン |
| `audit` | 誰がいつどのブランチを作成・削除・リセットし、どこに接続したかの記録、外部への転送（syslog・S3・SIEM） |

### OSS 側に用意する拡張ポイント

`pkg/ext`（外部から import できる公開パッケージ）にインターフェースを定義し、OSS 版には既定の実装を入れる。

| ポート | OSS 版の実装 | 有償モジュールでの差し替え |
|---|---|---|
| `Authenticator` | `admin` 1 人のパスワード認証 | 複数ユーザー・SSO・API トークン |
| `Authorizer` | 認証済みならすべて許可 | RBAC（チーム・ブランチ単位） |
| `AuditSink` | 何もしない（構造化ログに出すだけ） | 永続化・検索・外部転送 |

- すべての API 操作は `Authorizer` を通り、結果を `AuditSink` へイベントとして流す。OSS 版でもこの経路は必ず通す（モジュールを足しただけで漏れなく記録できるようにするため）
- ProxySQL 経由の接続記録は、ProxySQL の監査ログを agent が読み取り、`AuditSink` に流す

### 提供方式

モジュールは別のプロセスとして動かし、agent とは `hashicorp/go-plugin` で通信する（まず net/rpc。他言語のモジュールが要るときに gRPC を足す。[modules.md](modules.md)）。

- OSS のバイナリはそのまま使い、有償リポジトリから `apt install db-branch-auth db-branch-audit` で追加する
- 配置先: `/usr/lib/db-branch/modules/`。agent が起動時に検出して読み込む
- ライセンスキーは ed25519 で署名し、オフラインで検証する（エアギャップ環境でも使える）
- Web コンソールは `GET /v1/capabilities` で有効なモジュールを確認し、ユーザー管理や監査ログの画面を表示する
- 有償機能は db-branch 側だけで実装し、ProxySQL（GPL）には手を加えない

### 設定（2026-11-10）

- クラスタ全体の設定は Raft に置き、コンソールの Settings 画面と `db-branch settings list|set` で変える。db-branch 本体: ブランチのメモリ、既定の TTL、ブランチのポート範囲、コンソールのセッション期間。変更は数秒で全ノードに反映され、再起動は要らない
- ノード固有の設定（プール、待ち受けアドレス、データの置き場所）は従来どおり /etc/db-branch/config.toml
- コンソールのセッションは署名付きトークン（鍵は admin の資格情報と一緒に Raft に置く）。agent の再起動やノードをまたいでも続き、使うたびに延びる（既定 7 日）。パスワードを変えると以前のセッションはすべて無効

## 13. インターフェース（概要）

### CLI

```
db-branch init --pool tank
db-branch admin reset-password
db-branch cluster init | join <addr> | status
db-branch runtime available | install | list | remove | import
db-branch base create <name> --runtime mysql@8.4.3 --from-dump dump.sql.gz
db-branch base snapshot <name>
db-branch branch create <name> --base <base> [--init f.sql] [--ttl 72h]
db-branch branch list | show <name> | reset <name> | delete <name>
db-branch branch upgrade <name> --runtime mysql@8.4.13 [--check-only] [--no-rollback] [--force]
db-branch branch create <name> --base <base> --runtime mysql@8.4.13   # 作って、すぐ上げる
db-branch branch dsn <name>        # 接続文字列を表示
```

### REST API（`/v1`）

- ブランチ: `POST /branches`, `GET /branches`, `GET /branches/{name}`, `POST /branches/{name}/reset`, `DELETE /branches/{name}`
- ベース: `GET /bases`, `POST /bases`, `POST /bases/{name}/snapshots`
- ランタイム: `GET /runtimes`, `GET /runtimes/available`, `POST /runtimes`, `DELETE /runtimes/{engine}/{version}`
- クラスタ: `GET /cluster`
- 認証: `POST /auth/login`, `POST /auth/logout`, `PUT /auth/password`
- その他: `GET /capabilities`

## 14. 開発の進め方（フェーズ）

1. **単一ノード（MySQL）**: ランタイムマネージャ、ZFS クローンでのブランチ作成と起動、リセット、削除、CLI、.deb パッケージ化 — 完了（2026-11-09）
2. **Web コンソール**: admin 認証、パスワードの変更とリセット、ブランチの操作画面 — 完了（2026-11-09）
3. **ProxySQL（単一ノード）**: ユーザー名とコメントによるルーティング — 完了（2026-11-10、C6 と同時）
4. **クラスタ**: Raft、配置スケジューラ、ランタイムとベーススナップショットの伝搬 — 状態の複製とメンバー管理（C1）、担当ノードと配置（C2）、ベーススナップショットの伝搬（C3）、ランタイムの配布（C4）、クラスタプロトコル、ノード喪失（C5）は完了（2026-11-10）。手順は [cluster-plan.md](cluster-plan.md)
5. **クラスタルーティング**: 全ノードの ProxySQL から他ノードのブランチへ振り分け — MySQL は完了（2026-11-10、C6）
6. **PostgreSQL**: エンジンの実装、ProxySQL の PostgreSQL 対応の検証 — 完了（2026-11-10）。PGDG の .deb（署名付き InRelease → Packages の SHA-256 で固定）を展開してランタイムにし、`db-branch-postgres@.service` で動かす。正常停止は高速シャットダウン（SIGINT）で、`pg_controldata` が `shut down` を示すことを確認してからスナップショットを取る。ProxySQL は 6133 で、ユーザー名・コメントとも MySQL と同じ規則
7. **配布**: APT リポジトリ、署名、CI からの自動公開 — リポジトリの生成・署名・CI での作成まで完了（2026-11-10）。公開先と本番の署名鍵は未決（[distribution.md](distribution.md)）
8. **拡張ポイントの公開**: `pkg/ext` の安定化（有償モジュールの開発は別リポジトリ） — 完了（2026-11-10）。[modules.md](modules.md)

各フェーズで .deb を作り、検証 VM にインストールして動作確認する。

## 15. 未決事項

- APT リポジトリの置き場（Cloudflare R2 / GitHub Pages 案）
- ベースの作成元（MySQL: mysqldump / mydumper / XtraBackup、PostgreSQL: pg_dump / pg_basebackup のどれを一次サポートするか）
- ブランチの DB サーバーのリソース制限（systemd slice での CPU・メモリ上限）
- 各エンジンの既定バージョン
- MySQL の Generic tarball の配布 URL と署名検証の具体的な手順
- 有償モジュールのリポジトリ構成と配布経路
