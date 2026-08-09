#!/usr/bin/env bash
# Shared helpers for the hack/ scripts. Not executable on its own.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export REPO_ROOT

E2E_DIR="${E2E_DIR:-${REPO_ROOT}/.e2e}"
export E2E_DIR
export KUBECONFIG="${KUBECONFIG:-${E2E_DIR}/kubeconfig}"
export TALOSCONFIG="${TALOSCONFIG:-${E2E_DIR}/talosconfig}"

# All progress output goes to stderr. Functions here return values on stdout —
# download_iso returns a path — and a log line on the same stream is captured
# into the value, producing errors that name a filename with an ANSI escape in
# it rather than the mistake.
_ts() { date -u +%H:%M:%S; }
log() { printf '\033[1;34m[%s] ==>\033[0m %s\n' "$(_ts)" "$*" >&2; }
step() { printf '\033[1;32m[%s] ###\033[0m %s\n' "$(_ts)" "$*" >&2; }
warn() { printf '\033[1;33m[%s] warn:\033[0m %s\n' "$(_ts)" "$*" >&2; }
die() {
	printf '\033[1;31m[%s] error:\033[0m %s\n' "$(_ts)" "$*" >&2
	exit 1
}

require_tools() {
	local missing=()
	for t in "$@"; do command -v "$t" >/dev/null 2>&1 || missing+=("$t"); done
	[[ ${#missing[@]} -eq 0 ]] || die "missing tools: ${missing[*]} — run 'make deps-install'"
}

require_libvirt() {
	virsh -c "$LIBVIRT_URI" version >/dev/null 2>&1 ||
		die "cannot reach libvirt at $LIBVIRT_URI — is libvirtd running, and are you in the 'libvirt' group?"
}

# retry <attempts> <delay> <description> -- <command...>
#
# Talos and Kubernetes spend their first minutes returning connection errors
# indistinguishable from real faults. The last failure's output is printed on
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
		else
			# Captured here, not after the fi: there $? is the if statement's
			# own status, which is 0, and every failure reports "exit 0".
			rc=$?
		fi
		if ((n >= attempts)); then
			printf '%s\n' "$out" >&2
			die "$what: gave up after $n attempts (exit $rc)"
		fi
		((n % 10 == 0)) && log "$what: attempt $n/$attempts still failing"
		n=$((n + 1))
		sleep "$delay"
	done
}

indent() { sed 's/^/    /'; }
