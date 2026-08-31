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

## Proof-key lifecycle

The proof key is the RSA keypair the service uses to sign the
`X-WOPI-Proof`/`X-WOPI-ProofOld` headers on every WOPI call, and to
publish its public parts in the `<proof-key>` element of
`GET /hosting/discovery`.

**First start.** If neither `EXCALIDRAW_WOPI_PROOF_KEY_PEM` nor a file
at `EXCALIDRAW_WOPI_PROOF_KEY_PATH` exists, the service generates a new
2048-bit RSA key, sets the "old" key equal to the current key, and
persists both to `EXCALIDRAW_WOPI_PROOF_KEY_PATH`
(`internal/proof/proof.go`, `Load`). This file must survive a restart
and a redeploy — mount it on a persistent volume, or inject the key
through `EXCALIDRAW_WOPI_PROOF_KEY_PEM` instead (see below).

**File format.** The key file holds two concatenated PEM blocks,
PKCS8-encoded, `-----BEGIN PRIVATE KEY-----` type, current key first
and old key second. A file with only one block means the old key
equals the current key (the state right after first-start
generation). This format needs no custom parser and round-trips
through any PEM-aware tool (`openssl`, etc.).

**Kubernetes secret injection.** Generate a key file once. Run the
service against an absent key file and read back what it wrote, or use
`openssl` to generate a PKCS8 RSA key pair. Store its two-block PEM
content as a `Secret`. The recommended mode mounts that `Secret` as a
read-only volume, and `EXCALIDRAW_WOPI_PROOF_KEY_PATH` points at the
key file inside that volume. A process environment variable can leak
through `/proc/<pid>/environ`, an inherited child process, or a
diagnostic dump.

A mounted file with restrictive permissions avoids that class of leak.
Because the mount is read-only, the key must already exist in the
`Secret` before the pod boots. The service cannot write a freshly
generated key to a read-only mount. The alternative mode injects the
same PEM content as an environment variable, through
`EXCALIDRAW_WOPI_PROOF_KEY_PEM`. Every replica then loads the identical
key, from the file or from the environment. Neither mode needs a
shared or persistent filesystem for the proof key.

**Concurrent first-start race.** If two replicas both hit an absent
key file at once, `persist` uses `os.Link` (which fails instead of
silently overwriting) to detect the race: the losing replica discards
its own freshly generated key, re-reads the file the winner wrote, and
adopts that key instead. Every replica converges on one key on disk.
This path only matters when replicas share a filesystem; it does not
apply to the `EXCALIDRAW_WOPI_PROOF_KEY_PEM` injection mode, where the
key is already fixed before any replica starts.

**Rotation.** A WOPI host accepts a proof signature made with either the
current or the old key (that is what the six discovery XML attributes,
`modulus`/`exponent`/`oldmodulus`/`oldexponent`, publish). To rotate:

1. Generate a new RSA keypair (2048 bits or larger; the service
   rejects anything smaller on load).
2. Build a new two-block PEM file (or PEM string): the new key first,
   the previous *current* key second (not the previous *old* key —
   that one is retired).
3. Update the `proof-key.pem` key of the mounted `Secret` (or the file
   at `EXCALIDRAW_WOPI_PROOF_KEY_PATH`, or the
   `EXCALIDRAW_WOPI_PROOF_KEY_PEM` value), and restart every replica.
4. Drive re-fetches `/hosting/discovery` on its next nightly
   `trigger_wopi_configuration` run (or run it by hand — see the
   runbook step below) and picks up the new modulus/exponent pair. In
   the window between the restart and that refetch, Drive still
   verifies signatures against the old key, which the rotation above
   keeps valid as the published "old" key.

There is no in-process rotation command; rotation is a file/secret
replacement plus a restart, exactly like first-start generation.

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

## Conflict behavior

The service detects a save conflict; it never merges changes on its
own. Two situations trigger it:

- A `PutFile` call returns 409 with an `X-WOPI-Lock` header that names
  a lock value this service did not set (`internal/room/save.go`,
  `ensureLocked`/`performSave`).
- A scheduled version check (on every lock refresh, and whenever a new
  user is observed joining an already-established room) finds Drive's
  live `Version` (the S3 ETag) does not match the version this service
  last recorded from its own successful `PutFile`
  (`internal/room/save.go`, `checkVersion`).

Once a room enters conflict state, the service stops attempting saves
and stops refreshing the lock for that room, until the conflict is
resolved. Resolution is a client-driven choice between overwriting
the host with the retained in-memory scene, or discarding it and reloading
the host's current content (`internal/room/manager.go`,
`ResolveConflict`); the frontend surfaces this as the Overwrite/Reload
banner. The client learns about a
conflict two ways: a `conflict-state` socket.io push
(`{"inConflict": bool, "saveStalled": bool}`) the moment
`internal/room.Manager` detects or clears one, and, as a poll fallback
if the socket connection is down, `GET /api/board/conflict`.
`saveStalled` reports a dirty room whose every save attempt has failed
for at least 5 minutes, or one that has lost write access on every
tracked token (a deleted file, or write ability revoked on the host),
which reports immediately instead of waiting for the 5-minute window.
Either case is independent of a lock conflict. The client
resolves a conflict with `POST /api/board/conflict/resolve`
(writer-only; body `{"overwrite": bool}`). The reload branch also
sends a `reload-required` socket.io push to every client in the room,
not only the one that resolved the conflict, since the host's content
moved out from under every open session, not just the resolver's. An
operator does not need to intervene manually; a conflict clears itself
once a user in the room picks one of those two options.

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
