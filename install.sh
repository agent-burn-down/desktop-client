#!/bin/sh

set -eu

repo="agent-burn-down/desktop-client"
install_dir="${BURNDOWN_INSTALL_DIR:-}"

if [ "$(uname -s)" != "Darwin" ]; then
  printf '%s\n' "burndown-cli releases currently support macOS only." >&2
  exit 1
fi

case "$(uname -m)" in
  arm64) release_arch="arm64" ;;
  x86_64) release_arch="amd64" ;;
  *)
    printf 'Unsupported macOS architecture: %s\n' "$(uname -m)" >&2
    exit 1
    ;;
esac

if [ -z "$install_dir" ]; then
  if [ "$release_arch" = "arm64" ] && [ -d /opt/homebrew/bin ]; then
    install_dir="/opt/homebrew/bin"
  else
    install_dir="/usr/local/bin"
  fi
fi

latest_url="https://github.com/${repo}/releases/latest"
resolved_url=$(curl -fsSL -o /dev/null -w '%{url_effective}' "$latest_url")
version=${resolved_url##*/}

case "$version" in
  v[0-9]*) ;;
  *)
    printf 'Could not resolve the latest burndown-cli release from %s\n' "$resolved_url" >&2
    exit 1
    ;;
esac

archive="burndown-cli_${version#v}_darwin_${release_arch}.tar.gz"
download_base="https://github.com/${repo}/releases/download/${version}"
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/burndown-cli-install.XXXXXX")
trap 'rm -rf "$work_dir"' EXIT HUP INT TERM

printf 'Downloading burndown-cli %s for macOS/%s...\n' "$version" "$release_arch"
curl -fsSLo "$work_dir/$archive" "$download_base/$archive"
curl -fsSLo "$work_dir/checksums.txt" "$download_base/checksums.txt"

(
  cd "$work_dir"
  grep "  $archive\$" checksums.txt | shasum -a 256 -c -
  tar -xzf "$archive"
)

if [ ! -d "$install_dir" ]; then
  if mkdir -p "$install_dir" 2>/dev/null; then
    :
  elif command -v sudo >/dev/null 2>&1; then
    sudo mkdir -p "$install_dir"
  else
    printf 'Cannot create %s. Set BURNDOWN_INSTALL_DIR to a writable directory.\n' "$install_dir" >&2
    exit 1
  fi
fi

if [ -w "$install_dir" ]; then
  install -m 0755 "$work_dir/burndown-cli" "$install_dir/burndown-cli"
elif command -v sudo >/dev/null 2>&1; then
  sudo install -m 0755 "$work_dir/burndown-cli" "$install_dir/burndown-cli"
else
  printf 'Cannot write to %s. Set BURNDOWN_INSTALL_DIR to a writable directory.\n' "$install_dir" >&2
  exit 1
fi

printf '\nInstalled %s\n' "$install_dir/burndown-cli"
"$install_dir/burndown-cli" --version
printf '\nNext: run burndown-cli login\n'
