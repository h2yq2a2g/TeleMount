<p>
  <img src="assets/icon.svg" width="64" height="64" alt="TeleMount Logo" />
</p>

# TeleMount

Self-hosted **Telegram-backed encrypted storage** written in pure Go (`CGO_ENABLED=0`), compiled from **one single codebase (`cmd/telemount`)** for both:
1. **Windows Desktop App (`telemount.exe`)**: Taskbar / System Tray icon (left-click opens the Web UI to configure, right-click for quick actions), WinFsp Virtual Drive (e.g. `V:`), persistent local disk cache (default `10 GB`, LRU eviction), offline pinning, durable offline write queue, and optional Windows startup.
2. **Linux / Portainer Docker Server**: Headless HTTP `:8080` (`/` embedded Control & Ops Web UI + Setup Wizard, `/dav/` WebDAV with separate credentials for a separate `rclone` container), RAM-first with zero SSD wear for file data.

## Shared Core Architecture
- **1 Unified Binary (`cmd/telemount`)**:
  - `/` — Embedded Control & Ops Web UI (Setup Wizard, Dashboard & Ops, Settings — intentionally no web file browser or web trash).
  - `/dav/` — WebDAV server with separate credentials for `rclone`.
  - On Windows (default GUI mode): binds loopback `127.0.0.1:8080` (falls back to an ephemeral loopback port if busy), stores local data in `%LocalAppData%\TeleMount`, runs System Tray + WinFsp Virtual Drive + persistent disk cache. Pass `--headless` to run as a plain HTTP server on Windows, or `--minimized` to start silently in the tray.
  - On Linux / Docker: runs headless on `:8080` (`/data`, `/tmp/telemount`) with Desktop UI panels automatically hidden.
- **Telegram MTProto (`gotd/td`)**: Uses your Telegram user account + 1 dedicated Private Channel.
- **Hybrid Pack Storage (Duplicati-style on RAM)**:
  - **Small files (`< 128 MB` default, configurable in Web UI)**: Encrypted in RAM and coalesced into **1 GB Packs** (flushed at `1024 MB`, after `10s` idle, or on clean shutdown). Readable with O(1) random seek (`pack_offset`), plus one-click **Compact Packs** on Web UI.
  - **Large files (`>= 128 MB`)**: Streamed directly to Telegram in `1 GB` parts (`part_size_mb`, configurable in Web UI).
- **Encryption**: Passphrase derived via **Argon2id** (+ random 16-byte salt) into a 32-byte AES-256 master key; files are encrypted in **64 KiB AES-256-GCM blocks** (O(1) random byte-range seek) and stored on Telegram as random `<hex>.bin` blobs.
- **SQLite + Auto Encrypted Backup**: Metadata lives in `telemount.db` (WAL + `temp_store=MEMORY`) and auto-backs up (gzipped + Argon2id/AES-256-GCM `.tmdb`) to your Telegram Private Channel.
- **Multi-Account Server-Side Backup Mirroring**: Add 1 or more secondary Telegram accounts in Settings — TeleMount creates a dedicated private channel owned by each backup account and server-side copies (`DropAuthor: true`, 0 MB server bandwidth) every Pack/part + `.tmdb` backup with automatic read failover, one-click Promote to Primary, and fresh-machine recovery from any backup channel.

## Quick Start — Windows Desktop (`TeleMount-Setup.exe` / `telemount.exe`)

### Option A: Single-File Setup Installer (Recommended — auto-installs WinFsp)
Build `dist\TeleMount-Setup.exe` (bundles `telemount.exe` + official signed WinFsp MSI and silently installs WinFsp if missing):
```powershell
# Install Inno Setup 6 once (user scope, no admin required):
winget install --id JRSoftware.InnoSetup --scope user

# Build dist\TeleMount-Setup.exe:
powershell -ExecutionPolicy Bypass -File .\scripts\build-installer.ps1
```
Run `dist\TeleMount-Setup.exe` — it installs WinFsp automatically if not present, installs TeleMount, registers Start Menu / optional Desktop & Startup shortcuts, and launches TeleMount directly in your normal desktop session.

