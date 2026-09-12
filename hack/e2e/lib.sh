#!/usr/bin/env bash
# Shared settings for the end-to-end environment.
#
# Every script under hack/e2e sources this file. Nothing here contacts a
# cluster. The file resolves which tier runs, and where that tier's Docker
# daemon and kubeconfig live.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# The tier decides what executes an instance.
#
#   runc  The portable tier. A RuntimeClass carries the name the provider
#         expects, but runc handles it. The tier needs no hardware
#         virtualization, so it runs on any laptop and on a GitHub runner. It
#         proves everything the provider owns: claiming, Pod shape, status,
#         suspend and resume, and teardown. None of those depend on the runtime
#         that the kubelet hands the Pod to.
#
#   kata  The real tier. Kata Containers runs on a node with KVM, the Linux
#         Kernel-based Virtual Machine interface, so an instance boots its own
#         kernel. The tier runs the portable suites unchanged. It adds the
#         suites that only have meaning behind a hypervisor.
E2E_TIER="${E2E_TIER:-runc}"
case "${E2E_TIER}" in
runc | kata) ;;
*)
	echo "E2E_TIER must be 'runc' or 'kata', got '${E2E_TIER}'" >&2
	exit 1
	;;
esac

# Each tier gets its own cluster. The tiers differ in how the node is built and
# in what the RuntimeClass resolves to, so one cluster cannot serve as the
# other. The Kata tier's node needs the host's /dev/kvm device passed in.
if [[ "${E2E_TIER}" == "kata" ]]; then
	E2E_CLUSTER="${E2E_CLUSTER:-kata}"
	E2E_KIND_CONFIG="${REPO_ROOT}/hack/e2e/kind-kata.yaml"
else
	E2E_CLUSTER="${E2E_CLUSTER:-kata-provider-runc}"
	E2E_KIND_CONFIG="${REPO_ROOT}/hack/e2e/kind-runc.yaml"
fi

E2E_DIR="${REPO_ROOT}/tmp/e2e"
E2E_KUBECONFIG="${E2E_DIR}/${E2E_CLUSTER}.kubeconfig"

# The scripts build this image locally and side-load it. A run therefore never
# depends on a registry, and always tests the working tree rather than the last
# published image.
E2E_IMAGE="${E2E_IMAGE:-ghcr.io/datum-labs/kata-provider:e2e}"

E2E_NAMESPACE="${E2E_NAMESPACE:-kata-provider-system}"

# Which Kata hypervisor the real tier installs.
#
# clh is Cloud Hypervisor, the hypervisor the provider defaults to, so the
# suites test what a cell actually runs. Kata 4.x builds the Cloud Hypervisor
# shim for x86_64 only, so an arm64 host runs the tier with `E2E_KATA_SHIM=qemu`
# — the same override an arm64 cell makes in the provider's configuration.
E2E_KATA_SHIM="${E2E_KATA_SHIM:-clh}"

# The Kubernetes RuntimeClass that instance Pods name. kata-deploy names each
# class after the shim it installs, and both tiers use that name deliberately.
# The provider's configuration stays the same whichever runtime sits behind the
# name, and so does every assertion about the Pod the provider builds. Only the
# handler that the object resolves to changes.
E2E_RUNTIME_CLASS="${E2E_RUNTIME_CLASS:-kata-${E2E_KATA_SHIM}}"

# On macOS, the Kata tier needs a Linux virtual machine that exposes nested
# virtualization. A dedicated colima profile provides that virtual machine. The
# assignment below points the Docker client at the profile's daemon through the
# environment, for the duration of one command. It leaves the developer's
# default colima profile and their `docker context` configuration alone.
if [[ -z "${DOCKER_HOST:-}" && -S "${HOME}/.colima/kata/docker.sock" ]]; then
	export DOCKER_HOST="unix://${HOME}/.colima/kata/docker.sock"
fi

# The compute CustomResourceDefinitions (CRDs) come from the exact module
# version in go.mod, rather than from a branch of the compute repository. The
# provider compiles against that version's Go types. Any other source lets the
# cluster accept an Instance that the binary cannot represent, or reject one
# that it can.
#
# The module has to be in the local cache before its directory exists, and a
# build of this repository alone does not put it there. Asking go for the
# directory without downloading first yields nothing on any machine that has
# not already fetched this exact version, which is every fresh continuous
# integration runner after the pin moves.
compute_crd_dir() {
	local dir
	dir="$(cd "${REPO_ROOT}" &&
		go mod download go.datum.net/compute &&
		go list -m -f '{{.Dir}}' go.datum.net/compute)"
	echo "${dir}/config/base/crd"
}

KUSTOMIZE="${KUSTOMIZE:-${REPO_ROOT}/bin/kustomize}"
KUBECTL="${KUBECTL:-kubectl}"

kctl() {
	"${KUBECTL}" --kubeconfig "${E2E_KUBECONFIG}" "$@"
}

log() {
	echo "▸ $*"
}
