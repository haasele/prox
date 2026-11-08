# prox usage

This guide is written for the common Linux setup: a system daemon on ports
80/443 and `DOCKER_HOST` pointing at prox. A per-user install is shown where it
differs. All examples assume `gw` is on `PATH` (`prox` and `proxd` are the same
binary).

## 1. Start the daemon and point Docker at it

System install:

```bash
sudo systemctl enable --now prox
export DOCKER_HOST=unix:///run/prox/docker.sock
```

User install:

```bash
systemctl --user enable --now prox
export DOCKER_HOST=unix://${XDG_RUNTIME_DIR}/prox/prox.sock
# unprivileged ports default to 80/443; use drop-ins if needed:
#   Environment=PROX_HTTP_PORT=8080
#   Environment=PROX_HTTPS_PORT=8443
```

Add the export to your shell profile if you want every Docker command to go
through prox. The raw socket remains reachable:

```bash
DOCKER_HOST=unix:///var/run/docker.sock docker ps   # bypass prox
```

Trust the local CA once:

```bash
gw ca install        # system + Chrome/Firefox NSS; prints sudo commands if needed
gw doctor            # daemon, docker, DOCKER_HOST, CA, DNS, ports, enforcement
```

`gw doctor --fix` creates the `gw` network and configures split DNS for
systemd-resolved or NetworkManager when privileged. `*.localhost` often
resolves already; `.test` and other extra TLDs need split DNS or the optional
`/etc/hosts` fallback (off by default, manual):

```bash
sudo gw hosts sync     # writes a managed block for current routes + gw.localhost
```

## 2. An ad-hoc container

```bash
docker run -d --rm --name web -p 3000:3000 nginx
```

What happens:

- prox reads the image's `EXPOSE 80` (detection order: rule `--source`, label
  `gw.port`, image EXPOSE, `/proc/<pid>/net/tcp*` probe, first exposed port).
- the host binding is removed before Docker sees it; nothing binds host 3000;
- the container joins the `gw` bridge and gets `web.localhost`;
- the dashboard and `gw ls` show the rewrite.

```bash
curl --cacert ~/.local/state/prox/ca/pki/authorities/local/root.crt \
     --resolve web.localhost:443:127.0.0.1 https://web.localhost/
# or with split DNS: curl https://web.localhost/
gw ls
```

If the image did not expose a port and starts listening later, prox probes the
container network namespace at start and fills in the port.

## 3. Compose project

No Compose changes are needed:

```yaml
name: shop
services:
  web:
    build: .
    ports: ["80:80"]
    depends_on: [api]
  api:
    image: node:22
    command: npm start
    ports: ["3000:3000"]
    environment: { PORT: 3000 }
  db:
    image: postgres:17
    environment: { POSTGRES_PASSWORD: pass }
    ports: ["5432:5432"]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres"]
      interval: 2s
      retries: 20
```

```bash
docker compose up -d
# web.shop.localhost, api.shop.localhost
# db.shop.localhost:5432 (gateway port chosen by the postgres profile)
docker compose up -d          # second run: up-to-date, no recreation
gw check compose.yml          # dry run without touching Docker
```

`gw check` parses with compose-go and prints each service's action, hostname,
container port, gateway port and rule, including collisions and busy ports.

Compose stability relies on two things: prox preserves
`com.docker.compose.config-hash` and presents the original `PortBindings` and
`ExposedPorts` in inspect/list responses. The container is actually attached to
`gw` and has no host bindings, but Compose sees the configuration it wrote.

## 4. Shared Postgres

```bash
# gateway port 5432, short hostname; source/target aliases are equivalent
gw allow postgres --source 5432 --target 5432 --resolve database
# identical:
gw allow postgres --container-port 5432 --gateway-port 5432 --resolve database

docker run -d --name db -e POSTGRES_PASSWORD=pass -p 5432:5432 postgres:17
psql "postgresql://postgres:pass@database.localhost:5432/postgres?sslmode=disable"
```

If 5432 is occupied, an explicit `--target 5432` returns a clear error and asks
for another port. Without an explicit target, the profile's preferred port is
used when free, otherwise the next free port from the configured range
(20000–29999 by default). `gw ls` always shows the assigned port; a gateway
port is never reassigned while the route exists.

### TLS/SNI on 443

```bash
gw allow postgres --source 5432 --target 443 --resolve db
```

