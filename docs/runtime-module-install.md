# ランタイムモジュールの導入・管理ガイド

db-branch は組み込みの MySQL / PostgreSQL に加え、サードパーティ製を含む外部データベースエンジンを **ランタイムモジュール** として追加できます。
このガイドでは、GitHub Releases からランタイムモジュールを安全に導入・更新・削除する手順と、モジュール作者向けの配布契約を説明します。

---

## `.deb` を使わない理由

サードパーティのランタイムモジュール配布には `.deb`（APT）ではなく、**署名済み `.tar.gz`（GitHub Releases）＋ `db-branch module` CLI** を採用しています。

1. **パッケージメンテナスクリプトを実行しない（最小権限）**: `.deb` の `postinst` / `prerm` は `root` 権限で任意のスクリプトを実行できます。本方式ではモジュール側のスクリプトを一切実行せず、配置・権限設定・専用システムユーザー（`db-branch-rt-<engine>`）作成をすべて db-branch 本体が固定ロジックで行います。
2. **APT リポジトリ運用の不要化**: サードパーティ作者が GPG 署名済み APT リポジトリや CDN を自前運用する必要がなく、GitHub Releases にアーカイブを置くだけで配布できます。
3. **信頼境界の明確化**: 信頼の起点は OS の APT 鍵ではなく、各ノードの `/etc/db-branch/trusted-keys/<key_id>.pub`（Ed25519 公開鍵）に限定されます。また導入操作は HTTP API には公開せず、ノード上での `sudo`（root）実行に限定しています。

---

## 信頼モデルと公開鍵の指紋確認

- ランタイムモジュールの `manifest.json` にはバイナリとカタログの SHA-256 ダイジェストが含まれ、`manifest.json.sig` が Ed25519 秘密鍵で署名されています。
- `db-branch` は `/etc/db-branch/trusted-keys/<key_id>.pub` に登録された公開鍵で署名とダイジェストを検証できたモジュールのみ導入・起動します。
- Release アーカイブに `<key_id>.pub` が同梱されていても、**自動的には信頼しません**。
  - **対話端末（TTY）**: 鍵 ID と SHA-256 指紋を表示し、`y/N` の明示確認を求めます。
  - **非対話環境（CI / Ansible 等）**: `--trust-key sha256:<64桁小文字hex>` で期待する指紋を明示しない限り終了コード `3` で拒否します。
  - **既知の `key_id` の保護**: 既に登録済みの `key_id` で署名検証に失敗した場合、同梱公開鍵で上書きしたり再確認したりせず即座に拒否します。

> **重要（別経路での指紋確認）**  
> Release 同梱の公開鍵の指紋は、必ず **GitHub Release とは別の経路**（作者の公式サイト、ドキュメント、リポジトリ外の公開鍵配布ページ等）で事前に照合してから信頼してください。リポジトリ自体が乗っ取られた場合、同梱の公開鍵も同時に差し替えられる可能性があります。

---

## 利用者向けコマンドリファレンス

### 1. 信頼する公開鍵を事前に登録・確認する

```bash
# ファイルから登録（--fingerprint で期待する指紋を照合可能）
sudo db-branch module trust add acme-2026 ./acme-2026.pub \
  --fingerprint sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08

# 標準入力から登録
cat acme-2026.pub | sudo db-branch module trust add acme-2026 -

# 登録済み公開鍵の一覧（非 root でも実行可能）
db-branch module trust list

# 不要になった公開鍵の削除（その鍵で署名されたモジュールが残っている場合は拒否）
sudo db-branch module trust remove acme-2026
```

### 2. ランタイムモジュールを導入する

引数が `github.com/<owner>/<repo>[@<tag>]` 形式のとき、ローカルのランタイムモジュール導入として動作します（従来の `proxysql` 等の単一モジュール名は従来どおりクラスター APT モジュールとして処理されます）。

