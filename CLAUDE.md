# excalidraw-wopi

This file guides a contributor who works in this repository. It
describes the codebase as built. `docs/ARCHITECTURE.md`, `docs/E2E.md`,
`docs/DEPLOYMENT.md`, and `docs/HIGH-AVAILABILITY.md` hold the full
detail; this file is the short map.

## What the service does

The service makes excalidraw a WOPI editor for La Suite Drive. A user
opens a `.excalidraw` file in Drive. The editor loads in the browser.
Several users edit the same board at the same time. The file body stays
in Drive (S3). No data reaches the excalidraw cloud.

One Go binary serves every request on one port:

- the WOPI discovery XML (`GET /hosting/discovery`),
- the launch endpoint (`POST /launch`),
- the board REST API (`GET`/`PUT /api/board`),
- the socket.io realtime relay (`/socket.io/`),
- the embedded TypeScript SPA (`go:embed` of the `web/` build).

Every WOPI call to Drive goes through the Go server, proof-signed. The
browser never talks to Drive directly.

## Repository layout

```
/cmd/excalidraw-wopi   the binary entry point
/internal              the Go packages (see the map below)
/web                   the TypeScript React SPA, embedded by go:embed
/e2e                   the interop, local, and dockerized-Drive suites
/docs                  the architecture and operator guides
/deploy                the Helm chart (a GHCR OCI artifact) and a compose example
/assets                a template .excalidraw file
```

### Go packages (`internal/`)

| Package | Role |
|---|---|
| `config` | Loads and validates every `EXCALIDRAW_WOPI_*` variable into one `Config`. |
| `proof` | Owns the RSA proof keypair: load-or-generate-and-persist, PEM injection, public parts for discovery. |
| `discovery` | Renders and serves the WOPI discovery XML. |
| `wopiclient` | The signed WOPI HTTP client: `CheckFileInfo`, `GetFile`, `Lock`, `RefreshLock`, `PutFile`, `Unlock`. |
| `hostadapter` | Every Drive-specific WOPI quirk behind one profile, so the rest of the code speaks plain WOPI. |
| `session` | Mints and verifies the session JWT: HS256-signed, with an AES-256-GCM-sealed WOPI access token inside. |
| `launch` | `POST /launch`: validates the WOPI access token, mints the session, serves the SPA with launch config injected. |
| `boardapi` | `GET`/`PUT /api/board` and the conflict poll/resolve routes. |
| `room` | The save-and-lock orchestrator: one `Manager` per process, one room state per open board. |
| `relay` | The socket.io realtime layer: handshake auth, rooms, presence, broadcasts, image relay, syncer election. |
| `peers` | Multi-replica ownership: the rendezvous-hash owner function, DNS peer discovery, and the routing middleware. |
| `httpserver` | Builds the `http.Server` and the base mux; other packages register their routes onto it. |
| `app` | Wires every package into one `*http.Server`; also the dev-only fake-host mode (`fakehost.go`). |
| `wopitest` | An in-memory WOPI host that mimics Drive, for the fake-host mode and the client tests. |

### Frontend (`web/src/`)

`App.tsx`/`main.tsx`/`bootstrap.ts` read the injected launch config and
mount the stock `@excalidraw/excalidraw` editor. `hooks/` hold the
collaboration and sync lifecycles, `stores/` hold the Zustand state,
`utils/` hold the pure, unit-tested pieces, `workers/syncWorker.ts`
runs IndexedDB and server writes off the main thread, and `database/`
holds the Dexie local store.

## Invariants a contributor must keep

These rules hold the design together. Do not break one without changing
the design on purpose.

- **The proof private key stays server-side.** Every WOPI call runs
  from the Go server, never from the browser.
- **The relay never sees the WOPI access token.** `internal/app`
  drops the sealed token before it hands the claims to the relay.
- **The relay forwards scene bytes untouched.** It does not
  canonicalize or rewrite a `server-broadcast` payload. The one
  exception is the volatile channel, where the relay overwrites the
  sender-asserted cursor identity, so a client cannot spoof another
  user.
