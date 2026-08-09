#!/usr/bin/env bash
# Shared helpers for the hack/ scripts. Not executable on its own.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export REPO_ROOT

# All generated state lives here and is gitignored. Deleting this directory plus
# `e2e.sh down` returns the machine to a clean state.
E2E_DIR="${E2E_DIR:-${REPO_ROOT}/.e2e}"
export E2E_DIR
export KUBECONFIG="${KUBECONFIG:-${E2E_DIR}/kubeconfig}"
export TALOSCONFIG="${TALOSCONFIG:-${E2E_DIR}/talosconfig}"

# --- output ------------------------------------------------------------------

_ts() { date -u +%H:%M:%S; }
log() { printf '\033[1;34m[%s] ==>\033[0m %s\n' "$(_ts)" "$*"; }
step() { printf '\033[1;32m[%s] ###\033[0m %s\n' "$(_ts)" "$*"; }
warn() { printf '\033[1;33m[%s] warn:\033[0m %s\n' "$(_ts)" "$*" >&2; }
die() {
	printf '\033[1;31m[%s] error:\033[0m %s\n' "$(_ts)" "$*" >&2
	exit 1
}

# --- preconditions -----------------------------------------------------------

require_tools() {
	local missing=()
	for t in "$@"; do command -v "$t" >/dev/null 2>&1 || missing+=("$t"); done
	[[ ${#missing[@]} -eq 0 ]] || die "missing tools: ${missing[*]} — run 'make deps-install'"
}

require_libvirt() {
	virsh -c "$LIBVIRT_URI" version >/dev/null 2>&1 ||
		die "cannot reach libvirt at $LIBVIRT_URI — is libvirtd running, and are you in the 'libvirt' group?"
}

# --- retry -------------------------------------------------------------------

# retry <attempts> <sleep-seconds> <description> -- <command...>
#
# Talos and Kubernetes both spend their first minutes returning connection
# errors that are indistinguishable from real faults, so every command aimed at
# a booting cluster goes through here. The last failure's output is printed on
# give-up; swallowing it turns a five-minute bring-up into a blind one.
retry() {
	local attempts="$1" delay="$2" what="$3"
	shift 3
	[[ "${1:-}" == "--" ]] && shift
	local n=1 out rc
	while :; do
		if out="$("$@" 2>&1)"; then
			[[ -n "$out" ]] && printf '%s\n' "$out"
			return 0
		fi
		rc=$?
		if ((n >= attempts)); then
			printf '%s\n' "$out" >&2
			die "$what: gave up after $n attempts (exit $rc)"
		fi
		((n % 10 == 0)) && log "$what: attempt $n/$attempts still failing"
		n=$((n + 1))
		sleep "$delay"
	done
}

# --- misc --------------------------------------------------------------------

# indent stdin, so nested tool output is visually distinct from our own logs.
indent() { sed 's/^/    /'; }
