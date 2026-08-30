#!/usr/bin/env bash
# Two fixes that the Kata tier needs on every freshly built node.
#
# Neither fix works around anything in this repository. Both are properties of
# running a hypervisor inside a container inside a virtual machine. Both are
# lost when the component that owns them is rebuilt: the first whenever
# kata-deploy reinstalls its configuration, and the second on every node
# restart. This script therefore reapplies both, idempotently, every time the
# environment comes up, rather than an operator applying them once by hand.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

NODE="${E2E_CLUSTER}-control-plane"

log "applying Kata node fixes to ${NODE}"

# ── 1. No performance monitoring unit under nested virtualization ───────────
#
# Kata's arm64 defaults ask QEMU for cpu_features = "pmu=off", where PMU is the
# processor's performance monitoring unit. A guest CPU running under nested
# virtualization does not expose that property. QEMU then exits with
# "Property 'host-arm-cpu.pmu' not found", and the instance never gets past
# ContainerCreating. The shim log holds the reason, and the Pod does not. Asking
# for no CPU features at all boots the guest, and no test in this suite depends
# on guest performance counters.
docker exec "${NODE}" sh -c '
  set -e
  # Patch every hypervisor configuration that kata-deploy installed, not only
  # the one that the default handler reads. The debug handler ships its own
  # copy.
  configs=$(find /opt/kata/share/defaults/kata-containers -name "configuration-qemu.toml" -type f)
  if [ -z "${configs}" ]; then
    echo "Kata configuration not present yet on the node" >&2
    exit 1
  fi
  for config in ${configs}; do
    sed -i "s/^cpu_features = .*/cpu_features = \"\"/" "${config}"
    grep -q "^cpu_features = \"\"" "${config}"
  done
'
log "  cpu_features cleared in every configuration-qemu.toml on the node"

# ── 2. /dev/shm is 64 MB inside a container ────────────────────────────────
#
# Kata backs the guest's RAM with memory-backend-file on /dev/shm. A container's
# default 64 MB of shared memory cannot hold a guest. QEMU then fails the boot
# with "kvm run failed Bad address", which reads like a fault in KVM, the Linux
# Kernel-based Virtual Machine interface, rather than a sizing problem. The node
# needs room for every guest that the suites run concurrently, at the instance
# type's 2 GiB each.
docker exec "${NODE}" sh -c 'mount -o remount,size=8G /dev/shm'
log "  /dev/shm resized to $(docker exec "${NODE}" sh -c "df -h /dev/shm | awk 'NR==2{print \$2}'")"
