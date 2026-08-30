# Kata Provider

The Datum compute provider for the **`general-purpose` runtime class**: an
instance runs the customer's own Linux container image, isolated in its own
kernel by [Kata Containers](https://katacontainers.io) rather than sharing the
host's.

A runtime class is a published promise about isolation, image compatibility,
startup latency, and price, and a customer selects one on their workload. The
platform owns the catalog of classes; this repository is one provider serving
one entry in it. The `unikernel` class — very fast starts, very low per-instance
overhead, narrow image compatibility — is served by a separate provider,
released and operated separately, so a fault in one class cannot take the other
down.

What this class offers, and what it does not:

| | |
| --- | --- |
| **Isolation** | A per-instance kernel and virtual machine boundary, not a shared kernel. |
| **Compatibility** | Ordinary Linux container images. No position-independent binary requirement, no RAM-resident root filesystem. |
| **Startup** | Slower than the unikernel class: a guest kernel boots per instance. |
| **Not served** | Virtual machine instances booting a customer-supplied image, and disk-backed volumes. Both are refused at apply time, naming the class, rather than quietly dropped. |

The provider only ever claims the Instances labelled with the class it serves.
Instances of another class in the same cell are not read, not cached, and not
touched.

## What it requires from the cluster

The provider realizes an instance as a Pod on a real kubelet node, so the cell
cluster it runs in must already provide:

1. **Kata Containers installed on the nodes** that will run instances — normally
   via [`kata-deploy`](https://github.com/kata-containers/kata-containers/tree/main/tools/packaging/kata-deploy).
   The nodes must support hardware virtualization.
2. **A Kubernetes `RuntimeClass` object** naming the Kata handler. This is *not*
   Datum's runtime class: it is the node-level binding that points the kubelet
   at Kata. `kata-deploy` creates it; where the runtime is installed by other
   means, enable the `kata_runtimeclass` component. The handler defaults to
   `kata-clh`, Cloud Hypervisor, which reserves far less memory per instance
   than QEMU and so raises instance density. The handler stays configurable,
   because it is a deployment choice — and an arm64 cluster must set
   `kata-qemu`, since Kata 4.x builds Cloud Hypervisor for x86_64 only.
3. **Labelled nodes.** Instance Pods select `katacontainers.io/kata-runtime=true`,
   which `kata-deploy` applies to every node it has installed the runtime on.
   Override the selector, and add tolerations, in the provider's config.
4. **The compute CRDs**, which are owned and published by the compute control
   plane, not by this repository.

## Deploying

The manifests are kustomize base + components + overlays:

```
config/base/manager      the Deployment, its Service, ServiceAccount, and config file
config/components/       opt-in pieces: controller_rbac, leader_election, kata_runtimeclass
config/overlays/cell     what a cell runs: leader election, RBAC, control-plane scheduling
config/overlays/dev      a single dev cluster: RBAC and a locally declared RuntimeClass
```

```bash
kubectl apply -k config/overlays/cell
```

The ClusterRole in `config/components/controller_rbac/role.yaml` is generated
from the kubebuilder markers in `internal/`. Change the markers and run
`make manifests`; a hand-edit there disappears on the next regeneration and
leaves the controller wedged on a denied informer.

## Running locally

```bash
make install                     # compute CRDs into the current cluster
make run                         # the provider, against config/base/manager/config.yaml
kubectl apply -f config/samples/instance.yaml
kubectl get instances.compute.datumapis.com
```

Without Kata on the node the Pod will not be admitted, which is the correct
outcome: this provider will not fall back to a shared kernel.

## Testing

```bash
make test    # unit tests
make lint
```
