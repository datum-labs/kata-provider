// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"

	"go.datum.net/kata-provider/internal/config"
)

// vpcEnabledConfig is a provider in a cell that runs the VPC controller.
func vpcEnabledConfig() *config.KataProvider {
	return &config.KataProvider{
		DownstreamResourceManagement: config.DownstreamResourceManagementConfig{
			EnableVPCNetworking: true,
		},
	}
}

// instanceRequestingInterface is an instance asking for one interface on a
// tenant network, which is how compute shapes one by default.
func instanceRequestingInterface() *computev1alpha.Instance {
	return newTestInstance()
}

// instanceWithoutInterfaces is an instance that asks for no network of its own.
func instanceWithoutInterfaces() *computev1alpha.Instance {
	return newTestInstance(func(i *computev1alpha.Instance) {
		i.Spec.NetworkInterfaces = nil
	})
}

// TestReconcile_InterfaceInjectionLabel covers the provider's whole networking
// contribution. The opt-in label is stamped only where an interface is wanted
// and the cell can serve it.
func TestReconcile_InterfaceInjectionLabel(t *testing.T) {
	tests := []struct {
		name      string
		config    *config.KataProvider
		instance  func() *computev1alpha.Instance
		wantLabel string
	}{
		{
			name:      "stamped when the instance requests an interface",
			config:    vpcEnabledConfig(),
			instance:  instanceRequestingInterface,
			wantLabel: "true",
		},
		{
			name:     "not stamped when the instance requests no interfaces",
			config:   vpcEnabledConfig(),
			instance: instanceWithoutInterfaces,
		},
		{
			name:     "not stamped in a cell that runs no tenant networking",
			config:   &config.KataProvider{},
			instance: instanceRequestingInterface,
		},
		{
			name:     "not stamped when the provider has no config",
			instance: instanceRequestingInterface,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reconciler, fakeClient := newReconciler(t, tc.config, tc.instance())

			if _, err := reconciler.Reconcile(context.Background(), instanceRequest()); err != nil {
				t.Fatalf("reconcile failed: %v", err)
			}

			pod, found := getPod(t, fakeClient)
			if !found {
				t.Fatal("expected the instance to be backed by a pod")
			}

			if got := pod.Labels[injectInterfacesLabel]; got != tc.wantLabel {
				t.Errorf("label %s = %q, want %q", injectInterfacesLabel, got, tc.wantLabel)
			}

			// The webhook's objectSelector cannot select on annotations, so the
			// opt-in only works as a label.
			if got := pod.Annotations[injectInterfacesLabel]; got != "" {
				t.Errorf("opt-in must not be stamped as an annotation, got %q", got)
			}
		})
	}
}

// TestReconcile_InterfaceInjectionLabelFollowsConfig checks that the opt-in
// tracks the cell's configuration on a Pod that already exists. A label that is
// only ever added strands the Pods of a cell that turns tenant networking back
// off.
func TestReconcile_InterfaceInjectionLabelFollowsConfig(t *testing.T) {
	ctx := context.Background()
	reconciler, fakeClient := newReconciler(t, vpcEnabledConfig(), newTestInstance())

	if _, err := reconciler.Reconcile(ctx, instanceRequest()); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	pod, found := getPod(t, fakeClient)
	if !found {
		t.Fatal("expected the instance to be backed by a pod")
	}
	if got := pod.Labels[injectInterfacesLabel]; got != "true" {
		t.Fatalf("label %s = %q, want %q", injectInterfacesLabel, got, "true")
	}

	reconciler.Config = &config.KataProvider{}
	if _, err := reconciler.Reconcile(ctx, instanceRequest()); err != nil {
		t.Fatalf("reconcile after disabling failed: %v", err)
	}

	pod, found = getPod(t, fakeClient)
	if !found {
		t.Fatal("expected the instance to still be backed by a pod")
	}
	if got, present := pod.Labels[injectInterfacesLabel]; present {
		t.Errorf("label %s = %q, want it removed", injectInterfacesLabel, got)
	}
}

