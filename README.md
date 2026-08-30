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

### End-to-end

The end-to-end suites run the provider in a real cluster and assert on what a
customer and an operator would see: an instance that starts behind the runtime
its class promised, at the size it was sold, reachable at an address, stopped
when suspended, and never reported gone while it is still running.

They run in two tiers, and the tiers share their suites rather than duplicating
them.

| | `runc` | `kata` |
| --- | --- | --- |
| What executes an instance | A `RuntimeClass` named as the provider expects, handled by runc | Real Kata Containers |
| Needs hardware virtualization | No | Yes |
| Runs in CI | Yes — this is the gate | Yes, but non-blocking |
| Proves | Everything the provider decides | The above, plus isolation |

The portable tier is not a weaker version of the same test. Which instances the
provider claims, the instance it builds, the runtime class it names, where it
schedules, what it strips, what it reports, and the order it tears things down
in are all decided before any runtime executes anything, so they are provable
without a hypervisor — on any laptop, and on a GitHub runner. What genuinely
needs a guest is labelled `tier=kata` and is excluded from the portable tier
rather than softened to pass in it.

```bash
task e2e                # portable tier: create or reuse the cluster, deploy, run
task e2e TIER=kata      # every suite, against real Kata Containers
task e2e:test           # just the suites, against an environment already up
task e2e:down           # delete that tier's cluster
```

Each step is create-or-reuse, so the normal loop is `task e2e` again rather than
a cluster rebuild. The compute CRDs are installed from the exact module version
in `go.mod`, not from a branch, so the cluster's schema and the compiled API
cannot disagree. The provider is deployed in-cluster from `test/e2e/deploy`,
which is the real base and the real generated RBAC — a missing grant fails a
test here instead of shipping.

`.status.Ready` and `.status.QuotaGranted` stay Pending throughout, and the
suites assert that. Both belong to compute, no compute controller runs in this
environment, and a provider writing them would be a bug rather than a
convenience.

#### Prerequisite for the Kata tier

The Kata tier needs a Linux host with KVM. On a Mac that means a VM with nested
virtualization, which is a dedicated colima profile — never the default one:

```bash
colima start kata --vm-type vz --nested-virtualization \
  --cpu 8 --memory 16 --disk 60 --runtime docker
```

The e2e scripts find that profile's socket themselves and pass it through the
environment, so they change neither your `docker context` nor your
`~/.kube/config`.

The tier installs Cloud Hypervisor, the hypervisor the provider targets by
default, and the suites assert that instances name its RuntimeClass. Kata 4.x
builds the Cloud Hypervisor shim for x86_64 only, so an arm64 host — an Apple
silicon Mac, for example — runs the tier under QEMU instead:

```bash
task e2e TIER=kata E2E_KATA_SHIM=qemu
```

That variable picks the shim `kata-deploy` installs, the RuntimeClass name the
suites assert on, and the handler the provider is configured with, together. It
is the same override an arm64 cell makes in the provider's configuration.

CI runs this tier too, on the KVM that GitHub's standard Linux runners expose.
It is kept non-blocking anyway: that KVM is not a documented guarantee, and a
change in GitHub's fleet should not be able to block every merge.

Two things about that environment are worth knowing, because both are fatal and
neither reports itself clearly. Kata's arm64 defaults ask QEMU for a performance
monitoring unit the guest CPU does not have under nested virtualization, and a
container's 64 MB `/dev/shm` is too small to back a guest's RAM — the first
fails with a missing QEMU property, the second with what looks like a KVM fault.
Both are reapplied to the node on every `task e2e:up`, in
`hack/e2e/kata-workarounds.sh`, because both are lost whenever the thing that
owns them is rebuilt. The first applies to QEMU alone, so it is skipped where
the node runs Cloud Hypervisor.
