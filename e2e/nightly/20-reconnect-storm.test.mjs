// Nightly load test: one anchor socket holds the room open while eight
// churn sockets (two per user, across four users) connect and disconnect
// together, five times over, with jitter so their joins and leaves do not
// land in lockstep. Proves internal/relay's join/leave path (rooms.go's
// registry, guarded by roomEmitLocks) stays correct under concurrent
// membership churn, and that the anchor's own traffic never sees an
// error or a forced disconnect while it happens. Runs against the stack
// `make e2e-up` brings up.
import { afterAll, describe, expect, it } from "vitest";
import { EXCALIDRAW_URL, connectSocket, getBoard, pollUntil, putBoard, setupUsers, waitForEvent, withTimeout } from "./helpers.mjs";

const ROUNDS = 5;
const CHURN_SOCKETS_PER_USER = 2;
const JITTER_MAX_MS = 100;
const BROADCAST_INTERVAL_MS = 200;

function jitter() {
  return new Promise((r) => setTimeout(r, Math.random() * JITTER_MAX_MS));
}

describe("reconnect storm around a stable anchor", () => {
  let anchorSocket;
  let broadcastHandle;

  afterAll(() => {
    clearInterval(broadcastHandle);
    anchorSocket?.close();
  });

  it(
    "the anchor's room-user-change roster tracks five rounds of churn without an error or a forced disconnect",
    async () => {
      const { item, owner, users } = await setupUsers(4, { filename: `nightly-reconnect-storm-${Date.now()}.excalidraw` });
      const anchorUser = users[0];

      let latestRoster = [];
      let anchorErrorCount = 0;
      let anchorDisconnected = false;

      // Mirrors the browser flow: the editor's first GET /api/board runs
      // before it opens the relay socket, so Manager.OnJoin finds a room
      // (internal/room/manager.go:475-487).
      await getBoard(anchorUser.session);
      anchorSocket = await connectSocket(anchorUser.session);
      anchorSocket.on("room-user-change", (presence) => {
        latestRoster = presence;
      });
      anchorSocket.on("error", () => {
        anchorErrorCount++;
      });
      anchorSocket.on("disconnect", () => {
        anchorDisconnected = true;
      });

      const anchorDesignate = withTimeout(waitForEvent(anchorSocket, "sync-designate"), "anchor sync-designate");
      anchorSocket.emit("join-room", anchorUser.session.fileId);
      await anchorDesignate;

      // Prove the room's eager first save (internal/room/roomstate.go's
      // firstSaveDone) before the storm, so the close-grace assertion at
      // the end cannot be satisfied by this same eager save instead of
      // the flush it targets.
      const beforeFirstSave = await owner.itemDetail(item.id);
      await putBoard(anchorUser.session, JSON.stringify({ marker: "reconnect-storm-first" }));
      await pollUntil(async () => {
        const detail = await owner.itemDetail(item.id);
        return detail.updatedAt !== beforeFirstSave.updatedAt || detail.size !== beforeFirstSave.size ? detail : null;
      }, { timeoutMs: 30000, intervalMs: 1000, label: "the room's eager first save to land in Drive" });

      broadcastHandle = setInterval(() => {
        if (!anchorSocket.connected) {
          return;
        }
        const payload = Buffer.from(JSON.stringify({ type: "SCENE_UPDATE", payload: { ts: Date.now() } }));
        anchorSocket.emit("server-broadcast", anchorUser.session.fileId, payload, []);
      }, BROADCAST_INTERVAL_MS);

      // churnUsers lists each user twice, one entry per churn socket that
      // user gets this round: 4 users * 2 sockets = 8, matching the room
      // membership presence dedupes down to 4 distinct rows
      // (internal/relay/rooms.go's presence()), the roster count every
      // round below polls for.
      const churnUsers = users.flatMap((user) => Array(CHURN_SOCKETS_PER_USER).fill(user));
      const expectedRosterCount = users.length;

      async function connectChurnSocket(user) {
        await jitter();
        // Mirrors the browser flow: getBoard before connectSocket, so
        // Manager.OnJoin finds a room (internal/room/manager.go:475-487).
        await getBoard(user.session);
        const socket = await connectSocket(user.session);
        try {
          const designate = withTimeout(waitForEvent(socket, "sync-designate"), `${user.session.userId} churn sync-designate`);
          socket.emit("join-room", user.session.fileId);
          await designate;
          return socket;
        } catch (err) {
          socket.close();
          throw err;
        }
      }

      // connectChurnSockets connects every churn socket for a round
      // concurrently, but if any connect rejects it closes the sockets
      // that did open, instead of leaving them dangling the way a bare
      // Promise.all would.
      async function connectChurnSockets() {
        const results = await Promise.allSettled(churnUsers.map(connectChurnSocket));
        const opened = [];
        let failure;
        for (const result of results) {
          if (result.status === "fulfilled") {
            opened.push(result.value);
          } else {
            failure ??= result.reason;
          }
        }
        if (failure) {
          for (const socket of opened) {
            socket.close();
          }
          throw failure;
        }
        return opened;
      }

      async function closeWithJitter(socket) {
        await jitter();
        socket.close();
      }

      for (let round = 1; round <= ROUNDS; round++) {
        const churnSockets = await connectChurnSockets();

        await pollUntil(() => (latestRoster.length === expectedRosterCount ? latestRoster : null), {
          timeoutMs: 15000,
          intervalMs: 200,
          label: `round ${round} roster reaches ${expectedRosterCount} users`,
        });

        await Promise.all(churnSockets.map(closeWithJitter));

        await pollUntil(
          () => (latestRoster.length === 1 && latestRoster[0].userId === anchorUser.session.userId ? latestRoster : null),
          { timeoutMs: 15000, intervalMs: 200, label: `round ${round} roster shrinks back to the anchor alone` },
        );

        expect(anchorErrorCount, `round ${round}: the anchor received an unexpected error event`).toBe(0);
        expect(anchorDisconnected, `round ${round}: the anchor was forcibly disconnected`).toBe(false);
      }

      clearInterval(broadcastHandle);

      const healthzRes = await fetch(`${EXCALIDRAW_URL}/healthz`);
      expect(healthzRes.status).toBe(200);

      // Snapshot before the storm's marker so the close-grace poll below
      // can prove which flush moved the item. The 60s ServerSaveInterval
      // throttle (internal/room/manager.go) guarantees the background
      // save loop does not flush finalMarker on its own inside this
      // poll's 30s budget, so a move past beforeClose can only come from
      // the close-grace flush (closeGrace, 10s, same file).
      const beforeClose = await owner.itemDetail(item.id);
      const finalMarker = JSON.stringify({ marker: "reconnect-storm-final" });
      await putBoard(anchorUser.session, finalMarker);
      // Proves the REST round trip through internal/room's store, not
      // that the scene reached Drive; the itemDetail poll below is the
      // Drive-level proof for the close-grace assertion.
      await pollUntil(async () => (await getBoard(anchorUser.session)) === finalMarker, {
        timeoutMs: 10000,
        intervalMs: 200,
        label: "the anchor's final marker scene readable back from the room store",
      });

      anchorSocket.close();
      await pollUntil(async () => {
        const detail = await owner.itemDetail(item.id);
        return detail.updatedAt !== beforeClose.updatedAt || detail.size !== beforeClose.size ? detail : null;
      }, { timeoutMs: 30000, intervalMs: 1000, label: "the room's close-grace flush to land in Drive" });
    },
    300_000,
  );
});
