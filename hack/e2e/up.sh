#!/usr/bin/env bash
# Bring up the end-to-end environment for the selected tier.
#
# Every step creates or reuses. Re-running the script against a live environment
# is the normal case, and it keeps the edit and run loop measured in seconds
# rather than in cluster rebuilds. No step deletes or recreates something that
# is already correct.

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
	# The --kubeconfig flag keeps the cluster out of the developer's
	# ~/.kube/config. An end-to-end run must not repoint the context that the
	# developer is working in.
	kind create cluster --config "${E2E_KIND_CONFIG}" --kubeconfig "${E2E_KUBECONFIG}"
fi
kind export kubeconfig --name "${E2E_CLUSTER}" --kubeconfig "${E2E_KUBECONFIG}" >/dev/null

# Instance Pods select on the node label. kata-deploy applies the label to every
# node where it installs the runtime. The portable tier runs no kata-deploy, so
# this command applies the label in both tiers. Both environments then look
# identical from the provider's point of view.
kctl label node --all katacontainers.io/kata-runtime=true --overwrite >/dev/null

# ── Runtime ────────────────────────────────────────────────────────────────
case "${E2E_TIER}" in
runc)
	log "installing the runc-handled RuntimeClass ${E2E_RUNTIME_CLASS}"
	# The object carries whatever name the tier is running under, so the
	# manifest names it here rather than fixing one name in the file.
	sed "s|@RUNTIME_CLASS@|${E2E_RUNTIME_CLASS}|" \
		"${REPO_ROOT}/hack/e2e/runtimeclass-runc.yaml" | kctl apply -f - >/dev/null
	;;
kata)
	log "installing kata-deploy ${KATA_VERSION} with the ${E2E_KATA_SHIM} shim"
	# The shim selection sits here rather than in the values file, so one
	# variable decides the hypervisor, the RuntimeClass name the suites assert
	# on, and the handler the provider is configured with.
	helm --kubeconfig "${E2E_KUBECONFIG}" upgrade --install kata-deploy \
		"${KATA_CHART}" --version "${KATA_VERSION}" \
		--namespace kube-system \
		--values "${REPO_ROOT}/hack/e2e/kata-values.yaml" \
		--set "shims.${E2E_KATA_SHIM}.enabled=true" \
		--set "defaultShim.amd64=${E2E_KATA_SHIM}" \
		--set "defaultShim.arm64=${E2E_KATA_SHIM}" \
		--wait --timeout 10m >/dev/null

	# On a newly created cluster, the DaemonSet exists before any of its Pods do,
	# and neither readiness check means anything in that window. A DaemonSet with
	# nothing scheduled has trivially "successfully rolled out". The command
	# `kubectl wait -l` does not wait for a Pod to appear either. It fails
	# outright with "no matching resources found". Waiting for the DaemonSet to
	# want a Pod first makes both checks real. A reused cluster hides the problem
	# entirely, because the Pod is already there.
	for _ in $(seq 60); do
		desired="$(kctl -n kube-system get daemonset kata-deploy \
			-o jsonpath='{.status.desiredNumberScheduled}' 2>/dev/null || true)"
		if [[ -n "${desired}" && "${desired}" -gt 0 ]]; then
			break
		fi
		sleep 2
	done

	# kata-deploy reports ready before it finishes writing the runtime onto the
	# node, and the node fixes below edit the files that it installs.
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
