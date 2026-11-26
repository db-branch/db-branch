# クライアント CLI（開発者 PC / CI 向け `db-branch`）

開発者の手元の macOS / Windows / Linux や CI ランナーから、HTTPS API（TLS 1.3 + Bearer API トークン）経由で db-branch サーバー／クラスターのブランチを操作するための単体 CLI です。

## インストール

### ワンライナー（macOS / Linux / Windows、SHA-256 自動検証）

```sh
# macOS / Linux
curl -fsSL https://raw.githubusercontent.com/db-branch/db-branch/main/scripts/install-client.sh | sh
```

```powershell
# Windows (PowerShell)
irm https://raw.githubusercontent.com/db-branch/db-branch/main/scripts/install-client.ps1 | iex
```

`DB_BRANCH_VERSION`（バージョン指定）や `DB_BRANCH_INSTALL_DIR`（配置先ディレクトリ指定）で上書きできます：

```sh
# macOS / Linux の例
curl -fsSL https://raw.githubusercontent.com/db-branch/db-branch/main/scripts/install-client.sh | DB_BRANCH_VERSION=v1.0.0 sh
```

```powershell
# Windows (PowerShell) の例
$env:DB_BRANCH_VERSION='v1.0.0'; irm https://raw.githubusercontent.com/db-branch/db-branch/main/scripts/install-client.ps1 | iex
```

### Homebrew（macOS / Linux）

```sh
brew install db-branch/tap/db-branch
```

### Scoop（Windows）

```powershell
scoop bucket add db-branch https://github.com/db-branch/scoop-bucket
scoop install db-branch
```

### GitHub Releases から直接取得

