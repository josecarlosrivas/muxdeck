# muxdeck

A single-binary web app for creating, managing, and interacting with tmux
sessions from the browser. Serves a full terminal (xterm.js) attached to any
tmux session over WebSocket, plus a small management UI.

Works on any Linux or macOS machine that has `tmux` installed.

## Build

```sh
go build -o muxdeck .
```

The web frontend (including vendored xterm.js) is embedded in the binary —
deploying is copying one file.

## Run

```sh
./muxdeck                          # http://127.0.0.1:8300, no auth
./muxdeck -addr 0.0.0.0:8300       # listen on all interfaces
./muxdeck -addr 0.0.0.0:8300 -token s3cret   # require an access token
```

Environment variables `MUXDECK_ADDR` and `MUXDECK_TOKEN` are read as defaults
for the corresponding flags.

muxdeck talks to the default tmux server of the user it runs as. Sessions
created and killed in the UI are real tmux sessions — anything you can do in
`tmux attach` works in the browser terminal, including multiple simultaneous
viewers of one session.

## Security model

A browser terminal is remote code execution by design. Deploy accordingly:

- **Trusted network (e.g. a tailnet): ** bind to the tailnet interface (or
  `0.0.0.0` on a machine only reachable over it) and rely on network access
  control. Auth optional.
- **Anything less trusted:** set `-token` (or `MUXDECK_TOKEN`) and put muxdeck
  behind TLS (a reverse proxy such as Caddy, or `tailscale serve`). The token
  gates all `/api` routes; the login form sets an HttpOnly cookie, and
  non-browser clients can send `Authorization: Bearer <token>`.
- WebSocket upgrades enforce a same-host `Origin` check to block cross-site
  hijacking.

Never expose muxdeck directly to the public internet without both TLS and a
token.

## API

| Method | Path                          | Description                     |
|--------|-------------------------------|---------------------------------|
| POST   | `/api/login`                  | `{"token"}` → HttpOnly cookie   |
| GET    | `/api/sessions`               | list sessions                   |
| POST   | `/api/sessions`               | `{"name"}` → create (detached)  |
| DELETE | `/api/sessions/{name}`        | kill session                    |
| POST   | `/api/sessions/{name}/rename` | `{"name"}` → rename             |
| GET    | `/api/sessions/{name}/attach` | WebSocket terminal bridge       |

Attach protocol: client sends JSON text frames `{"type":"input","data":"…"}`
and `{"type":"resize","cols":N,"rows":N}`; server sends raw terminal output as
binary frames.

Session names are restricted to `[A-Za-z0-9_-]{1,64}`.
