#!/usr/bin/env sh
set -eu

# Standalone db-branch client CLI installer for macOS and Linux.
# Usage: curl -fsSL https://raw.githubusercontent.com/db-branch/db-branch/main/scripts/install-client.sh | sh

RELEASE_BASE="${DB_BRANCH_RELEASE_BASE:-https://github.com/db-branch/db-branch/releases}"
RELEASE_BASE="${RELEASE_BASE%/}"
REQUESTED_VERSION="${DB_BRANCH_VERSION:-}"

die() {
  echo "error: $*" >&2
  exit 1
}

detect_os() {
  case "$(uname -s)" in
    Darwin) echo "darwin" ;;
    Linux) echo "linux" ;;
    *) die "unsupported OS: $(uname -s) (supported: Darwin, Linux)" ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    arm64|aarch64) echo "arm64" ;;
    *) die "unsupported architecture: $(uname -m) (supported: amd64, arm64)" ;;
  esac
}

fetch_to_file() {
  url="$1"
  out="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" -o "$out"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$out" "$url"
  else
    die "curl or wget is required"
  fi
}

sha256_file() {
  file="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$file" | awk '{print tolower($1)}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$file" | awk '{print tolower($1)}'
  else
    die "sha256sum or shasum is required"
  fi
}

choose_install_dir() {
  if [ -n "${DB_BRANCH_INSTALL_DIR:-}" ]; then
    echo "$DB_BRANCH_INSTALL_DIR"
    return
  fi
  if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
    echo "/usr/local/bin"
    return
  fi
  [ -n "${HOME:-}" ] || die "HOME is not set; set DB_BRANCH_INSTALL_DIR"
  echo "${HOME}/.local/bin"
}

main() {
  os="$(detect_os)"
  arch="$(detect_arch)"

  if [ -n "$REQUESTED_VERSION" ]; then
    ver="${REQUESTED_VERSION#v}"
    case "$ver" in
      *[!A-Za-z0-9._+-]*|"")
        die "invalid DB_BRANCH_VERSION: $REQUESTED_VERSION"
        ;;
    esac
    download_base="${RELEASE_BASE}/download/v${ver}"
  else
    download_base="${RELEASE_BASE}/latest/download"
  fi

  tmp="$(mktemp -d 2>/dev/null || mktemp -d -t db-branch-install)"
  trap 'rm -rf "$tmp"' EXIT INT TERM

  sums_file="$tmp/SHA256SUMS"
  echo "Downloading checksums from ${download_base}/SHA256SUMS..."
  fetch_to_file "${download_base}/SHA256SUMS" "$sums_file"

  suffix="_${os}_${arch}.tar.gz"
  match_line="$(awk -v sfx="$suffix" '
    NF == 2 {
      fname = $2
      sub(/^\*/, "", fname)
      if (index(fname, "db-branch_") == 1 && length(fname) > length(sfx) && substr(fname, length(fname) - length(sfx) + 1) == sfx) {
        print tolower($1) " " fname
      }
    }
  ' "$sums_file")"

  line_count="$(printf '%s\n' "$match_line" | awk 'NF > 0 { c++ } END { print c + 0 }')"
  if [ "$line_count" -eq 0 ]; then
    die "no release archive matching *${suffix} found in SHA256SUMS"
  fi
  if [ "$line_count" -gt 1 ]; then
    die "multiple release archives matched *${suffix} in SHA256SUMS"
  fi

  expected_sha="$(printf '%s\n' "$match_line" | awk '{print $1}')"
  archive_name="$(printf '%s\n' "$match_line" | awk '{print $2}')"

  case "$expected_sha" in
    *[!0-9a-f]*) die "invalid SHA-256 digest in SHA256SUMS" ;;
  esac
  if [ "${#expected_sha}" -ne 64 ]; then
    die "unexpected SHA-256 digest length in SHA256SUMS"
  fi
  case "$archive_name" in
    *[!A-Za-z0-9._+-]*|*..*|"") die "unsafe archive filename in SHA256SUMS: $archive_name" ;;
  esac

  archive_path="$tmp/$archive_name"
  echo "Downloading ${archive_name}..."
  fetch_to_file "${download_base}/${archive_name}" "$archive_path"

  actual_sha="$(sha256_file "$archive_path")"
  if [ "$actual_sha" != "$expected_sha" ]; then
    die "SHA-256 mismatch for ${archive_name}: expected ${expected_sha}, got ${actual_sha}"
  fi

  extract_dir="$tmp/extract"
  mkdir -p "$extract_dir"
  tar -C "$extract_dir" -xzf "$archive_path" db-branch
  [ -f "$extract_dir/db-branch" ] && [ ! -L "$extract_dir/db-branch" ] || die "db-branch regular binary not found in archive"

  install_dir="$(choose_install_dir)"
  mkdir -p "$install_dir"
  stage_bin="${install_dir}/.db-branch.tmp.$$"
  cp "$extract_dir/db-branch" "$stage_bin"
  chmod 0755 "$stage_bin"
  mv -f "$stage_bin" "${install_dir}/db-branch"

  echo "Installed db-branch to ${install_dir}/db-branch"
  "${install_dir}/db-branch" version || true

  case ":${PATH:-}:" in
    *":${install_dir}:"*) ;;
    *)
      echo ""
      echo "Add ${install_dir} to your PATH:"
      echo "  export PATH=\"${install_dir}:\$PATH\""
      ;;
  esac
}

main "$@"
