#!/usr/bin/env bash
# Run the chainsaw suites for the selected tier.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

CHAINSAW="${CHAINSAW:-${REPO_ROOT}/bin/chainsaw}"

if [[ ! -f "${E2E_KUBECONFIG}" ]]; then
	echo "no kubeconfig at ${E2E_KUBECONFIG} — run 'task e2e:up' first" >&2
	exit 1
fi

# Tier selection is a label query. The shared suites therefore exist once and
# run in both tiers, rather than being copied per tier.
#
#   portable  Everything the provider itself decides. Those decisions are
#             runtime independent, so a suite asserts them without a hypervisor.
#   kata      The assertions that need a real guest. The portable tier excludes
#             these suites rather than rewriting them to pass there.
if [[ "${E2E_TIER}" == "runc" ]]; then
	SELECTOR=(--selector tier=portable)
	# The portable tier starts an instance in under a second, so overlapping
	# suites costs little.
	PARALLEL="${E2E_PARALLEL:-4}"
else
	SELECTOR=()
	# Each Kata instance holds 2 GiB of guest RAM in the node's /dev/shm for its
	# lifetime. Running one suite at a time keeps the node's memory from deciding
	# whether a test passes.
	PARALLEL="${E2E_PARALLEL:-1}"
fi

log "running tier=${E2E_TIER} suites against ${E2E_RUNTIME_CLASS} (parallel ${PARALLEL})"

cd "${REPO_ROOT}"
# The RuntimeClass name follows the hypervisor the tier installed, so the suites
# take it as a value rather than fixing one name in every manifest.
KUBECONFIG="${E2E_KUBECONFIG}" "${CHAINSAW}" test \
	--config test/e2e/chainsaw-config.yaml \
	--parallel "${PARALLEL}" \
	--set "runtimeClass=${E2E_RUNTIME_CLASS}" \
	${SELECTOR[@]+"${SELECTOR[@]}"} \
	"$@" \
	test/e2e/
