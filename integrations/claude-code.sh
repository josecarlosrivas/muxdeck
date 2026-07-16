#!/bin/sh
# muxdeck adapter for Claude Code: reports model, spend, and state to the
# muxdeck daemon so the session list can show live agent badges and alert
# when the agent is blocked on input.
#
# Wire it into ~/.claude/settings.json (paths adjusted to your checkout):
#
#   "statusLine": { "type": "command",
#     "command": "/path/to/claude-code.sh statusline" },
#   "hooks": {
#     "UserPromptSubmit": [{ "hooks": [{ "type": "command",
#       "command": "/path/to/claude-code.sh state working" }] }],
#     "Notification": [{ "hooks": [{ "type": "command",
#       "command": "/path/to/claude-code.sh state waiting" }] }],
#     "Stop": [{ "hooks": [{ "type": "command",
#       "command": "/path/to/claude-code.sh state idle" }] }]
#   }
#
# Env: MUXDECK_URL (default http://127.0.0.1:8300), MUXDECK_TOKEN (optional).
# Requires jq. Exits quietly when not inside tmux or when muxdeck is down —
# it must never break the agent it reports on.
set -eu

MUXDECK_URL="${MUXDECK_URL:-http://127.0.0.1:8300}"

session=""
[ -n "${TMUX:-}" ] && session=$(tmux display-message -p '#S' 2>/dev/null) || true

post() {
  [ -n "$session" ] || return 0
  curl -sf --max-time 1 -X POST "$MUXDECK_URL/api/agent/status" \
    ${MUXDECK_TOKEN:+-H "Authorization: Bearer $MUXDECK_TOKEN"} \
    -H 'Content-Type: application/json' \
    -d "$1" >/dev/null 2>&1 || true
}

json_escape() { printf '%s' "$1" | jq -Rr @json; }

case "${1:-}" in
statusline)
  # Claude Code pipes status JSON on stdin every turn; the line we print is
  # what the terminal shows. Model + cost also go to muxdeck as "working".
  input=$(cat)
  model=$(printf '%s' "$input" | jq -r '.model.display_name // empty')
  cost=$(printf '%s' "$input" | jq -r '.cost.total_cost_usd // 0')
  post "{\"session\":$(json_escape "$session"),\"agent\":\"claude-code\",\"state\":\"working\",\"model\":$(json_escape "$model"),\"cost_usd\":$cost}"
  printf '%s · $%.2f\n' "${model:-claude}" "$cost"
  ;;
state)
  st="${2:?usage: claude-code.sh state working|waiting|idle}"
  # Hook input JSON arrives on stdin; the Notification hook's message says
  # what the agent is blocked on — surface it as the badge note.
  note=""
  if [ ! -t 0 ]; then
    note=$(cat | jq -r '.message // empty' 2>/dev/null | head -c 200) || true
  fi
  post "{\"session\":$(json_escape "$session"),\"agent\":\"claude-code\",\"state\":\"$st\",\"note\":$(json_escape "$note")}"
  ;;
*)
  echo "usage: claude-code.sh statusline | state working|waiting|idle" >&2
  exit 2
  ;;
esac
