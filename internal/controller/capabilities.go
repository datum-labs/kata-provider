// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"go.datum.net/compute/pkg/runtimeclass"
)

// RuntimeClassName is the class this provider serves. A provider names its own
// class, because a class name compiled into the platform would be a tier the
// catalog could not retire. The name matches the RuntimeClass this repository
// registers, and the two must stay in step: the provider claims an instance by
// this name, so a mismatch leaves every instance of the class unclaimed.
const RuntimeClassName = "general-purpose"

// Capabilities declares what the general-purpose class can serve, as this
// provider realizes it: a Kata-isolated Pod on an ordinary kubelet node.
//
// The declaration is the class's published promise, so it lists a feature only
// where a Kata-backed instance genuinely delivers that feature. Compute rejects
// a request for an absent feature at apply time and names the class. That is a
// better outcome for a customer than an instance that starts with part of their
// request dropped.
var Capabilities = runtimeclass.Capabilities{
	Class: RuntimeClassName,
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

		// A stock image often needs a capability or two to start, for example
		// to change file ownership. See linuxCapabilities for why the class
		// grants every one.
		runtimeclass.FeatureContainerCapabilities,
	},
	GrantableCapabilities: linuxCapabilities,
}

// linuxCapabilities is every Linux capability, which is what the class grants.
//
// A general-purpose instance is a virtual machine, so a capability acts on the
// customer's own guest kernel rather than on the host. That boundary is why the
// class sits outside the cell's security profile. What would reach the host is
// not a capability but a Pod field, such as a host namespace, host port, or host
// path, and the provider never sets one. See
// TestReconcile_SubmittedPodNeverReachesTheHost.
var linuxCapabilities = []runtimeclass.Capability{
	"AUDIT_CONTROL", "AUDIT_READ", "AUDIT_WRITE", "BLOCK_SUSPEND", "BPF",
	"CHECKPOINT_RESTORE", "CHOWN", "DAC_OVERRIDE", "DAC_READ_SEARCH", "FOWNER",
	"FSETID", "IPC_LOCK", "IPC_OWNER", "KILL", "LEASE", "LINUX_IMMUTABLE",
	"MAC_ADMIN", "MAC_OVERRIDE", "MKNOD", "NET_ADMIN", "NET_BIND_SERVICE",
	"NET_BROADCAST", "NET_RAW", "PERFMON", "SETFCAP", "SETGID", "SETPCAP",
	"SETUID", "SYSLOG", "SYS_ADMIN", "SYS_BOOT", "SYS_CHROOT", "SYS_MODULE",
	"SYS_NICE", "SYS_PACCT", "SYS_PTRACE", "SYS_RAWIO", "SYS_RESOURCE",
	"SYS_TIME", "SYS_TTY_CONFIG", "WAKE_ALARM",
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
