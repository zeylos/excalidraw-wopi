// Proves two clients of one file land on the same backend behind the
// hashproxy: they only see each other's join-room presence when both
// requests hashed to the same instance's in-memory relay room.
import { afterAll, describe, expect, it } from "vitest";
import {
  PROXY_URL,
  WOPIHOST_URL,
  connectSocket,
  launchSession,
  seedFile,
  waitForEvent,
  withTimeout,
} from "./helpers.mjs";

describe("presence through the hashproxy", () => {
  let alice;
  let bob;

  afterAll(() => {
    alice?.close();
    bob?.close();
  });

  it("two writers of the same file see each other join", async () => {
    const { fileId, wopiSrc, tokens } = await seedFile(WOPIHOST_URL, {
      file: "ha-presence",
      writers: ["alice", "bob"],
    });

    const aliceConfig = await launchSession(PROXY_URL, wopiSrc, tokens.alice);
    const bobConfig = await launchSession(PROXY_URL, wopiSrc, tokens.bob);

    alice = await connectSocket(PROXY_URL, fileId, aliceConfig.sessionToken);
    const aliceDesignate = withTimeout(waitForEvent(alice, "sync-designate"), "alice sync-designate");
    alice.emit("join-room", fileId);
    const [aliceFirst] = await aliceDesignate;
    expect(aliceFirst).toEqual({ isSyncer: true });

    const roomChangeAfterBob = withTimeout(waitForEvent(alice, "room-user-change"), "room-user-change after bob");
    bob = await connectSocket(PROXY_URL, fileId, bobConfig.sessionToken);
    const bobDesignate = withTimeout(waitForEvent(bob, "sync-designate"), "bob sync-designate");
    bob.emit("join-room", fileId);
    const [bobFirst] = await bobDesignate;
    expect(bobFirst).toEqual({ isSyncer: false });

    const [presence] = await roomChangeAfterBob;
    expect(presence).toHaveLength(2);
    expect(presence.map((row) => row.userId).sort()).toEqual(["alice", "bob"]);
  });
});
