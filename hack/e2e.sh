#!/usr/bin/env bash
# Provision the phase-0 e2e environment: three Talos guests on libvirt/KVM,
# Cilium, Piraeus/DRBD, and the replicated StorageClasses.
#
#   hack/e2e.sh up          bring the cluster up (idempotent)
#   hack/e2e.sh down        destroy every resource this script created
#   hack/e2e.sh status      cluster, node and storage health
#   hack/e2e.sh nodes       print name/role/ip as TSV (read by the Go e2e suite)
#   hack/e2e.sh kill-node   hard power-off a guest, as an unclean node failure
#   hack/e2e.sh start-node  power a guest back on
#
# Provisioning only. Every assertion lives in Go under test/e2e — bash
# provisions, Go asserts (architecture.md §11).
set -euo pipefail

# shellcheck source=hack/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
# shellcheck source=hack/versions.sh
source "${REPO_ROOT}/hack/versions.sh"

CLUSTER_NAME="${CLUSTER_NAME:-paas-e2e}"
LIBVIRT_URI="${LIBVIRT_URI:-qemu:///system}"
LIBVIRT_POOL="${LIBVIRT_POOL:-default}"

E2E_NETWORK="${E2E_NETWORK:-${CLUSTER_NAME}}"
E2E_SUBNET="${E2E_SUBNET:-10.77.0.0/24}"
E2E_GATEWAY="${E2E_GATEWAY:-10.77.0.1}"

# name:role:ip:mac:memoryMB:vcpus:rootGB:dataGB
#
# Two workers is the minimum that exercises DRBD replication and pod
# anti-affinity honestly. replicated-3 therefore also places a replica on the
# control plane, which is why it carries a data disk too.
NODES=(
	"${CLUSTER_NAME}-cp-1:controlplane:10.77.0.11:52:54:00:77:00:0b:${CP_MEMORY:-2560}:${CP_VCPUS:-2}:12:20"
	"${CLUSTER_NAME}-w-1:worker:10.77.0.21:52:54:00:77:00:15:${WORKER_MEMORY:-3072}:${WORKER_VCPUS:-2}:12:20"
	"${CLUSTER_NAME}-w-2:worker:10.77.0.22:52:54:00:77:00:16:${WORKER_MEMORY:-3072}:${WORKER_VCPUS:-2}:12:20"
)
CONTROLPLANE_IP="10.77.0.11"
CLUSTER_ENDPOINT="https://${CONTROLPLANE_IP}:6443"

# Fields are read positionally because the MAC contains the separator.
node_field() {
	local spec="$1" idx="$2"
	local -a f
	IFS=':' read -r -a f <<<"$spec"
	case "$idx" in
	name) echo "${f[0]}" ;;
	role) echo "${f[1]}" ;;
	ip) echo "${f[2]}" ;;
	mac) echo "${f[3]}:${f[4]}:${f[5]}:${f[6]}:${f[7]}:${f[8]}" ;;
	memory) echo "${f[9]}" ;;
	vcpus) echo "${f[10]}" ;;
	root) echo "${f[11]}" ;;
	data) echo "${f[12]}" ;;
	esac
}

virsh_() { virsh -c "$LIBVIRT_URI" "$@"; }

preflight() {
	step "preflight"
	require_tools virsh virt-install qemu-img curl jq envsubst talosctl kubectl helm
	require_libvirt

	[[ -r /dev/kvm && -w /dev/kvm ]] ||
		die "/dev/kvm is not writable by $USER — add yourself to the 'kvm' group and re-login"

	local nested
	nested="$(cat /sys/module/kvm_intel/parameters/nested 2>/dev/null || cat /sys/module/kvm_amd/parameters/nested 2>/dev/null || echo N)"
	[[ "$nested" == Y || "$nested" == 1 ]] ||
		warn "nested virtualisation is off — phase 0 works, phase 5 (KubeVirt) will not"

	local want_mb=0 spec
	for spec in "${NODES[@]}"; do want_mb=$((want_mb + $(node_field "$spec" memory))); done
	local avail_mb
	avail_mb="$(awk '/MemAvailable/ {print int($2/1024)}' /proc/meminfo)"
	((avail_mb >= want_mb)) ||
		die "guests need ${want_mb} MB, host has ${avail_mb} MB available — lower CP_MEMORY/WORKER_MEMORY"
	log "memory: ${want_mb} MB requested, ${avail_mb} MB available"

	# The pool creates the qcow2 files, so they are owned by qemu and no
	# AppArmor or home-directory permission problem can reach them. Creating
	# them under $E2E_DIR instead fails on most distros.
	virsh_ pool-info "$LIBVIRT_POOL" >/dev/null 2>&1 || {
		log "creating libvirt storage pool '$LIBVIRT_POOL'"
		virsh_ pool-define-as "$LIBVIRT_POOL" dir --target /var/lib/libvirt/images
		virsh_ pool-autostart "$LIBVIRT_POOL"
	}
	virsh_ pool-start "$LIBVIRT_POOL" >/dev/null 2>&1 || true

	mkdir -p "${E2E_DIR}"/{config,images}
}

