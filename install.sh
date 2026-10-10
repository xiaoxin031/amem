#!/bin/sh
# amem installer: builds from a local checkout when Go is available, otherwise
# downloads the latest release, then wires Claude Code / Codex.
set -eu

repo=xiaoxin031/amem
dir=${AMEM_BIN:-$HOME/.local/bin}
src=$(CDPATH= cd -- "$(dirname -- "$0")" 2>/dev/null && pwd || true)
mkdir -p "$dir"

if [ -n "$src" ] && [ -f "$src/go.mod" ] && command -v go >/dev/null 2>&1; then
  echo "building from $src"
  (cd "$src" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$dir/amem.tmp" .)
else
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  case $(uname -m) in
    x86_64|amd64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) echo "unsupported CPU: $(uname -m)" >&2; exit 1 ;;
  esac
  url="https://github.com/$repo/releases/latest/download/amem_${os}_${arch}"
  echo "downloading $url"
  if command -v curl >/dev/null 2>&1; then curl -fsSL "$url" -o "$dir/amem.tmp"
  else wget -qO "$dir/amem.tmp" "$url"; fi
fi
chmod +x "$dir/amem.tmp"
mv "$dir/amem.tmp" "$dir/amem"
echo "installed $dir/amem"

"$dir/amem" install || echo "Claude Code / Codex not found yet: run 'amem install' after installing one."

case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "tip: add $dir to PATH to use 'amem' in your shell (hooks work without it)" ;;
esac
