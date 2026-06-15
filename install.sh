#!/bin/sh
# pocketpaas interactive installer for SSH-only containers
# Usage: curl -fsSL https://raw.githubusercontent.com/foxy1402/pocketpaas/main/install.sh | sh
set -e

REPO="foxy1402/pocketpaas"
BIN="$HOME/pocketpaas"
CFG_DIR="$HOME/.pocketpaas"
START_SCRIPT="$CFG_DIR/start.sh"
LOG_FILE="$CFG_DIR/pocketpaas.log"

# ── Platform detection ────────────────────────────────────────────────────────
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in
  x86_64)        ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) printf 'Unsupported architecture: %s\n' "$(uname -m)" >&2; exit 1 ;;
esac

# ── Port availability check ─────────────────────────────────────────────────
# Returns 0 (true) if the port is already bound on the local machine.
port_in_use() {
  local p hex
  p="$1"
  hex="$(printf '%04X' "$p")"
  # /proc/net/tcp and /proc/net/tcp6 list every bound socket (port in uppercase hex).
  grep -qsE ":${hex} " /proc/net/tcp /proc/net/tcp6 2>/dev/null && return 0
  # Fallback: ss (iproute2, available on most distros)
  if command -v ss >/dev/null 2>&1; then
    ss -tlnH 2>/dev/null | awk '{print $4}' | grep -qE ":${p}$" && return 0
  fi
  return 1
}

# ── Terminal I/O helpers ──────────────────────────────────────────────────────
# Read from /dev/tty so prompts work even when stdin is a curl pipe.
if [ -c /dev/tty ]; then
  TTY=/dev/tty
else
  TTY=/dev/stdin
fi

# ask PROMPT DEFAULT
# Prints the user's answer (or DEFAULT if blank) to stdout.
ask() {
  printf '  %-38s [%s]: ' "$1" "$2" >"$TTY"
  IFS= read -r _a <"$TTY"
  printf '%s' "${_a:-$2}"
}

# ask_secret PROMPT DEFAULT
# Like ask but hides the input. Shows bullet placeholder for DEFAULT.
ask_secret() {
  if [ -n "$2" ]; then
    _placeholder="$(printf '%s' "$2" | sed 's/./•/g')"
  else
    _placeholder="(blank)"
  fi
  printf '  %-38s [%s]: ' "$1" "$_placeholder" >"$TTY"
  stty -echo <"$TTY" 2>/dev/null || true
  IFS= read -r _a <"$TTY"
  stty echo  <"$TTY" 2>/dev/null || true
  printf '\n' >"$TTY"
  printf '%s' "${_a:-$2}"
}

# ── Banner ────────────────────────────────────────────────────────────────────
printf '\n'
printf '  ┌─────────────────────────────────────────┐\n'
printf '  │     pocketpaas  —  SSH installer        │\n'
printf '  └─────────────────────────────────────────┘\n'
printf '\n'
printf '  Platform : %s/%s\n' "$OS" "$ARCH"
printf '  Binary   : %s\n'    "$BIN"
printf '  Data dir : %s/data\n' "$CFG_DIR"
printf '\n'

# ── Step 1 — dashboard config ─────────────────────────────────────────────────
printf '  ── Step 1 of 2 : Dashboard ──────────────\n\n'

DASHBOARD_PASSWORD="$(ask_secret 'Dashboard password' 'changeme')"
while [ -z "$DASHBOARD_PASSWORD" ]; do
  printf '  Password cannot be empty.\n' >"$TTY"
  DASHBOARD_PASSWORD="$(ask_secret 'Dashboard password' '')"
done

while true; do
  PORT="$(ask 'HTTP port' '8080')"
  # Keep only digits; fall back to 8080 if blank
  PORT="$(printf '%s' "$PORT" | tr -cd '0-9')"
  [ -z "$PORT" ] && PORT=8080

  if [ "$PORT" -lt 1 ] || [ "$PORT" -gt 65535 ]; then
    printf '  Invalid port number (must be 1-65535).\n' >"$TTY"
    continue
  fi

  if port_in_use "$PORT"; then
    printf '  Port %s is already in use on this machine.\n' "$PORT" >"$TTY"
    _ans="$(ask 'Use it anyway? (y/N)' 'n')"
    case "$_ans" in
      y|Y) printf '  Proceeding with port %s.\n' "$PORT" >"$TTY"; break ;;
      *)   continue ;;
    esac
  else
    printf '  Port %s is available.\n' "$PORT" >"$TTY"
    break
  fi
