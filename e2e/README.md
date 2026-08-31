# e2e stack

This directory holds a dockerized La Suite Drive, pinned to `v0.21.1`, for
the excalidraw-wopi e2e suite. It gives the PR-gate smoke test and the
nightly test a real Drive host to launch from, lock against, and save
through. See `docs/E2E.md` for a target-level guide to every test
suite in this repository; this file covers the stack's own internals.

**WARNING**: Drive's e2e build exposes an auth-bypass login endpoint
(`POST /api/v1.0/e2e/user-auth/`) that needs no credentials. `app-dev`
(port 8000) and rustfs (port 9000) bind to `127.0.0.1` only, but run
this stack on a trusted network only, never on a host reachable by
untrusted users or networks.

## Host-networking constraint

Every service in `compose.yaml` runs with `network_mode: host`. The build
machine this stack targets runs docker with no bridge networking and no
iptables, so `ports:` mappings would be no-ops; each service binds its own
port directly on the host instead. Services address each other, and the
seed script addresses Drive, as `localhost:<port>` — never by service
name. GitHub Actions runners support host networking too, so this same
file runs in CI.

## Port map

| Port | Service |
|---|---|
| 5432 | postgresql |
| 6379 | redis |
| 9000 | rustfs (S3 API) |
| 8000 | Drive app-dev |
| 8080 | our excalidraw-wopi binary (runs on the host, not in compose) |

Our own binary is not a compose service. It runs as a plain host process,
started and stopped by the `make e2e-up` / `make e2e-down` targets, so it
rebuilds fast on every iteration.

## Running the stack

```sh
make e2e-up     # fetch Drive, build and start every container, start our binary
make e2e-seed   # create one excalidraw item and print its WOPI launch payload
make e2e-down   # stop our binary, tear down the containers and their volumes
```

`make e2e-up` runs, in order: `e2e/scripts/fetch-drive.sh` (clones Drive
into `e2e/.drive` if it is not already there), `docker compose up --build
--wait`, Django migrations, a build of our binary, a start of our binary
in the background (PID file under `e2e/.run/`), and a
`trigger_wopi_configuration` call that makes Drive fetch our discovery XML
and route the `.excalidraw` extension to us. It polls Drive's routing
cache for that last step instead of guessing a sleep.

`make e2e-seed` runs `e2e/scripts/seed.mjs`: it logs in through the e2e
auth bypass (`POST /api/v1.0/e2e/user-auth/`), creates a file item,
uploads a minimal excalidraw scene straight to rustfs through Drive's
presigned URL, and polls the item's `/wopi/` action until Drive returns a
`launch_url`. A `launch_url` pointing at `localhost:8080` is the proof
that Drive ingested our discovery XML.

## Notes on the Drive env

`env/drive.env` is derived from Drive's own
`env.d/development/{common,common.e2e,postgresql,postgresql.e2e}`. Every
hostname that pointed at a compose service name (`postgresql`, `redis`,
`rustfs`) now reads `localhost`, because of the host-networking constraint
above. It also switches `WOPI_CLIENTS` down to `excalidraw` only (this
stack carries no collabora or onlyoffice container) and sets
`MALWARE_DETECTION_BACKEND` to Drive's synchronous dummy backend, so an
uploaded file leaves `PENDING` state right away instead of sitting through
a scan-pending window.

`DJANGO_EMAIL_HOST`/`DJANGO_EMAIL_PORT` stay unset: the backend-development
image ships no compiled mail templates, so any notification email 500s.
Drive skips sending email cleanly when `EMAIL_HOST` is unset, and this
suite's login goes through the e2e auth bypass, not email, so no SMTP
service runs in this stack.

The dev OIDC settings are present but point nowhere: this stack ships no
keycloak. Nothing needs them, because the e2e auth bypass endpoint
replaces the OIDC login flow entirely.

## The Go integration suite

`e2e/integration/drive_test.go` runs internal/wopiclient against the live
Drive from this stack: signed CheckFileInfo and GetFile, the empty-file
PutFile rule, the full lock lifecycle, a version-move
check, a garbage-token rejection, and a proof-signature rejection with a
stranger keypair. It carries the `driveintegration` build tag, so plain
`go build ./...` and `go vet ./...` skip it.

```sh
make e2e-up                # stack must already be up
make test-drive-integration
make e2e-down
```

`test-drive-integration` runs `go test -tags driveintegration -count=1 -v
./e2e/integration/`. The suite seeds its own fixtures: each subtest group
runs `node e2e/scripts/seed.mjs` through `os/exec`, with `--empty` for a
0-byte item and `--filename` to keep items apart, and decodes the
script's JSON stdout (see the flags listed in seed.mjs). It signs
requests with the same proof keypair the running excalidraw-wopi binary
persisted: it loads config.Config and calls proof.Load exactly as
cmd/excalidraw-wopi does, which resolves to
`/var/lib/excalidraw-wopi/proof-key.pem` under this stack's env (neither
`env/excalidraw.env` nor the shell sets an override), the same file
`e2e-up.sh` had the binary write on its first start.

