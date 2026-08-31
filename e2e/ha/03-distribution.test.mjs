// Proves two files whose rendezvous hash disagrees land on different
// backends: for each of the two file ids, a client connected through the
// hashproxy and a client connected directly to the predicted owner
// (bypassing the proxy) converge into the same socket.io room only if
// the proxy really did route that file's traffic to that backend.
import { describe, expect, it } from "vitest";
import {
  BACKEND_URLS,
  PROXY_URL,
  WOPIHOST_URL,
  connectSocket,
  getOwner,
  launchSession,
  seedFile,
  waitForEvent,
  withTimeout,
} from "./helpers.mjs";

describe("hash distribution across backends", () => {
  it("two files whose hashes disagree land on different instances", async () => {
    // A well-mixing hash over two backends puts roughly half of any
    // candidate set on each side, so a scan of 20 ids has about a
    // 1-in-500,000 chance of landing entirely on one side.
    // pickDisagreeingPair still throws a clear error on that case,
    // rather than passing on an untested assumption.
    const candidateIds = Array.from({ length: 20 }, (_, i) => `ha-dist-${i}`);
    const owners = new Map();
    for (const id of candidateIds) {
      owners.set(id, await getOwner(PROXY_URL, id));
    }
    expect(new Set(owners.values()).size).toBeGreaterThan(1);

    const [fileX, fileY] = pickDisagreeingPair(owners);
    expect(owners.get(fileX)).not.toBe(owners.get(fileY));

    for (const fileId of [fileX, fileY]) {
      const ownerURL = owners.get(fileId);
      expect(BACKEND_URLS).toContain(ownerURL);

      // Two distinct users, not one user on two sockets: the relay
      // groups a room-user-change roster by userId (one row per user,
      // socketIds listed underneath), so a same-user pair would still
      // report a one-row roster and hide the very thing this asserts.
      const { wopiSrc, tokens } = await seedFile(WOPIHOST_URL, { file: fileId, writers: ["dana", "eve"] });
      const { sessionToken: danaToken } = await launchSession(PROXY_URL, wopiSrc, tokens.dana);
      const { sessionToken: eveToken } = await launchSession(PROXY_URL, wopiSrc, tokens.eve);

      const viaProxy = await connectSocket(PROXY_URL, fileId, danaToken);
      const viaProxyDesignate = withTimeout(waitForEvent(viaProxy, "sync-designate"), `${fileId} via-proxy designate`);
      viaProxy.emit("join-room", fileId);
      await viaProxyDesignate;

      const direct = await connectSocket(ownerURL, fileId, eveToken);
      const roomChange = withTimeout(waitForEvent(viaProxy, "room-user-change"), `${fileId} presence after direct join`);
      direct.emit("join-room", fileId);
      const [presence] = await roomChange;
      expect(presence).toHaveLength(2);
      expect(presence.map((row) => row.userId).sort()).toEqual(["dana", "eve"]);

      viaProxy.close();
      direct.close();
    }
  });
});

function pickDisagreeingPair(owners) {
  const entries = [...owners.entries()];
  for (let i = 0; i < entries.length; i++) {
    for (let j = i + 1; j < entries.length; j++) {
      if (entries[i][1] !== entries[j][1]) {
        return [entries[i][0], entries[j][0]];
      }
    }
  }
  throw new Error("no two candidate room ids hashed to different backends");
}
