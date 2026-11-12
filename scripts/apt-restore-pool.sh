#!/bin/sh
# Downloads the packages already listed in the published APT repository into
# <outdir>/pool/..., so a new release keeps every older version available.
# Every file is checked against the SHA-256 in the index it came from.
#
# usage: scripts/apt-restore-pool.sh <repo-url> <outdir>
#   <repo-url> is the repository root, e.g. https://apt.db-branch.com
#   Nothing to restore (first release, 404) is not an error.
set -eu
url=${1:?usage: apt-restore-pool.sh <repo-url> <outdir>}
out=${2:?missing outdir}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
n=0
for arch in amd64 arm64; do
	idx="$tmp/Packages.$arch"
	code=$(curl -s -o "$idx" -w '%{http_code}' "$url/dists/stable/main/binary-$arch/Packages" || true)
	if [ "$code" != 200 ]; then
		echo "no index for $arch ($code): nothing to restore"
		continue
	fi
	# One "<Filename> <SHA256>" pair per package.
	awk '/^Filename: /{f=$2} /^SHA256: /{print f, $2}' "$idx" >"$tmp/list.$arch"
	while read -r file sum; do
		[ -n "$file" ] || continue
		dest="$out/$file"
		mkdir -p "$(dirname "$dest")"
		if [ -f "$dest" ] && echo "$sum  $dest" | sha256sum -c --quiet 2>/dev/null; then
			continue
		fi
		curl -fsSL -o "$dest.part" "$url/$file"
		echo "$sum  $dest.part" | sha256sum -c --quiet
		mv "$dest.part" "$dest"
		n=$((n + 1))
		echo "restored $file"
	done <"$tmp/list.$arch"
done
echo "restored $n files"
