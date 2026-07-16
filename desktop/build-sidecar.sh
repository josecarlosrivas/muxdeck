#!/bin/sh
# Build the muxdeck Go daemon as a Tauri sidecar binary.
# Tauri expects externalBin files suffixed with the Rust target triple.
set -eu

cd "$(dirname "$0")/.."
VERSION=$(git describe --tags --always)
TRIPLE=$(rustc -vV | sed -n 's/^host: //p')

mkdir -p desktop/src-tauri/binaries
go build -ldflags "-X main.version=$VERSION" \
    -o "desktop/src-tauri/binaries/muxdeck-$TRIPLE" .
echo "built desktop/src-tauri/binaries/muxdeck-$TRIPLE ($VERSION)"
