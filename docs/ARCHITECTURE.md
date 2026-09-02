# Architecture tour

A compact map of how `excalidraw-wopi` fits together: the request
flow, the package layout, the save pipeline, the lock lifecycle, the
session model, and the test pyramid. This document describes what the
code, as built, actually does.

## Request flow

```
Browser ── iframe form POST (access_token, access_token_ttl, WOPISrc) ──> POST /launch
Browser <── static TS bundle (go:embed), session JWT injected ────────── POST /launch response
Browser <──── socket.io /socket.io/ (auth: session JWT) ───────────────> Go binary (internal/relay)
Browser <──── GET/PUT /api/board (bearer: session JWT) ────────────────> Go binary (internal/boardapi)
Go binary ── CheckFileInfo / GetFile / Lock / RefreshLock / PutFile / Unlock, proof-signed ──> Drive {WOPISrc}
Drive celery ── GET /hosting/discovery (nightly, or on trigger_wopi_configuration) ──> GET /hosting/discovery
```

One repository, two parts, one binary:

```
/web      TypeScript React SPA
          @excalidraw/excalidraw 0.18.x + hooks/stores ported from
          nextcloud/whiteboard's src/ (see the root README.md's
          "Provenance and license" note)
/internal Go packages
          discovery XML, launch, WOPI client + Drive host profile,
          session JWTs, board REST API, room save/lock orchestrator,
          socket.io relay, go:embed of the /web build
```

Every request the browser makes lands on one process, on one port
(`EXCALIDRAW_WOPI_LISTEN_ADDR`), routed by one `http.ServeMux`
(`internal/httpserver`, wired up in `internal/app.NewServer`):

| Route | Handler package | Notes |
|---|---|---|
| `GET /healthz` | `internal/httpserver` | Liveness/readiness probe. |
| `GET /hosting/discovery` | `internal/discovery` | WOPI discovery XML; publishes the proof-key public parts. |
| `POST /launch` | `internal/launch` | Validates the WOPI access token live, mints a session JWT, serves `index.html` with launch config injected. |
| `GET /api/board`, `PUT /api/board` | `internal/boardapi` | Bearer-authenticated (session JWT) board scene REST API. |
| `GET /api/board/conflict`, `POST /api/board/conflict/resolve` | `internal/boardapi` | Conflict-state poll and resolution; see "Lock lifecycle" below. |
| `/socket.io/` | `internal/relay` | The realtime collaboration channel. |
| `/` (fallback for any other path with no file extension) | `internal/httpserver` | Serves the embedded SPA bundle; unknown extensionless paths fall back to `index.html` for client-side routing. |
| `/fakewopi/*` (dev only) | `internal/app` (`fakehost.go`) | Mounted only when `EXCALIDRAW_WOPI_FAKE_HOST=1`; never present in production. |

## Package map

**Go, `internal/`:**

| Package | One-line role |
|---|---|
| `config` | Loads and validates every `EXCALIDRAW_WOPI_*` environment variable into one `Config` struct. |
| `proof` | Owns the RSA proof keypair: load-or-generate-and-persist, PEM env injection, current/old public parts for discovery. |
| `discovery` | Renders and serves the WOPI discovery XML (`GET /hosting/discovery`). |
| `wopiclient` | The signed WOPI HTTP client: `CheckFileInfo`, `GetFile`, `Lock`, `RefreshLock`, `PutFile`, `Unlock`, each request proof-signed. |
| `hostadapter` | Every Drive-specific WOPI quirk (403-not-401, the `Version`-as-ETag choice, the save/lock timing constants) behind one profile, so the rest of the service speaks plain WOPI. |
| `session` | Mints and verifies the session JWT: HS256-signed, AES-256-GCM-sealed WOPI access token inside. |
| `launch` | `POST /launch`: validates the WOPI access token, mints the session, serves the SPA with launch config injected. |
| `boardapi` | `GET`/`PUT /api/board`: the frontend-facing scene REST API. |
| `room` | The save-and-lock orchestrator: one `Manager` per process, one `roomState` per open board, driving the host save throttle, the WOPI lock lifecycle, and conflict detection. |
| `relay` | The socket.io realtime layer: handshake auth, rooms, presence, broadcasts, the volatile (cursor/viewport) channel, image relay, syncer election. |
| `peers` | Multi-replica ownership: the rendezvous-hash owner function, static/DNS peer discovery, and the routing middleware for `/socket.io/` and `/api/board*`. |
| `httpserver` | Builds the `http.Server` and the base mux (health check, static SPA handler); other packages register their own routes onto the same mux. |
| `app` | Wires every package above into one `*http.Server`; also the `--fake-host` dev-mode wiring (`fakehost.go`). |
| `wopitest` | An in-memory WOPI host that mimics Drive's status-code quirks and lock/empty-file rules, for the fake-host dev mode and `internal/wopiclient`'s own tests. |