TLS clients that send SNI `db.localhost` are passed through to the container's
TLS listener on 443. With PostgreSQL 17:

```bash
psql "postgresql://postgres:pass@db.localhost:443/postgres?sslmode=require&sslnegotiation=direct"
```

Protocols without a routable SNI name (redis, memcached) reject `--target 443`
with an explanatory error and suggest a dedicated gateway port:

```
redis cannot be SNI-routed on port 443: protocol "redis" does not carry a
routable SNI name. Omit --target/--gateway-port to get a dedicated gateway
port (e.g. --target 6379)
```

A non-TLS client that opens plaintext to 443 reaches the HTTPS terminator and
gets a TLS failure (visible in daemon logs); use the dedicated TCP port for
those protocols.

## 5. Local dev server

```bash
cd ~/src/my-app
gw run -- npm run dev
# prox: https://my-app.localhost -> 127.0.0.1:41501  (vite, pid 12345)
```

- The hostname defaults to the directory name, shortened/renamed with
  `--resolve app`.
- A free loopback port is injected via `PORT`, `HOST=127.0.0.1` and framework
  flags (Vite/SvelteKit `--port/--host`, Next `-p/-H`, Nuxt, Astro, Rails,
  Django positional, Flask, uvicorn, Laravel; Express, Phoenix and Go are
  env-only).
- Vite gets `__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS` so its host check accepts
  the `.localhost` name. WebSocket HMR works through Caddy.
- Override flag generation with `--port-flag '--listen={host}:{port}'`
  (`{port}` and `{host}` are substituted; npm/pnpm get a `--` separator).
- A lease is registered; the daemon watches the PID (pidfd where available) and
  removes the route when the process exits. Orphans are reaped on restart.

## 6. Two projects, one short name

```bash
gw allow web --source 80 --resolve app
(cd project-a && docker compose up -d)
(cd project-b && docker compose up -d)
```

Both remain reachable:

- `app.localhost` (first route),
- `app.project-b.localhost` (renamed because `app` was taken).

A warning event is emitted, `gw ls` marks the route with `!`, and the dashboard
shows a collision warning. Names are never silently stolen. `gw check` predicts
the same collision before creating anything.

## 7. Policies and exceptions

```bash
gw policy set rewrite   # default: strip host ports and route
gw policy set deny      # reject creates that publish host ports without a rule
gw policy set warn      # let them through and emit a warning event
```

- `deny` returns a Docker API error with a suggested `gw allow` command.
- `warn` leaves the container untouched (it then appears as an unmanaged
  warning).
- A matching explicit rule always wins over the default policy.
- `gw allow nginx --publish` (or `--publish` on a service/profile) keeps the
  original host bindings. This is the documented escape hatch.
- `gw allow web --project shop --target 443` scopes a rule to one Compose
  project. Project-scoped rules beat global; service matches beat image, image
  beats profile; later rules win ties.
- Rules and the default policy are evaluated on **container create**. Existing
  rewritten containers keep the route they were created with; recreate the
  container (or `docker compose up --force-recreate`) to apply a new rule.

Rules are stored in SQLite and mirrored to YAML:

- system: `/etc/proxd/rules.yaml`
- user: `~/.config/prox/rules.yaml`
- imported automatically when the database has no rules
- exported after each CLI/API change

## 8. Dashboard, events, API

- Dashboard: `https://gw.localhost` (served by Caddy from embedded assets).
- Routes with open/connect links, rules, leases, unmanaged containers,
  collision warnings, live SSE events, one-click allow.
- `gw events -f` follows the same event stream.
- The Unix-socket JSON API is the source of truth for CLI and dashboard; the
  dashboard is proxied over an ephemeral loopback listener.

## 9. Enforcement

```bash
gw enforce on
```

This starts a legacy Docker AuthZ plugin at
`/run/docker/plugins/prox-authz.sock` and prints the two daemon steps:

```json
// /etc/docker/daemon.json
{ "authorization-plugins": ["prox-authz"] }
```

```bash
sudo systemctl restart docker
```

After Docker restarts with the plugin:

- creates sent to `/var/run/docker.sock` that publish host ports are denied
  unless they carry prox's HMAC signature or match a `publish` rule;
- creates through the prox socket are signed and allowed;
- for full lock-down, create a `prox-users` group, set
  `PROX_SOCKET_GROUP=prox-users` in the prox unit, add users to it, and remove
  them from `docker`. They can still use `gw` (both sockets are group-owned by
  `prox-users`), but the raw Docker socket is root-only.

