# Changelog

Release notes for each version. The release workflow copies the section
matching the pushed tag into the GitHub Release, so add an entry here
**before** `make tag VERSION=x.y.z`.

## [0.6.0] - 2026-11-26

### Added
- **Tunnel menu** in the menu bar. It shows whether the `cloudflared`
  LaunchAgent is running and whether your public URL answers through
  Cloudflare. It also has Restart Tunnel, Open Tunnel Log, Edit Tunnel
  Config (`config.yml`), Edit Clipbridge Config (`config.json`), Restart
  Clipbridge, and a link to the setup guide.
- **Guided Cloudflare Tunnel setup**: Tunnel → *Set Up Cloudflare Tunnel…*
  runs `setup-tunnel.sh` in Terminal. It installs `cloudflared`, logs in,
  creates the tunnel and DNS route, writes `config.yml` and the
  LaunchAgent, and sets `public_url`. It's safe to re-run to repair a
  setup, and it asks before overwriting any file (keeping a `.bak` copy).
- `GET /healthz`: an unauthenticated probe that returns `clipbridge ok`.
- README: full LaunchAgent plist template for the Cloudflare tunnel.

### Changed
- **Sends from the browser to the Mac now queue up.** Sending several
  files or texts in a row used to deliver only the last one. Now
  *Receive* in the menu bar collects all of them, oldest first: files go
  to Downloads one by one, and texts are joined onto the clipboard. If
  one fails, the rest stay queued for the next click. The menu shows
  `Receive N items from NAME (… MB)` when more than one is waiting.
- Sends from the Mac to the browser are unchanged: the newest send still
  replaces the previous one.

### Upgrade notes
- An item still waiting from an older version is moved into the new
  queue on first launch.
- Re-running the setup on an existing tunnel offers to update
  `config.yml` to the current template (`127.0.0.1`, `connectTimeout`,
  `noHappyEyeballs`). Declining keeps your file.

## [0.5.0] - 2026-10-01

### Fixed
- Machines no longer vanish from the menu bar when the browser tab is
  hidden or the RDP session is minimised. A machine now stays listed for
  15 minutes (was 2), any authenticated request counts as a heartbeat,
  the machine list survives app restarts, and the web UI re-registers
  when the tab becomes visible again.

## [0.4.0] - 2026-08-19

### Added
- Chunked uploads (64 MiB slices, per-chunk retry), so files from the
  browser can exceed Cloudflare's 100 MB request limit.

## [0.3.0] - 2026-08-18

### Added
- About window with version, public URL, transport, and paths.
- The real version is stamped into the build.

## [0.2.0] - 2026-08-18

### Added
- Cloudflare Tunnel as an alternative to Tailscale Funnel: set
  `public_url` in `~/.clipbridge/config.json`.

## [0.1.1] - 2026-05-15

### Added
- Universal binary (Apple Silicon + Intel).
- Homebrew cask, with the tap updated automatically on release.

## [0.1.0] - 2026-05-15

- First release.

[0.6.0]: https://github.com/sven-s/clipbridge/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/sven-s/clipbridge/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/sven-s/clipbridge/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/sven-s/clipbridge/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/sven-s/clipbridge/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/sven-s/clipbridge/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/sven-s/clipbridge/releases/tag/v0.1.0
