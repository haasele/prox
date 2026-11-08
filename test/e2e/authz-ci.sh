#!/usr/bin/env bash
# CI-only: prove that Docker AuthZ enforcement rejects a port-binding create
# sent to the raw Docker socket while a signed create through prox succeeds.
# Requires root (or sudo) and a systemd-managed Docker daemon.
set -euo pipefail
cd "$(dirname "$0")/../.."
ROOT=$(pwd)

if [ "$(id -u)" -ne 0 ]; then
  echo "authz-ci.sh must run as root (use sudo)" >&2
  exit 1
fi

BIN="$ROOT/bin/prox"
STATE=/tmp/prox-authz-state
if [ ! -x "$BIN" ]; then
  go build -trimpath -o "$BIN" ./cmd/prox
fi
rm -rf "$STATE"
mkdir -p "$STATE" /run/docker/plugins

DAEMON_PID=""
DAEMON_JSON=/etc/docker/daemon.json
BACKUP=/tmp/prox-daemon.json.bak
HAD_JSON=0
if [ -f "$DAEMON_JSON" ]; then
  cp "$DAEMON_JSON" "$BACKUP"
  HAD_JSON=1
else
  echo '{}' > "$DAEMON_JSON"
fi

cleanup() {
  set +e
  DOCKER_HOST=unix:///tmp/prox-authz-run/docker.sock PROX_STATE_DIR="$STATE" PROX_RUNTIME_DIR=/tmp/prox-authz-run "$BIN" enforce off >/dev/null 2>&1
  if [ "$HAD_JSON" = "1" ]; then
    cp "$BACKUP" "$DAEMON_JSON"
  else
    rm -f "$DAEMON_JSON"
  fi
  systemctl restart docker >/dev/null 2>&1 || true
  kill "${DAEMON_PID:-}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

export PROX_SYSTEM=0
export PROX_STATE_DIR="$STATE"
export PROX_RUNTIME_DIR=/tmp/prox-authz-run
export PROX_HTTP_PORT=18080
export PROX_HTTPS_PORT=18443
export PROX_TLS_LOOPBACK_PORT=18444
export PROX_DNS_PORT=15335
export PROX_DOCKER_SOCKET=/tmp/prox-authz-run/docker.sock
export PROX_API_SOCKET=/tmp/prox-authz-run/api.sock
export PROX_AUTHZ_SOCKET=/run/docker/plugins/prox-authz.sock
export PROX_REAL_DOCKER_SOCKET=/var/run/docker.sock
export PROX_RULES_FILE="$STATE/rules.yaml"
export PROX_PROFILES_DIR="$STATE/profiles.d"
export PROX_CA_DIR="$STATE/ca"
export PROX_LOG_LEVEL=info

# Make sure the test image exists while Docker still accepts raw requests.
docker pull -q alpine:3.22 >/dev/null

"$BIN" daemon >/tmp/prox-authz.log 2>&1 &
DAEMON_PID=$!
for _ in $(seq 1 100); do
  "$BIN" ls >/dev/null 2>&1 && break
  sleep 0.2
done
"$BIN" enforce on

python3 - "$DAEMON_JSON" <<'PY'
import json, sys
p = sys.argv[1]
try:
    data = json.load(open(p))
except Exception:
    data = {}
plugins = data.get("authorization-plugins") or []
if "prox-authz" not in plugins:
    plugins.append("prox-authz")
data["authorization-plugins"] = plugins
json.dump(data, open(p, "w"), indent=2)
PY

systemctl restart docker
for _ in $(seq 1 100); do
  if docker info >/dev/null 2>&1; then break; fi
  sleep 0.2
done
docker info >/dev/null

PORT=$(python3 - <<'PY'
import socket
s=socket.socket(); s.bind(('',0)); print(s.getsockname()[1]); s.close()
PY
)

set +e
OUT=$(DOCKER_HOST=unix:///var/run/docker.sock docker run --rm -p "$PORT:80" alpine:3.22 true 2>&1)
RC=$?
set -e
if [ "$RC" -eq 0 ]; then
  echo "FAIL: raw Docker socket accepted a port-binding create under enforcement" >&2
  exit 1
fi
echo "$OUT" | grep -qiE 'authorization denied|prox enforcement' || {
  echo "FAIL: unexpected raw-socket error: $OUT" >&2
  exit 1
}
echo "PASS: raw-socket create denied: $OUT"

NAME="prox-authz-check-$RANDOM"
DOCKER_HOST=unix:///tmp/prox-authz-run/docker.sock docker run -d --rm --name "$NAME" -p "$PORT:80" alpine:3.22 sleep 10 >/dev/null
DOCKER_HOST=unix:///tmp/prox-authz-run/docker.sock docker rm -f "$NAME" >/dev/null
echo "PASS: signed create through prox allowed"