```bash
# タグ指定で導入（未知鍵なら対話確認、既知鍵ならそのまま検証・導入）
sudo db-branch module install github.com/acme/db-branch-rt-clickhouse@v0.2.0

# タグ省略（最新の非 draft / 非 prerelease を取得）
sudo db-branch module install github.com/acme/db-branch-rt-clickhouse

# 非対話環境（Ansible / CI）で未知鍵の指紋を明示して導入
sudo db-branch module install github.com/acme/db-branch-rt-clickhouse@v0.2.0 \
  --trust-key sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08

# 導入後の db-branch.service 再起動を抑止する（複数モジュール連続導入時など）
sudo db-branch module install github.com/acme/db-branch-rt-clickhouse@v0.2.0 --no-restart
```

導入に成功すると、以下が自動的に行われます。

1. `root:root` 所有の `/usr/lib/db-branch/runtimes/<engine>/` への原子的配置と出所（`.source.json`）の記録
2. 専用非特権システムユーザー `db-branch-rt-<engine>`（`/usr/sbin/nologin`・ホームなし）の冪等作成
3. `db-branch.service` の再起動（`--no-restart` 指定時を除く）

### 3. アップグレードする

既に導入済みのエンジンを更新する場合は `--upgrade` を指定します。同一バージョンまたは古いバージョンへのダウングレード、および異なる GitHub リポジトリからの差し替えは拒否されます。

```bash
sudo db-branch module install github.com/acme/db-branch-rt-clickhouse@v0.3.0 --upgrade
```

### 4. 一覧を確認する

```bash
# クラスター APT モジュールとローカルランタイムモジュールの両方を表示（非 root 可）
db-branch module list

# ローカルランタイムモジュールのみ表示（エージェント停止中でも動作）
db-branch module list --runtime

# JSON 出力（--runtime なしは既存のクラスターモジュール配列を維持）
db-branch module list --runtime -o json
```

### 5. 削除する

対象エンジンを使用しているベースやブランチが 1 つでも存在する場合は削除を拒否します（`--force` は提供しません）。

```bash
sudo db-branch module remove clickhouse
# ローカルランタイムモジュールであることを明示する場合
sudo db-branch module remove clickhouse --runtime
# 再起動を抑止する場合
sudo db-branch module remove clickhouse --runtime --no-restart
```

削除後も以下は意図的に残ります。

- **OS ユーザー `db-branch-rt-<engine>`**: 残存データやバックアップの所有権を壊さないため削除しません。
- **信頼鍵 `/etc/db-branch/trusted-keys/<key_id>.pub`**: 必要なければ `sudo db-branch module trust remove <key_id>` で削除してください。

### 6. クラスター構成での運用

ランタイムモジュールの導入・更新・削除・信頼鍵登録は **実行したノード単体** に対して行われます。
クラスター構成ではノード間のバージョンずれを防ぐため、**必ず `@<tag>` と `--trust-key` を明示した同一コマンドを全ノードで実行**してください。

```bash
# 各ノードで同じタグを指定して実行する
sudo db-branch module install github.com/acme/db-branch-rt-clickhouse@v0.2.0 \
  --trust-key sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
```

また、クラスター構成で `module remove` を実行する際は、クラスター全体の使用状況を Raft 状態から確認するため、`db-branch.service` が稼働している必要があります（停止中は安全側に倒して拒否されます）。

### 7. 終了コード

自動化スクリプト（Ansible / CI）で原因を判別できるよう、以下の終了コードを返します。

| 終了コード | 意味 | 代表例 |
|---:|---|---|
| `0` | 成功 | 導入・更新・削除・鍵登録・一覧取得の成功 |
| `1` | 一般エラー | 通信失敗、SSRF 拒否、アーカイブ異常、SHA-256 不一致、署名改ざん、非 root |
| `2` | 引数・入力不正 | 不正な GitHub 参照、不正な `--trust-key`/`--fingerprint`、組み込みエンジン名指定 |
| `3` | 信頼拒否 | 対話確認で `N`、非 TTY で `--trust-key` 未指定、指紋不一致、同梱公開鍵なし |
| `4` | 競合・前提違反 | `--upgrade` なしで既存あり、ダウングレード、使用中エンジン/鍵の削除、既存 `key_id` 衝突、出所リポジトリ不一致 |

