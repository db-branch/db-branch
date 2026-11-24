# Contributing to db-branch

Thank you for helping. Before you open a pull request:

1. Read `CLAUDE.md` (development process: test-first, coverage ≥ 95%, clean architecture).
2. Run `make cover` and, for the console, `cd web && npx tsc`.

## Authorization Formal Verification (Lean 4)

If you modify authorization- or authentication-related files—including:

- `internal/interface/httpapi/access.go`
- `internal/interface/httpapi/server.go` (routes, `requireSession`, `guard`, `allowedBeforePasswordChange`)
- `internal/interface/httpapi/ownership.go`
- `internal/interface/httpapi/ext.go` / `tokens.go` / `misc.go` (auth/session/token handlers)
- `internal/interface/nodeapi/` (`forwardedAPI`, `socketOnlyRoutes`)
- `internal/usecase/auth/`
- `internal/usecase/token/`
- `internal/infrastructure/raftstore/` / `internal/infrastructure/authfile/` (session revocation & token storage)
- `internal/app/agent_modules.go` (`authUserDeleteStore` token revocation hook)
- `pkg/ext/license/`

you must update the Lean 4 formal model under `formal/lean/` (`DbBranchAuthz/*.lean`) to match the new specification and run `./formal/lean/verify.sh`:

1. Update the model, routes/aliases, and safety theorems in `formal/lean/DbBranchAuthz/` (no `sorry` allowed).
2. Run `./formal/lean/verify.sh` (requires `elan` / Lean 4 matching `formal/lean/lean-toolchain`). This checks for `sorry`, runs `lake build`, regenerates `formal/lean/decision_matrix.json`, and runs the Go differential and route-coverage tests (`go test -v ./internal/interface/httpapi -run TestLeanAuthz`).
3. Commit the updated `formal/lean/decision_matrix.json` together with your changes. CI runs `./formal/lean/verify.sh` and `git diff --exit-code formal/lean/decision_matrix.json`, and `TestLeanAuthz_RouterAndAliasesMatchDecisionMatrix` fails if routes, action names, or legacy aliases diverge from the model.

## Developer Certificate of Origin (DCO)

Every commit must be signed off, certifying the [Developer Certificate of
Origin 1.1](https://developercertificate.org/): that you wrote the change or
otherwise have the right to submit it under the project's licence
(Apache-2.0). Add the line with `git commit -s`:

    Signed-off-by: Your Name <you@example.com>

Use your real name and an address you can be reached at (a GitHub noreply
address is fine). The `dco` check fails a pull request with an unsigned
commit; fix it with `git rebase --signoff main` and force-push.

Contributions are licensed under Apache-2.0 (section 5 of the licence), as
is the rest of the repository.
