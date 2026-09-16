// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
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
	// The provider holds the finalizer until it has deleted the Pod and
	// observed the Pod gone. The Instance therefore never disappears while a
	// guest is still running, and neither does the WorkloadDeployment or the
	// Workload waiting on that Instance.
	instanceFinalizer = "kata.datumapis.com/finalizer"

	// DefaultRuntimeHandler is the Kubernetes RuntimeClass that the provider
	// targets when a deployment names none.
	//
	// kata-clh runs each guest under Cloud Hypervisor, which reserves far less
	// memory per instance than QEMU does. kata-deploy declares 130Mi of pod
	// overhead for kata-clh against 320Mi for kata-qemu. That overhead sets how
	// many instances a node holds, and therefore what the general-purpose tier
	// costs to run. Cloud Hypervisor also boots a guest faster, though pulling
	// the image dominates the time a tenant waits.
	//
	// Cloud Hypervisor is x86_64 only in Kata 4.x. An arm64 deployment must set
	// RuntimeHandler to kata-qemu, because kata-deploy builds no arm64 Cloud
	// Hypervisor shim.
	DefaultRuntimeHandler = "kata-clh"

	// kataAnnotationPrefix is the annotation namespace that Kata reads runtime
	// configuration from. No key under the prefix may originate with a tenant.
	// For the reasoning, see podAnnotations.
	kataAnnotationPrefix = "io.katacontainers."

	// managedByLabel and instanceLabel mark a Pod as this provider's, and point
	// back at the Instance that the Pod realizes. An operator can then find
	// either object from the other without parsing owner references.
	managedByLabel = "managed-by"
	instanceLabel  = "upstream.instance"

	managedByValue = "kata-provider"
)

// DefaultNodeSelector places instance Pods on nodes where the Kata runtime is
// installed. kata-deploy labels every node it provisioned with this label, so
// the selector works on an unmodified installation.
var DefaultNodeSelector = map[string]string{
	"katacontainers.io/kata-runtime": "true",
}

// providerRuntimeAnnotations is the complete allow-list of runtime annotations
// that the provider sets on an instance Pod. No other key under
// kataAnnotationPrefix may exist on an instance Pod.
//
// SECURITY. Kata treats io.katacontainers.* Pod annotations as runtime
// configuration and acts on them as host root. Every escape reported against
// Kata in 2026 reduces to the same bug, including a guest-to-host escape
// scoring 9.6: a value that a tenant could write reached that configuration.
// The defense here is structural, and it is not a matter of sanitizing values.
// The provider builds an instance Pod with the annotations named in this map
// and with no others. Instance metadata is tenant-writable, and it is never a
// source for these annotations.
//
// The map is empty because the provider needs no runtime annotation today.
// Guest sizing comes from the Pod's resource requests, which the platform
// computes from the instance type catalog. Any annotation added to this map is
// a platform decision with a fixed value. It is never a value copied from an
// Instance.
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

	// Teardown runs before every short-circuit below. An instance that became
	// ineligible after its Pod was created therefore still has that Pod
	// removed.
	if !instance.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &instance)
	}

	// Only a sandbox of containers maps onto a Pod. This class does not serve a
	// virtual machine instance, and compute rejects such an instance at apply
	// time against the capabilities declared here. An instance that reaches
	// this point predates that validation, so leave it alone rather than
	// realize it as something it did not ask for.
	if instance.Spec.Runtime.Sandbox == nil {
		logger.Info("skipping instance that does not declare a sandbox runtime", "instance", instance.Name)
		return ctrl.Result{}, nil
	}

	// A gated instance is not ready to be placed. Clearing the gate updates the
	// spec, which re-triggers reconciliation, so this branch needs no requeue.
	if instance.Spec.Controller != nil && len(instance.Spec.Controller.SchedulingGates) > 0 {
		logger.Info("instance has scheduling gates, deferring placement",
			"instance", instance.Name,
			"gates", instance.Spec.Controller.SchedulingGates,
		)
		return ctrl.Result{}, nil
	}

	// A suspended instance keeps its placement, its addresses, and its quota.
	// Only its process stops. Deleting the Pod stops the process, and
	// reconciliation recreates the Pod when the customer resumes the instance.
	if instance.Status.Suspended {
		return r.reconcileSuspended(ctx, &instance)
	}

	// The provider adds the finalizer only once it has decided that this
	// instance is its to realize. An instance the provider never backed is
	// therefore never held up on a teardown with nothing to do. Such an
	// instance is one that is still gated, or one shaped as something this
	// class does not run.
	if !controllerutil.ContainsFinalizer(&instance, instanceFinalizer) {
		controllerutil.AddFinalizer(&instance, instanceFinalizer)
		if err := r.Update(ctx, &instance); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer to instance %s: %w", instance.Name, err)
		}
	}

	return r.reconcileInstance(ctx, &instance)
}

