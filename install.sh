#!/bin/sh
# muxdeck installer
#
#   curl -fsSL https://raw.githubusercontent.com/josecarlosrivas/muxdeck/main/install.sh | sh
#
# Environment overrides (all optional):
#   MUXDECK_BIN_DIR   install location            (default /usr/local/bin)
#   MUXDECK_MODE      run | service | binary      (default: ask on a TTY, else binary)
#   MUXDECK_PORT      port for service mode       (default 8300)
set -eu

REPO="josecarlosrivas/muxdeck"
BIN_DIR="${MUXDECK_BIN_DIR:-/usr/local/bin}"
MODE="${MUXDECK_MODE:-}"
PORT="${MUXDECK_PORT:-8300}"

say() { printf '%s\n' "$*"; }
err() { printf 'muxdeck install: %s\n' "$*" >&2; exit 1; }

# --- platform detection ---
os=$(uname -s); arch=$(uname -m)
case "$os" in
  Linux)  os=linux ;;
  Darwin) os=darwin ;;
  *) err "unsupported OS: $os (linux and macOS only)" ;;
esac
case "$arch" in
  x86_64|amd64)  arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) err "unsupported architecture: $arch" ;;
esac

# --- tmux check ---
if command -v tmux >/dev/null 2>&1; then
  say "+ tmux found: $(tmux -V)"
else
  say "! tmux is NOT installed — muxdeck requires it."
  case "$os" in
    linux)  say "  install it first, e.g.: sudo apt install tmux   (or dnf / pacman / apk)" ;;
    darwin) say "  install it first, e.g.: brew install tmux" ;;
  esac
  say "  continuing — muxdeck will work as soon as tmux is present."
fi

# --- download ---
asset="muxdeck-$os-$arch"
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
say "+ downloading $asset (latest release)"
if ! curl -fsSL -o "$tmp/muxdeck" "https://github.com/$REPO/releases/latest/download/$asset"; then
  if command -v gh >/dev/null 2>&1; then
    say "  direct download failed, retrying via gh"
    gh release download -R "$REPO" -p "$asset" -O "$tmp/muxdeck" || err "download failed"
  else
    err "download failed (https://github.com/$REPO/releases)"
  fi
fi
chmod +x "$tmp/muxdeck"

# --- install ---
# BIN_DIR may not exist (fresh macOS has no /usr/local/bin); install(1) won't create it.
if [ ! -d "$BIN_DIR" ]; then
  say "+ creating $BIN_DIR"
  mkdir -p "$BIN_DIR" 2>/dev/null || sudo mkdir -p "$BIN_DIR"
fi
if [ -w "$BIN_DIR" ]; then
  install -m 755 "$tmp/muxdeck" "$BIN_DIR/muxdeck"
else
  say "+ installing to $BIN_DIR (needs sudo)"
  sudo install -m 755 "$tmp/muxdeck" "$BIN_DIR/muxdeck"
fi
say "+ installed: $("$BIN_DIR/muxdeck" -version 2>/dev/null || echo "muxdeck (pre-0.3 release)") at $BIN_DIR/muxdeck"

# --- mode selection ---
if [ -z "$MODE" ] && [ -r /dev/tty ]; then
  say ""
  say "How do you want to run muxdeck?"
  say "  1) Try it now  — foreground on http://127.0.0.1:$PORT, ^C to stop"
  say "  2) Service     — starts at boot, listens on all interfaces, token-protected"
  say "  3) Nothing     — just the binary, I'll run it myself"
  printf 'Choose [1/2/3, default 1]: '
  read -r choice </dev/tty || choice=""
  case "$choice" in
    2) MODE=service ;;
    3) MODE=binary ;;
    *) MODE=run ;;
  esac
fi
[ -n "$MODE" ] || MODE=binary

gen_token() {
  LC_ALL=C tr -dc 'A-HJ-KM-NP-TV-Z2-9' </dev/urandom | head -c 6
}

case "$MODE" in
  binary)
    say ""
    say "Run 'muxdeck' for a local instance, 'muxdeck -h' for all options."
    say "Binding beyond localhost (-addr 0.0.0.0:$PORT) auto-generates an access"
    say "code on startup; use -token for a fixed secret or -no-auth to disable."
    ;;

  run)
    say ""
    exec "$BIN_DIR/muxdeck" -addr "127.0.0.1:$PORT"
    ;;

  service)
    token=$(gen_token)
    if [ "$os" = linux ]; then
      command -v systemctl >/dev/null 2>&1 || err "service mode needs systemd on Linux"
      unit=/etc/systemd/system/muxdeck.service
      [ -e "$unit" ] && say "! $unit already exists — overwriting"
      sudo tee "$unit" >/dev/null <<EOF
[Unit]
Description=muxdeck web terminal for tmux
After=network-online.target

[Service]
User=$(id -un)
Environment=MUXDECK_ADDR=0.0.0.0:$PORT MUXDECK_TOKEN=$token
ExecStart=$BIN_DIR/muxdeck
Restart=on-failure
RestartSec=3
# keep the tmux server (and your sessions) alive across muxdeck restarts
KillMode=process

[Install]
WantedBy=multi-user.target
EOF
      sudo systemctl daemon-reload
      sudo systemctl enable --now muxdeck
      say "+ systemd service 'muxdeck' is running (journalctl -u muxdeck for logs)"
    else
      plist="$HOME/Library/LaunchAgents/com.muxdeck.agent.plist"
      [ -e "$plist" ] && say "! $plist already exists — overwriting"
      mkdir -p "$HOME/Library/LaunchAgents"
      cat >"$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.muxdeck.agent</string>
  <key>ProgramArguments</key><array><string>$BIN_DIR/muxdeck</string></array>
  <key>EnvironmentVariables</key><dict>
    <key>MUXDECK_ADDR</key><string>0.0.0.0:$PORT</string>
    <key>MUXDECK_TOKEN</key><string>$token</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
</dict></plist>
EOF
      launchctl bootout "gui/$(id -u)/com.muxdeck.agent" 2>/dev/null || true
      launchctl bootstrap "gui/$(id -u)" "$plist"
      say "+ launchd agent 'com.muxdeck.agent' is running"
    fi
    say ""
    say "Access token: $token   (kept in the service definition; change it there)"
    say "Listening on port $PORT on all interfaces. Likely addresses:"
    if [ "$os" = linux ]; then
      hostname -I 2>/dev/null | tr ' ' '\n' | sed "/^$/d; s|^|  http://|; s|\$|:$PORT|" || true
    else
      for ifc in en0 en1; do ipconfig getifaddr "$ifc" 2>/dev/null || true; done \
        | sed "s|^|  http://|; s|\$|:$PORT|"
    fi
    say ""
    say "How you expose it is up to you: keep it LAN/VPN-only, front it with a"
    say "reverse proxy, or use built-in TLS (-tls-cert/-tls-key). HTTPS is required"
    say "for installing the PWA on phones/tablets."
    ;;
esac
