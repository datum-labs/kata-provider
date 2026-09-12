// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	"go.datum.net/compute/pkg/instancetype"

	"go.datum.net/kata-provider/internal/config"
)

const (
	testInstanceName      = "test-instance"
	testInstanceNamespace = "default"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("failed to add clientgo scheme: %v", err)
	}
	if err := computev1alpha.AddToScheme(s); err != nil {
		t.Fatalf("failed to add compute scheme: %v", err)
	}
	return s
}

// newTestInstance returns an instance shaped the way compute creates one: a
// single-container sandbox, sized by instance type, in the general-purpose
// runtime class.
func newTestInstance(mutators ...func(*computev1alpha.Instance)) *computev1alpha.Instance {
	instance := &computev1alpha.Instance{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testInstanceName,
			Namespace:  testInstanceNamespace,
			Generation: 1,
			Labels: map[string]string{
				computev1alpha.RuntimeClassLabel: RuntimeClassName,
			},
		},
		Spec: computev1alpha.InstanceSpec{
			Runtime: computev1alpha.InstanceRuntimeSpec{
				Class: RuntimeClassName,
				Resources: computev1alpha.InstanceRuntimeResources{
					InstanceType: instancetype.D1Standard2,
				},
				Sandbox: &computev1alpha.SandboxRuntime{
					Containers: []computev1alpha.SandboxContainer{
						{Name: "app", Image: "docker.io/library/nginx:latest"},
					},
				},
			},
			NetworkInterfaces: []computev1alpha.InstanceNetworkInterface{{Name: "eth0"}},
		},
	}
	for _, mutate := range mutators {
		mutate(instance)
	}
	return instance
}

func newReconciler(t *testing.T, cfg *config.KataProvider, objects ...client.Object) (*InstanceReconciler, client.Client) {
	t.Helper()
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&computev1alpha.Instance{}).
		Build()
	return &InstanceReconciler{Client: fakeClient, Scheme: scheme, Config: cfg}, fakeClient
}

func instanceRequest() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{
		Name:      testInstanceName,
		Namespace: testInstanceNamespace,
	}}
}

func getPod(t *testing.T, c client.Client) (*core.Pod, bool) {
	t.Helper()
	var pod core.Pod
	err := c.Get(context.Background(), client.ObjectKey{Name: testInstanceName, Namespace: testInstanceNamespace}, &pod)
	switch {
	case err == nil:
		return &pod, true
	case apierrors.IsNotFound(err):
		return nil, false
	default:
		t.Fatalf("failed to get pod: %v", err)
		return nil, false
	}
}

func getInstance(t *testing.T, c client.Client) *computev1alpha.Instance {
	t.Helper()
	var instance computev1alpha.Instance
	if err := c.Get(context.Background(), client.ObjectKey{Name: testInstanceName, Namespace: testInstanceNamespace}, &instance); err != nil {
		t.Fatalf("failed to get instance: %v", err)
	}
	return &instance
}

