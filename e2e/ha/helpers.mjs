// Shared helpers for the e2e/ha test files: reads the topology
// global-setup.mjs wrote, drives the launch/board/socket.io client paths
// a real browser would use, and talks to the wopihost's and hashproxy's
// dev-only introspection routes. Kept self-contained (no import from
// e2e/interop) so `make e2e-ha` needs no other suite's node_modules.
import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { io } from "socket.io-client";

const here = path.dirname(fileURLToPath(import.meta.url));
const topology = JSON.parse(readFileSync(path.join(here, ".run", "topology.json"), "utf8"));

export const WOPIHOST_URL = topology.wopihostURL;
export const PROXY_URL = topology.proxyURL;
export const BACKEND_URLS = topology.backendURLs;
export const CONTROL_URL = topology.controlURL;

const EVENT_TIMEOUT_MS = 5000;

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

// connectSocket opens a socket.io connection to baseURL with room set as
// the "room" query parameter, mirroring web/src/utils/roomParam.ts: the
// hashproxy and, in production, internal/peers' Middleware both route on
// that parameter. It retries a few times with a fresh socket: a busy
// host can stall or drop a brand-new WebSocket handshake.
export async function connectSocket(baseURL, room, token, attempts = 5) {
  let lastErr;
  for (let i = 0; i < attempts; i++) {
    const socket = io(baseURL, {
      auth: { token },
      query: { room },
      transports: ["websocket"],
      forceNew: true,
      reconnection: false,
    });
    try {
      await withTimeout(waitForEvent(socket, "connect"), `connect to ${baseURL} (attempt ${i + 1}/${attempts})`);
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
// save landing, a lock changing, or a proxy reroute goes through this,
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

// seedFile calls the wopihost's POST /seed: it registers a fresh file
// (a random id when file is omitted) plus one User and access token per
// name in writers/readers.
export async function seedFile(wopihostURL, { file, writers = [], readers = [] } = {}) {
  const params = new URLSearchParams();
  if (file) params.set("file", file);
  if (writers.length) params.set("writers", writers.join(","));
  if (readers.length) params.set("readers", readers.join(","));

  const res = await fetch(`${wopihostURL}/seed?${params}`, { method: "POST" });
  if (!res.ok) {
    throw new Error(`seed failed: ${res.status} ${await res.text()}`);
  }
  return res.json(); // { fileId, wopiSrc, tokens }
}

// getState calls the wopihost's GET /state: fileId's stored bytes
// (decoded to text too, for a direct scene-body assertion), version,
// save count, and current lock.
export async function getState(wopihostURL, fileId) {
  const res = await fetch(`${wopihostURL}/state?${new URLSearchParams({ file: fileId })}`);
  if (res.status === 404) {
    return null;
  }
  if (!res.ok) {
    throw new Error(`state failed: ${res.status} ${await res.text()}`);
  }
  const state = await res.json();
  return { ...state, contentText: Buffer.from(state.content, "base64").toString("utf8") };
}

// getOwner calls the hashproxy's GET /__owner: the backend URL its
// current health snapshot picks for room.
export async function getOwner(proxyURL, room) {
  const res = await fetch(`${proxyURL}/__owner?${new URLSearchParams({ room })}`);
  if (!res.ok) {
    throw new Error(`__owner failed: ${res.status} ${await res.text()}`);
  }
  const { owner } = await res.json();
  return owner;
}

// killBackend asks global-setup's control server to SIGKILL the instance
// at backendURL: a test file may run in a worker thread that shares no
// process handle with the setup module, so this is the only portable way
// to kill a specific instance from a test.
export async function killBackend(controlURL, backendURL) {
  const res = await fetch(`${controlURL}/kill?${new URLSearchParams({ backend: backendURL })}`, { method: "POST" });
  if (!res.ok) {
    throw new Error(`kill failed: ${res.status} ${await res.text()}`);
  }
  return res.json();
}

export async function isAlive(controlURL, backendURL) {
  const res = await fetch(`${controlURL}/alive?${new URLSearchParams({ backend: backendURL })}`);
  const { alive } = await res.json();
  return alive;
}

// launchSession drives POST /launch exactly as a WOPI host's auto-submitted
// form POST does, then reads the sessionToken and the rest of
// web/src/config.ts's AppConfig back out of the returned HTML's injected
// #ew-config script tag (internal/launch/launch.go's
// configScriptOpen/configScriptClose).
export async function launchSession(proxyURL, wopiSrc, accessToken, ttlMs = 60 * 60 * 1000) {
  const url = `${proxyURL}/launch?${new URLSearchParams({ WOPISrc: wopiSrc })}`;
  const body = new URLSearchParams({
    access_token: accessToken,
    access_token_ttl: String(Date.now() + ttlMs),
  });

  const res = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: body.toString(),
  });
  if (!res.ok) {
    throw new Error(`launch failed: ${res.status} ${await res.text()}`);
  }
  return extractConfig(await res.text());
}

// putBoard calls PUT /api/board with room set the same way a real save
// would, room the hashproxy routes on. It throws unless the save is
// accepted (204): internal/room.Manager takes it from there, flushing it
// to the WOPI host on its own save cadence.
export async function putBoard(proxyURL, apiBase, fileId, sessionToken, scene) {
  const res = await fetch(`${proxyURL}${apiBase}/board?${new URLSearchParams({ room: fileId })}`, {
    method: "PUT",
    headers: { Authorization: `Bearer ${sessionToken}`, "Content-Type": "application/json" },
    body: scene,
  });
  if (res.status !== 204) {
    throw new Error(`PUT /api/board failed: ${res.status} ${await res.text()}`);
  }
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
