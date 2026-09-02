# excalidraw-wopi

This service makes excalidraw a WOPI editor. It has been initially designed for [La Suite Drive](https://github.com/suitenumerique/drive).
The current version supports collaboration so that several Drive users can edit the same board
at the same time and storage stays in Drive, no data ever goes out.

## Features

- Realtime collaboration: several users draw on the same board at once,
  with cursors, presence, and live syncer election.
- The file body lives only in Drive (S3); the service retains no
  independent copy across restarts.
- WOPI proof signing on every call, so the access token cannot be
  replayed against a spoofed host.
- Read-only sessions render in view mode; they still see live cursors
  and updates, but they never save.
- One static Go binary. The frontend bundle ships inside it
  (`go:embed`); no separate frontend deployment.
- CJK text support: the embedded editor ships all nine excalidraw font
  families, Xiaolai included.

## Architecture

The Go binary serves the WOPI discovery XML, the launch endpoint, the
board REST API, and the socket.io realtime relay, and it embeds the
built TypeScript SPA. Every WOPI call to Drive goes through the Go
server, proof-signed; the browser never talks to Drive directly. See
`docs/ARCHITECTURE.md` for the full request-flow diagram, package map,
save pipeline, lock lifecycle, and session model.

## Documentation

- `docs/ARCHITECTURE.md` — the request flow, the package map, the save
  pipeline, the lock lifecycle, the session JWT model, and the test
  pyramid.
- `docs/E2E.md` — every test target: what it does, when to run it,
  and the invariant it proves.
- `docs/DEPLOYMENT.md` — the operator guide: every environment
  variable, the proof-key lifecycle, Drive-side registration, ingress
  paths, and known limitations.
- `docs/HIGH-AVAILABILITY.md` — the two supported multi-instance
  topologies, DNS health checks and a load-balancer consistent hash,
  and how to choose between them.
- `deploy/helm/excalidraw-wopi` — the Helm chart, published as an OCI
  artifact at `oci://ghcr.io/zeylos/charts/excalidraw-wopi`.
- `deploy/docker` — a docker compose example.

## Build

Run `make build`. The command builds the frontend, then builds one Go
binary at `bin/excalidraw-wopi`.

## Configuration

The service reads these environment variables at boot. Each name takes
the `EXCALIDRAW_WOPI_` prefix.

| Variable | Default | Meaning |
| --- | --- | --- |
| `LISTEN_ADDR` | `:8080` | The address the HTTP server binds to. |
| `PUBLIC_URL` | `http://localhost:8080` | The absolute base URL that clients use to reach this service. |
| `SESSION_SECRET` | (random) | The key that signs session tokens. Set a fixed value so sessions survive a restart. |
| `SESSION_SECRET_FILE` | (empty) | The path of a file that holds the session secret. The service reads it at boot. Prefer this with a mounted Kubernetes `Secret`. |
| `PROOF_KEY_PATH` | `/var/lib/excalidraw-wopi/proof-key.pem` | The file path for the RSA keypair that signs WOPI proof headers. |
| `MAX_IMAGE_BYTES` | `10485760` (10 MB) | The largest image a user can add to a board. |
| `MAX_SCENE_BYTES` | `52428800` (50 MB) | The largest scene, files included, that the service accepts. |
| `SOCKET_BUFFER_BYTES` | `62914560` (60 MB) | The largest message the WebSocket relay accepts. |
| `WOPI_ALLOWED_ORIGINS` | (empty) | Comma-separated list of allowed WOPI host origins (`scheme://host[:port]`) that `/launch` accepts a WOPISrc from. Empty rejects every launch with 403. |

Two more variables exist outside this table, for the proof-key PEM
injection and the dev-only fake WOPI host; see `docs/DEPLOYMENT.md`
for both.

## Make targets

| Target | What it does |
| --- | --- |
| `make build` | Builds the frontend, then builds `bin/excalidraw-wopi`. |
| `make web-deps` | Installs the `web/` npm dependencies (`npm ci`). |
| `make web-build` | Builds the `web/` frontend bundle with vite, for `go:embed`. |
| `make dev` | Runs the service straight from source, against `localhost:8080`. |
| `make test` | Runs `go-test` and `web-test`. The PR-gate unit layer. |
| `make go-test` | Runs `go test ./...`. |
| `make web-test` | Type-checks and unit-tests `web/src` (`tsc --noEmit` + `vitest run`). |
| `make lint` | Runs golangci-lint on the Go code. |
| `make web-lint` | Runs eslint on `web/src`. |
| `make chart-lint` | Lints and renders the Helm chart in `deploy/helm` (needs helm on PATH). |
| `make interop` | Runs the socket.io relay interop harness against a real socket.io-client (`e2e/interop`). No Drive needed. |
| `make e2e-local` | Builds the binary, then runs the fast local Playwright suite: fake WOPI host, no Drive, no docker. |
| `make e2e-ha` | Runs the multi-replica failover harness: instances behind a consistent-hash proxy, fake WOPI host (`e2e/ha`). |
| `make e2e-up` | Brings up a dockerized Drive stack and starts this binary against it, for e2e work. |
| `make e2e-seed` | Seeds one excalidraw item in the e2e stack and prints its WOPI launch payload. |
| `make test-drive-integration` | Runs the Go integration suite against the live e2e Drive stack (`make e2e-up` first). |
| `make e2e-down` | Stops the binary and tears down the dockerized Drive stack. |
| `make e2e-smoke` | Runs the dockerized-Drive Playwright smoke suite (needs `make e2e-up` first). |
| `make e2e-nightly` | Runs the nightly slow suite (needs `make e2e-up` first); see `docs/E2E.md`. |

## Development quickstart

**Fastest automated loop, no Drive, no docker:** run the local
Playwright suite. It builds the binary and starts it itself, with the
fake WOPI host on, on a fixed port:

```sh
make e2e-local
```

**Fastest manual loop, no Drive, no docker:** build the binary and run
it yourself with the fake-host flag on a loopback `PUBLIC_URL`.
`internal/app` then mounts an in-process WOPI host with a fixed writer
(`alice`) and a fixed read-only user (`bob`), so you can open
`http://localhost:8080/fakewopi/launch?user=alice` in a browser and
edit against it directly, with no Drive and no docker:

```sh
make build
EXCALIDRAW_WOPI_FAKE_HOST=1 ./bin/excalidraw-wopi
```

**Full stack, against a real dockerized Drive:**

```sh
make e2e-up     # fetch and start Drive, start this binary, register discovery
make e2e-seed   # create one excalidraw item and print its launch URL
make e2e-down   # tear everything down
```

See `e2e/README.md` for what each of those steps does under the hood,
`docs/E2E.md` for a target-level guide to every test suite, and
`docs/DEPLOYMENT.md` for registering this service against a non-e2e,
real Drive deployment.

## Provenance and license

The `web/src` frontend is adapted from
[nextcloud/whiteboard](https://github.com/nextcloud/whiteboard), which
uses the AGPL-3.0-or-later license. This project therefore uses the
AGPL-3.0 license as a whole. Each file under `web/src` keeps its SPDX
header; do not remove these headers. The Go server, under `internal/`
and `cmd/`, is this project's own code.

## License

This project uses the GNU Affero General Public License, version 3.
See the `LICENSE` file for the full text.
