// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	computev1alpha "go.datum.net/compute/api/v1alpha"
	"go.datum.net/compute/pkg/runtimeclass"
)

// Capabilities declares what the general-purpose class can serve, as this
// provider realizes it: a Kata-isolated Pod on an ordinary kubelet node.
//
// The declaration is the class's published promise, so each feature is listed
// only where a Kata-backed instance genuinely delivers it. Anything absent is
// rejected at apply time with the class named, which is a better outcome for a
// customer than an instance that starts with part of their request dropped.
var Capabilities = runtimeclass.Capabilities{
	Class: computev1alpha.RuntimeClassGeneralPurpose,
	Features: []runtimeclass.Feature{
		// The class exists to run ordinary Linux container images: a real
		// kernel, a writable root filesystem, and no requirement that the
		// binary be position independent.
		runtimeclass.FeatureSandboxRuntime,

		// ConfigMap and Secret volumes are mounted by the kubelet and shared
		// into the guest, so they behave as they do on any Kubernetes node.
		runtimeclass.FeatureConfigMapVolumes,
		runtimeclass.FeatureSecretVolumes,

		// Whole-ConfigMap and whole-Secret environment sources are resolved by
		// the kubelet before the container starts, so they need nothing from
		// the runtime.
		runtimeclass.FeatureEnvFrom,

		// Images are pulled on the host by the container runtime under the
		// credentials the Pod names, exactly as for a shared-kernel container.
		runtimeclass.FeatureImagePullSecrets,
	},
}

// Features this class does not declare, and why. Each is a capability gap to
// close deliberately rather than a promise to make now.
//
//   - FeatureVirtualMachineRuntime: Kata boots a platform-owned guest kernel
//     and image to run containers in. Booting a customer-supplied VM image is
//     a different realization, not a configuration of this one.
//   - FeatureDiskVolumes: a durable disk needs a storage integration — a
//     volume source this provider can resolve — that no cell serving this
//     class runs yet. Declaring it without one would build Pods that never
//     bind.
//   - FeatureDeviceVolumeAttachments: an attachment handed to the guest as a
//     raw device presupposes a disk to attach.
