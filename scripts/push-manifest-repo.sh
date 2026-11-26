#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 5 ]; then
  echo "usage: $0 <repo-url> <src-file> <dst-relpath> <version> <token-env-name>" >&2
  exit 1
fi

repo_url="$1"
src_file="$2"
dst_relpath="$3"
version="$4"
token_env="$5"

token="${!token_env:-}"
if [ -z "$token" ]; then
  echo "::warning::${token_env} not set; skipping ${repo_url} update"
  exit 0
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

askpass="$tmp/askpass.sh"
cat > "$askpass" <<EOF
#!/usr/bin/env sh
case "\$1" in
  *Username*) printf 'x-access-token\n' ;;
  *) printf '%s\n' "\${${token_env}}" ;;
esac
EOF
chmod 0700 "$askpass"

export GIT_TERMINAL_PROMPT=0
export GIT_ASKPASS="$askpass"

git clone "$repo_url" "$tmp/repo"
cd "$tmp/repo"
git checkout -B main
mkdir -p "$(dirname "$dst_relpath")"
cp "$src_file" "$dst_relpath"
git config user.name "github-actions[bot]"
git config user.email "41898282+github-actions[bot]@users.noreply.github.com"
git add "$dst_relpath"
if ! git diff --cached --quiet; then
  git commit -m "db-branch $version"
  git push -u origin main
fi
