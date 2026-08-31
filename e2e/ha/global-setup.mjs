// Vitest globalSetup for the HA harness. It builds bin/excalidraw-wopi
// plus this directory's two helper commands, then spawns, in order: the
// wopihost (e2e/ha/wopihost, outlives every service instance), two plain
// single-replica excalidraw-wopi instances sharing one proof key and one
// session secret, and the hashproxy (e2e/ha/hashproxy) in front of both.
//
// A test file may run in a worker thread that shares no process handles
// with this setup module, so this also starts a tiny control HTTP
// server, in this same process, that lets a test SIGKILL one backend by
// URL for the failover scenario. Every address this module picks is
// written to .run/topology.json for helpers.mjs to read back.
import { spawn, spawnSync } from "node:child_process";
import { existsSync, mkdirSync, writeFileSync } from "node:fs";
import http from "node:http";
import crypto from "node:crypto";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(here, "../..");
const binDir = path.join(here, ".bin");
const runDir = path.join(here, ".run");

const wopihostAddr = process.env.HA_WOPIHOST_ADDR ?? "127.0.0.1:18866";
const appAAddr = process.env.HA_APP_A_ADDR ?? "127.0.0.1:18869";
const appBAddr = process.env.HA_APP_B_ADDR ?? "127.0.0.1:18870";
const proxyAddr = process.env.HA_PROXY_ADDR ?? "127.0.0.1:18868";
const controlAddr = process.env.HA_CONTROL_ADDR ?? "127.0.0.1:18871";

const startTimeoutMs = 30000;

export default async function setup() {
  mkdirSync(binDir, { recursive: true });
  mkdirSync(runDir, { recursive: true });

  buildGoCommand(path.join(binDir, "wopihost"), "./e2e/ha/wopihost");
  buildGoCommand(path.join(binDir, "hashproxy"), "./e2e/ha/hashproxy");
  buildAppBinary();

  // started lists every child spawned so far, in start order. On a
  // failure partway through, the catch block below kills all of them,
  // so a setup error never leaves an orphan process behind.
  const started = [];
  try {
    const wopihost = spawnMarked("wopihost", path.join(binDir, "wopihost"), [], {
      HA_WOPIHOST_ADDR: wopihostAddr,
    }, "ha wopihost listening");
    started.push(wopihost.child);
    await wopihost.ready;

    const sessionSecret = crypto.randomBytes(32).toString("hex");
    const proofKeyPath = path.join(runDir, "proof-key.pem");
    const allowedOrigin = `http://${wopihostAddr}`;
    const proxyURL = `http://${proxyAddr}`;

    // Instance A starts, and this waits for its own /healthz, before
    // instance B starts: proof.Load generates cfg.ProofKeyPath on its
    // first use when the file does not exist yet, so starting the two
    // instances one at a time avoids a concurrent-write race over that
    // one shared file. Instance B then simply loads what A already wrote.
    const appA = spawnApp("appA", appAAddr, { sessionSecret, proofKeyPath, allowedOrigin, publicURL: proxyURL });
    started.push(appA.child);
    await Promise.race([waitForHealthz(`http://${appAAddr}`, startTimeoutMs), appA.failure]);

    const appB = spawnApp("appB", appBAddr, { sessionSecret, proofKeyPath, allowedOrigin, publicURL: proxyURL });
    started.push(appB.child);
    await Promise.race([waitForHealthz(`http://${appBAddr}`, startTimeoutMs), appB.failure]);

    const backendURLs = [`http://${appAAddr}`, `http://${appBAddr}`];

    const hashproxy = spawnMarked("hashproxy", path.join(binDir, "hashproxy"), [], {
      HA_PROXY_ADDR: proxyAddr,
      HA_PROXY_BACKENDS: backendURLs.join(","),
    }, "ha hashproxy listening");
    started.push(hashproxy.child);
    await hashproxy.ready;

    const controlServer = await startControlServer(controlAddr, {
      [backendURLs[0]]: appA.child,
      [backendURLs[1]]: appB.child,
    });

    writeFileSync(
      path.join(runDir, "topology.json"),
      JSON.stringify(
        {
          wopihostURL: `http://${wopihostAddr}`,
          proxyURL,
          backendURLs,
          controlURL: `http://${controlAddr}`,
        },
        null,
        2,
      ),
    );

    return async function teardown() {
      await new Promise((resolve) => controlServer.close(resolve));
      await killChild(hashproxy.child);
      await killChild(appB.child);
      await killChild(appA.child);
      await killChild(wopihost.child);
    };
  } catch (err) {
    for (const child of started.reverse()) {
      await killChild(child);
    }
    throw err;
  }
}

