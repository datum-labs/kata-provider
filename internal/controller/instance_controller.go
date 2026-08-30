// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	"go.datum.net/compute/pkg/instancepod"
	"go.datum.net/compute/pkg/runtimeclass"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"

	"go.datum.net/kata-provider/internal/config"
)

const (
	// instanceFinalizer gates Instance deletion on teardown of the backing Pod.
	// The provider holds it until the Pod is deleted and observed gone, so the
	// Instance — and the WorkloadDeployment and Workload waiting on it — is
	// never removed while a guest is still running.
	instanceFinalizer = "kata.datumapis.com/finalizer"

	// DefaultRuntimeHandler is the Kubernetes RuntimeClass the provider targets
	// when a deployment names none.
	//
	// kata-clh runs each guest under Cloud Hypervisor, which reserves far less
	// memory per instance than QEMU does — kata-deploy declares 130Mi of pod
	// overhead for kata-clh against 320Mi for kata-qemu. That overhead sets how
	// many instances a node holds, and therefore what the general-purpose tier
	// costs to run. Cloud Hypervisor also boots a guest faster, though pulling
	// the image dominates the time a tenant waits.
	//
	// Cloud Hypervisor is x86_64 only in Kata 4.x. An arm64 deployment must set
	// RuntimeHandler to kata-qemu, because kata-deploy builds no arm64 Cloud
	// Hypervisor shim.
	DefaultRuntimeHandler = "kata-clh"

	// kataAnnotationPrefix is the annotation namespace Kata reads runtime
	// configuration from. Nothing under it may originate with a tenant; see
	// podAnnotations.
	kataAnnotationPrefix = "io.katacontainers."

	// managedByLabel and instanceLabel mark a Pod as this provider's and point
	// back at the Instance it realizes, so an operator can find either from the
	// other without parsing owner references.
	managedByLabel = "managed-by"
	instanceLabel  = "upstream.instance"

	managedByValue = "kata-provider"
)

// DefaultNodeSelector places instance Pods on nodes where the Kata runtime is
// installed. kata-deploy labels every node it has provisioned with this, so it
// is the selector that works on an unmodified installation.
var DefaultNodeSelector = map[string]string{
	"katacontainers.io/kata-runtime": "true",
}

// providerRuntimeAnnotations is the complete allow-list of runtime annotations
// the provider sets on an instance Pod, and the only keys under
// kataAnnotationPrefix permitted to exist on one.
//
// SECURITY. Kata treats io.katacontainers.* Pod annotations as runtime
// configuration and acts on them as host root. Every escape reported against it
// in 2026 — up to a guest-to-host escape scoring 9.6 — reduces to the same bug:
// a value a tenant could write reached that configuration. The defense is
// structural rather than a matter of sanitizing values, so an instance Pod is
// built with the annotations named here and no others. Instance metadata is
// tenant-writable and is never a source for them.
//
// It is empty because the provider needs no runtime annotation today: guest
// sizing comes from the Pod's resource requests, which the platform computes
// from the instance type catalog. Anything added here is a platform decision
// with a fixed value, never a value copied from an Instance.
var providerRuntimeAnnotations = map[string]string{}

// +kubebuilder:rbac:groups=compute.datumapis.com,resources=instances,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=compute.datumapis.com,resources=instances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=compute.datumapis.com,resources=instances/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=node.k8s.io,resources=runtimeclasses,verbs=get;list;watch

// InstanceReconciler realizes the Instances of the general-purpose runtime
// class as Kata-isolated Pods.
type InstanceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config *config.KataProvider
}