**TypeScript, `web/src/`:**

| Area | One-line role |
|---|---|
| `App.tsx`, `main.tsx`, `bootstrap.ts` | Entry point and top-level component: reads the injected launch config, decides ready/not-ready, mounts the Excalidraw editor. |
| `config.ts` | The typed shape of the `#ew-config` JSON blob `internal/launch` injects. |
| `stores/` | Zustand stores: session identity, whiteboard config, collaboration connection status, the Excalidraw imperative API handle. |
| `hooks/` | `useCollaboration` (socket.io client lifecycle), `useSync` (the three throttled save paths), `useBoardDataManager` (initial load/reconciliation), `useReadOnlyState` (view-mode wiring). |
| `database/` | Dexie/IndexedDB local persistence for offline-first editing. |
| `workers/syncWorker.ts` | Off-main-thread IndexedDB reads/writes and server `PUT`s. |
| `utils/` | Pure, independently-testable pieces factored out of the hooks above: scene merge/dedup, payload decode/validate, room-join and image-size-limit logic, and more. |
| `types/` | The wire-protocol types shared between the relay client code and the rest of the app. |
| `components/` | `AuthErrorNotification`, `NetworkStatusIndicator`: small UI pieces ported from nextcloud/whiteboard. |
| `styles/` | Base layout CSS and Excalidraw override styles. |

## Peer routing

> [!INFO]
> This is only a thing for Kubernetes or Consul deployments able to provide a DNS
> discovery setup. For every other deployments types, the routing part has to
> be done by the load balancer in front of excalidraw-wopi. See `docs/HIGH-AVAILABILITY.md`.

Several replicas of the process can run at once. Each replica still
holds every open room's state only in its own memory. `internal/peers`
assigns exactly one owner replica per room. The single-lock model then
still has one authority per file, without any shared store.

- **Owner function.** `rendezvousOwner`
  (`internal/peers/rendezvous.go`) scores every peer with a SHA-256
  hash of the file id and that peer, and picks the highest score.
  Every replica holds the same peer set and runs the same function.
  Each one names the same owner for a given file, without asking any
  other replica.
- **Discovery mode.** `EXCALIDRAW_WOPI_DNS_PEERS` (`<host>:<port>`)
  re-resolves that DNS name every 5 seconds, and turns every A/AAAA
  record into a peer `http://<addr>:<port>`. This matches a Kubernetes
  headless Service in front of a plain Deployment, or a Consul-managed
  VM fleet. `EXCALIDRAW_WOPI_DNS_SELF` names this replica's own
  advertised URL; it is required whenever `DNS_PEERS` is set. Peer
  proxy traffic uses HTTP/1.1. An empty `EXCALIDRAW_WOPI_DNS_PEERS`
  means single-replica mode: one replica, no routing layer. See
  `docs/HIGH-AVAILABILITY.md` for the load-balancer alternative that
  runs with this routing layer off entirely.
- **Routing middleware and the `room` hint.** `Cluster.Middleware`
  (`internal/peers/middleware.go`) wraps `/socket.io/` and
  `/api/board*`. It reads the `room` query parameter, the raw fileId
  the frontend sends on every such request. It reverse-proxies to the
  owner when the owner is not this replica. The hint only picks a
  proxy target. It grants no access on its own.
- **Enforcement at the token layer.** The serving replica always
  checks ownership against the fileID inside the verified session
  token, and it does this before the room observer registers that
  token. A non-owner replica answers 421 "wrong replica" on the board
  API. It rejects the socket handshake with the same, distinct message
  "wrong replica" (never the terminal "Authentication error").
  Checking the token, not the `room` hint, is what stops a non-owner
  replica from ever creating a duplicate room.
- **Client-side recovery.** A 421 on the board API surfaces through
  each caller's own failure path. The conflict poll refires on its
  15-second interval. The sync worker retries its `PUT`. The initial
  snapshot fetch falls back to local data instead, and the scene then
  arrives over collaboration. A "wrong replica" handshake rejection
  destroys the client socket. The client schedules a fresh connection
  after 3 seconds, and each fresh handshake re-routes through the
  middleware above.
- **Hop guard.** A proxied request carries the header
  `X-Excalidraw-Wopi-Hop: 1`. A replica can receive a proxied request
  while it is still not the owner. It then answers 421 and logs an
  error, instead of proxying again. The peer sets disagree, and a
  second hop would only hide that fact.
