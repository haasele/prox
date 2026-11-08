# prox architecture

## Components

```mermaid
flowchart LR
  subgraph host["Host"]
    CLI["gw CLI"]
    Browser["Browser / curl"]
    DockerCLI["docker / compose CLI"]
    DB["postgres client"]

    subgraph proxd["proxd"]
      Proxy["Docker API proxy\nunix socket"]
      Policy["Policy engine\nrules + profiles"]
      Reg[("SQLite\nroutes · leases · events\ncontainer originals")]
      API["JSON API\nunix socket + SSE"]
      Web["Dashboard\nembedded assets"]
      DNS["DNS resolver\n*.localhost"]
      CA["Caddy PKI\nlocal CA"]
      Router["Embedded Caddy\nHTTP + layer4"]
    end

    Docker["dockerd\n/var/run/docker.sock"]
    Containers["containers\non gw bridge"]
    Dev["host dev process\n127.0.0.1:port"]
  end

  CLI -->|JSON/SSE| API
  Browser -->|HTTPS| Router
  Router --> Web
  Web --- API
  DockerCLI -->|DOCKER_HOST| Proxy
  Proxy -->|rewritten API| Docker
  Docker --> Containers
  Proxy <--> Policy
  Policy <--> Reg
  API <--> Reg
  Router <--> Reg
  Router -->|HTTP / TCP / SNI| Containers
  Router -->|HTTP| Dev
  DNS -.-> Browser
  CA --- Router
  DB -->|gateway port / SNI 443| Router
```

Everything except Caddy's public listeners and the Docker proxy socket lives
behind local sockets. SQLite is the source of truth; Caddy configuration is
derived from the route table on every change and re-applied atomically through
`caddy.Load`.

## Container create path

```mermaid
sequenceDiagram
  participant C as docker compose/run
  participant P as prox Docker proxy
  participant E as policy engine
  participant D as dockerd
  participant R as registry (SQLite)
  participant Y as Caddy

  C->>P: POST /containers/create (+PortBindings)
  P->>P: parse Labels (top level!), image, ports
  P->>E: Decide(CreateInfo)
  E->>R: rules + policy + profiles
  E-->>P: decision (route-http / route-tcp / publish / deny / warn)
  alt deny
    P-->>C: 403 {message: "policy is 'deny' … gw allow …"}
  else warn / skip
    P->>D: original create
    P->>R: event "warn"
  else publish
    P->>D: create with original bindings + gw labels
  else route
    P->>P: strip PortBindings, attach gw network, add labels
    P->>R: reserve gateway port / resolve host collision
    P->>D: rewritten create
    D-->>P: {Id}
    P->>R: container original body + pending route
    P-->>C: response
  end
  Note over P,Y: reconcile/start event resolves the container IP and syncs Caddy
```

The original create JSON is stored per container. `GET /containers/{id}/json`
and `GET /containers/json` inject the original `HostConfig.PortBindings`,
`PublishAllPorts`, `ExposedPorts` and `NetworkMode`, plus a derived
`NetworkSettings.Ports`. This is what makes Compose's second `up` report
`Running` instead of recreating.

## Start / stop / reconcile

```mermaid
flowchart TD
  Boot["daemon start"] --> Net["ensure gw bridge"]
  Net --> Snap["snapshot all containers"]
  Snap --> Rec["reconcile"]
  Rec --> Routes["upsert routes\nstatus up/down/pending"]
  Routes --> Sync["Caddy config sync"]
  Ev["Docker events\nstart · die · destroy · health"] --> Deb["debounced reconcile"]
  Deb --> Rec
  Boot --> Watch["event watcher\nreconnect backoff"]
  Watch --> Ev
  Timer["lease reaper\npidfd / kill(0)"] --> Leases["remove dead leases + routes"]
  Leases --> Sync
```

Reconcile is idempotent. It:

1. creates the `gw` network if missing;
2. verifies managed containers, attaches `gw` when needed, resolves
   backend-specific addresses, probes `/proc/<pid>/net/tcp*` for a port when
   detection was deferred, and marks unhealthy/stopped routes down;
3. removes routes and stored create bodies for containers that no longer exist
   (destroy events do this immediately; a grace period covers racing creates);
4. keeps gateway-port reservations aligned with live TCP routes;
5. reaps leases whose PID is gone;
6. recomputes unmanaged warnings and pushes a single Caddy config.

Destroy events remove routes immediately. Compose recreates reuse the same
container name; if the old route is still present, the proxy reassigns it
instead of renaming the hostname.

## Caddy layout

Layer4 always owns the public HTTPS port so a reload can move between "no SNI
routes" and "SNI routes" without a bind race.