func (r *InstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var instance computev1alpha.Instance
	if err := r.Get(ctx, req.NamespacedName, &instance); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get instance: %w", err)
	}

	// Teardown runs before every short-circuit below, so an instance that
	// became ineligible after its Pod was created still has that Pod removed.
	if !instance.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &instance)
	}

	// Only a sandbox of containers maps onto a Pod. A virtual machine instance
	// is not something this class serves, and compute rejects it at apply time
	// against the capabilities declared here, so reaching this point means the
	// instance predates that validation: leave it alone rather than realize it
	// as something it did not ask for.
	if instance.Spec.Runtime.Sandbox == nil {
		logger.Info("skipping instance that does not declare a sandbox runtime", "instance", instance.Name)
		return ctrl.Result{}, nil
	}

	// A gated instance is not ready to be placed. Clearing the gate updates the
	// spec, which re-triggers reconciliation, so no requeue is needed.
	if instance.Spec.Controller != nil && len(instance.Spec.Controller.SchedulingGates) > 0 {
		logger.Info("instance has scheduling gates, deferring placement",
			"instance", instance.Name,
			"gates", instance.Spec.Controller.SchedulingGates,
		)
		return ctrl.Result{}, nil
	}

	// A suspended instance keeps its placement, addresses, and quota; only its
	// process stops. Deleting the Pod stops it, and reconciliation recreates it
	// when the instance is resumed.
	if instance.Status.Suspended {
		return r.reconcileSuspended(ctx, &instance)
	}

	// The finalizer goes on only once the provider has decided this instance is
	// its to realize, so an instance it never backed — one still gated, or one
	// shaped as something this class does not run — is not held up on a
	// teardown that has nothing to do.
	if !controllerutil.ContainsFinalizer(&instance, instanceFinalizer) {
		controllerutil.AddFinalizer(&instance, instanceFinalizer)
		if err := r.Update(ctx, &instance); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer to instance %s: %w", instance.Name, err)
		}
	}

	return r.reconcileInstance(ctx, &instance)
}

// handleDeletion stops the instance and releases the finalizer only once its
// Pod is confirmed gone.
//
// The Pod is deleted explicitly rather than left to owner-reference garbage
// collection: GC will not reclaim it until the Instance leaves etcd, and the
// Instance cannot leave etcd while this finalizer is held. The owner reference
// remains as a backstop for a Pod created before the finalizer was recorded.
func (r *InstanceReconciler) handleDeletion(ctx context.Context, instance *computev1alpha.Instance) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(instance, instanceFinalizer) {
		// The provider never claimed this instance, so it has nothing running.
		return ctrl.Result{}, nil
	}

	pod := &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: instance.Name, Namespace: instance.Namespace}}
	if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("failed to delete pod for instance %s: %w", instance.Name, err)
	}

	// A Kata guest takes time to shut down, and the Pod exists until it has.
	// Releasing the finalizer earlier would report the instance gone while its
	// guest still held the node's memory and its addresses. The Owns(Pod) watch
	// re-triggers on final removal; the requeue is a backstop for a missed
	// event.
	pending, err := r.backingPodPending(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pending {
		logger.Info("waiting for the instance to stop before finalizing", "instance", instance.Name)
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	controllerutil.RemoveFinalizer(instance, instanceFinalizer)
	if err := r.Update(ctx, instance); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer from instance %s: %w", instance.Name, err)
	}
	logger.Info("instance stopped; finalizer released", "instance", instance.Name)
	return ctrl.Result{}, nil
}

// backingPodPending reports whether the Instance's Pod still exists.
func (r *InstanceReconciler) backingPodPending(ctx context.Context, instance *computev1alpha.Instance) (bool, error) {
	key := client.ObjectKey{Name: instance.Name, Namespace: instance.Namespace}

	var pod core.Pod
	switch err := r.Get(ctx, key, &pod); {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, fmt.Errorf("failed to check backing pod for instance %s: %w", instance.Name, err)
	}
}

// reconcileSuspended stops the instance's process without releasing anything
// else it holds.
//
// The Instance object, its finalizer, and its addresses stay. Status is left
// alone: compute owns the Available and Ready conditions of a suspended
// instance and writes the suspension reason itself, so a provider writing them
// here would only fight it.
func (r *InstanceReconciler) reconcileSuspended(ctx context.Context, instance *computev1alpha.Instance) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	pod := &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: instance.Name, Namespace: instance.Namespace}}
	if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("failed to stop suspended instance %s: %w", instance.Name, err)
	}

	logger.Info("instance suspended", "instance", instance.Name)
	return ctrl.Result{}, nil
}

