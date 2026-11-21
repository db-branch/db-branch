# マスク済みのベースを作る

本番のダンプから作ったベース（生データ）を、マスカーのモジュールで変換して、別のベースに書き出します。

```
db-branch base create prod --runtime postgres@18.6 --dump /srv/prod.sql.gz   # 生データ
db-branch base create dev  --runtime postgres@18.6                           # マスク先（空）
db-branch base mask dev --from prod --masker greenmask --rules rules.yaml
```

`base mask` の流れは次のとおりです。

1. マスク先のベースのノードで、元のベースのスナップショット（既定は最新）を一時サーバーとして起動する。
2. マスカーが一時サーバーを読み、マスク先のサーバーへ書き込む。
3. マスク先を止めてスナップショットを撮る。

撮ったスナップショットには、元のベースとスナップショット、マスカー、ルールの SHA-256、実行した人が記録されます（`GET /v1/bases/<name>` の `snapshots[].mask`）。マスカーが失敗したときは、マスク先を前のスナップショットに戻します。途中まで変換したデータは残りません。一時サーバーは、成功しても失敗しても消します。

ブランチは、いつもどおりマスク先のベースから作ります。元のベース（生データ）からブランチを作れないようにしたり、ルールの承認や検査を入れたりするのは、有償のマスク管理モジュールの役目です。

## 最初に試す（PostgreSQL と Greenmask）

小さなテーブルで、ダンプの読み込みからブランチの確認までを一通り試す手順です。次の例は、テストクラスタ（PostgreSQL 18.6、Greenmask v0.2.26）で実際に動かして確かめています。

### 1. 準備

1. マスカーのモジュールを入れます。マスカーのモジュールは本体とは別に提供します（本体のリポジトリには含まれません）。導入手順は別リポジトリの README に従ってください。