`gw enforce off` stops the plugin. Enforcement fails closed: if the plugin
socket disappears while enabled, Docker denies requests.

## 10. Doctor and troubleshooting

```bash
gw doctor          # table of OK/WARN/FAIL
gw doctor --fix    # gw network, split DNS, CA install (when possible)
```

Checks: daemon reachable, Docker reachable, `DOCKER_HOST` points at prox, `gw`
network, ports 80/443 owned by prox, CA trusted, embedded DNS, system DNS, and
enforcement.

Common fixes:

| Symptom | Fix |
| --- | --- |
| `Cannot connect to the Docker daemon` | daemon down; start proxd, or unset `DOCKER_HOST` to use the raw socket |
| browser TLS warning | `gw ca install` (and restart the browser) |
| `*.localhost` doesn't resolve | `gw doctor --fix`; browsers usually map `*.localhost` themselves |
| `.test` doesn't resolve | split DNS or the optional `/etc/hosts` fallback |
| port 80/443 in use | stop the other listener or set `PROX_HTTP_PORT` / `PROX_HTTPS_PORT` |
| route is 503 | container stopped/unhealthy; inspect `gw ls`, dashboard, events |
| Compose recreates containers | check that all Docker traffic used the prox socket; `gw events` shows rewrites |

When prox is down, `DOCKER_HOST=unix:///run/prox/docker.sock` fails fast with a
clear connection error. Fall back with:

```bash
export DOCKER_HOST=unix:///var/run/docker.sock
```

## 11. Backends

`PROX_BACKEND=auto|native|rootless|desktop`:

- **native** — container IPs on the `gw` bridge are dialed directly.
- **rootless** — detected from Docker security options; proj adds
  loopback-only ephemeral publications internally and dials `127.0.0.1`.
  Gateway ports above 1024; for 80/443 either grant
  `CAP_NET_BIND_SERVICE` or set
  `net.ipv4.ip_unprivileged_port_start=0`.
- **desktop** — Docker Desktop on Linux; same loopback publication strategy
  because container IPs are not host-reachable. All of this stays behind
  `internal/backend`.

The inspect view still hides these internal publications and shows the original
Compose ports.

## 12. Config reference

| Variable | Default | Meaning |
| --- | --- | --- |
| `PROX_SYSTEM` | root? | use `/var/lib/prox`, `/run/prox`, `/etc/proxd` |
| `PROX_STATE_DIR` | XDG state | SQLite, CA, keys |
| `PROX_RUNTIME_DIR` | XDG runtime | proxy/API sockets |
| `PROX_RULES_FILE` | `/etc/proxd/rules.yaml` or `~/.config/prox/rules.yaml` | rule mirror |
| `PROX_PROFILES_DIR` | `/etc/prox/profiles.d` or `~/.config/prox/profiles.d` | user profiles |
| `PROX_REAL_DOCKER_SOCKET` | `/var/run/docker.sock` | upstream Docker socket |
| `PROX_DOCKER_SOCKET` | runtime/docker.sock | proxy socket |
| `PROX_API_SOCKET` | runtime/api.sock | CLI/API socket |
| `PROX_AUTHZ_SOCKET` | `/run/docker/plugins/prox-authz.sock` | AuthZ plugin |
| `PROX_HTTP_PORT` / `PROX_HTTPS_PORT` | 80 / 443 | public listeners |
| `PROX_DNS_PORT` | 5335 | embedded resolver port |
| `PROX_EXTRA_TLDS` | none | e.g. `test` (comma-separated) |
| `PROX_GATEWAY_PORT_START/END` | 20000/29999 | TCP gateway range |
| `PROX_BACKEND` | auto | native/rootless/desktop |
| `PROX_ENFORCE` | false | start the plugin on boot |
| `PROX_SOCKET_GROUP` | `docker` (system) | group that can use the proxy/API sockets |
| `PROX_LOG_LEVEL` | info | debug/info/warn/error |
| `PROX_DOMAIN` | localhost | default hostname suffix |

## Podman

Podman is not a supported target. Its API compatibility is incomplete for the
create/inspect rewrite and its socket paths/config differ. You can experiment
with `podman system service --time=0 unix:///run/user/$UID/podman.sock` and
`PROX_REAL_DOCKER_SOCKET` pointing there, but networking and Compose behavior
are untested. Use native Docker for now.
