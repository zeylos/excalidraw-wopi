# Test targets

One reference page for every `make` test target: what it does, when to
run it, and which invariant or failure mode it proves. See
`docs/ARCHITECTURE.md`'s "Test pyramid" section for the short table
form, and `e2e/README.md` for the dockerized Drive stack's internals.

## `make go-test`

**What it does.** `go test ./...`. Every `internal/` package in
isolation: config validation, proof key lifecycle, session sealing,
the WOPI client against fakes, the room save/lock state machine, the
relay's registry, election, and broadcast logic.

**When to run it.** Every PR (`ci.yml`, the `go` job). Run it locally
before every commit; it needs no Drive, no docker, and no browser.

**The goal.** Fast, deterministic proof that one package's own logic
holds, with every dependency faked or stubbed.

## `make web-test`

**What it does.** `tsc --noEmit` plus `vitest run` for `web/src`.
Every hook, store, and pure util in isolation (the `*.test.ts(x)`
files next to their sources).

**When to run it.** Every PR (`ci.yml`, the `web` job).

**The goal.** Type safety and unit-level proof for the frontend, with
no browser and no server.

## `make test`

**What it does.** `go-test` plus `web-test` in sequence.

**When to run it.** The PR-gate unit layer. Run it before you open a
pull request.

**The goal.** One command that catches most regressions in seconds,
before a slower suite ever starts.

## `make interop`

**What it does.** `cd e2e/interop && npm ci && npx vitest run`. A
real `socket.io-client` v4 drives the Go relay (`internal/relay`) over
a real WebSocket: handshake auth, join/presence, broadcast
byte-identity and read-only drop, volatile identity rewriting,
image-get, oversize-payload rejection, syncer failover on disconnect.

**When to run it.** Every PR (`ci.yml`, the `interop` job). No Drive
needed.

**The goal.** Proof that the relay's wire contract matches a real
socket.io client, not only the Go server's own test doubles, and that
"the relay forwards scene bytes untouched" and "one syncer at a time"
hold under a real connection.

## `make e2e-ha`

**What it does.** `cd e2e/ha && npm ci && npx vitest run`. Two
`bin/excalidraw-wopi` instances behind a rendezvous-hash proxy, against
a standalone `internal/wopitest` host: same-file presence lands on one
instance, a write reaches the host, two files whose hashes disagree
land on different instances, and a SIGKILL of the owner fails over to
the survivor.

**When to run it.** Every PR (`ci.yml`, the `e2e-ha` job). No Drive, no
docker, no browser; `e2e/ha/global-setup.mjs` builds the binary itself.

**The goal.** Proof of "one room has one owner replica" and "the
session is stateless" (a reconnecting client re-locks and resumes
after its owner instance dies), under the real rendezvous-hash
routing, not a unit-test stand-in for it.

## `make e2e-local`

**What it does.** `make build`, then `cd e2e/playwright && npm ci &&
(npx playwright install --with-deps chromium 2>/dev/null || npx
playwright install chromium) && npx playwright test`
(`specs/local/convergence.spec.ts`). The real Go binary, run with `EXCALIDRAW_WOPI_FAKE_HOST=1`, no Drive,
no docker: two writers converge on a drawn rectangle, the syncer's REST
save reaches `PUT /api/board`, and a read-only session cannot draw but
still receives the writer's broadcasts.

**When to run it.** Every PR (`ci.yml`, the `e2e-local` job). Also the
fastest manual browser loop during frontend work: it builds and starts
the binary itself, on a fixed port, so it needs no `make e2e-up`.

**The goal.** Proof, in a real browser, that collaboration and
read-only enforcement hold end to end, not only at the unit or relay
level.

## `make e2e-up` / `make e2e-seed` / `make e2e-down`

**What it does.** `e2e-up` fetches and starts a dockerized La Suite
Drive (`e2e/compose.yaml`, pinned to `v0.21.1`) plus this project's own
binary as a host process, and registers the discovery XML against
Drive. `e2e-seed` creates one excalidraw item through Drive's e2e auth
bypass and prints its WOPI launch payload. `e2e-down` stops the binary
and tears the stack down.

**When to run it.** `e2e-up` is a prerequisite for
`test-drive-integration`, `e2e-smoke`, and `e2e-nightly`; every one of
those three needs the stack up first. `e2e-seed` is a manual
convenience for exploring a launch by hand; the automated suites seed
their own fixtures.

**The goal.** Give the slower suites a real Drive host to launch from,
lock against, and save through. See `e2e/README.md` for the full
stack detail (host-networking constraint, port map, env derivation).

## `make test-drive-integration`

**What it does.** `go test -tags driveintegration -count=1 -v
./e2e/integration/`. `internal/wopiclient` against the live,
dockerized Drive: signed `CheckFileInfo`/`GetFile`, the empty-file
`PutFile` rule, the full lock lifecycle, version-move detection,
token/proof rejection.

**When to run it.** Nightly (`nightly.yml`, the `drive-integration`
job). Needs `make e2e-up` first.

