SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

GO ?= go
# Keep this target under ten seconds. The moment unit tests need a cluster the
# tier boundary has been violated and the feedback loop is gone for good.
UNIT_TIMEOUT ?= 60s
E2E_TIMEOUT  ?= 45m

# Statement coverage floor for the unit tier, enforced in CI.
#
# A ratchet, not a target: raise it when a phase lands with more coverage, never
# lower it to make a red build green. Coverage measures which lines ran, not
# whether anything was asserted about them, so it is a floor under carelessness
# and not evidence that the tests are good.
COVERAGE_MIN ?= 85

.PHONY: help
help: ## Show this help
	@awk 'BEGIN{FS=":.*##"; printf "\nTargets:\n"} /^[a-zA-Z0-9_-]+:.*##/ {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

## --- verification ------------------------------------------------------------

.PHONY: test
test: ## Unit tests, race detector on. Must stay under ten seconds.
	$(GO) test -race -timeout $(UNIT_TIMEOUT) ./...

.PHONY: cover
cover: ## Unit coverage, failing below COVERAGE_MIN
	$(GO) test -race -timeout $(UNIT_TIMEOUT) -coverprofile=coverage.out -covermode=atomic ./...
	@$(GO) tool cover -func=coverage.out | tail -1
	@total=$$($(GO) tool cover -func=coverage.out | awk '/^total:/ {gsub(/%/,"",$$3); print $$3}'); \
	awk -v got="$$total" -v min=$(COVERAGE_MIN) 'BEGIN { \
		if (got+0 < min+0) { printf "coverage %.1f%% is below the %s%% floor\n", got, min; exit 1 } \
		printf "coverage %.1f%% (floor %s%%)\n", got, min }'

.PHONY: vet
vet: ## go vet — separate from lint, non-negotiable in CI
	$(GO) vet ./...

.PHONY: vet-e2e
vet-e2e: ## The e2e suite is behind a build tag and invisible to `make test`
	$(GO) vet -tags e2e ./...

.PHONY: actionlint
actionlint: ## Lint the GitHub Actions workflows
	$(GO) run github.com/rhysd/actionlint/cmd/actionlint@latest

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
verify: vet vet-e2e cover ## Everything that must pass before pushing

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
	TALOSCONFIG=$${TALOSCONFIG:-$$PWD/.e2e/talosconfig} \
		$(GO) test -tags e2e -race -timeout $(E2E_TIMEOUT) -v ./test/e2e/...
