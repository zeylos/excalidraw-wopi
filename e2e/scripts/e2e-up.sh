#!/usr/bin/env bash
# Bring up the dockerized Drive stack plus our own binary for the e2e
# suite. Called by `make e2e-up`; see e2e/README.md for the port map and
# the host-networking constraint every service here runs under.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
ROOT_DIR="$(cd "$E2E_DIR/.." && pwd)"
RUN_DIR="$E2E_DIR/.run"
PID_FILE="$RUN_DIR/excalidraw-wopi.pid"
LOG_FILE="$RUN_DIR/excalidraw-wopi.log"
COMPOSE=(docker compose -f "$E2E_DIR/compose.yaml")

mkdir -p "$RUN_DIR"

echo "== fetch-drive =="
"$SCRIPT_DIR/fetch-drive.sh"

echo "== docker compose up =="
export DOCKER_USER="$(id -u):$(id -g)"

# Under the fuse-overlayfs storage driver, docker sometimes drops a
# container create with "symlink /proc/mounts .../etc/mtab: file exists"
# — a driver-level race, not a compose or image problem (reproduces even
# on a bare `docker run python:3.13-alpine`, and clears up on a plain
# retry). Retry the whole `up` a few times: each retry only touches
# containers that are not already up.
up_attempt=0
until "${COMPOSE[@]}" up -d --build --wait; do
  up_attempt=$((up_attempt + 1))
  if [ "$up_attempt" -ge 5 ]; then
    echo "docker compose up failed after $up_attempt attempts" >&2
    exit 1
  fi
  echo "== docker compose up failed (attempt $up_attempt), retrying in 3s ==" >&2
  sleep 3
done

echo "== django migrations =="
"${COMPOSE[@]}" exec -T app-dev python manage.py migrate

echo "== building excalidraw-wopi =="
(cd "$ROOT_DIR" && make build)

# Always stop any running instance and start a fresh one: the binary was
# just rebuilt above, and a stale process would also miss any change to
# e2e/env/excalidraw.env, since env vars are read once at process start.
if [ -f "$PID_FILE" ] && kill -0 "$(cat "$PID_FILE")" 2> /dev/null; then
  echo "== stopping previous excalidraw-wopi (pid $(cat "$PID_FILE")) =="
  kill "$(cat "$PID_FILE")" 2> /dev/null || true
  for _ in $(seq 1 20); do
    kill -0 "$(cat "$PID_FILE")" 2> /dev/null || break
    sleep 0.5
  done
fi

echo "== starting excalidraw-wopi on :8080 =="
(
  set -a
  # shellcheck disable=SC1091
  . "$E2E_DIR/env/excalidraw.env"
  set +a
  exec "$ROOT_DIR/bin/excalidraw-wopi"
) > "$LOG_FILE" 2>&1 &
echo $! > "$PID_FILE"

echo "== waiting for excalidraw-wopi /healthz =="
ready=0
for _ in $(seq 1 60); do
  if curl -sf http://localhost:8080/healthz > /dev/null; then
    ready=1
    break
  fi
  sleep 0.5
done
if [ "$ready" -ne 1 ]; then
  echo "excalidraw-wopi did not answer /healthz within 30s; see $LOG_FILE" >&2
  exit 1
fi

echo "== triggering WOPI discovery configuration (needs the celery worker up) =="
"${COMPOSE[@]}" exec -T app-dev python manage.py trigger_wopi_configuration

# The routing map lives in Drive's redis cache, filled in async by the
# celery worker after the trigger above. Poll it directly instead of
# guessing a sleep: this is the same condition e2e-seed's step (c) needs.
CHECK_ROUTE='import sys
from django.core.cache import cache
config = cache.get("wopi_configuration") or {}
sys.exit(0 if "excalidraw" in config.get("extensions", {}) else 1)'

echo "== waiting for Drive to ingest the excalidraw discovery route =="
routed=0
for _ in $(seq 1 60); do
  if "${COMPOSE[@]}" exec -T app-dev python manage.py shell -c "$CHECK_ROUTE" > /dev/null 2>&1; then
    routed=1
    break
  fi
  sleep 1
done

if [ "$routed" -eq 1 ]; then
  echo "== stack is ready: Drive routes .excalidraw to our service =="
else
  echo "== stack is up, but Drive has not routed .excalidraw yet ==" >&2
  echo "   our service serves GET /hosting/discovery, and Drive's" >&2
  echo "   celery worker must fetch and cache it before routing .excalidraw here; check that" >&2
  echo "   excalidraw-wopi answered /hosting/discovery and that the celery worker container is" >&2
  echo "   healthy. 'make e2e-seed' will report the same condition when it polls the wopi/ action." >&2
  exit 1
fi
