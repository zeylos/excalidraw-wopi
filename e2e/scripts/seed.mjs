#!/usr/bin/env node
// Seed the e2e Drive stack with one excalidraw item and prove that Drive
// routes it to our WOPI service.
//
// Steps:
//   a. log in through the e2e auth bypass and capture the session
//   b. create a file item, upload a minimal excalidraw scene to S3 (rustfs),
//      then tell Drive the upload ended
//   c. poll the item's /wopi/ action until Drive returns a launch_url; a
//      launch_url proves Drive ingested our discovery XML and mapped the
//      .excalidraw extension to our service
//
// Plain Node, no dependencies: run with `node e2e/scripts/seed.mjs`.
//
// Flags (both additive, for the Go integration test in e2e/integration):
//   --empty             upload a 0-byte body instead of the default scene,
//                        to exercise the WOPI empty-file PutFile rule
//   --filename <name>   use <name> instead of the default board.excalidraw

const DRIVE_URL = process.env.DRIVE_URL ?? "http://localhost:8000";
const EMAIL = process.env.SEED_EMAIL ?? "wopi-e2e@example.com";

const args = process.argv.slice(2);
const EMPTY = args.includes("--empty");
const filenameFlagIndex = args.indexOf("--filename");
const FILENAME = filenameFlagIndex === -1 ? "board.excalidraw" : args[filenameFlagIndex + 1];

const DEFAULT_SCENE = JSON.stringify({
  type: "excalidraw",
  version: 2,
  elements: [],
  appState: {},
  files: {},
});
const SCENE_BODY = EMPTY ? "" : DEFAULT_SCENE;

const WOPI_POLL_INTERVAL_MS = 1000;
const WOPI_POLL_TIMEOUT_MS = 60_000;

// Django's CSRF check is a double-submit cookie: it only requires the
// X-CSRFToken header to match whatever csrftoken cookie the client holds.
// Drive never has a reason to hand this script a browser page that would
// mint one via {% csrf_token %}, so the script mints its own 64-char
// secret and carries it as both the cookie and the header on every
// session-authenticated write (contract WP note, verified against
// rest_framework.authentication.SessionAuthentication.enforce_csrf).
function makeCsrfToken() {
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789";
  let token = "";
  for (let i = 0; i < 64; i++) {
    token += alphabet[Math.floor(Math.random() * alphabet.length)];
  }
  return token;
}

class CookieJar {
  #cookies = new Map();

  absorb(response) {
    for (const setCookie of response.headers.getSetCookie?.() ?? []) {
      const [pair] = setCookie.split(";");
      const eq = pair.indexOf("=");
      if (eq === -1) continue;
      this.#cookies.set(pair.slice(0, eq).trim(), pair.slice(eq + 1).trim());
    }
  }

  set(name, value) {
    this.#cookies.set(name, value);
  }

  get(name) {
    return this.#cookies.get(name);
  }

  header() {
    return [...this.#cookies.entries()].map(([k, v]) => `${k}=${v}`).join("; ");
  }
}

const jar = new CookieJar();

async function api(method, path, { body, headers = {}, raw = false } = {}) {
  const url = path.startsWith("http") ? path : `${DRIVE_URL}${path}`;
  const csrf = jar.get("csrftoken");

  const finalHeaders = {
    Cookie: jar.header(),
    ...(csrf ? { "X-CSRFToken": csrf, Referer: DRIVE_URL } : {}),
    ...headers,
  };
  if (body !== undefined && !raw) {
    finalHeaders["Content-Type"] = "application/json";
  }

  const response = await fetch(url, {
    method,
    headers: finalHeaders,
    body: body === undefined ? undefined : raw ? body : JSON.stringify(body),
  });
  jar.absorb(response);

  const text = await response.text();
  let data = text;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    // Not JSON (e.g. an S3 error body); keep the raw text.
  }

  if (!response.ok) {
    throw new Error(`${method} ${url} -> ${response.status}: ${text}`);
  }
  return data;
}

async function login() {
  await api("POST", "/api/v1.0/e2e/user-auth/", { body: { email: EMAIL } });
  if (!jar.get("csrftoken")) {
    jar.set("csrftoken", makeCsrfToken());
  }
  console.error(`seed: logged in as ${EMAIL}`);
}

async function createItem() {
  const item = await api("POST", "/api/v1.0/items/", {
    body: { type: "file", filename: FILENAME },
  });
  console.error(`seed: created item ${item.id}`);
  return item;
}

async function uploadScene(item) {
  // The presigned URL signs the x-amz-acl header too (Drive's
  // AWS_S3_UPLOAD_ACL defaults to "private"), so the PUT must send the
  // same header back or rustfs rejects it as an unsigned header.
  const putResponse = await fetch(item.policy, {
    method: "PUT",
    headers: { "Content-Type": "application/json", "x-amz-acl": "private" },
    body: SCENE_BODY,
  });
  if (!putResponse.ok) {
    throw new Error(
      `PUT ${item.policy} -> ${putResponse.status}: ${await putResponse.text()}`,
    );
  }
  console.error("seed: uploaded scene to rustfs");

  await api("POST", `/api/v1.0/items/${item.id}/upload-ended/`, { body: {} });
  console.error("seed: told Drive the upload ended");
}

async function pollWopi(itemId) {
  const deadline = Date.now() + WOPI_POLL_TIMEOUT_MS;
  let lastError;

  while (Date.now() < deadline) {
    try {
      return await api("GET", `/api/v1.0/items/${itemId}/wopi/`);
    } catch (err) {
      lastError = err;
      await new Promise((resolve) => setTimeout(resolve, WOPI_POLL_INTERVAL_MS));
    }
  }
  throw new Error(
    `seed: timed out waiting for /wopi/ to route ${itemId} (last error: ${lastError})`,
  );
}

async function main() {
  await login();
  const item = await createItem();
  await uploadScene(item);

  console.error("seed: polling for Drive's WOPI routing (discovery must be ingested)");
  const wopi = await pollWopi(item.id);

  const result = {
    itemId: item.id,
    launchUrl: wopi.launch_url,
    accessToken: wopi.access_token,
    accessTokenTtl: wopi.access_token_ttl,
    filename: FILENAME,
    // Base64, so a caller can assert on the exact uploaded bytes without
    // guessing this script's scene content.
    sceneBase64: Buffer.from(SCENE_BODY).toString("base64"),
  };
  console.log(JSON.stringify(result, null, 2));
}

main().catch((err) => {
  console.error(err.stack ?? String(err));
  process.exit(1);
});
