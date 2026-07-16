# Agent integrations

muxdeck can show live status badges — state, model, spend — next to sessions
that run AI coding agents, and raise a browser notification the moment an
agent blocks on human input.

The core is agent-agnostic: muxdeck never parses terminal content. Agents
self-report over one endpoint, and thin per-agent adapters do the reporting.

## The ingest API

```
POST /api/agent/status
{
  "session":  "myproject",        # tmux session name (required)
  "agent":    "claude-code",      # reporting tool (required)
  "state":    "working",          # working | waiting | idle (required)
  "model":    "Opus 4.8",         # optional
  "cost_usd": 4.20,               # optional
  "note":     "needs permission"  # optional, shown in the badge tooltip
}
```

Same auth as the rest of the API (`Authorization: Bearer <token>`, not needed
on loopback binds). Status is in-memory and vanishes with the session.

`state: "waiting"` is the important one — the UI fires a notification on the
transition into it (enable via the bell button in the sidebar). Page
notifications don't exist on iOS Safari; Web Push is a planned follow-up.

## Claude Code

`claude-code.sh` (requires `jq`) reports via Claude Code's statusline and
hooks. See the header comment for the `~/.claude/settings.json` wiring:
the statusline pipe carries model + cumulative cost every turn, and the
`UserPromptSubmit` / `Notification` / `Stop` hooks map to
`working` / `waiting` / `idle`.

## Other agents

Any tool that can run a shell command on state changes can report — one
`curl` per transition. Codex CLI's `notify` hook is the obvious next
adapter; contributions welcome.
