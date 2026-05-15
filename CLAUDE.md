# Clipbridge — Claude project notes

Cross-machine clipboard relay. Mac menu bar app hosts a web UI that any Windows browser can hit via Tailscale Funnel. Solves: corporate RDP (Horizon / Jump Desktop Fluid / etc.) that blocks clipboard or limits it to text.

## Architecture in one breath

One Go process on Mac:
- `fyne.io/systray` menu bar
- HTTP server on `:8457` (Tailscale Funnel terminates TLS → forwards plain HTTP)
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
- Multi-file uploads: client sets `X-File-Count` header. Count > 1 → server zips parts directly into the slot file. Client can set `X-Zip-Filename` to control the resulting filename.
- Menu bar machine slots are pre-allocated (5 max). `fyne.io/systray` can't remove items, only show/hide.
- Sorting: `GetMachines()` returns alphabetical order — map iteration would otherwise flip the menu around on every poll.

## Constraints worth remembering before changing things

- **Corporate proxies (Zscaler, etc.)** — buffer entire downloads and "scan-then-burst." `0 B/s` is normal for minutes. The UI explicitly warns users about this.
- **Tailscale Funnel** — bandwidth-limited; expect slow but functional for big files. We do not own this path.
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