// handleDeletion stops the instance, and releases the finalizer only once the
// instance's Pod is confirmed gone.
//
// The provider deletes the Pod explicitly rather than leaving it to
// owner-reference garbage collection. Garbage collection does not reclaim the
// Pod until the Instance leaves etcd, and the Instance cannot leave etcd while
// the provider holds this finalizer. The owner reference remains as a backstop
// for a Pod created before the finalizer was recorded.
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

	// A Kata guest takes time to shut down, and the Pod exists until the guest
	// has stopped. Releasing the finalizer earlier would report the instance
	// gone while its guest still held the node's memory and its addresses. The
	// Owns(Pod) watch re-triggers on the Pod's final removal, and the requeue
	// is a backstop for a missed event.
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
// else that the instance holds.
//
// The Instance object, its finalizer, and its addresses stay. The provider
// leaves status alone. Compute owns the Available and Ready conditions of a
// suspended instance, and compute writes the suspension reason itself. A
// provider writing those conditions here would only fight compute.
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
		// A refused request fails identically on every retry. Changing it
		// changes the instance spec, which triggers reconciliation again.
		if message, refused := buildRefusalMessage(instance, err); refused {
			logger.Info("instance configuration refused by its runtime class", "instance", instance.Name, "reason", err.Error())
			return ctrl.Result{}, r.reportConfigurationError(ctx, instance, message)
		}
		return ctrl.Result{}, fmt.Errorf("failed to build pod for instance %s: %w", instance.Name, err)
	}

	// The RuntimeClass name is what makes the instance Kata-isolated. The Pod
	// names a Kubernetes RuntimeClass, and the kubelet hands the Pod to the
	// Kata handler instead of to the shared-kernel default.
	desired.Spec.RuntimeClassName = ptr.To(r.runtimeHandler())

	applyPodSecurityContext(&desired.Spec)

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

		// Ask for the instance's interfaces to be wired up. Clearing the label
		// explicitly takes the opt-in back off Pods that already carry it when
		// a cell turns the feature off.
		if r.requestsInterfaceInjection(instance) {
			pod.Labels[injectInterfacesLabel] = "true"
		} else {
			delete(pod.Labels, injectInterfacesLabel)
		}

		// The injecting webhook writes its annotations outside the runtime
		// namespace, so they survive this filter.
		pod.Annotations = podAnnotations(pod.Annotations)

		// A Pod's containers and resources are immutable in the ways that
		// matter here, and Kata cannot resize a guest in place, so an existing
		// Pod keeps the spec it booted with. Compute replaces the instance when
		// its template changes.
		if pod.CreationTimestamp.IsZero() {
			pod.Spec = desired.Spec
		}

		return controllerutil.SetControllerReference(instance, pod, r.Scheme)
	})
	if err != nil {
		if pod.CreationTimestamp.IsZero() && podCreationDeclined(err) {
			return ctrl.Result{}, r.reportDeclined(ctx, instance, err)
		}
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

// applyPodSecurityContext sets the Pod-level confinement of an instance's
// sandbox.
//
// Only what is not a customer choice is set here. Container confinement —
// capabilities, privilege escalation, and the seccomp profile — comes from the
// Instance, so a customer reads on their own workload exactly what their
// container runs with. A value the provider added would be a privilege nobody
// could see or remove.
//
// The seccomp profile confines the sandbox itself and applies to any container
// that states none. A container that states one overrides it, so stating it
// here overrules nothing a customer asked for. A cell's PodSecurity admission
// reads the field on the Pod, and so does an operator auditing what the
// provider submits.
//
// runAsNonRoot is deliberately absent. The general-purpose class exists to run
// stock container images, and many of them start as root. Setting the field
// would fail exactly the images the class promises to run, so a cell hosting
// these instances enforces the PodSecurity baseline profile rather than
// restricted.
func applyPodSecurityContext(spec *core.PodSpec) {
	spec.SecurityContext = &core.PodSecurityContext{
		SeccompProfile: &core.SeccompProfile{Type: core.SeccompProfileTypeRuntimeDefault},
	}
}

// podCreationDeclined reports whether the API server refused a new instance Pod
// on its content, as PodSecurity admission or a validating webhook does. Retrying
// cannot change that outcome until the cell's policy changes.
func podCreationDeclined(err error) bool {
	return apierrors.IsForbidden(err) || apierrors.IsInvalid(err)
}

// reportDeclined tells the customer that their instance cannot start as
// configured, then returns an error so the work queue retries with backoff. A
// cell may yet be reconfigured to admit the instance.
//
// The message stays generic because the API server's text names Pods and
// admission policies, which mean nothing to a customer. The full error goes to
// the log for operators.
func (r *InstanceReconciler) reportDeclined(ctx context.Context, instance *computev1alpha.Instance, declined error) error {
	log.FromContext(ctx).Error(declined, "instance pod declined by the api server", "instance", instance.Name)

	const message = "The location running this instance refused its configuration, " +
		"such as the Linux capabilities its containers request, so the instance cannot start. " +
		"The platform keeps retrying."

	if err := r.reportConfigurationError(ctx, instance, message); err != nil {
		return err
	}
	return fmt.Errorf("pod for instance %s was declined: %w", instance.Name, declined)
}

// reportConfigurationError marks the instance not programmed and not available
// because of its configuration. It writes only on change, so a repeat reconcile
// does not churn status.
func (r *InstanceReconciler) reportConfigurationError(ctx context.Context, instance *computev1alpha.Instance, message string) error {
	base := instance.DeepCopy()
	changed := meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
		Type:               computev1alpha.InstanceProgrammed,
		ObservedGeneration: instance.Generation,
		Status:             metav1.ConditionFalse,
		Reason:             computev1alpha.InstanceProgrammedReasonConfigurationError,
		Message:            message,
	})
	changed = meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
		Type:               computev1alpha.InstanceAvailable,
		ObservedGeneration: instance.Generation,
		Status:             metav1.ConditionFalse,
		Reason:             computev1alpha.InstanceReadyReasonConfigurationError,
		Message:            message,
	}) || changed

	if changed {
		if err := r.Status().Patch(ctx, instance, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("failed to report configuration error for instance %s: %w", instance.Name, err)
		}
	}
	return nil
}

