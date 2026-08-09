#!/usr/bin/env bash
# Single source of truth for every pinned external version.
#
# Sourced by deps.sh, e2e.sh and CI. Nothing else may hardcode a version — a
# version that appears in two places drifts, and the drift surfaces as an
# unreproducible e2e failure weeks later.
#
# Run `hack/versions.sh check` to verify every pin still resolves upstream.

# Tools installed on the developer/CI machine.
KUBECTL_VERSION="${KUBECTL_VERSION:-v1.34.1}"
HELM_VERSION="${HELM_VERSION:-v3.19.0}"
TALOSCTL_VERSION="${TALOSCTL_VERSION:-v1.11.2}"
FLUX_VERSION="${FLUX_VERSION:-2.7.2}" # no leading v, flux release assets omit it

# Cluster components.
TALOS_VERSION="${TALOS_VERSION:-v1.11.2}"    # must match TALOSCTL_VERSION
KUBERNETES_VERSION="${KUBERNETES_VERSION:-v1.34.1}"
CILIUM_VERSION="${CILIUM_VERSION:-1.18.12}"  # helm chart version
PIRAEUS_VERSION="${PIRAEUS_VERSION:-v2.11.0}"
# Pinned to what Cilium 1.18 declares conformance against. A newer Gateway API
# release installs CRDs with fields the Cilium operator does not understand, and
# it reports that as a reconcile error against the GatewayClass rather than as a
# version mismatch.
GATEWAY_API_VERSION="${GATEWAY_API_VERSION:-v1.3.0}"

# Talos Image Factory. The DRBD system extension MUST be baked into the
# installer image; without it the first replicated PVC fails and the failure
# mode is an opaque mount error, not a missing-module message.
IMAGE_FACTORY="${IMAGE_FACTORY:-https://factory.talos.dev}"
TALOS_EXTENSIONS="${TALOS_EXTENSIONS:-siderolabs/drbd siderolabs/qemu-guest-agent}"

# --- check -------------------------------------------------------------------

_check_url() {
	local what="$1" url="$2"
	if curl -fsSL --max-time 20 -o /dev/null "$url"; then
		printf '  ok       %-14s %s\n' "$what" "$url"
	else
		printf '  MISSING  %-14s %s\n' "$what" "$url"
		return 1
	fi
}

versions_check() {
	local rc=0
	echo "Verifying pinned versions resolve upstream:"
	_check_url kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" || rc=1
	_check_url helm "https://get.helm.sh/helm-${HELM_VERSION}-linux-amd64.tar.gz" || rc=1
	_check_url talosctl "https://github.com/siderolabs/talos/releases/download/${TALOSCTL_VERSION}/talosctl-linux-amd64" || rc=1
	_check_url flux "https://github.com/fluxcd/flux2/releases/download/v${FLUX_VERSION}/flux_${FLUX_VERSION}_linux_amd64.tar.gz" || rc=1
	_check_url talos-iso "https://github.com/siderolabs/talos/releases/download/${TALOS_VERSION}/metal-amd64.iso" || rc=1
	_check_url image-factory "${IMAGE_FACTORY}/versions" || rc=1
	_check_url gateway-api "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml" || rc=1
	_check_url piraeus "https://github.com/piraeusdatastore/piraeus-operator/releases/tag/${PIRAEUS_VERSION}" || rc=1

	# The chart repository is an index, not a file tree, so a version is
	# "present" only if it appears in the index. The index is buffered rather
	# than piped into grep: under `set -o pipefail`, grep -q exiting early makes
	# curl fail with SIGPIPE and the check reports a missing chart that is there.
	local index
	index="$(curl -fsSL --max-time 30 https://helm.cilium.io/index.yaml || true)"
	if grep -q "version: ${CILIUM_VERSION}$" <<<"$index"; then
		printf '  ok       %-14s chart %s\n' cilium "$CILIUM_VERSION"
	else
		printf '  MISSING  %-14s chart %s not in helm.cilium.io index\n' cilium "$CILIUM_VERSION"
		rc=1
	fi

	# The Image Factory only proves out by building: a schematic naming a
	# nonexistent extension is accepted at POST time and fails at image
	# download, which is 900 MB too late.
	local id
	id="$(curl -fsSL --max-time 30 -X POST --data-binary @"$(dirname "${BASH_SOURCE[0]}")/talos/schematic.yaml" \
		"${IMAGE_FACTORY}/schematics" | jq -r .id 2>/dev/null || true)"
	if [[ -n "$id" && "$id" != null ]]; then
		printf '  ok       %-14s %s\n' schematic "$id"
	else
		printf '  MISSING  %-14s image factory rejected hack/talos/schematic.yaml\n' schematic
		rc=1
	fi

	return $rc
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	set -euo pipefail
	case "${1:-check}" in
	check) versions_check ;;
	print) set | grep -E '^(KUBECTL|HELM|TALOS|TALOSCTL|FLUX|KUBERNETES|CILIUM|PIRAEUS|GATEWAY_API|IMAGE_FACTORY)' ;;
	*)
		echo "usage: $0 [check|print]" >&2
		exit 2
		;;
	esac
fi