# The ID is a content hash, so an unchanged schematic re-resolves to the same
# one and this is free on the second run.
factory_schematic_id() {
	local cache="${E2E_DIR}/schematic-id"
	if [[ -s "$cache" ]]; then
		cat "$cache"
		return
	fi
	local id
	id="$(curl -fsSL --retry 3 -X POST --data-binary @"${REPO_ROOT}/hack/talos/schematic.yaml" \
		"${IMAGE_FACTORY}/schematics" | jq -r .id)"
	[[ -n "$id" && "$id" != null ]] || die "image factory did not return a schematic ID"
	printf '%s' "$id" >"$cache"
	printf '%s' "$id"
}

download_iso() {
	local id="$1"
	local iso="${E2E_DIR}/images/talos-${TALOS_VERSION}-${id:0:12}.iso"
	if [[ -s "$iso" ]]; then
		printf '%s' "$iso"
		return
	fi
	log "downloading Talos ${TALOS_VERSION} ISO"
	curl -fSL --retry 3 --progress-bar \
		-o "${iso}.part" "${IMAGE_FACTORY}/image/${id}/${TALOS_VERSION}/metal-amd64.iso"
	mv "${iso}.part" "$iso"
	printf '%s' "$iso"
}

# Into the pool so guests can read it regardless of where this repository sits.
upload_iso() {
	local iso="$1" vol
	vol="$(basename "$iso")"
	if virsh_ vol-info --pool "$LIBVIRT_POOL" "$vol" >/dev/null 2>&1; then
		virsh_ vol-path --pool "$LIBVIRT_POOL" "$vol"
		return
	fi
	log "uploading $vol into libvirt pool '$LIBVIRT_POOL'"
	virsh_ vol-create-as "$LIBVIRT_POOL" "$vol" "$(stat -c%s "$iso")" --format raw >/dev/null
	virsh_ vol-upload --pool "$LIBVIRT_POOL" "$vol" "$iso"
	virsh_ vol-path --pool "$LIBVIRT_POOL" "$vol"
}

create_network() {
	if virsh_ net-info "$E2E_NETWORK" >/dev/null 2>&1; then
		virsh_ net-start "$E2E_NETWORK" >/dev/null 2>&1 || true
		return
	fi
	step "creating libvirt network '$E2E_NETWORK' (${E2E_SUBNET})"

	# Static DHCP reservations. Talos boots from ISO into maintenance mode with
	# a DHCP address; without reservations that address is whatever dnsmasq
	# hands out, and the machine configs — which pin etcd and the API server
	# endpoint — cannot be rendered ahead of boot.
	local hosts="" spec
	for spec in "${NODES[@]}"; do
		hosts+="      <host mac='$(node_field "$spec" mac)' name='$(node_field "$spec" name)' ip='$(node_field "$spec" ip)'/>
"
	done

	cat >"${E2E_DIR}/network.xml" <<EOF
<network>
  <name>${E2E_NETWORK}</name>
  <forward mode='nat'/>
  <bridge name='virbr-paas' stp='on' delay='0'/>
  <mtu size='9000'/>
  <ip address='${E2E_GATEWAY}' netmask='255.255.255.0'>
    <dhcp>
      <range start='10.77.0.100' end='10.77.0.200'/>
${hosts}    </dhcp>
  </ip>
</network>
EOF
	virsh_ net-define "${E2E_DIR}/network.xml"
	virsh_ net-autostart "$E2E_NETWORK"
	virsh_ net-start "$E2E_NETWORK"
}

create_volume() {
	virsh_ vol-info --pool "$LIBVIRT_POOL" "$1" >/dev/null 2>&1 && return 0
	virsh_ vol-create-as "$LIBVIRT_POOL" "$1" "${2}G" --format qcow2 >/dev/null
}

