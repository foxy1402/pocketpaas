# pocketpaas

A lightweight personal app runtime dashboard. Pull OCI images, run them as
isolated subprocesses, and manage them from a clean dark web UI — no Docker
daemon required.

---

## Install inside an SSH container

If you have SSH access to a container but no Docker access (e.g. a free tier
slot on Railway, Render, Fly.io, or similar), install directly inside the
container:

```bash
curl -fsSL https://raw.githubusercontent.com/foxy1402/pocketpaas/main/install.sh | sh
```

The installer is fully interactive and will:
1. Prompt for a dashboard password, HTTP port, and optional ngrok tunnel.
2. Download a pre-built binary matching your arch (`amd64` or `arm64`), or
   fall back to building from source (auto-installs the latest Go if needed).
3. Generate `~/.pocketpaas/start.sh` with all your settings baked in.

**After installing:**

```bash
# Start pocketpaas
sh ~/.pocketpaas/start.sh

# Keep it running after you disconnect
nohup sh ~/.pocketpaas/start.sh > ~/.pocketpaas/pocketpaas.log 2>&1 &
echo "PID=$!"
```

### Updating

Re-running the installer detects your existing config and running instance:

```bash
curl -fsSL https://raw.githubusercontent.com/foxy1402/pocketpaas/main/install.sh | sh
```

- If pocketpaas is running, it offers to stop it before updating and
  auto-restarts it with the new binary when done.
- If a config exists, it asks **"Update binary only?"** — choosing yes
  downloads the latest binary and preserves your password, port, and ngrok
  settings without re-prompting.
- Choosing no re-runs the interactive prompts with your previous values
  pre-filled as defaults.

### ngrok tunnel

Once `NGROK_AUTHTOKEN` is set, pocketpaas prints a public URL to the log:

```
ngrok: tunnel active → https://your-app.ngrok-free.app
ngrok: dashboard is reachable at https://your-app.ngrok-free.app
```

Open that URL in any browser — no port forwarding needed. Get a free auth
token and a free static domain at <https://dashboard.ngrok.com>.

> **Security:** pocketpaas is authenticated with `DASHBOARD_PASSWORD`. Use a
> strong password whenever the dashboard is reachable from the public internet.

---

## Quick Start (Docker)

```bash
docker run -d \
  -p 8080:8080 \
  -e DASHBOARD_PASSWORD=changeme \
  --name pocketpaas \
  ghcr.io/foxy1402/pocketpaas:latest
```

Open `http://localhost:8080` and log in.

---

## With Persistent Storage

Mount a volume so the SQLite database (and optionally pulled image rootfs)
survives container restarts:

```bash
docker run -d \
  -p 8080:8080 \
  -e DASHBOARD_PASSWORD=changeme \
  -v pocketpaas-data:/data \
  --name pocketpaas \
  ghcr.io/foxy1402/pocketpaas:latest
```

---

## Environment Variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `DASHBOARD_PASSWORD` | **Yes** | — | Login password for the dashboard |
| `DATA_DIR` | No | auto | `/data` when writable (Docker); `~/.pocketpaas/data` otherwise |
| `PORT` | No | `8080` | HTTP listen port |
| `REGISTRY_USER` | No | — | Username for private registry auth |
| `REGISTRY_PASSWORD` | No | — | Password for private registry auth |
| `NGROK_AUTHTOKEN` | No | — | Enables a public ngrok tunnel (get free at dashboard.ngrok.com) |
| `NGROK_DOMAIN` | No | — | Optional static ngrok hostname, e.g. `my-app.ngrok-free.app` |

---

## Ephemeral vs Persistent Storage

**Ephemeral (no volume):**
The database is wiped on every container restart. Use the **Portability** page
to export your app configs before stopping the container, and import them after
restarting. Then use **Sequential Deploy** to re-pull and start apps one by one
without exhausting ephemeral storage.

**Persistent (with volume):**
The database and rootfs directories survive restarts. Apps with
**Auto-start** enabled will start automatically when the dashboard boots.

---

## Features

- **App management** — create, edit, delete apps with OCI image references
- **Image pull** — pulls directly from Docker Hub, GHCR, or any OCI registry
  (no Docker daemon needed)
- **Subprocess isolation** — each app runs as a child process with its own
  isolated env vars; no cross-app env leakage
- **Live log streaming** — last 1000 lines buffered; streamed live via SSE
- **Health checks** — optional HTTP ping every 15 s; shown as a status dot
- **Export / Import** — plain JSON backup of all app configs and env vars
- **Sequential Deploy** — import and deploy apps one by one (pull → start →
  prune rootfs → next), preventing OOM on ephemeral storage
- **System stats** — live CPU % and RAM usage displayed in the nav bar
- **Auto-start** — apps can be configured to start automatically on dashboard
  boot (useful with persistent storage)
- **Exposed port links** — click-through links to apps running on localhost

---

## Project Layout

```
cmd/server/          Entry point + embedded web assets
  web/
    static/          CSS, HTMX
    templates/       HTML templates
internal/
  auth/              Session management
  api/               HTTP handlers
  registry/          OCI image pull + extraction
  runtime/           Subprocess manager + log buffer
  store/             SQLite persistence
  portability/       Export / Import
  sysinfo/           CPU & RAM stats (Linux /proc)
```

---

## Building Locally

```bash
go build -o pocketpaas ./cmd/server
DASHBOARD_PASSWORD=secret ./pocketpaas
```

Requires Go 1.25+. No other dependencies needed at runtime.

---

## Security Notes

- Session tokens are stored in memory only — a dashboard restart invalidates
  all sessions (re-login required).
- Env var values are stored as plaintext in SQLite and appear in plain JSON
  exports. Keep your export files private.
- This dashboard is designed for **personal / trusted-network use only**. It
  is not hardened for public-internet exposure.
