#!/bin/sh
# Install pocketcaddy from its GitHub releases.
#
#   curl -fsSL https://raw.githubusercontent.com/tychoengineering/pocketcaddy/main/install.sh | sh
#
# Set POCKETCADDY_VERSION to pin a release, and INSTALL_DIR to choose where
# the binary lands.
set -eu

REPO="tychoengineering/pocketcaddy"
BIN="pocketcaddy"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

die() {
	echo "install: $1" >&2
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"
}

# Resolve the platform against the targets we publish.
os=$(uname -s)
arch=$(uname -m)
case "$os-$arch" in
Darwin-arm64) target="darwin_arm64" ;;
Linux-x86_64) target="linux_amd64" ;;
Linux-aarch64 | Linux-arm64) target="linux_arm64" ;;
Darwin-x86_64)
	die "macOS on Intel is not a published target.
Build from source instead: go install github.com/$REPO/cmd/$BIN@latest"
	;;
*)
	die "unsupported platform $os/$arch.
Published targets are macOS arm64, Linux amd64, and Linux arm64.
Build from source instead: go install github.com/$REPO/cmd/$BIN@latest"
	;;
esac

need curl
need tar

# Resolve the version from the releases API. A HEAD request against
# /releases/latest answers 404 even when the release exists, so ask the API
# with a GET rather than following a redirect.
version="${POCKETCADDY_VERSION:-}"
if [ -z "$version" ]; then
	version=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null |
		sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' |
		head -1)
	[ -n "$version" ] || die "could not determine the latest version.
The API returned no release for $REPO. If the repository is private,
download the archive manually or set POCKETCADDY_VERSION."
fi
# Accept both "v0.1.0" and "0.1.0"; archive names carry no leading v.
num_version="${version#v}"

archive="${BIN}_${num_version}_${target}.tar.gz"
base="https://github.com/$REPO/releases/download/$version"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "Downloading $BIN $version for $target..."
curl -fsSL "$base/$archive" -o "$tmp/$archive" ||
	die "download failed. Does $version have a $target build?"

# Verify against the release checksums. A corrupted or tampered archive
# fails here rather than at first run.
if curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt" 2>/dev/null; then
	expected=$(grep " $archive\$" "$tmp/checksums.txt" | awk '{print $1}')
	if [ -n "$expected" ]; then
		if command -v sha256sum >/dev/null 2>&1; then
			actual=$(sha256sum "$tmp/$archive" | awk '{print $1}')
		elif command -v shasum >/dev/null 2>&1; then
			actual=$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')
		else
			actual=""
		fi
		if [ -n "$actual" ] && [ "$actual" != "$expected" ]; then
			die "checksum mismatch for $archive
  expected $expected
  actual   $actual"
		fi
		[ -n "$actual" ] && echo "Checksum verified."
	fi
else
	echo "install: warning: no checksums.txt, skipping verification" >&2
fi

tar -xzf "$tmp/$archive" -C "$tmp" || die "could not extract $archive"
[ -f "$tmp/$BIN" ] || die "$BIN not found in the archive"
chmod +x "$tmp/$BIN"

# Write directly when we own the directory, and escalate only when we do not.
if [ -w "$INSTALL_DIR" ]; then
	mv "$tmp/$BIN" "$INSTALL_DIR/$BIN"
elif [ -d "$INSTALL_DIR" ] || mkdir -p "$INSTALL_DIR" 2>/dev/null; then
	if [ -w "$INSTALL_DIR" ]; then
		mv "$tmp/$BIN" "$INSTALL_DIR/$BIN"
	else
		echo "$INSTALL_DIR needs elevated permissions."
		need sudo
		sudo mv "$tmp/$BIN" "$INSTALL_DIR/$BIN"
	fi
else
	echo "$INSTALL_DIR needs elevated permissions."
	need sudo
	sudo mkdir -p "$INSTALL_DIR"
	sudo mv "$tmp/$BIN" "$INSTALL_DIR/$BIN"
fi

echo "Installed $BIN to $INSTALL_DIR/$BIN"

if command -v "$BIN" >/dev/null 2>&1; then
	echo
	"$BIN" version
else
	echo
	echo "$INSTALL_DIR is not on your PATH. Add it with:"
	echo "  export PATH=\"$INSTALL_DIR:\$PATH\""
fi
