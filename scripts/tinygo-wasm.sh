#!/bin/sh
# tinygo-wasm.sh <outdir> <version>
#
# Builds the browser module (the root package for GOOS=js) with TinyGo and
# writes skywire-tinygo.wasm.gz, its manifest skywire-tinygo.wasm.json and the
# wasm_exec_tinygo.js loader that matches the toolchain into outdir.
set -eu
out=$1 ver=$2
tmp=$(mktemp -d)
trap 'rm -f "$tmp/skywire.wasm"; rmdir "$tmp"' EXIT
# wasm-opt recurses deep enough on this module to overflow an 8 MB thread stack.
ulimit -s 1048576 2>/dev/null || ulimit -s unlimited
tinygo build -target=wasm -no-debug -stack-size=256KB -interp-timeout=10s \
	-tags "withoutsystray withoutgotop" \
	-ldflags "-X github.com/skycoin/skywire/pkg/buildinfo.version=$ver" \
	-o "$tmp/skywire.wasm" .
ls -l "$tmp/skywire.wasm"
sh scripts/wasm-module.sh "$tmp/skywire.wasm" "$out" "$ver" skywire-tinygo
cp "$(tinygo env TINYGOROOT)/targets/wasm_exec.js" "$out/wasm_exec_tinygo.js"
