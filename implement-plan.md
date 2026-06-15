# Implementation Plan: PaaS App Dashboard (Portainer-like)

## Project Overview

A lightweight Go web dashboard that acts as a mini container runtime inside a single PaaS container.
It pulls OCI images (Docker Hub, GHCR, etc.), extracts their filesystems, and runs each "app" as an
isolated child subprocess with its own env vars. A Portainer-inspired UI allows managing all apps
from a browser.

**Project name suggestion:** `apphive` (placeholder — rename freely)

---

## Core Design Decisions

- **Language:** Go (single static binary, minimal memory, no runtime deps)
- **Execution model:** Each app = one OS subprocess (`os/exec`), env vars set via `cmd.Env` (no
  cross-app leakage possible by design)
- **Image pulling:** `github.com/google/go-containerregistry` (pure Go, no Docker daemon needed)
- **State storage:** SQLite via `modernc.org/sqlite` (pure Go, no cgo) — stored at a configurable
  path so it works with or without a persistent volume
- **UI:** Server-rendered HTML with HTMX + minimal vanilla JS (no build step, embedded via
  `embed.FS`)
- **Auth:** Single dashboard password via env var (`DASHBOARD_PASSWORD`), compared with
  `subtle.ConstantTimeCompare` on each login, session stored as a plain cookie (no signing needed
  for personal use)
- **Import/Export:** Plain JSON file containing all app configs + env vars, downloadable and
  re-uploadable via UI to fully restore state after a container restart on ephemeral storage

---

## Project Structure

```
apphive/
├── cmd/
│   └── server/
│       └── main.go                  # Entry point, flag/env parsing, server bootstrap
├── internal/
│   ├── auth/
│   │   ├── auth.go                  # Password check, session creation/validation
│   │   └── middleware.go            # HTTP middleware for protected routes
│   ├── registry/
│   │   ├── pull.go                  # OCI image pull via go-containerregistry
│   │   └── extract.go               # Layer extraction to rootfs directory
│   ├── runtime/
│   │   ├── manager.go               # App lifecycle: start, stop, restart, status
│   │   ├── process.go               # os/exec subprocess wrapper, env injection
│   │   └── logs.go                  # Per-app stdout/stderr ring buffer capture
│   ├── store/
│   │   ├── db.go                    # SQLite init, migrations
│   │   ├── apps.go                  # CRUD for app records
│   │   └── models.go                # App, EnvVar, DeploymentStatus structs
│   ├── api/
│   │   ├── router.go                # HTTP route registration
│   │   ├── apps.go                  # App CRUD + action handlers
│   │   ├── logs.go                  # Log streaming handler (SSE)
│   │   └── portability.go           # Import/export handlers
│   └── portability/
│       ├── export.go                # Serialize all apps+envvars to plain JSON
│       └── import.go                # Parse JSON, validate, restore apps+envvars to DB
└── web/
    ├── static/
    │   ├── htmx.min.js              # Vendored, served via embed.FS
    │   └── style.css                # Minimal dashboard CSS
    └── templates/
        ├── layout.html              # Base layout with nav
        ├── login.html               # Password login page
        ├── apps.html                # App list dashboard (main page)
        ├── app_detail.html          # Single app view: logs, env vars, controls
        ├── app_form.html            # Create/edit app form
        └── portability.html         # Import/export page
```

---

## Environment Variables (Dashboard Config)

These are env vars for the dashboard itself, not for any managed app.

| Variable | Required | Default | Description |
|---|---|---|---|
| `DASHBOARD_PASSWORD` | ✅ Yes | — | Plain-text password used to log into the dashboard |
| `DATA_DIR` | No | `/data` | Directory for SQLite DB and extracted app rootfs dirs |
| `PORT` | No | `8080` | Port the dashboard HTTP server listens on |
| `LOG_LEVEL` | No | `info` | Logging verbosity: `debug`, `info`, `warn`, `error` |

> **Ephemeral storage note:** If the PaaS has no persistent volume, `DATA_DIR` lives in the
> container's temporary filesystem and is wiped on restart. The import/export feature handles
> recovery in this case — export before the container stops, import after it restarts. If the PaaS
> provides a persistent volume, mount it at `DATA_DIR` and state survives restarts automatically
> with no extra steps needed.

---

## Data Model

### App

