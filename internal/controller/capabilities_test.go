// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"

	computev1alpha "go.datum.net/compute/api/v1alpha"
	"go.datum.net/compute/pkg/runtimeclass"
)

// TestCapabilities pins the published contract of the general-purpose class.
// Adding or removing a feature changes what the platform promises customers, so
// any such change must be a deliberate edit to this table rather than a side
// effect.
func TestCapabilities(t *testing.T) {
	if Capabilities.Class != RuntimeClassName {
		t.Fatalf("class = %q, want %q", Capabilities.Class, RuntimeClassName)
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
		{runtimeclass.FeatureContainerCapabilities, true, "a capability acts inside the guest kernel, not on the host"},
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

// TestCapabilities_UnsupportedRequestsAreRejected checks that compute refuses a
// request this class cannot serve, and names the class, rather than serving the
// request with the unsupported part silently dropped.
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
				if got := err.Error(); !strings.Contains(got, RuntimeClassName) {
					t.Errorf("rejection %q does not name the class the customer should move to", got)
				}
			}
		})
	}
}

// registeredClassPath is the RuntimeClass this repository registers for the
// class, relative to this package.
const registeredClassPath = "../../config/components/runtime_classes/general-purpose.yaml"

// TestCapabilities_MatchRegisteredClass fails when the compiled declaration and
// the registered RuntimeClass drift apart. Compute validates instances against
// the registered object, and this provider builds Pods against the compiled
// one, so a drift either rejects what the provider could serve or admits what
// it would refuse.
func TestCapabilities_MatchRegisteredClass(t *testing.T) {
	registered := runtimeclass.CapabilitiesFrom(readRegisteredClass(t))
	if registered.Class != Capabilities.Class {
		t.Errorf("registered class = %q, compiled class = %q", registered.Class, Capabilities.Class)
	}
	if got, want := sorted(registered.Features), sorted(Capabilities.Features); !slices.Equal(got, want) {
		t.Errorf("registered features = %v, compiled features = %v", got, want)
	}
	if got, want := sorted(registered.GrantableCapabilities), sorted(Capabilities.GrantableCapabilities); !slices.Equal(got, want) {
		t.Errorf("registered grantable capabilities = %v, compiled = %v", got, want)
	}
}

// TestCapabilities_DefaultSecurityContextMatchesRegisteredClass fails when the
// compiled default and the published one drift apart. The published value is
// what a customer reads and what compute stamps onto their container, so a
// drift would have the class promise one confinement and the provider expect
// another.
func TestCapabilities_DefaultSecurityContextMatchesRegisteredClass(t *testing.T) {
	registered := readRegisteredClass(t).Spec.DefaultSecurityContext
	if registered == nil {
		t.Fatal("the registered class publishes no default security context")
	}
	if diff := cmp.Diff(DefaultSecurityContext, registered); diff != "" {
		t.Errorf("registered default security context differs from the compiled one (-compiled +registered):\n%s", diff)
	}
}

// TestCapabilities_DefaultSecurityContextIsGrantable pins that every capability
// the class grants by default is one a customer could also request. A default
// outside the grantable set would be a privilege only the platform can hand
// out, which is the invisible grant the published default replaces.
func TestCapabilities_DefaultSecurityContextIsGrantable(t *testing.T) {
	for _, capability := range DefaultSecurityContext.Capabilities.Add {
		if !Capabilities.Grants(capability) {
			t.Errorf("Grants(%s) = false; the class grants it by default but would refuse the request", capability)
		}
	}
}

// TestCapabilities_DefaultSecurityContextStaysMinimal pins the decision behind
// the default set: Docker's default capabilities less the four that reach past
// an ordinary application. Widening it grants every customer a privilege they
// did not ask for, so any change here is deliberate.
func TestCapabilities_DefaultSecurityContextStaysMinimal(t *testing.T) {
	want := []runtimeclass.Capability{
		"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL",
		"NET_BIND_SERVICE", "SETGID", "SETPCAP", "SETUID",
	}
	if got := sorted(DefaultSecurityContext.Capabilities.Add); !slices.Equal(got, want) {
		t.Errorf("default capabilities = %v, want %v", got, want)
	}
	for _, withheld := range []runtimeclass.Capability{"NET_RAW", "SYS_CHROOT", "MKNOD", "AUDIT_WRITE"} {
		if slices.Contains(DefaultSecurityContext.Capabilities.Add, withheld) {
			t.Errorf("%s is granted by default; it reaches past an ordinary application and must be requested", withheld)
		}
	}
	if DefaultSecurityContext.AllowPrivilegeEscalation == nil || *DefaultSecurityContext.AllowPrivilegeEscalation {
		t.Error("expected privilege escalation to be denied by default")
	}
	if DefaultSecurityContext.SeccompProfile == nil ||
		DefaultSecurityContext.SeccompProfile.Type != computev1alpha.SeccompProfileTypeRuntimeDefault {
		t.Error("expected the runtime's own seccomp profile by default")
	}
}

// TestCapabilities_GrantsEveryLinuxCapability pins the decision that a
// general-purpose container may request any Linux capability, because the guest
// kernel confines it. Narrowing the set is a change to what customers are
// promised.
func TestCapabilities_GrantsEveryLinuxCapability(t *testing.T) {
	const linuxCapabilityCount = 41

	unique := slices.Compact(sorted(Capabilities.GrantableCapabilities))
	if len(unique) != linuxCapabilityCount || len(Capabilities.GrantableCapabilities) != linuxCapabilityCount {
		t.Fatalf("grantable capabilities = %d (%d unique), want every one of the %d Linux capabilities",
			len(Capabilities.GrantableCapabilities), len(unique), linuxCapabilityCount)
	}
	for _, capability := range []runtimeclass.Capability{capChown, capSetuid, capSetgid, "DAC_OVERRIDE", capSysAdmin, capNetAdmin} {
		if !Capabilities.Grants(capability) {
			t.Errorf("Grants(%s) = false, want true", capability)
		}
	}
	if Capabilities.Grants(computev1alpha.CapabilityAll) {
		t.Error("Grants(ALL) = true; a container must name what it needs")
	}
}

// readRegisteredClass parses the RuntimeClass this repository registers.
func readRegisteredClass(t *testing.T) *computev1alpha.RuntimeClass {
	t.Helper()

	raw, err := os.ReadFile(registeredClassPath)
	if err != nil {
		t.Fatalf("failed to read the registered class: %v", err)
	}
	var class computev1alpha.RuntimeClass
	if err := yaml.UnmarshalStrict(raw, &class); err != nil {
		t.Fatalf("failed to parse the registered class: %v", err)
	}
	return &class
}

func sorted[S ~[]E, E ~string](s S) S {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}
