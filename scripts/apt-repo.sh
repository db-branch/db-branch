#!/bin/sh
# Builds a signed APT repository ("stable" suite, "main" component) from .deb
# files and optional extra files for the pool (e.g. the ProxySQL source).
#
# usage: scripts/apt-repo.sh <repo-dir> <key-id> <file>...
#   The key must be in gpg's keyring (GNUPGHOME). With GPG_PASSPHRASE set it
#   is used non-interactively.
#
# Layout:
#   pool/main/<first letter>/<package>/<file>
#   dists/stable/{InRelease,Release,Release.gpg}
#   dists/stable/main/binary-{amd64,arm64,all}/Packages{,.gz}
#   db-branch.asc, db-branch.gpg   (public key, armored and binary)
set -eu
repo=${1:?usage: apt-repo.sh <repo-dir> <key-id> <file>...}
key=${2:?missing key id}
shift 2
mkdir -p "$repo/pool/main" "$repo/dists/stable/main"
for f in "$@"; do
	case "$f" in
	*.deb) pkg=$(dpkg-deb -f "$f" Package) ;;
	*) pkg=$(basename "$f" | sed 's/[-_][0-9].*//') ;;
	esac
	letter=$(printf %s "$pkg" | cut -c1)
	mkdir -p "$repo/pool/main/$letter/$pkg"
	cp -f "$f" "$repo/pool/main/$letter/$pkg/"
done
cd "$repo"
for arch in amd64 arm64; do
	mkdir -p "dists/stable/main/binary-$arch"
	# Architecture "all" packages belong in every architecture's index.
	apt-ftparchive --arch "$arch" packages pool/main >"dists/stable/main/binary-$arch/Packages"
	gzip -9nkf "dists/stable/main/binary-$arch/Packages"
done
apt-ftparchive \
	-o APT::FTPArchive::Release::Origin=db-branch \
	-o APT::FTPArchive::Release::Label=db-branch \
	-o APT::FTPArchive::Release::Suite=stable \
	-o APT::FTPArchive::Release::Codename=stable \
	-o APT::FTPArchive::Release::Architectures="amd64 arm64" \
	-o APT::FTPArchive::Release::Components=main \
	-o APT::FTPArchive::Release::Description="db-branch: instant database branches on ZFS" \
	release dists/stable >dists/stable/Release.new
mv dists/stable/Release.new dists/stable/Release
sign() {
	if [ -n "${GPG_PASSPHRASE:-}" ]; then
		gpg --batch --yes --pinentry-mode loopback --passphrase "$GPG_PASSPHRASE" --local-user "$key" "$@"
	else
		gpg --batch --yes --local-user "$key" "$@"
	fi
}
sign --clearsign -o dists/stable/InRelease dists/stable/Release
sign --armor --detach-sign -o dists/stable/Release.gpg dists/stable/Release
gpg --batch --yes --armor --export "$key" >db-branch.asc
gpg --batch --yes --export "$key" >db-branch.gpg
echo "repository ready in $repo"
