# prox

One global HTTPS/TCP gateway for Docker containers and local dev processes.

`prox` makes every service reachable at a stable `*.localhost` hostname and
keeps host ports mine — port 3000, 5432, 8080 and friends stop colliding between
projects. It works by proxying the Docker API socket, rewriting
`POST /containers/create` in flight, and routing traffic through an embedded
Caddy instance. No Compose file or Dockerfile changes are required.

```
docker run -p 3000:3000 nginx      ->  https://my-container.localhost
compose service web:80             ->  https://web.shop.localhost
compose service db:5432            ->  db.shop.localhost:5432 (or SNI on 443)
npm run dev                        ->  https://my-project.localhost
```

## What you get

- **Docker socket proxy** — `DOCKER_HOST=unix:///run/prox/docker.sock` is all it
  takes. Streaming, hijacked `exec -it`, `attach`, `logs -f`, `cp`, `build` and
  `events` pass through unchanged. Create bodies are rewritten and inspect/list
  responses are restored so `docker compose up` stays up-to-date.
- **Policy engine** — default `rewrite`, opt-in `deny` and `warn`; per-service,
  per-project, per-image and per-profile rules. Every rewrite, rename and denial
  is an event in `gw ls`, `gw events` and the dashboard.
- **Stable hostnames** — `<service>.<project>.localhost` by default, short
  `database.localhost` names with rules. Collisions append the project instead
  of stealing a name.
- **HTTPS everywhere** — local CA, on-demand certificates, HTTP→HTTPS redirects,
  and a friendly 503 page for stopped/unhealthy containers.
- **TCP + SNI** — Postgres, MySQL, Mongo, Redis, RabbitMQ and friends get
  dedicated gateway ports; TLS protocols can share 443 via SNI (Postgres 17
  `sslnegotiation=direct` works).
- **Host processes** — `gw run -- npm run dev` allocates a port, detects Vite,
  Next, Nuxt, SvelteKit, Astro, Rails, Django, Flask, FastAPI, Express, Phoenix,
  Laravel and Go, registers a lease, and removes the route when the process
  exits.
- **Dashboard** — `https://gw.localhost` shows routes, rules, leases, collisions,
  live events and one-click allow buttons.
- **Enforcement** — an opt-in Docker AuthZ plugin rejects direct port binding
  from the raw socket; the proxy signs its rewritten requests.

## Install

### One command, any Linux distro

```bash
git clone https://github.com/haasele/prox.git
cd prox
./install.sh
```

The installer elevates itself with sudo when needed and:

- builds a static binary (or uses `--binary PATH` / a release download),
- installs `/usr/local/bin/{prox,gw,proxd}`,
- creates and starts a systemd, OpenRC or runit service on 80/443,
- installs the local CA into the system store and (when run via sudo) the
  invoking user's Chrome/Firefox NSS databases,
- writes `/etc/profile.d/prox.sh` and a fish snippet so new shells set
  `DOCKER_HOST=unix:///run/prox/docker.sock`,
- installs shell completions and documentation.

Common options:

```bash
./install.sh --no-docker-host          # install without changing DOCKER_HOST
./install.sh --http-port 8080 --https-port 8443
./install.sh --no-service              # binary only
./install.sh --binary ./prox           # skip the Go build
./install.sh --uninstall               # stop + remove; --purge also wipes state
```

It works on systemd distributions (Debian/Ubuntu, Fedora/RHEL, Arch, openSUSE,
…) and falls back to OpenRC or runit when detected. CA trust-store paths cover
Debian/Ubuntu, Fedora/RHEL, Arch, openSUSE and Alpine, plus NSS. If no Go
toolchain is present, pass `--binary` or use a release asset
(`prox-linux-amd64` / `prox-linux-arm64`).

### Packages from source