## The relay interop harness

`e2e/interop/` is a structured, CI-runnable successor to
`internal/relay/smoke`: it drives the socket.io relay (`internal/relay`)
with a real `socket.io-client` v4, over WebSocket, and asserts each
scenario instead of eyeballing console output. It needs no Drive stack.

```sh
make interop
```

This runs `cd e2e/interop && npm ci && npx vitest run`. Vitest's
`globalSetup` (`e2e/interop/global-setup.mjs`) builds
`e2e/interop/server` once into `e2e/interop/.bin/` (gitignored), starts
it on `127.0.0.1:18765` (override with `INTEROP_ADDR`), waits for its
"interop server listening" stdout line, and tears it down after the run.

`e2e/interop/server/main.go` is a small standalone relay process with a
static `TokenVerifier`: `writer-a` and `writer-b` (read-write,
`fileId: "interop-room"`), `reader` (read-only, same room), and
`other-room-writer` (`fileId: "other-room"`, used to exercise the
room-does-not-match-claim rejection). It configures a 1 MiB scene byte
limit (override with `INTEROP_MAX_SCENE_BYTES`) so the oversize-payload
test does not need a multi-megabyte fixture.

`e2e/interop/relay.test.mjs` asserts, in order: the exact
`connect_error` auth message; `init-room` and the join-room happy path
(`sync-designate`, the deduped `room-user-change` roster shape,
`user-joined`); the room-claim mismatch rejection; `server-broadcast`
byte-identical relay with sender exclusion; the read-only
`server-broadcast` drop; `server-volatile-broadcast` identity rewriting
for `MOUSE_LOCATION` and `VIEWPORT_UPDATE`, and the drop of a
disallowed volatile type; a read-only cursor still passing through;
`image-get`; the oversize-payload drop with an error
emit; and syncer failover on disconnect.

## The HA multi-replica harness

`e2e/ha/` proves the service works as N independent instances behind a
load balancer with a consistent hash: no Drive, no docker, no
`internal/peers` multi-replica wiring on the service side (every
instance runs in plain single-replica mode). It needs no live browser.

```sh
make e2e-ha
```

This runs `cd e2e/ha && npm ci && npx vitest run`. Vitest's
`globalSetup` (`e2e/ha/global-setup.mjs`) builds `bin/excalidraw-wopi`
plus two small commands local to this directory, then starts, in order:

