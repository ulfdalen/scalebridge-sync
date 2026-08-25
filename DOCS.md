# scalebridge-sync — full guide

Everything beyond the [three steps in the README](README.md). One binary, a setup wizard in your browser, and every weigh-in on your Withings scale shows up in Garmin Connect on its own.

If you'd rather not run it yourself, the hosted version at [scalebridge.ulf.su](https://scalebridge.ulf.su) does all of this for you.

- [Install](#install)
- [Docker](#docker)
- [Quick start](#quick-start)
- [Keep it running after a reboot](#keep-it-running-after-a-reboot)
- [The Withings developer app](#the-withings-developer-app)
- [macOS Gatekeeper and Windows SmartScreen](#macos-gatekeeper-and-windows-smartscreen)
- [Security](#security)
- [How it works](#how-it-works)
- [Honest disclaimer](#honest-disclaimer)
- [CLI reference](#cli-reference)
- [Contributing](#contributing)

## Install

### Docker

The one-liner from the README — see the [Docker section](#docker) below for what it does and how to update.

### Download a release

Grab the archive for your platform from [the latest release](https://github.com/ulfdalen/scalebridge-sync/releases/latest), unpack it, and you have a single executable.

| Platform | Artifact |
| --- | --- |
| macOS (Intel + Apple Silicon, universal) | `scalebridge-sync_<version>_darwin_all.tar.gz` |
| macOS (Apple Silicon only) | `scalebridge-sync_<version>_darwin_arm64.tar.gz` |
| macOS (Intel only) | `scalebridge-sync_<version>_darwin_amd64.tar.gz` |
| Windows (x64) | `scalebridge-sync_<version>_windows_amd64.zip` |
| Windows (ARM64) | `scalebridge-sync_<version>_windows_arm64.zip` |
| Linux (x64) | `scalebridge-sync_<version>_linux_amd64.tar.gz` |
| Linux (ARM64, e.g. Raspberry Pi 4/5 64-bit) | `scalebridge-sync_<version>_linux_arm64.tar.gz` |
| Linux (ARMv7, e.g. Raspberry Pi 32-bit) | `scalebridge-sync_<version>_linux_armv7.tar.gz` |

Every release ships a `checksums.txt`. To verify what you downloaded:

```
shasum -a 256 -c checksums.txt --ignore-missing
```

### go install

If you have a Go toolchain (1.25 or newer — Go fetches the exact version pinned in `go.mod` on demand):

```
go install github.com/ulfdalen/scalebridge-sync@latest
```

This builds from source on your machine, which also means macOS Gatekeeper and Windows SmartScreen have nothing to complain about.

### Homebrew

Coming soon — a `ulfdalen/tap` formula is planned for the first stable release.

## Docker

The README's run command, unpacked:

```
docker run -d --name scalebridge-sync --restart unless-stopped \
  -p 127.0.0.1:8723:8723 -v scalebridge-sync:/data \
  ulfdalen/scalebridge-sync
```

- `--restart unless-stopped` — the container comes back after reboots. This replaces `scalebridge-sync install`; you don't need an OS service inside Docker.
- `-p 127.0.0.1:8723:8723` — the UI is published to your machine's loopback only. Keep the `127.0.0.1:` prefix: the UI has no login, and `-p 8723:8723` alone would expose it to your whole network.
- `-v scalebridge-sync:/data` — all state (tokens, sync cursor, settings) lives in this named volume and survives container updates. Back it up and you've backed up everything.

The image is built for `linux/amd64` and `linux/arm64` (Raspberry Pi 4/5, Apple Silicon under Docker Desktop) and runs as a non-root user.

**With compose** — the repo ships [compose.yaml](compose.yaml) with the same setup:

```
curl -LO https://raw.githubusercontent.com/ulfdalen/scalebridge-sync/main/compose.yaml
docker compose up -d
```

**Updating:**

```
docker compose pull && docker compose up -d
```

Or with plain `docker run`: pull, `docker rm -f scalebridge-sync`, run the start command again. Either way your data is in the volume; recreating the container loses nothing.

**Changing the port** under Docker: keep the container's side at the app's configured port and change both ends together (change the port in Settings to 9000, then map `-p 127.0.0.1:9000:9000`). The port is part of the callback URL registered with Withings, so the Settings page walks you through updating Withings first. Simplest is to leave it at 8723.

**Building the image yourself:**

```
docker build -t scalebridge-sync .
```

## Quick start

You will need a Withings account, a Garmin Connect account, and about ten minutes. Run the binary:

```
./scalebridge-sync
```

It starts a small web server on `127.0.0.1:8723` and opens your browser at the setup wizard. Follow it through six steps: create your own Withings developer app (the wizard shows you exactly what to enter), authorize Withings, sign in to Garmin, choose how much history to backfill, pick a sync interval, done. The first sync starts immediately; after that it runs every 15 minutes by default.

When the wizard finishes, that same address — `http://localhost:8723` — is your dashboard: connection status, recent measurements, an event log, and a Sync now button.

## Keep it running after a reboot

(Docker users: `--restart unless-stopped` already covers this — skip ahead.)

Foreground mode stops when you close the terminal. To register it with your operating system so it starts on login and keeps syncing unattended:

```
./scalebridge-sync install
```

What that does on each platform:

- **macOS** — writes a LaunchAgent to `~/Library/LaunchAgents/` and loads it. It runs as you, starting at login.
- **Linux** — writes a systemd **user** unit and enables it. User units stop when you log out, so on a headless box (a Pi, a NAS, a home server) also run `loginctl enable-linger $USER` once. That keeps your user manager alive across logouts and reboots.
- **Windows** — registers a Windows Service that starts automatically at boot.

On every platform the absolute config directory is baked into the service arguments at install time, so the service never has to guess where your state file lives.

`./scalebridge-sync uninstall` removes it again. `start`, `stop` and `status` control and inspect the installed service.

## The Withings developer app

The wizard's centerpiece is a guided walkthrough for creating your own Withings developer app. This is the one step that looks unusual, so here is the point of it: your credentials are yours, your data flows directly between Withings, your computer, and Garmin, and nothing ever passes through a server belonging to this project or its author. There is no server to pass through.

The portal walkthrough, in short:

1. Go to [developer.withings.com](https://developer.withings.com/) and sign in with your normal Withings account. Open the developer dashboard and choose to create a new application.
2. Pick the public API integration type (not the medical or research programs) and accept the terms.
3. Fill in the application details. Name and description are yours to choose — "scalebridge-sync" and one sentence are fine. Nothing here is reviewed.
4. Set the callback URL to exactly `http://localhost:8723/callback`. This is the field that breaks setups: `http` not `https`, `localhost` not `127.0.0.1`, port `8723`, no trailing slash. The wizard shows the exact string with a copy button, built from the port it actually bound.
5. Save, then copy the **Client ID** and **Client Secret** into the wizard. The secret is stored on your computer and never leaves it.

Withings changes the portal's wording and layout every now and then, so treat the list above as the shape of the thing. The wizard inside the app always shows the current steps, with the target field highlighted at each stage.

## macOS Gatekeeper and Windows SmartScreen

The released binaries are not code-signed. Signing certificates cost money annually and this is a free project; that tradeoff is stated here rather than hidden. (Docker and `go install` are unaffected.)

**macOS.** A downloaded binary carries a quarantine flag and Gatekeeper refuses to run it. Remove the flag:

```
xattr -d com.apple.quarantine ./scalebridge-sync
```

Or, without the terminal: right-click (or Control-click) the binary in Finder, choose Open, and confirm in the dialog. Doing it once marks the binary as trusted.

**Windows.** SmartScreen shows a blue "Windows protected your PC" screen for executables it has not seen before. Click **More info**, then **Run anyway**. SmartScreen is reporting that this binary has no reputation with Microsoft yet, which is true of every unsigned build, including honest ones. If that is not a tradeoff you want to make, build from source with `go install` and you will know exactly what you ran.

Either way, verify the download against `checksums.txt` first — that tells you the file is the one that came out of the release pipeline.

## Security

- **Everything stays on your machine.** There is no backend for this project. Your computer talks to Withings and to Garmin, and to nothing else.
- **Tokens are stored in plaintext in a user-only file** with mode `0600`, alongside your Withings client ID and secret:
  - macOS — `~/Library/Application Support/scalebridge-sync/state.json`
  - Linux — `~/.config/scalebridge-sync/state.json` (or `$XDG_CONFIG_HOME/scalebridge-sync/state.json`)
  - Windows — `%AppData%\scalebridge-sync\state.json`
  - Docker — `/data/state.json` inside the `scalebridge-sync` volume

  This is the same posture as `docker`, `gh` and `kubectl`, and for the same reason: encrypting a file with a key stored next to it protects nothing. If someone can read files as your user, they can read the key too. File permissions are the actual control here.
- **Your Garmin password is never written to disk.** It is posted straight to Garmin's sign-in flow, held in memory only for the length of that request, and discarded. Only the resulting session tokens are stored.
- **The web UI binds to `127.0.0.1` by default.** It is not reachable from your network. There is a Host-header allowlist on top of that to defeat DNS rebinding, and API requests require a custom header that browsers refuse to send cross-origin. The Docker image binds `0.0.0.0` inside the container so the port mapping works — publish it to `127.0.0.1` as shown and the result is the same. If you deliberately want LAN access, put a reverse proxy in front of it and add your own authentication.
- **There is no login on the UI itself.** On a computer you share with other people, anyone with a local session can open the dashboard, see your measurements and trigger or disconnect syncs (they can never read your tokens or secrets through it). If that matters to you, an optional UI password is on the roadmap — until then, treat the app like any other per-user tool on a shared machine.
- **The browser makes zero external requests.** No CDN, no fonts, no analytics, no error reporting. The UI is HTML, CSS and vanilla JavaScript embedded in the binary.
- **The update check is opt-in and off by default.** When enabled, the binary itself (not your browser) asks the GitHub releases API once a day whether a newer version exists. Leave it off and nothing ever phones home.

## How it works

```
1. Poll the Withings API on a timer, sending back the cursor from the last poll
2. Keep only measurement groups you have not seen before (deduped by Withings grpid)
3. Write the new ones to disk before advancing the cursor, so a crash loses nothing
4. Encode each measurement group into a FIT file, the format Garmin's own devices use
5. Upload it to Garmin Connect and mark it synced; failures stay queued for next time
6. Sleep until the next tick — 15 minutes by default, configurable to 1h or 6h
```

## Honest disclaimer

Garmin has no public API for writing weight data. This tool signs in the way Garmin's own mobile app does. That can break without warning when Garmin changes their sign-in flow, and it has: most recently in March 2026. When that happens, a fix ships here as fast as we can build one. Open an issue if you see it break before we do.

This project is not affiliated with, endorsed by, or supported by Garmin or Withings. Both are trademarks of their respective owners.

## CLI reference

| Command | What it does |
| --- | --- |
| `scalebridge-sync` | Same as `run`. |
| `scalebridge-sync run` | Run in the foreground: web UI plus scheduler. Opens your browser if setup is incomplete. |
| `scalebridge-sync sync` | Sync once and exit. Hands the job to a running instance if there is one; otherwise does the work itself. Exits non-zero if the sync fails. |
| `scalebridge-sync install` | Register as an OS service so it starts at login or boot. |
| `scalebridge-sync uninstall` | Remove the OS service. |
| `scalebridge-sync start` | Start the installed service. |
| `scalebridge-sync stop` | Stop the installed service. |
| `scalebridge-sync status` | Report whether the service is installed and running. |
| `scalebridge-sync open` | Open the dashboard in your browser. |
| `scalebridge-sync version` | Print version and commit. |

| Flag | What it does |
| --- | --- |
| `--config <dir>` | Use a different directory for `state.json`. `install` bakes the resolved absolute path into the service definition. |
| `--bind <addr>` | Address the web UI listens on (default `127.0.0.1`). The Docker image uses `0.0.0.0` so the port mapping works. The UI has no login — bind another address only behind a firewall or a container port mapping. |

The port lives in Settings, not on the command line, because it is not just a local choice: it is part of the callback URL you registered with Withings (`http://localhost:8723/callback`). The Settings page has a guided flow that updates the Withings portal and the app in the right order.

## Contributing

Issues are welcome — bug reports, Garmin breakage sightings, and confusing wizard copy especially. The codebase is deliberately small and readable: standard library plus two dependencies, no frameworks, no build step for the UI. Start at `main.go`, follow `webui` into `syncer`, and you have seen most of it.

Before opening a pull request, run `gofmt -l .`, `go vet ./...` and `go test -race ./...` — CI runs those on Linux, macOS and Windows.
