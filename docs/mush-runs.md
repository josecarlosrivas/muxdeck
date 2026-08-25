# Agent runs (`:mush`, `:runs`)

muxdeck is a *viewer and starter* of [mush](https://github.com/josecarlosrivas/mush)
runs. mush owns every run — a row in its per-user ledger (`~/.mush/runs.db`),
a replayable journal (`.mush/runs/<id>.jsonl` in the project) and, for a
checkout a `mush serve` daemon owns, the work queue and the approval broker
behind `serve.sock`. muxdeck reads what mush records and hands new work to
mush's own front door. Runs therefore survive muxdeck restarts, show up
whether muxdeck started them or a schedule / issue did, and a checkout is
never worked by two engines at once.

## Setup

The feature activates when a `mush` binary is found on the daemon's `PATH`
(override with `MUXDECK_MUSH_BIN`). Without one, the endpoints report
`available: false` and the UI stays unchanged. The daemon reads the same
ledger and socket as the `mush` CLI of the user it runs as (`$MUSH_HOME`,
default `~/.mush`; a `MUSH_HOME=` line in `mush.env` also works).

## Use

- **Runs** section in the sidebar — the ledger, newest first, across the
  local daemon and every live remote. State dot: blue pulsing = live,
  amber = waiting for an approval, green = done / PR open / merged,
  red = failed / interrupted / blocked. Click a row to open it; click the
  header to collapse the section.
- `⌘K` → `:runs` — the same list as a fuzzy picker.
- `⌘K` → `:mush` → type the task — starts a run in the focused session's
  working directory (`-m <model>` prefix overrides the model). If a
  `mush serve` owns that checkout the run lands in its queue; otherwise the
  daemon spawns an engine there. Either way the run opens in the opposite
  pane. Runs muxdeck starts always use the conservative `ask` approval
  policy.
- **Run pane** — the journal replayed from the top, then tailed live:
  streamed text, collapsible reasoning, tool cards, verdicts, state changes.
  The header carries the ledger row (project, model, steps, tokens, cost,
  duration, verdict, branch) and the actions mush offers: **open PR**,
  **stop** (engines this daemon spawned), **retry** (`mush run --parent`)
  and, for a failed / interrupted / blocked run, **resume** (`mush resume`,
  which continues from the last checkpoint as a new run).
- **Approvals** — when a run pauses on a risky action the pane shows an
  approve / deny card. The answer goes to whoever owns the run: over stdin
  for an engine this daemon spawned, over `serve.sock` (`approve
  {run_id, approved}`) for a served one. A decision taken elsewhere — the
  CLI's `mush approve`, another viewer — retires the card when its
  `approval_resolved` event reaches the journal.

Through the remotes proxy all of this lands on the daemon that owns the
session, so a run on a workstation can be watched and approved from a phone.

## API

- `GET /api/mush/runs?project=&limit=` — `{available, models, serve: {live,
  socket, projects: [{project, current, queued, queue, awaiting}]}, runs:
  [row…]}`; `row` is the ledger row as `mush runs --json` prints it plus
  `local` (this daemon spawned the engine). `models` is the advisory id
  list from a `MUXDECK_MUSH_MODELS` line in `mush.env`.
- `POST /api/mush/runs` — `{task, session}` (working dir = the session's
  active pane) or `{task, dir}`; optional `model`. Returns the row.
- `GET /api/mush/runs/{id}/stream` — WebSocket. First frame `_run {row}`,
  then the journal's events in order, then live tail; `_state {row}` on
  every ledger transition and `_exit {state, error}` once the row is
  terminal and the journal has drained. Send `approval_response` to answer
  a pending approval, `interrupt` to stop an engine this daemon spawned.
  Checkpoint frames are reduced to `{step}` — the engine's state blob never
  crosses the wire.
- `POST /api/mush/runs/{id}/retry` — `mush run --parent {id}` with the
  original task; returns the child row.
- `POST /api/mush/runs/{id}/resume` — `mush resume {id}` in the project;
  the resumed run appears in the ledger under a new id.
- `DELETE /api/mush/runs/{id}` — interrupt an engine this daemon spawned
  (served runs are stopped from the serve host).

Frames muxdeck adds to the stream, not protocol events: `_run`, `_state`,
`_exit`, `_error {error}`.

## Engine credentials

Provider keys can live in `mush.env` (KEY=VALUE lines, `#` comments) in the
muxdeck config dir — the same directory as `remotes.json`. Engines and
`mush` invocations get these on top of the daemon's environment, and the
file is re-read per use. This exists because daemons run under
launchd/systemd with a bare environment: shell rc files never apply.
Per-repo `.mush/.env` still works and wins for repo-specific configuration.
A served run uses the serve's own environment.

## The 0.11 host

`MUXDECK_MUSH_LEGACY=1` restores the previous design for one release:
muxdeck spawns `mush stdio` per run and keeps the event stream in a 4 MiB
ring buffer per run. Runs then live in daemon memory (no ledger row of their
own is required, no `:runs`, no retry/resume) and do not survive restarts.
It exists for a rollback path while the viewer settles and goes away in the
release after.
