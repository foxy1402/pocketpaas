# pocketpaas

A lightweight personal app runtime dashboard. Pull OCI images, run them as
isolated subprocesses, and manage them from a clean dark web UI — no Docker
daemon required.

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
| `DATA_DIR` | No | `/data` | Where SQLite DB and app rootfs dirs are stored |
| `PORT` | No | `8080` | HTTP listen port |
| `LOG_LEVEL` | No | `info` | Verbosity: `debug`, `info`, `warn`, `error` |
| `REGISTRY_USER` | No | — | Username for private registry auth |
| `REGISTRY_PASSWORD` | No | — | Password for private registry auth |

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
