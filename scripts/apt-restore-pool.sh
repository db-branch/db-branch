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
url=${url%/}
out=${2:?missing outdir}
ua="db-branch-publish/1.0"
retry=${APT_RESTORE_RETRY:-3}
retry_delay=${APT_RESTORE_RETRY_DELAY:-2}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
verify_sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		printf '%s  %s\n' "$1" "$2" | sha256sum -c --quiet -
	else
		printf '%s  %s\n' "$1" "$2" | shasum -a 256 -c --quiet
	fi
}
n=0
for arch in amd64 arm64; do
	idx="$tmp/Packages.$arch"
	rc=0
	code=$(curl -sSL -A "$ua" --retry "$retry" --retry-delay "$retry_delay" --retry-connrefused --max-time 30 -o "$idx" -w '%{http_code}' "$url/dists/stable/main/binary-$arch/Packages") || rc=$?
	if [ "$rc" -eq 0 ] && [ "$code" = 404 ]; then
		echo "no index for $arch (404): nothing to restore"
		continue
	fi
	if [ "$rc" -ne 0 ] || [ "$code" != 200 ]; then
		echo "error fetching index for $arch (curl exit $rc, http ${code:-000})" >&2
		exit 1
	fi
	# One "<Filename> <SHA256>" pair per package, independent of field order.
	awk '
		function flush() {
			if (f != "" && s != "") {
				print f, s
			} else if (f != "" || s != "") {
				print "incomplete package stanza in index" > "/dev/stderr"
				exit 1
			}
			f = ""; s = ""
		}
		/^[[:space:]]*$/ { flush(); next }
		/^Filename: / { f = $2 }
		/^SHA256: / { s = $2 }
		END { flush() }
	' "$idx" >"$tmp/list.$arch"
	while read -r file sum; do
		[ -n "$file" ] || continue
		case "$file" in
		/* | .. | ../* | */../* | */..)
			echo "invalid filename in index: $file" >&2
			exit 1
			;;
		esac
		dest="$out/$file"
		mkdir -p "$(dirname "$dest")"
		if [ -f "$dest" ] && verify_sha256 "$sum" "$dest" 2>/dev/null; then
			continue
		fi
		if ! curl -fsSL -A "$ua" --retry "$retry" --retry-delay "$retry_delay" --retry-connrefused --max-time 120 -o "$dest.part" "$url/$file"; then
			rm -f "$dest.part"
			exit 1
		fi
		if ! verify_sha256 "$sum" "$dest.part"; then
			rm -f "$dest.part"
			exit 1
		fi
		mv "$dest.part" "$dest"
		n=$((n + 1))
		echo "restored $file"
	done <"$tmp/list.$arch"
done
echo "restored $n files"
