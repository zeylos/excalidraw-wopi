// Shared socket.io-client event-promise helpers for relay.test.mjs. Kept
// close to the pattern proven informally in internal/relay/smoke/smoke.mjs.
import { io } from "socket.io-client";

export const ADDR = process.env.INTEROP_ADDR ?? "127.0.0.1:18765";
export const SERVER_URL = `http://${ADDR}`;
export const ROOM = "interop-room";
const TIMEOUT_MS = 5000;

export function withTimeout(promise, label) {
  let timer;
  const timeout = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error(`timed out waiting for ${label}`)), TIMEOUT_MS);
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
    // Nobody awaits this promise once withTimeout's own race has settled,
    // so drop the listener here too, or a socket that never fires `event`
    // keeps it registered forever.
    const timer = setTimeout(() => socket.off(event, handler), TIMEOUT_MS);
  });
}

// waitForNoEvent proves a negative (a drop): it rejects if event fires
// within graceMs, and otherwise resolves once graceMs has elapsed.
export function waitForNoEvent(socket, event, graceMs) {
  return new Promise((resolve, reject) => {
    const handler = (...args) => {
      clearTimeout(timer);
      reject(new Error(`unexpected ${event} received: ${JSON.stringify(args)}`));
    };
    socket.once(event, handler);
    const timer = setTimeout(() => {
      socket.off(event, handler);
      resolve();
    }, graceMs);
  });
}

export function decodeJSON(buf) {
  return JSON.parse(Buffer.from(buf).toString("utf8"));
}

// connectReliable opens a socket for token and waits for "connect",
// retrying with a fresh socket a few times on failure. A busy CI host can
// stall or drop a brand-new WebSocket handshake; a real client hitting
// that would retry too, so this keeps the harness's signal on the relay's
// own behavior rather than on host scheduling jitter. It returns the
// connected socket plus a promise for the init-room event, captured before
// "connect" resolves so a caller that wants to assert on init-room does not
// race the server's post-connect emit.
export async function connectReliable(token, attempts = 5) {
  let lastErr;
  for (let i = 0; i < attempts; i++) {
    const socket = io(SERVER_URL, { auth: { token }, transports: ["websocket"], forceNew: true, reconnection: false });
    const initRoom = withTimeout(waitForEvent(socket, "init-room"), `${token} init-room`);
    // A caller that does not care about init-room (most callers past the
    // first connection in a test file) never awaits this promise; give it
    // a no-op handler so a later timeout cannot surface as an unhandled
    // rejection and kill the vitest worker.
    initRoom.catch(() => {});
    try {
      await withTimeout(waitForEvent(socket, "connect"), `${token} connect (attempt ${i + 1}/${attempts})`);
      return { socket, initRoom };
    } catch (err) {
      lastErr = err;
      socket.close();
      await new Promise((r) => setTimeout(r, 200));
    }
  }
  throw lastErr;
}