- **Tolerated transient.** A rollout, a scale event, or DNS lag can
  make two replicas briefly disagree on membership. A room can then
  briefly exist on both. This stays safe, because the WOPI lock value
  is deterministic per file: neither replica sees the other as a
  foreign editor. Clients re-sync once membership converges, within
  about one save interval (60 seconds).

## The save pipeline

Four cadences, each throttled independently, feeding into each other:

```
Excalidraw onChange
      │
      ├─ 200 ms  ──> IndexedDB (local)            web/src/hooks/useSync.ts: LOCAL_SYNC_DELAY
      │
      ├─ 500 ms  ──> socket.io broadcast           web/src/hooks/useSync.ts: WEBSOCKET_SYNC_DELAY
      │              (server-broadcast, only from the elected syncer's writes;
      │               relayed to every other room member as client-broadcast)
      │
      └─ 10 s    ──> PUT /api/board (REST)         web/src/hooks/useSync.ts: SERVER_API_SYNC_DELAY
                      (only the elected syncer sends this; internal/boardapi
                      stores it and hands it to internal/room)
                            │
                            ├─ 60 s throttle  ──> WOPI PutFile   hostadapter.ServerSaveInterval
                            ├─ 30 s idle flush ─> WOPI PutFile   room.idleFlushInterval (no new
                            │                     PutScene call for 30s triggers an early save)
                            └─ on last-member-leaves (10 s close grace) ──> WOPI PutFile, then Unlock
```

A fifth interval, `FULL_SCENE_HEALING_INTERVAL` (20 s), is not a save
path: it is a periodic full-scene rebroadcast the syncer sends to
correct any drift a client's incremental updates missed.

`internal/room`'s `Manager` (`internal/room/manager.go`,
`internal/room/save.go`) owns everything below the REST line: it
throttles WOPI `PutFile` calls to at most once per 60 seconds per
room, flushes early after 30 seconds of no new `PutScene` call even if
the 60-second throttle has not elapsed, and flushes once more, with the
room's own lock, when the room's last relay member leaves (after a
10-second grace period that absorbs a quick page-refresh reconnect
without needless unlock/re-lock churn). On process shutdown
(`Manager.Shutdown`), every dirty room gets one final, unthrottled save
attempt.

## Lock lifecycle

One deterministic lock value per file (`internal/room`,
`lockValueFor`), refreshed every 10 minutes
(`hostadapter.LockRefreshInterval`) while the room is "live" (has a
connected relay member, an unexpired observed session token, or unsaved
changes — `Manager.roomLiveLocked`):

1. **Acquire.** Before the first save, `ensureLocked` calls WOPI `LOCK`.
   A success, or a 409 that already carries this room's own lock value
   (a same-value `LOCK` is a refresh under WOPI), both count as
   holding the lock.
2. **Refresh.** Every 10 minutes, `refreshLock` calls `REFRESH_LOCK`.
   A 409 with an *empty* `X-WOPI-Lock` means the lock already expired
   (Drive's `cache.touch` cannot revive an expired lock); that
   candidate falls through to a full re-`LOCK` instead.
3. **Conflict.** Two triggers put a room into conflict state: a 409
   carrying a *foreign* lock value (neither empty nor this room's
   own), or a version check that finds Drive's live `Version` (the S3
   ETag) does not match the version this service recorded from its own
   last successful `PutFile` (`internal/room/save.go`, `checkVersion`;
   it runs on every lock refresh, and when a new user joins an
   already-established room). In conflict state, saves and
   lock refreshes both pause until a user resolves it. The transition
   pushes a `conflict-state` socket.io event
   (`{"inConflict": bool, "saveStalled": bool}`) to every socket in the
   room (`room.WithOnConflictChange`, wired to `relay.BroadcastToRoom`
   in `internal/app/app.go`); `saveStalled` is set once a dirty room
   has failed every save attempt for at least `saveStalledWindow` (5
   minutes), or immediately once a save or lock-refresh pass finds
   every tracked token has lost write access (e.g. the file was
   deleted or write ability was revoked on the host). `GET
   /api/board/conflict` answers the same shape as a poll fallback. A
   writer resolves it with `POST
   /api/board/conflict/resolve` (`{"overwrite": bool}`): `overwrite:
   true` forces the retained scene to the host on the next background
   pass; `overwrite: false` discards it, so the next `GET /api/board`
   proxies fresh host content instead (`internal/boardapi/boardapi.go`,
   `internal/room/manager.go`'s `ResolveConflict`). The reload branch
   also broadcasts a `reload-required` socket.io event to every socket
   in the room (`room.WithOnReloadRequired`, wired the same way), not
   only to the client that resolved the conflict, since every other
   client's local scene is now stale against the host's content too. See
   `docs/DEPLOYMENT.md`'s "Conflict behavior" for the operator-facing
   summary.
4. **Token ladder.** Every WOPI call tries the syncer's own observed
   token first, then falls back to any other tracked writer's token on
   a token rejection; a token that fails is marked and skipped on the
   next attempt, until it expires and is pruned.
5. **Unlock.** When a room closes (empty past its close grace, and not
   dirty), the service calls `UNLOCK` with its lock value and drops the
   room's state. If no usable token remains to unlock with, the lock is
   left for the host's own lock TTL (30 minutes in Drive) to expire.
6. **Empty-file bypass.** `PutFile` against a zero-byte, never-locked
   file succeeds unlocked; `performSave` uses this only as a
   last-resort fallback on a room's very first-ever save attempt, so a
   `LOCK` call failing for an unrelated reason cannot block the first
   write to a brand-new file.

## Session JWT model

Minted once, at `POST /launch`, by `internal/session`:

- **Claims:** file id, WOPISrc, user id, user name, `canWrite`, and an
  `exp` matching the WOPI access token's own end of life (parsed from
  the launch form's `access_token_ttl`, capped at 10 hours from mint
  time).
