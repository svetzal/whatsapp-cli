# Fork Changelog

Tracks changes carried in this fork (`svetzal/whatsapp-cli`) on top of
upstream (`vicentereig/whatsapp-cli`). The base project's history lives in
[`CHANGELOG.md`](CHANGELOG.md); this file only records work that has not
(yet) landed upstream.

When an entry lands upstream — either through a PR back to
`vicentereig/whatsapp-cli` or because upstream independently shipped the
same fix — remove it from this file rather than versioning it into
upstream's changelog. Use date headings; the fork does not carry its own
version numbers.

## 2026-06-06

### Changed

- `sync` no longer exits on `StreamReplaced`. A linked WhatsApp device
  permits only one active websocket, so when another session connects with
  this device's credentials the server tears down our stream. Instead of
  treating that as fatal, `sync` now waits a short backoff (8s, letting a
  transient competitor such as a one-shot `whatsapp-cli` command finish and
  disconnect) and reconnects to reclaim the stream. A sliding-window cap
  (3 reclaims within 2 minutes) detects a genuinely persistent competitor —
  WhatsApp Web/Desktop left open, or another process sharing the same
  `--store` — and stops the sync rather than ping-ponging into a reconnect
  war (which risks a temporary ban). Tuning lives in `streamReclaim*` vars
  in `internal/commands/commands.go`.

## 2026-05-08

### Fixed

- `sync` no longer silently stops delivering messages after a network
  timeout. Added a watchdog that checks the connection every 30s and
  forces a clean `Disconnect`+`Connect` cycle if the client has been
  offline for ≥ 5 minutes, breaking out of any wedged auto-reconnect
  state inside `whatsmeow`.
- `sync` now reacts to terminal session events. `LoggedOut` cancels the
  sync loop instead of leaving the process running idle; the user is told
  to re-run `whatsapp-cli auth`. (`StreamReplaced` handling was reworked
  on 2026-06-06 — see below.)

### Improved

- `whatsmeow` client logger raised from `ERROR` to `INFO` (database
  logger to `WARN`) so reconnect attempts and keepalive timeouts surface
  to the user instead of being invisible.
- `sync` event handler now reports `KeepAliveTimeout`,
  `KeepAliveRestored`, `ConnectFailure`, and `TemporaryBan` events with
  human-readable messages.
