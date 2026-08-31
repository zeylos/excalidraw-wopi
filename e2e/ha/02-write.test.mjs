// Proves a board write made through the hashproxy reaches the wopihost:
// internal/room.Manager's background loop flushes a fresh room's very
// first save almost immediately (see internal/room/manager.go's
// saveDueLocked), so this needs no long wait for the normal save cadence.
import { describe, expect, it } from "vitest";
import { PROXY_URL, WOPIHOST_URL, getState, launchSession, pollUntil, putBoard, seedFile } from "./helpers.mjs";

describe("board write reaches the wopihost", () => {
  it("a PUT /api/board save lands in the WOPI host's stored file", async () => {
    const { fileId, wopiSrc, tokens } = await seedFile(WOPIHOST_URL, { file: "ha-write", writers: ["carol"] });
    const { sessionToken, apiBase } = await launchSession(PROXY_URL, wopiSrc, tokens.carol);

    const scene = JSON.stringify({ elements: [{ id: "rect-1", type: "rectangle" }], marker: "ha-write" });
    await putBoard(PROXY_URL, apiBase, fileId, sessionToken, scene);

    const state = await pollUntil(
      async () => {
        const s = await getState(WOPIHOST_URL, fileId);
        return s && s.putCount >= 1 ? s : null;
      },
      { timeoutMs: 15000, intervalMs: 300, label: "the first save to land in the wopihost" },
    );

    expect(state.contentText).toBe(scene);
  });
});
