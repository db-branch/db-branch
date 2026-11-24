#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"
export PATH="${HOME}/.elan/bin:${PATH}"

echo "==> Checking no sorry or axiom in Lean sources"
if grep -rn -E '\b(sorry|axiom)\b' DbBranchAuthz DbBranchAuthz.lean Main.lean; then
  echo "ERROR: found sorry or axiom in Lean sources" >&2
  exit 1
fi

echo "==> Building Lean formalization (lake build)"
lake build

echo "==> Exporting Lean decision matrix JSON"
lake exe export-matrix > decision_matrix.json

echo "==> Running Go differential, coverage, and safety verification tests"
(cd ../.. && go test -v ./internal/interface/httpapi -run TestLeanAuthz)
