#!/usr/bin/env bash
# Build the provider from the working tree and deploy it into the tier's
# cluster.
#
# The script builds and side-loads the image rather than pulling it. A run must
# test the code in front of the developer, not the last published image, and
# must not depend on a registry.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

log "building ${E2E_IMAGE}"
docker build -t "${E2E_IMAGE}" "${REPO_ROOT}"

log "loading ${E2E_IMAGE} into ${E2E_CLUSTER}"
kind load docker-image "${E2E_IMAGE}" --name "${E2E_CLUSTER}"

log "applying test/e2e/deploy with runtimeHandler ${E2E_RUNTIME_CLASS}"
# The tier picks the hypervisor, so the provider has to be configured for the
# handler the tier installed. Naming a handler that no RuntimeClass resolves to
# leaves every instance Pod rejected at admission, which is a confusing way to
# discover an architecture mismatch. The substitution matches whichever handler
# the shipped config names, so the environment does not depend on that value.
"${KUSTOMIZE}" build "${REPO_ROOT}/test/e2e/deploy" |
	sed -E "s|runtimeHandler: kata-[a-z-]+|runtimeHandler: ${E2E_RUNTIME_CLASS}|" |
	kctl apply --server-side -f - >/dev/null

# A new image under an unchanged tag does not restart the Deployment on its own.
kctl -n "${E2E_NAMESPACE}" rollout restart deployment/kata-provider >/dev/null
kctl -n "${E2E_NAMESPACE}" rollout status deployment/kata-provider --timeout=5m

log "provider running in ${E2E_NAMESPACE}"
