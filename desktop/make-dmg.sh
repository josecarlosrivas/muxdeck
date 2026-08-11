#!/bin/sh
# Package the built muxdeck.app into a distributable DMG.
# Uses plain hdiutil (no Finder automation), so it works headless and in CI —
# Tauri's own dmg target needs a GUI session for icon layout.
set -eu

cd "$(dirname "$0")"
APP=src-tauri/target/release/bundle/macos/muxdeck.app
[ -d "$APP" ] || { echo "make-dmg: $APP not found — run 'npx tauri build' first" >&2; exit 1; }

VERSION=$(sed -n 's/.*"version": "\([^"]*\)".*/\1/p' src-tauri/tauri.conf.json | head -1)
ARCH=$(uname -m)
OUT="muxdeck_${VERSION}_${ARCH}.dmg"

STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT
cp -R "$APP" "$STAGE/"
ln -s /Applications "$STAGE/Applications"

hdiutil create -volname muxdeck -srcfolder "$STAGE" -ov -format UDZO -fs HFS+ "$OUT" >/dev/null
echo "built $OUT"
