# muxdeck relay (design)

Status: design draft — nothing here is built.

muxdeck today is loopback-first: bind wider than localhost and you get
token auth, or you put the daemon behind your own transport (a VPN,
`tailscale serve`, a reverse proxy). That covers people who already run
that infrastructure. A relay covers everyone else: a rendezvous server
that gives a daemon a public HTTPS name with no inbound ports, no VPN,
and no certificates on the home machine — and because it speaks plain
muxdeck protocol, the deck, sessions, key bar, and agent runs work out
of the box.

This document covers the open parts: the tunnel client that ships in
the daemon, the relay protocol, and a minimal self-hostable relay
server. A hosted, managed relay service is planned as a separate
product and specified elsewhere.

## Shape

```
browser / PWA / iOS app ──https/wss──▶ relay ◀──outbound wss── daemon
                                      (public)                 (home/office)
```

- The daemon dials **out** to the relay over wss and keeps the tunnel up
  (reconnect with backoff, heartbeats). The machine running muxdeck
  never accepts an inbound connection.
- TLS terminates at the relay; clients see ordinary HTTPS on a stable
  hostname that maps to the connected daemon.
- The relay forwards the daemon's HTTP surface and WebSocket streams
  as-is. No feature forks: anything that works locally works through
  the relay.

## Tunnel client (in the daemon)

- Enabled by configuration (relay URL + credential); loopback and token
  modes are untouched.
- The daemon is claimed once with a code shown during setup, then holds
  its own credential to dial the relay. The credential is revocable
  server-side; the daemon handles revocation as a permanent
  disconnect, not a retry loop.
- Sessions authenticate with a bearer presented in the WebSocket
  authentication message, held by the page in its own origin (storage,
  not cookies). Embedded WebViews drop third-party cookies — a framed
  page cannot rely on a cookie set by its shell — so nothing in the
  relay path depends on cookies.

## Self-hosted relay

The relay server ships as open source: one binary, same as the daemon.
Single-tenant by design — your hostname, your certificate (or ACME),
your accounts. It exists so that running your own rendezvous requires
no more infrastructure than running muxdeck itself.

## End-to-end mode

An optional mode where the relay is a blind pipe: client and daemon run
a key exchange on top of the tunnel (keys pinned on first claim,
verified out-of-band with a short fingerprint phrase), and all terminal
frames are encrypted client↔daemon. The relay still does auth,
rendezvous, and rate limiting, but cannot read terminal content.

Plain mode (TLS to the relay, TLS to the daemon) remains the default —
E2E constrains any future feature that would require the relay to
inspect frames, so it stays a toggle.

## Non-goals

- Multi-user shared terminals (sharing a deck is a different feature
  with its own trust model).
- Replacing local auth: loopback and token modes are untouched.
- Relay-side terminal rendering or recording.

## Open questions

- Hostname scheme: per-account paths vs per-daemon subdomains —
  affects cert strategy and the picker UX.
- Whether the daemon's relay credential lives in the existing config
  file or the OS keychain where one exists.
- Protocol versioning between daemon and relay across upgrades.