```go
type App struct {
    ID          string        // UUID
    Name        string        // User-defined display name
    ImageRef    string        // e.g. "ghcr.io/user/myapp:latest"
    Entrypoint  []string      // Override image entrypoint (optional)
    Command     []string      // Override image CMD (optional)
    EnvVars     []EnvVar      // Isolated env vars for this app
    WorkDir     string        // Extracted rootfs path under DATA_DIR
    Status      AppStatus     // stopped | pulling | running | crashed | error
    CreatedAt   time.Time
    UpdatedAt   time.Time
    LastStarted *time.Time
}

type EnvVar struct {
    Key   string
    Value string  // stored as plaintext in SQLite
}

type AppStatus string
const (
    StatusStopped  AppStatus = "stopped"
    StatusPulling  AppStatus = "pulling"
    StatusRunning  AppStatus = "running"
    StatusCrashed  AppStatus = "crashed"
    StatusError    AppStatus = "error"
)
```

---

## Feature Breakdown

### 1. Authentication

- On startup, check if `DASHBOARD_PASSWORD` is set — fatal error if missing
- On first request to any protected route, redirect to `/login`
- `POST /login`: compare submitted password against `DASHBOARD_PASSWORD` using
  `subtle.ConstantTimeCompare`, set a plain session cookie (random token stored in memory),
  redirect to `/`
- `GET /logout`: clear session cookie
- Session TTL: 24 hours, refreshed on each request
- All UI routes and API routes except `/login` are protected by auth middleware
- Session tokens are stored in a simple in-memory map in the Go process — they are lost on
  dashboard restart, requiring a re-login (acceptable for personal use)

### 2. App Management (CRUD)

**Create app (`GET/POST /apps/new`):**
- Fields: Name, Image Ref (e.g. `ghcr.io/user/app:latest`), optional Entrypoint override,
  optional CMD override
- Env vars: key/value pair list, addable/removable dynamically in the form
- On save: persist to SQLite, do NOT auto-pull or start (user triggers those separately)

**Edit app (`GET/POST /apps/:id/edit`):**
- Same form as create, pre-populated
- Editing env vars of a running app: save to DB, apply on next restart (note shown in UI)

**Delete app (`DELETE /apps/:id`):**
- Stop the app if running
- Remove DB record and extracted rootfs directory
- Confirm dialog in UI before delete

**List apps (`GET /`):**
- Table/card view of all apps with: Name, Image, Status badge, Last Started, action buttons
  (Pull, Start, Stop, Restart, Logs, Edit, Delete)

**App detail (`GET /apps/:id`):**
- Status, image info, env var list (values masked by default, toggle to reveal)
- Live log stream
- Action buttons

### 3. Image Pull & Extraction

**Trigger:** `POST /apps/:id/pull`

Flow:
1. Set app status to `pulling` (UI polls for status updates via HTMX)
2. Call `crane.Pull(imageRef)` with optional auth for private registries
3. Export image layers as tar to a temp file
4. Extract tar to `DATA_DIR/apps/<id>/rootfs/`
5. Parse image config to extract default `Entrypoint`, `Cmd`, and `WorkingDir`
6. Store these as defaults on the app record (user overrides take priority)
7. Set status back to `stopped` on success, `error` on failure
8. Stream pull progress to UI via SSE

> **Private registry auth:** Support `REGISTRY_USER` and `REGISTRY_PASSWORD` env vars as
> dashboard-level config, applied globally to all pulls. Per-app registry auth can be a v2 feature.

### 4. App Runtime (Start/Stop/Restart)

**Start (`POST /apps/:id/start`):**
1. Check rootfs exists (prompt to pull first if not)
2. Resolve entrypoint: user override → image default → error
3. Build `cmd.Env` from app's EnvVar list only (no parent env inheritance)
4. Set `cmd.Dir` to the extracted rootfs path
5. Attach stdout/stderr to per-app ring buffer (last 1000 lines)
6. Launch subprocess; store PID
7. Start a goroutine to `cmd.Wait()` and update status to `crashed` or `stopped` on exit
8. Update app status to `running`

**Stop (`POST /apps/:id/stop`):**
1. Send `SIGTERM` to the subprocess
2. Wait up to 10 seconds for graceful exit
3. Send `SIGKILL` if still running after timeout
4. Update status to `stopped`

**Restart:** Stop then Start in sequence.

**Crash recovery:** When the wait goroutine detects non-zero exit, set status to `crashed`.

### 5. Log Streaming

- Each app's subprocess stdout+stderr is piped into an in-memory ring buffer (circular slice of
  lines, capped at 1000 lines)
- `GET /apps/:id/logs` — SSE endpoint; sends buffered lines immediately on connect, then streams
  new lines as they arrive
- UI uses HTMX SSE extension to display live logs in an auto-scrolling `<pre>` block
- Log lines are plain text; no structured parsing required

### 6. Import / Export

This is the key resilience feature for ephemeral PaaS storage.

#### Export (`GET /portability/export`)