- **One room has one owner replica.** The owner comes from a
  rendezvous hash over the file id and the peer set. Ownership is
  enforced against the file id inside the verified session token, not
  against the client's `room` hint.
- **One deterministic lock value per file.** The room keeps the WOPI
  lock, refreshes it, and re-locks after an expiry. The service detects
  a save conflict and prompts the user; it never merges on its own.
- **The session is stateless.** The server holds no session table. A
  client reconnects after a restart with the same JWT, and the room
  re-locks and resumes.
- **Read-only sessions never emit or save.** The claim comes from the
  JWT; the relay drops broadcasts from a read-only socket but keeps its
  cursor visible.

## Build, test, and lint

Use the Makefile targets.

| Target | What it does |
|---|---|
| `make build` | Builds the frontend, then builds `bin/excalidraw-wopi`. |
| `make dev` | Runs the service from source against `localhost:8080`. |
| `make test` | The PR-gate unit layer: `go-test` plus `web-test`. |
| `make go-test` | `go test ./...`. |
| `make web-test` | `tsc --noEmit` plus `vitest run` for `web/src`. |
| `make lint` | golangci-lint on the Go code. |
| `make web-lint` | eslint on `web/src`. |
| `make chart-lint` | Lints the Helm chart and renders it under every documented topology. |
| `make interop` | The socket.io relay interop harness; no Drive needed. |
| `make e2e-local` | The fast local Playwright suite: fake WOPI host, no Drive, no docker. |
| `make e2e-ha` | The LB+hash multi-instance suite: two instances behind a hashing proxy, fake WOPI host. |
| `make e2e-up` / `make e2e-down` | Brings a dockerized Drive stack up or down. |
| `make e2e-seed` | Seeds one excalidraw item and prints its launch payload. |
| `make test-drive-integration` | The Go integration suite against the live e2e Drive stack. |
| `make e2e-smoke` | The dockerized-Drive Playwright smoke suite (needs `make e2e-up`). |
| `make e2e-nightly` | The nightly slow suite: smoke gate, socket load/storm, slow browser scenarios (needs `make e2e-up`). |

Run `make test` before you open a pull request. Run `make lint` and
`make web-lint` too; both are style gates.

### Local development without Drive

The fastest manual loop needs no Drive and no docker. Build the binary
and run it with the fake host on a loopback URL:

```sh
make build
EXCALIDRAW_WOPI_FAKE_HOST=1 ./bin/excalidraw-wopi
```

Then open `http://localhost:8080/fakewopi/launch?user=alice` to edit as
a writer, or `?user=bob` to test a read-only session. The service
refuses the fake host unless `EXCALIDRAW_WOPI_PUBLIC_URL` is a loopback
address.

## Testing constraints

- A test that binds and dials a loopback TCP port fails in a sandbox
  that refuses such dials. Let CI confirm those tests.
- The browser e2e suites (`make e2e-local`, `make e2e-smoke`, and the
  nightly Playwright suite under `make e2e-nightly`) need real
  browsers. A change to `web/src` collaboration or sync logic needs one
  of those suites, not only `vitest`, to prove it.
- `make e2e-nightly` needs `make e2e-up` first. The full run takes
  roughly 60-75 minutes; the lock spec alone waits roughly 35 minutes.

## Provenance and license

The `web/src` frontend is adapted from
[nextcloud/whiteboard](https://github.com/nextcloud/whiteboard), which
is licensed AGPL-3.0-or-later. This project is therefore AGPL-3.0. Each
file under `web/src` carries an SPDX header; keep it attached through
every edit. The Go server is this project's own code. The `LICENSE`
file at the repository root carries the full license text.

## Writing style

Write every comment, docstring, commit message, and document in
Simplified Technical English. Comment sparingly: state the reason a line
exists, never restate what it does. The global conventions file holds
the full rules.