done

printf '\n'

# ── Step 2 — ngrok config ─────────────────────────────────────────────────────
printf '  ── Step 2 of 2 : ngrok tunnel ───────────\n'
printf '\n'
printf '  ngrok exposes your dashboard on a public\n'
printf '  HTTPS URL — no open ports needed.\n'
printf '  Free token + static domain:\n'
printf '  https://dashboard.ngrok.com\n'
printf '\n'

NGROK_AUTHTOKEN="$(ask_secret 'ngrok auth token' '')"

NGROK_DOMAIN=""
if [ -n "$NGROK_AUTHTOKEN" ]; then
  printf '\n'
  printf '  A free static domain keeps the same URL\n'
  printf '  across restarts (e.g. my-app.ngrok-free.app).\n'
  printf '  Leave blank for an auto-assigned URL.\n'
  printf '\n'
  NGROK_DOMAIN="$(ask 'ngrok static domain' '')"
fi

printf '\n'

# ── Install binary ────────────────────────────────────────────────────────────
mkdir -p "$CFG_DIR"

printf '  ─────────────────────────────────────────\n\n'

DOWNLOAD_URL="https://github.com/$REPO/releases/latest/download/pocketpaas-$OS-$ARCH"
printf '==> Downloading binary...\n'
if curl -fsSL "$DOWNLOAD_URL" -o "$BIN" 2>/dev/null; then
  chmod +x "$BIN"
  printf '==> Downloaded → %s\n' "$BIN"
else
  printf '==> No pre-built release found; building from source...\n'

  # Install Go if missing
  if ! command -v go >/dev/null 2>&1; then
    printf '==> Installing Go 1.25...\n'
    curl -fsSL "https://go.dev/dl/go1.25.7.linux-$ARCH.tar.gz" | tar -C "$HOME" -xz
    export PATH="$HOME/go/bin:$PATH"
    printf 'export PATH="$HOME/go/bin:$PATH"\n' >> "$HOME/.profile" 2>/dev/null || true
    printf '==> Go installed.\n'
  fi

  TMP="$(mktemp -d)"
  printf '==> Cloning repository...\n'
  git clone --depth=1 "https://github.com/$REPO" "$TMP/src"
  printf '==> Building (this may take a minute)...\n'
  cd "$TMP/src"
  CGO_ENABLED=0 go build -ldflags="-s -w" -o "$BIN" ./cmd/server
  cd /
  rm -rf "$TMP"
  printf '==> Built from source → %s\n' "$BIN"
fi

# ── Write start.sh ────────────────────────────────────────────────────────────
# Values are embedded at install time; user edits this file to reconfigure.
cat > "$START_SCRIPT" <<STARTEOF
#!/bin/sh
# pocketpaas start script — generated by installer.
# Edit the values below then re-run: sh $START_SCRIPT
# Background mode: nohup sh $START_SCRIPT > $LOG_FILE 2>&1 &

DASHBOARD_PASSWORD="${DASHBOARD_PASSWORD}"
PORT="${PORT}"
NGROK_AUTHTOKEN="${NGROK_AUTHTOKEN}"
NGROK_DOMAIN="${NGROK_DOMAIN}"

exec env \\
  DASHBOARD_PASSWORD="\$DASHBOARD_PASSWORD" \\
  PORT="\$PORT" \\
  NGROK_AUTHTOKEN="\$NGROK_AUTHTOKEN" \\
  NGROK_DOMAIN="\$NGROK_DOMAIN" \\
  "${BIN}"
STARTEOF
chmod +x "$START_SCRIPT"

# ── Done ──────────────────────────────────────────────────────────────────────
printf '\n'
printf '  ┌─────────────────────────────────────────┐\n'
printf '  │     Installation complete!              │\n'
printf '  └─────────────────────────────────────────┘\n'
printf '\n'
printf '  Start now:\n'
printf '    sh %s\n' "$START_SCRIPT"
printf '\n'
printf '  Keep running after SSH disconnect:\n'
printf '    nohup sh %s > %s 2>&1 &\n' "$START_SCRIPT" "$LOG_FILE"
printf '    tail -f %s\n' "$LOG_FILE"
printf '\n'
printf '  Edit settings anytime:\n'
printf '    nano %s\n' "$START_SCRIPT"
printf '\n'
if [ -n "$NGROK_AUTHTOKEN" ]; then
  printf '  After starting, your public URL appears in the log:\n'
  printf '    ngrok: tunnel active → https://...\n'
  printf '\n'
fi
