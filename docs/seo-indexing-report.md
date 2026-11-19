# Google Search Console「ページがインデックスに登録されない新しい要因」調査・修正レポート

実施日: 2026-11-19  
作業ブランチ: `fix/seo-indexing`

---

## 1. サイト構成とソースの所在

`db-branch.com` および関連サブドメインを実際に HTTP GET で調査し、本リポジトリ内の構成と照合した結果は以下のとおりです。

| ドメイン | 実体・ホスティング | ソースの所在 |
|---|---|---|
| `db-branch.com` | Astro v7.3.6 + Starlight v0.42.10（Cloudflare 上で配信） | **本リポジトリ外**（別リポジトリ / Cloudflare Pages・Workers 等）。本リポジトリの `docs/` は GitHub 上の設計ドキュメント（日本語）であり、Web サイト本体ではない。 |
| `www.db-branch.com` | Cloudflare による `db-branch.com` への 301 リダイレクト | Cloudflare 設定（本リポジトリ外） |
| `demo.db-branch.com` | ブラウザ完結型デモコンソール（Cloudflare Workers Static Assets） | **本リポジトリ内**: `web/`（`web/vite.config.ts`, `web/src/demo/`）および `web/demo-worker/`（`wrangler.jsonc`, `_headers`） |
| `apt.db-branch.com` | Debian パッケージ用 APT リポジトリ（Cloudflare R2 バケット `db-branch-apt`） | **本リポジトリ内**: `scripts/publish-apt.py`, `scripts/apt-repo.sh`, `.github/workflows/release.yml` |

---

## 2. 4分類ごとの該当 URL・原因・意図/非意図の判定

サイトマップ（`https://db-branch.com/sitemap-index.xml` → `https://db-branch.com/sitemap-0.xml`、全 37 URL）および内部リンククロール・URL バリエーション検証（計 76 URL）から特定した原因は以下のとおりです。

### 2.1 重複しています。ユーザーにより、正規ページとして選択されていません

| 該当 URL | 観測結果・原因 | 判定 |
|---|---|---|
| `https://demo.db-branch.com/admin`<br>`https://demo.db-branch.com/admin/bases`<br>`https://demo.db-branch.com/account`<br>`https://demo.db-branch.com/robots.txt`<br>`https://demo.db-branch.com/sitemap.xml`<br>その他 SPA 配下の任意パス | `web/demo-worker/wrangler.jsonc` の `"not_found_handling": "single-page-application"` により全パスが HTTP 200 で同一の `index.html` を返し、`<meta name="robots" content="index,follow" />` がある一方 `<link rel="canonical">` が未設定だった。アプリ内に `<a href="/admin">` 等のリンクが多数あるため、クローラが複数 URL を同一コンテンツ・canonical なしとして検出する。 | **非意図的**（本リポジトリで修正済み） |
| `http://demo.db-branch.com/` | HTTP→HTTPS リダイレクトがなく HTTP 200 で同一 HTML（canonical なし）を返す。 | **非意図的**（Cloudflare 設定で修正必要） |
| `https://db-branch.com/api-reference/` | HTTP 200 だが `<link rel="canonical">` が未設定かつサイトマップ未掲載。`/ja/api-reference/` から 301 リダイレクトされる先でもある。 | **非意図的**（外部サイトソースで修正必要） |

### 2.2 代替ページ（適切な canonical タグあり）

| 該当 URL | 観測結果・原因 | 判定 |
|---|---|---|
| `http://db-branch.com/` | HTTP→HTTPS リダイレクトがなく HTTP 200 を返し、HTML 内に `<link rel="canonical" href="https://db-branch.com/"/>` が含まれるため、HTTP 版が HTTPS 版の代替ページ扱いになる。 | **非意図的**（Cloudflare の「常に HTTPS を使用」を有効化して 301 リダイレクトにすべき） |
| `https://db-branch.com/` と `/ja/` などの日英ペア | 各ページが自己参照 canonical と双方向 `hreflang`（`en` / `ja` / `x-default`）を持つ。 | **意図的**（正常な多言語構成） |

### 2.3 見つかりませんでした（404）

