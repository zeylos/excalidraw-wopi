// Nightly load test: four editors hold the room open under continuous
// cursor traffic and a steady save cadence while the syncer socket gets
// killed and re-elected three times in a row. Proves internal/relay's
// syncer election (rooms.go's electSyncer) and internal/room's re-lock
// path hold up under repeated churn, not just a single failover
// (e2e/interop/relay.test.mjs's test "k" already covers that single
// case). Runs against the stack `make e2e-up` brings up.
import { afterAll, describe, expect, it } from "vitest";
import { EXCALIDRAW_URL, connectSocket, getBoard, pollUntil, putBoard, setupUsers } from "./helpers.mjs";

const ROUNDS = 3;
const SETTLE_WINDOW_MS = 2000;
const SETTLE_CHECK_INTERVAL_MS = 100;
const VOLATILE_INTERVAL_MS = 200;
const SAVE_TICK_INTERVAL_MS = 1000;

describe("syncer failover under load", () => {
  let members = [];
  let volatileHandle;
  let saveTickHandle;

  function attachTracking(member) {
    member.isSyncer = false;
    member.designateReceived = false;
    member.errorCount = 0;
    member.socket.on("sync-designate", (payload) => {
      member.isSyncer = payload.isSyncer;
      member.designateReceived = true;
    });
    member.socket.on("error", () => {
      member.errorCount++;
    });
  }

  async function join(member) {
    member.socket.emit("join-room", member.user.session.fileId);
    await pollUntil(() => member.designateReceived, {
      timeoutMs: 10000,
      label: `${member.user.session.userId} first sync-designate`,
    });
  }

  function currentSyncers() {
    return members.filter((m) => m.socket.connected && m.isSyncer);
  }

  // startVolatileLoad emits a MOUSE_LOCATION cursor update from every
  // connected member every VOLATILE_INTERVAL_MS: the type this suite's
  // load must use, since it is the one server-volatile-broadcast never
  // drops as unparseable (internal/relay/broadcast.go's rewriteVolatile).
  function startVolatileLoad() {
    volatileHandle = setInterval(() => {
      for (const m of members) {
        if (!m.socket.connected) {
          continue;
        }
        const payload = Buffer.from(JSON.stringify({
          type: "MOUSE_LOCATION",
          payload: {
            pointer: { x: Math.random() * 1000, y: Math.random() * 1000 },
            user: { id: m.user.session.userId, name: m.user.session.userName },
          },
        }));
        m.socket.emit("server-volatile-broadcast", m.user.session.fileId, payload);
      }
    }, VOLATILE_INTERVAL_MS);
  }

  // startSaveTicker has whichever member currently holds the syncer role
  // save once a second, mirroring the frontend's own syncer-only save
  // policy. A tick that finds zero or more than one syncer (a brief
  // in-flight election) skips silently: it is background load, not an
  // assertion, and stopSaveTicker pauses it around each round's own
  // deterministic marker write below.
  let saveCounter = 0;
  let saveTickInFlight = null;
  function startSaveTicker() {
    saveTickHandle = setInterval(() => {
      const syncers = currentSyncers();
      if (syncers.length !== 1) {
        return;
      }
      saveTickInFlight = putBoard(syncers[0].user.session, JSON.stringify({ marker: "load-tick", counter: saveCounter++ }))
        .catch(() => {
          // A save mid-election can hit a room that just lost its lock
          // holder; the next tick retries. Only the round-marker write
          // below is a hard assertion.
        })
        .finally(() => {
          saveTickInFlight = null;
        });
    }, SAVE_TICK_INTERVAL_MS);
  }
  // stopSaveTicker awaits any tick's putBoard already in flight, so a
  // late-landing tick save cannot overwrite the round marker written
  // right after this returns.
  async function stopSaveTicker() {
    clearInterval(saveTickHandle);
    await saveTickInFlight;
  }

  afterAll(() => {
    clearInterval(volatileHandle);
    clearInterval(saveTickHandle);
    for (const m of members) {
      m.socket.close();
    }
  });

  it(
    "the syncer role survives three consecutive kill-and-reconnect rounds",
    async () => {
      const { item, owner, users } = await setupUsers(4, { filename: `nightly-syncer-failover-${Date.now()}.excalidraw` });
      const beforeDetail = await owner.itemDetail(item.id);

      for (const user of users) {
        // Mirrors the browser flow: the editor's first GET /api/board runs
        // before it opens the relay socket, so Manager.OnJoin finds a room
        // (internal/room/manager.go:475-487).
        await getBoard(user.session);
        const socket = await connectSocket(user.session);
        const member = { user, socket };
        attachTracking(member);
        members.push(member);
      }
      await Promise.all(members.map(join));

      startVolatileLoad();
      startSaveTicker();

      for (let round = 1; round <= ROUNDS; round++) {
        const syncer = await pollUntil(() => {
          const syncers = currentSyncers();
          return syncers.length === 1 ? syncers[0] : null;
        }, { timeoutMs: 15000, intervalMs: 200, label: `round ${round} exactly one syncer before failover` });

        members = members.filter((m) => m !== syncer);
        syncer.socket.close();

        const newSyncer = await pollUntil(() => {
          const syncers = currentSyncers();
          return syncers.length === 1 && syncers[0] !== syncer ? syncers[0] : null;
        }, { timeoutMs: 15000, intervalMs: 200, label: `round ${round} a different socket becomes syncer` });
        expect(newSyncer).not.toBe(syncer);

        const settleDeadline = Date.now() + SETTLE_WINDOW_MS;
        while (Date.now() < settleDeadline) {
          const syncers = currentSyncers();
          expect(syncers).toHaveLength(1);
          expect(syncers[0]).toBe(newSyncer);
          await new Promise((r) => setTimeout(r, SETTLE_CHECK_INTERVAL_MS));
        }

        await stopSaveTicker();
        const marker = `round-${round}-marker`;
        const scene = JSON.stringify({ marker });
        await putBoard(newSyncer.user.session, scene);
        // Proves the REST round trip through internal/room's store, not
        // that the scene reached Drive; the itemDetail poll below is the
        // Drive-level proof for the whole test.
        await pollUntil(async () => (await getBoard(newSyncer.user.session)) === scene, {
          timeoutMs: 10000,
          intervalMs: 200,
          label: `round ${round} marker scene readable back from the room store`,
        });
        startSaveTicker();

        await getBoard(syncer.user.session);
        const replacementSocket = await connectSocket(syncer.user.session);
        const replacement = { user: syncer.user, socket: replacementSocket };
        attachTracking(replacement);
        await join(replacement);
        members.push(replacement);
      }

      await stopSaveTicker();
      clearInterval(volatileHandle);

      for (const m of members) {
        expect(m.errorCount, `${m.user.session.userId}'s socket received an error event`).toBe(0);
      }

      const healthzRes = await fetch(`${EXCALIDRAW_URL}/healthz`);
      expect(healthzRes.status).toBe(200);

      await pollUntil(async () => {
        const detail = await owner.itemDetail(item.id);
        return detail.updatedAt !== beforeDetail.updatedAt || detail.size !== beforeDetail.size ? detail : null;
      }, { timeoutMs: 90000, intervalMs: 1000, label: "the item's Drive metadata to move past its pre-test snapshot" });
    },
    360_000,
  );
});
