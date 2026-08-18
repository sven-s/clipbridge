# Security model

## TL;DR

This is **personal-use software**. Don't treat it like an enterprise file-share product.

- Public over the internet via Tailscale Funnel or Cloudflare Tunnel
- Protected by a single 32-character shared secret
- TLS terminated by whichever tunnel you chose — Tailscale (Let's Encrypt) or Cloudflare. Either way a third party sees plaintext.
- Files stored unencrypted in `~/.clipbridge/slots/`

If that's not enough for your data, don't use this for that data.

## Threat model

| Threat                                          | Mitigation                                                   |
| ----------------------------------------------- | ------------------------------------------------------------ |
| Eavesdropping on the wire                       | HTTPS via the tunnel (Tailscale Funnel or Cloudflare)        |
| Unauthorized access to the API                  | Bearer token (random 32-char hex secret) on every request    |
| Brute-forcing the secret                        | 128-bit secret, ~3.4 × 10^38 keyspace; no rate limit (yet)   |
| Browser XSS leaking the secret                  | UI page is static, no user content rendered                  |
| Secret left in URL history / proxy logs         | `Authorization: Bearer ...` header by default; `?auth=` only for downloads |
| Local attacker reading config                   | `~/.clipbridge/config.json` is mode `0600`                     |
| Local attacker reading in-flight files          | `~/.clipbridge/slots/*` are mode `0600` but unencrypted        |

## What is NOT mitigated

- **Rate limiting** — the API has no rate limit; brute-forcing the secret is theoretically possible. With 128 bits of entropy you'd need geological time, but a determined attacker on a fast pipe could try.
- **Authorization revocation** — if you suspect the secret leaked, edit the `secret` field in `~/.clipbridge/config.json` to a fresh 32-char hex string and restart the app, then re-pair each browser. (Deleting the whole file also works, but takes `public_url` with it.) There's no per-machine credentialing.
- **Tunnel provider sees plaintext** — TLS terminates at Tailscale or Cloudflare, not at the Go server, so whoever fronts you can read everything passing through. Terminating TLS in-process was tried and dropped; it conflicts with Funnel on the same port.
- **File-at-rest encryption** — slot data is plain bytes on disk. Anyone with read access to your home directory can see what's in transit.
- **Audit log** — no record of what was sent / received / by whom.

## Authentication flow

1. App generates a 16-byte random secret on first run, hex-encoded → 32-char string
2. Stored at `~/.clipbridge/config.json` mode `0600`
3. Menu bar item "Copy UI URL" produces `https://<your-tunnel-host>/?secret=<hex>`
   — note this puts the secret in the URL bar, so it lands in browser history and any
   intercepting proxy's logs. Treat a secret you've pasted at work as burned; rotate it
   if that matters to you.
4. The Windows browser stores the secret in `localStorage` (browser-side risk: extensions can read it)
5. Every API call from the browser includes `Authorization: Bearer <secret>`

## Hardening ideas (not implemented)

If this were going beyond personal-use:

- **Per-device tokens** with revocation endpoint
- **Rate limit** on auth failures (e.g., `1 request / s` per IP after 3 bad attempts)
- **At-rest encryption** of slot files with a key derived from the secret
- **Audit log** of registrations, sends, receives
- **Slot TTL** — auto-delete after N hours
- **Signed downloads** — pre-signed short-lived URLs instead of `?auth=`

Pull requests welcome.

## Recommendation

Don't paste anything into this you wouldn't be comfortable having in a personal Dropbox folder. Use it for the boring stuff (logs, scripts, config snippets, document files) and email yourself the spicy stuff.