func (r *InstanceReconciler) reconcileInstance(ctx context.Context, instance *computev1alpha.Instance) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	desired, err := instancepod.BuildPod(instance, r.podOptions(instance))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to build pod for instance %s: %w", instance.Name, err)
	}

	// What actually makes the instance Kata-isolated: the Pod names a
	// Kubernetes RuntimeClass, and the kubelet hands it to the Kata handler
	// instead of the shared-kernel default.
	desired.Spec.RuntimeClassName = ptr.To(r.runtimeHandler())

	pod := &core.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
		},
	}

	result, err := controllerutil.CreateOrPatch(ctx, r.Client, pod, func() error {
		if pod.Labels == nil {
			pod.Labels = map[string]string{}
		}
		for key, value := range desired.Labels {
			pod.Labels[key] = value
		}

		pod.Annotations = podAnnotations(pod.Annotations)

		// A Pod's containers and resources are immutable in the ways that
		// matter here, and a Kata guest cannot be resized in place, so an
		// existing Pod keeps the spec it booted with. Compute replaces the
		// instance when its template changes.
		if pod.CreationTimestamp.IsZero() {
			pod.Spec = desired.Spec
		}

		return controllerutil.SetControllerReference(instance, pod, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create/update pod for instance %s: %w", instance.Name, err)
	}

	logger.Info("reconciled instance",
		"result", result,
		"instance", instance.Name,
		"runtimeHandler", r.runtimeHandler(),
		"phase", pod.Status.Phase,
	)

	if err := r.syncInstanceStatus(ctx, instance, pod); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to sync status for instance %s: %w", instance.Name, err)
	}

	return ctrl.Result{}, nil
}

// podOptions is the Kata-specific policy this provider contributes to an
// otherwise platform-owned translation. Everything else about the Pod —
// containers, volumes, environment, ports, and sizing from the instance type
// catalog — comes from the shared translation, so the two runtime classes
// cannot drift into two dialects of the same instance.
func (r *InstanceReconciler) podOptions(instance *computev1alpha.Instance) instancepod.Options {
	return instancepod.Options{
		Capabilities: Capabilities,
		NodeSelector: r.nodeSelector(),
		Tolerations:  r.tolerations(),
		PodLabels: map[string]string{
			managedByLabel: managedByValue,
			instanceLabel:  instance.Name,
		},
		PodAnnotations: podAnnotations(nil),
	}
}

// podAnnotations returns the annotations an instance Pod carries, given
// whatever a Pod already has.
//
// SECURITY, read before changing. The result is built from the provider's own
// allow-list. Nothing is copied from the Instance, which is tenant-writable,
// and any io.katacontainers.* key already present that the provider did not put
// there is removed rather than preserved — Kata reads those as host-root
// runtime configuration, and every 2026 escape against it came from a tenant
// reaching one. Do not add passthrough of Instance annotations here, however
// narrowly scoped it looks: a prefix filter is what those CVEs got wrong. A new
// runtime annotation belongs in providerRuntimeAnnotations with a fixed,
// platform-chosen value.
func podAnnotations(existing map[string]string) map[string]string {
	annotations := make(map[string]string, len(existing)+len(providerRuntimeAnnotations))

	for key, value := range existing {
		if strings.HasPrefix(key, kataAnnotationPrefix) {
			continue
		}
		annotations[key] = value
	}

	for key, value := range providerRuntimeAnnotations {
		annotations[key] = value
	}

	return annotations
}

