# Operator guide

This document tells an operator how to configure, deploy, and register
`excalidraw-wopi` against a running La Suite Drive instance. It does
not cover building the binary; see the root `README.md` for that.

## Environment variables

The service reads every setting from environment variables at boot
(`internal/config/config.go`). Every name takes the
`EXCALIDRAW_WOPI_` prefix. Two more variables, read outside the
`config` package, are listed at the end of the table.

| Variable | Default | Required | Meaning |
|---|---|---|---|
| `EXCALIDRAW_WOPI_LISTEN_ADDR` | `:8080` | no | The address the HTTP server binds to. |
| `EXCALIDRAW_WOPI_PUBLIC_URL` | `http://localhost:8080` | no, but must be correct | The absolute base URL clients and Drive use to reach this service. It is also the base every generated link (`/launch`, `/hosting/discovery`'s `urlsrc`) is built from, so a wrong value breaks launch and discovery both. |
| `EXCALIDRAW_WOPI_SESSION_SECRET` | (random, regenerated every start) | **yes, in production** | The key that derives the session JWT's signing and sealing keys (HKDF-SHA256). Without a fixed value, every restart invalidates every open session; a client must relaunch from the host. Set at least 32 bytes of entropy. For a Kubernetes deployment, prefer `EXCALIDRAW_WOPI_SESSION_SECRET_FILE` instead. |
| `EXCALIDRAW_WOPI_SESSION_SECRET_FILE` | (empty) | no | A file path. The service reads the session secret from this file at boot and removes the leading and trailing whitespace. Setting this together with `EXCALIDRAW_WOPI_SESSION_SECRET` is an error. The 32-byte minimum still applies. This is the preferred delivery for a Kubernetes `Secret` mounted as a volume. |
| `EXCALIDRAW_WOPI_PROOF_KEY_PATH` | `/var/lib/excalidraw-wopi/proof-key.pem` | no | The file path for the RSA keypair that signs WOPI proof headers. See "Proof-key lifecycle" below. |
| `EXCALIDRAW_WOPI_MAX_IMAGE_BYTES` | `10485760` (10 MB) | no | The largest image a user can add to a board. |
| `EXCALIDRAW_WOPI_MAX_SCENE_BYTES` | `52428800` (50 MB) | no | The largest scene, files included, the service accepts, at the relay's transport check and at the board save endpoint. Must be at least `MAX_IMAGE_BYTES`. |
| `EXCALIDRAW_WOPI_SOCKET_BUFFER_BYTES` | `62914560` (60 MB) | no | `maxHttpBufferSize` for the socket.io relay: 50 MB plus margin. Must be at least `MAX_SCENE_BYTES`. |
| `EXCALIDRAW_WOPI_WOPI_ALLOWED_ORIGINS` | (empty) | **yes** | Comma-separated list of allowed WOPI host origins (`scheme://host[:port]`, e.g. `https://drive.example.org`) that `POST /launch` accepts a `WOPISrc` from. An empty list makes `/launch` refuse every request with 403, and the service logs a startup warning naming this variable. This same list becomes the launch page's `Content-Security-Policy: frame-ancestors` value. |
| `EXCALIDRAW_WOPI_DNS_PEERS` | (empty) | no | The peer-discovery DNS name: `<host>:<port>`. Every A/AAAA record behind it becomes a peer. Empty disables multi-replica routing. See `docs/HIGH-AVAILABILITY.md`. |
| `EXCALIDRAW_WOPI_DNS_SELF` | (empty) | **yes, when `EXCALIDRAW_WOPI_DNS_PEERS` is set** | This replica's own advertised URL, as another replica reaches it. |

Two more variables exist outside `internal/config`, read directly with
`os.Getenv` because neither needs validation or a documented default:

| Variable | Meaning |
|---|---|
| `EXCALIDRAW_WOPI_PROOF_KEY_PEM` | Injects the proof key PEM content directly, instead of a file path. This variable stays supported, but the recommended delivery is a Kubernetes `Secret` mounted as a read-only file at `EXCALIDRAW_WOPI_PROOF_KEY_PATH`. When set, `EXCALIDRAW_WOPI_PROOF_KEY_PEM` wins over `EXCALIDRAW_WOPI_PROOF_KEY_PATH` entirely: the service never touches the filesystem for the proof key. A value in the process environment can leak through `/proc/<pid>/environ`, an inherited child process, or a diagnostic dump. To use the file mode, remove this variable. An empty value still wins over the path and then fails to parse. |
| `EXCALIDRAW_WOPI_FAKE_HOST` | Set to `1` to mount an in-process fake WOPI host at `/fakewopi/` for local development and the `make e2e-local` Playwright suite. **Never set this in production**: the fake host performs no authentication of its own. The service refuses to enable it unless `EXCALIDRAW_WOPI_PUBLIC_URL` is a loopback address (`localhost` or a loopback IP), and it logs a loud warning whenever it is on. |

## Drive-side registration

Registering this service with Drive needs two Drive environment
values, set on the Drive deployment, not on `excalidraw-wopi`:

```yaml
WOPI_CLIENTS: onlyoffice,excalidraw
WOPI_EXCALIDRAW_DISCOVERY_URL: https://<service-host>/hosting/discovery
```

`WOPI_CLIENTS` is a comma-separated client list; the last client
listed wins on an extension overlap, so list `excalidraw` after any
office suite that might otherwise claim the same extension (it will
not, since excalidraw claims only the `.excalidraw` extension, but the
ordering rule still applies generally). `WOPI_EXCALIDRAW_DISCOVERY_URL`
must be this service's own externally reachable base URL, plus
`/hosting/discovery`, matching `EXCALIDRAW_WOPI_PUBLIC_URL` here.

Drive's celery task rebuilds its routing map from every registered
client's discovery XML once a night. A manual trigger command exists
for immediate ingestion:

```sh
python manage.py trigger_wopi_configuration
```

### Runbook: after every discovery change

**Run `trigger_wopi_configuration` on the Drive side after any change
that affects this service's discovery XML** — a `PUBLIC_URL` change, a
proof-key rotation, or a fresh deployment. Drive's routing map and
proof-key trust otherwise lag up to 24 hours behind the nightly
rebuild. `WOPISrc` URLs Drive already handed out stay valid across a
routing-map rebuild (the `urlsrc` carries no version token), so this
step is about picking up new configuration promptly, not about
avoiding broken links.

## Proof-key lifecycle

The proof key is the RSA keypair that signs every WOPI call to Drive.
Drive learns its public parts from the discovery XML.

**First start.** When no key exists, the service generates one and
writes it to `EXCALIDRAW_WOPI_PROOF_KEY_PATH`. This file must survive
a restart and a redeploy: mount it on a persistent volume, or inject
the key through `EXCALIDRAW_WOPI_PROOF_KEY_PEM` instead.

**File format.** The file holds two concatenated PKCS8 PEM
private-key blocks: the current key first, the old key second. A file
with one block means the old key equals the current key. Any
PEM-aware tool (`openssl`) can produce or read it.

**Kubernetes secret injection.** Generate the key file once (run the
service once against an absent file and read back what it wrote, or
use `openssl`), and store the two-block PEM in a `Secret`. The
recommended mode mounts the `Secret` read-only and points
`EXCALIDRAW_WOPI_PROOF_KEY_PATH` at the file inside it; an
environment variable can leak through diagnostics or a child process.
The alternative mode sets the same PEM as
`EXCALIDRAW_WOPI_PROOF_KEY_PEM`. In both modes every replica loads
the identical key, and no persistent volume is needed. Replicas that
share a writable volume are also safe: a first-start race converges
on one key.

**Rotation.** Drive accepts a signature from the current key or the
old key, so a rotation causes no downtime:

1. Generate a new RSA keypair (2048 bits or larger; the service
   rejects a smaller key).
2. Build a new two-block PEM: the new key first, the previous
   *current* key second (the previous *old* key retires).
3. Replace the `Secret`, the key file, or the variable value, and
   restart every replica.
4. Trigger a Drive discovery refetch, or wait for its nightly run
   (see the runbook below). Until then Drive verifies against the old
   key, which step 2 keeps published.

There is no rotation command; a rotation is a secret replacement plus
a restart.

## Ingress

Every path below must reach the `excalidraw-wopi` binary. All of them
are served from the same process on `EXCALIDRAW_WOPI_LISTEN_ADDR`, so a
single ingress rule forwarding the whole host is the simplest correct
configuration; list the specific paths only if a narrower rule is
required:

| Path prefix | Purpose |
|---|---|
| `/` | The static SPA bundle (`go:embed`) and its client-side routes. |
| `/launch` | `POST` only: the WOPI action URL. The host auto-submits an iframe form to start an editor session. |
| `/api/` | The board REST API (`GET`/`PUT /api/board`), bearer-authenticated with the session JWT. |
| `/socket.io/` | The realtime relay (WebSocket and polling transports). An ingress or reverse proxy in front of this path must support WebSocket upgrade. |
| `/hosting/discovery` | The WOPI discovery XML Drive's celery task fetches. |

`GET /healthz` is also served, for a liveness/readiness probe; it is
not part of the WOPI-facing contract above.

## Multi-replica deployment

The service can run several replicas at once, each holding its own
in-memory state, with one owner replica per open file. The owner is
chosen by a rendezvous hash over the file id and the peer set; every
replica computes the same owner without asking another. See
`docs/HIGH-AVAILABILITY.md` for the two supported topologies —
`EXCALIDRAW_WOPI_DNS_PEERS`/`DNS_SELF` peer discovery, and a
load-balancer consistent hash with no peer variables at all — their
deployment recipes, and their failure behavior. See
`docs/ARCHITECTURE.md`'s "Peer routing" section for the routing
middleware, the token-layer enforcement, and the hop guard the
peer-discovery topology relies on.

## Docker Compose

`deploy/docker/compose.yaml` holds a single-instance docker compose
example. See `deploy/docker/README.md` for the setup steps. The
example runs one instance; it does not enforce the multi-replica
invariants above. For a multi-replica deployment, use the Helm chart
below, and see `docs/HIGH-AVAILABILITY.md` for its two topologies.

## Kubernetes: the Helm chart

The Helm chart at `deploy/helm/excalidraw-wopi` packages the manifest
set from `docs/HIGH-AVAILABILITY.md`'s "Kubernetes: Deployment plus a
headless Service" section. It is published as an OCI artifact:

```sh
helm install excalidraw-wopi oci://ghcr.io/zeylos/charts/excalidraw-wopi \
  --version <version> \
  --set config.publicUrl=https://excalidraw.example.org \
  --set 'config.wopiAllowedOrigins={https://drive.example.org}'
```

Create the `excalidraw-wopi-secrets` Secret before the install. It
must hold the `proof-key.pem` and `session-secret` keys; see
"Proof-key lifecycle" above for how to generate and store them.

You must set `config.publicUrl` and `config.wopiAllowedOrigins`; they
map to `EXCALIDRAW_WOPI_PUBLIC_URL` and
`EXCALIDRAW_WOPI_WOPI_ALLOWED_ORIGINS`. The chart does not enforce
these two values at render time, but the service refuses launches
without them. Set `peerDiscovery.enabled=false` for the load-balancer
topology (Setup 2 in `docs/HIGH-AVAILABILITY.md`). The chart version
equals the application version; one release tag publishes both.

## Known limitations

These limitations are known and accepted:

1. **No "New whiteboard" button in Drive.** Drive's create-file menu is
   hardcoded.
2. **No thumbnails** for `.excalidraw` files in the Drive grid.
3. **No rename from inside the editor.** Rename the file from Drive's
   own UI instead.
4. **Scene payloads are plaintext to the server** over the websocket
   (TLS protects the wire; the server itself sees plaintext, same as
   upstream nextcloud/whiteboard).
5. **Per-room capacity is one replica's capacity.** One room always
   lives on one replica, its owner (see "Multi-replica deployment"
   above). Adding replicas raises total capacity, but not one room's
   capacity (nextcloud's own benchmark: roughly 300-500 users per
   node, 5-10 MB RAM per user).
6. **A hard crash of the syncer's browser tab can lose up to about 10
   seconds of work** — the crashed tab still has it in its own
   IndexedDB, but no other client received it yet.
