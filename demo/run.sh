#!/usr/bin/env bash
# Live prox demo: an isolated daemon that pre-owns 13000/tcp (HTTP hint) and
# 5431/tcp (TCP forward to postgres). The installed system prox owns 80/443.
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT=$(pwd)
DEMO="$ROOT/demo"
RUN="${PROX_DEMO_DIR:-/tmp/prox-demo}"
HTTP="${PROX_DEMO_HTTP_PORT:-18080}"
HTTPS="${PROX_DEMO_HTTPS_PORT:-18443}"
LOOP="${PROX_DEMO_LOOP_PORT:-18444}"
DNS="${PROX_DEMO_DNS_PORT:-15353}"
HOST_PORT="${PROX_DEMO_HOST_PORT:-13000}"
TCP_PORT="${PROX_DEMO_TCP_PORT:-15432}"
BIN="$ROOT/bin/prox"
SOCK="$RUN/run/docker.sock"
API="$RUN/run/api.sock"
REAL_DOCKER="${PROX_DEMO_REAL_DOCKER_SOCKET:-/var/run/docker.sock}"

[ -x "$BIN" ] || make build >/dev/null

cleanup() {
	set +e
	DOCKER_HOST="unix://$SOCK" docker rm -f prox-demo >/dev/null 2>&1
	if [ -f "$RUN/daemon.pid" ]; then
		kill "$(cat "$RUN/daemon.pid")" >/dev/null 2>&1
	fi
	rm -f "$DEMO/demo-app"
}
trap cleanup EXIT INT TERM

rm -rf "$RUN"
mkdir -p "$RUN/state" "$RUN/run"

info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }

info "building demo app and image"
CGO_ENABLED=0 go build -trimpath -o "$DEMO/demo-app" ./demo
docker build -q -t prox-demo:latest "$DEMO" >/dev/null
rm -f "$DEMO/demo-app"

info "starting an isolated prox daemon"
env PROX_STATE_DIR="$RUN/state" PROX_RUNTIME_DIR="$RUN/run" \
	PROX_HTTP_PORT="$HTTP" PROX_HTTPS_PORT="$HTTPS" \
	PROX_TLS_LOOPBACK_PORT="$LOOP" PROX_DNS_PORT="$DNS" \
	PROX_API_SOCKET="$API" PROX_DOCKER_SOCKET="$SOCK" \
	PROX_REAL_DOCKER_SOCKET="$REAL_DOCKER" \
	PROX_ORIGINAL_HINTS=1 \
	PROX_OWN_HTTP_PORTS="$HOST_PORT" \
	PROX_OWN_TCP_PORTS="$TCP_PORT" \
	PROX_DISCOVER=1 \
	PROX_LOG_LEVEL=info \
	"$BIN" daemon >"$RUN/daemon.log" 2>&1 &
echo $! >"$RUN/daemon.pid"

for _ in $(seq 1 100); do
	[ -S "$SOCK" ] && [ -S "$API" ] && break
	sleep 0.1
done
[ -S "$API" ] || { cat "$RUN/daemon.log" >&2; exit 1; }

export PROX_STATE_DIR="$RUN/state" PROX_RUNTIME_DIR="$RUN/run" \
	PROX_HTTP_PORT="$HTTP" PROX_HTTPS_PORT="$HTTPS" \
	PROX_API_SOCKET="$API"

"$BIN" ls >/dev/null

info "creating the HTTP container through the prox Docker socket"
DOCKER_HOST="unix://$SOCK" docker rm -f prox-demo >/dev/null 2>&1 || true
DOCKER_HOST="unix://$SOCK" docker run -d --rm --name prox-demo \
	-p "$HOST_PORT:3000" \
	-e ORIGINAL_URL="http://127.0.0.1:$HOST_PORT/" \
	prox-demo:latest

for _ in $(seq 1 100); do
	"$BIN" ls 2>/dev/null | grep -q 'prox-demo.localhost.*up' && break
	sleep 0.1
done

CA="$RUN/state/ca/pki/authorities/local/root.crt"
URL="https://prox-demo.localhost:$HTTPS/"
PORTLESS=""
for _ in $(seq 1 30); do
	if curl -sk --max-time 1 --resolve prox-demo.localhost:443:127.0.0.1 https://prox-demo.localhost/ >/dev/null 2>&1; then
		PORTLESS="https://prox-demo.localhost/"
		break
	fi
	sleep 0.2
done

printf '\n'
info "demo is live"
if [ -n "$PORTLESS" ]; then
	printf '  portless site:  %s  (installed system prox on 443)\n' "$PORTLESS"
fi
printf '  demo site:      %s\n' "$URL"
printf '  curl:           curl --cacert %s --resolve prox-demo.localhost:%s:127.0.0.1 %s\n' "$CA" "$HTTPS" "$URL"
printf '  owned HTTP:     http://127.0.0.1:%s (prox-owned hint page -> new site)\n' "$HOST_PORT"
printf '  owned TCP:      127.0.0.1:%s (reserved; no app registered)\n' "$TCP_PORT"
printf '  inspect:        %s ls\n' "$BIN"
printf '  events:         %s events -n 10\n' "$BIN"
printf '\nPort 80/443 belong to the installed prox; this isolated demo owns %s/%s instead.\n' "$HOST_PORT" "$TCP_PORT"
printf 'Press Ctrl+C to stop the demo.\n\n'

"$BIN" ls
printf '\n'
wait "$(cat "$RUN/daemon.pid")"
