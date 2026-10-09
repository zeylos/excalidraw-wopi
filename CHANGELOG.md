# Changelog

All notable changes to this project are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [1.1.2] - 2026-10-09

### Changed

- The Go toolchain moved to 1.27, and the `golang` build image to
  `1.27-trixie`. The `node`, `golang`, and distroless image digests
  moved to the current multi-arch indexes.
- The GitHub Actions pins moved to setup-node v7.1.0, upload-artifact
  v7.0.2, setup-buildx v4.4.1, and setup-qemu v4.4.0. The release
  workflow uses goreleaser v2.18.3 and helm v4.3.0. The lint target
  uses golangci-lint v2.14.0.
- The web build moved to vite 8, `@vitejs/plugin-react` 6, and
  vitest 5. The e2e suites moved to vitest 5 and Playwright 1.64.0.
  The `esbuild` key left `allowScripts`, because vite 8 no longer
  installs esbuild.
- The e2e stack moved to rustfs 1.0.1 and curl 8.22.0. The socket.io
  Go modules moved to v3.0.6, and every lockfile moved to the latest
  in-range versions. `@excalidraw/excalidraw` stays at 0.18.1.

### Fixed

- `make e2e-up` failed on the current GitHub runner image: Compose 5
  sends every build to bake, and bake refuses `network: host`. The
  script now builds the Drive image with a plain `docker build
  --network=host` and starts the stack with `--no-build`.

## [1.1.1] - 2026-09-20

### Added

- A Playwright spec under `e2e/playwright/specs/local` delays the save
  and measures the main-thread block on a tab hide. The spec fails
  against the previous code with a 2080 ms block.

### Fixed

- A tab hide froze the board in Firefox. The `visibilitychange`
  listener sent the whole scene with a synchronous XMLHttpRequest,
  which blocked the main thread. The listener now hands the save to
  the sync worker, which does an asynchronous fetch. It keeps the
  blocking save only while the worker is not ready.
- The teardown save stays synchronous on purpose: a keepalive fetch
  limits the sum of all in-flight bodies to 64 KiB, and a board with
  images is larger than that. The listener now also runs on
  `pagehide`, for iOS Safari, and it skips a back-forward cache entry.

## [1.1.0] - 2026-09-02

### Added

- A docker compose example at `deploy/docker`: one app instance
  behind a Caddy reverse proxy with automatic HTTPS, a one-shot
  chown service for the proof-key volume, and healthchecks on both
  containers. Caddy answers 200 on `/lbhealthz` and runs with the
  admin API off.
- A `-healthcheck` flag on the binary: it calls `GET /healthz` on
  the local server and exits 0 or 1. Both Dockerfiles now declare a
  `HEALTHCHECK` that runs it, so the distroless image probes itself.

### Changed

- The conflict-detection detail moved from `docs/DEPLOYMENT.md` to
  the "Lock lifecycle" section of `docs/ARCHITECTURE.md`; the
  operator guide keeps a short summary.
- The proof-key lifecycle section of `docs/DEPLOYMENT.md` shrank to
  the operator view.

## [1.0.1] - 2026-08-31

### Changed

- The Helm chart probes now set `timeoutSeconds: 5`; Kubernetes used
  the 1 s default before.
- The Helm chart `podSecurityContext` now sets `runAsGroup: 65532`.
- The Helm chart `containerSecurityContext` now defaults to
  `readOnlyRootFilesystem: true`, `allowPrivilegeEscalation: false`,
  and `capabilities.drop: ["ALL"]`.

### Fixed

- A race in the relay interop suite: test h now awaits writer-b's copy
  of the reader broadcast, so a late event no longer fails tests i
  and j on a slow CI runner.
- A flaky timeout in the relay deadlock test: the budget grew from
  10 s to 30 s for busy CI runners.

## [1.0.0] - 2026-08-31

Initial release.