create_domain() {
	local spec="$1" iso_path="$2"
	local name role mac mem vcpus
	name="$(node_field "$spec" name)"
	role="$(node_field "$spec" role)"
	mac="$(node_field "$spec" mac)"
	mem="$(node_field "$spec" memory)"
	vcpus="$(node_field "$spec" vcpus)"

	if virsh_ dominfo "$name" >/dev/null 2>&1; then
		virsh_ start "$name" >/dev/null 2>&1 || true
		return
	fi

	create_volume "${name}-root.qcow2" "$(node_field "$spec" root)"
	create_volume "${name}-data.qcow2" "$(node_field "$spec" data)"
	local root data
	root="$(virsh_ vol-path --pool "$LIBVIRT_POOL" "${name}-root.qcow2")"
	data="$(virsh_ vol-path --pool "$LIBVIRT_POOL" "${name}-data.qcow2")"

	log "defining domain '$name' (${role}, ${mem} MB, ${vcpus} vcpu)"
	# --boot hd,cdrom: the root disk is empty on first boot so firmware falls
	# through to the ISO and Talos comes up in maintenance mode. Once
	# apply-config has written the disk, the same order picks the installed
	# system with no XML edit and no ISO ejection.
	virt-install \
		--connect "$LIBVIRT_URI" \
		--name "$name" \
		--memory "$mem" \
		--vcpus "$vcpus" \
		--cpu host-passthrough \
		--machine q35 \
		--disk "path=${root},format=qcow2,bus=virtio,cache=unsafe" \
		--disk "path=${data},format=qcow2,bus=virtio,cache=unsafe" \
		--disk "path=${iso_path},device=cdrom,bus=sata,readonly=on" \
		--network "network=${E2E_NETWORK},mac=${mac},model=virtio" \
		--channel unix,target_type=virtio,name=org.qemu.guest_agent.0 \
		--boot hd,cdrom \
		--graphics none \
		--console pty,target_type=serial \
		--osinfo detect=off,name=generic \
		--noautoconsole >/dev/null
}

wait_maintenance() {
	step "waiting for nodes to reach Talos maintenance mode"
	local spec ip
	for spec in "${NODES[@]}"; do
		ip="$(node_field "$spec" ip)"
		retry 120 5 "node ${ip} maintenance mode" -- \
			talosctl --nodes "$ip" --endpoints "$ip" --insecure version --short >/dev/null
		log "node ${ip} is up"
	done
}

gen_configs() {
	step "rendering Talos machine configs"
	local id installer
	id="$(factory_schematic_id)"
	installer="factory.talos.dev/metal-installer/${id}:${TALOS_VERSION}"

	# Regenerating secrets would invalidate a live cluster's PKI, so `up` on an
	# existing cluster must reuse them.
	[[ -s "${E2E_DIR}/config/secrets.yaml" ]] ||
		talosctl gen secrets --output-file "${E2E_DIR}/config/secrets.yaml"

	local common="${E2E_DIR}/config/common.patch.yaml"
	TALOS_INSTALLER_IMAGE="$installer" \
		envsubst <"${REPO_ROOT}/hack/talos/common.patch.yaml" >"$common"
	local cp_patch="${E2E_DIR}/config/controlplane.patch.yaml"
	E2E_SUBNET="$E2E_SUBNET" envsubst <"${REPO_ROOT}/hack/talos/controlplane.patch.yaml" >"$cp_patch"

	talosctl gen config "$CLUSTER_NAME" "$CLUSTER_ENDPOINT" \
		--with-secrets "${E2E_DIR}/config/secrets.yaml" \
		--kubernetes-version "${KUBERNETES_VERSION#v}" \
		--config-patch "@${common}" \
		--config-patch-control-plane "@${cp_patch}" \
		--config-patch-worker "@${REPO_ROOT}/hack/talos/worker.patch.yaml" \
		--output "${E2E_DIR}/config" \
		--force
	log "installer image: ${installer}"
}

apply_configs() {
	step "applying machine configs"
	local spec ip role name
	for spec in "${NODES[@]}"; do
		ip="$(node_field "$spec" ip)"
		role="$(node_field "$spec" role)"
		name="$(node_field "$spec" name)"

		# apply-config --insecure only works in maintenance mode, and re-running
		# `up` against a live cluster must be a no-op.
		if talosctl --talosconfig "${E2E_DIR}/config/talosconfig" \
			--nodes "$ip" --endpoints "$ip" version --short >/dev/null 2>&1; then
			log "node ${name} is already configured"
			continue
		fi

		log "applying ${role} config to ${name} (${ip})"
		retry 30 5 "apply-config to ${ip}" -- \
			talosctl apply-config --insecure --nodes "$ip" \
			--file "${E2E_DIR}/config/${role}.yaml"
	done

	cp "${E2E_DIR}/config/talosconfig" "$TALOSCONFIG"
	talosctl --talosconfig "$TALOSCONFIG" config endpoint "$CONTROLPLANE_IP"
	talosctl --talosconfig "$TALOSCONFIG" config node "$CONTROLPLANE_IP"
}

