#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -lt 4 ]; then
  echo "usage: $0 <version> <goos> <goarch> <out-dir> [prebuilt-bin]" >&2
  exit 1
fi

version="$1"
goos="$2"
goarch="$3"
out_dir="$4"
prebuilt="${5:-}"

script_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"

mkdir -p "$out_dir"
out_abs="$(cd "$out_dir" && pwd)"

stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT

bin_name="db-branch"
if [ "$goos" = "windows" ]; then
  bin_name="db-branch.exe"
fi

if [ -n "$prebuilt" ]; then
  cp "$prebuilt" "$stage/$bin_name"
else
  (cd "$repo_root" && CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath -ldflags "-s -w -X main.version=${version}" -o "$stage/$bin_name" ./cmd/db-branch-client)
fi
if [ "$goos" != "windows" ]; then
  chmod 0755 "$stage/$bin_name"
fi

cp "$repo_root/LICENSE" "$repo_root/NOTICE" "$repo_root/README.md" "$stage/"

stem="db-branch_${version}_${goos}_${goarch}"
if [ "$goos" = "windows" ]; then
  if command -v zip >/dev/null 2>&1; then
    (cd "$stage" && zip -q -r "$out_abs/${stem}.zip" .)
  else
    python3 -c '
import os, sys, zipfile
src, dst = sys.argv[1], sys.argv[2]
with zipfile.ZipFile(dst, "w", zipfile.ZIP_DEFLATED) as zf:
    for root, _, files in os.walk(src):
        for f in sorted(files):
            p = os.path.join(root, f)
            zf.write(p, os.path.relpath(p, src))
' "$stage" "$out_abs/${stem}.zip"
  fi
else
  COPYFILE_DISABLE=1 tar -C "$stage" -czf "$out_abs/${stem}.tar.gz" "$bin_name" LICENSE NOTICE README.md
fi
