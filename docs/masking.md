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

## マスカー（無料のモジュール）

| モジュール | 対象 | 中身 |
|---|---|---|
| `greenmask`（`db-branch-greenmask`） | PostgreSQL | [Greenmask](https://github.com/GreenmaskIO/greenmask) v0.2 でダンプしながら変換し、マスク先へ戻す。ディスクにはマスク済みのデータしか書かれない |
| `greenmask-mysql`（`db-branch-greenmask-mysql`） | MySQL | Greenmask v1（`greenmask-mysql`、**ベータ版**）で同じことをする |
| `anon`（`db-branch-anon`） | PostgreSQL | [PostgreSQL Anonymizer](https://labs.dalibo.com/postgresql_anonymizer) の静的マスキング。マスク先へコピーしてから、その場で匿名化する |

### greenmask

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

## 更新の後に自動でマスクする（Business プラン）

[定期更新](base-refresh.md)や「今すぐ更新」が成功すると、masking モジュールに拡張ポイント `ext.RefreshFollower`（`AfterRefresh(ctx, base, snapshot) ([]MaskRun, error)`）で尋ねます。答えの `MaskRun{Target, Masker}` ごとに、更新で撮ったスナップショットを元にした `base.mask` オペレーションを起動します（実行者は `refresh`、ルールは `PrepareMask` が返す承認済みのもの）。この連鎖には masking モジュール（Business プラン）が必要で、OSS だけでは更新の後に何も起こりません。同じマスク先に同時には走らせず、マスク先が使用中なら飛ばします。
