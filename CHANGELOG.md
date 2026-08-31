# Changelog

All notable changes to this project are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- The Helm chart probes now set `timeoutSeconds: 5`; Kubernetes used
  the 1 s default before.
- The Helm chart `podSecurityContext` now sets `runAsGroup: 65532`.
- The Helm chart `containerSecurityContext` now defaults to
  `readOnlyRootFilesystem: true`, `allowPrivilegeEscalation: false`,
  and `capabilities.drop: ["ALL"]`.

## [1.0.0] - 2026-08-31

Initial release.

### Added

- A single Go binary that makes excalidraw a WOPI editor for La Suite
  Drive: discovery XML, `POST /launch`, the board REST API, the
  socket.io realtime relay, and the embedded TypeScript SPA.
- Proof-signed WOPI calls from the server; the browser never talks to
  Drive directly.
- Stateless sessions: an HS256 JWT with an AES-256-GCM-sealed WOPI
  access token inside.
- Multi-user realtime collaboration with presence, cursors, image
  relay, and syncer election.
- Multi-replica operation: rendezvous-hash room ownership with DNS
  peer discovery, or a hashing load balancer in front.
- Save-and-lock orchestration with conflict detection and a user
  prompt; the service never merges on its own.
- A Helm chart, published to GHCR as an OCI artifact.
- The e2e suites: interop, local Playwright, HA, dockerized Drive
  smoke, and the nightly slow suite.

[Unreleased]: https://github.com/zeylos/excalidraw-wopi/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/zeylos/excalidraw-wopi/releases/tag/v1.0.0