```bash
# Debian/Ubuntu (build locally or download the release artifact)
make package && sudo dpkg -i dist/prox_*.deb

# Arch
cd packaging/aur && makepkg -si

# macOS / Linuxbrew
brew install --build-from-source ./packaging/homebrew/prox.rb

# From source
make build        # bin/prox, with bin/gw and bin/proxd wrappers
```

System service (native Linux Docker, binds 80/443):

```bash
sudo systemctl enable --now prox
export DOCKER_HOST=unix:///run/prox/docker.sock
sudo gw ca install
gw doctor
```

User service (no root; use high ports unless
`net.ipv4.ip_unprivileged_port_start=0`):

```bash
install -Dm755 bin/prox ~/.local/bin/proxd
ln -sf proxd ~/.local/bin/prox
ln -sf proxd ~/.local/bin/gw
mkdir -p ~/.config/systemd/user
cp packaging/systemd/prox-user.service ~/.config/systemd/user/prox.service
systemctl --user enable --now prox
export DOCKER_HOST=unix://$XDG_RUNTIME_DIR/prox/prox.sock
gw ca install
```

`gw ca install` installs the CA into the system store and Chrome/Firefox NSS
databases. Without root it prints the exact commands instead of failing. The
system daemon's Docker proxy and API sockets are group-owned by `docker`
(mode 0660), so existing Docker users can use `gw` without sudo.

## Quick start

```bash
# terminal 1: daemon
proxd

# terminal 2: everything Docker does goes through prox
export DOCKER_HOST=unix://$XDG_RUNTIME_DIR/prox/prox.sock
docker run -d --name web -p 3000:3000 nginx
# nginx is really on container port 80; prox detects EXPOSE 80
curl --cacert ~/.local/state/prox/ca/pki/authorities/local/root.crt \
     --resolve web.localhost:443:127.0.0.1 https://web.localhost/

gw ls
gw events -f
open https://gw.localhost/
```

## CLI at a glance

```
gw allow <name> [--source N | --container-port N]
                [--target N | --gateway-port N]
                [--resolve HOST] [--project P]
                [--proto http|tcp] [--publish]
gw deny  <name> [--project P]
gw ls
gw rm <id>                       # rN rules, tN routes, lN leases
gw check <compose.yml>           # dry-run every service
gw run [--resolve H] -- <cmd...>
gw events [-f]
gw policy set rewrite|deny|warn
gw ca install | uninstall | show
gw doctor [--fix]
gw enforce on|off|status
```

See [docs/USAGE.md](docs/USAGE.md) for complete scenarios and
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the design.

## How it works

Four parts run inside one daemon:

1. a Unix-socket Docker API proxy that rewrites creates and restores inspect
   responses;
2. a policy engine backed by SQLite and YAML profiles;
3. an embedded Caddy router (HTTP + caddy-l4 for TCP/SNI);
4. a JSON API on a Unix socket that serves the CLI and dashboard.

Full diagrams are in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Compatibility

- Native Linux Docker is the primary target.
- Rootless Docker and Docker Desktop are supported through the backend
  interface (loopback-only internal publications; container IPs are not
  assumed reachable).
- Podman is **not** a supported target; notes are in
  [docs/USAGE.md](docs/USAGE.md#podman).
- HTTP/HTTPS ports default to 80/443 and are configurable
  (`PROX_HTTP_PORT`, `PROX_HTTPS_PORT`), which the test suite uses.

## Development

```bash
make test     # unit tests
make e2e      # real Docker acceptance suite (build tag e2e)
make lint     # gofmt + go vet
make package  # deb + tarball + shell completions
```

The end-to-end suite covers every acceptance criterion, including two Compose
projects on 80/443, Postgres TCP + SNI, Vite HMR, Compose no-recreate, daemon
restart, proxy conformance, collision naming, deny/publish policies and the
AuthZ plugin. CI additionally runs enforcement against a real dockerd and
`gw doctor` on 80/443 as root.

## License

MIT. See [LICENSE](LICENSE).