### Option B: Standalone Portable Binary (`telemount.exe`)
1. **Install WinFsp** once: `winget install WinFsp.WinFsp` (or download from [winfsp.dev/rel/](https://winfsp.dev/rel/)).
2. **Build `telemount.exe`** (pure Go, no CGO/GCC needed):
   ```powershell
   $env:CGO_ENABLED="0"; $env:GOOS="windows"; $env:GOARCH="amd64"
   go build -trimpath -ldflags="-s -w -H=windowsgui" -o telemount.exe ./cmd/telemount
   ```
3. **Use TeleMount on Windows**:
   - First launch opens the local Web UI (`http://127.0.0.1:8080`) for the 4-step Setup Wizard (Admin → Telegram → Private Channel → Encryption Key — no WebDAV/rclone step on Windows) and auto-mounts drive `V:` right away.
   - **Left-click** the TeleMount icon in the Windows taskbar/system tray anytime to open the Web UI and configure drive letter (`V:`), disk cache limit (default `10 GB`), pinned offline paths, and Windows startup.
   - **Right-click** the tray icon for Mount/Unmount, Open Drive, Open Web UI, Run at Startup, and Clean Quit (flushes pending writes before exiting).

## Quick Start — Linux VPS / Portainer (Docker)

No `.env` secrets are required to boot.

```bash
mkdir -p data mnt
docker compose up -d --build
```

Cross-compile the Linux server binary from Windows/macOS (zero CGO):

```powershell
# ARM64 (e.g. Raspberry Pi / ARM VPS)
$env:CGO_ENABLED="0"; $env:GOOS="linux"; $env:GOARCH="arm64"
go build -trimpath -ldflags="-s -w" -o telemount-linux-arm64 ./cmd/telemount

# AMD64 (x86_64 VPS)
$env:CGO_ENABLED="0"; $env:GOOS="linux"; $env:GOARCH="amd64"
go build -trimpath -ldflags="-s -w" -o telemount-linux-amd64 ./cmd/telemount
```

Open `http://127.0.0.1:8080` (or via your reverse proxy) and complete the **5-step Setup Wizard**:
1. **Create Web UI Admin account** (mandatory on first boot; locks all API/WebDAV until completed).
2. **Connect Telegram** (`api_id` & `api_hash` from [my.telegram.org/apps](https://my.telegram.org/apps)) and sign in via **QR code** or **Phone + OTP + 2FA**.
3. **Create or select 1 Private Channel**.
4. **Encryption Key**:
   - **Fresh vault**: Enter any passphrase (or click *Sinh ngẫu nhiên*), save it safely — TeleMount hashes it via Argon2id and saves only the derived key to `master.key` (`0600`), then pushes an initial encrypted DB backup to your channel.
   - **Restore on a new machine/server**: If the channel already contains `.tmdb` backups, pick the backup and re-enter your original Encryption Key to restore your entire folder tree.
5. **Create separate WebDAV credentials** (independent of Web UI Admin; optional on Windows Desktop).

### Attach the separate `rclone` container (Linux / Portainer)

```bash
cp rclone/rclone.conf.example rclone/rclone.conf
# Generate obscured password for rclone.conf:
docker run --rm rclone/rclone obscure "YOUR_WEBDAV_PASSWORD"
# Put the obscured password into rclone/rclone.conf, then start rclone:
docker compose --profile rclone up -d
```

On the host, `./mnt` is backed by your encrypted Telegram storage with VFS write cache running in RAM (`/cache` `tmpfs`).

> **Reverse proxy tip (Nginx)**: Disable request buffering and set `client_max_body_size 0;` + long `proxy_read_timeout`/`proxy_send_timeout` so multi-gigabyte uploads stream smoothly without buffering to proxy disk.

## Local Development / Tests

```bash
go test ./...
go test -tags e2e ./e2e/...
```

## Automated Releases (GitHub Actions)

Push a version tag to build and publish **Windows Setup (`TeleMount-Setup-*.exe` with bundled WinFsp)**, portable Windows/Linux binaries, `SHA256SUMS.txt`, and multi-arch Docker images (`linux/amd64`, `linux/arm64`) on GHCR:

```bash
git tag v1.0.0
git push origin v1.0.0
```

Prebuilt Docker image (for Portainer / VPS):
```bash
docker pull ghcr.io/h2yq2a2g/telemount:latest
```
