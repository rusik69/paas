#!/usr/bin/env bash
# Publishes the platform: every chart under packages/, then the release artifact
# that lists them.
#
# Provisioning only. Every assertion about what was published lives in Go, under
# test/e2e or internal/controller/platform.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=hack/lib.sh
source "${HERE}/lib.sh"
# shellcheck source=hack/versions.sh
source "${HERE}/versions.sh"

PACKAGES_DIR="${PACKAGES_DIR:-$(dirname "$HERE")/packages}"
# Which manifest is published. Overridable so a caller can cut a release from a
# variant without editing the repository's own.
PACKAGES_MANIFEST="${PACKAGES_MANIFEST:-${PACKAGES_DIR}/packages.yaml}"
# Where the cluster pulls from. The in-cluster registry's ClusterIP is not
# reachable from here, so publishing goes through a port-forward or a host
# alias; both resolve to the same repository path the Platform names.
REGISTRY="${REGISTRY:-oci://localhost:5000/paas}"

# One scratch directory for the whole run, cleaned on exit. A per-function
# RETURN trap fires again as the caller returns, when its local is already out
# of scope, and under `set -u` that aborts a run that had already succeeded.
SCRATCH=""
cleanup() { [[ -z "$SCRATCH" ]] || rm -rf "$SCRATCH"; }
trap cleanup EXIT

scratch_dir() {
	[[ -n "$SCRATCH" ]] || SCRATCH="$(mktemp -d)"
	local dir="${SCRATCH}/$1"
	mkdir -p "$dir"
	printf '%s' "$dir"
}

# Charts go directly under the registry, because that is where helm-controller
# looks: a HelmRepository whose url is <registry> resolves chart "hello" to
# <registry>/hello. They cannot collide with the release artifact, which is a
# tag on <registry> itself rather than a repository beneath it.
chart_repo() { printf '%s' "${REGISTRY%/}"; }

cmd_charts() {
	require_tools helm
	step "publishing charts from ${PACKAGES_DIR}"

	local scratch chart name version
	scratch="$(scratch_dir charts)"

	while IFS= read -r chart; do
		name="$(basename "$chart")"
		version="$(awk '/^version:/ {print $2; exit}' "${chart}/Chart.yaml")"
		[[ -n "$version" ]] || die "${chart}/Chart.yaml has no version"

		log "packaging ${name} ${version}"
		helm package "$chart" --destination "$scratch" >/dev/null
		helm push "${scratch}/${name}-${version}.tgz" "$(chart_repo)" >/dev/null 2>&1 ||
			die "pushing ${name} ${version} to $(chart_repo) failed"
		log "pushed ${name} ${version}"
	done < <(find "$PACKAGES_DIR" -mindepth 2 -maxdepth 2 -type d -exec test -f '{}/Chart.yaml' \; -print | sort)
}

cmd_release() {
	require_tools flux
	local version="${1:-}"
	[[ -n "$version" ]] || die "usage: $0 release <version>"

	[[ -f "$PACKAGES_MANIFEST" ]] ||
		die "${PACKAGES_MANIFEST} is missing; there is nothing to release"

	step "publishing release ${version}"
	local scratch
	scratch="$(scratch_dir release)"
	cp "$PACKAGES_MANIFEST" "${scratch}/packages.yaml"

	# --revision is required by flux and is recorded in the artifact's
	# annotations; it is not read by anything here.
	flux push artifact "${REGISTRY%/}:${version}" \
		--path="$scratch" \
		--source="paas" \
		--revision="${version}@sha1:$(git -C "$(dirname "$HERE")" rev-parse HEAD 2>/dev/null || echo 0)" \
		>/dev/null || die "pushing release ${version} failed"
	log "pushed release ${version} to ${REGISTRY}"
}

cmd_all() {
	cmd_charts
	cmd_release "${1:-}"
}

case "${1:-}" in
charts) cmd_charts ;;
release)
	shift
	cmd_release "$@"
	;;
all)
	shift
	cmd_all "$@"
	;;
*)
	echo "usage: $0 {charts|release <version>|all <version>}" >&2
	exit 2
	;;
esac
