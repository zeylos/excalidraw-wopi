// Shared helpers for the e2e/nightly suite: drives the real Drive launch
// flow through DriveClient (e2e/playwright/fixtures/drive.ts), then the
// board REST API and the socket.io relay a real browser would use. Adapted
// from e2e/ha/helpers.mjs, minus that suite's topology.json (this suite
// runs against one binary and one Drive, not a hashproxy'd fleet).
import { io } from "socket.io-client";
import { DriveClient } from "../playwright/fixtures/drive.ts";

export { DriveClient };

export const EXCALIDRAW_URL = process.env.EXCALIDRAW_URL ?? "http://localhost:8080";

const EVENT_TIMEOUT_MS = 5000;
const FETCH_TIMEOUT_MS = 30000;

const EMPTY_SCENE = JSON.stringify({ type: "excalidraw", version: 2, elements: [], appState: {}, files: {} });

export function withTimeout(promise, label, ms = EVENT_TIMEOUT_MS) {
  let timer;
  const timeout = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error(`timed out waiting for ${label}`)), ms);
  });
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer));
}

export function waitForEvent(socket, event) {
  return new Promise((resolve) => {
    const handler = (...args) => {
      clearTimeout(timer);
      resolve(args);
    };
    socket.once(event, handler);
    const timer = setTimeout(() => socket.off(event, handler), EVENT_TIMEOUT_MS);
  });
}

// connectSocket opens a socket.io connection for session (a launchSession
// result), room set as the "room" query parameter. It retries a few times
// with a fresh socket: a busy host can stall or drop a brand-new WebSocket
// handshake. The session JWT carries no server-side session table (see
// internal/session), so calling this more than once with the same session
// is a legitimate reconnect, not a special case.
export async function connectSocket(session, attempts = 5) {
  let lastErr;
  for (let i = 0; i < attempts; i++) {
    const socket = io(EXCALIDRAW_URL, {
      auth: { token: session.sessionToken },
      query: { room: session.fileId },
      transports: ["websocket"],
      forceNew: true,
      reconnection: false,
    });
    try {
      await withTimeout(waitForEvent(socket, "connect"), `connect (attempt ${i + 1}/${attempts})`);
      return socket;
    } catch (err) {
      lastErr = err;
      socket.close();
      await new Promise((r) => setTimeout(r, 200));
    }
  }
  throw lastErr;
}

// pollUntil calls fn on intervalMs until it returns a truthy value, or
// throws once timeoutMs has elapsed. Every wait in this suite for a
// syncer election, a roster change, or a save landing goes through this,
// never a fixed sleep.
export async function pollUntil(fn, { timeoutMs = 10000, intervalMs = 200, label = "condition" } = {}) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const result = await fn();
    if (result) {
      return result;
    }
    if (Date.now() >= deadline) {
      throw new Error(`timed out waiting for ${label}`);
    }
    await new Promise((r) => setTimeout(r, intervalMs));
  }
}

// putBoard calls PUT /api/board for session, the same save a real client's
// throttled sync would. It throws unless the save is accepted (204):
// internal/room.Manager takes it from there, flushing it to Drive on its
// own save cadence.
export async function putBoard(session, scene) {
  const res = await fetch(`${EXCALIDRAW_URL}${session.apiBase}/board?${new URLSearchParams({ room: session.fileId })}`, {
    method: "PUT",
    headers: { Authorization: `Bearer ${session.sessionToken}`, "Content-Type": "application/json" },
    body: scene,
    signal: AbortSignal.timeout(FETCH_TIMEOUT_MS),
  });
  if (res.status !== 204) {
    throw new Error(`PUT /api/board failed: ${res.status} ${await res.text()}`);
  }
}

// getBoard calls GET /api/board for session and returns the raw body text,
// so a caller can compare it against a scene string putBoard sent.
export async function getBoard(session) {
  const res = await fetch(`${EXCALIDRAW_URL}${session.apiBase}/board?${new URLSearchParams({ room: session.fileId })}`, {
    headers: { Authorization: `Bearer ${session.sessionToken}` },
    signal: AbortSignal.timeout(FETCH_TIMEOUT_MS),
  });
  if (!res.ok) {
    throw new Error(`GET /api/board failed: ${res.status} ${await res.text()}`);
  }
  return res.text();
}

const configOpenTag = '<script type="application/json" id="ew-config">';
const configCloseTag = "</script>";

function extractConfig(html) {
  const start = html.indexOf(configOpenTag);
  if (start === -1) {
    throw new Error("launch response is missing the ew-config script tag");
  }
  const jsonStart = start + configOpenTag.length;
  const end = html.indexOf(configCloseTag, jsonStart);
  if (end === -1) {
    throw new Error("launch response is missing the ew-config closing tag");
  }
  return JSON.parse(html.slice(jsonStart, end));
}

// launchSession drives POST /launch exactly as a WOPI host's auto-submitted
// form POST does (WopiEditorFrame.tsx's iframe form, mirrored by
// e2e/playwright/fixtures/launch.ts's openEditor), then reads the
// sessionToken and the rest of web/src/config.ts's AppConfig back out of
// the returned HTML's injected #ew-config script tag
// (internal/launch/launch.go's configScriptOpen/configScriptClose).
export async function launchSession(wopi) {
  const body = new URLSearchParams({
    access_token: wopi.accessToken,
    access_token_ttl: String(wopi.accessTokenTtl),
  });

  const res = await fetch(wopi.launchUrl, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: body.toString(),
    signal: AbortSignal.timeout(FETCH_TIMEOUT_MS),
  });
  if (!res.ok) {
    throw new Error(`launch failed: ${res.status} ${await res.text()}`);
  }
  return extractConfig(await res.text());
}

// setupUsers logs in n Drive users, has the first (the owner) create one
// fresh item and upload an initial empty scene, grants every other user
// the editor role, and mints one launch session per user against that
// item. It mirrors the recipe e2e/playwright/specs/smoke/smoke.spec.ts's
// beforeAll already proved against the real Drive stack.
export async function setupUsers(n, { filename } = {}) {
  const stamp = Date.now();
  const drives = [];
  for (let i = 0; i < n; i++) {
    const drive = new DriveClient();
    await drive.login(`nightly-user${i}-${stamp}@example.com`);
    drives.push(drive);
  }

  const owner = drives[0];
  const item = await owner.createItem(filename ?? `nightly-${stamp}.excalidraw`);
  await owner.uploadScene(item, EMPTY_SCENE);
  // The first wopi/ poll proves Drive routed .excalidraw to us (discovery
  // ingested), the same gate e2e-seed and the smoke suite depend on.
  await owner.wopiLaunch(item.id);

  for (const drive of drives.slice(1)) {
    const { id: userId } = await drive.me();
    await owner.shareEditor(item.id, userId);
  }

  const users = [];
  for (const drive of drives) {
    const wopi = await drive.wopiLaunch(item.id);
    const session = await launchSession(wopi);
    users.push({ drive, session });
  }

  return { item, owner, users };
}