// TestReconcile_KataPodPolicy covers the policy that this provider contributes
// to an otherwise platform-owned Pod: which Kubernetes RuntimeClass the
// instance runs under, where the instance lands, and how the provider labels
// it.
func TestReconcile_KataPodPolicy(t *testing.T) {
	tests := []struct {
		name               string
		config             *config.KataProvider
		wantRuntimeHandler string
		wantNodeSelector   map[string]string
		wantTolerations    int
	}{
		{
			name:               "unconfigured provider uses the Cloud Hypervisor handler and kata-deploy's node label",
			config:             nil,
			wantRuntimeHandler: DefaultRuntimeHandler,
			wantNodeSelector:   DefaultNodeSelector,
		},
		{
			name: "hypervisor choice is a deployment decision, as an arm64 site must make",
			config: &config.KataProvider{
				DownstreamResourceManagement: config.DownstreamResourceManagementConfig{
					RuntimeHandler: "kata-qemu",
				},
			},
			wantRuntimeHandler: "kata-qemu",
			wantNodeSelector:   DefaultNodeSelector,
		},
		{
			name: "shim choice is a deployment decision, as a cell moving to runtime-rs makes",
			config: &config.KataProvider{
				DownstreamResourceManagement: config.DownstreamResourceManagementConfig{
					RuntimeHandler: "katars",
				},
			},
			wantRuntimeHandler: "katars",
			wantNodeSelector:   DefaultNodeSelector,
		},
		{
			name: "node targeting is overridable for sites that install the runtime themselves",
			config: &config.KataProvider{
				DownstreamResourceManagement: config.DownstreamResourceManagementConfig{
					NodeSelector: map[string]string{"datum.net/kata": "true"},
					Tolerations: []core.Toleration{
						{Key: "datum.net/tenant-instances", Operator: core.TolerationOpExists},
					},
				},
			},
			wantRuntimeHandler: DefaultRuntimeHandler,
			wantNodeSelector:   map[string]string{"datum.net/kata": "true"},
			wantTolerations:    1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reconciler, fakeClient := newReconciler(t, tc.config, newTestInstance())

			if _, err := reconciler.Reconcile(context.Background(), instanceRequest()); err != nil {
				t.Fatalf("reconcile failed: %v", err)
			}

			pod, found := getPod(t, fakeClient)
			if !found {
				t.Fatal("expected the instance to be backed by a pod")
			}

			if pod.Spec.RuntimeClassName == nil {
				t.Fatal("expected the pod to name a Kubernetes RuntimeClass")
			}
			if got := *pod.Spec.RuntimeClassName; got != tc.wantRuntimeHandler {
				t.Errorf("runtimeClassName = %q, want %q", got, tc.wantRuntimeHandler)
			}

			for key, want := range tc.wantNodeSelector {
				if got := pod.Spec.NodeSelector[key]; got != want {
					t.Errorf("nodeSelector[%q] = %q, want %q", key, got, want)
				}
			}
			if got := len(pod.Spec.Tolerations); got != tc.wantTolerations {
				t.Errorf("tolerations = %d, want %d", got, tc.wantTolerations)
			}

			if got := pod.Labels[managedByLabel]; got != managedByValue {
				t.Errorf("label %q = %q, want %q", managedByLabel, got, managedByValue)
			}
			if got := pod.Labels[instanceLabel]; got != testInstanceName {
				t.Errorf("label %q = %q, want %q", instanceLabel, got, testInstanceName)
			}
		})
	}
}

// TestReconcile_SizingComesFromTheCatalog checks that an instance receives what
// the platform claimed quota for, rather than a size this provider invented.
func TestReconcile_SizingComesFromTheCatalog(t *testing.T) {
	reconciler, fakeClient := newReconciler(t, nil, newTestInstance())

	if _, err := reconciler.Reconcile(context.Background(), instanceRequest()); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	pod, found := getPod(t, fakeClient)
	if !found {
		t.Fatal("expected the instance to be backed by a pod")
	}

	sizing, ok := instancetype.Lookup(instancetype.D1Standard2)
	if !ok {
		t.Fatalf("instance type %q is not in the catalog", instancetype.D1Standard2)
	}

	limits := pod.Spec.Containers[0].Resources.Limits
	wantCPU := resource.NewMilliQuantity(sizing.CPUMillicores, resource.DecimalSI)
	wantMemory := resource.NewQuantity(sizing.MemoryMiB*1024*1024, resource.BinarySI)

	if got := limits.Cpu(); got.Cmp(*wantCPU) != 0 {
		t.Errorf("cpu limit = %s, want %s", got, wantCPU)
	}
	if got := limits.Memory(); got.Cmp(*wantMemory) != 0 {
		t.Errorf("memory limit = %s, want %s", got, wantMemory)
	}
}

// TestPodAnnotations verifies the boundary that keeps tenant-supplied metadata
// out of runtime configuration.
//
// Kata reads io.katacontainers.* Pod annotations as host-root configuration.
// Every 2026 escape against Kata came from a tenant reaching one of those
// annotations. A tenant annotation must therefore not survive onto an instance
// Pod by any route. The provider must not copy such an annotation from the
// Instance, and must not leave one in place if it reached the Pod by another
// path.
func TestPodAnnotations(t *testing.T) {
	tests := []struct {
		name     string
		existing map[string]string
		want     map[string]string
		absent   []string
	}{
		{
			name: "a new pod carries only what the provider sets",
			want: map[string]string{},
		},
		{
			name: "an annotation Kata reads as runtime configuration is removed",
			existing: map[string]string{
				"io.katacontainers.config.hypervisor.kernel_params": "init=/bin/sh",
				"io.katacontainers.config.hypervisor.path":          "/tmp/evil",
			},
			absent: []string{
				"io.katacontainers.config.hypervisor.kernel_params",
				"io.katacontainers.config.hypervisor.path",
			},
		},
		{
			name: "annotations outside the runtime namespace are left alone",
			existing: map[string]string{
				"kubectl.kubernetes.io/last-applied-configuration": "{}",
			},
			want: map[string]string{
				"kubectl.kubernetes.io/last-applied-configuration": "{}",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := podAnnotations(tc.existing)

			for key, want := range tc.want {
				if got[key] != want {
					t.Errorf("annotation %q = %q, want %q", key, got[key], want)
				}
			}
			for _, key := range tc.absent {
				if _, present := got[key]; present {
					t.Errorf("annotation %q survived; it must never reach the runtime", key)
				}
			}
		})
	}
}

