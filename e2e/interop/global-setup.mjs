// Vitest globalSetup: builds the interop relay server once (server/main.go)
// and starts it as a background process before any test file runs, then
// tears it down after the whole run finishes. Building once here, instead
// of `go run` per test file, keeps a single vitest run's startup cost to
// one compile.
import { spawn, spawnSync } from "node:child_process";
import { mkdirSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(here, "../..");
const binDir = path.join(here, ".bin");
const binPath = path.join(binDir, "interop-server");
const addr = process.env.INTEROP_ADDR ?? "127.0.0.1:18765";
const readyMarker = "interop server listening";
const startTimeoutMs = 20000;

export default async function setup() {
  mkdirSync(binDir, { recursive: true });

  const build = spawnSync("go", ["build", "-o", binPath, "./e2e/interop/server"], {
    cwd: repoRoot,
    stdio: "inherit",
  });
  if (build.status !== 0) {
    throw new Error(`go build ./e2e/interop/server failed (exit ${build.status})`);
  }

  const child = spawn(binPath, [], {
    cwd: repoRoot,
    env: { ...process.env, INTEROP_ADDR: addr },
    stdio: ["ignore", "pipe", "pipe"],
  });

  await new Promise((resolve, reject) => {
    let buffered = "";
    let settled = false;

    const onStdout = (chunk) => {
      process.stdout.write(chunk);
      buffered += chunk.toString();
      if (!settled && buffered.includes(readyMarker)) {
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
        reject(new Error(`interop server exited early (code ${code}) before it reported ready`));
      }
    };
    const timer = setTimeout(() => {
      if (!settled) {
        settled = true;
        child.kill();
        reject(new Error(`interop server did not report ready within ${startTimeoutMs}ms`));
      }
    }, startTimeoutMs);

    child.stdout.on("data", onStdout);
    child.stderr.on("data", (chunk) => process.stderr.write(chunk));
    child.once("exit", onExit);
  });

  return async function teardown() {
    if (child.exitCode !== null || child.signalCode !== null) {
      return;
    }
    child.kill("SIGTERM");
    await new Promise((resolve) => child.once("exit", resolve));
  };
}
