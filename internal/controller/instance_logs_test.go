// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"strconv"
	"testing"

	"go.datum.net/kata-provider/internal/config"
)

// Switching collection ownership must update existing Pods as well as new
// ones. Tenant metadata must not select or suppress a collection path.
func TestReconcile_InstanceLogCollectionOwnership(t *testing.T) {
	enabledLabel := strconv.FormatBool(true)
	ctx := context.Background()
	instance := newTestInstance()
	instance.Labels[nativeLogsLabel] = enabledLabel
	instance.Annotations = map[string]string{nativeLogsLabel: enabledLabel}
	reconciler, fakeClient := newReconciler(t, nil, instance)

	for _, tc := range []struct {
		name    string
		config  *config.KataProvider
		enabled bool
	}{
		{name: "tenant cannot opt in"},
		{
			name: "platform enables instance logs",
			config: &config.KataProvider{DownstreamResourceManagement: config.DownstreamResourceManagementConfig{
				InstanceLogs: true,
			}},
			enabled: true,
		},
		{name: "platform restores shared collection", config: &config.KataProvider{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reconciler.Config = tc.config
			if _, err := reconciler.Reconcile(ctx, instanceRequest()); err != nil {
				t.Fatalf("reconcile failed: %v", err)
			}
			pod, found := getPod(t, fakeClient)
			if !found {
				t.Fatal("instance Pod not found")
			}
			label, present := pod.Labels[nativeLogsLabel]
			if present != tc.enabled || (tc.enabled && label != enabledLabel) {
				t.Errorf("collection label = %q (present %t), want enabled %t", label, present, tc.enabled)
			}
			if _, present := pod.Annotations[nativeLogsLabel]; present {
				t.Error("tenant log annotation copied to Pod")
			}
		})
	}
}