bootstrap_etcd() {
	step "bootstrapping etcd"
	# Bootstrap is one-shot: a second call returns AlreadyExists, which is
	# success here but a non-zero exit.
	if talosctl --talosconfig "$TALOSCONFIG" --nodes "$CONTROLPLANE_IP" etcd status >/dev/null 2>&1; then
		log "etcd is already bootstrapped"
		return
	fi
	retry 60 10 "talosctl bootstrap" -- \
		talosctl --talosconfig "$TALOSCONFIG" --nodes "$CONTROLPLANE_IP" bootstrap
}

fetch_kubeconfig() {
	step "fetching kubeconfig"
	retry 60 10 "talosctl kubeconfig" -- \
		talosctl --talosconfig "$TALOSCONFIG" --nodes "$CONTROLPLANE_IP" \
		kubeconfig --force "$KUBECONFIG"
	retry 60 10 "kube-apiserver reachable" -- kubectl version -o json
	log "KUBECONFIG=${KUBECONFIG}"
}

wait_nodes_registered() {
	step "waiting for all ${#NODES[@]} nodes to register"
	retry 90 10 "nodes to register" -- bash -c \
		"test \"\$(kubectl get nodes --no-headers 2>/dev/null | wc -l)\" -eq ${#NODES[@]}"
	kubectl get nodes -o wide | indent
}

install_cilium() {
	step "installing Cilium ${CILIUM_VERSION}"

	# Before the chart, or the Cilium operator CrashLoops on a missing
	# GatewayClass type and the failure reads as an unrelated RBAC problem.
	retry 5 5 "gateway api crds" -- kubectl apply --server-side -f \
		"https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml"

	helm repo add cilium https://helm.cilium.io/ >/dev/null 2>&1 || true
	helm repo update cilium >/dev/null
	helm upgrade --install cilium cilium/cilium \
		--version "$CILIUM_VERSION" \
		--namespace kube-system \
		--values "${REPO_ROOT}/hack/manifests/cilium-values.yaml" \
		--wait --timeout 10m

	step "waiting for all nodes to become Ready"
	retry 60 10 "nodes Ready" -- kubectl wait --for=condition=Ready nodes --all --timeout=30s
	kubectl get nodes | indent
}

install_piraeus() {
	step "installing Piraeus ${PIRAEUS_VERSION}"
	retry 5 10 "piraeus operator" -- kubectl apply --server-side -k \
		"https://github.com/piraeusdatastore/piraeus-operator//config/default?ref=${PIRAEUS_VERSION}"
	retry 30 10 "piraeus operator ready" -- kubectl wait pod --for=condition=Ready \
		-n piraeus-datastore -l app.kubernetes.io/component=piraeus-operator --timeout=60s

	kubectl apply --server-side -f - <<'EOF'
apiVersion: piraeus.io/v1
kind: LinstorCluster
metadata:
  name: linstorcluster
spec: {}
EOF
	kubectl apply --server-side -f "${REPO_ROOT}/hack/manifests/piraeus-satellite.yaml"

	step "waiting for LINSTOR satellites"
	retry 60 10 "linstor satellites ready" -- kubectl wait pod \
		-n piraeus-datastore -l app.kubernetes.io/component=linstor-satellite \
		--for=condition=Ready --timeout=60s

	# The pool is created asynchronously once the LVM thin pool exists. Binding
	# a PVC before it appears fails with a scheduling error that blames the CSI
	# driver rather than the pool.
	step "waiting for storage pools on every node"
	# shellcheck disable=SC2016 # must expand in the inner shell, on each retry
	retry 60 10 "linstor storage pools" -- bash -c '
		pod=$(kubectl get pods -n piraeus-datastore -l app.kubernetes.io/component=linstor-controller -o name | head -1)
		test -n "$pod" || exit 1
		kubectl exec -n piraeus-datastore "$pod" -- linstor --no-color storage-pool list |
			grep -c " pool " | grep -qE "^[3-9]"'

	kubectl apply --server-side -f "${REPO_ROOT}/hack/manifests/storageclasses.yaml"
	kubectl get storageclass | indent
}

