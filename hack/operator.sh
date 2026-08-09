#!/usr/bin/env bash
# Builds the operator, pushes it to the in-cluster registry, and deploys it.
#
# Provisioning only. Everything asserted about the running operator lives in
# test/e2e.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "$HERE")"
# shellcheck source=hack/lib.sh
source "${HERE}/lib.sh"

# The name Talos' machine.registries mirror resolves to the registry Service, so
# containerd on every node can pull what was pushed through the port-forward.
OPERATOR_IMAGE="${OPERATOR_IMAGE:-registry.paas.io/paas/operator:e2e}"
# Where the push goes. The registry ClusterIP is not routable from here, so
# publishing goes through a port-forward that maps to the same repository path.
PUSH_HOST="${PUSH_HOST:-localhost:5000}"
REGISTRY_NAMESPACE="${REGISTRY_NAMESPACE:-paas-system}"

PORT_FORWARD_PID=""
cleanup() { [[ -z "$PORT_FORWARD_PID" ]] || kill "$PORT_FORWARD_PID" 2>/dev/null || true; }
trap cleanup EXIT

start_port_forward() {
	kubectl -n "$REGISTRY_NAMESPACE" port-forward svc/registry 5000:5000 >/dev/null 2>&1 &
	PORT_FORWARD_PID=$!
	# The forward is ready when the registry answers, not when kubectl returns.
	retry 30 1 "registry port-forward" -- curl -fsS -o /dev/null "http://${PUSH_HOST}/v2/"
}

cmd_image() {
	require_tools go kubectl curl
	step "building the operator"
	local out="${REPO_ROOT}/.e2e/paas-operator"
	mkdir -p "$(dirname "$out")"
	# Static, because the image has no libc: it is the binary and nothing else.
	(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$out" ./cmd/paas-operator)

	start_port_forward
	step "pushing ${OPERATOR_IMAGE}"
	local push_ref="${PUSH_HOST}/${OPERATOR_IMAGE#*/}"
	(cd "$REPO_ROOT" && go run ./hack/tools/imgpush -binary "$out" -ref "$push_ref") >/dev/null
	log "pushed ${push_ref}"
}

cmd_publish() {
	require_tools kubectl curl helm flux
	start_port_forward
	local version="${1:-}"
	[[ -n "$version" ]] || die "usage: $0 publish <version>"
	local manifest="${2:-}"
	if [[ -n "$manifest" ]]; then
		REGISTRY="oci://${PUSH_HOST}/paas" PACKAGES_MANIFEST="$manifest" \
			"${HERE}/publish.sh" all "$version"
	else
		REGISTRY="oci://${PUSH_HOST}/paas" "${HERE}/publish.sh" all "$version"
	fi
}

cmd_deploy() {
	require_tools kubectl envsubst
	step "deploying the operator"
	OPERATOR_IMAGE="$OPERATOR_IMAGE" envsubst <"${HERE}/manifests/operator.yaml" |
		kubectl apply --server-side -f - >/dev/null
	kubectl -n "$REGISTRY_NAMESPACE" rollout status deploy/paas-operator --timeout=5m
	log "operator is running"
}

# The two releases the e2e sequence moves between. v0.2.0 changes the
# component's values and drops the migration, so one upgrade exercises both an
# update and a removal, and the rollback has something real to restore.
cmd_e2e_releases() {
	require_tools kubectl curl helm flux
	local scratch
	scratch="$(mktemp -d)"
	trap 'rm -rf "$scratch"' RETURN

	cmd_publish v0.1.0

	cat >"${scratch}/packages.yaml" <<-'YAML'
		packages:
		  - name: hello
		    chart: hello
		    version: "0.1.0"
		    stage: component
		    values:
		      message: v2
	YAML
	cmd_publish v0.2.0 "${scratch}/packages.yaml"
}

cmd_all() {
	cmd_image
	cmd_deploy
	cmd_e2e_releases
}

case "${1:-}" in
image) cmd_image ;;
releases) cmd_e2e_releases ;;
deploy) cmd_deploy ;;
publish)
	shift
	cmd_publish "$@"
	;;
all) cmd_all ;;
*)
	echo "usage: $0 {image|deploy|publish <version> [manifest]|releases|all}" >&2
	exit 2
	;;
esac
