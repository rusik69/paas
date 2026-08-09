#!/usr/bin/env bash
# Provision the phase-0 toolchain on a Debian/Ubuntu developer machine.
#
#   hack/deps.sh check     report what is present and what is missing (no sudo)
#   hack/deps.sh check --tools-only
#                          as above, but skip host capabilities. CI proves the
#                          installer works on a clean machine, and a hosted
#                          runner has neither nested virtualisation nor a
#                          re-logged-in shell for the new group membership.
#   hack/deps.sh install   install everything missing (needs sudo)
#
# Idempotent: re-running installs nothing that is already at the pinned version.
# Binaries go to /usr/local/bin so distro packages never shadow them.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=hack/versions.sh
source "${REPO_ROOT}/hack/versions.sh"

BINDIR="${BINDIR:-/usr/local/bin}"
APT_PACKAGES=(
	make git curl jq
	gettext-base # envsubst, used to render the Talos config patches
	# qemu-system-x86, not qemu-kvm: the latter is a virtual package on
	# Debian trixie and Ubuntu 25.10+, and apt refuses to install one.
	qemu-system-x86 libvirt-daemon-system libvirt-clients virtinst
	genisoimage # talos machine-config delivery via a metadata ISO
	docker.io   # chart/image publishing in phase 1; not used by phase-0 e2e
)

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarn:\033[0m %s\n' "$*" >&2; }
die() {
	printf '\033[1;31merror:\033[0m %s\n' "$*" >&2
	exit 1
}

have() { command -v "$1" >/dev/null 2>&1; }

in_group_on_disk() { getent group "$1" | cut -d: -f4 | tr ',' '\n' | grep -qx "$USER"; }

# dpkg -s succeeds for a package that was removed but not purged, so it reports
# config-files state as installed and apt-get is never called.
installed_pkg() { [[ "$(dpkg-query -W -f='${db:Status-Status}' "$1" 2>/dev/null)" == installed ]]; }

# version_of <tool> — best-effort installed version, empty if absent.
version_of() {
	case "$1" in
	kubectl) kubectl version --client -o json 2>/dev/null | jq -r .clientVersion.gitVersion 2>/dev/null ;;
	helm) helm version --template '{{.Version}}' 2>/dev/null ;;
	talosctl) talosctl version --client 2>/dev/null | awk '/Tag:/ {print $2}' ;;
	flux) flux version --client 2>/dev/null | awk '/flux/ {print $2}' ;;
	golangci-lint) golangci-lint version 2>/dev/null | awk '/has version/ {print $4}' ;;
	esac
}

check() {
	local tools_only=0
	[[ "${1:-}" == "--tools-only" ]] && tools_only=1
	local missing=0
	printf '%-14s %-12s %s\n' TOOL WANT STATUS
	for t in kubectl helm talosctl flux golangci-lint; do
		local want got
		case "$t" in
		kubectl) want="$KUBECTL_VERSION" ;;
		helm) want="$HELM_VERSION" ;;
		talosctl) want="$TALOSCTL_VERSION" ;;
		flux) want="v$FLUX_VERSION" ;;
		# GOLANGCI_VERSION is pinned with a leading v (matches the release tag);
		# the binary reports its own version without one.
		golangci-lint) want="${GOLANGCI_VERSION#v}" ;;
		esac
		got="$(version_of "$t" || true)"
		if [[ -z "$got" ]]; then
			printf '%-14s %-12s MISSING\n' "$t" "$want"
			missing=1
		elif [[ "$got" != "$want" ]]; then
			printf '%-14s %-12s have %s (mismatch)\n' "$t" "$want" "$got"
			missing=1
		else
			printf '%-14s %-12s ok\n' "$t" "$want"
		fi
	done
	for t in go git make curl jq envsubst virsh virt-install qemu-system-x86_64 qemu-img genisoimage; do
		if have "$t"; then
			printf '%-14s %-12s ok\n' "$t" -
		else
			printf '%-14s %-12s MISSING\n' "$t" -
			missing=1
		fi
	done

	if ((tools_only)); then
		return $missing
	fi

	# Host capabilities the e2e harness depends on.
	local nested
	nested="$(cat /sys/module/kvm_intel/parameters/nested 2>/dev/null || cat /sys/module/kvm_amd/parameters/nested 2>/dev/null || echo N)"
	printf '%-14s %-12s %s\n' nested-virt Y "$nested"
	[[ "$nested" == Y || "$nested" == 1 ]] || {
		warn "nested virtualisation is off; phase 5 (KubeVirt in guests) will not work"
		missing=1
	}

	# Three states, not two. usermod cannot alter a running session, so being in
	# the group on disk but not in this shell means the install worked and only
	# a re-login is outstanding — reporting that as missing makes a successful
	# install look failed.
	for g in libvirt kvm; do
		if id -nG | tr ' ' '\n' | grep -qx "$g"; then
			printf '%-14s %-12s ok\n' "${g}-group" -
		elif in_group_on_disk "$g"; then
			printf '%-14s %-12s pending (run: newgrp %s, or log out and back in)\n' "${g}-group" - "$g"
		else
			printf '%-14s %-12s MISSING — run make deps-install\n' "${g}-group" -
			missing=1
		fi
	done

	return $missing
}

