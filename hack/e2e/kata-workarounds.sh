#!/usr/bin/env bash
# Two fixes the Kata tier needs on every freshly built node.
#
# Neither is a workaround for anything in this repository: both are properties
# of running a hypervisor inside a container inside a VM. Both are lost when the
# thing that owns them is rebuilt — the first whenever kata-deploy reinstalls
# its configuration, the second on every node restart — so they are reapplied,
# idempotently, every time the environment is brought up rather than done once
# by hand.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

NODE="${E2E_CLUSTER}-control-plane"

log "applying Kata node fixes to ${NODE}"

# ── 1. No performance monitoring unit under nested virtualization ───────────
#
# Kata's arm64 defaults ask QEMU for cpu_features = "pmu=off". A guest CPU
# running on nested virtualization does not expose that property, so QEMU exits
# with "Property 'host-arm-cpu.pmu' not found" and the instance never gets past
# ContainerCreating — with the reason buried in the shim log rather than on the
# Pod. Asking for no CPU features at all is what boots; nothing in this test
# suite depends on guest performance counters.
docker exec "${NODE}" sh -c '
  set -e
  # Every hypervisor configuration kata-deploy installed, not just the one the
  # default handler reads: the debug handler ships its own copy.
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
# default 64 MB shared memory cannot hold a guest, and QEMU fails the boot with
# "kvm run failed Bad address" — which reads like a KVM fault rather than a
# sizing problem. The node needs enough room for the guests the suites run
# concurrently, at the instance type's 2 GiB each.
docker exec "${NODE}" sh -c 'mount -o remount,size=8G /dev/shm'
log "  /dev/shm resized to $(docker exec "${NODE}" sh -c "df -h /dev/shm | awk 'NR==2{print \$2}'")"
