SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

GO ?= go
# Keep this target under ten seconds. The moment unit tests need a cluster the
# tier boundary has been violated and the feedback loop is gone for good.
UNIT_TIMEOUT ?= 60s
E2E_TIMEOUT  ?= 45m
INTEGRATION_TIMEOUT ?= 10m
# Tracks the cluster's Kubernetes version: envtest must serve the same API
# surface the reconcilers will meet in production.
ENVTEST_K8S_VERSION ?= 1.34.x

# The floor applies per package, to the packages named here, not to a global
# percentage. A global number is satisfied by covering whatever is cheapest,
# which is how you get tests written for the number rather than for the bug
# (testing.md says so directly). Naming the packages says which code has to be
# near-exhaustive and leaves the rest to review.
#
# Add a package here when it becomes load-bearing. Raise the floor when a phase
# lands above it; never lower it to make a red build green.
COVERED_PACKAGES ?= internal/crd internal/flux internal/kube pkg/wait
COVERAGE_MIN ?= 95
MODULE := github.com/rusik69/paas

# hack/versions.sh is the only place a version may live, so read the go-run tool
# pins back out of it. Lazy `=`, so a target that needs none spawns no shell.
pin = $(shell source hack/versions.sh && printf '%s' "$${$(1)}")

.PHONY: help
help: ## Show this help
	@awk 'BEGIN{FS=":.*##"; printf "\nTargets:\n"} /^[a-zA-Z0-9_-]+:.*##/ {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

## --- verification ------------------------------------------------------------

.PHONY: test
test: ## Unit tests, race detector on. Must stay under ten seconds.
	$(GO) test -race -timeout $(UNIT_TIMEOUT) ./...

.PHONY: cover
# One run with the integration tag rather than two profiles merged: the untagged
# unit tests compile into this build too, so it is the union already, and merging
# profiles by hand double-counts every block both tiers execute. Gating on the
# unit tier alone would demand fake-client tests for server-side apply, which
# go-guidelines forbids — testing SSA against a fake tests the fake.
#
# Two exclusions. Generated deepcopy is mechanical and large enough to dominate
# the number in either direction. cmd/ is flag wiring by construction, because
# go-guidelines requires it stay thin and everything real live below; measuring
# it would reward moving logic up into main(), which is backwards. If a cmd/
# file ever grows logic worth testing, that logic is in the wrong package.
cover: ## Coverage across both test tiers, failing below COVERAGE_MIN
	KUBEBUILDER_ASSETS="$$($(ENVTEST_ASSETS))" \
		$(GO) test -tags integration -race -timeout $(INTEGRATION_TIMEOUT) \
		-coverprofile=coverage.out -covermode=atomic ./...
	@grep -vE '/zz_generated\.|/cmd/' coverage.out >coverage.filtered && mv coverage.filtered coverage.out
	@$(GO) tool cover -func=coverage.out | tail -1
	@awk -v pkgs="$(COVERED_PACKAGES)" -v module="$(MODULE)" -v min=$(COVERAGE_MIN) ' \
	NR > 1 { \
		file = $$1; sub(/:.*/, "", file); sub(/\/[^\/]*$$/, "", file); \
		total[file] += $$2; if ($$3 > 0) covered[file] += $$2 \
	} \
	END { \
		n = split(pkgs, want, " "); rc = 0; \
		for (i = 1; i <= n; i++) { \
			p = module "/" want[i]; \
			if (!(p in total)) { printf "no coverage data for %s\n", want[i]; rc = 1; continue } \
			pct = 100 * covered[p] / total[p]; \
			if (pct + 0 < min + 0) { printf "%-16s %.1f%% is below the %s%% floor\n", want[i], pct, min; rc = 1 } \
			else { printf "%-16s %.1f%% (floor %s%%)\n", want[i], pct, min } \
		} \
		exit rc \
	}' coverage.out

.PHONY: vet
vet: ## go vet — separate from lint, non-negotiable in CI
	$(GO) vet ./...

