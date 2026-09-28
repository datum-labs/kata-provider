// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	"go.datum.net/kata-provider/internal/config"
)

// The provider owns collection for new and existing Pods. Neither tenant
// metadata nor an absent provider configuration can suppress the Kata pipeline.
func TestReconcile_InstanceLogCollectionOwnership(t *testing.T) {
	for _, tc := range []struct {
		name        string
		config      *config.KataProvider
		tenantLabel string
	}{
		{name: "no configuration or tenant label"},
		{name: "default configuration", config: &config.KataProvider{}},
		{name: "tenant cannot disable collection", tenantLabel: "false"},
		{name: "tenant cannot change collection ownership", tenantLabel: nativeLogsLabelValue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			instance := newTestInstance()
			if tc.tenantLabel != "" {
				instance.Labels[nativeLogsLabel] = tc.tenantLabel
			}
			instance.Annotations = map[string]string{nativeLogsLabel: "false"}
			reconciler, fakeClient := newReconciler(t, tc.config, instance)

			assertCollection := func() {
				t.Helper()
				if _, err := reconciler.Reconcile(ctx, instanceRequest()); err != nil {
					t.Fatalf("reconcile failed: %v", err)
				}
				pod, found := getPod(t, fakeClient)
				if !found {
					t.Fatal("instance Pod not found")
				}
				if got := pod.Labels[nativeLogsLabel]; got != nativeLogsLabelValue {
					t.Errorf("collection label = %q, want true", got)
				}
				if _, present := pod.Annotations[nativeLogsLabel]; present {
					t.Error("tenant log annotation copied to Pod")
				}
			}

			assertCollection()
			for _, staleLabel := range []string{"false", ""} {
				pod, _ := getPod(t, fakeClient)
				if staleLabel == "" {
					delete(pod.Labels, nativeLogsLabel)
				} else {
					pod.Labels[nativeLogsLabel] = staleLabel
				}
				if err := fakeClient.Update(ctx, pod); err != nil {
					t.Fatalf("update existing Pod: %v", err)
				}
				assertCollection()
			}
		})
	}
}
