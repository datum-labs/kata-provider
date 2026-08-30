// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha "go.datum.net/compute/api/v1alpha"
)

// CacheOptions scopes the manager's cache to the objects that this provider
// works with: the Instances of the runtime class it serves, and the Pods it
// created for those Instances.
//
// Providers partition a cell's Instances by class, and the cache is where the
// partition has to be enforced. A controller-runtime predicate drops the other
// class's events, but only after the informer cache has already listed,
// watched, and stored every Instance in the cell. That memory cost has
// crash-looped a provider in this system with an out-of-memory kill. Once a
// provider crash-loops, its delete reconciles stop running and its instances
// wedge in Terminating. Selecting on the cache means the informer never asks
// for the other class's Instances at all.
//
// Filtering by class rather than by workload shape is also what keeps two
// providers in one cell from both claiming an instance, or from neither
// claiming it.
func CacheOptions() cache.Options {
	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&computev1alpha.Instance{}: {
				Label: computev1alpha.InstanceRuntimeClassSelector(computev1alpha.RuntimeClassGeneralPurpose),
			},
			// A cell runs Pods that this provider did not create, including
			// the other class's Pods and the platform's own. Caching all of
			// them carries the same memory cost for the same reason.
			&core.Pod{}: {
				Label: labels.SelectorFromSet(labels.Set{managedByLabel: managedByValue}),
			},
		},
	}
}
