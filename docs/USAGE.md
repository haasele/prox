# prox usage

This guide is written for the common Linux setup: a system daemon on ports
80/443 and `DOCKER_HOST` pointing at prox. A per-user install is shown where it
differs. All examples assume `prox` is on `PATH` (`proxd` is the same binary;
`gw` remains as a compatibility alias).

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
prox ca install        # system + Chrome/Firefox NSS; prints sudo commands if needed
prox doctor            # daemon, docker, DOCKER_HOST, CA, DNS, ports, enforcement
```

`prox doctor --fix` creates the `gw` network and configures split DNS for
systemd-resolved or NetworkManager when privileged. `*.localhost` often
resolves already; `.test` and other extra TLDs need split DNS or the optional
`/etc/hosts` fallback (off by default, manual):

```bash
sudo prox hosts sync     # writes a managed block for current routes + prox.localhost
```

## 2. An ad-hoc container

```bash
docker run -d --rm --name web -p 3000:3000 nginx
```

What happens:

- prox reads the image's `EXPOSE 80` (detection order: rule `--source`, label
  `gw.port`, image EXPOSE, `/proc/<pid>/net/tcp*` probe, first exposed port).
- the host binding is removed before Docker sees it; prox listens on the
  original host port itself and serves a hint page there;
- the container joins the `gw` bridge and gets `web.localhost`;
- Docker prints the route when the container is created:
  `WARNING: web: port 80/tcp routed by prox -> https://web.localhost`;
- opening the original URL (`http://localhost:3000`) shows
  “You're trying to access web” with the prox URL and redirects after two
  seconds;
- the dashboard and `prox ls` show the rewrite. Every route reports the covered
  container port (`source→gateway`), its public URL, and an event like
  `web: port 80/tcp routed by prox -> https://web.localhost`.

Set `PROX_ORIGINAL_HINTS=off` to skip the redirect listener and keep the host
port completely free.

```bash
curl --cacert ~/.local/state/prox/ca/pki/authorities/local/root.crt \
     --resolve web.localhost:443:127.0.0.1 https://web.localhost/
# or with split DNS: curl https://web.localhost/
prox ls
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
prox check compose.yml          # dry run without touching Docker
```

`prox check` parses with compose-go and prints each service's action, hostname,
container port, gateway port and rule, including collisions and busy ports.

Compose stability relies on two things: prox preserves
`com.docker.compose.config-hash` and presents the original `PortBindings` and
`ExposedPorts` in inspect/list responses. The container is actually attached to
`gw` and has no host bindings, but Compose sees the configuration it wrote.

## 4. Shared Postgres

```bash
# gateway port 5432, short hostname; source/target aliases are equivalent
prox allow postgres --source 5432 --target 5432 --resolve database
# identical:
prox allow postgres --container-port 5432 --gateway-port 5432 --resolve database

docker run -d --name db -e POSTGRES_PASSWORD=pass -p 5432:5432 postgres:17
psql "postgresql://postgres:pass@database.localhost:5432/postgres?sslmode=disable"
```

If 5432 is occupied, an explicit `--target 5432` returns a clear error and asks
for another port. Without an explicit target, the profile's preferred port is
used when free, otherwise the next free port from the configured range
(20000–29999 by default). `prox ls` always shows the assigned gateway port, the
covered container port and the public URL; a gateway port is never reassigned
while the route exists.

### TLS/SNI on 443

```bash
prox allow postgres --source 5432 --target 443 --resolve db
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
prox run -- npm run dev
# prox: port 41501/tcp routed by prox -> https://my-app.localhost  (vite, pid 12345)
# prox: http://localhost:5173 shows the hint page -> https://my-app.localhost
```

`prox run` knows the framework's conventional port (Vite 5173, Next/Nuxt 3000,
Astro 4321, Django/uvicorn 8000, Flask 5000, Rails 3000, Phoenix 4000, Go 8080,
generic 3000) and answers there with the same hint page. The application itself
gets a random free port, so the conventional port stays available for the hint.

### Pre-owning canonical ports

To make prox own a port even before the application starts, list it explicitly.
This is the “prox owns 3000/3306/5431” mode:

```bash
# HTTP ports answer with the hint page
PROX_OWN_HTTP_PORTS=3000,5173,8000,8080
# TCP ports forward to the registered app (postgres, mysql, redis, ...)
PROX_OWN_TCP_PORTS=3306,5431
```

```ini
# /etc/systemd/system/prox.service.d/own-ports.conf
[Service]
Environment=PROX_OWN_HTTP_PORTS=3000,5173,8000,8080
Environment=PROX_OWN_TCP_PORTS=3306,5431
```

Then `sudo systemctl daemon-reload && sudo systemctl restart prox`.

Only one process can own a port. prox must start before anything else that would
bind one of these ports; if the port is already taken, prox logs a warning and
keeps the others. Apps must be started through prox (`prox run`, the prox Docker
socket, or a rule-driven route); a plain `bun dev` that tries to bind an owned
3000 will fail instead of silently bypassing prox. HTTP ports show the hint page;
TCP ports forward transparently, so `psql -h 127.0.0.1 -p 5431` reaches the
routed database.

