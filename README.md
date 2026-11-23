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

### Running the container as a non-root user

The image runs as root by default because `chroot(2)` needs `CAP_SYS_CHROOT`.
You can run it unprivileged — pocketpaas probes for the strongest fallback
(see *Subprocess isolation* below):

```bash
docker run -d \
  -p 8080:8080 --user 1000:1000 \
  -e DASHBOARD_PASSWORD=changeme \
  --security-opt seccomp=unconfined \
  --name pocketpaas \
  ghcr.io/foxy1402/pocketpaas:latest
```

- **`DATA_DIR` works out of the box for uid 1000.** The image pre-owns `/data`
  for that uid (named volumes mounted there inherit its ownership). For any
  other uid pocketpaas logs a warning and falls back to
  `$HOME/.pocketpaas/data` — or the OS temp dir when there is no home.
  Set `DATA_DIR` (and pre-`chown` the volume) to pin a specific path.
- **`--security-opt seccomp=unconfined` enables virtual-root mode.** Docker's
  default seccomp profile denies unprivileged `clone(CLONE_NEWUSER)`, so the
  probe fails and apps fall back to proot mode — which does give them their own
  rootfs view, at the cost of ptrace overhead (see below).
- **proot is the fallback for locked-down sandboxes.** The image ships
  `proot`, so an app still sees its own rootfs when the runtime grants *no*
  capabilities at all (root with `CapEff: 0000000000000000`) and denies both
  `chroot(2)` and `clone(CLONE_NEWUSER)` — the usual profile of a managed PaaS.
  proot intercepts every syscall, so throughput-heavy apps are noticeably
  slower than in chroot/virtual-root mode, and it requires `ptrace` to be
  permitted. For bare-metal/SSH installs run `apt-get install proot` to get
  this tier; otherwise pocketpaas uses rootfs-prefix mode.
- Without any of these the dashboard still runs fine; only app isolation and
  cross-libc image support are reduced.

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

## Troubleshooting app starts

If the dashboard boots but starting an app fails with
`start process: fork/exec …: operation not permitted`, the runtime hosting
pocketpaas is denying the syscalls used to spawn the app child. Some hardened
container runtimes restrict chroot/`clone` in ways a naive boot probe cannot
detect, so pocketpaas probes each tier with the exact syscall shape an app
start uses and demotes at runtime — chroot → virtual root → proot →
rootfs-prefix — logging the tier it lands on:

```
runtime: chroot(2) available — apps run in direct chroot mode
runtime: chroot(2) is permitted but spawning a chrooted child was denied — probing lower tiers
runtime: no chroot and no user namespaces — apps run in proot mode (translated rootfs, fake uid 0, no privileges needed)
app … started (isolation: proot, pid 42)
```

On a spawn failure it also logs the security context of the process:
`uid`/`euid`, `CapEff`, `NoNewPrivs`, `Seccomp`, and the rootfs mount flags.

To see exactly what the environment allows, run inside the container:

```sh
/app/pocketpaas --diagnose
```

It probes a plain spawn, a chrooted spawn, a user-namespace clone and proot,
then prints the tier app starts will use. A sandbox that reports
`uid=0 … CapEff: 0000000000000000` cannot chroot at all — proot (or prefix
mode) is the only option there, and if `probe: proot … failed` shows up, the
runtime blocks `ptrace` too and only static images will run.

---

## Environment Variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `DASHBOARD_PASSWORD` | **Yes** | — | Login password for the dashboard |
| `DATA_DIR` | No | auto | `/data` when writable (Docker); `~/.pocketpaas/data` otherwise |
| `PORT` | No | `8080` | HTTP listen port |
| `CACHE_DIR` | No | temp dir | Layer cache location. Defaults to the OS temp dir (ephemeral — wiped on restart, never eats persistent-volume quota); set it to a persistent path to keep the cache across reboots |
| `CACHE_MAX_GB` | No | `10` | Layer cache eviction cap (oldest blobs pruned first) |
| `REGISTRY_USER` | No | — | Username for private registry auth |
| `REGISTRY_PASSWORD` | No | — | Password for private registry auth |
| `NGROK_AUTHTOKEN` | No | — | Enables a public ngrok tunnel (get free at dashboard.ngrok.com) |
| `NGROK_DOMAIN` | No | — | Optional static ngrok hostname, e.g. `my-app.ngrok-free.app` |

---

## Ephemeral vs Persistent Storage

**Ephemeral (no volume):**
The database is wiped on every container restart. Use the **Portability** page
to export your app configs before stopping the container, and import them after
restarting. Then use **Sequential Deploy** to re-pull, start, and cache-prune
apps one by one to reduce ephemeral storage pressure.

**Persistent (with volume):**
The database and rootfs directories survive restarts. Apps with
**Auto-start** enabled will start automatically when the dashboard boots.

---

## Features

- **App management** — create, edit, delete apps with OCI image references
- **Image pull** — pulls directly from Docker Hub, GHCR, or any OCI registry
  (no Docker daemon needed); layers stream network → disk (no RAM buffering)
  and are cached digest-verified on disk, so re-pulls skip downloads entirely
- **Subprocess isolation** — four tiers, self-healing and probed at startup:
  direct **chroot** when root/CAP_SYS_CHROOT is available; **virtual root**
  (chroot inside a private user+mount namespace with an emulated uid 0) on
  rootless hosts that allow user namespaces; **proot** (ptrace path
  translation, fake uid 0, no privileges at all) on hardened sandboxes that
  deny both; rootfs-**prefix** mode otherwise. Apps always see their own
  isolated env vars; if exec-time policy denies a spawn even though the boot
  probe passed, pocketpaas demotes down the ladder (chroot → virtual root →
  proot → prefix) and logs the security context
- **Live log streaming** — last 1000 lines buffered; streamed live via SSE
- **Health checks** — optional HTTP ping every 15 s; shown as a status dot
- **Export / Import** — plain JSON backup of all app configs and env vars
- **Sequential Deploy** — import and deploy apps one by one (pull → start →
  prune caches/docs → next), reducing ephemeral storage pressure
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