[Releases](https://github.com/db-branch/db-branch/releases) から OS/アーキテクチャ別のアーカイブ（`darwin/amd64`, `darwin/arm64`, `linux/amd64`, `linux/arm64`, `windows/amd64`, `windows/arm64`）と `SHA256SUMS`（署名あり環境では `SHA256SUMS.asc`）を取得します。

```sh
# Linux の例
sha256sum -c SHA256SUMS --ignore-missing
# macOS の例（Apple Silicon）
grep darwin_arm64 SHA256SUMS | shasum -a 256 -c -
# GPG 署名検証（apt 公開鍵を使用する場合）
curl -fsSL https://apt.db-branch.com/db-branch.gpg | gpg --import
gpg --verify SHA256SUMS.asc SHA256SUMS
```

## ログインと認証設定

個人 API トークン（コンソールの Account → MCP access から発行、所有者ロール継承済み）またはサービス API トークン（`db-branch token create`）を使用します。

```sh
# 対話ログイン（トークンは非エコー入力、自己署名証明書は SHA-256 指紋を確認して TOFU 保存）
db-branch login --server https://db-branch.example.com:7080

# 複数プロファイルを使い分ける場合
db-branch login --profile staging --server https://staging.example.com:7080 --token-stdin < token.txt

# 社内 CA 証明書を指定する場合
db-branch login --server https://db-branch.example.com:7080 --ca /path/to/ca.pem
```

### 設定ファイルとトークン保管

- 設定ファイル（URL・TLS 指紋・プロファイル一覧）:
  - macOS / Linux: `~/.config/db-branch/client.toml`（`$XDG_CONFIG_HOME` 優先）
  - Windows: `%AppData%\db-branch\client.toml`
  - `--config`（クライアント単体バイナリ）または `DB_BRANCH_CONFIG` で変更可能。
- トークン保管先:
  - macOS: Keychain（`/usr/bin/security`）
  - Linux: Secret Service（`secret-tool`）
  - Windows: Credential Manager（`CredWriteW`）
  - キーチェーンが使えない環境（ヘッドレス Linux / SSH 等）では同ディレクトリの `credentials.toml`（パーミッション `0600`）へ保存し、警告を表示します。

### CI / 非対話環境（環境変数）

`DB_BRANCH_URL` と `DB_BRANCH_TOKEN` を設定すれば `login` や `client.toml` なしで実行できます。

| 環境変数 | 意味 |
|---|---|
| `DB_BRANCH_URL` | 接続先サーバー URL（`https://...`） |
| `DB_BRANCH_TOKEN` | Bearer API トークン |
| `DB_BRANCH_PROFILE` | 使用するプロファイル名 |
| `DB_BRANCH_CA` | CA 証明書 PEM パス |
| `DB_BRANCH_TLS_SHA256` | サーバー証明書の期待 SHA-256 フィンガープリント |
| `DB_BRANCH_CONFIG` | `client.toml` パスの上書き |

セキュリティ上の注意: `--profile`（`DB_BRANCH_PROFILE`）を指定せずに `--server`（`DB_BRANCH_URL`）を指定した場合、既存プロファイルのトークンや TLS ピン留めは流用されません（`DB_BRANCH_TOKEN` が必要です）。

### TLS 1.3 と自己署名証明書（TOFU・825 日再生成）

- すべての通信で **TLS 1.3** を必須としています（TLS 1.2 以下は拒否）。
- 自己署名証明書サーバーへ初回ログイン時は、サーバー証明書の Subject・有効期限・SAN・SHA-256 フィンガープリントを表示します。サーバー上で以下を実行して照合し、`y` で承認（または `--trust-sha256 <hex>` 指定）してください。

```sh
openssl x509 -in /var/lib/db-branch/tls/cert.pem -noout -fingerprint -sha256
```

- サーバー側の自己署名証明書が再生成された場合（825 日上限更新等）はピン留め不一致として接続を停止します。新しい指紋を確認した上で `db-branch login --server <url> [--profile <name>] --trust-sha256 <new-sha256>` で更新してください。

## コマンド一覧

共通フラグ: `-o json|table`、`--profile`、`--server`、`--ca`

### プロファイル・認証管理

- `db-branch login [--server <url>] [--profile <name>] [--token-stdin | --token <tok>] [--ca <pem>] [--trust-sha256 <hex>] [--no-use]`
- `db-branch logout [--profile <name>] [--all]`
- `db-branch profile list` / `db-branch profile use <name>` / `db-branch profile remove <name>`

### ブランチ・ベース操作

ノード上の CLI と同じサブコマンド・`-o json` 出力を使えます（詳細は [api.md](api.md)・[checkpoints.md](checkpoints.md)・[branch-fork.md](branch-fork.md)・[branch-merge.md](branch-merge.md)）。

- `db-branch base list` / `db-branch base show <name>`
- `db-branch branch create <name> --base <base> [--ttl 72h] [--init-sql ...] [--init-file ...] [--record] [--runtime ...]`
- `db-branch branch list` / `db-branch branch show <name>` / `db-branch branch delete <name>`
- `db-branch branch reset|pause|resume|recreate|extend|fork|upgrade|merge|dsn ...`
- `db-branch branch checkpoint|checkpoints|restore ...`
- `db-branch version`

### `connect`（接続情報・CLI 起動・GUI 接続ファイル生成）

```sh
# 接続情報を表示（既定はパスワードマスク、--reveal で表示、-o json 可）
db-branch connect mybranch [--proxy] [--reveal]

# mysql / psql を安全に起動（一時 0600 ファイル経由、argv/PGPASSWORD/MYSQL_PWD を使わない）
db-branch connect mybranch --exec mysql [-- -e "SELECT 1"]
db-branch connect mybranch --exec psql [-- -c "\dt"]
db-branch connect mybranch --exec auto

# GUI クライアント向け接続ファイル／URL を生成
db-branch connect mybranch --format dbeaver [--include-password] [--out mybranch.dbp]
db-branch connect mybranch --format chat2db
db-branch connect mybranch --format datagrip [--include-password]
db-branch connect mybranch --format navicat
db-branch connect mybranch --format pgadmin
db-branch connect mybranch --format workbench
db-branch connect mybranch --format tableplus [--include-password]
```

## リリースと署名・公証の設定（メンテナ向け）

タグ `v*` を `main` 上で push すると [`.github/workflows/release.yml`](../.github/workflows/release.yml) が 6 ターゲットの単体バイナリをビルドし、GitHub Releases とパッケージマネージャ定義を生成します。

### 必要な GitHub Secrets（`release` 環境）

秘密情報が未設定の項目は警告（`::warning::`）を出力してスキップし、リリース自体は失敗しません。

| Secret 名 | 用途 | 作り方・準備手順 |
|---|---|---|
| `MACOS_CERT_P12` | Apple Developer ID Application 証明書（base64） | Xcode / Developer ポータルで **Developer ID Application** 証明書を発行し、キーチェーンアクセスから `.p12` 形式で書き出し、`base64 -i cert.p12 \| pbcopy` で設定 |
| `MACOS_CERT_PASSWORD` | `.p12` の書き出しパスワード | 上記書き出し時に設定したパスワード |
| `APPLE_ID` | 公証（`notarytool`）用 Apple Account メール | Apple Developer 契約アカウントのメールアドレス |
| `APPLE_APP_PASSWORD` | 公証用 App 固有パスワード | [account.apple.com](https://account.apple.com) の「アプリ用パスワード」で生成 |
| `APPLE_TEAM_ID` | Apple Developer Team ID（10 桁） | Developer ポータルの Membership 詳細で確認 |
| `WINDOWS_CERT_PFX` | Windows Authenticode コード署名証明書（base64） | 認証局から発行されたコード署名 `.pfx` を `[Convert]::ToBase64String([IO.File]::ReadAllBytes("cert.pfx"))` で base64 化 |
| `WINDOWS_CERT_PASSWORD` | `.pfx` のパスワード | 上記 `.pfx` のパスワード |
| `APT_SIGNING_KEY` / `APT_SIGNING_PASSPHRASE` | 既存 APT GPG 秘密鍵（[distribution.md](distribution.md)） | 設定済みであれば `SHA256SUMS.asc` の生成にも共用 |
| `HOMEBREW_TAP_TOKEN` | `db-branch/homebrew-tap` 更新用 PAT（任意） | `homebrew-tap` リポジトリへの `contents: write` 権限を持つ Fine-grained PAT |
| `SCOOP_BUCKET_TOKEN` | `db-branch/scoop-bucket` 更新用 PAT（任意） | `scoop-bucket` リポジトリへの `contents: write` 権限を持つ Fine-grained PAT |

### 外部リポジトリの初期構成

- **`db-branch/homebrew-tap`**: 空リポジトリを作成し、`Formula/db-branch.rb` が自動更新されます（テンプレートは `packaging/homebrew/db-branch.rb.tmpl`）。
- **`db-branch/scoop-bucket`**: 空リポジトリを作成し、`bucket/db-branch.json` が自動更新されます（テンプレートは `packaging/scoop/db-branch.json.tmpl`）。
