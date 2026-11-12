# db-branch ユーザーの uid/gid をクラスタで揃える

パッケージは `db-branch` システムユーザーとグループを `adduser --system` で作る。uid/gid はそのマシンで空いている値が割り当てられるため、ノードごとに違う（例: Ubuntu 24.04 の 3 台は uid 109 / gid 112、後から加えた 26.04 は uid 103 / gid 109）。

ベースのコピーは `zfs send/receive` で運ばれ、ファイルの所有者は **数値のまま** 残る。uid が違うノードでそのコピーからブランチを作ると、データディレクトリは他ノードの uid の持ち物のままで、ローカルの `db-branch` で動く mysqld が `File './binlog.index' not found (OS errno 13 - Permission denied)` で起動できない。そこで、クラスタ全体で 1 組の uid/gid に揃える。

## どこに記録するか

- Raft の状態 `service_ids`（`{uid, gid}`）。コマンド `init_service_ids` で書く。**最初の記録だけが残り**（`init_admin` と同じく、状態機械が決める）、後からは変わらない。
- 新しいクラスタでは、最初のノード（`cluster init` したノード）のリーダーが、最初のハートビートで自分の値を記録する。
- 既存のクラスタは、全ノードが新しい版になってから（`Protocol` 15）、**そのときのリーダーの値** を採用する。揃っていないノードがあれば `cluster status` に出る（下記）。
- `Protocol` を 14 から 15 に上げた（新コマンド）。古いノードが混ざっている間はコマンドがログに書かれないので、記録も照合も始まらない。

## 参加の手順（`db-branch cluster join`）

1. トークンで参加先のメンバーに接続し、CA を受け取る。同じ応答に、クラスタの uid/gid（`service_ids`）が入っている。記録がまだないクラスタでは、リーダーが自分の値を返す。古いメンバーは何も返さない（その場合は揃えず、後でクラスタが記録した時点で食い違いとして報告される）。
2. **データを受け取る前に**、ローカルの `db-branch` の uid/gid と比べる。同じなら何もしない。
3. 違い、かつクラスタの uid/gid がローカルで **空いている** なら: `db-branch.service` と `db-branch-mysqld@*` / `db-branch-postgres@*` を止め、`/var/lib/db-branch`、`/etc/db-branch`、`/run/db-branch*`、`<pool>/db-branch` 以下の ZFS データセットのマウントで、古い uid/gid の持ち物を新しい値に付け替え（シンボリックリンクはリンク自体）、`groupmod -g` / `usermod -u -g` で `db-branch` を変更する。付け替えを先に行うので、途中で失敗しても同じコマンドをもう一度実行すれば続きから直る。
4. クラスタの uid/gid が **ローカルの別のユーザー / グループで使われている** なら、何も変えずに参加を断る。メッセージにどのアカウントが使っているかと対処（そのアカウントを別の id に変えてファイルも付け替える、または id が空いているマシンで作り直す）が出る。
5. 続けて CA と証明書を保存し、エージェントを再起動する。

## 食い違いの検出と配置

- 全ノードがハートビートに自分の uid/gid（`service_ids`）と、クラスタのものと食い違うときの理由（`id_problem`）を載せる。
- `db-branch cluster status` は食い違うノードごとに `PROBLEM on <node>: ...` を出し、コンソールの Cluster ページは警告とノードごとの uid/gid を出す。`GET /v1/cluster` はクラスタの `service_ids` とノードごとの `service_ids` / `id_problem` を返す。
- 食い違うノードには新しいブランチを置かず、ベースのコピーも受け取らない。`--node` でそのノードを指定すると `409 conflict`（理由つき）。
- 直すには、そのノードで root が `db-branch cluster align-ids` を実行する（エージェントが動いている必要がある。参加のときと同じ処理: 停止、付け替え、`usermod`、エージェントの再起動）。

## ブランチの起動に失敗したとき

データディレクトリの所有者がサーバーの実行ユーザーと違うときは、起動の前に検出して、ブランチを `failed` にする。`status_reason` は `data directory ownership mismatch: ... the node's db-branch uid/gid probably differ from the cluster's (see db-branch cluster status)` になる。サーバーが起動の途中で終了した場合（systemd のユニットが `failed`）も、2 分のタイムアウトを待たずにその場で失敗にし、ログに `Permission denied` / `errno 13` があれば同じ理由にする。`provisioning` のまま再試行し続けることはない。

## なぜパッケージで uid を固定しないか

固定の uid/gid（例: 全マシンで 990）をパッケージが決めると、すでにその値を使っているシステムで衝突する（Ubuntu のバージョンや他のパッケージが割り当てる範囲は一定でない）。衝突するマシンには入れられなくなる。そこで、インストール時は従来どおり空いている値を使い、クラスタに加わるときに揃える。