// runtimeHandler is the name of the Kubernetes RuntimeClass — the cluster
// object that points containerd at a Kata handler — that instance Pods run
// under.
//
// This is NOT Datum's runtime class. Datum's is a customer-facing promise about
// isolation, compatibility, startup, and price, selected on a workload;
// Kubernetes' is a node-level runtime binding. This provider serves exactly one
// Datum class, and targets exactly one Kubernetes RuntimeClass to do it. The
// two happen to be related here and are unrelated concepts everywhere else.
func (r *InstanceReconciler) runtimeHandler() string {
	if r.Config != nil && r.Config.DownstreamResourceManagement.RuntimeHandler != "" {
		return r.Config.DownstreamResourceManagement.RuntimeHandler
	}
	return DefaultRuntimeHandler
}

func (r *InstanceReconciler) nodeSelector() map[string]string {
	if r.Config != nil && len(r.Config.DownstreamResourceManagement.NodeSelector) > 0 {
		return r.Config.DownstreamResourceManagement.NodeSelector
	}
	return DefaultNodeSelector
}

func (r *InstanceReconciler) tolerations() []core.Toleration {
	if r.Config != nil {
		return r.Config.DownstreamResourceManagement.Tolerations
	}
	return nil
}

// syncInstanceStatus reports what the customer sees on their instance.
//
// The provider owns Programmed, Available, the observed template hash, and the
// network interface status. It must not write Ready: compute derives that from
// Programmed and Available, and writing it here would race that derivation.
func (r *InstanceReconciler) syncInstanceStatus(
	ctx context.Context,
	instance *computev1alpha.Instance,
	pod *core.Pod,
) error {
	logger := log.FromContext(ctx)

	// Snapshot before any mutation so the patch carries only the fields this
	// controller owns. QuotaGranted and Ready belong to compute.
	base := instance.DeepCopy()

	available := metav1.Condition{
		Type:               computev1alpha.InstanceAvailable,
		ObservedGeneration: instance.Generation,
		Status:             metav1.ConditionUnknown,
		Reason:             computev1alpha.InstanceReadyReasonProvisioning,
		Message:            "Instance is provisioning",
	}
	programmed := metav1.Condition{
		Type:               computev1alpha.InstanceProgrammed,
		ObservedGeneration: instance.Generation,
		Status:             metav1.ConditionUnknown,
		Reason:             computev1alpha.InstanceProgrammedReasonProgrammingInProgress,
		Message:            "Instance is provisioning",
	}

	statusChanged := false

	switch pod.Status.Phase {
	case core.PodRunning:
		available.Status = metav1.ConditionTrue
		available.Reason = computev1alpha.InstanceAvailableReasonAvailable
		available.Message = "Instance is available"
		programmed.Status = metav1.ConditionTrue
		programmed.Reason = computev1alpha.InstanceProgrammedReasonProgrammed
		programmed.Message = "Instance is available"

		// Compute counts an instance toward its deployment's replicas only
		// while the hash it observed matches the template it was asked for, so
		// the hash is echoed back once the instance is actually running.
		if instance.Spec.Controller != nil {
			if instance.Status.Controller == nil {
				instance.Status.Controller = &computev1alpha.InstanceControllerStatus{}
			}
			if instance.Status.Controller.ObservedTemplateHash != instance.Spec.Controller.TemplateHash {
				instance.Status.Controller.ObservedTemplateHash = instance.Spec.Controller.TemplateHash
				statusChanged = true
			}
		}

	case core.PodPending:
		// The first container that reports why it is waiting explains the whole
		// instance. Its Kubernetes reason is translated centrally into
		// Instance-domain language, and the raw detail is logged for operators
		// rather than shown to the customer.
		for _, status := range pod.Status.ContainerStatuses {
			if status.State.Waiting == nil || status.State.Waiting.Reason == "" {
				continue
			}
			logger.Info("instance container waiting",
				"instance", instance.Name,
				"containerReason", status.State.Waiting.Reason,
				"containerMessage", status.State.Waiting.Message,
			)
			available.Reason, available.Message = runtimeclass.TranslateWaitingReason(status.State.Waiting.Reason)
			break
		}
		programmed.Message = available.Message

	case core.PodSucceeded:
		available.Status = metav1.ConditionFalse
		available.Reason = computev1alpha.InstanceAvailableReasonStopped
		available.Message = "Instance has stopped"
		programmed.Status = metav1.ConditionFalse
		programmed.Reason = computev1alpha.InstanceAvailableReasonStopped
		programmed.Message = "Instance has stopped"

	case core.PodFailed:
		available.Status = metav1.ConditionFalse
		available.Reason = computev1alpha.InstanceReadyReasonInstanceCrashing
		available.Message = "Instance stopped unexpectedly"
		programmed.Status = metav1.ConditionFalse
		programmed.Reason = computev1alpha.InstanceProgrammedReasonInstanceCrashing
		programmed.Message = available.Message

	default:
		available.Reason = computev1alpha.InstanceReadyReasonProvisioning
		available.Message = "Instance is provisioning"
	}

	statusChanged = meta.SetStatusCondition(&instance.Status.Conditions, available) || statusChanged
	statusChanged = meta.SetStatusCondition(&instance.Status.Conditions, programmed) || statusChanged

	interfaces := buildNetworkInterfaceStatus(instance, pod)
	if !reflect.DeepEqual(instance.Status.NetworkInterfaces, interfaces) {
		instance.Status.NetworkInterfaces = interfaces
		statusChanged = true
	}

	if !statusChanged {
		return nil
	}

	// An optimistic-lock merge patch turns a concurrent write by compute's
	// quota controller into a conflict the caller requeues on, rather than
	// silently clobbering it.
	return r.Status().Patch(ctx, instance, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// buildNetworkInterfaceStatus reports the addresses the instance is reachable
// at. The Pod's addresses are the instance's addresses: a Kata guest holds the
// Pod's network namespace, so what the cluster assigned the Pod is what runs
// inside the guest.
func buildNetworkInterfaceStatus(
	instance *computev1alpha.Instance,
	pod *core.Pod,
) []computev1alpha.InstanceNetworkInterfaceStatus {
	if len(pod.Status.PodIPs) == 0 {
		return nil
	}

	name := "eth0"
	if len(instance.Spec.NetworkInterfaces) > 0 && instance.Spec.NetworkInterfaces[0].Name != "" {
		name = instance.Spec.NetworkInterfaces[0].Name
	}

	addresses := make([]computev1alpha.InstanceNetworkInterfaceAddress, 0, len(pod.Status.PodIPs))
	var primary string
	for _, podIP := range pod.Status.PodIPs {
		parsed := net.ParseIP(podIP.IP)
		if parsed == nil {
			continue
		}

		// A Pod address is a single host address, so it is reported at its
		// full prefix length rather than that of the subnet behind it.
		family := networkingv1alpha.IPv6Protocol
		prefix := "/128"
		if parsed.To4() != nil {
			family = networkingv1alpha.IPv4Protocol
			prefix = "/32"
		}

		isPrimary := primary == ""
		if isPrimary {
			primary = podIP.IP
		}

		addresses = append(addresses, computev1alpha.InstanceNetworkInterfaceAddress{
			Family:  family,
			Address: podIP.IP + prefix,
			Primary: isPrimary,
		})
	}

	if len(addresses) == 0 {
		return nil
	}

	networkIP := primary
	return []computev1alpha.InstanceNetworkInterfaceStatus{
		{
			Name:      name,
			Addresses: addresses,
			Assignments: computev1alpha.InstanceNetworkInterfaceAssignmentsStatus{
				NetworkIP: &networkIP,
			},
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
//
// The class selector is applied to the manager's CACHE rather than here; see
// InstanceCacheOptions.
func (r *InstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.Scheme == nil {
		r.Scheme = mgr.GetScheme()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&computev1alpha.Instance{}).
		Owns(&core.Pod{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 5}).
		Named("instance").
		Complete(r)
}
