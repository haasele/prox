#!/usr/bin/env bash
# prox installer — distro-agnostic Linux setup.
#
#   ./install.sh [options]
#
# Installs a static prox binary, a system service (systemd, OpenRC or runit),
# CA trust, shell completions, and optionally points DOCKER_HOST at prox.
# Run as a normal user; the script elevates itself with sudo when required.
set -euo pipefail

PREFIX=/usr/local
BIN_SRC=""
ACTION=install
DO_SERVICE=1
DO_CA=1
DO_DOCKER_HOST=1
PURGE=0
FORCE_PORTS=0
HTTP_PORT=80
HTTPS_PORT=443

usage() {
	cat <<'EOF'
prox installer

Usage: ./install.sh [options]

Options:
  --prefix DIR          install prefix (default /usr/local)
  --binary PATH         install this prebuilt prox binary instead of building
  --no-service          do not install or start an init service
  --no-ca               do not install the local CA into trust stores
  --no-docker-host      do not write /etc/profile.d/prox.sh (new shells keep
                        their current DOCKER_HOST)
  --http-port N         public HTTP port (default 80)
  --https-port N        public HTTPS port (default 443)
  --force-ports         install even if 80/443 are already in use
  --uninstall           remove prox (keeps /var/lib/prox, /etc/proxd unless
                        --purge)
  --purge               with --uninstall, also remove state, rules and CA data
  --help                show this help

The installer detects apt/dnf/pacman/zypper/apk for hints, writes systemd,
OpenRC or runit service files as appropriate, and uses prox's own CA trust-store
detection (Debian/Fedora/Arch/openSUSE/Alpine + NSS).

After install:
  export DOCKER_HOST=unix:///run/prox/docker.sock   # done for new shells
  gw doctor
  open https://gw.localhost

Uninstall:
  ./install.sh --uninstall
  ./install.sh --uninstall --purge          # also delete CA/rules/state
EOF
}

info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

