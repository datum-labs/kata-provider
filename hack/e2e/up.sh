#!/usr/bin/env bash
# Bring up the end-to-end environment for the selected tier.
#
# Every step is create-or-reuse. Re-running this against a live environment is
# the normal case — it is what keeps the edit/run loop measured in seconds
# rather than in cluster rebuilds — so nothing here deletes or recreates
# something that is already correct.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

KATA_CHART="${KATA_CHART:-oci://quay.io/kata-containers/kata-deploy-charts/kata-deploy}"
KATA_VERSION="${KATA_VERSION:-4.1.0}"

log "tier=${E2E_TIER} cluster=${E2E_CLUSTER}"

mkdir -p "${E2E_DIR}"

# ── Cluster ────────────────────────────────────────────────────────────────
if kind get clusters 2>/dev/null | grep -qx "${E2E_CLUSTER}"; then
	log "reusing existing cluster ${E2E_CLUSTER}"
else
	log "creating cluster ${E2E_CLUSTER}"
	# --kubeconfig keeps the cluster out of the developer's ~/.kube/config: an
	# e2e run must not repoint the context they are working in.
	kind create cluster --config "${E2E_KIND_CONFIG}" --kubeconfig "${E2E_KUBECONFIG}"
fi
kind export kubeconfig --name "${E2E_CLUSTER}" --kubeconfig "${E2E_KUBECONFIG}" >/dev/null

# The node label is what instance Pods select on. kata-deploy applies it to
# every node it installs the runtime on; the portable tier has no kata-deploy,
# so it is applied here for both tiers to keep the two environments identical
# from the provider's point of view.
kctl label node --all katacontainers.io/kata-runtime=true --overwrite >/dev/null

# ── Runtime ────────────────────────────────────────────────────────────────
case "${E2E_TIER}" in
runc)
	log "installing the runc-handled RuntimeClass ${E2E_RUNTIME_CLASS}"
	kctl apply -f "${REPO_ROOT}/hack/e2e/runtimeclass-runc.yaml" >/dev/null
	;;
kata)
	log "installing kata-deploy ${KATA_VERSION}"
	helm --kubeconfig "${E2E_KUBECONFIG}" upgrade --install kata-deploy \
		"${KATA_CHART}" --version "${KATA_VERSION}" \
		--namespace kube-system \
		--values "${REPO_ROOT}/hack/e2e/kata-values.yaml" \
		--wait --timeout 10m >/dev/null

	# On a cluster that has just been created the DaemonSet exists before any of
	# its Pods do, and neither readiness check means anything in that window: a
	# DaemonSet with nothing scheduled yet is trivially "successfully rolled
	# out", and `kubectl wait -l` does not wait for a Pod to appear — it fails
	# outright with "no matching resources found". Waiting for the DaemonSet to
	# want a Pod first is what makes both checks real. Reusing a cluster hides
	# this entirely, because the Pod is already there.
	for _ in $(seq 60); do
		desired="$(kctl -n kube-system get daemonset kata-deploy \
			-o jsonpath='{.status.desiredNumberScheduled}' 2>/dev/null || true)"
		if [[ -n "${desired}" && "${desired}" -gt 0 ]]; then
			break
		fi
		sleep 2
	done

	# kata-deploy reports ready before it has finished writing the runtime onto
	# the node, and the node fixes below edit files it installs.
	kctl -n kube-system rollout status daemonset/kata-deploy --timeout=10m
	kctl wait --for=condition=Ready pod -n kube-system -l name=kata-deploy --timeout=5m

	"${REPO_ROOT}/hack/e2e/kata-workarounds.sh"
	;;
esac

# ── Compute CRDs ───────────────────────────────────────────────────────────
CRD_DIR="$(compute_crd_dir)"
log "installing compute CRDs from ${CRD_DIR}"
"${KUSTOMIZE}" build "${CRD_DIR}" | kctl apply --server-side -f - >/dev/null
kctl wait --for=condition=Established crd/instances.compute.datumapis.com --timeout=60s >/dev/null

log "environment ready — kubeconfig ${E2E_KUBECONFIG}"
