# db-branch 認可・認証 形式検証 (Lean 4)

`internal/interface/httpapi`・`internal/interface/nodeapi`・`internal/usecase/auth`・`internal/usecase/token`・`pkg/ext/license` の認可ロジックを Lean 4 でモデル化し、安全性の性質を形式証明（`sorry` なし）および Go 実ルーターとの差分テストで検証します。

## 構成

- `lean-toolchain`: 使用する Lean 4 バージョン (`leanprover/lean4:v4.36.0`)
- `lakefile.toml`: ライブラリ `DbBranchAuthz` と決定表出力バイナリ `export-matrix` の定義（Mathlib 非依存）
- `DbBranchAuthz/Types.lean`: チャネル (`socket` / `nodeForward` / `publicHTTP`)、認証手段、ロール/主体種別、セッション状態、所有関係、`server.go` / `ext.go` 全ルートの定義
- `DbBranchAuthz/Model.lean`: `server.go` / `access.go` / `ownership.go` / `ext.go` / `tokens.go` / `uploads.go` / `token.go` / `nodeapi.go` の認可判定モデル
- `DbBranchAuthz/Session.lean`: `auth.go` / `raftstore/fsm.go` / `authfile/revocations.go` のセッション失効・再起動・クラスター複製モデル、および `token.go` の個人トークン失効モデル
- `DbBranchAuthz/License.lean`: `pkg/ext/license/license.go` のプラン機能・opt-in・状態判定モデル
- `DbBranchAuthz/Theorems.lean`: 7 種の安全性定理・ライセンス性質および反例（バグ候補）の存在証明
- `DbBranchAuthz/JsonExport.lean` / `Main.lean`: 有限入力空間の全列挙と `decision_matrix.json` 出力
- `decision_matrix.json`: Lean モデルから生成した決定表（Go テストは Lean 未導入環境でもこの JSON で差分検証可能）
- `verify.sh`: `sorry` 検査・`lake build`・JSON 再生成・Go 差分テスト一括実行

## 検証コマンド

```bash
./formal/lean/verify.sh
```

Go 側のみの差分・反例テスト:

```bash
go test -v ./internal/interface/httpapi -run TestLeanAuthz
```