// TestReconcile_TenantAnnotationsNeverReachThePod is the end-to-end form of the
// same guarantee. An Instance is tenant-writable, so nothing on an Instance may
// become runtime configuration.
func TestReconcile_TenantAnnotationsNeverReachThePod(t *testing.T) {
	instance := newTestInstance(func(i *computev1alpha.Instance) {
		i.Annotations = map[string]string{
			"io.katacontainers.config.hypervisor.kernel_params": "init=/bin/sh",
			"io.katacontainers.config.runtime.enable_debug":     "true",
			"customer.example.com/team":                         "platform",
		}
	})

	reconciler, fakeClient := newReconciler(t, nil, instance)

	if _, err := reconciler.Reconcile(context.Background(), instanceRequest()); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	pod, found := getPod(t, fakeClient)
	if !found {
		t.Fatal("expected the instance to be backed by a pod")
	}

	for key := range pod.Annotations {
		if _, allowed := providerRuntimeAnnotations[key]; !allowed {
			t.Errorf("pod carries annotation %q, which the provider did not set", key)
		}
	}
}

// TestReconcile_SuspendAndResume checks that suspending stops the instance
// without releasing anything else it holds, and that resuming starts it again.
func TestReconcile_SuspendAndResume(t *testing.T) {
	ctx := context.Background()
	reconciler, fakeClient := newReconciler(t, nil, newTestInstance())

	if _, err := reconciler.Reconcile(ctx, instanceRequest()); err != nil {
		t.Fatalf("initial reconcile failed: %v", err)
	}
	if _, found := getPod(t, fakeClient); !found {
		t.Fatal("expected the instance to be running before suspension")
	}

	instance := getInstance(t, fakeClient)
	instance.Status.Suspended = true
	if err := fakeClient.Status().Update(ctx, instance); err != nil {
		t.Fatalf("failed to suspend instance: %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, instanceRequest()); err != nil {
		t.Fatalf("reconcile of suspended instance failed: %v", err)
	}
	if _, found := getPod(t, fakeClient); found {
		t.Error("expected the instance process to be stopped while suspended")
	}

	// Suspension is not deletion. The instance keeps its place in the system,
	// so the provider keeps its claim on the instance.
	suspended := getInstance(t, fakeClient)
	if !hasFinalizer(suspended) {
		t.Error("expected the provider to keep its finalizer on a suspended instance")
	}

	suspended.Status.Suspended = false
	if err := fakeClient.Status().Update(ctx, suspended); err != nil {
		t.Fatalf("failed to resume instance: %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, instanceRequest()); err != nil {
		t.Fatalf("reconcile of resumed instance failed: %v", err)
	}
	if _, found := getPod(t, fakeClient); !found {
		t.Error("expected the instance to be running again after resume")
	}
}

// TestReconcile_FinalizerIsClaimScoped checks that the provider takes a
// finalizer only on the instances it backs. An instance that the provider never
// realized must not be held up on a teardown with nothing to do.
func TestReconcile_FinalizerIsClaimScoped(t *testing.T) {
	tests := []struct {
		name          string
		instance      *computev1alpha.Instance
		wantFinalizer bool
		wantPod       bool
	}{
		{
			name:          "a claimed instance is finalized",
			instance:      newTestInstance(),
			wantFinalizer: true,
			wantPod:       true,
		},
		{
			name: "a gated instance is not claimed until its gates clear",
			instance: newTestInstance(func(i *computev1alpha.Instance) {
				i.Spec.Controller = &computev1alpha.InstanceController{
					SchedulingGates: []computev1alpha.SchedulingGate{{Name: "compute.datumapis.com/quota"}},
				}
			}),
		},
		{
			name: "an instance this class does not realize is left alone",
			instance: newTestInstance(func(i *computev1alpha.Instance) {
				i.Spec.Runtime.Sandbox = nil
				i.Spec.Runtime.VirtualMachine = &computev1alpha.VirtualMachineRuntime{}
			}),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reconciler, fakeClient := newReconciler(t, nil, tc.instance)

			if _, err := reconciler.Reconcile(context.Background(), instanceRequest()); err != nil {
				t.Fatalf("reconcile failed: %v", err)
			}

			if got := hasFinalizer(getInstance(t, fakeClient)); got != tc.wantFinalizer {
				t.Errorf("finalizer present = %v, want %v", got, tc.wantFinalizer)
			}
			if _, found := getPod(t, fakeClient); found != tc.wantPod {
				t.Errorf("pod present = %v, want %v", found, tc.wantPod)
			}
		})
	}
}

// TestReconcile_TeardownOrdering checks that the provider does not report an
// instance gone while its guest is still shutting down. Releasing the finalizer
// early would tell the workload above the instance that capacity, addresses,
// and quota were free before they were.
func TestReconcile_TeardownOrdering(t *testing.T) {
	ctx := context.Background()

	deleting := newTestInstance(func(i *computev1alpha.Instance) {
		i.Finalizers = []string{instanceFinalizer}
		i.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
	})

	// A Pod with a finalizer of its own lingers after deletion, which is what a
	// guest that has not finished shutting down looks like.
	lingering := &core.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testInstanceName,
			Namespace:  testInstanceNamespace,
			Finalizers: []string{"example.com/lingering"},
			Labels:     map[string]string{managedByLabel: managedByValue},
		},
	}

	reconciler, fakeClient := newReconciler(t, nil, deleting, lingering)

	result, err := reconciler.Reconcile(ctx, instanceRequest())
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected the reconciler to wait for the instance to stop")
	}
	if !hasFinalizer(getInstance(t, fakeClient)) {
		t.Fatal("finalizer released while the instance was still running")
	}

	// The guest finishes shutting down, and its Pod goes away.
	var pod core.Pod
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(lingering), &pod); err != nil {
		t.Fatalf("failed to get pod: %v", err)
	}
	pod.Finalizers = nil
	if err := fakeClient.Update(ctx, &pod); err != nil {
		t.Fatalf("failed to release pod finalizer: %v", err)
	}
	if _, found := getPod(t, fakeClient); found {
		t.Fatal("expected the pod to be gone once its own finalizer was released")
	}

	if _, err := reconciler.Reconcile(ctx, instanceRequest()); err != nil {
		t.Fatalf("second reconcile failed: %v", err)
	}

	var instance computev1alpha.Instance
	err = fakeClient.Get(ctx, client.ObjectKey{Name: testInstanceName, Namespace: testInstanceNamespace}, &instance)
	if err == nil && hasFinalizer(&instance) {
		t.Error("expected the finalizer to be released once the instance had stopped")
	} else if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("failed to get instance: %v", err)
	}
}

