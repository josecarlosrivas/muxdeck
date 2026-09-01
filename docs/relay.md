# muxdeck relay (design)

Status: design draft — nothing here is built.

muxdeck today is loopback-first: bind wider than localhost and you get
token auth, or you put the daemon behind your own transport (a VPN,
`tailscale serve`, a reverse proxy). That covers people who already run
that infrastructure. The relay is for everyone else: a hosted rendezvous
that gives a daemon a public HTTPS name with no inbound ports, no VPN,
and no certificates to manage — the ngrok shape, but muxdeck-native, so
the deck, sessions, key bar, and agent runs work out of the box.

## Shape

```
browser / PWA / iOS app ──https/wss──▶ relay ◀──outbound wss── daemon
                                      (public)                 (home/office)
```

- The daemon dials **out** to the relay over wss and keeps the tunnel up
  (reconnect with backoff, heartbeats). The machine running muxdeck
  never accepts an inbound connection.
- Each account gets stable hostnames (`<name>.example-relay.app`) that
  map to its connected daemons. TLS terminates at the relay with real
  certificates; clients see ordinary HTTPS.
- The relay speaks plain muxdeck protocol frames — it forwards the
  daemon's HTTP surface and WebSocket streams as-is. No feature forks:
  anything that works locally works through the relay.

## Authentication

Three layers, replacing daemon tokens for relayed access. Design
constraints that shaped this: embedded WebViews drop third-party
cookies (a framed page cannot rely on a cookie set by its shell — this
broke cookie-token auth in the iOS app), so nothing below depends on
cookies reaching a framed origin.

1. **Account** — passkeys (WebAuthn) as the primary credential;
   username/password + TOTP as the fallback for devices without an
   authenticator. Phishing-resistant, no shared secret for the primary
   path, and registration doubles as the first device enrollment.
2. **Device** — after first sign-in, the client mints a long-lived
   device token ("remember this iPad"), revocable per-device from the
   account page. Daily use is open-and-type; the passkey ceremony is
   for new devices and sensitive actions.
3. **Session** — the page holds a short-lived bearer in its own origin
   (storage, not cookies) and presents it in the WebSocket
   authentication message. Works identically in Safari, the PWA, and
   the wrapped iOS/desktop apps.

Daemon enrollment mirrors the device flow: the daemon is claimed once
with a code shown during setup, then holds its own credential to dial
the relay. Either side can be revoked independently.

## End-to-end mode

The paid differentiator worth building: an optional mode where the
relay is a blind pipe. Client and daemon run a key exchange on top of
the tunnel (keys pinned on first claim, verified out-of-band with a
short fingerprint phrase), and all terminal frames are encrypted
client↔daemon. The relay still does auth, rendezvous, and rate
limiting, but cannot read terminal content. Tunnel providers generally
cannot make that claim about application semantics; a terminal product
can.

Plain mode (TLS to the relay, TLS to the daemon) remains the default —
E2E costs some features that require the relay to inspect frames
(nothing today, but it constrains future server-side rendering), so it
stays a toggle.

## Free and paid

- **Free, forever:** loopback, LAN tokens, bring-your-own transport,
  self-hosted relay (the relay server ships as open source — one binary,
  same as the daemon).
- **Paid:** the hosted relay — stable hostnames, certificates,
  presence, multi-device auth, E2E mode. Recurring infrastructure is
  what the subscription pays for.

Honest scoping: users with a working VPN/tailnet already have transport
(including public sharing via their VPN vendor's own tooling). The
hosted relay's market is the no-VPN crowd: locked-down corp laptops,
quick access from someone else's machine, small teams who will not run
infrastructure.

## Non-goals

- Multi-user shared terminals (sharing a deck is a different feature
  with its own trust model).
- Replacing local auth: loopback and token modes are untouched.
- Relay-side terminal rendering or recording.

## Open questions

- Hostname scheme: per-account (`name.example-relay.app/daemon`) vs
  per-daemon subdomains — affects cert strategy and the picker UX.
- Passkey recovery flow for the fallback-less user who loses their only
  authenticator (recovery codes at minimum).
- Pricing shape: flat per-account vs per-daemon; whether E2E is a tier
  or on by default for all paid accounts.
- Whether the daemon's relay credential lives in the existing config
  file or the OS keychain where one exists.