cmd_up() {
	local started=$SECONDS
	preflight

	local id iso_path
	id="$(factory_schematic_id)"
	log "schematic ${id}"
	iso_path="$(upload_iso "$(download_iso "$id")")"

	create_network
	local spec
	for spec in "${NODES[@]}"; do create_domain "$spec" "$iso_path"; done

	wait_maintenance
	gen_configs
	apply_configs
	bootstrap_etcd
	fetch_kubeconfig
	wait_nodes_registered
	install_cilium
	install_piraeus

	step "cluster up in $((SECONDS - started))s"
	echo
	echo "  export KUBECONFIG=${KUBECONFIG}"
	echo "  export TALOSCONFIG=${TALOSCONFIG}"
	echo "  make e2e"
	echo "  make cluster-down"
}

cmd_down() {
	step "destroying ${CLUSTER_NAME}"
	require_tools virsh
	require_libvirt

	local spec name v
	for spec in "${NODES[@]}"; do
		name="$(node_field "$spec" name)"
		if virsh_ dominfo "$name" >/dev/null 2>&1; then
			log "removing domain $name"
			virsh_ destroy "$name" >/dev/null 2>&1 || true
			virsh_ undefine "$name" --nvram >/dev/null 2>&1 ||
				virsh_ undefine "$name" >/dev/null 2>&1 || true
		fi
		# Explicit, because --remove-all-storage would take the shared ISO too.
		for v in "${name}-root.qcow2" "${name}-data.qcow2"; do
			virsh_ vol-delete --pool "$LIBVIRT_POOL" "$v" >/dev/null 2>&1 || true
		done
	done

	local leaked
	leaked="$(virsh_ list --all --name | grep -E "^${CLUSTER_NAME}-" || true)"
	if [[ -n "$leaked" ]]; then
		warn "removing leaked domains: $(echo "$leaked" | tr '\n' ' ')"
		while read -r name; do
			[[ -n "$name" ]] || continue
			virsh_ destroy "$name" >/dev/null 2>&1 || true
			virsh_ undefine "$name" --remove-all-storage >/dev/null 2>&1 || true
		done <<<"$leaked"
	fi

	if virsh_ net-info "$E2E_NETWORK" >/dev/null 2>&1; then
		log "removing network $E2E_NETWORK"
		virsh_ net-destroy "$E2E_NETWORK" >/dev/null 2>&1 || true
		virsh_ net-undefine "$E2E_NETWORK" >/dev/null 2>&1 || true
	fi

	# The ISO and schematic ID stay: re-downloading 900 MB per test run is the
	# difference between a usable inner loop and an unusable one.
	rm -rf "${E2E_DIR}/config" "$KUBECONFIG" "$TALOSCONFIG"
	step "down"
}

cmd_status() {
	require_tools virsh
	echo "== domains =="
	virsh_ list --all | grep -E "Id|${CLUSTER_NAME}|^-" || true
	echo
	if [[ ! -s "$KUBECONFIG" ]]; then
		echo "no kubeconfig at $KUBECONFIG — cluster is not up"
		return 0
	fi
	echo "== nodes =="
	kubectl get nodes -o wide 2>&1 | indent || true
	echo
	echo "== storage =="
	kubectl get storageclass 2>&1 | indent || true
	local ctrl
	ctrl="$(kubectl get pods -n piraeus-datastore -l app.kubernetes.io/component=linstor-controller -o name 2>/dev/null | head -1)"
	if [[ -n "$ctrl" ]]; then
		kubectl exec -n piraeus-datastore "$ctrl" -- linstor --no-color storage-pool list 2>&1 | indent || true
		kubectl exec -n piraeus-datastore "$ctrl" -- linstor --no-color resource list 2>&1 | indent || true
	fi
}

# Read by the Go suite instead of duplicating the node table.
cmd_nodes() {
	local spec
	for spec in "${NODES[@]}"; do
		printf '%s\t%s\t%s\n' "$(node_field "$spec" name)" "$(node_field "$spec" role)" "$(node_field "$spec" ip)"
	done
}

case "${1:-}" in
up) cmd_up ;;
down) cmd_down ;;
status) cmd_status ;;
nodes) cmd_nodes ;;
kill-node)
	# Unclean power-off, which is the failure DRBD has to survive. A graceful
	# shutdown would let the kubelet drain and would test nothing.
	require_libvirt
	virsh_ destroy "${2:?usage: e2e.sh kill-node <domain>}"
	;;
start-node)
	require_libvirt
	virsh_ start "${2:?usage: e2e.sh start-node <domain>}"
	;;
*)
	sed -n '2,13p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
	exit 2
	;;
esac
