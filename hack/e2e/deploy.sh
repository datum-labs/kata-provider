#!/usr/bin/env bash
# Build the provider from the working tree and deploy it into the tier's
# cluster.
#
# The image is built and side-loaded rather than pulled: a run must test the
# code in front of the developer, not whatever was last published, and must not
# depend on a registry.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

log "building ${E2E_IMAGE}"
docker build -t "${E2E_IMAGE}" "${REPO_ROOT}"

log "loading ${E2E_IMAGE} into ${E2E_CLUSTER}"
kind load docker-image "${E2E_IMAGE}" --name "${E2E_CLUSTER}"

log "applying test/e2e/deploy"
"${KUSTOMIZE}" build "${REPO_ROOT}/test/e2e/deploy" | kctl apply --server-side -f - >/dev/null

# A new image under an unchanged tag does not restart the Deployment on its own.
kctl -n "${E2E_NAMESPACE}" rollout restart deployment/kata-provider >/dev/null
kctl -n "${E2E_NAMESPACE}" rollout status deployment/kata-provider --timeout=5m

log "provider running in ${E2E_NAMESPACE}"
