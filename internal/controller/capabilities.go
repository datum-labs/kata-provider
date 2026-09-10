// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	computev1alpha "go.datum.net/compute/api/v1alpha"
	"go.datum.net/compute/pkg/runtimeclass"
)

// Capabilities declares what the general-purpose class can serve, as this
// provider realizes it: a Kata-isolated Pod on an ordinary kubelet node.
//
// The declaration is the class's published promise, so it lists a feature only
// where a Kata-backed instance genuinely delivers that feature. Compute rejects
// a request for an absent feature at apply time and names the class. That is a
// better outcome for a customer than an instance that starts with part of their
// request dropped.
var Capabilities = runtimeclass.Capabilities{
	Class: computev1alpha.RuntimeClassGeneralPurpose,
	Features: []runtimeclass.Feature{
		// The class exists to run ordinary Linux container images. Such an
		// image gets a real kernel and a writable root filesystem, and its
		// binary does not have to be position independent.
		runtimeclass.FeatureSandboxRuntime,

		// The kubelet mounts ConfigMap and Secret volumes and shares them into
		// the guest, so they behave as they do on any Kubernetes node.
		runtimeclass.FeatureConfigMapVolumes,
		runtimeclass.FeatureSecretVolumes,

		// The kubelet resolves whole-ConfigMap and whole-Secret environment
		// sources before the container starts, so they need nothing from the
		// runtime.
		runtimeclass.FeatureEnvFrom,

		// The container runtime pulls images on the host, under the credentials
		// that the Pod names, exactly as it does for a shared-kernel container.
		runtimeclass.FeatureImagePullSecrets,
	},
}

// Features that this class does not declare, and why. Each entry is a
// capability gap to close deliberately rather than a promise to make now.
//
//   - FeatureVirtualMachineRuntime: Kata boots a platform-owned guest kernel
//     and image, then runs containers inside that guest. Booting a
//     customer-supplied virtual machine image is a different realization, not a
//     configuration of this one.
//   - FeatureDiskVolumes: a durable disk needs a storage integration that
//     supplies a volume source this provider can resolve. No cell serving this
//     class runs such an integration yet. Declaring the feature without one
//     would build Pods that never bind.
//   - FeatureDeviceVolumeAttachments: an attachment handed to the guest as a
//     raw device presupposes a disk to attach.
