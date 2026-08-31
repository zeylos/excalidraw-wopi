// Proves failover: after the room's owning instance is SIGKILLed, the
// surviving instance re-locks the file and a new write still lands, using
// the same session JWT the client already held. This runs last (the
// numeric file prefix keeps vitest's fixed file order, set by
// fileParallelism: false in vitest.config.mjs): it permanently kills one
// backend, so no later test in this suite can still assume both are up.
import { afterAll, describe, expect, it } from "vitest";
import {
  CONTROL_URL,
  PROXY_URL,
  WOPIHOST_URL,
  connectSocket,
  getOwner,
  getState,
  isAlive,
  killBackend,
  launchSession,
  pollUntil,
  putBoard,
  seedFile,
  waitForEvent,
  withTimeout,
} from "./helpers.mjs";

describe("failover", () => {
  let socket;

  afterAll(() => {
    socket?.close();
  });

  it("a killed owner's room re-locks on the survivor and keeps saving", async () => {
    const { fileId, wopiSrc, tokens } = await seedFile(WOPIHOST_URL, { file: "ha-failover", writers: ["erin"] });
    const { sessionToken, apiBase } = await launchSession(PROXY_URL, wopiSrc, tokens.erin);
    const owner = await getOwner(PROXY_URL, fileId);

    socket = await connectSocket(PROXY_URL, fileId, sessionToken);
    const designate = withTimeout(waitForEvent(socket, "sync-designate"), "first join sync-designate");
    socket.emit("join-room", fileId);
    await designate;

    const firstScene = JSON.stringify({ marker: "before-failover" });
    await putBoard(PROXY_URL, apiBase, fileId, sessionToken, firstScene);

    const beforeState = await pollUntil(
      async () => {
        const s = await getState(WOPIHOST_URL, fileId);
        return s && s.putCount >= 1 ? s : null;
      },
      { timeoutMs: 15000, intervalMs: 300, label: "the first save before failover" },
    );
    expect(beforeState.contentText).toBe(firstScene);
    expect(beforeState.lock).not.toBe("");

    await killBackend(CONTROL_URL, owner);
    await pollUntil(async () => !(await isAlive(CONTROL_URL, owner)), {
      timeoutMs: 5000,
      intervalMs: 100,
      label: "the killed owner to exit",
    });

    // The hashproxy's health poll runs every 500ms (see
    // e2e/ha/hashproxy/main.go's healthInterval); wait for it to eject
    // the dead backend and start naming the survivor as fileId's owner.
    const survivor = await pollUntil(
      async () => {
        const current = await getOwner(PROXY_URL, fileId);
        return current !== owner ? current : null;
      },
      { timeoutMs: 5000, intervalMs: 200, label: "the proxy to reroute fileId to the survivor" },
    );
    expect(survivor).not.toBe(owner);

    socket.close();
    // The session JWT carries no server-side session table (see
    // internal/session), so the same token from before the kill still
    // authenticates a fresh connection through the proxy.
    socket = await connectSocket(PROXY_URL, fileId, sessionToken);
    const designateAfter = withTimeout(waitForEvent(socket, "sync-designate"), "post-failover sync-designate");
    socket.emit("join-room", fileId);
    await designateAfter;

    const secondScene = JSON.stringify({ marker: "after-failover" });
    await putBoard(PROXY_URL, apiBase, fileId, sessionToken, secondScene);

    const afterState = await pollUntil(
      async () => {
        const s = await getState(WOPIHOST_URL, fileId);
        return s && s.contentText === secondScene ? s : null;
      },
      { timeoutMs: 15000, intervalMs: 300, label: "the post-failover save to land" },
    );
    expect(afterState.putCount).toBeGreaterThan(beforeState.putCount);
    expect(afterState.lock).not.toBe("");
  });
});