// buildAppBinary builds bin/excalidraw-wopi. It reuses `make build`
// (frontend build plus the Go build) when web/dist has not been built
// yet, and falls back to a plain `go build` when it has: a repeat local
// run does not need to rebuild the frontend on every iteration, but a
// clean checkout, as CI always is, does.
function buildAppBinary() {
  if (existsSync(path.join(repoRoot, "web", "dist", "index.html"))) {
    buildGoCommand(path.join(repoRoot, "bin", "excalidraw-wopi"), "./cmd/excalidraw-wopi");
    return;
  }
  const build = spawnSync("make", ["build"], { cwd: repoRoot, stdio: "inherit" });
  if (build.status !== 0) {
    throw new Error(`make build failed (exit ${build.status})`);
  }
}

function buildGoCommand(outPath, pkg) {
  const build = spawnSync("go", ["build", "-o", outPath, pkg], { cwd: repoRoot, stdio: "inherit" });
  if (build.status !== 0) {
    throw new Error(`go build -o ${outPath} ${pkg} failed (exit ${build.status})`);
  }
}

// cleanExcalidrawEnv strips every EXCALIDRAW_WOPI_* variable from a copy
// of the current environment, so a variable set in the outer shell (a
// developer's own EXCALIDRAW_WOPI_FAKE_HOST=1, say) cannot leak into an
// HA instance, which must run in plain single-replica mode with no fake
// host.
function cleanExcalidrawEnv() {
  const env = { ...process.env };
  for (const key of Object.keys(env)) {
    if (key.startsWith("EXCALIDRAW_WOPI_")) {
      delete env[key];
    }
  }
  return env;
}

function spawnApp(label, addr, { sessionSecret, proofKeyPath, allowedOrigin, publicURL }) {
  const env = cleanExcalidrawEnv();
  Object.assign(env, {
    EXCALIDRAW_WOPI_LISTEN_ADDR: addr,
    EXCALIDRAW_WOPI_PUBLIC_URL: publicURL,
    EXCALIDRAW_WOPI_SESSION_SECRET: sessionSecret,
    EXCALIDRAW_WOPI_PROOF_KEY_PATH: proofKeyPath,
    EXCALIDRAW_WOPI_WOPI_ALLOWED_ORIGINS: allowedOrigin,
  });
  return spawnLabeled(label, path.join(repoRoot, "bin", "excalidraw-wopi"), [], env);
}

// spawnLabeled starts command and returns its child plus a failure
// promise that rejects on an early exit or a spawn error, naming the
// exit code. A caller races failure against its own readiness wait, so
// a boot failure reports why instead of waiting out a timeout.
function spawnLabeled(label, command, args, env) {
  const child = spawn(command, args, { cwd: repoRoot, env, stdio: ["ignore", "pipe", "pipe"] });
  child.stdout.on("data", (chunk) => process.stdout.write(`[${label}] ${chunk}`));
  child.stderr.on("data", (chunk) => process.stderr.write(`[${label}] ${chunk}`));

  const failure = new Promise((_, reject) => {
    child.once("exit", (code, signal) => {
      reject(new Error(`${label} exited early (code ${code}, signal ${signal}) before it reported ready`));
    });
    child.once("error", (err) => {
      reject(new Error(`${label} failed to start: ${err.message}`));
    });
  });
  // A clean exit after the caller already stopped racing failure must
  // not surface as an unhandled rejection.
  failure.catch(() => {});

  return { child, failure };
}

