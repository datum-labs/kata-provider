#!/usr/bin/env bash
# Shared settings for the end-to-end environment.
#
# Sourced by every script under hack/e2e. Nothing here talks to a cluster; it
# only resolves which tier is being run and where its Docker daemon and
# kubeconfig live.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# The tier decides what actually executes an instance:
#
#   runc  the portable tier. A RuntimeClass named as the provider expects, but
#         handled by runc. No hardware virtualization, so it runs on any laptop
#         and on a GitHub runner. It proves everything the provider owns —
#         claiming, Pod shape, status, suspend/resume, teardown — because none
#         of that depends on which runtime the kubelet hands the Pod to.
#
#   kata  the real tier. Kata Containers on a node with KVM, so an instance
#         boots its own kernel. It runs the portable suites unchanged and adds
#         the ones that only mean something behind a hypervisor.
E2E_TIER="${E2E_TIER:-runc}"
case "${E2E_TIER}" in
runc | kata) ;;
*)
	echo "E2E_TIER must be 'runc' or 'kata', got '${E2E_TIER}'" >&2
	exit 1
	;;
esac

# Separate clusters per tier. They differ in how the node is built (the Kata
# tier needs /dev/kvm passed in) and in what the RuntimeClass resolves to, so
# one cannot be reused as the other.
if [[ "${E2E_TIER}" == "kata" ]]; then
	E2E_CLUSTER="${E2E_CLUSTER:-kata}"
	E2E_KIND_CONFIG="${REPO_ROOT}/hack/e2e/kind-kata.yaml"
else
	E2E_CLUSTER="${E2E_CLUSTER:-kata-provider-runc}"
	E2E_KIND_CONFIG="${REPO_ROOT}/hack/e2e/kind-runc.yaml"
fi

E2E_DIR="${REPO_ROOT}/tmp/e2e"
E2E_KUBECONFIG="${E2E_DIR}/${E2E_CLUSTER}.kubeconfig"

# Built locally and side-loaded, so a run never depends on a registry and always
# tests the working tree rather than whatever was last published.
E2E_IMAGE="${E2E_IMAGE:-ghcr.io/datum-labs/kata-provider:e2e}"

E2E_NAMESPACE="${E2E_NAMESPACE:-kata-provider-system}"

# The Kubernetes RuntimeClass instance Pods name. Identical in both tiers on
# purpose: the provider's configuration, and therefore every assertion about the
# Pod it builds, is the same whichever runtime is behind the name. Only the
# handler the object resolves to changes.
E2E_RUNTIME_CLASS="${E2E_RUNTIME_CLASS:-kata-qemu}"

# On macOS the Kata tier needs a Linux VM that exposes nested virtualization,
# which is a dedicated colima profile — the developer's default profile is never
# touched, and neither is their `docker context`: this points the Docker client
# at the right daemon through the environment for the duration of a command,
# and leaves the client's own configuration alone.
if [[ -z "${DOCKER_HOST:-}" && -S "${HOME}/.colima/kata/docker.sock" ]]; then
	export DOCKER_HOST="unix://${HOME}/.colima/kata/docker.sock"
fi

# The compute CRDs come from the exact module version in go.mod rather than from
# a branch of the compute repository. The provider is compiled against that
# version's Go types, so resolving the schema any other way lets the cluster
# accept an Instance the binary cannot represent — or reject one it can.
compute_crd_dir() {
	local version
	version="$(cd "${REPO_ROOT}" && go list -m -f '{{.Version}}' go.datum.net/compute)"
	echo "$(go env GOMODCACHE)/go.datum.net/compute@${version}/config/base/crd"
}

KUSTOMIZE="${KUSTOMIZE:-${REPO_ROOT}/bin/kustomize}"
KUBECTL="${KUBECTL:-kubectl}"

kctl() {
	"${KUBECTL}" --kubeconfig "${E2E_KUBECONFIG}" "$@"
}

log() {
	echo "▸ $*"
}
