// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"testing"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha "go.datum.net/compute/api/v1alpha"
)

// otherRuntimeClassName stands for a class served by a different provider in
// the same cell. The cache test needs a name this provider must not claim, and
// any name other than its own does.
const otherRuntimeClassName = "unikernel"

// TestCacheOptions_ClaimsByRuntimeClass checks that the provider claims exactly
// the instances of the class it serves, and that it claims them on the cache
// rather than after the fact. Two providers share a cell, so an instance
// claimed by both providers, or by neither, is a failure with no symptom in
// status.
func TestCacheOptions_ClaimsByRuntimeClass(t *testing.T) {
	byObject, ok := byObjectFor[*computev1alpha.Instance](CacheOptions())
	if !ok {
		// Without a cache selector, the informer lists and stores every
		// Instance in the cell, whatever an event predicate does afterwards.
		t.Fatal("Instances must be selected on the cache, not only on events")
	}
	if byObject.Label == nil {
		t.Fatal("expected a label selector scoping the Instance cache")
	}

	tests := []struct {
		name       string
		labels     map[string]string
		wantClaim  bool
		wantReason string
	}{
		{
			name:       "an instance in this class is claimed",
			labels:     map[string]string{computev1alpha.RuntimeClassLabel: RuntimeClassName},
			wantClaim:  true,
			wantReason: "this is the class the provider serves",
		},
		{
			name:       "another class's instance is left to its own provider",
			labels:     map[string]string{computev1alpha.RuntimeClassLabel: otherRuntimeClassName},
			wantReason: "claiming it would run a unikernel workload on the wrong runtime",
		},
		{
			name:       "an unlabelled instance is not claimed by shape",
			labels:     nil,
			wantReason: "workload shape does not identify whose instance it is",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := byObject.Label.Matches(labels.Set(tc.labels)); got != tc.wantClaim {
				t.Errorf("claimed = %v, want %v: %s", got, tc.wantClaim, tc.wantReason)
			}
		})
	}
}

// TestCacheOptions_ScopesPods checks that the provider caches only the Pods it
// created. A cell runs many more Pods than that, and caching all of them costs
// the memory that has crash-looped a provider here before.
func TestCacheOptions_ScopesPods(t *testing.T) {
	byObject, ok := byObjectFor[*core.Pod](CacheOptions())
	if !ok {
		t.Fatal("expected the Pod cache to be scoped")
	}

	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{
			name:   "a pod this provider created is cached",
			labels: map[string]string{managedByLabel: managedByValue},
			want:   true,
		},
		{
			name:   "another component's pod is not",
			labels: map[string]string{managedByLabel: "some-other-provider"},
		},
		{
			name: "an unrelated pod is not",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := byObject.Label.Matches(labels.Set(tc.labels)); got != tc.want {
				t.Errorf("cached = %v, want %v", got, tc.want)
			}
		})
	}
}

// byObjectFor finds the cache settings for a type. The options are keyed by a
// sample object, so this helper matches them by type rather than by identity.
func byObjectFor[T client.Object](options cache.Options) (cache.ByObject, bool) {
	for object, byObject := range options.ByObject {
		if _, match := object.(T); match {
			return byObject, true
		}
	}
	return cache.ByObject{}, false
}