need_sudo() {
	sudo -n true 2>/dev/null && return 0
	sudo -v || die "this step needs sudo"
}

install_apt() {
	local todo=()
	for p in "${APT_PACKAGES[@]}"; do
		installed_pkg "$p" || todo+=("$p")
	done
	if [[ ${#todo[@]} -eq 0 ]]; then
		log "apt packages already present"
		return
	fi
	log "apt install: ${todo[*]}"
	sudo apt-get update -qq
	sudo DEBIAN_FRONTEND=noninteractive apt-get install -y "${todo[@]}"
}

# One scratch dir for the whole run. A per-function `trap ... RETURN` is not
# scoped to that function: it stays installed and fires again on the next
# return, when its local temp variable is gone and `set -u` aborts the script.
SCRATCH=""
cleanup() { [[ -z "$SCRATCH" ]] || rm -rf "$SCRATCH"; }
trap cleanup EXIT

scratch_dir() {
	[[ -n "$SCRATCH" ]] || SCRATCH="$(mktemp -d)"
	local d="$SCRATCH/$1"
	mkdir -p "$d"
	printf '%s' "$d"
}

# fetch_bin <name> <url> — download a bare binary to $BINDIR.
fetch_bin() {
	local name="$1" url="$2" d
	d="$(scratch_dir "$name")"
	log "installing $name from $url"
	curl -fsSL --retry 3 -o "$d/$name" "$url"
	sudo install -m0755 "$d/$name" "$BINDIR/$name"
}

# fetch_tgz <name> <url> <path-in-archive>
fetch_tgz() {
	local name="$1" url="$2" inner="$3" d
	d="$(scratch_dir "$name")"
	log "installing $name from $url"
	curl -fsSL --retry 3 -o "$d/a.tgz" "$url"
	tar -xzf "$d/a.tgz" -C "$d"
	sudo install -m0755 "$d/$inner" "$BINDIR/$name"
}

install_tools() {
	[[ "$(version_of kubectl)" == "$KUBECTL_VERSION" ]] ||
		fetch_bin kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl"
	[[ "$(version_of helm)" == "$HELM_VERSION" ]] ||
		fetch_tgz helm "https://get.helm.sh/helm-${HELM_VERSION}-linux-amd64.tar.gz" linux-amd64/helm
	[[ "$(version_of talosctl)" == "$TALOSCTL_VERSION" ]] ||
		fetch_bin talosctl "https://github.com/siderolabs/talos/releases/download/${TALOSCTL_VERSION}/talosctl-linux-amd64"
	[[ "$(version_of flux)" == "v$FLUX_VERSION" ]] ||
		fetch_tgz flux "https://github.com/fluxcd/flux2/releases/download/v${FLUX_VERSION}/flux_${FLUX_VERSION}_linux_amd64.tar.gz" flux
	local golangci_no_v="${GOLANGCI_VERSION#v}"
	[[ "$(version_of golangci-lint)" == "$golangci_no_v" ]] ||
		fetch_tgz golangci-lint \
			"https://github.com/golangci/golangci-lint/releases/download/${GOLANGCI_VERSION}/golangci-lint-${golangci_no_v}-linux-amd64.tar.gz" \
			"golangci-lint-${golangci_no_v}-linux-amd64/golangci-lint"
}

enable_libvirt() {
	sudo systemctl enable --now libvirtd
	for g in libvirt kvm; do
		getent group "$g" >/dev/null || continue
		in_group_on_disk "$g" && continue
		log "adding $USER to group $g"
		sudo usermod -aG "$g" "$USER"
		warn "log out and back in (or run: newgrp $g) before hack/e2e.sh up"
	done
}

case "${1:-check}" in
check)
	shift || true
	check "$@"
	;;
install)
	need_sudo
	install_apt
	install_tools
	enable_libvirt
	log "done — re-run 'hack/deps.sh check' to confirm"
	;;
*)
	echo "usage: $0 [check [--tools-only]|install]" >&2
	exit 2
	;;
esac
