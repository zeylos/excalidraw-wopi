# Changelog

All notable changes to this project are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