1. Query all app records + env vars from SQLite
2. Serialize to a plain JSON file:
```json
{
  "version": 1,
  "exported_at": "2026-06-15T10:00:00Z",
  "apps": [
    {
      "id": "...",
      "name": "my-proxy",
      "image_ref": "ghcr.io/user/app:latest",
      "entrypoint": [],
      "command": [],
      "env_vars": [
        { "key": "PORT", "value": "8080" },
        { "key": "SECRET", "value": "hunter2" }
      ]
    }
  ]
}
```
3. Return as a file download: `apphive-export-<timestamp>.json`

> The file is plain JSON with no encryption. Store it somewhere private (local machine, private
> cloud drive) since it contains env var values in plaintext. This is fine for personal use.

#### Import (`POST /portability/import`)

1. Accept file upload of a `.json` export file
2. Parse and validate JSON structure and version field — show clear error if malformed
3. For each app in the export:
   - If an app with the same ID already exists: skip it
   - Otherwise: insert app record + env vars into SQLite
4. Show import summary: N apps restored, M skipped
5. Apps are restored in `stopped` status — user starts them manually
6. Rootfs directories are NOT included in the export (too large) — user re-pulls images after
   import. The UI shows a **Pull All** button after a successful import.

#### Portability page (`GET /portability`)

Simple page with:
- **Export** section: description + "Download Export File" button
- **Import** section: file picker + "Import" button + result summary after upload
- **Pull All** button: triggers pull for all apps currently in `stopped`/`error` state (useful
  post-import on a fresh container)

### 7. Dashboard UI Pages

| Route | Description |
|---|---|
| `GET /login` | Login form |
| `GET /` | App list (main dashboard) |
| `GET /apps/new` | Create app form |
| `GET /apps/:id` | App detail + logs |
| `GET /apps/:id/edit` | Edit app form |
| `GET /portability` | Import/export page |

UI style: clean, dark-themed, minimal — inspired by Portainer but much simpler. HTMX handles
all dynamic updates (status polling, log streaming, form submissions) without a JS framework.

---

## Startup Sequence

```
main()
  → check DASHBOARD_PASSWORD is set (fatal error if missing)
  → ensure DATA_DIR exists (create if not)
  → open/init SQLite DB, run migrations
  → init RuntimeManager (scan DB for apps marked "running" at last shutdown → reset to "stopped")
  → register HTTP routes
  → start HTTP server on PORT
```

> On ephemeral PaaS: DB is gone on restart so there are no stale "running" apps. On persistent
> storage: the reset step above handles cleanup correctly either way.

---

## Key Dependencies

| Package | Purpose |
|---|---|
| `github.com/google/go-containerregistry` | OCI image pull and layer extraction |
| `modernc.org/sqlite` | Pure-Go SQLite (no cgo), app state persistence |
| `crypto/subtle` | Constant-time password comparison |
| Standard `encoding/json` | Export/import serialization |
| Standard `net/http` | HTTP server |
| Standard `os/exec` | Subprocess management |
| Standard `embed` | Embed web templates and static assets |
| `github.com/google/uuid` | App ID generation |

No web framework needed — standard `net/http` with a simple route multiplexer is sufficient.

---

## Dockerfile (for the dashboard itself)

```dockerfile
FROM golang:1.23-alpine AS builder
WORKDIR /build
COPY . .
RUN CGO_ENABLED=0 go build -o apphive ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /build/apphive .
EXPOSE 8080
ENTRYPOINT ["./apphive"]
```

`CGO_ENABLED=0` ensures a fully static binary. Alpine base is ~8MB total image size.

---

## Out of Scope (v1)

- Filesystem isolation between apps (all apps share the container filesystem; env vars are the
  isolation boundary in v1)
- Network port mapping between apps (apps bind to whatever port they want; your PaaS routes
  external traffic to port 8080 which the dashboard occupies — apps needing external access need
  their own PaaS service or a reverse proxy in front)
- Per-app resource limits (cgroups require root, not available on free PaaS tiers)
- Multi-user / role-based access (single password only)
- Per-app private registry credentials (global registry auth only in v1)
- Auto-restart policy (v2 — groundwork is in the wait goroutine)
- Export file encryption (out of scope for personal use; store the file privately instead)

---

## Suggested Build Order for AI Agent

1. `internal/store` — models, DB init, migrations, app CRUD
2. `internal/auth` — password check, in-memory session map, middleware
3. `internal/registry` — image pull + layer extraction
4. `internal/runtime` — subprocess manager, log ring buffer
5. `internal/portability` — export/import JSON logic
6. `internal/api` — HTTP handlers wiring all the above together
7. `web/` — HTML templates and static assets
8. `cmd/server/main.go` — bootstrap, env parsing, server start
9. `Dockerfile` + `README.md`
