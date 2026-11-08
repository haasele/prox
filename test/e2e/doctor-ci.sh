#!/usr/bin/env bash
# CI-only: install prox system-wide on default ports, trust its CA, and assert
# that `gw doctor` reports a healthy machine.
set -euo pipefail
cd "$(dirname "$0")/../.."
ROOT=$(pwd)

if [ "$(id -u)" -ne 0 ]; then
  echo "doctor-ci.sh must run as root (use sudo)" >&2
  exit 1
fi

BIN="$ROOT/bin/prox"
if [ ! -x "$BIN" ]; then
  go build -trimpath -o "$BIN" ./cmd/prox
fi
rm -rf /var/lib/prox /run/prox /etc/proxd

DAEMON_PID=""
cleanup() {
  set +e
  kill "${DAEMON_PID:-}" >/dev/null 2>&1 || true
  rm -f /usr/local/share/ca-certificates/prox-local-ca.crt
  update-ca-certificates >/dev/null 2>&1 || true
  rm -rf /var/lib/prox /run/prox /etc/proxd
}
trap cleanup EXIT

export PROX_SYSTEM=1
export PROX_LOG_LEVEL=info
"$BIN" daemon >/tmp/prox-doctor.log 2>&1 &
DAEMON_PID=$!
for _ in $(seq 1 150); do
  "$BIN" ls >/dev/null 2>&1 && break
  sleep 0.2
done
"$BIN" ls >/dev/null 2>&1 || { echo "daemon failed to start"; cat /tmp/prox-doctor.log; exit 1; }

"$BIN" ca install
"$BIN" doctor --fix || true
DOCKER_HOST=unix:///run/prox/docker.sock "$BIN" doctor
echo "PASS: gw doctor reports healthy on a clean machine"