// buildRefusalMessage reports whether BuildPod refused the instance on its
// content, and if so returns the message the customer sees. Only a refusal is
// safe to stop retrying on. Any other error, such as a failed API read, may
// clear on its own.
func buildRefusalMessage(instance *computev1alpha.Instance, err error) (string, bool) {
	switch {
	case errors.Is(err, instancepod.ErrHostPathVolume):
		return "The instance requests a volume the platform cannot attach, so the instance cannot start.", true
	case errors.Is(err, instancepod.ErrNotSandbox):
		return "The instance declares no containers to run, so the instance cannot start.", true
	}

	// Class validation is the only source of field errors in the build. An
	// aggregate holding anything else is not known to be the customer's to fix.
	var aggregate utilerrors.Aggregate
	if !errors.As(err, &aggregate) || len(aggregate.Errors()) == 0 {
		return "", false
	}
	hints := capabilityHints(instance)
	var problems []string
	for _, item := range aggregate.Errors() {
		var fieldErr *field.Error
		if !errors.As(item, &fieldErr) {
			return "", false
		}
		problem, ok := hints[fieldErr.Field]
		if !ok {
			problem = asSentence(fieldErr.Detail)
		}
		if !slices.Contains(problems, problem) {
			problems = append(problems, problem)
		}
	}
	return "The instance cannot start as configured. " + strings.Join(problems, " "), true
}

// capabilityHints maps each capability add the class refuses, keyed by its field
// path, to a fix the customer can apply. The class's own rejection lists every
// capability it grants. For this class that is all of them, which buries the
// actual mistake: ALL, or a CAP_-prefixed name.
func capabilityHints(instance *computev1alpha.Instance) map[string]string {
	hints := map[string]string{}
	if instance.Spec.Runtime.Sandbox == nil {
		return hints
	}
	containersPath := field.NewPath("spec", "runtime", "sandbox", "containers")
	for i, container := range instance.Spec.Runtime.Sandbox.Containers {
		if container.SecurityContext == nil || container.SecurityContext.Capabilities == nil {
			continue
		}
		addPath := containersPath.Index(i).Child("securityContext", "capabilities", "add")
		for j, capability := range container.SecurityContext.Capabilities.Add {
			if Capabilities.Grants(capability) {
				continue
			}
			hints[addPath.Index(j).String()] = capabilityHint(container.Name, capability)
		}
	}
	return hints
}

