.PHONY: build web-build web-deps web-lint go-test web-test test lint dev \
	e2e-up e2e-down e2e-seed e2e-smoke e2e-local e2e-nightly interop e2e-ha test-drive-integration

build: web-build
	go build -o bin/excalidraw-wopi ./cmd/excalidraw-wopi

web-deps:
	cd web && npm ci

# vite empties dist on every build. The touch restores the tracked
# placeholder that keeps go:embed valid on a clean checkout.
web-build: web-deps
	cd web && npx vite build && touch dist/.gitkeep

go-test:
	go test ./...

web-test: web-deps
	cd web && npx tsc --noEmit && npx vitest run

web-lint: web-deps
	cd web && npx eslint src

test: go-test web-test

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.1 run

chart-lint: ## lint the Helm chart and render it under every documented topology
	@command -v helm >/dev/null || { echo "chart-lint needs helm v3.21+ on PATH" >&2; exit 1; }
	helm lint deploy/helm/excalidraw-wopi
	helm template deploy/helm/excalidraw-wopi > /dev/null
	helm template deploy/helm/excalidraw-wopi --set peerDiscovery.enabled=false > /dev/null
	helm template deploy/helm/excalidraw-wopi --set secret.create=true --set secret.proofKeyPem=x --set secret.sessionSecret=x > /dev/null
	helm template deploy/helm/excalidraw-wopi --set ingress.enabled=true --set podDisruptionBudget.enabled=true > /dev/null
.PHONY: chart-lint

dev:
	EXCALIDRAW_WOPI_LISTEN_ADDR=:8080 \
	EXCALIDRAW_WOPI_PUBLIC_URL=http://localhost:8080 \
	go run ./cmd/excalidraw-wopi

e2e-up: ## bring up the dockerized Drive stack and our own binary for e2e tests
	@e2e/scripts/e2e-up.sh
.PHONY: e2e-up

e2e-down: ## stop our binary and tear down the dockerized Drive stack
	@if [ -f e2e/.run/excalidraw-wopi.pid ]; then \
		pid=$$(cat e2e/.run/excalidraw-wopi.pid); \
		if kill -0 "$$pid" 2>/dev/null; then kill "$$pid"; fi; \
		rm -f e2e/.run/excalidraw-wopi.pid; \
	fi
	docker compose -f e2e/compose.yaml down -v
.PHONY: e2e-down

e2e-seed: ## seed one excalidraw item in the e2e stack and print its WOPI launch payload
	node e2e/scripts/seed.mjs
.PHONY: e2e-seed

e2e-smoke: ## run the PR-gate Playwright suite against an already-up e2e stack (`make e2e-up` first)
	@if ! docker compose -f e2e/compose.yaml ps --services --status running 2>/dev/null | grep -qx app-dev; then \
		echo "e2e-smoke: the e2e Drive stack is not up; run 'make e2e-up' first" >&2; \
		exit 1; \
	fi
	@if ! curl -sf http://localhost:8080/healthz > /dev/null; then \
		echo "e2e-smoke: our excalidraw-wopi binary is not answering on :8080; run 'make e2e-up' first" >&2; \
		exit 1; \
	fi
	cd e2e/playwright && npm ci
	cd e2e/playwright && (npx playwright install --with-deps chromium 2>/dev/null || npx playwright install chromium)
	cd e2e/playwright && npx playwright test --config=playwright.smoke.config.ts
.PHONY: e2e-smoke

e2e-local: build ## fast local Playwright suite: fake WOPI host, no Drive, no docker
	cd e2e/playwright && npm ci
	cd e2e/playwright && (npx playwright install --with-deps chromium 2>/dev/null || npx playwright install chromium)
	cd e2e/playwright && npx playwright test

# The e2e-smoke prerequisite installs the playwright npm deps and the
# chromium browser. A future split of this recipe must repeat both steps.
e2e-nightly: e2e-smoke ## nightly slow suite: smoke gate, socket load/storm, slow browser scenarios (needs `make e2e-up` first)
	cd e2e/nightly && npm ci && npx vitest run
	cd e2e/playwright && npx playwright test --config=playwright.nightly.config.ts
.PHONY: e2e-nightly

interop: ## Go-relay <-> socket.io-client v4 interop harness (see e2e/interop)
	cd e2e/interop && npm ci && npx vitest run
.PHONY: interop

e2e-ha: ## multi-replica hash-proxy failover harness: N instances behind a consistent hash (see e2e/ha)
	cd e2e/ha && npm ci && npx vitest run
.PHONY: e2e-ha

test-drive-integration: ## run the Go integration suite against the live e2e Drive stack (needs `make e2e-up` first)
	# e2e/integration/drive_test.go calls config.Load() and proof.Load()
	# itself; source the same env file e2e-up.sh starts the binary with, so
	# both processes resolve the same proof key and session secret. `go
	# test` runs the test binary with the package directory as its cwd, not
	# the invocation directory, so the env file's relative
	# EXCALIDRAW_WOPI_PROOF_KEY_PATH would resolve under e2e/integration/
	# instead of the repo root; override it here with an absolute path so
	# it matches what the running binary resolved.
	set -a && . e2e/env/excalidraw.env && export EXCALIDRAW_WOPI_PROOF_KEY_PATH="$$(pwd)/e2e/.run/proof-key.pem" && set +a && go test -tags driveintegration -count=1 -v ./e2e/integration/
.PHONY: test-drive-integration