// TestReconcile_InjectedAnnotationsSurvive checks that the annotations the
// networking webhook delivers stay on the Pod across a later patch.
//
// The provider strips every io.katacontainers.* annotation it did not set,
// because Kata reads those as host-root configuration. The webhook writes
// outside that namespace, so the filter must leave its annotations alone.
func TestReconcile_InjectedAnnotationsSurvive(t *testing.T) {
	ctx := context.Background()
	reconciler, fakeClient := newReconciler(t, vpcEnabledConfig(), newTestInstance())

	if _, err := reconciler.Reconcile(ctx, instanceRequest()); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	// Stand in for the webhook, which runs at admission and is not in play
	// against a fake client.
	pod, found := getPod(t, fakeClient)
	if !found {
		t.Fatal("expected the instance to be backed by a pod")
	}
	injected := map[string]string{
		"k8s.v1.cni.cncf.io/networks":                     "default/test-instance-eth0",
		"v1.multus-cni.io/default-network":                "default/test-instance-eth0",
		"networking.datumapis.com/injected-interfaces":    "test-instance-eth0",
		"io.katacontainers.config.hypervisor.kernel_para": "init=/bin/sh",
	}
	pod.Annotations = injected
	if err := fakeClient.Update(ctx, pod); err != nil {
		t.Fatalf("failed to annotate pod: %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, instanceRequest()); err != nil {
		t.Fatalf("second reconcile failed: %v", err)
	}

	pod, found = getPod(t, fakeClient)
	if !found {
		t.Fatal("expected the instance to still be backed by a pod")
	}
	for _, key := range []string{
		"k8s.v1.cni.cncf.io/networks",
		"v1.multus-cni.io/default-network",
		"networking.datumapis.com/injected-interfaces",
	} {
		if pod.Annotations[key] == "" {
			t.Errorf("annotation %q was dropped; the instance would lose its network", key)
		}
	}
	// The tenant isolation defense still holds alongside the injected keys.
	if _, present := pod.Annotations["io.katacontainers.config.hypervisor.kernel_para"]; present {
		t.Error("a runtime annotation survived; it must never reach the runtime")
	}
}

// TestProviderOwnsInterfaceStatus covers who publishes an instance's addresses.
//
// Compute publishes them wherever the platform allocates them, including the
// NetworkInterface reference that interface injection reads to admit the Pod.
// Overwriting that entry with the Pod's cluster addresses drops the reference,
// and the instance is then refused its interfaces.
func TestProviderOwnsInterfaceStatus(t *testing.T) {
	withPublishedRef := newTestInstance(func(i *computev1alpha.Instance) {
		i.Status.NetworkInterfaces = []computev1alpha.InstanceNetworkInterfaceStatus{{
			Name:                "eth0",
			NetworkInterfaceRef: &networkingv1alpha.LocalNetworkInterfaceRef{Name: "test-instance-eth0"},
		}}
	})

	tests := []struct {
		name     string
		config   *config.KataProvider
		instance *computev1alpha.Instance
		want     bool
	}{
		{
			name:     "the pod address is the only address in a cell without tenant networking",
			config:   &config.KataProvider{},
			instance: newTestInstance(),
			want:     true,
		},
		{
			name:     "compute owns the field in a cell that allocates tenant addresses",
			config:   vpcEnabledConfig(),
			instance: newTestInstance(),
		},
		{
			name:     "a published reference defers the provider even before it opts in",
			config:   &config.KataProvider{},
			instance: withPublishedRef,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reconciler, _ := newReconciler(t, tc.config, tc.instance)

			if got := reconciler.providerOwnsInterfaceStatus(tc.instance); got != tc.want {
				t.Errorf("providerOwnsInterfaceStatus = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSyncInstanceStatus_LeavesTenantAddressesAlone is the end-to-end form of
// the same guarantee. A status sync must not cost the instance the reference
// that admits its Pod.
func TestSyncInstanceStatus_LeavesTenantAddressesAlone(t *testing.T) {
	ctx := context.Background()

	published := []computev1alpha.InstanceNetworkInterfaceStatus{{
		Name:                "eth0",
		NetworkInterfaceRef: &networkingv1alpha.LocalNetworkInterfaceRef{Name: "test-instance-eth0"},
		Addresses: []computev1alpha.InstanceNetworkInterfaceAddress{{
			Family:  networkingv1alpha.IPv6Protocol,
			Address: "fd20:0:f::4:0:0/128",
			Primary: true,
		}},
	}}

	instance := newTestInstance(func(i *computev1alpha.Instance) {
		i.Status.NetworkInterfaces = published
	})
	reconciler, fakeClient := newReconciler(t, vpcEnabledConfig(), instance)

	// A Pod holding a cluster address from cilium.
	pod := podWithPhase(core.PodRunning)
	pod.Status.PodIPs = []core.PodIP{{IP: "10.244.1.7"}}

	if err := reconciler.syncInstanceStatus(ctx, instance, pod); err != nil {
		t.Fatalf("status sync failed: %v", err)
	}

	updated := getInstance(t, fakeClient)
	if len(updated.Status.NetworkInterfaces) != 1 {
		t.Fatalf("interfaces = %d, want 1", len(updated.Status.NetworkInterfaces))
	}
	if updated.Status.NetworkInterfaces[0].NetworkInterfaceRef == nil {
		t.Fatal("the reference that admits the instance's pod was dropped")
	}
	if got := updated.Status.NetworkInterfaces[0].Addresses[0].Address; got != "fd20:0:f::4:0:0/128" {
		t.Errorf("address = %q, want the tenant network address", got)
	}

	// The conditions the provider does own are still published.
	if !hasCondition(updated, computev1alpha.InstanceProgrammed, metav1.ConditionTrue) {
		t.Error("expected the provider to still report the instance programmed")
	}
}

// TestSyncInstanceStatus_PublishesPodAddressesWithoutTenantNetworking checks
// that a cell with no tenant networking still tells the customer where their
// instance is, because the Pod's address is the only one it has.
func TestSyncInstanceStatus_PublishesPodAddressesWithoutTenantNetworking(t *testing.T) {
	ctx := context.Background()

	instance := newTestInstance()
	reconciler, fakeClient := newReconciler(t, &config.KataProvider{}, instance)

	pod := podWithPhase(core.PodRunning)
	pod.Status.PodIPs = []core.PodIP{{IP: "10.244.1.7"}}

	if err := reconciler.syncInstanceStatus(ctx, instance, pod); err != nil {
		t.Fatalf("status sync failed: %v", err)
	}

	updated := getInstance(t, fakeClient)
	if len(updated.Status.NetworkInterfaces) != 1 {
		t.Fatalf("interfaces = %d, want 1", len(updated.Status.NetworkInterfaces))
	}
	if got := updated.Status.NetworkInterfaces[0].Addresses[0].Address; got != "10.244.1.7/32" {
		t.Errorf("address = %q, want the pod address", got)
	}
}

func hasCondition(instance *computev1alpha.Instance, conditionType string, status metav1.ConditionStatus) bool {
	for _, condition := range instance.Status.Conditions {
		if condition.Type == conditionType {
			return condition.Status == status
		}
	}
	return false
}
