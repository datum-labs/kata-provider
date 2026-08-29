#!/usr/bin/env bash
# Run the chainsaw suites for the selected tier.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

CHAINSAW="${CHAINSAW:-${REPO_ROOT}/bin/chainsaw}"

if [[ ! -f "${E2E_KUBECONFIG}" ]]; then
	echo "no kubeconfig at ${E2E_KUBECONFIG} — run 'task e2e:up' first" >&2
	exit 1
fi

# Tier selection is a label query, so the shared suites exist once and run in
# both tiers rather than being copied per tier.
#
#   portable  everything the provider itself decides, which is runtime
#             independent and therefore assertable without a hypervisor.
#   kata      the assertions that need a real guest. Excluded from the portable
#             tier rather than rewritten to pass there.
if [[ "${E2E_TIER}" == "runc" ]]; then
	SELECTOR=(--selector tier=portable)
	# The portable tier starts an instance in under a second, so suites overlap
	# cheaply.
	PARALLEL="${E2E_PARALLEL:-4}"
else
	SELECTOR=()
	# Each Kata instance holds 2 GiB of guest RAM out of the node's /dev/shm for
	# its lifetime. Running the suites one at a time keeps the node's memory
	# from deciding whether a test passes.
	PARALLEL="${E2E_PARALLEL:-1}"
fi

log "running tier=${E2E_TIER} suites (parallel ${PARALLEL})"

cd "${REPO_ROOT}"
KUBECONFIG="${E2E_KUBECONFIG}" "${CHAINSAW}" test \
	--config test/e2e/chainsaw-config.yaml \
	--parallel "${PARALLEL}" \
	${SELECTOR[@]+"${SELECTOR[@]}"} \
	"$@" \
	test/e2e/
