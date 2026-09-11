// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	computev1alpha "go.datum.net/compute/api/v1alpha"
)

// injectInterfacesLabel asks the networking stack to wire this Pod up to the
// interfaces its instance requested. The webhook that matches on the label owns
// how that happens, so both runtime classes reach a tenant network the same
// way.
//
// A label rather than an annotation, because a webhook objectSelector can only
// select on labels.
const injectInterfacesLabel = "networking.datumapis.com/inject-interfaces"

// vpcNetworkingEnabled reports whether this cell wires instances onto tenant
// networks.
func (r *InstanceReconciler) vpcNetworkingEnabled() bool {
	return r.Config != nil && r.Config.DownstreamResourceManagement.EnableVPCNetworking
}

// requestsInterfaceInjection reports whether an instance Pod should carry the
// opt-in label.
//
// The webhook's objectSelector matches on exactly this label. That narrowness
// makes its failurePolicy of Fail safe, because an outage blocks only the Pods
// that need an interface.
func (r *InstanceReconciler) requestsInterfaceInjection(instance *computev1alpha.Instance) bool {
	return r.vpcNetworkingEnabled() && len(instance.Spec.NetworkInterfaces) > 0
}

// providerOwnsInterfaceStatus reports whether the Pod's addresses are the
// instance's addresses, and therefore whether this provider may publish them.
//
// Compute owns the field wherever the platform allocates the addresses. It
// publishes the tenant network address and the NetworkInterface reference that
// interface injection reads to admit the next instance Pod. Overwriting that
// entry with the Pod's cluster addresses drops the reference, and the instance
// never starts.
//
// The published reference is the second test because a cell can run the
// platform's address allocation before this provider opts in. Deferring on that
// evidence keeps the provider out of the field during a rollout.
func (r *InstanceReconciler) providerOwnsInterfaceStatus(instance *computev1alpha.Instance) bool {
	if r.vpcNetworkingEnabled() {
		return false
	}

	for _, networkInterface := range instance.Status.NetworkInterfaces {
		if networkInterface.NetworkInterfaceRef != nil {
			return false
		}
	}

	return true
}
