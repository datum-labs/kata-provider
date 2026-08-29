// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation/field"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	"go.datum.net/compute/pkg/runtimeclass"
)

// TestCapabilities pins the published contract of the general-purpose class.
// Adding or removing a feature here changes what customers are promised, so it
// should be a deliberate edit to this table rather than a side effect.
func TestCapabilities(t *testing.T) {
	if Capabilities.Class != computev1alpha.RuntimeClassGeneralPurpose {
		t.Fatalf("class = %q, want %q", Capabilities.Class, computev1alpha.RuntimeClassGeneralPurpose)
	}

	tests := []struct {
		feature runtimeclass.Feature
		want    bool
		reason  string
	}{
		{runtimeclass.FeatureSandboxRuntime, true, "the class exists to run ordinary Linux container images"},
		{runtimeclass.FeatureConfigMapVolumes, true, "the kubelet mounts them and shares them into the guest"},
		{runtimeclass.FeatureSecretVolumes, true, "the kubelet mounts them and shares them into the guest"},
		{runtimeclass.FeatureEnvFrom, true, "the kubelet resolves them before the container starts"},
		{runtimeclass.FeatureImagePullSecrets, true, "images are pulled on the host under the named credentials"},
		{runtimeclass.FeatureVirtualMachineRuntime, false, "the guest kernel and image are platform-owned"},
		{runtimeclass.FeatureDiskVolumes, false, "no cell serving this class runs a storage integration yet"},
		{runtimeclass.FeatureDeviceVolumeAttachments, false, "a raw device attachment presupposes a disk"},
	}

	for _, tc := range tests {
		t.Run(string(tc.feature), func(t *testing.T) {
			if got := Capabilities.Supports(tc.feature); got != tc.want {
				t.Errorf("Supports(%s) = %v, want %v: %s", tc.feature, got, tc.want, tc.reason)
			}
		})
	}
}

// TestCapabilities_UnsupportedRequestsAreRejected checks that a request this
// class cannot serve is refused with the class named, rather than served with
// the unsupported part quietly dropped.
func TestCapabilities_UnsupportedRequestsAreRejected(t *testing.T) {
	tests := []struct {
		name       string
		spec       computev1alpha.InstanceSpec
		wantErrors int
	}{
		{
			name: "an ordinary container instance is accepted",
			spec: newTestInstance().Spec,
		},
		{
			name: "a disk-backed volume is refused",
			spec: newTestInstance(func(i *computev1alpha.Instance) {
				i.Spec.Volumes = []computev1alpha.InstanceVolume{
					{
						Name: "data",
						VolumeSource: computev1alpha.VolumeSource{
							Disk: &computev1alpha.DiskTemplateVolumeSource{},
						},
					},
				}
			}).Spec,
			wantErrors: 1,
		},
		{
			name: "a virtual machine instance is refused",
			spec: newTestInstance(func(i *computev1alpha.Instance) {
				i.Spec.Runtime.Sandbox = nil
				i.Spec.Runtime.VirtualMachine = &computev1alpha.VirtualMachineRuntime{}
			}).Spec,
			wantErrors: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := runtimeclass.ValidateInstanceSpec(tc.spec, Capabilities, field.NewPath("spec"))
			if len(errs) != tc.wantErrors {
				t.Fatalf("got %d rejections, want %d: %v", len(errs), tc.wantErrors, errs)
			}
			for _, err := range errs {
				if got := err.Error(); !strings.Contains(got, computev1alpha.RuntimeClassGeneralPurpose) {
					t.Errorf("rejection %q does not name the class the customer should move to", got)
				}
			}
		})
	}
}