---

## モジュール作者向け: GitHub Release の配布契約

### 1. Release と Asset の命名規則

- リリースは **非 draft・非 prerelease** として公開してください。
- タグ名は `<version>` または `v<version>`（SemVer 2.0.0）とし、asset 名および `manifest.json` の `module_version` と一致させてください。
- 1 リリースあたり各アーキテクチャ（`amd64`, `arm64`）につき **ちょうど 1 つ** の `.tar.gz` asset を添付してください。

```text
db-branch-rt-<engine>_<version>_linux_<arch>.tar.gz
```

例: `db-branch-rt-clickhouse_0.2.0_linux_amd64.tar.gz`

- `<engine>`: `^[a-z][a-z0-9-]{0,18}$`（`mysql` / `postgres` は不可）
- `<version>`: SemVer 2.0.0（例: `0.2.0`）
- `<arch>`: `amd64` または `arm64`

### 2. `.tar.gz` アーカイブの中身

アーカイブは **トップレベル直下のフラットな通常ファイルのみ** を許可します（サブディレクトリ、シンボリックリンク、ハードリンク、特殊ファイル、`setuid`/`setgid`/`sticky` ビット、重複エントリはすべて拒否されます。サイズ上限はアーカイブ 100 MiB、展開合計 256 MiB、単一ファイル 200 MiB、最大 16 エントリ）。

必須ファイル:

1. `manifest.json`
   - `engine` が asset 名の `<engine>` と一致すること
   - `module_version` がタグ（先頭 `v` 除去）および asset 名の `<version>` と一致すること
   - `run_as_user` を省略せず、明示的に `"db-branch-rt-<engine>"` を指定すること
   - `binary` と `catalog` は本番配置先の絶対パス（`/usr/lib/db-branch/runtimes/<engine>/<file>`）を記載し、`binary_sha256` と `catalog_sha256` にそれぞれの SHA-256（64 桁小文字 hex）を設定すること
2. `manifest.json.sig`
   - `manifest.json` の生バイト列に対する Ed25519 署名 JSON:
     ```json
     {
       "key_id": "acme-2026",
       "signature": "<base64 encoded 64-byte Ed25519 signature>"
     }
     ```
3. `runtimes.json`（`manifest.json` の `catalog` が指すカタログファイル）
4. `<binary>`（`manifest.json` の `binary` が指す実行ファイル。展開時に `0755` が付与されます）

任意ファイル（最大 1 つ）:

5. `<key_id>.pub`（base64 エンコードされた 32 バイト Ed25519 公開鍵。初回導入時の候補鍵として利用者に指紋を提示するために同梱できます。ステージングディレクトリには配置されません）

### 3. Ed25519 鍵生成・署名・指紋計算の例

Go の標準ライブラリ（`crypto/ed25519`）だけで鍵生成・署名・指紋出力を行えます。

```go
// 公開鍵ファイル (<key_id>.pub): 32 バイト生公開鍵の標準 base64 (+ 改行)
pubB64 := base64.StdEncoding.EncodeToString(pub) + "\n"

// 公開鍵の SHA-256 指紋 (利用者へ公式サイト等で公開する値)
sum := sha256.Sum256(pub)
fingerprint := fmt.Sprintf("sha256:%x", sum)

// manifest.json の生バイト列を署名して manifest.json.sig を生成
sig := ed25519.Sign(priv, manifestBytes)
sigJSON, _ := json.MarshalIndent(map[string]string{
	"key_id":    "acme-2026",
	"signature": base64.StdEncoding.EncodeToString(sig),
}, "", "  ")
```

署名後に `manifest.json` を 1 バイトでも編集すると検証に失敗するため、必ず `manifest.json` を確定してから `manifest.json.sig` を生成し、フラットな `.tar.gz` にまとめてください。

```bash
tar -czf db-branch-rt-clickhouse_0.2.0_linux_amd64.tar.gz \
  -C ./dist-clickhouse \
  manifest.json manifest.json.sig runtimes.json db-branch-rt-clickhouse acme-2026.pub
```
