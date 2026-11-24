# 通知

クラスタのリーダー（または単一ノードのエージェント）が状態を定期的に（ハートビートと同じ間隔で）確かめ、次のことを Slack と Webhook に一度ずつ知らせる。

| イベント | レベル | 内容 |
|---|---|---|
| `branch.expiring` | warning | ブランチの期限が `notify.expiry_warning`（既定 1h）以内に迫った。延長して期限が変わると、新しい期限について改めて知らせる |
| `operation.failed` | error | ブランチやベースの作成などの操作が失敗した（理由つき） |
| `node.down` | error | ノードが 30 秒以上応答しない。そのノードのブランチには接続できない |
| `license.renewal_inactive` | warning | 月額・年額のサブスクリプションが解約または未払いで、キーを更新できない（期限ごとに 1 回） |
| `license.grace` | error | ライセンスが猶予期間に入ったのに自動更新が失敗している（期限ごとに 1 回） |
| `module.version_ahead` | warning | ノードに入っているモジュールがクラスターの記録より新しい（ダウングレードせず維持。全ノード更新後に記録を揃える案内） |
| `module.sync_failed` | error | ノードでモジュールの同期（インストール・削除）が連続して失敗した |
| `node.up` | info | 止まっていたノードが戻った |

既定ではどれも送らない。送り先を設定（設定画面の「全般」、または `db-branch settings set core ...`）すると始まる。

| 設定 | 内容 |
|---|---|
| `notify.slack_url` | Slack の Incoming Webhook の URL |
| `notify.webhook_url` | 任意の URL。各通知を JSON で POST する |
| `notify.webhook_secret` | 設定すると Webhook の本文に署名する |
| `notify.expiry_warning` | 期限の何時間前に知らせるか。`0` で期限の通知を止める |

## Webhook の形式

```
POST <notify.webhook_url>
Content-Type: application/json
X-Db-Branch-Event: branch.expiring
X-Db-Branch-Signature: sha256=<本文の HMAC-SHA256（鍵は notify.webhook_secret）の 16 進>

{"event":"branch.expiring","level":"warning","subject":"feature-x","text":"Branch feature-x expires in 45m (at 2026-11-10T15:47:22Z). Extend it if it is still in use.","time":"2026-11-10T15:02:00Z","owner":"ann@example.com"}
```

`owner` はブランチの通知だけに付く。受け側で持ち主にメールやチャットを送り直すときに使う。2xx 以外の応答は失敗としてリーダーのログに残る（再送はしない）。

## セキュリティ（SSRF 対策）と配送

- 通知先の URL は **HTTPS のみ**許可され、HTTP リダイレクトは追跡しません
- ループバック（`127.0.0.0/8`, `::1`）およびリンクローカル（`169.254.0.0/16`, `fe80::/10`、AWS EC2 IPv6 IMDS `fd00:ec2::254` を含む）アドレスは常に拒否します
- プライベート IP（RFC 1918 / IPv6 ULA）も既定で拒否します。社内ネットワークの Webhook を使う場合のみ、`/etc/db-branch/config.toml` の `[notify]` で `allow_private_ips = true` を設定してください
- 失敗時のエージェントログには秘密の Webhook URL をそのまま出さず、ホスト名など最小限の情報にマスクします
- 送信済みキーは送信が成功した場合のみ記録し、対象リソースが消えたものは定期的に回収（GC）します

## 注意

- 送ったかどうかの記録はメモリにだけ持つ。リーダーの交代やエージェント再起動の直後は、期限が迫ったブランチや止まったノードについて同じ通知がもう一度届くことがある
- 起動前に失敗していた操作は知らせない