// capabilityHint mirrors the wording compute uses when it checks a capability
// request's shape at apply time.
func capabilityHint(container string, capability computev1alpha.Capability) string {
	name := string(capability)
	trimmed, prefixed := strings.CutPrefix(name, "CAP_")
	switch {
	case capability == computev1alpha.CapabilityAll:
		return fmt.Sprintf("Container %q may not add ALL; list each capability the container needs.", container)
	case prefixed:
		return fmt.Sprintf("Container %q must name %s without the CAP_ prefix, for example %q.", container, name, trimmed)
	default:
		return fmt.Sprintf("Container %q requests %s, which %s does not grant.", container, name, Capabilities.ClassDescription())
	}
}

// asSentence turns a field error detail, which compute already words for the
// customer, into a sentence that can follow another.
func asSentence(detail string) string {
	if detail == "" {
		return "The runtime class does not support part of the request."
	}
	sentence := strings.ToUpper(detail[:1]) + detail[1:]
	if !strings.HasSuffix(sentence, ".") {
		sentence += "."
	}
	return sentence
}

// podOptions returns the Kata-specific policy that this provider contributes to
// an otherwise platform-owned translation. The shared translation supplies
// everything else about the Pod: containers, volumes, environment, ports, and
// sizing from the instance type catalog. The two runtime classes therefore
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

// podAnnotations returns the annotations that an instance Pod carries, given
// whatever annotations that Pod already has.
//
// SECURITY. Read this comment before changing the function. The provider builds
// the result from its own allow-list. The function copies nothing from the
// Instance, which is tenant-writable. The function also removes, rather than
// preserves, any io.katacontainers.* key already present that the provider did
// not put there. Kata reads those keys as host-root runtime configuration, and
// every 2026 escape against Kata came from a tenant reaching one of them. Do
// not add passthrough of Instance annotations here, however narrowly scoped the
// passthrough looks. A prefix filter over tenant input is exactly what the
// known Common Vulnerabilities and Exposures (CVE) reports in this area got
// wrong. A new runtime annotation belongs in providerRuntimeAnnotations, with a
// fixed, platform-chosen value.
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

// runtimeHandler returns the name of the Kubernetes RuntimeClass that instance
// Pods run under. A Kubernetes RuntimeClass is the cluster object that points
// containerd at a Kata handler.
//
// A Kubernetes RuntimeClass is NOT a Datum runtime class. A Datum runtime class
// is a customer-facing promise about isolation, compatibility, startup, and
// price, and a customer selects it on a workload. A Kubernetes RuntimeClass is
// a node-level runtime binding. This provider serves exactly one Datum class,
// and it targets exactly one Kubernetes RuntimeClass to do so. The two are
// related here, and they are unrelated concepts everywhere else.
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
// The provider owns Programmed, Available, and the observed template hash. It
// owns the network interface status only in a cell that allocates the instance
// no tenant network address. For the reasoning, see providerOwnsInterfaceStatus.
//
// The provider must not write Ready. Compute derives Ready from Programmed and
// Available, and writing Ready here would race that derivation.
func (r *InstanceReconciler) syncInstanceStatus(
	ctx context.Context,
	instance *computev1alpha.Instance,
	pod *core.Pod,
) error {
	logger := log.FromContext(ctx)

	// Snapshot the instance before any mutation, so that the patch carries only
	// the fields this controller owns. QuotaGranted and Ready belong to
	// compute.
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
		// while the hash it observed matches the template it asked for. The
		// provider therefore echoes the hash back once the instance is running.
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
		// instance. A shared helper translates that container's Kubernetes
		// reason into Instance-domain language. The raw detail goes to the log
		// for operators rather than to the customer.
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

	if r.providerOwnsInterfaceStatus(instance) {
		interfaces := buildNetworkInterfaceStatus(instance, pod)
		if !reflect.DeepEqual(instance.Status.NetworkInterfaces, interfaces) {
			instance.Status.NetworkInterfaces = interfaces
			statusChanged = true
		}
	}

	if !statusChanged {
		return nil
	}

	// An optimistic-lock merge patch turns a concurrent write by compute's
	// quota controller into a conflict that the caller requeues on, rather than
	// silently overwriting that write.
	return r.Status().Patch(ctx, instance, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// buildNetworkInterfaceStatus reports the addresses that the instance is
// reachable at in a cell with no tenant networking. The Pod's addresses are
// then the only addresses the instance has. A Kata guest holds the Pod's
// network namespace, so the address that the cluster assigned to the Pod is the
// address that runs inside the guest.
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

		// A Pod address is a single host address, so the provider reports it
		// at its full prefix length rather than at the prefix length of the
		// subnet behind it.
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

// SetupWithManager registers the controller with the Manager.
//
// The class selector applies to the manager's CACHE, not to this controller's
// event filters. A predicate alone still caches every object. For the
// reasoning, see CacheOptions.
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
