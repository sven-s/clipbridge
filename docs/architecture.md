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
     │ tailscale funnel │ ◄────── HTTPS 443 ───────────┘
     │  Let's Encrypt   │
     │  *.ts.net        │
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
~/.clipsync/slots/to-OFFICE-PC.meta   →  {"type":"text","size":42}
~/.clipsync/slots/to-OFFICE-PC.data   →  raw payload
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
- Server streams the part directly to `~/.clipsync/slots/<dir>.data` with a 1 MiB I/O buffer

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

## Tailscale Funnel

- The app shells out to `tailscale funnel --bg 8457` on startup
- It also runs `tailscale status --json` to read `Self.DNSName` for building the public URL
- Funnel handles TLS termination with a real Let's Encrypt cert (CT-logged → Chrome accepts it without warning)
- No port forwarding, no router config, no DDNS

If Tailscale isn't running, the app falls back to `http://localhost:8457` which is fine for local testing.
