# Clipbridge — Claude project notes

Cross-machine clipboard relay. Mac menu bar app hosts a web UI that any Windows browser can hit over a public tunnel. Solves: corporate RDP (Horizon / Jump Desktop Fluid / etc.) that blocks clipboard or limits it to text.

## Architecture in one breath

One Go process on Mac:
- `fyne.io/systray` menu bar
- HTTP server on `:8457` (the tunnel terminates TLS → forwards plain HTTP)
- Embedded HTML/JS web UI served at `/` and `/ui`
- Slot model: per-machine `to-NAME` and `from-NAME` directories with `.data` + `.meta` files
- Each Windows browser registers via `POST /register` every 30 s; 2-minute liveness window
- No Windows binary — that path failed (AV quarantine, Zscaler `.exe` blocks, no admin rights)

## Build

```
make          # full pipeline → build/Clipbridge.dmg
make build    # binary only
make app      # .app bundle
make dmg      # .dmg installer
make run      # build + run
make clean
```

## Project layout

```
src/cmd/mac/main.go          # entry: menu bar, polling, sends/receives
src/internal/clip/           # Mac clipboard (pbcopy/pbpaste + osascript)
src/internal/config/         # ~/.clipbridge/config.json
src/internal/server/         # HTTP handlers + slot storage
src/internal/server/ui.go    # embedded HTML/JS as a Go string constant
scripts/gen-icon.go          # generates assets/icon.png
scripts/Info.plist           # .app bundle metadata
docs/                        # architecture.md, security.md
```

## Conventions

- Module path: `clipbridge` (go.mod at root); imports look like `clipbridge/src/internal/...`
- Mac-only at the host. `//go:build darwin` on platform-specific files.
- The Mac process talks to its OWN server via direct method calls on the `*Server` struct (`PutText`, `PutFile`, `PutZip`, `ReadText`, `CopyFile`, `ClearSlot`, `PeekMeta`, `GetMachines`). NOT via HTTP. HTTP is only for the browsers.
- All API requests need `Authorization: Bearer <secret>`. `/receive` also accepts `?auth=` so browsers can use plain `<a download>`.
- Streaming everywhere: `http.ServeContent` for downloads (Range support), `MultipartReader` for uploads, 1 MiB `io.CopyBuffer` buffers. No `ioutil.ReadAll` on file paths.
- Multi-file uploads: client sets `X-File-Count` header. Count > 1 → server zips parts directly into the slot file. Client can set `X-Zip-Filename` to control the resulting filename. (This is the legacy single-shot `/send/` path — the web UI now uses `/upload/*` for all files.)
- Chunked uploads (`/upload/init` → `/upload/chunk/<id>/<idx>?offset=N` → `/upload/complete/<id>`) address writes by **byte offset**, never sequence number. That is what makes a retried chunk idempotent — don't "optimise" it into an append.
- Slot names from the URL must go through `validDir()`. A bare `strings.HasPrefix(dir, "to-")` check passes `to-../../x` and escapes the slots directory.
- Menu bar machine slots are pre-allocated (5 max). `fyne.io/systray` can't remove items, only show/hide.
- Sorting: `GetMachines()` returns alphabetical order — map iteration would otherwise flip the menu around on every poll.

## Build details

- The Makefile builds a **universal binary** (arm64 + x86_64) via `lipo` so one DMG works on both Apple Silicon and Intel Macs.
- Cross-compiling from arm64 to amd64 needs `CGO_ENABLED=1` + `CC="clang -arch x86_64"` because `fyne.io/systray` uses CGo on macOS.

## Release flow

```
make tag VERSION=x.y.z     # tags and pushes → triggers .github/workflows/release.yml
```

The release workflow:
1. Builds universal DMG
2. Attaches `Clipbridge-vX.Y.Z.dmg` to a new GitHub Release
3. If `TAP_PAT` secret is set: regenerates `Casks/clipbridge.rb` in `sven-s/homebrew-tap` from the local `homebrew/clipbridge.rb` template (just substitutes `version` and `sha256`) and pushes it