- **Signing:** HS256, with a signing key HKDF-derived from
  `EXCALIDRAW_WOPI_SESSION_SECRET`.
- **Sealing:** the WOPI access token itself is never a plaintext
  claim. It is AES-256-GCM-sealed under a second HKDF-derived key,
  with the file id bound in as additional authenticated data, so a
  sealed token from one file's session can never be replayed, even in
  principle, as a stand-in for another file's.
- **Statelessness:** the server holds no session table. A client that
  reconnects after a server restart (with `SESSION_SECRET` fixed)
  presents the same JWT; `Verify` unseals the access token, and the
  room orchestrator re-locks the file and resumes work, exactly as if
  the process had never restarted.
- **No renewal:** the JWT is minted once and never refreshed
  (`web/src/stores/useSessionStore.ts` only reads and best-effort
  decodes the `exp` claim to show a client-side warning). The
  server-side token-expiry watch (`internal/room`,
  `checkTokenExpiry`) instead warns, flushes, and is meant to
  disconnect a session before its access token expires; a relaunch
  from the host mints a fresh JWT.
- **Relay auth:** the same JWT authenticates the socket.io handshake
  (`internal/relay`'s `authenticate`); the relay only ever sees the
  unsealed `FileID`/`UserID`/`UserName`/`CanWrite` claims, never the
  WOPI access token itself (`internal/app.sessionVerifier` drops it
  explicitly before handing the claims to the relay).

## Test pyramid

| Layer | Make target | What it covers |
|---|---|---|
| Go unit tests | `make go-test` (`go test ./...`) | Every `internal/` package in isolation. |
| Frontend unit tests | `make web-test` (`tsc --noEmit` + `vitest run`) | Every `web/src` hook, store, and util in isolation. |
| Both of the above | `make test` | The PR-gate unit layer. |
| Relay interop | `make interop` (`e2e/interop`) | A real `socket.io-client` v4 against the Go relay over WebSocket. No Drive needed. |
| HA multi-instance | `make e2e-ha` (`e2e/ha`) | Two instances behind a rendezvous-hash proxy against a standalone `internal/wopitest` host, failover included. No Drive, no docker, no browser. |
| Local Playwright suite | `make e2e-local` (`e2e/playwright/specs/local`) | The real Go binary, fake WOPI host, no Drive, no docker: convergence, the syncer's REST save, read-only enforcement. |
| Drive integration | `make test-drive-integration` (needs `make e2e-up` first) | `internal/wopiclient` against a live, dockerized Drive. |
| Drive smoke e2e | `make e2e-smoke` (needs `make e2e-up` first) | `e2e/playwright/specs/smoke` against a live Drive and our real binary. The PR-gate smoke test (`.github/workflows/e2e-smoke.yml`). |
| Nightly e2e | `make e2e-nightly` (needs `make e2e-up` first) | Smoke gate, socket load/storm, conflict prompt, token expiry, lock TTL; ~60-75 min. Runs nightly in CI (`.github/workflows/nightly.yml`). |

`make lint` (golangci-lint) and `make web-lint` (eslint) are style
gates, not part of the pyramid above. See `docs/E2E.md` for when to
run each target and the full detail.
