#!/bin/bash
# Builds ikemen/bin/ikemen.dll (Go -buildmode=c-shared, build tag "overlay") with MSYS2
# clang64 and copies every DLL it needs from /clang64/bin next to it (SDL2, libxmp and their dependencies; the overlay
# build has no FFmpeg - stage videos are stubbed out, src/host_ov_novideo.go).
# Run from Windows: ikemen/build/build_dll.cmd (MSYS2 at C:/msys64 with the clang64 packages go, clang, pkgconf, SDL2, libxmp)
set -e
export PATH=/clang64/bin:/usr/bin:$PATH
export GOROOT=/clang64/lib/go
export CC=clang CXX=clang++ CGO_ENABLED=1 GOEXPERIMENT=arenas
export GOPATH="${GOPATH:-/c/Users/${USERNAME:-$(whoami)}/go}"
export GOCACHE="${GOCACHE:-/c/Users/${USERNAME:-$(whoami)}/AppData/Local/go-build}"
ENGINE="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ENGINE/bin"
mkdir -p "$BIN"
BUILD_DIR="$(cygpath -u "${IKEMEN_BUILD_DIR:-$BIN/.cache}")"
mkdir -p "$BUILD_DIR/go-path" "$BUILD_DIR/go-cache" "$BUILD_DIR/go-mod" "$BUILD_DIR/tmp"
export GOPATH="$BUILD_DIR/go-path" GOCACHE="$BUILD_DIR/go-cache" GOMODCACHE="$BUILD_DIR/go-mod"
export GOTMPDIR="$BUILD_DIR/tmp" TEMP="$BUILD_DIR/tmp" TMP="$BUILD_DIR/tmp"
export GOTOOLCHAIN=local GOFLAGS=-mod=readonly
export PKG_CONFIG=pkg-config
export CGO_CFLAGS="$(pkg-config --cflags libxmp sdl2)"
export CGO_LDFLAGS="$(pkg-config --libs libxmp sdl2)"
go -C "$ENGINE" build -p 2 -tags overlay -buildmode=c-shared -trimpath \
	-ldflags "-s -w -X 'main.Version=v1.0.0-godotmen' -X 'main.BuildTime=$(date +%F)'" \
	-o "$BIN/ikemen.dll" ./src
echo "built $BIN/ikemen.dll"

# Every non-system DLL ikemen.dll imports, recursively, copied from /clang64/bin
declare -A seen
copy_deps() {
	local file="$1" dll
	for dll in $(objdump -p "$file" | sed -n 's/^\s*DLL Name: //p' | tr -d '\r'); do
		[ -n "${seen[$dll]}" ] && continue
		seen[$dll]=1
		if [ -f "/clang64/bin/$dll" ]; then
			cp -u "/clang64/bin/$dll" "$BIN/"
			echo "  dep $dll"
			copy_deps "/clang64/bin/$dll"
		fi
	done
	return 0
}
copy_deps "$BIN/ikemen.dll"