SELF=${BASH_SOURCE[0]:-$0}
case "$SELF" in
	/*) ;;
	*) SELF="$(pwd)/$SELF" ;;
esac

while [ $# -gt 0 ]; do
	case "$1" in
		--prefix) [ $# -ge 2 ] || die "--prefix needs a value"; PREFIX=$2; shift 2 ;;
		--binary) [ $# -ge 2 ] || die "--binary needs a value"; BIN_SRC=$2; shift 2 ;;
		--no-service) DO_SERVICE=0; shift ;;
		--no-ca) DO_CA=0; shift ;;
		--no-docker-host) DO_DOCKER_HOST=0; shift ;;
		--http-port) [ $# -ge 2 ] || die "--http-port needs a value"; HTTP_PORT=$2; shift 2 ;;
		--https-port) [ $# -ge 2 ] || die "--https-port needs a value"; HTTPS_PORT=$2; shift 2 ;;
		--force-ports) FORCE_PORTS=1; shift ;;
		--uninstall) ACTION=uninstall; shift ;;
		--purge) PURGE=1; shift ;;
		--help|-h) usage; exit 0 ;;
		*) die "unknown option: $1 (try --help)" ;;
	esac
done

case "$PREFIX" in
	/*) ;;
	*) die "--prefix must be an absolute path" ;;
esac
case "$PREFIX" in
	*" "*) die "--prefix must not contain spaces" ;;
	/) die "refusing to use / as an install prefix" ;;
esac
case "$HTTP_PORT" in
	''|*[!0-9]*) die "HTTP port must be numeric" ;;
esac
case "$HTTPS_PORT" in
	''|*[!0-9]*) die "HTTPS port must be numeric" ;;
esac
{ [ "$HTTP_PORT" -ge 1 ] && [ "$HTTP_PORT" -le 65535 ]; } || die "HTTP port out of range"
{ [ "$HTTPS_PORT" -ge 1 ] && [ "$HTTPS_PORT" -le 65535 ]; } || die "HTTPS port out of range"
[ "$HTTP_PORT" != "$HTTPS_PORT" ] || die "HTTP and HTTPS ports must differ"

prefix_writable() {
	if [ -d "$PREFIX" ]; then
		[ -w "$PREFIX" ]
	else
		p=$PREFIX
		while [ ! -e "$p" ]; do p=$(dirname "$p"); done
		[ -w "$p" ]
	fi
}

need_root() {
	if [ "$(id -u)" -eq 0 ]; then
		return 1
	fi
	if [ "$PURGE" -eq 0 ] && [ "$DO_SERVICE" -eq 0 ] && [ "$DO_CA" -eq 0 ] && [ "$DO_DOCKER_HOST" -eq 0 ] && prefix_writable; then
		return 1
	fi
	return 0
}

if need_root; then
	if command -v sudo >/dev/null 2>&1; then
		info "root is required; elevating with sudo"
		exec sudo -- bash "$SELF" "$@"
	fi
	die "root is required. Re-run as root or install sudo."
fi

if [ "$(uname -s)" != "Linux" ]; then
	die "prox targets Linux (detected $(uname -s))"
fi
IS_ROOT=0
[ "$(id -u)" -eq 0 ] && IS_ROOT=1

REPO_DIR=$(cd "$(dirname "$SELF")" && pwd)
TMP=$(mktemp -d "${TMPDIR:-/tmp}/prox-install.XXXXXX")
trap 'rm -rf "$TMP"' EXIT

DISTRO="unknown Linux"
if [ -r /etc/os-release ]; then
	# shellcheck disable=SC1091
	. /etc/os-release
	DISTRO=${PRETTY_NAME:-${NAME:-unknown Linux}}
fi

ARCH=$(uname -m)
case "$ARCH" in
	x86_64|amd64) ARCH=amd64 ;;
	aarch64|arm64) ARCH=arm64 ;;
	armv7l|armv6l) ARCH=arm ;;
	*) : ;;
esac

go_hint() {
	case "${ID:-}" in
		debian|ubuntu) echo "sudo apt-get install -y golang-go" ;;
		fedora|rhel|centos|rocky|almalinux) echo "sudo dnf install -y golang" ;;
		arch|manjaro|garuda) echo "sudo pacman -S --needed go" ;;
		opensuse*|suse*) echo "sudo zypper install go" ;;
		alpine) echo "sudo apk add go" ;;
		void) echo "sudo xbps-install -S go" ;;
		*) echo "install Go, or pass --binary /path/to/prox" ;;
	esac
}

write_file() {
	dst=$1
	mode=$2
	mkdir -p "$(dirname "$dst")"
	cat > "$dst.tmp.$$"
	chmod "$mode" "$dst.tmp.$$"
	mv -f "$dst.tmp.$$" "$dst"
}

put_file() {
	src=$1
	dst=$2
	mode=$3
	mkdir -p "$(dirname "$dst")"
	cp "$src" "$dst.tmp.$$"
	chmod "$mode" "$dst.tmp.$$"
	mv -f "$dst.tmp.$$" "$dst"
}

version_string() {
	if command -v git >/dev/null 2>&1 && [ -d "$REPO_DIR/.git" ]; then
		git -C "$REPO_DIR" describe --tags --always --dirty 2>/dev/null || echo dev
	else
		echo dev
	fi
}

service_manager() {
	if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
		echo systemd
	elif command -v rc-service >/dev/null 2>&1 && [ -d /etc/init.d ]; then
		echo openrc
	elif command -v sv >/dev/null 2>&1; then
		echo runit
	else
		echo none
	fi
}

service_active() {
	case "$(service_manager)" in
		systemd) systemctl is-active --quiet prox.service 2>/dev/null ;;
		openrc) rc-service prox status >/dev/null 2>&1 ;;
		runit) sv status prox >/dev/null 2>&1 ;;
		*) return 1 ;;
	esac
}

port_busy() {
	port=$1
	if command -v ss >/dev/null 2>&1; then
		if ss -ltnH "sport = :$port" 2>/dev/null | grep -q .; then
			return 0
		fi
		return 1
	fi
	hex=$(printf '%04X' "$port")
	awk -v h=":$hex" '$2 ~ h"$" && $4 == "0A" {found=1} END {exit !found}' /proc/net/tcp /proc/net/tcp6 2>/dev/null
}

port_owned_by_prox() {
	port=$1
	command -v ss >/dev/null 2>&1 || return 1
	ss -ltnpH "sport = :$port" 2>/dev/null | grep -qi 'prox'
}

check_ports() {
	if [ "$DO_SERVICE" -eq 0 ]; then
		return 0
	fi
	for port in "$HTTP_PORT" "$HTTPS_PORT"; do
		if port_busy "$port"; then
			if port_owned_by_prox "$port"; then
				info "port $port is already held by prox"
				continue
			fi
			# Without ss we cannot inspect the owner; trust an active service
			# during an upgrade.
			if ! command -v ss >/dev/null 2>&1 && service_active; then
				info "port $port is held by the running prox service"
				continue
			fi
			if [ "$FORCE_PORTS" -eq 1 ]; then
				warn "port $port is in use; continuing because of --force-ports"
				continue
			fi
			die "port $port is already in use. Stop the other listener or pass --force-ports / custom --http-port/--https-port."
		fi
	done
}

build_binary() {
	out=$1
	if [ -n "$BIN_SRC" ]; then
		[ -f "$BIN_SRC" ] || die "--binary $BIN_SRC does not exist"
		cp "$BIN_SRC" "$out"
	elif [ -f "$REPO_DIR/cmd/prox/main.go" ] && command -v go >/dev/null 2>&1; then
		ver=$(version_string)
		info "building prox ($ver) with Go"
		(
			cd "$REPO_DIR"
			CGO_ENABLED=0 go build -trimpath \
				-ldflags "-s -w -X github.com/haasele/prox/internal/version.Version=$ver" \
				-o "$out" ./cmd/prox
		) || die "go build failed. $(go_hint)"
	elif [ -x "$REPO_DIR/bin/prox" ]; then
		info "using prebuilt $REPO_DIR/bin/prox"
		cp "$REPO_DIR/bin/prox" "$out"
	else
		info "no Go toolchain and no local binary; downloading release"
		url="https://github.com/haasele/prox/releases/latest/download/prox-linux-$ARCH"
		if command -v curl >/dev/null 2>&1; then
			curl -fsSL "$url" -o "$out" || die "download failed ($url). $(go_hint)"
		elif command -v wget >/dev/null 2>&1; then
			wget -qO "$out" "$url" || die "download failed ($url). $(go_hint)"
		else
			die "Go is not installed and neither curl nor wget is available. $(go_hint)"
		fi
	fi
	chmod 0755 "$out"
	"$out" version >/dev/null 2>&1 || die "the selected binary is not a working prox build"
}

install_binary() {
	bin=$1
	mkdir -p "$PREFIX/bin"
	put_file "$bin" "$PREFIX/bin/prox" 0755
	ln -sfn prox "$PREFIX/bin/gw"
	ln -sfn prox "$PREFIX/bin/proxd"
}

install_docs() {
	for f in README.md docs/USAGE.md docs/ARCHITECTURE.md docs/DECISIONS.md; do
		if [ -f "$REPO_DIR/$f" ]; then
			put_file "$REPO_DIR/$f" "$PREFIX/share/doc/prox/$(basename "$f")" 0644
		fi
	done
}

install_completions() {
	gw="$PREFIX/bin/gw"
	mkdir -p "$PREFIX/share/bash-completion/completions" \
		"$PREFIX/share/zsh/site-functions" \
		"$PREFIX/share/fish/vendor_completions.d"
	"$gw" completions bash > "$TMP/gw.bash" 2>/dev/null || warn "bash completions unavailable"
	if [ -s "$TMP/gw.bash" ]; then
		put_file "$TMP/gw.bash" "$PREFIX/share/bash-completion/completions/gw" 0644
	fi
	"$gw" completions zsh > "$TMP/_gw" 2>/dev/null || true
	if [ -s "$TMP/_gw" ]; then
		put_file "$TMP/_gw" "$PREFIX/share/zsh/site-functions/_gw" 0644
	fi
	"$gw" completions fish > "$TMP/gw.fish" 2>/dev/null || true
	if [ -s "$TMP/gw.fish" ]; then
		put_file "$TMP/gw.fish" "$PREFIX/share/fish/vendor_completions.d/gw.fish" 0644
	fi
}

write_profile() {
	ports=""
	if [ "$HTTP_PORT" != "80" ]; then
		ports="$ports
	export PROX_HTTP_PORT=$HTTP_PORT"
	fi
	if [ "$HTTPS_PORT" != "443" ]; then
		ports="$ports
	export PROX_HTTPS_PORT=$HTTPS_PORT"
	fi
	write_file /etc/profile.d/prox.sh 0644 <<EOF
# Generated by prox install.sh. Remove this file to stop new shells from
# routing Docker through prox. Override DOCKER_HOST to use the raw socket.
if [ -z "\${DOCKER_HOST:-}" ]; then
	export DOCKER_HOST=unix:///run/prox/docker.sock
fi$ports
EOF
	if [ -d /etc/fish ] || command -v fish >/dev/null 2>&1; then
		fishports=""
		if [ "$HTTP_PORT" != "80" ]; then
			fishports="$fishports
	if not set -q PROX_HTTP_PORT
		set -gx PROX_HTTP_PORT $HTTP_PORT
	end"
		fi
		if [ "$HTTPS_PORT" != "443" ]; then
			fishports="$fishports
	if not set -q PROX_HTTPS_PORT
		set -gx PROX_HTTPS_PORT $HTTPS_PORT
	end"
		fi
		write_file /etc/fish/conf.d/prox.fish 0644 <<EOF
# Generated by prox install.sh.
if not set -q DOCKER_HOST
	set -gx DOCKER_HOST unix:///run/prox/docker.sock
end$fishports
EOF
	fi
}

service_unit_env() {
	if [ "$HTTP_PORT" != "80" ]; then
		echo "Environment=PROX_HTTP_PORT=$HTTP_PORT"
	fi
	if [ "$HTTPS_PORT" != "443" ]; then
		echo "Environment=PROX_HTTPS_PORT=$HTTPS_PORT"
	fi
}

install_service() {
	manager=$(service_manager)
	case "$manager" in
		systemd)
			{
				cat <<EOF
[Unit]
Description=prox local gateway (Docker + host process proxy)
Documentation=https://github.com/haasele/prox
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
Environment=PROX_SYSTEM=1
Environment=PROX_LOG_LEVEL=info
EOF
				service_unit_env
				cat <<EOF
ExecStart=$PREFIX/bin/proxd
Restart=on-failure
RestartSec=2
RuntimeDirectory=prox
RuntimeDirectoryMode=0755
StateDirectory=prox
StateDirectoryMode=0700
LimitNOFILE=65536
User=root
Group=root

[Install]
WantedBy=multi-user.target
EOF
			} | write_file /etc/systemd/system/prox.service 0644
			systemctl daemon-reload
			systemctl enable prox.service >/dev/null 2>&1 || warn "could not enable prox.service"
			systemctl restart prox.service || die "systemctl failed to start prox (journalctl -u prox -n 80)"
			;;
		openrc)
			{
				cat <<EOF
#!/sbin/openrc-run
name="prox"
description="prox local gateway"
command="$PREFIX/bin/proxd"
command_background=true
pidfile="/run/prox.pid"
output_log="/var/log/prox.log"
error_log="/var/log/prox.log"
export PROX_SYSTEM=1
EOF
				if [ "$HTTP_PORT" != "80" ]; then
					echo "export PROX_HTTP_PORT=$HTTP_PORT"
				fi
				if [ "$HTTPS_PORT" != "443" ]; then
					echo "export PROX_HTTPS_PORT=$HTTPS_PORT"
				fi
				cat <<'EOF'
depend() {
	use net
	after docker
}
EOF
			} | write_file /etc/init.d/prox 0755
			rc-update add prox default >/dev/null 2>&1 || warn "could not add prox to default runlevel"
			if rc-service prox status >/dev/null 2>&1; then
				rc-service prox restart || die "rc-service failed to restart prox"
			else
				rc-service prox start || die "rc-service failed to start prox"
			fi
			;;
		runit)
			mkdir -p /etc/sv/prox
			{
				echo '#!/bin/sh'
				echo 'export PROX_SYSTEM=1'
				if [ "$HTTP_PORT" != "80" ]; then echo "export PROX_HTTP_PORT=$HTTP_PORT"; fi
				if [ "$HTTPS_PORT" != "443" ]; then echo "export PROX_HTTPS_PORT=$HTTPS_PORT"; fi
				echo "exec $PREFIX/bin/proxd 2>&1"
			} | write_file /etc/sv/prox/run 0755
			linkdir=""
			for d in /var/service /etc/service /etc/runit/runsvdir/default; do
				if [ -d "$d" ]; then linkdir=$d; break; fi
			done
			if [ -n "$linkdir" ]; then
				ln -sfn /etc/sv/prox "$linkdir/prox"
			fi
			sv up prox >/dev/null 2>&1 || sv restart prox >/dev/null 2>&1 || die "sv failed to start prox"
			;;
		*)
			warn "no supported init system found; start the daemon manually: $PREFIX/bin/proxd"
			DO_SERVICE=0
			;;
	esac
}

wait_ready() {
	i=0
	while [ "$i" -lt 60 ]; do
		if [ -S /run/prox/api.sock ] && "$PREFIX/bin/gw" ls >/dev/null 2>&1; then
			return 0
		fi
		i=$((i + 1))
		sleep 1
	done
	return 1
}

install_ca() {
	[ "$DO_CA" -eq 1 ] || return 0
	[ -S /run/prox/api.sock ] || { warn "daemon is not running; run '$PREFIX/bin/gw ca install' later"; return 0; }
	info "installing the prox local CA"
	"$PREFIX/bin/gw" ca install || warn "CA installation reported problems; run '$PREFIX/bin/gw ca install'"
	if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != "root" ] && command -v sudo >/dev/null 2>&1; then
		sudo -u "$SUDO_USER" -H "$PREFIX/bin/gw" ca install >/dev/null 2>&1 \
			|| warn "could not add the CA to $SUDO_USER's browser NSS database; run 'gw ca install' as that user"
	fi
}

doctor() {
	if ! "$PREFIX/bin/gw" ls >/dev/null 2>&1; then
		return 0
	fi
	info "running gw doctor"
	env DOCKER_HOST="unix:///run/prox/docker.sock" \
		PROX_HTTP_PORT="$HTTP_PORT" PROX_HTTPS_PORT="$HTTPS_PORT" \
		"$PREFIX/bin/gw" doctor || warn "gw doctor reported issues (see output above)"
}

uninstall() {
	info "stopping prox"
	if [ "$IS_ROOT" -eq 1 ]; then
	case "$(service_manager)" in
		systemd)
			systemctl disable --now prox.service >/dev/null 2>&1 || true
			rm -f /etc/systemd/system/prox.service
			systemctl daemon-reload >/dev/null 2>&1 || true
			;;
		openrc)
			rc-service prox stop >/dev/null 2>&1 || true
			rc-update del prox default >/dev/null 2>&1 || true
			rm -f /etc/init.d/prox
			;;
		runit)
			sv down prox >/dev/null 2>&1 || true
			for d in /var/service /etc/service /etc/runit/runsvdir/default; do
				rm -f "$d/prox"
			done
			rm -rf /etc/sv/prox
			;;
	esac
	fi

	if [ "$IS_ROOT" -eq 1 ] && [ -x "$PREFIX/bin/gw" ]; then
		if [ "$DO_CA" -eq 1 ]; then
			info "removing the CA from trust stores"
			"$PREFIX/bin/gw" ca uninstall >/dev/null 2>&1 || true
			if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != "root" ] && command -v sudo >/dev/null 2>&1; then
				sudo -u "$SUDO_USER" -H "$PREFIX/bin/gw" ca uninstall >/dev/null 2>&1 || true
			fi
		fi
	fi

	rm -f "$PREFIX/bin/prox" "$PREFIX/bin/gw" "$PREFIX/bin/proxd"
	rm -f "$PREFIX/share/bash-completion/completions/gw" \
		"$PREFIX/share/zsh/site-functions/_gw" \
		"$PREFIX/share/fish/vendor_completions.d/gw.fish"
	rm -rf "$PREFIX/share/doc/prox"
	if [ "$IS_ROOT" -eq 1 ]; then
		rm -f /etc/profile.d/prox.sh /etc/fish/conf.d/prox.fish
	fi

	if [ "$PURGE" -eq 1 ]; then
		info "purging state, rules and CA data"
		rm -rf /var/lib/prox /run/prox /etc/proxd /etc/prox
	fi
	info "prox uninstalled"
	[ "$PURGE" -eq 1 ] || info "state kept in /var/lib/prox; rules in /etc/proxd/rules.yaml (use --purge to remove)"
}

if [ "$ACTION" = "uninstall" ]; then
	uninstall
	exit 0
fi

[ "$PURGE" -eq 0 ] || die "--purge only makes sense with --uninstall"

info "installing prox on $DISTRO ($ARCH), prefix $PREFIX"
if [ "$IS_ROOT" -eq 1 ]; then
	mkdir -p /etc/proxd /etc/prox/profiles.d
fi
check_ports
build_binary "$TMP/prox"
install_binary "$TMP/prox"
install_docs
install_completions

if [ "$DO_SERVICE" -eq 1 ]; then
	install_service
else
	info "--no-service: installed the binary only"
fi

if [ "$DO_DOCKER_HOST" -eq 1 ]; then
	write_profile
	info "wrote /etc/profile.d/prox.sh (new shells will use DOCKER_HOST=unix:///run/prox/docker.sock)"
fi

if [ "$DO_SERVICE" -eq 1 ]; then
	if wait_ready; then
		info "prox daemon is running"
	else
		warn "prox daemon did not become ready"
		if [ -d /run/systemd/system ]; then
			journalctl -u prox --no-pager -n 60 2>/dev/null || true
		fi
		die "installation incomplete; inspect the service log"
	fi
	install_ca
	doctor
else
	info "start it with: $PREFIX/bin/proxd"
	if [ "$IS_ROOT" -eq 1 ]; then
		info "then: export DOCKER_HOST=unix:///run/prox/docker.sock"
	else
		user_runtime=${XDG_RUNTIME_DIR:-$HOME/.local/state}
		info "then: export DOCKER_HOST=unix://$user_runtime/prox/docker.sock"
	fi
fi

dashboard="https://gw.localhost"
[ "$HTTPS_PORT" = "443" ] || dashboard="$dashboard:$HTTPS_PORT"
installed_version=$("$PREFIX/bin/gw" version 2>/dev/null || version_string)
info "installed $installed_version at $PREFIX/bin/prox"
info "dashboard: $dashboard"
if [ "$IS_ROOT" -eq 1 ]; then
	info "docker:    export DOCKER_HOST=unix:///run/prox/docker.sock"
fi
info "uninstall: $SELF --uninstall"