**The goal.** Proof that the signed WOPI client speaks the real Drive
dialect, not only `internal/wopitest`'s in-memory approximation of it.

## `make e2e-smoke`

**What it does.** `cd e2e/playwright && npm ci && npx playwright
install --with-deps chromium && npx playwright test
--config=playwright.smoke.config.ts` (`specs/smoke`). Against the live
Drive stack and the real binary: launch from Drive, two browsers
drawing at the same time and converging, an image paste, a save with
an ETag check, a reopen, and read-only enforcement. Checks the stack
and the binary are already answering before it starts.

**When to run it.** Every PR (`e2e-smoke.yml`) and the first stage of
`make e2e-nightly`. Needs `make e2e-up` first.

**The goal.** The PR-gate proof that a real Drive launch, a real save
round trip, and read-only enforcement hold against Drive itself, not
only against the fake host.

## `make e2e-nightly`

**What it does.** `e2e-smoke`, then two slower suites against the same
live Drive stack:

- `cd e2e/nightly && npm ci && npx vitest run`
  (`e2e/nightly/10-syncer-failover.test.mjs`,
  `e2e/nightly/20-reconnect-storm.test.mjs`): a vitest socket-level
  suite, no browser. `10-syncer-failover` runs four writers under
  continuous cursor traffic (`server-volatile-broadcast`) plus a save
  ticker, through three failover rounds. It checks exactly one
  `sync-designate` holder at a time, and polls Drive's `itemDetail` to
  confirm saves keep landing under that load. `20-reconnect-storm` runs
  one anchor socket plus eight churn sockets under a continuous
  durable-broadcast load (`server-broadcast`), through five join/leave
  rounds. It checks roster convergence, then polls the same
  `itemDetail` to confirm the room's close-grace flush lands after the
  anchor closes.
- `cd e2e/playwright && npx playwright test
  --config=playwright.nightly.config.ts`
  (`e2e/playwright/specs/nightly/`, workers 1, retries 0):
  - `01-conflict.spec.ts`: an out-of-band Drive upload moves the
    item's version while a second editor joins. The conflict banner
    appears. Both the Overwrite path and the Reload path are checked.
  - `02-token-expiry.spec.ts`: a launch uses a short, client-supplied
    `access_token_ttl`. After expiry, `/api/board` answers 401. A new
    socket handshake is refused with "Authentication error". A stale
    relaunch answers 400. The already-open socket stays online by
    design. A fresh relaunch works again.
  - `03-lock-ttl.spec.ts`: roughly 35 minutes of real wait by design.
    It polls the redis lock key `drive:1:wopi_lock:<itemId>`'s TTL
    once a minute. A refresh bumps the TTL back toward 1800s after the
    10-minute refresh interval. An edit made past the 30-minute lock
    TTL still saves.

The full run takes roughly 60-75 minutes, mostly the lock-TTL wait.
The kill-and-restart re-lock scenario is future work here; `make
e2e-ha` already covers the restart path, against the fake host instead
of live Drive.

**When to run it.** Nightly, by cron, in CI (`nightly.yml`, 03:00 UTC).
A person can also run it by hand, through `workflow_dispatch`. It needs
`make e2e-up` first; `e2e-smoke` runs first as a sanity gate, so a
broken stack fails fast instead of burning the full 60-75 minutes.

**The goal.** Catch the slow, time-based, and load-shaped failure
modes the PR gate cannot afford to wait for: the conflict prompt under
a real out-of-band edit, token expiry cutting a session off cleanly,
the lock surviving its own TTL and refresh cycle, and the syncer
election and roster staying correct under load and churn.

### Running one nightly spec alone

A single browser spec:

```sh
cd e2e/playwright && npx playwright test --config=playwright.nightly.config.ts specs/nightly/01-conflict.spec.ts
```

A single socket-level suite:

```sh
cd e2e/nightly && npx vitest run 10-syncer-failover.test.mjs
```

Both need `make e2e-up` to have already started the stack.

### Probing the lock TTL by hand

`03-lock-ttl.spec.ts` polls this same command; run it yourself to
check a lock's remaining time against a live stack:

```sh
docker compose -f e2e/compose.yaml exec -T redis redis-cli TTL "drive:1:wopi_lock:<itemId>"
```

## CI mapping

| Workflow | Targets it runs |
|---|---|
| `ci.yml` | `go-test`, `lint` (Go job); `web-test`, `web-lint` (web job); `interop`; `e2e-ha`; `e2e-local`; `chart-lint`; `build`. |
| `e2e-smoke.yml` | `e2e-up`, `e2e-smoke`, `e2e-down`. Runs on push to `main`, on every pull request, and on manual dispatch. |
| `nightly.yml` | `e2e-up`, `e2e-nightly`, `e2e-down` (the `nightly` job); `e2e-up`, `test-drive-integration`, `e2e-down` (the `drive-integration` job). Runs on cron at 03:00 UTC, and on manual dispatch. |
