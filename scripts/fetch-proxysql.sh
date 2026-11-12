#!/bin/sh
# Downloads the pinned ProxySQL release (both architectures and the source)
# into the given directory and checks every SHA-256.
# usage: scripts/fetch-proxysql.sh <outdir> [--no-source]
set -eu
out=${1:?usage: fetch-proxysql.sh <outdir> [--no-source]}
here=$(cd "$(dirname "$0")/.." && pwd)
. "$here/packaging/proxysql/upstream.env"
mkdir -p "$out"
fetch() { # url sha256 file
	[ -f "$out/$3" ] && echo "$2  $out/$3" | sha256sum -c --quiet 2>/dev/null && return 0
	curl -fsSL -o "$out/$3.part" "$1"
	echo "$2  $out/$3.part" | sha256sum -c --quiet
	mv "$out/$3.part" "$out/$3"
}
fetch "$PROXYSQL_DEB_AMD64_URL" "$PROXYSQL_DEB_AMD64_SHA256" "proxysql_${PROXYSQL_VERSION}_amd64.deb"
fetch "$PROXYSQL_DEB_ARM64_URL" "$PROXYSQL_DEB_ARM64_SHA256" "proxysql_${PROXYSQL_VERSION}_arm64.deb"
if [ "${2:-}" != "--no-source" ]; then
	fetch "$PROXYSQL_SOURCE_URL" "$PROXYSQL_SOURCE_SHA256" "proxysql-${PROXYSQL_VERSION}.tar.gz"
fi
