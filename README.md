# db-branch

ZFS クローンで MySQL / PostgreSQL のデータベースブランチを数秒で発行・リセット・破棄するツールです。
複数 VM でクラスタを組み、ProxySQL でユーザー名またはクエリコメント（`/*db_branch='name'*/`）によってブランチへ振り分けます。

> 開発中です。設計は [docs/design.md](docs/design.md) を参照してください。

## ライセンス

Apache-2.0
