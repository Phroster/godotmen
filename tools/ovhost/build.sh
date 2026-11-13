#!/bin/bash
# Builds ikemen/bin/ovhost.exe, a native test host for ikemen.dll (no Godot needed), with MSYS2 clang64.
set -e
export PATH=/clang64/bin:/usr/bin:$PATH
HERE="$(cd "$(dirname "$0")" && pwd)"
BIN="$HERE/../../ikemen/bin"
mkdir -p "$BIN/.cache/tmp"
export TMP="$BIN/.cache/tmp" TEMP="$BIN/.cache/tmp"
clang -O2 -Wall -Wno-unused-function -Wno-unused-variable "$HERE/ovhost.c" -o "$BIN/ovhost.exe" \
	-lopengl32 -lgdi32 -luser32 -lpsapi -Wl,-Bstatic -lz -Wl,-Bdynamic
echo "built $BIN/ovhost.exe"
