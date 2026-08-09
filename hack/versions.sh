#!/usr/bin/env bash
# Every pinned external version. Nothing else may hardcode one — a version in
# two places drifts, and the drift surfaces weeks later as an unreproducible
# e2e failure.
#
# `hack/versions.sh check` verifies every pin still resolves upstream.

KUBECTL_VERSION="${KUBECTL_VERSION:-v1.34.1}"
HELM_VERSION="${HELM_VERSION:-v3.19.0}"
TALOSCTL_VERSION="${TALOSCTL_VERSION:-v1.11.2}"
FLUX_VERSION="${FLUX_VERSION:-2.7.2}" # no leading v; flux release assets omit it
GOLANGCI_VERSION="${GOLANGCI_VERSION:-v2.12.2}"

TALOS_VERSION="${TALOS_VERSION:-v1.11.2}" # must match TALOSCTL_VERSION
KUBERNETES_VERSION="${KUBERNETES_VERSION:-v1.34.1}"
CILIUM_VERSION="${CILIUM_VERSION:-1.18.12}" # helm chart version
PIRAEUS_VERSION="${PIRAEUS_VERSION:-v2.11.0}"
ZOT_VERSION="${ZOT_VERSION:-v2.1.20}"
CRANE_VERSION="${CRANE_VERSION:-v0.21.9}" # e2e only: pushes a fixture image

# Pinned to what Cilium 1.18 declares conformance against. A newer Gateway API
# installs CRDs with fields the Cilium operator does not understand, and it
# reports that as a GatewayClass reconcile error rather than a version mismatch.
GATEWAY_API_VERSION="${GATEWAY_API_VERSION:-v1.3.0}"

IMAGE_FACTORY="${IMAGE_FACTORY:-https://factory.talos.dev}"

_check_url() {
	local what="$1" url="$2"
	if curl -fsSL --max-time 20 -o /dev/null "$url"; then
		printf '  ok       %-14s %s\n' "$what" "$url"
	else
		printf '  MISSING  %-14s %s\n' "$what" "$url"
		return 1
	fi
}

# The registry image is pulled by the cluster, not downloaded here, so the tag
# is checked against the registry API rather than a release page.
_check_zot() {
	local repo=project-zot/zot-linux-amd64 tok code
	tok="$(curl -fsSL --max-time 20 "https://ghcr.io/token?scope=repository:${repo}:pull" | jq -r .token)"
	code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 \
		-H "Authorization: Bearer ${tok}" \
		-H 'Accept: application/vnd.oci.image.index.v1+json,application/vnd.oci.image.manifest.v1+json' \
		"https://ghcr.io/v2/${repo}/manifests/${ZOT_VERSION}")"
	if [[ "$code" == 200 ]]; then
		printf '  ok       %-14s ghcr.io/%s:%s\n' zot "$repo" "$ZOT_VERSION"
	else
		printf '  MISSING  %-14s ghcr.io/%s:%s (HTTP %s)\n' zot "$repo" "$ZOT_VERSION" "$code"
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
	_check_url golangci-lint "https://github.com/golangci/golangci-lint/releases/tag/${GOLANGCI_VERSION}" || rc=1
	_check_url talos-iso "https://github.com/siderolabs/talos/releases/download/${TALOS_VERSION}/metal-amd64.iso" || rc=1
	_check_url image-factory "${IMAGE_FACTORY}/versions" || rc=1
	_check_url gateway-api "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml" || rc=1
	_check_url piraeus "https://github.com/piraeusdatastore/piraeus-operator/releases/tag/${PIRAEUS_VERSION}" || rc=1
	_check_zot || rc=1
	_check_url crane "https://github.com/google/go-containerregistry/releases/tag/${CRANE_VERSION}" || rc=1

	# Buffered rather than piped into grep: under `set -o pipefail`, grep -q
	# exiting early makes curl fail with SIGPIPE and the chart reads as missing.
	local index
	index="$(curl -fsSL --max-time 30 https://helm.cilium.io/index.yaml || true)"
	if grep -q "version: ${CILIUM_VERSION}$" <<<"$index"; then
		printf '  ok       %-14s chart %s\n' cilium "$CILIUM_VERSION"
	else
		printf '  MISSING  %-14s chart %s not in helm.cilium.io index\n' cilium "$CILIUM_VERSION"
		rc=1
	fi

	# A schematic naming a nonexistent extension is accepted at POST time and
	# fails at image download, which is 900 MB too late.
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
	*)
		echo "usage: $0 check" >&2
		exit 2
		;;
	esac
fi
