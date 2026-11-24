#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"
export PATH="${HOME}/.elan/bin:${PATH}"

echo "==> Checking no sorry in Lean sources"
if grep -rn '\bsorry\b' DbBranchAuthz DbBranchAuthz.lean Main.lean; then
  echo "ERROR: found sorry in Lean sources" >&2
  exit 1
fi

echo "==> Building Lean formalization (lake build)"
lake build

echo "==> Exporting Lean decision matrix JSON"
lake exe export-matrix > decision_matrix.json

echo "==> Running Go differential and counterexample tests"
(cd ../.. && go test -v ./internal/interface/httpapi -run TestLeanAuthz)
