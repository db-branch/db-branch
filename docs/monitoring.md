# 監視

db-branch は Prometheus 形式のメトリクスと、ロードバランサー向けの稼働確認を出す。細かな警告の判定は Prometheus と Alertmanager に任せる。利用者や運用者にすぐ知らせたいことは、[通知](notifications.md)で Slack や Webhook に送れる。

## エンドポイント

| パス | 認証 | 内容 |
|---|---|---|
| `GET /healthz` | 不要 | エージェントが応答していれば 200 |
| `GET /readyz` | 不要 | 初期化済みで、クラスタにリーダーがいれば 200。いなければ 503 |
| `GET /v1/metrics` | トークン（`branches` で可） | Prometheus のテキスト形式 |

`/healthz` と `/readyz` は秘密を含まないので、ロードバランサーのヘルスチェックにそのまま使える。

## メトリクス

どのノードに聞いてもクラスタ全体の状態を返す（`dbbranch_api_requests_total` だけは、そのノードが受けたリクエスト数）。

| 名前 | 種類 | ラベル | 内容 |
|---|---|---|---|
| `dbbranch_build_info` | gauge | node, version | 応答したノードとその版 |
| `dbbranch_node_up` | gauge | node | ハートビートが届いていれば 1 |
| `dbbranch_node_leader` | gauge | node | Raft のリーダーなら 1 |
| `dbbranch_pool_size_bytes` / `dbbranch_pool_free_bytes` | gauge | node | ZFS プールの大きさと空き |
| `dbbranch_branches` | gauge | node, status | ブランチの数 |
| `dbbranch_bases` | gauge | status | ベースの数 |
| `dbbranch_base_snapshots` | gauge | base | ベースが持つスナップショットの数 |
| `dbbranch_base_replicas_missing` | gauge | base | 最新スナップショットをまだ持っていない稼働中ノードの数（複製の遅れ）。コピーの対象でないノード（`replica_nodes` にない、PostgreSQL で別の OS）は数えない |
| `dbbranch_module_problem` | gauge | node, module | モジュールが問題（ライセンスなど）を報告していれば 1 |
| `dbbranch_api_requests_total` | counter | action, outcome | このノードが受けた API リクエスト（`ok` / `failed` / `denied`） |

## Prometheus の設定例

サービス用トークン（Settings → Service tokens、範囲は `branches` で足りる）を発行し、ファイルに置く。証明書は各ノードの `/tls.pem`。

```yaml
scrape_configs:
  - job_name: db-branch
    scheme: https
    metrics_path: /v1/metrics
    tls_config:
      ca_file: /etc/prometheus/db-branch.pem
    authorization:
      credentials_file: /etc/prometheus/db-branch.token
    static_configs:
      - targets: ['192.0.2.10:7080', '192.0.2.11:7080', '192.0.2.12:7080']
```

全ノードから取ると、クラスタ全体のメトリクスが重複して届く。警告ルールでは `max by (...)` でまとめる（1 台が落ちても、残りのノードから取れる）。

## 警告ルールの例

```yaml
groups:
  - name: db-branch
    rules:
      - alert: DbBranchNodeDown
        expr: max by (node) (dbbranch_node_up) == 0
        for: 2m
        annotations: { summary: "db-branch node {{ $labels.node }} is down" }
      - alert: DbBranchNoLeader
        expr: max(dbbranch_node_leader) == 0 or absent(dbbranch_node_leader)
        for: 1m
        annotations: { summary: "the db-branch cluster has no Raft leader" }
      - alert: DbBranchPoolAlmostFull
        expr: max by (node) (dbbranch_pool_free_bytes / dbbranch_pool_size_bytes) < 0.10
        for: 10m
        annotations: { summary: "ZFS pool on {{ $labels.node }} is over 90% full" }
      - alert: DbBranchReplicationLag
        expr: max by (base) (dbbranch_base_replicas_missing) > 0
        for: 30m
        annotations: { summary: "base {{ $labels.base }} has not reached every node for 30 minutes" }
      - alert: DbBranchModuleProblem
        expr: max by (node, module) (dbbranch_module_problem) == 1
        for: 10m
        annotations: { summary: "module {{ $labels.module }} reports a problem on {{ $labels.node }}" }
      - alert: DbBranchBranchesFailing
        expr: max by (node) (dbbranch_branches{status=~"failed|lost"}) > 0
        for: 15m
        annotations: { summary: "branches on {{ $labels.node }} are failed or lost" }
```
