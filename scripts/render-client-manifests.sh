#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <version> <sha256sums-file> <out-dir>" >&2
  exit 1
fi

version="$1"
sums_file="$2"
out_dir="$3"

case "$version" in
  ''|*[!A-Za-z0-9._+~-]*)
    echo "invalid version: $version" >&2
    exit 1
    ;;
esac

script_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"

lookup_sha() {
  local target="$1"
  local sha
  sha="$(awk -v f="$target" '$2 == f { print tolower($1); exit }' "$sums_file")"
  if [[ ! "$sha" =~ ^[0-9a-f]{64}$ ]]; then
    echo "missing or invalid sha256 for $target in $sums_file" >&2
    exit 1
  fi
  printf '%s' "$sha"
}

sha_darwin_arm64="$(lookup_sha "db-branch_${version}_darwin_arm64.tar.gz")"
sha_darwin_amd64="$(lookup_sha "db-branch_${version}_darwin_amd64.tar.gz")"
sha_linux_arm64="$(lookup_sha "db-branch_${version}_linux_arm64.tar.gz")"
sha_linux_amd64="$(lookup_sha "db-branch_${version}_linux_amd64.tar.gz")"
sha_windows_amd64="$(lookup_sha "db-branch_${version}_windows_amd64.zip")"
sha_windows_arm64="$(lookup_sha "db-branch_${version}_windows_arm64.zip")"

mkdir -p "$out_dir/Formula" "$out_dir/bucket"

sed \
  -e "s|__VERSION__|${version}|g" \
  -e "s|__SHA_DARWIN_ARM64__|${sha_darwin_arm64}|g" \
  -e "s|__SHA_DARWIN_AMD64__|${sha_darwin_amd64}|g" \
  -e "s|__SHA_LINUX_ARM64__|${sha_linux_arm64}|g" \
  -e "s|__SHA_LINUX_AMD64__|${sha_linux_amd64}|g" \
  "$repo_root/packaging/homebrew/db-branch.rb.tmpl" > "$out_dir/Formula/db-branch.rb"

sed \
  -e "s|__VERSION__|${version}|g" \
  -e "s|__SHA_WINDOWS_AMD64__|${sha_windows_amd64}|g" \
  -e "s|__SHA_WINDOWS_ARM64__|${sha_windows_arm64}|g" \
  "$repo_root/packaging/scoop/db-branch.json.tmpl" > "$out_dir/bucket/db-branch.json"