// TestSyncInstanceStatus covers what a customer reads on their instance: the
// conditions that compute derives readiness from, and the words that explain a
// failure.
func TestSyncInstanceStatus(t *testing.T) {
	tests := []struct {
		name                 string
		pod                  *core.Pod
		wantProgrammed       metav1.ConditionStatus
		wantProgrammedReason string
		wantAvailable        metav1.ConditionStatus
		wantAvailableReason  string
	}{
		{
			name:                 "a running instance is programmed and available",
			pod:                  podWithPhase(core.PodRunning),
			wantProgrammed:       metav1.ConditionTrue,
			wantProgrammedReason: computev1alpha.InstanceProgrammedReasonProgrammed,
			wantAvailable:        metav1.ConditionTrue,
			wantAvailableReason:  computev1alpha.InstanceAvailableReasonAvailable,
		},
		{
			name:                 "a provisioning instance is still unknown",
			pod:                  podWithPhase(core.PodPending),
			wantProgrammed:       metav1.ConditionUnknown,
			wantProgrammedReason: computev1alpha.InstanceProgrammedReasonProgrammingInProgress,
			wantAvailable:        metav1.ConditionUnknown,
			wantAvailableReason:  computev1alpha.InstanceReadyReasonProvisioning,
		},
		{
			name:                 "an image that cannot be pulled is reported in instance language",
			pod:                  podWaitingWith("ImagePullBackOff"),
			wantProgrammed:       metav1.ConditionUnknown,
			wantProgrammedReason: computev1alpha.InstanceProgrammedReasonProgrammingInProgress,
			wantAvailable:        metav1.ConditionUnknown,
			wantAvailableReason:  computev1alpha.InstanceReadyReasonImageUnavailable,
		},
		{
			name:                 "an unrecognized runtime failure never leaks to the customer",
			pod:                  podWaitingWith("SomeNewKubeletReason"),
			wantProgrammed:       metav1.ConditionUnknown,
			wantProgrammedReason: computev1alpha.InstanceProgrammedReasonProgrammingInProgress,
			wantAvailable:        metav1.ConditionUnknown,
			wantAvailableReason:  computev1alpha.InstanceReadyReasonProvisioning,
		},
		{
			name:                 "an instance that stopped unexpectedly is not programmed",
			pod:                  podWithPhase(core.PodFailed),
			wantProgrammed:       metav1.ConditionFalse,
			wantProgrammedReason: computev1alpha.InstanceProgrammedReasonInstanceCrashing,
			wantAvailable:        metav1.ConditionFalse,
			wantAvailableReason:  computev1alpha.InstanceReadyReasonInstanceCrashing,
		},
		{
			name:                 "an instance that ran to completion is stopped",
			pod:                  podWithPhase(core.PodSucceeded),
			wantProgrammed:       metav1.ConditionFalse,
			wantProgrammedReason: computev1alpha.InstanceAvailableReasonStopped,
			wantAvailable:        metav1.ConditionFalse,
			wantAvailableReason:  computev1alpha.InstanceAvailableReasonStopped,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			instance := newTestInstance()
			reconciler, fakeClient := newReconciler(t, nil, instance)

			if err := reconciler.syncInstanceStatus(context.Background(), instance, tc.pod); err != nil {
				t.Fatalf("status sync failed: %v", err)
			}

			updated := getInstance(t, fakeClient)

			programmed := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha.InstanceProgrammed)
			if programmed == nil {
				t.Fatal("expected a Programmed condition")
			}
			if programmed.Status != tc.wantProgrammed || programmed.Reason != tc.wantProgrammedReason {
				t.Errorf("Programmed = %s/%s, want %s/%s",
					programmed.Status, programmed.Reason, tc.wantProgrammed, tc.wantProgrammedReason)
			}

			available := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha.InstanceAvailable)
			if available == nil {
				t.Fatal("expected an Available condition")
			}
			if available.Status != tc.wantAvailable || available.Reason != tc.wantAvailableReason {
				t.Errorf("Available = %s/%s, want %s/%s",
					available.Status, available.Reason, tc.wantAvailable, tc.wantAvailableReason)
			}

			// Compute derives readiness from the two conditions above.
			if apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha.InstanceReady) != nil {
				t.Error("the provider must not write the Ready condition")
			}
		})
	}
}