| 該当 URL | 観測結果・原因 | 判定 |
|---|---|---|
| `https://db-branch.com/docs/`<br>（および `https://www.db-branch.com/docs/` → 301 先） | Starlight のドキュメントトップは `/docs/getting-started/` であり `/docs/` が存在しない（404）。一方、本リポジトリの `README.md`、`SUPPORT.md`、`docs/README.md`、`.github/ISSUE_TEMPLATE/config.yml` が `https://db-branch.com/docs/` にリンクしていた。 | **非意図的**（本リポジトリのリンクは修正済み。外部リンク対策としてサイト側でも `/docs/` → `/docs/getting-started/` の 301 設定を推奨） |
| `https://db-branch.com/tokushoho/` | `/ja/tokushoho/` の `<link rel="alternate" hreflang="en" ...>` / `x-default` および言語切替リンクが `/tokushoho/` を指しているが、英語版ページが存在せず 404。 | **非意図的**（外部サイトソースで修正必要） |
| `https://db-branch.com/plans/%EF%BC%89%E3%81%AE%E3%81%A8%E3%81%8A%E3%82%8A%E3%81%A7%E3%81%99%E3%80%82`<br>`https://db-branch.com/ja/terms/%EF%BC%89%E3%81%AB%E5%AE%9A%E3%82%81%E3%81%A6%E3%81%84%E3%81%BE%E3%81%99%E3%80%82`<br>`https://db-branch.com/docs/%EF%BC%89%E3%81%AB%E8%A8%98%E8%BC%89%E3%81%97%E3%81%A6%E3%81%84%E3%81%BE%E3%81%99%E3%80%82Ubuntu`<br>`https://db-branch.com/plans/%EF%BC%89%E3%81%AB%E6%8E%B2%E8%BC%89%E3%81%99%E3%82%8B%E3%80%81%E3%83%97%E3%83%A9%E3%83%B3%E3%81%AE%E5%86%85%E5%AE%B9%E3%83%BB%E6%96%99%E9%87%91%E3%81%AE%E4%B8%80%E8%A6%A7%E3%80%82` | `/ja/tokushoho/` および `/ja/terms/` 本文中の URL 直後に全角括弧 `）` や句点が空白なしで続き、自動リンクが後続テキストまで URL に取り込んで 404 リンクを生成している。 | **非意図的**（外部サイトソースで Markdown 修正必要） |
| `https://db-branch.com/sitemap.xml` | Astro/Starlight は `/sitemap-index.xml` を生成するため `/sitemap.xml` は 404。`/robots.txt` にも `Sitemap:` 指定がない（Cloudflare 管理の Content Signals のみ）。 | **非意図的**（`robots.txt` への `Sitemap: https://db-branch.com/sitemap-index.xml` 追記、必要なら `/sitemap.xml` → `/sitemap-index.xml` 301 を推奨） |
| `https://db-branch.com/docs`, `/ja/docs`, `/docs/index.html`, `/DOCS/` | 存在しないパス・ディレクトリインデックス。 | 概ね意図的（ただし `/docs` は `/docs/getting-started/` へ 301 すると親切） |
| `https://apt.db-branch.com/`, `/dists/`, `/sitemap.xml` | R2 バケットの仕様上、ディレクトリ一覧やルートページが存在せず 404。 | **意図的**（APT リポジトリ用途のため問題なし） |

### 2.4 ページにリダイレクトがあります

| 該当 URL | 観測結果・原因 | 判定 |
|---|---|---|
| `https://www.db-branch.com/*`<br>`http://www.db-branch.com/*` | `301` → `https://db-branch.com/*` | **意図的**（ドメイン正規化） |
| `https://db-branch.com/ja/api-reference/` | `301` → `https://db-branch.com/api-reference/` | **意図的**（API リファレンスは英語共通。ただし内部リンクが `/ja/api-reference/` を指している箇所は直接 `/api-reference/` に揃えると不要なリダイレクトを減らせる） |
| `https://db-branch.com/docs/getting-started`<br>`https://db-branch.com/ja`<br>`https://db-branch.com/ja/docs/getting-started`<br>`https://db-branch.com/index.html`<br>`https://db-branch.com/ja/index.html` | `307 Temporary Redirect` → 末尾スラッシュ付き正規 URL（または `/`） | **半意図的**（URL 正規化自体は意図的だが、SEO 上は一時リダイレクト `307` ではなく恒久リダイレクト `301` が望ましい） |

