// A Go-relay <-> socket.io-client v4 interop harness: structured, assertive
// vitest cases against a real relay process (e2e/interop/server), driven
// over a real WebSocket transport. The cases cover handshake auth, room
// join and presence, syncer designation and failover, server-broadcast and
// server-volatile-broadcast relaying (byte-identical payloads, read-only
// drops, forged-identity rewrites), the image-relay handshake, and the
// oversize-payload rejection path. Tests run in file order
// (vitest.config.mjs disables parallelism): the room and syncer state
// built by an earlier test is the starting state the next one asserts
// against, one shared room across the whole file.
import { afterAll, describe, expect, it } from "vitest";
import { io } from "socket.io-client";
import crypto from "node:crypto";
import { ROOM, SERVER_URL, connectReliable, decodeJSON, waitForEvent, waitForNoEvent, withTimeout } from "./helpers.mjs";

describe("relay interop", () => {
  let writerA;
  let writerB;
  let reader;

  afterAll(() => {
    writerA?.close();
    writerB?.close();
    reader?.close();
  });

  it("a: a bad token fails the handshake with exactly 'Authentication error'", async () => {
    const socket = io(SERVER_URL, { auth: { token: "not-a-real-token" }, transports: ["websocket"], forceNew: true, reconnection: false });
    try {
      const [err] = await withTimeout(waitForEvent(socket, "connect_error"), "connect_error");
      expect(err.message).toBe("Authentication error");
    } finally {
      socket.close();
    }
  });

  it("b: connect, init-room, and the first writer's join-room happy path", async () => {
    const conn = await connectReliable("writer-a");
    writerA = conn.socket;
    await conn.initRoom;

    const roomUserChange = withTimeout(waitForEvent(writerA, "room-user-change"), "room-user-change");
    const userJoined = withTimeout(waitForEvent(writerA, "user-joined"), "user-joined");
    const syncDesignate = withTimeout(waitForEvent(writerA, "sync-designate"), "sync-designate");
    writerA.emit("join-room", ROOM);

    const [designate] = await syncDesignate;
    expect(designate).toEqual({ isSyncer: true });

    const [presence] = await roomUserChange;
    expect(presence).toEqual([
      { socketId: writerA.id, user: { id: "writer-a", name: "Writer A" }, userId: "writer-a", socketIds: [writerA.id] },
    ]);

    const [joined] = await userJoined;
    expect(joined).toMatchObject({ userId: "writer-a", socketId: writerA.id, isSyncer: true });
  });

  it("c: joining with a session whose fileId claim does not match the room is rejected", async () => {
    const conn = await connectReliable("other-room-writer");
    const socket = conn.socket;
    try {
      const err = withTimeout(waitForEvent(socket, "error"), "error");
      const noJoinBroadcast = waitForNoEvent(writerA, "room-user-change", 300);
      socket.emit("join-room", ROOM);

      const [message] = await err;
      expect(typeof message).toBe("string");
      await noJoinBroadcast;
    } finally {
      socket.close();
    }
  });

  it("setup: writer-b and reader join the same room", async () => {
    const connB = await connectReliable("writer-b");
    writerB = connB.socket;
    const bDesignate = withTimeout(waitForEvent(writerB, "sync-designate"), "writer-b sync-designate");
    const bRoomChange = withTimeout(waitForEvent(writerA, "room-user-change"), "room-user-change after writer-b");
    writerB.emit("join-room", ROOM);

    const [bFirst] = await bDesignate;
    expect(bFirst).toEqual({ isSyncer: false });

    const [presenceAfterB] = await bRoomChange;
    expect(presenceAfterB).toHaveLength(2);
    expect(presenceAfterB.map((row) => row.userId).sort()).toEqual(["writer-a", "writer-b"]);
    for (const row of presenceAfterB) {
      expect(row).toEqual({
        socketId: expect.any(String),
        user: { id: row.userId, name: expect.any(String) },
        userId: row.userId,
        socketIds: [row.socketId],
      });
    }

    const connR = await connectReliable("reader");
    reader = connR.socket;
    const readerDesignate = withTimeout(waitForEvent(reader, "sync-designate"), "reader sync-designate");
    reader.emit("join-room", ROOM);

    const [readerFirst] = await readerDesignate;
    expect(readerFirst).toEqual({ isSyncer: false });
  });

  it("bonus: a second socket for the same user dedupes into one roster row", async () => {
    const conn = await connectReliable("writer-a");
    const second = conn.socket;
    try {
      const roomUserChange = withTimeout(waitForEvent(writerB, "room-user-change"), "room-user-change (dedup join)");
      second.emit("join-room", ROOM);

      const [presence] = await roomUserChange;
      expect(presence).toHaveLength(3); // writer-a (2 sockets, 1 row), writer-b, reader
      const row = presence.find((entry) => entry.userId === "writer-a");
      expect([...row.socketIds].sort()).toEqual([writerA.id, second.id].sort());

      const leftChange = withTimeout(waitForEvent(writerB, "room-user-change"), "room-user-change (dedup leave)");
      second.close();
      const [afterLeave] = await leftChange;
      expect(afterLeave.find((entry) => entry.userId === "writer-a").socketIds).toEqual([writerA.id]);
    } finally {
      second.close();
    }
  });

  it("d: server-broadcast relays a 64 KiB payload byte-identical and excludes the sender", async () => {
    const payload = crypto.randomBytes(64 * 1024);
    const recv = withTimeout(waitForEvent(writerB, "client-broadcast"), "writer-b client-broadcast");
    const senderSilence = waitForNoEvent(writerA, "client-broadcast", 500);

    writerA.emit("server-broadcast", ROOM, payload, []);

    const [receivedPayload, receivedIv] = await recv;
    expect(Buffer.from(receivedPayload).equals(payload)).toBe(true);
    expect(receivedIv).toEqual([]);
    await senderSilence;
  });

  it("e: a read-only server-broadcast is dropped, delivered to no writer", async () => {
    const silenceA = waitForNoEvent(writerA, "client-broadcast", 500);
    const silenceB = waitForNoEvent(writerB, "client-broadcast", 500);

    reader.emit("server-broadcast", ROOM, Buffer.from("should-not-arrive"), []);

    await Promise.all([silenceA, silenceB]);
  });

  it("f: server-volatile-broadcast rewrites a forged MOUSE_LOCATION identity and drops a disallowed type", async () => {
    const forgedMouse = Buffer.from(JSON.stringify({
      type: "MOUSE_LOCATION",
      payload: { pointer: { x: 1, y: 2 }, user: { id: "attacker", name: "Evil" } },
    }));
    const mouseRecv = withTimeout(waitForEvent(writerB, "client-broadcast"), "writer-b client-broadcast (mouse)");
    writerA.emit("server-volatile-broadcast", ROOM, forgedMouse);

    const [mouseRaw] = await mouseRecv;
    const mouseEvent = decodeJSON(mouseRaw);
    expect(mouseEvent.type).toBe("MOUSE_LOCATION");
    expect(mouseEvent.payload.user).toEqual({ id: "writer-a", name: "Writer A" });

    const sceneUpdateSilence = waitForNoEvent(writerB, "client-broadcast", 500);
    writerA.emit("server-volatile-broadcast", ROOM, Buffer.from(JSON.stringify({
      type: "SCENE_UPDATE",
      payload: { elements: [] },
    })));
    await sceneUpdateSilence;
  });

  it("g: server-volatile-broadcast overwrites a forged VIEWPORT_UPDATE userId", async () => {
    const recv = withTimeout(waitForEvent(writerB, "client-broadcast"), "writer-b client-broadcast (viewport)");
    writerA.emit("server-volatile-broadcast", ROOM, Buffer.from(JSON.stringify({
      type: "VIEWPORT_UPDATE",
      payload: { userId: "attacker", scrollX: 1, scrollY: 2 },
    })));

    const [raw] = await recv;
    const event = decodeJSON(raw);
    expect(event.type).toBe("VIEWPORT_UPDATE");
    expect(event.payload.userId).toBe("writer-a");
    expect(event.payload.scrollX).toBe(1);
  });

  it("h: a read-only session's MOUSE_LOCATION still passes through with the server identity", async () => {
    const recv = withTimeout(waitForEvent(writerA, "client-broadcast"), "writer-a client-broadcast (reader mouse)");
    // writer-b receives the same broadcast; await it here, or on a slow
    // host it lands during test i and cascades into test j.
    const recvB = withTimeout(waitForEvent(writerB, "client-broadcast"), "writer-b client-broadcast (reader mouse)");
    reader.emit("server-volatile-broadcast", ROOM, Buffer.from(JSON.stringify({
      type: "MOUSE_LOCATION",
      payload: { pointer: { x: 9, y: 9 }, user: { id: "forged", name: "Forged" } },
    })));

    const [raw] = await recv;
    const event = decodeJSON(raw);
    expect(event.payload.user).toEqual({ id: "reader-1", name: "Reader" });
    await recvB;
  });

  it("i: image-get produces an IMAGE_REQUEST client-broadcast at the peer", async () => {
    const recv = withTimeout(waitForEvent(writerB, "client-broadcast"), "writer-b client-broadcast (image-get)");
    writerA.emit("image-get", ROOM, "file-abc123");

    const [raw] = await recv;
    const event = decodeJSON(raw);
    expect(event).toEqual({ type: "IMAGE_REQUEST", payload: { fileId: "file-abc123" } });
  });

  it("j: an oversize server-broadcast payload is dropped and the sender gets an error emit", async () => {
    // The interop server configures a 1 MiB scene limit (see
    // e2e/interop/server/main.go); this payload exceeds it.
    const big = crypto.randomBytes(1.5 * 1024 * 1024);
    const err = withTimeout(waitForEvent(writerA, "error"), "error (oversize)");
    const silence = waitForNoEvent(writerB, "client-broadcast", 500);

    writerA.emit("server-broadcast", ROOM, big, []);

    const [message] = await err;
    expect(typeof message).toBe("string");
    await silence;
  });

  it("k: syncer failover promotes writer-b once writer-a disconnects", async () => {
    const failover = withTimeout(waitForEvent(writerB, "sync-designate"), "writer-b sync-designate (failover)");
    writerA.close();

    const [payload] = await failover;
    expect(payload).toEqual({ isSyncer: true });
  });
});
