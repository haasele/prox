# Design decisions

Each decision favors visible, reversible behavior. Events and `prox ls` are the
primary way to see what prox did; nothing is silently rewritten or renamed.

## 1. One binary, one CLI name

`cmd/prox` is the same program for the CLI and the daemon. Invoked as `proxd`
(or `prox daemon`) it runs the daemon; otherwise it is the `prox` CLI.
Packaging installs one static binary plus a `proxd` symlink; `gw` remains as a
compatibility alias but is not what help, errors or docs show. This keeps
behavior identical in tests and installed systems and satisfies the
single-binary constraint without duplicating daemon code.

## 2. Caddy is the only public listener

Caddy is embedded (`caddy.Load` with JSON configuration), and every change is a
full config push. Caddy validates a new config before applying it; an invalid
push leaves the previous configuration running.

`caddy-l4` always owns the public HTTPS port, even when there are no SNI routes.
It forwards unmatched TLS to Caddy's HTTP server on a loopback port where TLS is
terminated. This avoids a listener handoff race when the first SNI route appears
or disappears. Plain HTTP runs on its own Caddy server with our own 308
redirects, so redirects work identically on non-standard test ports.

## 3. Docker API body shape

The Docker create API sends `Labels` and `ExposedPorts` at the top level of the
request body (not under `Config`). The first implementation read `Config` and
silently lost the `gw.*` labels; the proxy now reads/writes both shapes, with
top level authoritative. Inspect responses do nest these fields under `Config`.

## 4. Port detection order and probing

Per the spec, detection is: rule `--source`, label `gw.port`, image `EXPOSE`,
`/proc/<pid>/net/tcp*` probe, then the first exposed port. Bindings are only a
fallback hint. This is why `docker run -p 3000:3000 nginx` routes to nginx's real
port 80 instead of a non-existent container port 3000. If no port is known at
create time, a route with source port 0 is created; reconcile probes the
container's network namespace once it starts and updates the route. If nothing
listens, the route is marked down and serves the friendly 503 page.

## 5. Explicit vs preferred gateway ports

Profile-derived gateway ports (5432, 6379, …) are preferences: if the port is
busy, the allocator falls through to the configured range and `prox ls` shows the
actual port. An explicit `--target`/`--gateway-port` is a contract: if that port
is unavailable, the create fails with a clear error instead of silently using a
different port. SNI on 443 is never allocated as a gateway port.

To preserve this distinction after rule creation and YAML export, rules carry a
`target_explicit` column. A rule created without `--target` stores the profile's
preferred port but marks it non-explicit; re-imported YAML with a target is
treated as explicit because the file no longer records intent. This is visible
in behavior (`prox check` reports a busy preferred port and the create allocates
another) and in `prox ls` (the actual assigned port).

## 6. Compose stability

Two mechanisms keep Compose from recreating containers:

1. the original create body is stored per container ID;
2. inspect/list responses restore the original `HostConfig.PortBindings`,
   `PublishAllPorts`, `Config.ExposedPorts` and `HostConfig.NetworkMode`, and
   derive `NetworkSettings.Ports` from the original bindings.

Compose's `com.docker.compose.config-hash` label is untouched. This was tested
with two real Compose projects and repeated `up -d` runs.

## 7. Hostname collisions

Hostnames are allocated at route-creation time and persisted. The first route
keeps the short name; a later project gets
`<name>.<project>.localhost`, then numbered suffixes if necessary. The route is
marked `collision`, a warning event is emitted, and it stays renamed for the
life of the route — no silent promotion when the first container stops.

Compose recreations with the same container name take over the old route rather
than being treated as a collision. Destroy events remove routes immediately; a
90-second grace period only covers crash cleanup and racing creates.

## 8. Rules: precedence and defaults

Rules are scored by (project-scoped, match kind, rule ID), where match kind is
service (4) > container name (3) > image (2) > profile (1). Project-scoped
beats global; equal specificity resolves to the later rule. A deny rule at the
same specificity as a route rule therefore wins only if it is later — this keeps
the rule list comprehensible.

`--resolve` defaults to `<rule name>.localhost`, as specified. This means a
profile rule (`prox allow postgres`) intentionally produces
`postgres.localhost` for any matched container; use `--resolve` or a
project-scoped rule to get per-project names.