Servers started **without** `prox run` are still discovered when they listen on
loopback, a runtime such as Node/Bun/Python/Go/Rails/PHP (or a project directory
with a manifest), and a port below 20000 **after the daemon starts**. prox
creates a lease and a `*.localhost` route, e.g. `bun dev` in `~/src/my-app`
becomes `my-app.localhost`. That process already owns its original port, so prox
cannot answer there. Use `prox run` (or send Docker traffic through prox) if you
want the original-port hint page. Set `PROX_DISCOVER_EXISTING=1` to also scan
listeners that predate the daemon, and `PROX_DISCOVER=off` to disable the
scanner.

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
prox allow web --source 80 --resolve app
(cd project-a && docker compose up -d)
(cd project-b && docker compose up -d)
```

Both remain reachable:

- `app.localhost` (first route),
- `app.project-b.localhost` (renamed because `app` was taken).

A warning event is emitted, `prox ls` marks the route with `!`, and the dashboard
shows a collision warning. Names are never silently stolen. `prox check` predicts
the same collision before creating anything.

## 7. Policies and exceptions

```bash
prox policy set rewrite   # default: strip host ports and route
prox policy set deny      # reject creates that publish host ports without a rule
prox policy set warn      # let them through and emit a warning event
```

- `deny` returns a Docker API error with a suggested `prox allow` command.
- `warn` leaves the container untouched (it then appears as an unmanaged
  warning).
- A matching explicit rule always wins over the default policy.
- `prox allow nginx --publish` (or `--publish` on a service/profile) keeps the
  original host bindings. This is the documented escape hatch.
- `prox allow web --project shop --target 443` scopes a rule to one Compose
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

- Dashboard: `https://prox.localhost` (served by Caddy from embedded assets).
- Tabs: Applications, Rules, Activity, Settings. The URL hash follows the active
  tab, so `#rules`, `#activity` and `#settings` are linkable.
- Applications lists every route with its DNS name, source (container/listener)
  port, target/gateway port, upstream, status and actions; pre-owned HTTP/TCP
  ports are shown above the table. The search field filters applications by app
  name, DNS name, project or upstream.
- The New rule dialog creates and edits rules through `POST /v1/rules` and
  `PUT /v1/rules/{id}`; edit buttons on applications, unmanaged containers and
  events prefill it so the name, DNS name and ports stay editable. Rules apply to
  the next matching create or lease.
- Policy and Enforcement live in the Settings tab. Policy explains Rewrite,
  Warn and Deny; Enforcement explains that blocking direct host-port publishing
  from the raw Docker socket needs a Docker restart.
- Editing a container shows `route the running container now`. It creates a
  manual HTTP route to the container's published host port, so an existing
  container starts working without a recreate. Manual routes have a `remove`
  action in the Applications table.
- The `redirect HTTP to HTTPS` checkbox is stored on the rule. Uncheck it for
  an HTTP-only application; prox still terminates TLS on 443 for the named URL.
- Activity is the event stream; `prox events -f` follows the same events from the
  CLI. Unmanaged containers and warnings sit under Applications.
- The Unix-socket JSON API is the source of truth for CLI and dashboard; the
  dashboard is proxied over an ephemeral loopback listener.

## 9. Enforcement

```bash
prox enforce on
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
  them from `docker`. They can still use `prox` (both sockets are group-owned by
  `prox-users`), but the raw Docker socket is root-only.

`prox enforce off` stops the plugin. Enforcement fails closed: if the plugin
socket disappears while enabled, Docker denies requests.

## 10. Doctor and troubleshooting

```bash
prox doctor          # table of OK/WARN/FAIL
prox doctor --fix    # prox network, split DNS, CA install (when possible)
```

Checks: daemon reachable, Docker reachable, `DOCKER_HOST` points at prox, prox
network (Docker network `gw`), ports 80/443 owned by prox, CA trusted, embedded
DNS, system DNS, and enforcement.

Common fixes:

| Symptom | Fix |
| --- | --- |
| `Cannot connect to the Docker daemon` | daemon down; start proxd, or unset `DOCKER_HOST` to use the raw socket |
| browser TLS warning | `prox ca install` (and restart the browser) |
| `*.localhost` doesn't resolve | `prox doctor --fix`; browsers usually map `*.localhost` themselves |
| `.test` doesn't resolve | split DNS or the optional `/etc/hosts` fallback |
| port 80/443 in use | stop the other listener or set `PROX_HTTP_PORT` / `PROX_HTTPS_PORT` |
| route is 503 | container stopped/unhealthy; inspect `prox ls`, dashboard, events |
| Compose recreates containers | check that all Docker traffic used the prox socket; `prox events` shows rewrites |

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
| `PROX_ORIGINAL_HINTS` | true | answer HTTP original host ports with a redirect page |
| `PROX_OWN_HTTP_PORTS` | none | pre-own HTTP ports and answer with the hint page |
| `PROX_OWN_TCP_PORTS` | none | pre-own TCP ports and forward to the registered app |
| `PROX_DISCOVER` | true | find loopback HTTP dev servers and route them by name |
| `PROX_DISCOVER_EXISTING` | false | include listeners that predate the daemon |
| `PROX_SOCKET_GROUP` | `docker` (system) | group that can use the proxy/API sockets |
| `PROX_LOG_LEVEL` | info | debug/info/warn/error |
| `PROX_DOMAIN` | localhost | default hostname suffix |

## Podman

Podman is not a supported target. Its API compatibility is incomplete for the
create/inspect rewrite and its socket paths/config differ. You can experiment
with `podman system service --time=0 unix:///run/user/$UID/podman.sock` and
`PROX_REAL_DOCKER_SOCKET` pointing there, but networking and Compose behavior
are untested. Use native Docker for now.
