#!/usr/bin/env bash
# Delete the selected tier's cluster and its artefacts.
#
# Not part of a test run: the environment is reused between runs on purpose, and
# tearing it down is an explicit choice.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

log "deleting cluster ${E2E_CLUSTER}"
kind delete cluster --name "${E2E_CLUSTER}" --kubeconfig "${E2E_KUBECONFIG}" || true
rm -f "${E2E_KUBECONFIG}"