- `e2e/ha/wopihost`: `internal/wopitest`'s in-memory WOPI host run
  standalone, so it outlives any service instance the harness kills. It
  serves the real WOPI routes plus two dev-only routes: `POST /seed`
  (registers a file and mints one access token per user) and
  `GET /state` (the file's stored bytes, version, save count, and
  current lock, for a test's poll loop).
- Two `bin/excalidraw-wopi` instances, each with no peer environment
  variables and no `EXCALIDRAW_WOPI_FAKE_HOST`, sharing one session
  secret and one proof key file. Instance A starts first and generates
  the key file; instance B starts only once A is up, and loads what A
  wrote, so the two instances never race to create the file.
- `e2e/ha/hashproxy`: a reverse proxy in front of both instances. It
  routes on the `room` query parameter with a rendezvous hash (the same
  shape `internal/peers`' `Cluster.Owner` uses for replica ownership), so
  every request naming one room lands on one instance for as long as
  that instance stays healthy; it polls each backend's `/healthz` every
  500ms to eject and readmit one. `GET /__owner?room=<id>` reports which
  backend a room currently hashes to, so a test knows which instance to
  kill.

A test file reaches `wopihost` and `hashproxy` directly over plain HTTP
(`e2e/ha/helpers.mjs`'s `seedFile`, `getState`, `getOwner`), and drives
the real client paths through the proxy: `POST /launch` for a session
JWT (parsed back out of the returned HTML's injected `#ew-config` tag),
`socket.io-client` with `query: { room }` for presence, and
`PUT /api/board?room=<id>` for a save. A tiny control HTTP server, also
started by `global-setup.mjs` in its own process, answers a test's
`POST /kill?backend=<url>`: a test file can run in a worker thread that
shares no process handle with the setup module, so this is the only
portable way for a test to `SIGKILL` one specific instance.

The suite runs its four test files in a fixed numeric order
(`vitest.config.mjs` sets `fileParallelism: false`): presence (two
clients of one file land on the same instance), a board write reaching
`wopihost`, two files whose hashes disagree landing on different
instances, and failover last, since it permanently kills one instance.
The failover case asks `/__owner` which instance owns a room, `SIGKILL`s
it, waits for the proxy to reroute the room to the survivor, and proves
the survivor re-locks (the deterministic lock value in
`internal/room/lockvalue.go` means the survivor presents the same value,
and a same-value LOCK counts as a refresh, with no wait for the dead
instance's lock to expire) and a new write lands, reusing the same
session JWT from before the kill: `internal/session` holds no
server-side session table, so a stateless client reconnecting after a
restart is exactly what the design already commits to.

## The local Playwright suite and --fake-host dev mode

`e2e/playwright/` runs a fast Playwright suite against the real Go binary,
with no Drive and no docker: `internal/wopitest` implements an in-memory
WOPI host that mimics Drive's status-code quirks and lock/empty-file
rules closely enough that `internal/wopiclient` plus
`internal/hostadapter.Drive` work against it unchanged (see
`internal/wopitest/host_test.go`).

Setting `EXCALIDRAW_WOPI_FAKE_HOST=1` makes `internal/app.NewServer`
additionally mount that host at `/fakewopi/files/`, with two users
(`alice`, a writer; `bob`, read-only) and one file, `f-local`, that starts
empty. It also serves `GET /fakewopi/launch?user=alice|bob`, an
auto-submitting HTML form that reproduces the WOPI action-URL launch POST
(WOPISrc query parameter, `access_token`/`access_token_ttl` form fields),
and `GET /fakewopi/_state`, a dev-only introspection endpoint returning
`{size, version, putCount}` for `f-local`. The service logs a loud warning
whenever fake-host mode is on; never set this in production.

```sh
make e2e-local
```

This runs `make build`, then `cd e2e/playwright && npm ci && npx
playwright install --with-deps chromium` (falling back to a plain
`playwright install chromium` when the sandbox has no route to apt, e.g.
a locked-down container without root-level package install), then `npx
playwright test` (`playwright.config.ts` sets `testDir: './specs/local'`,
so no path argument is needed). The webServer config starts
`bin/excalidraw-wopi` itself with `EXCALIDRAW_WOPI_FAKE_HOST=1` on a
fixed port (`:8085`); it needs no `make e2e-up`.

`specs/local/convergence.spec.ts` covers three scenarios: two writers
converge on a drawn rectangle; the syncer's REST save reaches the Go
server (`PUT /api/board`); and a read-only session cannot draw but still
receives element updates the writer broadcasts. The test hook it reads
scene state through, `window.__excaTest`, is a small, read-only accessor
(`getElements`, `getAppState`, `getFiles`, `getCollabState`) that
`web/src/stores/useExcalidrawStore.ts` populates once the Excalidraw API
is available. It exposes nothing the page's own script could not already
read from the Excalidraw API directly, but it still stays gated, not
always-on: it needs either Vite dev mode or the Go server's own
`testHooks` config flag (see "The test-hooks knob" below).

Two things worth knowing if this suite ever goes red:

- `PUT /api/board` stores a save in the room manager, and the manager's
  background loop flushes it to the WOPI host on its own save cadence.
  The save test polls `/fakewopi/_state` until `putCount >= 1`.
- `web/src/styles/base.css` gives `html`, `body`, `#root`, and `.App` a
  definite height. Without it, the canvas renders at 0x0 in a real
  browser. The convergence spec relies on that stylesheet.

## The dockerized-Drive smoke suite

`e2e/playwright/specs/smoke/smoke.spec.ts` is the PR gate: it runs
against the real stack `make e2e-up` brings up (Drive on
`:8000`, our own binary on `:8080`), not the local suite's `--fake-host`
mode. Bring the stack up first, then:

```sh
make e2e-up
make e2e-smoke
make e2e-down
```

`e2e-smoke` checks the Drive stack and our binary are already answering
(a helpful error otherwise, since bringing the stack up takes several
minutes and must not happen on every test run), then runs `npx playwright
test --config=playwright.smoke.config.ts` (`testDir: './specs/smoke'`,
so no path argument is needed). `playwright.smoke.config.ts` starts no
`webServer` of its own, unlike `playwright.config.ts`.

### Fixtures

- `e2e/playwright/fixtures/drive.ts`: a typed `DriveClient`, one instance
  per logged-in Drive user (its own cookie jar), porting
  `e2e/scripts/seed.mjs`'s login/create/upload/poll recipe into
  reusable methods instead of shelling out to the script -- a test needs
  two users logged in at once (a writer and a read-only sharee), which a
  one-shot CLI script cannot give it. `shareReadOnly(itemId, userId)`
  grants read-only access through Drive's direct accesses grant, `POST
  /api/v1.0/items/{id}/accesses/` with `{"user_id": ..., "role":
  "reader"}` (`core/api/viewsets.py`'s `ItemAccessViewSet`; matches every
  fixture in `core/tests/items/test_api_item_accesses_create.py`) --
  not the by-email invitations flow, since the caller already knows the
  target user's id from `DriveClient.me()`. `itemDetail(itemId)` reads
  plain, unsigned item metadata (`updated_at`, `size`) as the save-landed
  signal: a raw WOPI `CheckFileInfo` call from a test would need a valid
  `X-WOPI-Proof` signature, since our discovery XML always publishes a
  proof-key and Drive's `WopiViewSet._verify_request_signature`
  (`wopi/viewsets.py:74-114`) enforces that for every caller, not just
  ones Drive itself already trusts -- confirmed against the live stack,
  an unsigned call answers 500. Only our Go server holds
  the proof private key and calls `CheckFileInfo` itself, once per
  `/launch`; it exposes no public passthrough for a test to reuse that
  call.
- `e2e/playwright/fixtures/launch.ts`: `openEditor(page, wopi)` drives the
  real launch flow exactly as Drive's `WopiEditorFrame.tsx` iframe form
  POST does (`access_token`/`access_token_ttl` hidden fields, POSTed to
  `wopi.launchUrl`), via `page.setContent` plus an auto-submit script --
  the same shape `internal/app/fakehost.go`'s `/fakewopi/launch` page
  uses for the local suite. `openSession(browser, wopi)` wraps it in a
  fresh browser context.

### The test-hooks knob

`window.__excaTest` (`web/src/stores/useExcalidrawStore.ts`) needs
`testHooks: true` in the injected launch config
(`internal/launch/launch.go`'s `appConfig`). The local suite gets it for
free from `--fake-host` mode (`launch.WithTestHooks()`), but this suite
launches through the real Drive flow, which never sets it. Setting
`EXCALIDRAW_WOPI_TEST_HOOKS=1` on our binary (`e2e/env/excalidraw.env`)
makes `internal/app.NewServer` pass `launch.WithTestHooks()` independently
of `--fake-host` (`internal/app/testhooks.go`): e2e-only, documented,
default off, and it carries no loopback restriction the way `--fake-host`
does, since the hook is a read-only accessor that discloses nothing a
page's own script could not already read off the live Excalidraw API.
`__excaTest` also grew a `getFiles()` accessor (`ExcalidrawImperativeAPI.getFiles()`)
for this suite's image-propagation assertion.

### Scenario notes

- The image scenario dispatches a synthetic `paste` `ClipboardEvent`
  carrying a PNG `File` on the `.excalidraw` container element (falling
  back to `document.body`), the technique excalidraw's own test suite
  uses for its native paste-to-insert-image handling; this needs no
  dependency on the DOM shape of any hidden file input the library
  renders internally. A bubbling dispatch from that container still
  reaches a `document`-level listener, if the library ever binds one
  there instead.
- The save-version scenario seeds a second, still-empty item just for
  itself: room.Manager's save schedule (`internal/room/manager.go`) skips
  the wait for a room's very first-ever save (`saveDueLocked`'s
  `lastSaveAttemptAt.IsZero()` branch), so drawing on a never-saved file
  flushes almost immediately. A later save on that same room follows
  its normal cadence: a 30s idle flush
  (`room.idleFlushInterval`) once new edits stop, or a 60s ceiling
  (`hostadapter.ServerSaveInterval`) under continuous editing, whichever
  comes first (see `SAVE_POLL_TIMEOUT_MS`'s own comment in
  `smoke.spec.ts`). Reusing the convergence/image scenarios' item here
  would make this timing depend on their prior draws instead.
- The reopen and read-only scenarios reuse that same file afterwards, so
  they keep exercising the save scenario's own saved content, matching
  the save-then-reopen-then-read-only order this suite follows.

## Nightly suite

`make e2e-nightly` runs `e2e-smoke` as a sanity gate, then
`e2e/nightly/` (a vitest socket-level suite: syncer failover under
load, a reconnection storm) and `e2e/playwright/specs/nightly/` (the
conflict prompt, token expiry, and a lock-TTL check that waits roughly
35 minutes by design). It needs `make e2e-up` first, and the full run
takes roughly 60-75 minutes. See `docs/E2E.md` for the full scenario
list, how to run one spec alone, and the redis TTL probe command.

## A fuse-overlayfs docker quirk

Under the fuse-overlayfs storage driver, the docker daemon occasionally
drops a container create with `symlink /proc/mounts .../etc/mtab: file
exists`. This is a driver-level race, not a problem in this compose file
or in Drive's image: it reproduces even on a bare
`docker run python:3.13-alpine`, and a plain retry clears it.
`e2e-up.sh` retries `docker compose up` a few times for exactly this
reason. If `make e2e-up` still fails with that message after its
retries, retry `make e2e-up` again by hand.
