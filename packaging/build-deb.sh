#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT=$(pwd)
if ! command -v dpkg-deb >/dev/null 2>&1; then
  echo "dpkg-deb not found; skipping .deb build (use packaging/aur/PKGBUILD on Arch)"
  exit 0
fi
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo 0.1.0)}
VERSION=${VERSION#v}
if [[ ! "$VERSION" =~ ^[0-9] ]]; then
  VERSION="0.0.0+$VERSION"
fi
ARCH=$(dpkg --print-architecture 2>/dev/null || echo amd64)
STAGE="$ROOT/build/deb/prox_${VERSION}_${ARCH}"

rm -rf "$STAGE"
mkdir -p "$STAGE/DEBIAN" \
  "$STAGE/usr/bin" \
  "$STAGE/usr/lib/systemd/system" \
  "$STAGE/usr/lib/systemd/user" \
  "$STAGE/usr/share/bash-completion/completions" \
  "$STAGE/usr/share/zsh/site-functions" \
  "$STAGE/usr/share/fish/vendor_completions.d" \
  "$STAGE/usr/share/doc/prox" \
  "$STAGE/etc/prox/profiles.d" \
  "$STAGE/etc/proxd"

install -m 0755 "$ROOT/bin/prox" "$STAGE/usr/bin/prox"
ln -sf prox "$STAGE/usr/bin/gw"
ln -sf prox "$STAGE/usr/bin/proxd"
install -m 0644 "$ROOT/packaging/systemd/prox.service" "$STAGE/usr/lib/systemd/system/prox.service"
install -m 0644 "$ROOT/packaging/systemd/prox-user.service" "$STAGE/usr/lib/systemd/user/prox.service"
install -m 0644 "$ROOT/packaging/completions/gw.bash" "$STAGE/usr/share/bash-completion/completions/gw"
install -m 0644 "$ROOT/packaging/completions/_gw" "$STAGE/usr/share/zsh/site-functions/_gw"
install -m 0644 "$ROOT/packaging/completions/gw.fish" "$STAGE/usr/share/fish/vendor_completions.d/gw.fish"
install -m 0644 "$ROOT/README.md" "$STAGE/usr/share/doc/prox/README.md"
install -m 0644 "$ROOT/docs/USAGE.md" "$STAGE/usr/share/doc/prox/USAGE.md"
install -m 0644 "$ROOT/docs/ARCHITECTURE.md" "$STAGE/usr/share/doc/prox/ARCHITECTURE.md"
install -m 0644 "$ROOT/docs/DECISIONS.md" "$STAGE/usr/share/doc/prox/DECISIONS.md"

cat > "$STAGE/DEBIAN/control" <<EOF
Package: prox
Version: ${VERSION}
Section: net
Priority: optional
Architecture: ${ARCH}
Maintainer: prox contributors <github@haasele.org>
Depends: ca-certificates
Recommends: docker.io | docker-ce | moby-engine
Homepage: https://github.com/haasele/prox
Description: one global gateway for Docker containers and local dev processes
 prox runs a Docker API socket proxy, rewrites host port bindings, and routes
 every service through an embedded Caddy HTTPS/TCP gateway on stable
 *.localhost hostnames. Includes the gw CLI, a local CA, DNS, and dashboard.
EOF

cat > "$STAGE/DEBIAN/postinst" <<'EOF'
#!/bin/sh
set -e
if command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload >/dev/null 2>&1 || true
fi
cat <<'MSG'
prox installed.
  system daemon: sudo systemctl enable --now prox
  then:         export DOCKER_HOST=unix:///run/prox/docker.sock
  trust CA:     sudo gw ca install
  diagnose:     gw doctor
MSG
exit 0
EOF
chmod 0755 "$STAGE/DEBIAN/postinst"

mkdir -p "$ROOT/dist"
dpkg-deb --build --root-owner-group "$STAGE" "$ROOT/dist/prox_${VERSION}_${ARCH}.deb"
echo "built dist/prox_${VERSION}_${ARCH}.deb"