HTTP rules always terminate on 443. Non-443 `--target` for HTTP is rejected
with a suggestion to use TCP. `--source`/`--container-port` and
`--target`/`--gateway-port` are normalized in the CLI; passing both spellings is
an error.

## 9. Portless

The current [portless](https://github.com/vercel-labs/portless) project is a pre-1.0 Node CLI that
explicitly warns its state-directory format may change between releases. It
exposes user-facing commands and an MCP server, but no stable library or wire
contract intended for embedding by a Go daemon. prox therefore **reimplements
the concept natively** (`prox run`: port allocation, framework adapter table,
lease, route, PID watch) and does not depend on portless. If a stable backend
API appears, it can be added behind `internal/hostproc` without changing the
CLI.

## 10. Rootless and Docker Desktop

Container IPs are not reliably host-reachable under rootless Docker
(slirp4netns/pasta) or Docker Desktop. Those backends add a loopback-only
ephemeral publication for each routed container port at create time, resolve it
from `NetworkSettings.Ports`, and dial `127.0.0.1:<port>`. Inspect rewriting
hides these internal publications and presents the user's original bindings, so
Compose remains stable. Gateway ports above 1024 are preferred for rootless;
binding 80/443 requires capabilities or lowering
`net.ipv4.ip_unprivileged_port_start`.

Docker-socket discovery lives in `internal/config` because it must happen before
a backend is selected (rootless sockets under `$XDG_RUNTIME_DIR`, Desktop
sockets, `PROX_REAL_DOCKER_SOCKET`). `internal/backend.Backend` owns the two
backend-dependent operations: resolving a container port to a host-reachable
address and deciding whether internal loopback publications are required.

## 11. Enforcement

A real Docker daemon must be restarted to load an AuthZ plugin, so `prox enforce
on` never edits `/etc/docker/daemon.json` or restarts Docker on its own. It
starts the plugin socket and prints the exact daemon.json fragment and restart
command. Enforcement fails closed.

The plugin is implemented as a legacy v1 plugin (`/Plugin.Activate`,
`/AuthZPlugin.AuthZReq`, `/AuthZPlugin.AuthZRes`) because that is discoverable
with only a Unix socket. Prox-forwarded creates carry
`X-Prox-Signature: HMAC-SHA256(key, body)`; the key lives 0600 in the state
directory. Unsigned port-binding creates are allowed only by a `publish` rule.
The CI workflow exercises this against a real dockerd with
`authorization-plugins: ["prox-authz"]`.

## 12. CA trust without privilege

`prox ca install` installs into the system store when run as root (Debian,
Fedora, Arch paths), and into the current user's Chrome/Chromium NSS database
and Firefox profiles with `certutil`. It never invokes sudo or escalates. If
root is missing it prints copy-paste commands and exits successfully, so
`prox doctor` can point at the remaining manual step.

## 13. DNS

The embedded resolver listens on a high port (default 5335) on loopback and
answers `*.localhost` plus configured extra TLDs (e.g. `test`) with 127.0.0.1 /
::1. `prox doctor --fix` writes a systemd-resolved drop-in or NetworkManager
dnsmasq config and prints commands when unprivileged. An `/etc/hosts` fallback
helper exists but is **off by default**; it is not part of normal startup.
Browsers already map `*.localhost` to loopback, which makes the common case work
even without split DNS.

Containers get `prox.localhost` and `host.docker.internal` via `ExtraHosts` at
create time, plus currently known route hostnames mapped to the `gw` gateway.
Docker's own DNS continues to serve Compose service names; prox does not replace
it.

## 14. Rules YAML

SQLite is authoritative. YAML at `/etc/proxd/rules.yaml` (system) or
`~/.config/prox/rules.yaml` (user) is imported only when the rules table is
empty, and is rewritten after every rule change. This avoids a YAML file
clobbering CLI-created rules on startup while still giving users a reviewable
file.

## 15. Dashboard and API

The JSON API and SSE stream live on a 0600 Unix socket. The dashboard's static
assets are embedded; Caddy proxies `prox.localhost` to an ephemeral loopback
listener serving the same handler. There is no separate frontend build or
network dependency. The dashboard is tabbed (Applications, Rules, Activity);
the Applications tab has search, route actions and unmanaged containers, while
rule create/edit uses a dialog. It calls the same rules and manual-route APIs as
the CLI; suggestions are computed server-side from the image profile.

## 16. Testing strategy

- Unit tests cover matcher precedence, alias normalization, hostname
  sanitization/collisions, profiles and overrides, adapters, proxy rewriting,
  inspect restoration, AuthZ decisions, DNS, compose prediction, backends and
  config.
- Real-Docker e2e tests are behind the `e2e` build tag and use the actual
  compiled binary as a subprocess per test. This avoids Caddy's process-global
  state and tests graceful startup/shutdown. `prox run` is covered with both a
  plain Node fixture and a real Vite 7 dev server.
- The e2e suite covers all acceptance criteria except raw-socket enforcement,
  which needs a dockerd restart. `test/e2e/authz-ci.sh` performs that on CI with
  root. `test/e2e/doctor-ci.sh` installs on 80/443 and asserts a healthy doctor.
- The Compose fixture deliberately maps the same host port in two projects;
  both start because the binding never reaches Docker.

## 17. Original-port hints and dev-server discovery

`localhost:3000` can only answer a hint page if some process listens on
`127.0.0.1:3000`. For containers created through the prox socket, prox strips
the Docker host binding, so the port is free and prox binds it itself. The hint
page names the application and redirects to its `*.localhost` URL. This is on by
default for HTTP routes (`PROX_ORIGINAL_HINTS=off` disables it) and is
best-effort: if another process owns the port, the hint is skipped and reported
once as an event.

Rules apply at container create. For an already-running container, the
dashboard can add a manual HTTP route (`POST /v1/routes`) to the published host
port. The rule covers future creates and the manual route covers the process
that is running now; removing the route does not remove the rule.

`PROX_OWN_HTTP_PORTS` and `PROX_OWN_TCP_PORTS` extend that to a broker model:
prox binds the listed ports at startup, before an app can claim them. HTTP ports
start with a “prox owns this port” page and switch to the app-specific redirect
when a route registers. TCP ports forward to the route's target once it is up;
before that they accept and explain that no app is registered. This is opt-in
because it deliberately takes ports away from unrelated processes: only one
process can own a port, so the unit must list exactly the ports the workstation
wants prox to arbitrate, and prox must start first.

Host dev servers that were started directly already own their port. prox cannot
bind it simultaneously, so discovery (`PROX_DISCOVER`, default on) scans
loopback listeners from known runtimes or project directories, creates a lease,
and routes the app at `myapp.localhost`. The original port stays the app. Use
`prox run` to make the conventional port free for the hint page, because it
starts the app on a random port.

Discovery only considers ports below 20000, loopback listeners, non-system
working directories, known runtimes or directories with a project manifest, and
processes started after the daemon (use `PROX_DISCOVER_EXISTING=1` to include
older ones). It is a convenience for a developer workstation; disable it on
shared machines because discovered services become reachable through the
gateway.

## 18. Not supported / known limits

- Podman is not a target (API and networking differences).
- Docker Desktop is implemented behind the backend interface but the primary
  CI path is native Linux Docker.
- A Docker daemon restart is handled by reconnect+reconcile. Restarting the
  shared Docker daemon is disruptive, so the main e2e suite only restarts proxd;
  a dedicated CI job runs `TestDockerRestart` with
  `PROX_E2E_DOCKER_RESTART=1` and passwordless sudo.
- Wildcard hostnames (`*.foo.localhost`) are not supported; rules resolve
  concrete hosts.
- Rules are evaluated at container create. A running container that was already
  rewritten does not change hostname when a rule is added; recreate it to apply
  the rule. This avoids mutating live routes behind the user's back.
- UDP is not proxied. Only TCP/HTTP routes are created.
- A process that already owns a port cannot be intercepted without killing or
  restarting it. Discovery can name such a server but cannot answer on its
  original port; `prox run`, Docker through the prox socket, or
  `PROX_ORIGINAL_HINTS` on a port prox owns are the ways to get the hint page.
- The dashboard has no authentication beyond the loopback listener and Caddy's
  local access; do not expose port 443 to an untrusted network with prox in its
  default configuration.