---

## 3. 本リポジトリで行った修正

- コミット: `d09e1b3`（ブランチ `fix/seo-indexing`）
  1. **デモサイト SEO 出力の追加**（`web/src/demo/seo.ts`, `web/src/demo/seo.test.ts`, `web/vite.config.ts`）:
     - `--mode demo` ビルド時に `index.html` へ `<link rel="canonical" href="https://demo.db-branch.com/" />` を注入し、SPA サブルート（`/admin`, `/account` 等）が重複ページ扱いされないようにした。
     - `dist-demo/robots.txt`（`Sitemap: https://demo.db-branch.com/sitemap.xml` 付き）および `dist-demo/sitemap.xml`（`https://demo.db-branch.com/` のみ掲載）を生成するようにし、SPA フォールバックで `text/html` が返る問題を解消した。
     - 通常のエージェント埋め込み用ビルド（`npm run build` → `web/dist`）には影響しないことを確認済み。
  2. **404 リンクの修正**（`README.md`, `SUPPORT.md`, `docs/README.md`, `.github/ISSUE_TEMPLATE/config.yml`）:
     - 404 となっていた `https://db-branch.com/docs/` へのリンクを `https://db-branch.com/docs/getting-started/` に修正した。

---

## 4. 手動対応が必要な項目（本リポジトリ外）

1. **Cloudflare ダッシュボード（`db-branch.com` / `demo.db-branch.com` ゾーン）**:
   - **SSL/TLS → エッジ証明書 →「常に HTTPS を使用（Always Use HTTPS）」を有効化**: `http://db-branch.com/` および `http://demo.db-branch.com/` が HTTP 200 を返している状態を 301 HTTPS リダイレクトにする。
   - **リダイレクトルール（必要に応じて）**: `https://db-branch.com/docs` および `/docs/` → `https://db-branch.com/docs/getting-started/`（301）。
2. **`db-branch.com`（Astro / Starlight サイト）の別リポジトリ**:
   - `/ja/tokushoho/` と `/ja/terms/` 内の URL 直後の `）` 巻き込みを修正（例: `[https://db-branch.com/plans/](https://db-branch.com/plans/)` のように明示的リンクにするか前後に半角スペースを入れる）。
   - `/ja/tokushoho/` の `<link rel="alternate" hreflang="en" ...>` / `x-default` が 404 の `/tokushoho/` を指さないよう調整（または英語版ページを用意）。
   - `/api-reference/` に `<link rel="canonical" href="https://db-branch.com/api-reference/" />` を追加し、可能なら `sitemap-0.xml` に含める。`/ja/docs/api/` 等から `/ja/api-reference/` へリンクしている場合は `/api-reference/` に直接リンクする。
   - `public/robots.txt` を配置し、`Sitemap: https://db-branch.com/sitemap-index.xml` を明記する（Cloudflare の Managed robots.txt と競合する場合は Cloudflare 側の設定を確認）。
   - 末尾スラッシュや `index.html` の 307 リダイレクトを 301 に変更できる場合は設定を見直す。
3. **`demo.db-branch.com` のデプロイ**:
   - 本リポジトリの修正をマージ後、`make demo`（`cd web && npm run build:demo`）を実行し、`web/demo-worker/wrangler.jsonc` で Cloudflare Workers へデプロイする（承認後）。

---

## 5. Google Search Console での「修正を検証」手順

1. 上記の本番反映（`demo.db-branch.com` の再デプロイ、Cloudflare の「常に HTTPS を使用」有効化、外部サイト側の 404/canonical 修正）を完了する。
2. Search Console の **「URL 検査」** で代表的な修正対象 URL（`https://demo.db-branch.com/admin`, `http://db-branch.com/`, `https://db-branch.com/docs/`, `https://db-branch.com/api-reference/`）を「公開 URL をテスト」し、ステータス・リダイレクト・canonical が期待どおりであることを確認する。
3. Search Console の **「ページ」→「ページがインデックスに登録されない理由」** から該当する 4 つの要因をそれぞれ開き、**「修正を検証」** ボタンを押す。
4. **「サイトマップ」** 画面で `https://db-branch.com/sitemap-index.xml`（および必要に応じて `https://demo.db-branch.com/sitemap.xml`）が正常に読み込まれていることを確認する。
