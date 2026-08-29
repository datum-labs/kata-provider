// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha "go.datum.net/compute/api/v1alpha"
)

// CacheOptions scopes the manager's cache to the objects this provider works
// with: the Instances of the runtime class it serves, and the Pods it created
// for them.
//
// Providers partition a cell's Instances by class, and this is where the
// partition has to be enforced. A controller-runtime predicate would drop the
// other class's events but only after the cache had already listed, watched,
// and stored every Instance in the cell — a cost that has OOM crash-looped a
// provider in this system, at which point delete reconciles stop running and
// instances wedge in Terminating. Selecting on the cache means the informer
// never asks for them.
//
// Filtering by class rather than by workload shape is also what keeps two
// providers in one cell from both claiming an instance, or neither claiming it.
func CacheOptions() cache.Options {
	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&computev1alpha.Instance{}: {
				Label: computev1alpha.InstanceRuntimeClassSelector(computev1alpha.RuntimeClassGeneralPurpose),
			},
			// A cell runs Pods this provider did not create — the other
			// class's, the platform's own — and caching all of them carries the
			// same cost for the same reason.
			&core.Pod{}: {
				Label: labels.SelectorFromSet(labels.Set{managedByLabel: managedByValue}),
			},
		},
	}
}
