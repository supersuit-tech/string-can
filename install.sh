#!/bin/sh
# Install the latest string-can release, or print build instructions if none exists.
set -e

REPO="${STRING_CAN_REPO:-supersuit-tech/string-can}"
DEST="${STRING_CAN_INSTALL_DIR:-/usr/local/bin}"
VERSION="${1:-latest}"

os=$(uname -s | tr 'A-Z' 'a-z')
arch=$(uname -m)
case "$arch" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*)
	echo "install.sh: unsupported architecture: $arch" >&2
	exit 1
	;;
esac
case "$os" in
linux | darwin) ;;
*)
	echo "install.sh: unsupported OS: $os" >&2
	exit 1
	;;
esac

asset="string-can-${os}-${arch}"

api="https://api.github.com/repos/${REPO}/releases/latest"
if [ "$VERSION" != "latest" ]; then
	api="https://api.github.com/repos/${REPO}/releases/tags/${VERSION}"
fi

echo "Looking up ${VERSION} from ${REPO}..." >&2
json=$(curl -fsSL "$api") || {
	echo "No GitHub release found. Install Go 1.22+ and run:" >&2
	echo "  git clone https://github.com/${REPO}.git" >&2
	echo "  cd string-can && CGO_ENABLED=0 go build -o string-can ." >&2
	exit 1
}

url=$(printf '%s\n' "$json" | sed -n "s/.*\"browser_download_url\": \"\\([^\"]*${asset}\\)\".*/\\1/p" | head -n 1)
if [ -z "$url" ]; then
	echo "No asset ${asset} in that release. Install Go 1.22+ and build from source:" >&2
	echo "  CGO_ENABLED=0 go build -o string-can ." >&2
	exit 1
fi

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
curl -fsSL "$url" -o "$tmp"
chmod +x "$tmp"

if [ -w "$DEST" ] || [ "$(id -u)" -eq 0 ]; then
	mv "$tmp" "${DEST}/string-can"
	trap - EXIT
	echo "Installed ${DEST}/string-can"
else
	mkdir -p "${HOME}/.local/bin"
	mv "$tmp" "${HOME}/.local/bin/string-can"
	trap - EXIT
	echo "Installed ${HOME}/.local/bin/string-can"
	echo "Add \$HOME/.local/bin to PATH if needed."
fi
