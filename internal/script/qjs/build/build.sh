#!/usr/bin/env bash
# Builds the QuickJS reactor (testdata/qjs.wasm) from the pinned sources.
# Usage: build.sh [workdir] <output.wasm>
# Env:   WASI_SDK      existing wasi-sdk root (skips download)
#        WASI_CACHE_DIR where the verified wasi-sdk tarball is kept
#                      (default ${XDG_CACHE_HOME:-$HOME/.cache}/kora-wasi-sdk)
# See README.md in this directory.
set -euo pipefail

WASI_VERSION=34.0
WASI_SHA256=b761e3a0721dbae9c09a0059e5fdb2bf917d1b4a8a7b430fb3b5aafb0984b2c4
WASI_TARBALL=wasi-sdk-${WASI_VERSION}-x86_64-linux.tar.gz
WASI_URL=https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-34/${WASI_TARBALL}
QJS_TAG=v0.18.0
QJS_URL=https://github.com/quickjs-ng/quickjs.git

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [ $# -lt 1 ] || [ $# -gt 2 ]; then
	echo "usage: build.sh [workdir] <output.wasm>" >&2
	exit 2
fi
if [ $# -eq 2 ]; then
	work="$1"
	out="$2"
else
	work="$(mktemp -d)"
	out="$1"
fi
mkdir -p "$work"
work="$(cd "$work" && pwd)"
out="$(realpath -m "$out")"

if [ -z "${WASI_SDK:-}" ]; then
	cache="${WASI_CACHE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/kora-wasi-sdk}"
	mkdir -p "$cache"
	tarball="$cache/$WASI_TARBALL"
	if [ ! -f "$tarball" ] || ! echo "$WASI_SHA256  $tarball" | sha256sum -c --status; then
		curl -fsSL -o "$tarball.part" "$WASI_URL"
		mv "$tarball.part" "$tarball"
	fi
	echo "$WASI_SHA256  $tarball" | sha256sum -c -
	WASI_SDK="$cache/wasi-sdk-${WASI_VERSION}-x86_64-linux"
	if [ ! -x "$WASI_SDK/bin/clang" ]; then
		tar -xzf "$tarball" -C "$cache"
	fi
fi
if [ ! -x "$WASI_SDK/bin/clang" ]; then
	echo "build.sh: no clang under WASI_SDK=$WASI_SDK" >&2
	exit 1
fi

# Fresh clone every run so a reused workdir cannot leak stale sources.
rm -rf "$work/quickjs"
git clone --depth 1 --branch "$QJS_TAG" "$QJS_URL" "$work/quickjs"
cp "$here/qjs_reactor.c" "$work/quickjs/"

# Compile from inside the source tree so __FILE__ paths stay relative.
cd "$work/quickjs"
"$WASI_SDK/bin/clang" --target=wasm32-wasip1 \
	--sysroot="$WASI_SDK/share/wasi-sysroot" \
	-mexec-model=reactor -O2 -DQUICKJS_NG_BUILD \
	-Wl,-z,stack-size=1048576 \
	-o qjs.wasm \
	qjs_reactor.c quickjs.c dtoa.c libregexp.c libunicode.c -lm

# The linker embeds the output basename in the name section, so link under a
# fixed name and copy: the result must not depend on where or as what it is written.
cp qjs.wasm "$out"
sha256sum "$out"