.PHONY: vet-e2e
vet-e2e: ## The e2e suite is behind a build tag and invisible to `make test`
	$(GO) vet -tags e2e ./...

.PHONY: vet-integration
vet-integration: ## The envtest suite is behind a build tag and invisible to `make test`
	$(GO) vet -tags integration ./...

# Downloading on demand, so a fresh checkout needs no install step.
ENVTEST_ASSETS = $(GO) run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(call pin,SETUP_ENVTEST_VERSION) use -p path $(ENVTEST_K8S_VERSION)

.PHONY: test-integration
test-integration: ## envtest — a real apiserver and etcd, no kubelet
	KUBEBUILDER_ASSETS="$$($(ENVTEST_ASSETS))" \
		$(GO) test -tags integration -race -timeout $(INTEGRATION_TIMEOUT) ./...

.PHONY: generate
generate: ## controller-gen: deepcopy methods and CRD manifests
	$(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@$(call pin,CONTROLLER_GEN_VERSION) \
		object paths=./api/...
	$(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@$(call pin,CONTROLLER_GEN_VERSION) \
		crd paths=./api/... output:crd:artifacts:config=internal/crd/manifests

# Vendored rather than fetched at runtime, for the reason the CRDs are embedded:
# a binary should install the versions it was built against, and an operator
# that reaches the network on startup fails in a new way.
.PHONY: vendor-flux
vendor-flux: ## Regenerate the vendored Flux manifests from the pinned CLI
	@command -v flux >/dev/null || { echo "flux not installed: run 'make deps-install'"; exit 1; }
	@mkdir -p internal/flux/manifests
	flux install --export \
		--components=source-controller,helm-controller \
		--namespace=flux-system >internal/flux/manifests/flux.yaml

.PHONY: actionlint
actionlint: ## Lint the GitHub Actions workflows
	$(GO) run github.com/rhysd/actionlint/cmd/actionlint@$(call pin,ACTIONLINT_VERSION)

.PHONY: lint
lint: ## golangci-lint, configured by .golangci.yml
	@command -v golangci-lint >/dev/null || { echo "golangci-lint not installed: run 'make deps-install'"; exit 1; }
	golangci-lint run

.PHONY: fmt
fmt: ## gofumpt + shfmt, writing in place
	@command -v gofumpt >/dev/null && gofumpt -l -w . || echo "gofumpt not installed, skipping"
	$(GO) run mvdan.cc/sh/v3/cmd/shfmt@$(call pin,SHFMT_VERSION) -w -ln bash hack/*.sh

.PHONY: shfmt
shfmt: ## Shell formatting, the read-only check CI runs
	$(GO) run mvdan.cc/sh/v3/cmd/shfmt@$(call pin,SHFMT_VERSION) -d -ln bash hack/*.sh

.PHONY: check-stdout
check-stdout: ## hack/lib.sh progress must not reach stdout, or it lands in a captured return value
	@out=$$(bash -c 'source hack/lib.sh; log a; step b; warn c' 2>/dev/null); \
	test -z "$$out" || { echo "hack/lib.sh wrote to stdout: $$out"; exit 1; }
	@echo "stdout clean"

.PHONY: shellcheck
shellcheck: ## Lint the provisioning scripts
	@command -v shellcheck >/dev/null || { echo "shellcheck not installed"; exit 1; }
	shellcheck -x hack/*.sh

.PHONY: vuln
vuln: ## govulncheck — blocks merge in CI
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(call pin,GOVULNCHECK_VERSION) ./...

.PHONY: verify
verify: vet vet-e2e vet-integration lint cover check-stdout ## Everything that must pass before pushing

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

.PHONY: test-e2e
test-e2e: ## Provision, assert, tear down — the target named in docs/testing.md
	$(MAKE) cluster-up
	$(MAKE) e2e; status=$$?; $(MAKE) cluster-down; exit $$status