`TAP_PAT` = a fine-grained Personal Access Token with `Contents: write` on `sven-s/homebrew-tap`. Without it, the tap update step warns and exits 0.

## Constraints worth remembering before changing things

- **Corporate proxies (Zscaler, etc.)** — buffer entire downloads and "scan-then-burst." `0 B/s` is normal for minutes. The UI explicitly warns users about this.
- **Tailscale Funnel is blocked in the target corporate environment** (2026-08-18). `*.ts.net` is killed at the proxy — TCP connects, then `ERR_CONNECTION_CLOSED` before TLS completes. Confirmed on two independent corporate machines; `tailscale.com` itself may still resolve and load, so testing *that* proves nothing. Funnel is still the fallback when `public_url` is unset, but it is not the primary path.
- **Testing Funnel from the host Mac is misleading** — MagicDNS resolves `<host>.tail….ts.net` to the tailnet IP, so it returns 200 while the public path is dead. Worse, connecting to a Funnel ingress IP (`185.40.234.0/24`) from *inside* the tailnet also gets accept-then-close, which looks exactly like a broken ingress. Only an off-tailnet, off-corporate vantage point tells the truth.
- **Cloudflare Tunnel is the primary path** — `cloudflared` runs as a LaunchAgent (`~/Library/LaunchAgents/de.svens.clipbridge-tunnel.plist`), config in `~/.cloudflared/config.yml`, logs in `~/.cloudflared/clipbridge-tunnel.log`. Set `public_url` in `~/.clipbridge/config.json` and the app skips Funnel setup entirely.
- **Cloudflare caps request bodies at 100 MB** on Free/Pro. Downloads (responses) are uncapped. Uploads get around it via the chunked protocol in `src/internal/server/upload.go` — the UI slices at 64 MiB and the server reassembles. Verified end to end with a 150 MB file through the tunnel (checksum-matched); a single-shot `POST /send/` of the same file still returns 413, which is why the UI never uses that path for files.
- **Browser timers are not a reliable heartbeat** — Chrome throttles `setInterval` in hidden tabs (~1/min, then freezes the tab), and a minimised or disconnected RDP session stops them entirely. The old 2-minute liveness window made machines vanish from the menu bar whenever the user looked away. Now: `machineTTL` = 15 min, *any* authenticated request refreshes liveness (`X-Machine-Name` header, `?machine=` on `/receive`), the list is persisted to `slots/machines.json` across restarts, and the UI re-registers on `visibilitychange`/`focus`/`online`/`pageshow`.
- **Launchd-launched apps have stripped PATH** — `tailscale` won't be found via `exec.LookPath`. We hardcode `/opt/homebrew/bin/tailscale`, `/usr/local/bin/tailscale`, and `/Applications/Tailscale.app/Contents/MacOS/Tailscale` candidates.
- **systray icon on Mac** — PNG bytes via `image/png`. (The old Windows code needed ICO bytes; that's gone now.)
- **No `cd <cwd>` prefix on `git` commands** — that triggers permission prompts in this harness.

## Things that have been tried and dropped (don't re-suggest)

- Windows tray .exe — quarantined by Symantec / Defender ML heuristics; Zscaler also blocks .exe downloads
- Code signing for the Windows exe — adds cost + still doesn't bypass Zscaler exe-type rules
- Client-side ZIP in JS — would need JSZip (~100 KB inline); server-side zip with `archive/zip` is cleaner
- Streaming downloads through a JS `fetch` + blob URL — buffers in browser memory; breaks for 3 GB files. Use native `<a download>`.
- TLS termination in the Go server using `tailscale cert` — conflicts with Funnel's own TLS termination on the same port
- Auto-clearing slots after a fixed timeout — too short for big downloads through corporate proxies; user dismisses manually

## Style

User wants concise responses. Code over commentary. Fragment over sentence. Don't narrate tool calls. Don't summarize the diff after writing it.
