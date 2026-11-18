#!/bin/sh
# Reproducible build of the forge binary.
# Static, stdlib-only Go binary, symbols stripped, trimmed paths.
set -eu
cd "$(dirname "$0")/forge-src"
GO="${GO:-$HOME/go-toolchain/go/bin/go}"
"$GO" build -trimpath -ldflags="-s -w" -o ../forge .
ls -l ../forge