```mermaid
flowchart LR
  Client443["client :443"] --> L4["layer4 front\nSNI matcher"]
  L4 -->|SNI matches a TCP rule| Pg["container:5432\n(passthrough TLS)"]
  L4 -->|default raw TLS| Loop["127.0.0.1:https+1\nCaddy HTTP server"]
  Loop -->|TLS termination\ninternal CA| Routes["host routes"]
  Routes -->|reverse_proxy| Up["container IP:port\nor 127.0.0.1:port"]
  Routes -->|down| Page["friendly 503 page"]
  Routes -->|gw.localhost| Dash["dashboard\nloopback HTTP"]
  Client80["client :80"] --> HTTP["Caddy HTTP server"]
  HTTP -->|default| Redir["308 → https"]
  HTTP -->|rule redirect=false| Up
  TCP["client :5432 etc"] --> L4Port["layer4 gateway per port"]
  L4Port --> Up
```

Certificates come from Caddy's internal PKI. Every route hostname (plus
`gw.localhost`) is in an explicit automation policy using the `internal` issuer,
so certificates are issued per hostname without public ACME. The root CA is
created on first boot at:

```
<state>/ca/pki/authorities/local/root.crt
```

## Policy resolution

```mermaid
flowchart LR
  Create["create info\nservice · project · image · ports · labels"] --> Match{"matching rules"}
  Match -->|"score = project-scoped, match kind, id"| Best["best rule"]
  Best -->|deny| Deny["reject"]
  Best -->|publish| Publish["keep host ports"]
  Best -->|route-http| HTTP["host + container port + 443"]
  Best -->|route-tcp| TCP["host + gateway port\nor SNI 443"]
  Match -->|none| Default{"policy"}
  Default -->|rewrite| Auto["TCP profile → gateway port\nelse HTTP route"]
  Default -->|warn| Warn["leave bindings + event"]
  Default -->|deny| Deny
```

Match kinds, most specific first: service name, container name, image name,
profile name. Project-scoped rules beat global; within the same specificity,
later rules beat earlier ones.

Hostname collisions are resolved at route creation: the first route keeps the
short name, subsequent projects get `<name>.<project>.localhost` (or `-2`
suffixes if that is also taken), a warning event is emitted, and the renamed
route is marked `collision` in SQLite.

## Data model

```mermaid
erDiagram
  RULES {
    int id PK
    string name
    string project
    int source_port
    int target_port
    string resolve
    string proto
    string action
    bool redirect
  }
  ROUTES {
    int id PK
    string host
    string container_id
    string project
    string service
    string proto
    int source_port
    int gateway_port
    string target_addr
    int rule_id
    string status
    bool collision
  }
  LEASES {
    int id PK
    string host
    int pid
    int port
    string dir
    string command
  }
  EVENTS {
    int id PK
    int ts
    string level
    string kind
    string message
    json data
  }
  CONTAINER_META {
    string id PK
    string name
    blob original_json
    blob rewritten_json
    blob decision_json
  }
  GATEWAY_PORTS {
    int port PK
    string key
  }
```

`container_meta` powers inspect rewriting. `gateway_ports` makes reservations
survive daemon restarts and prevents reassignment while a lease is active.

## API

The Unix-socket JSON API is the single source of truth for the CLI and
dashboard:

| Endpoint | Purpose |
| --- | --- |
| `GET /v1/status` | policy, backend, sockets, counts, health |
| `GET/POST /v1/rules`, `DELETE /v1/rules/{id}` | rule CRUD |
| `GET /v1/routes`, `DELETE /v1/routes/{id}` | routes |
| `GET/POST /v1/leases`, `DELETE /v1/leases/{id}` | host-process leases |
| `GET /v1/events`, `GET /v1/events/stream` | history and SSE |
| `GET/POST /v1/policy` | rewrite/deny/warn |
| `POST /v1/check` | dry-run a Compose file |
| `GET /v1/containers`, `GET /v1/suggest` | dashboard allow buttons |
| `POST /v1/reconcile` | force a sweep |
| `GET /v1/ca` | root CA PEM |
| `GET/POST /v1/enforce` | AuthZ plugin control |
| `GET /v1/doctor?fix=1` | health checks and safe fixes |
| `/` | embedded dashboard |

The same handler runs on the Unix socket and on an ephemeral loopback listener
that Caddy exposes as `gw.localhost`.

## Backends

`internal/backend.Backend` isolates native Linux Docker from rootless and Docker
Desktop:

- `Name()`, `RequiresPublishedPorts()`, `ResolveAddress(container, port)`,
  `CanBind(port)`.
- **native** dials the container's `gw` bridge IP.
- **rootless/desktop** cannot assume host-routable container IPs. The proxy adds
  a loopback-only ephemeral publication for each routed port at create time;
  the backend resolves `127.0.0.1:<assigned>` and inspect rewriting still hides
  it from Compose.

## Security boundaries

- The Docker proxy socket is 0660; the API socket is 0600. In the default
  system install the daemon runs as root.
- Enforcement uses a legacy Docker AuthZ plugin. Every prox-forwarded create is
  signed with HMAC-SHA256 over the rewritten body (key stored 0600 in the state
  dir). The plugin allows unsigned creates only when a `publish` rule matches,
  and denies port-binding creates from the raw socket otherwise.
- The CA private key stays under the state dir. `gw ca install` only copies the
  public root certificate.
- No container is granted elevated privileges by prox; the daemon only rewrites
  API payloads and attaches a bridge network.
