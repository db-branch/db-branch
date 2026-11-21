# 配布（APT リポジトリ）

## 利用者の手順

```sh
curl -fsSL https://apt.db-branch.com/db-branch.gpg | sudo tee /usr/share/keyrings/db-branch.gpg >/dev/null
echo "deb [signed-by=/usr/share/keyrings/db-branch.gpg] https://apt.db-branch.com stable main" | sudo tee /etc/apt/sources.list.d/db-branch.list
sudo apt update
sudo apt install db-branch          # Recommends で db-branch-proxysql と proxysql も入る
sudo db-branch init --pool <pool>
```

## リポジトリの中身

| パッケージ | 中身 |
|---|---|
| `db-branch`（amd64 / arm64） | agent・CLI・Web コンソール。`Recommends: db-branch-proxysql` |
| `db-branch-proxysql`（amd64 / arm64） | ProxySQL を設定するモジュール（`/usr/lib/db-branch/modules/proxysql`）。`proxysql (= 3.0.12)` に依存し、入れると agent を再起動して読み込ませる |
| `proxysql`（amd64 / arm64） | 公式リリースの .deb をそのまま転載（SHA-256 は `packaging/proxysql/upstream.env`） |
| `proxysql-3.0.12.tar.gz` | ProxySQL のソース（GPL-3.0 のため、バイナリと同じ場所で提供する） |

- 構成: `dists/stable/{InRelease,Release,Release.gpg}`、`dists/stable/main/binary-{amd64,arm64}/Packages`、`pool/main/...`、公開鍵 `db-branch.asc` / `db-branch.gpg`
- 作り方: `make deb deb-proxysql && make apt-repo APT_KEY=<鍵の指紋>`（`scripts/fetch-proxysql.sh` が ProxySQL を取得して SHA-256 を確認し、`scripts/apt-repo.sh` が `apt-ftparchive` で索引を作って署名する）
- CI（`.github/workflows/release.yml`）: `v*` のタグを push すると、両アーキテクチャの .deb（`make deb-oss`: `db-branch` と `db-branch-proxysql`。マスカーの各モジュールは別リポジトリで配布）を作り、`release` 環境の `publish` ジョブが次を行う。タイマーでの定期実行はない。
  1. タグが main の履歴にあることを確かめる
  2. 署名鍵を取り込み、`packaging/apt-signing-key.fpr` に書いた指紋と同じ鍵か確かめる（違えば止まる）
  3. 公開済みの索引から、これまでの版を取り込む（`scripts/apt-restore-pool.sh`。古い版が消えない）
  4. `make apt-repo` で署名した索引を作る
  5. R2 に上げる（`scripts/publish-apt.py`。`pool/` の新しいファイルを先に、索引を次に、`InRelease` を最後に。`pool/` は 1 年の immutable、索引と公開鍵は 60 秒のキャッシュ）
  6. 公開されたリポジトリに、利用者と同じ手順で `apt-get update` と `download` をして確かめる

## 公開先と設定

- 公開先は Cloudflare R2 のバケット `db-branch-apt`（カスタムドメイン `apt.db-branch.com`）。索引（`dists/`、公開鍵）は Cloudflare のキャッシュルールで 60 秒に抑えてある（抑えないと、リリース直後に古い索引が返り、`Hash Sum mismatch` になる）。
- GitHub の `release` 環境（`v*` のタグからだけ読める）に、次の Secrets を置く。
  - `APT_SIGNING_KEY`: 署名用の秘密鍵（`gpg --armor --export-secret-keys <指紋>` の出力）
  - `APT_SIGNING_PASSPHRASE`: その鍵のパスフレーズ
  - `CLOUDFLARE_API_TOKEN`: R2 の編集だけを許す API トークン
  - `CLOUDFLARE_ACCOUNT_ID`: Cloudflare のアカウント ID
- 本番の署名鍵を作ったら、指紋を `packaging/apt-signing-key.fpr` に書いて commit する。ジョブは、この指紋と違う鍵では署名しない。
- 必須レビュアーによる承認は、GitHub の有料プラン、またはリポジトリの公開で使える。それまでは、`release` 環境を `v*` のタグだけに絞っている。
- ブランチ保護とルールセットのひな形は `.github/rulesets/`、設定をまとめて入れるスクリプトは `scripts/apply-github-settings.sh`。private のあいだは GitHub が断るので、公開したあとにもう一度実行する。

## 署名鍵の扱い

- 秘密鍵は、パスフレーズ付きでパスワード管理ツールなどに保管し、失効証明書も別に保管する。リポジトリや開発用の VM には置かない。
- 鍵を替えるときは、新しい鍵で署名した索引を出し、公開鍵（`db-branch.gpg`）を差し替える。利用者は鍵を入れ直す。
- 秘密鍵が漏れたら、失効証明書を使って失効させ、新しい鍵で出し直す。

## 検証（2026-11-10）

使い捨ての鍵で署名したリポジトリを node1 から HTTP で配り、node3 で `apt update`（署名の検証）→ `apt install db-branch` を実行し、db-branch の更新と db-branch-proxysql の追加、`proxysql` の取得がリポジトリから行われることを確認した。試験用の設定は削除済み。
