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
	qemu-kvm libvirt-daemon-system libvirt-clients virtinst
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

# version_of <tool> — best-effort installed version, empty if absent.
version_of() {
	case "$1" in
	kubectl) kubectl version --client -o json 2>/dev/null | jq -r .clientVersion.gitVersion 2>/dev/null ;;
	helm) helm version --template '{{.Version}}' 2>/dev/null ;;
	talosctl) talosctl version --client --short 2>/dev/null | awk '/Tag/ {print $2}' ;;
	flux) flux version --client 2>/dev/null | awk '/flux/ {print $2}' ;;
	esac
}

check() {
	local tools_only=0
	[[ "${1:-}" == "--tools-only" ]] && tools_only=1
	local missing=0
	printf '%-14s %-12s %s\n' TOOL WANT STATUS
	for t in kubectl helm talosctl flux; do
		local want got
		case "$t" in
		kubectl) want="$KUBECTL_VERSION" ;;
		helm) want="$HELM_VERSION" ;;
		talosctl) want="$TALOSCTL_VERSION" ;;
		flux) want="v$FLUX_VERSION" ;;
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

	if id -nG | tr ' ' '\n' | grep -qx libvirt; then
		printf '%-14s %-12s ok\n' libvirt-group -
	else
		printf '%-14s %-12s MISSING (log out/in after install)\n' libvirt-group -
		missing=1
	fi

	return $missing
}

need_sudo() {
	sudo -n true 2>/dev/null && return 0
	sudo -v || die "this step needs sudo"
}

install_apt() {
	local todo=()
	for p in "${APT_PACKAGES[@]}"; do
		dpkg -s "$p" >/dev/null 2>&1 || todo+=("$p")
	done
	if [[ ${#todo[@]} -eq 0 ]]; then
		log "apt packages already present"
		return
	fi
	log "apt install: ${todo[*]}"
	sudo apt-get update -qq
	sudo DEBIAN_FRONTEND=noninteractive apt-get install -y "${todo[@]}"
}

# fetch_bin <name> <url> — download a bare binary to $BINDIR.
fetch_bin() {
	local name="$1" url="$2" tmp
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' RETURN
	log "installing $name from $url"
	curl -fsSL --retry 3 -o "$tmp/$name" "$url"
	chmod +x "$tmp/$name"
	sudo install -m0755 "$tmp/$name" "$BINDIR/$name"
}

# fetch_tgz <name> <url> <path-in-archive>
fetch_tgz() {
	local name="$1" url="$2" inner="$3" tmp
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' RETURN
	log "installing $name from $url"
	curl -fsSL --retry 3 -o "$tmp/a.tgz" "$url"
	tar -xzf "$tmp/a.tgz" -C "$tmp"
	sudo install -m0755 "$tmp/$inner" "$BINDIR/$name"
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
}

enable_libvirt() {
	sudo systemctl enable --now libvirtd
	# The default NAT network is what e2e.sh attaches guests to unless a
	# dedicated network is created; without it virt-install fails late.
	sudo virsh net-info default >/dev/null 2>&1 || sudo virsh net-define /usr/share/libvirt/networks/default.xml
	sudo virsh net-autostart default >/dev/null 2>&1 || true
	sudo virsh net-start default >/dev/null 2>&1 || true
	for g in libvirt kvm; do
		getent group "$g" >/dev/null || continue
		id -nG | tr ' ' '\n' | grep -qx "$g" || {
			log "adding $USER to group $g"
			sudo usermod -aG "$g" "$USER"
			warn "log out and back in (or run: newgrp $g) before hack/e2e.sh up"
		}
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