2. Greenmask 本体を、ベースがあるノードを含む**すべてのノード**に入れます（モジュールは本体を同梱しません）。[リリースページ](https://github.com/GreenmaskIO/greenmask/releases)から `greenmask-linux-amd64.tar.gz` を取り、`greenmask` を `/usr/local/bin/` に置きます。別の場所に置いたときは、設定 `greenmask_path` で教えます。

   ```sh
   tar xzf greenmask-linux-amd64.tar.gz greenmask
   sudo install -m 0755 greenmask /usr/local/bin/greenmask
   greenmask --version
   ```

   インストールの詳細は [Greenmask のインストール](https://docs.greenmask.io/latest/installation/)を見てください。

### 2. 生データのベースを作る

次のダンプ `shop.sql` を例にします。ユーザーの名前、メールアドレス、電話番号があり、注文のメモには電話番号が自由記述で入っています。

```sql
CREATE TABLE users (
  id serial PRIMARY KEY,
  name text NOT NULL,
  email text NOT NULL,
  phone text,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE orders (
  id serial PRIMARY KEY,
  user_id int NOT NULL REFERENCES users(id),
  amount numeric(10,2) NOT NULL,
  note text
);
INSERT INTO users (name, email, phone) VALUES
  ('Alice Example', 'alice@example.com', '090-1111-2222'),
  ('Bob Example', 'bob@example.com', '090-3333-4444'),
  ('Carol Example', 'carol@example.com', NULL);
INSERT INTO orders (user_id, amount, note) VALUES
  (1, 1200.00, 'gift'), (1, 800.50, NULL), (2, 300.00, 'call 090-3333-4444');
```

```sh
gzip shop.sql
db-branch base create prod --runtime postgres@18.6 --dump shop.sql.gz   # 生データ
db-branch base create dev  --runtime postgres@18.6                      # マスク先（空のまま）
```

### 3. ルールを書く

`rules.yaml` です。`databases` に書いたデータベースだけが写され、`transformation` には Greenmask の `dump.transformation` をそのまま書きます。

```yaml
databases:
  - name: postgres            # 写すデータベース。ここに書いたものだけを写す
    transformation:
      - schema: public
        name: users
        transformers:
          - name: RandomEmail # メールアドレスを別のものに置き換える
            params:
              column: email
              engine: hash    # 同じ値は同じ値になる（結合や重複検査が保たれる）
          - name: RandomPerson
            params:
              columns:
                - name: name
                  template: "{{ .FirstName }} {{ .LastName }}"
              gender: Any
          - name: RandomString
            params:
              column: phone
              min_length: 12
              max_length: 12
              symbols: "0123456789"
```

使える変換の一覧と各パラメーターは、[組み込みの変換（公式）](https://docs.greenmask.io/latest/built_in_transformers/)に載っています。手元でも `greenmask list-transformers` で一覧できます。

### 4. マスクして、ブランチで確かめる

```sh
db-branch base mask dev --from prod --masker greenmask --rules rules.yaml
db-branch branch create try1 --base dev
db-branch branch dsn try1        # 接続 URL（パスワードを含むので他人に見せない）
```

`psql "$(db-branch branch dsn try1)" -c 'select * from users'` の結果は、次のようになります（名前、メールアドレス、電話番号は実行のたびに変わります）。

```
 id |      name       |                              email                             |    phone
----+-----------------+----------------------------------------------------------------+--------------
  1 | Annabel Corwin  | 18cfe3de…770b@gmail.com                                         | 377156368870
  2 | Eula Veum       | 07b3d7b6…0774@fastmail.com                                      | 423254132966
  3 | Rogers Schimmel | 233a03e8…e8bf@zoho.com                                          |
```

`phone` が空だった行は、空のままです。`orders.note` は、ルールに書かなかったので元のままで、`call 090-3333-4444` という電話番号が残っています。**ルールに書いていない列は、そのまま写されます。**

### 5. 結果を確かめる

マスクした結果が十分かどうかは、必ず利用者が確かめます。次の点を見てください。

- **自由記述の列**（メモ、備考、コメント、問い合わせ本文）に、個人情報が入っていないか。上の `orders.note` のように、名前や電話番号が文章に混ざっている列は、列を丸ごと置き換えるか、`Replace` や `RandomString` などで消します。
- **ルールに書いていない列と、新しく増えた列**が、そのままでよいか。スキーマが変わっても、ルールに書いていない列は変換されません。ルールを更新するたびに、全列の一覧と見比べてください。
- **結合や重複の検査**に使う列は、`engine: hash` のように同じ値を同じ値にする設定にしないと、テーブル間で値が合わなくなります。
- 値の形（桁数、書式、`NOT NULL` や一意制約）が、アプリケーションの検査に通るか。通らないときは、変換の `params` を調整します。
- スナップショットの記録（`GET /v1/bases/dev` の `snapshots[].mask`）に、元のベースとルールの SHA-256 が残っていること。

そのほかに、変換する前の `prod` からブランチを作れないようにしたり、ルールの承認や検査 SQL を入れたりしたい場合は、有償のマスク管理モジュールを使います（この文書の最後の節）。

## マスカー（別提供のモジュール）

マスカーのモジュールは本体とは別に提供します（本体のリポジトリには含まれません）。導入手順は別リポジトリの README に従ってください。

| モジュール | 対象 | 中身 |
|---|---|---|
| `greenmask` | PostgreSQL | [Greenmask](https://github.com/GreenmaskIO/greenmask) v0.2 でダンプしながら変換し、マスク先へ戻す。ディスクにはマスク済みのデータしか書かれない |
| `greenmask-mysql` | MySQL | Greenmask v1（`greenmask-mysql`、**ベータ版**）で同じことをする |
| `anon` | PostgreSQL | [PostgreSQL Anonymizer](https://labs.dalibo.com/postgresql_anonymizer) の静的マスキング。マスク先へコピーしてから、その場で匿名化する |

### greenmask

公式: [Greenmask のドキュメント](https://docs.greenmask.io/latest/)、[組み込みの変換](https://docs.greenmask.io/latest/built_in_transformers/)

Greenmask 本体は同梱していません。各ノードに公式のリリースから入れてください（既定のパスは `/usr/local/bin/greenmask`。設定の `greenmask_path` で変えられます）。

ルールでは、写すデータベースと、そのテーブルの変換を書きます。`transformation` には Greenmask の `dump.transformation` をそのまま書けます。

```yaml
databases:
  - name: postgres          # 写すデータベース。ここに書いたものだけを写す
    transformation:
      - schema: public
        name: users
        transformers:
          - name: RandomEmail
            params:
              column: email
              engine: hash   # 同じ値は同じ値に置き換える（結合が保たれる）
  - name: analytics         # 変換なしで写す
```

使える変換は `greenmask list-transformers` で確かめられます。マスク先の既存のオブジェクトは、置き換えてから戻します。そのため、同じマスク先にもう一度実行しても問題ありません。

### greenmask-mysql

公式: [Greenmask のドキュメント](https://docs.greenmask.io/latest/)（MySQL 版は v1 系）

`greenmask-mysql`（Greenmask v1 のベータ版、v1.0.0b1 で確認）を各ノードに入れます（既定のパスは `/usr/local/bin/greenmask-mysql`）。ルールの形は greenmask と同じです。MySQL では `transformation` の `schema` にデータベース名を書きます。

```yaml
databases:
  - name: shop
    transformation:
      - schema: shop
        name: users
        transformers:
          - name: RandomEmail
            params:
              column: email
              engine: hash
```

Greenmask v1 はソケットで接続できず、TCP とパスワードを使います。そのため実行のたびに、元とマスク先に一時的なログイン `dbb_mask`（ランダムなパスワード、接続元は自ノードだけ）を作り、終わったら消します。マスク先の `local_infile` も実行中だけ有効にして、元に戻します。どちらも、スナップショットを撮る前に元へ戻ります。ベータ版のため、本番で使う前に手元のデータで結果を確かめてください。

### anon

公式: [PostgreSQL Anonymizer のドキュメント](https://postgresql-anonymizer.readthedocs.io/en/stable/)、[静的マスキング](https://postgresql-anonymizer.readthedocs.io/en/stable/static_masking/)、[マスキング関数](https://postgresql-anonymizer.readthedocs.io/en/stable/masking_functions/)

PostgreSQL Anonymizer の拡張は、db-branch の PostgreSQL ランタイムに入っている必要があります。Dalibo の `postgresql_anonymizer_<メジャー版>` の .deb（repo.dalibo.com。PostgreSQL 18 には 3.x）を各ノードに置き、設定の `package` に指定してください。拡張が無ければ、初回の実行時にランタイムへ展開します。

ルールには、データベースごとに Anonymizer のセキュリティラベルを書きます。

```yaml
databases:
  - name: postgres
    rules: |
      SECURITY LABEL FOR anon ON COLUMN public.users.email IS 'MASKED WITH FUNCTION anon.fake_email()';
      SECURITY LABEL FOR anon ON COLUMN public.users.name  IS 'MASKED WITH FUNCTION anon.dummy_name()';
```

1 回の実行の流れは次のとおりです。

1. 元のデータベースをダンプし、マスク先へ戻す。
2. 拡張を読み込み、ラベルを付けて `anon.anonymize_database()` を実行する。
3. ラベルと拡張を外す。
4. `VACUUM FULL` で古い行の版（元の値を持つ）を消してから、スナップショットを撮る。

Greenmask と違い、作業ディレクトリ（実行中だけ）とマスク先には、いったんマスク前のデータが書かれます。作業ディレクトリは root だけが読めるようにし、マスク先のベースは実行中にほかの人へ使わせないでください（実行中はメンテナンス状態になります）。

**どの列をどう変換するかを決め、結果を確かめるのは利用者です。** db-branch とマスカーは、与えられたルールを実行するだけです。マスクしたデータでも、多くの場合は個人情報として扱う必要があります。

## 公式ドキュメント

マスカーの書き方と使える変換は、それぞれの公式ドキュメントが正です。db-branch はルールをそのまま渡すだけで、中身は解釈しません。

| ツール | リンク |
|---|---|
| Greenmask（トップ） | <https://greenmask.io> |
| Greenmask のドキュメント | <https://docs.greenmask.io/latest/> |
| Greenmask のインストール | <https://docs.greenmask.io/latest/installation/> |
| Greenmask の組み込みの変換（一覧） | <https://docs.greenmask.io/latest/built_in_transformers/> |
| Greenmask の `dump` コマンドと設定（`transformation` の書き方） | <https://docs.greenmask.io/latest/commands/dump/> |
| Greenmask のリリース | <https://github.com/GreenmaskIO/greenmask/releases> |
| PostgreSQL Anonymizer のドキュメント | <https://postgresql-anonymizer.readthedocs.io/en/stable/> |
| PostgreSQL Anonymizer の静的マスキング | <https://postgresql-anonymizer.readthedocs.io/en/stable/static_masking/> |
| PostgreSQL Anonymizer のマスキングルールの宣言 | <https://postgresql-anonymizer.readthedocs.io/en/stable/declare_masking_rules/> |
| PostgreSQL Anonymizer のマスキング関数 | <https://postgresql-anonymizer.readthedocs.io/en/stable/masking_functions/> |

## つまずいたとき

| 症状 | 見るところ |
|---|---|
| `greenmask` が見つからない | 実行するノード（マスク先のベースがあるノード）に本体を入れたか。`greenmask_path` は絶対パスで書く |
| 変換の名前や `params` のエラー | ルールの該当部分を Greenmask のドキュメントと見比べる。`greenmask list-transformers` で名前を確かめる |
| 実行が失敗した | マスク先は前のスナップショットに戻っている。コマンドの出力に出るエラーを読み、ルールを直して、もう一度実行する |

## 更新の後に自動でマスクする（Business プラン）

[定期更新](base-refresh.md)や「今すぐ更新」が成功すると、masking モジュールに拡張ポイント `ext.RefreshFollower`（`AfterRefresh(ctx, base, snapshot) ([]MaskRun, error)`）で尋ねます。答えの `MaskRun{Target, Masker}` ごとに、更新で撮ったスナップショットを元にした `base.mask` オペレーションを起動します（実行者は `refresh`、ルールは `PrepareMask` が返す承認済みのもの）。この連鎖には masking モジュール（どのプランにも入っていないオプトインのベータ）が必要で、OSS だけでは更新の後に何も起こりません。同じマスク先に同時には走らせず、マスク先が使用中なら飛ばします。
