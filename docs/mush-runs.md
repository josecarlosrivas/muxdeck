# Agent runs (`:mush`)

muxdeck can host [mush](https://github.com/josecarlosrivas/mush) agent runs as
first-class panes. muxdeck is a *protocol client*: it spawns `mush stdio` per
run, streams the engine's NDJSON events into a rendered pane, and sends
protocol commands back — most importantly approval decisions, so a run started
with the conservative `ask` policy can be supervised from any device, including
through the remotes proxy.

## Setup

The feature activates when a `mush` binary is found on the daemon's `PATH`
(override with `MUXDECK_MUSH_BIN`). Without one, the endpoints report
`available: false` and the UI stays unchanged. Run configuration comes from the
target repo's `.mush/settings.json`; muxdeck always starts runs with
`-approve ask`.

## Use

Focus a session whose working directory is the repo you want the agent in,
then `⌘K` → `:mush` → type the task. The run opens in the opposite pane:
streamed text, collapsible reasoning, tool cards, verification verdicts, and —
when the engine pauses on a risky action — an approve/deny card. The pane's ↻
reconnects the stream; the full run replays from the daemon's ring buffer
(4 MiB per run, oldest frames trimmed).

## API

- `GET /api/mush/runs` — `{available, runs: [{id, task, dir, state, steps, started_at}]}`
- `POST /api/mush/runs` — `{task, session}` (working dir = the session's
  active pane) or `{task, dir}`
- `GET /api/mush/runs/{id}/stream` — WebSocket: replay + live protocol events;
  send `approval_response` / `user_turn` / `interrupt` frames back
- `DELETE /api/mush/runs/{id}` — interrupt, then terminate

States: `running`, `awaiting_approval`, `done`, `failed`, `interrupted`.
Runs do not survive daemon restarts. Frames muxdeck adds to the stream (not
protocol events): `_exit {state}`, `_trimmed`, `_error {error}`.

## Engine credentials

Provider keys can live in `mush.env` (KEY=VALUE lines, `#` comments) in the
muxdeck config dir — the same directory as `remotes.json`. Spawned engines get
these on top of the daemon's environment, and the file is re-read per run.
This exists because daemons run under launchd/systemd with a bare environment:
shell rc files never apply. Per-repo `.mush/.env` still works and wins for
repo-specific configuration.
