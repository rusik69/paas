SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

GO ?= go
# Keep this target under ten seconds. The moment unit tests need a cluster the
# tier boundary has been violated and the feedback loop is gone for good.
UNIT_TIMEOUT ?= 60s
E2E_TIMEOUT  ?= 45m

.PHONY: help
help: ## Show this help
	@awk 'BEGIN{FS=":.*##"; printf "\nTargets:\n"} /^[a-zA-Z0-9_-]+:.*##/ {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

## --- verification ------------------------------------------------------------

.PHONY: test
test: ## Unit tests, race detector on. Must stay under ten seconds.
	$(GO) test -race -timeout $(UNIT_TIMEOUT) ./...

.PHONY: vet
vet: ## go vet — separate from lint, non-negotiable in CI
	$(GO) vet ./...

.PHONY: lint
lint: ## golangci-lint (installed on demand into ./bin)
	@command -v golangci-lint >/dev/null || { echo "golangci-lint not installed: see docs/go-guidelines.md"; exit 1; }
	golangci-lint run

.PHONY: fmt
fmt: ## gofumpt + shfmt
	@command -v gofumpt >/dev/null && gofumpt -l -w . || echo "gofumpt not installed, skipping"
	@command -v shfmt >/dev/null && shfmt -w -ln bash hack/*.sh || echo "shfmt not installed, skipping"

.PHONY: shellcheck
shellcheck: ## Lint the provisioning scripts
	@command -v shellcheck >/dev/null || { echo "shellcheck not installed"; exit 1; }
	shellcheck hack/*.sh

.PHONY: vuln
vuln: ## govulncheck — blocks merge in CI
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

.PHONY: verify
verify: vet test ## Everything that must pass before pushing

## --- environment -------------------------------------------------------------

.PHONY: deps
deps: ## Report missing phase-0 tooling
	hack/deps.sh check

.PHONY: deps-install
deps-install: ## Install missing phase-0 tooling (needs sudo)
	hack/deps.sh install

.PHONY: versions
versions: ## Verify every pinned external version still resolves
	hack/versions.sh check

## --- e2e ---------------------------------------------------------------------

.PHONY: cluster-up
cluster-up: ## Bring up the three-node Talos cluster on KVM
	hack/e2e.sh up

.PHONY: cluster-down
cluster-down: ## Destroy the cluster and every libvirt resource it created
	hack/e2e.sh down

.PHONY: cluster-status
cluster-status: ## Show cluster and storage health
	hack/e2e.sh status

.PHONY: e2e
e2e: ## Run the Go e2e assertions against a running cluster
	KUBECONFIG=$${KUBECONFIG:-$$PWD/.e2e/kubeconfig} \
		$(GO) test -tags e2e -race -timeout $(E2E_TIMEOUT) -v ./test/e2e/...

.PHONY: e2e-full
e2e-full: cluster-up e2e ## Provision, assert, then tear down
	$(MAKE) cluster-down