// spawnMarked starts command and resolves `ready` once its stdout carries
// marker, the same wait-for-stdout-line pattern e2e/interop/global-setup.mjs
// uses for its own relay process.
function spawnMarked(label, command, args, envExtra, marker) {
  const child = spawn(command, args, {
    cwd: repoRoot,
    env: { ...process.env, ...envExtra },
    stdio: ["ignore", "pipe", "pipe"],
  });

  const ready = new Promise((resolve, reject) => {
    let buffered = "";
    let settled = false;

    const onStdout = (chunk) => {
      process.stdout.write(`[${label}] ${chunk}`);
      buffered += chunk.toString();
      if (!settled && buffered.includes(marker)) {
        settled = true;
        clearTimeout(timer);
        child.stdout.off("data", onStdout);
        resolve();
      }
    };
    const onExit = (code) => {
      if (!settled) {
        settled = true;
        clearTimeout(timer);
        reject(new Error(`${label} exited early (code ${code}) before it reported ready`));
      }
    };
    const onError = (err) => {
      if (!settled) {
        settled = true;
        clearTimeout(timer);
        reject(new Error(`${label} failed to start: ${err.message}`));
      }
    };
    const timer = setTimeout(() => {
      if (!settled) {
        settled = true;
        child.kill();
        reject(new Error(`${label} did not report ready within ${startTimeoutMs}ms`));
      }
    }, startTimeoutMs);

    child.stdout.on("data", onStdout);
    child.stderr.on("data", (chunk) => process.stderr.write(`[${label}] ${chunk}`));
    child.once("exit", onExit);
    child.once("error", onError);
  });

  return { child, ready };
}

async function waitForHealthz(baseURL, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  let lastErr;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(`${baseURL}/healthz`);
      if (res.ok) {
        return;
      }
    } catch (err) {
      lastErr = err;
    }
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error(`${baseURL}/healthz did not answer within ${timeoutMs}ms: ${lastErr}`);
}

async function killChild(child) {
  if (!child || child.exitCode !== null || child.signalCode !== null) {
    return;
  }
  child.kill("SIGTERM");
  await new Promise((resolve) => child.once("exit", resolve));
}

// startControlServer answers a test's request to kill or check one
// backend by its URL, the key both /__owner (hashproxy) and
// topology.json's backendURLs use, so a test never needs a raw process
// handle of its own. It resolves once the server is actually listening,
// or rejects with the listen error (an EADDRINUSE names the port),
// instead of returning before either is known.
function startControlServer(addr, backendChildren) {
  const [host, port] = addr.split(":");
  const server = http.createServer((req, res) => {
    const url = new URL(req.url, `http://${addr}`);

    if (url.pathname === "/kill" && req.method === "POST") {
      const child = backendChildren[url.searchParams.get("backend")];
      if (!child) {
        writeJSON(res, 404, { error: "unknown backend" });
        return;
      }
      const wasAlive = child.exitCode === null && child.signalCode === null;
      if (wasAlive) {
        child.kill("SIGKILL");
      }
      writeJSON(res, 200, { killed: wasAlive, pid: child.pid });
      return;
    }

    if (url.pathname === "/alive" && req.method === "GET") {
      const child = backendChildren[url.searchParams.get("backend")];
      const alive = !!child && child.exitCode === null && child.signalCode === null;
      writeJSON(res, 200, { alive });
      return;
    }

    res.writeHead(404).end();
  });

  return new Promise((resolve, reject) => {
    server.once("error", (err) => {
      reject(new Error(`control server: listen on ${addr}: ${err.message}`));
    });
    server.once("listening", () => resolve(server));
    server.listen(Number(port), host);
  });
}

function writeJSON(res, status, body) {
  res.writeHead(status, { "Content-Type": "application/json" });
  res.end(JSON.stringify(body));
}