// TestBuildNetworkInterfaceStatus checks the addresses that the provider
// reports back to the customer for their instance.
func TestBuildNetworkInterfaceStatus(t *testing.T) {
	tests := []struct {
		name          string
		podIPs        []string
		wantName      string
		wantNetworkIP string
		wantAddresses []string
	}{
		{
			name:   "an instance with no address yet reports none",
			podIPs: nil,
		},
		{
			name:          "an IPv4 address is reported as a host address",
			podIPs:        []string{"10.128.0.7"},
			wantName:      "eth0",
			wantNetworkIP: "10.128.0.7",
			wantAddresses: []string{"10.128.0.7/32"},
		},
		{
			name:          "a dual-stack instance reports both, primary first",
			podIPs:        []string{"2001:db8::5", "10.128.0.7"},
			wantName:      "eth0",
			wantNetworkIP: "2001:db8::5",
			wantAddresses: []string{"2001:db8::5/128", "10.128.0.7/32"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pod := &core.Pod{}
			for _, ip := range tc.podIPs {
				pod.Status.PodIPs = append(pod.Status.PodIPs, core.PodIP{IP: ip})
			}

			got := buildNetworkInterfaceStatus(newTestInstance(), pod)

			if len(tc.wantAddresses) == 0 {
				if got != nil {
					t.Fatalf("expected no interface status, got %+v", got)
				}
				return
			}

			if len(got) != 1 {
				t.Fatalf("expected one interface, got %d", len(got))
			}
			if got[0].Name != tc.wantName {
				t.Errorf("interface name = %q, want %q", got[0].Name, tc.wantName)
			}
			if got[0].Assignments.NetworkIP == nil || *got[0].Assignments.NetworkIP != tc.wantNetworkIP {
				t.Errorf("networkIP = %v, want %q", got[0].Assignments.NetworkIP, tc.wantNetworkIP)
			}
			if len(got[0].Addresses) != len(tc.wantAddresses) {
				t.Fatalf("addresses = %d, want %d", len(got[0].Addresses), len(tc.wantAddresses))
			}
			for i, want := range tc.wantAddresses {
				if got[0].Addresses[i].Address != want {
					t.Errorf("address[%d] = %q, want %q", i, got[0].Addresses[i].Address, want)
				}
			}
			if !got[0].Addresses[0].Primary {
				t.Error("expected the first address to be the primary one")
			}
		})
	}
}

