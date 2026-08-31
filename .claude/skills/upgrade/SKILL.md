---
name: upgrade
description: Upgrade every production dependency to the latest version, hash, or digest, then run the test suites. Use when the user asks for the dependency upgrade sweep.
---

# Dependency upgrade sweep

Upgrade every item in the inventory. Apply the policy below. Then run
the test procedure. The git diff is the only record.

## Inventory

| Item | Where | How to find the latest |
|---|---|---|
| GitHub Actions | `.github/workflows/*.yml` | `GET https://api.github.com/repos/<action>/releases/latest`, then `GET .../commits/<tag>` with `Accept: application/vnd.github.sha` for the hash |
| Docker base images | `Dockerfile`, `Dockerfile.goreleaser` | a HEAD request on the registry manifest; read `docker-content-digest` (see below) |
| e2e images | `e2e/compose.yaml` | Docker Hub tag list |
| Go toolchain | the `go` line in `go.mod`, the `golang:` tag in `Dockerfile` | https://go.dev/dl |
| Go modules | `go.mod` | `go get -u ./... && go mod tidy` |
| Node dependencies | `web/package.json`, `e2e/*/package.json` | `npx npm-check-updates` in each directory |
| Node toolchain | the `node:` tag in `Dockerfile`, the `node-version` inputs in the workflows | https://nodejs.org/en/about/previous-releases |
| golangci-lint | the version pin in the Makefile `lint` target | GitHub releases |
| goreleaser | the `version:` line in `.github/workflows/release.yml` | GitHub releases |

## How to read a registry digest

Docker Hub needs a bearer token first:

```sh
tok=$(curl -s "https://auth.docker.io/token?service=registry.docker.io&scope=repository:<ns>/<repo>:pull" | jq -r .token)
curl -sI -H "Authorization: Bearer $tok" \
  -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json' \
  "https://registry-1.docker.io/v2/<ns>/<repo>/manifests/<tag>" | grep -i docker-content-digest
```

A library image uses the `library/` namespace. `gcr.io` accepts the
same HEAD request without a token. Confirm the response content type
is an index, not a single-platform manifest.

## Pin forms

- An action pin is `uses: <action>@<full-sha> # <tag>`. Update the hash
  and the comment together.
- A production image pin is `<tag>@sha256:<digest>`. Use the multi-arch
  index digest, not a single-platform digest.
- An e2e image pin is a plain tag. Do not add a digest in
  `e2e/compose.yaml`.

## Policy

- Apply a minor or patch upgrade without a question. Test after it.
- Before a major upgrade, read the release notes. When the upgrade
  needs changes in our code, ask the user first. Apply it only after
  the user agrees.
- Keep `@excalidraw/excalidraw` below 0.19.0. Ask before a move to
  0.19 or higher.
- Do not push. Do not write an upgrade log in the docs.

## Test procedure

1. Run `make test`, `make lint`, and `make web-lint`.
2. Run `make interop`, `make e2e-local`, and `make e2e-ha`.
3. When docker is available, run `make e2e-up`, then
   `make test-drive-integration`, then `make e2e-smoke`, then
   `make e2e-down`.
4. When a suite fails, fix the cause or revert the one upgrade that
   broke it. Report every reverted upgrade to the user.
