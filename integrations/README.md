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
  "note":     "needs permission", # optional, shown in the badge tooltip
  "progress": {"value": 0.4, "label": "migrating"},   # optional, see below
  "chips":    [                                       # optional, see below
    {"key": "tests", "value": "12/14", "icon": "flask", "color": "warn"}
  ]
}
```

Same auth as the rest of the API (`Authorization: Bearer <token>`, not needed
on loopback binds). Status is in-memory and vanishes with the session.

### Progress and chips

`progress` is a fraction of one (clamped) plus a label naming what is being
worked through. It renders as a bar under the session row.

`chips` are the agent's own facts — whatever it thinks is worth a glance.
muxdeck renders them and never interprets them:

- `key` — what the fact is; shown in the tooltip.
- `value` — the text on the chip. A chip without one is dropped.
- `icon` — optional, one of `check` `alert` `clock` `flask` `coins` `file`
  `branch` `plug`.
- `color` — optional, one of `accent` `warn` `danger` `dim`.

Icon and color are closed sets: a chip is drawn into muxdeck's own sidebar, so
an unknown icon would render nothing and an arbitrary CSS color would fight
the theme around it. An unrecognised value is dropped and the rest of the chip
still shows — a cosmetic field should not cost an agent its whole status.
Up to 6 chips, 24 characters each; labels 60, notes 200.

### What a post leaves out

One session is usually reported by more than one caller: a statusline that
knows the model, the spend and the progress, and hooks that know only that the
state changed. So **a field left out of a post keeps the value the last post
gave it**, and blanking something is said explicitly:

- `"progress": {}` — nothing in progress.
- `"chips": []` — no chips.

The rule is per reporter. A post under a different `agent` starts clean, since
one agent taking over a session says nothing about the last one's numbers.

`state: "waiting"` is the important one — the UI fires a notification on the
transition into it (enable via the bell button in the sidebar). Page
notifications don't exist on iOS Safari; Web Push is a planned follow-up.

## Claude Code

`claude-code.sh` (requires `jq`) reports via Claude Code's statusline and
hooks. See the header comment for the `~/.claude/settings.json` wiring:
the statusline pipe carries model + cumulative cost every turn, and the
`UserPromptSubmit` / `Notification` / `Stop` hooks map to
`working` / `waiting` / `idle`.

The statusline pipe also carries the lines the session has added and removed,
which the adapter sends as a `diff` chip — a worked example of the chip shape
and the one number that says how much a long agent run has actually touched.

## Other agents

Any tool that can run a shell command on state changes can report — one
`curl` per transition. Codex CLI's `notify` hook is the obvious next
adapter; contributions welcome.