func hasFinalizer(instance *computev1alpha.Instance) bool {
	for _, finalizer := range instance.Finalizers {
		if finalizer == instanceFinalizer {
			return true
		}
	}
	return false
}

func podWithPhase(phase core.PodPhase) *core.Pod {
	return &core.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testInstanceName, Namespace: testInstanceNamespace},
		Status:     core.PodStatus{Phase: phase},
	}
}

// podWaitingWith builds a pending Pod whose container reports why it has not
// started, as the kubelet does for a failed image pull or a crash loop.
func podWaitingWith(k8sReason string) *core.Pod {
	pod := podWithPhase(core.PodPending)
	pod.Status.ContainerStatuses = []core.ContainerStatus{
		{
			Name: "app",
			State: core.ContainerState{
				Waiting: &core.ContainerStateWaiting{
					Reason:  k8sReason,
					Message: "internal detail that must not reach a customer",
				},
			},
		},
	}
	return pod
}

// TestReconcile_PodSecurityContext covers what a cell's PodSecurity admission
// checks on an instance Pod. A cell rejects, or at minimum flags, a Pod that
// leaves these fields unset, so an instance that omits them never starts.
func TestReconcile_PodSecurityContext(t *testing.T) {
	reconciler, fakeClient := newReconciler(t, nil, newTestInstance())

	if _, err := reconciler.Reconcile(context.Background(), instanceRequest()); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	pod, found := getPod(t, fakeClient)
	if !found {
		t.Fatal("expected the instance to be backed by a pod")
	}

	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.SeccompProfile == nil {
		t.Fatal("expected the pod to select a seccomp profile")
	}
	if got := pod.Spec.SecurityContext.SeccompProfile.Type; got != core.SeccompProfileTypeRuntimeDefault {
		t.Errorf("seccompProfile.type = %q, want %q", got, core.SeccompProfileTypeRuntimeDefault)
	}

	// The class exists to run stock images, and many of them start as root.
	// Requiring a non-root user would fail the images the class promises to
	// run, so the field stays unset and the cell enforces the baseline profile.
	if pod.Spec.SecurityContext.RunAsNonRoot != nil {
		t.Error("expected the provider to leave the user of a stock image alone")
	}

	security := pod.Spec.Containers[0].SecurityContext
	if security == nil {
		t.Fatal("expected the instance container to carry a security context")
	}
	if security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation {
		t.Error("expected privilege escalation to be denied")
	}
	if security.RunAsNonRoot != nil {
		t.Error("expected the provider to leave the user of a stock image alone")
	}
	if security.Capabilities == nil {
		t.Fatal("expected the instance container to drop capabilities")
	}
	if got := security.Capabilities.Drop; len(got) != 1 || got[0] != "ALL" {
		t.Errorf("capabilities.drop = %v, want [ALL]", got)
	}
	// Serving on a privileged port is ordinary for a stock image, and both
	// PodSecurity profiles permit adding this capability back.
	if got := security.Capabilities.Add; len(got) != 1 || got[0] != "NET_BIND_SERVICE" {
		t.Errorf("capabilities.add = %v, want [NET_BIND_SERVICE]", got)
	}
}
