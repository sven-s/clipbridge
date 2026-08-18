# Architecture

## High level

```
       Local clipboard                          Browser clipboard
       (pbcopy/pbpaste,                         (navigator.clipboard
        NSPasteboard)                           writeText/readText)
              │                                         │
              ▼                                         ▼
     ┌──────────────────┐                     ┌──────────────────┐
     │  cmd/mac (Go)    │                     │  web UI (vanilla │
     │  • menu bar      │                     │   HTML + JS)     │
     │  • HTTP server   │                     │  • polls /poll/  │
     │  • slot storage  │                     │  • POSTs /send/  │
     └────────┬─────────┘                     │  • GETs /receive │
              │                               └────────┬─────────┘
              │ localhost:8457 (HTTP)                  │
              ▼                                        │
     ┌──────────────────┐                              │
     │  tunnel (TLS)    │ ◄────── HTTPS 443 ───────────┘
     │  Tailscale       │
     │  Funnel *.ts.net │
     │       ── or ──   │
     │  Cloudflare      │
     │  your-domain     │
     └──────────────────┘
```

## Why a single host process

Originally there was a Windows tray app too — it died on contact with corporate AV. Heuristic scanners quarantine unsigned Go binaries, Zscaler blocks `.exe` downloads, and you don't have admin rights anyway. So the Windows side became browser-only.

That leaves one Go process on the Mac, doing everything:

- **Menu bar UI** via `fyne.io/systray`
- **HTTP server** with streaming upload/download handlers
- **Local clipboard I/O** via `osascript` and `pbcopy`/`pbpaste`
- **Web UI** served as a single embedded HTML string (no static assets, no build step for the front end)

## Slot model

Each registered machine gets two "slots":

| Slot          | Direction        | Written by    | Read by       |
| ------------- | ---------------- | ------------- | ------------- |
| `to-NAME`     | Mac → Windows    | Mac process   | Windows web UI|
| `from-NAME`   | Windows → Mac    | Windows web UI| Mac process   |

A slot is a pair of files on disk:

```
~/.clipbridge/slots/to-OFFICE-PC.meta   →  {"type":"text","size":42}
~/.clipbridge/slots/to-OFFICE-PC.data   →  raw payload
```

The `.meta` file is what `/poll/<dir>` returns; the `.data` file is what `/receive/<dir>` streams.

## HTTP endpoints

| Method | Path             | Auth | Purpose                                          |
| ------ | ---------------- | ---- | ------------------------------------------------ |
| GET    | `/` or `/ui`     | none | Serves the web UI HTML                           |
| POST   | `/register`      | yes  | Windows announces its name (heartbeat)           |
| GET    | `/machines`      | yes  | Returns names seen in last 2 minutes             |
| POST   | `/send/<dir>`    | yes  | Multipart file OR raw text body → slot           |
| GET    | `/poll/<dir>`    | yes  | Returns slot metadata or 404                     |
| GET    | `/receive/<dir>` | yes* | Streams slot data (supports HTTP Range)          |
| DELETE | `/clear/<dir>`   | yes  | Removes the slot                                 |

\* `/receive` also accepts `?auth=<secret>` so the browser's native download manager can use a plain `<a href download>` link.

## Streaming details

**Uploads** (Windows → Mac):

- Browser sends `multipart/form-data`
- Server uses `r.MultipartReader()` (streaming) instead of `r.ParseMultipartForm` (buffering)
- Server streams the part directly to `~/.clipbridge/slots/<dir>.data` with a 1 MiB I/O buffer

**Downloads** (Mac → Windows):

- Handler calls `http.ServeContent` — automatic `Accept-Ranges: bytes`, `If-Modified-Since`, conditional GETs
- Resumable: if the connection drops mid-download, browser issues a Range request from where it left off
- `Cache-Control: no-store` to defeat any caching proxy

Result: 3 GB files work without OOM on either side.

## Machine registration

The Windows web UI POSTs to `/register` every 30 s with its name. The Mac stores `{name: lastSeen}` in memory.

`GetMachines()` returns names with `lastSeen > now - 2 minutes`, sorted alphabetically. The menu bar pre-allocates 5 machine slots in `onReady()`; `pollMachines()` shows/hides them based on the current list.

The 5-machine cap is a fyne.io/systray limitation — menu items can't be removed cleanly, only shown/hidden, so we pre-allocate.

## Why polling, not WebSockets / SSE

- The whole protocol is request/response by design (manual push, manual receive)
- 3 s polling adds at most ~30 KB/hour of overhead
- Behind Zscaler, long-lived connections often get killed
- WebSocket adds zero functional benefit for this UX

## Transports

The Go server only ever speaks plain HTTP on `127.0.0.1:8457`. Something in front terminates TLS. `setupFunnel()` picks which, in this order:

1. **`public_url` in `~/.clipbridge/config.json`** — if set, it wins. The app does no tunnel setup at all and just uses that URL for **Copy UI URL**. This is the Cloudflare Tunnel path (or any reverse proxy you run yourself).
2. **Tailscale Funnel** — shells out to `tailscale funnel --bg 8457`, and reads `Self.DNSName` from `tailscale status --json` to build the public URL. Funnel terminates TLS with a real Let's Encrypt cert (CT-logged → Chrome accepts it without warning). No port forwarding, no router config, no DDNS.
3. **`http://localhost:8457`** — fallback when Tailscale isn't installed or isn't running. Fine for local testing.

### Why Funnel isn't always enough

Corporate proxies commonly block `*.ts.net` outright — it classifies as Dynamic DNS or lands uncategorized. The symptom is TCP connecting and then the TLS handshake dying: `ERR_CONNECTION_CLOSED`, no HTTP status, no block page. Nothing on the Tailscale side fixes it; the domain is the problem. Arriving on a domain you own generally sails through, which is what the Cloudflare transport is for.

Two things make this failure mode hard to diagnose, both worth knowing before you spend an afternoon on it:

- **MagicDNS lies to you.** On the host Mac, `<host>.tail….ts.net` resolves to the tailnet IP (`100.x.y.z`), not the public Funnel ingress. `curl` returns `200` while the public path is stone dead.
- **Funnel ingress IPs reject tailnet members.** Forcing the public path from the host with `curl --resolve …:443:185.40.234.x` gets you accept-then-close — the *exact* signature of a genuinely broken ingress. It isn't broken; you're just connecting from inside the tailnet.

Verify from a vantage point that is neither on the tailnet nor behind the corporate proxy. A phone on cellular is the cheapest one.

### Cloudflare trade-offs

- **Uploads cap at 100 MB per request** on Free/Pro. Downloads (responses) are uncapped, so Mac → browser is unaffected at any size; browser → Mac over 100 MB returns `413`. Fixing that means chunked uploads in the web UI + a reassembling handler.
- **Cloudflare sees plaintext.** It terminates TLS at the edge and re-originates to `cloudflared`. Same trust shape as Funnel (where Tailscale holds the cert), different company.
- `cloudflared` runs as its own process — a LaunchAgent with `KeepAlive`, not something Clipbridge spawns. Setup lives in the README; it's deliberately outside the app so the app has no Cloudflare-specific code beyond reading `public_url`.
