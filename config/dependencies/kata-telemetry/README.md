# Instance logs

This optional collector sends Kata application output to Datum's existing
project log service. It reads standard output and standard error from every
application container, including retained startup and crash output. Customers
do not need to install an agent or change their images. Files inside containers
and host diagnostics are outside this integration.

## Enable collection

The cell must have the OpenTelemetry Operator, the shared telemetry gateway,
and namespaces labeled with `resourcemanager.miloapis.com/project-name` and
`meta.datumapis.com/upstream-namespace`.

1. Create an environment overlay for this directory. Match the collector's node
   selector and tolerations to the provider's actual compute pool. The default
   selector uses `katacontainers.io/kata-runtime=true`. Datum's current Talos
   pool uses `compute.datumapis.com/runtime=unikraft` for both runtimes, so its
   overlay must replace the default selector.
2. Deploy the overlay and wait for the collector on every eligible node. The
   default destination is
   `gateway-collector-collector.o11y-system.svc.cluster.local:4317`. Configure
   TLS and authentication before using another trust boundary.
3. Set `downstreamResourceManagement.instanceLogs: true` in the provider's
   configuration and roll out the controller. It labels managed Pods with
   `telemetry.miloapis.com/otlp-native-logs=true`. The shared node collector in
   telemetry bundle `v0.4.13` excludes that label; the Kata collector requires
   it. Verify the deployed shared collector has this exclusion before enabling
   collection. An older shared collector needs the same exclusion first.

The collector has a separate namespace because its host log and storage mounts
require a Pod Security exemption. Its account can only read Pods and
namespaces. Keep tenant workloads out of this namespace. The application Pods
receive neither host access nor telemetry credentials.

For rollback, set `instanceLogs: false`, roll out the provider, and verify the
shared collector resumes collection before removing this collector. Allow its
export queue to drain. The switch between collectors is not atomic: check for
gaps or duplicates during either transition. Keep the node storage directory
when updating or restarting the collector.

## Identity and project access

The collector uses each log file's Pod UID to look up platform metadata. It
exports only Pods managed by Kata with collection enabled. It preserves the
CRI timestamp, stream, and container name, and adds:

| Attribute | Platform source | Purpose |
| --- | --- | --- |
| `datum.project.name`, `project_name` | Namespace project label | Compute queries and gateway project routing |
| `datum.instance.namespace` | Namespace upstream namespace label | Instance namespace |
| `datum.instance.name` | Pod `upstream.instance` label | Instance identity |
| `datum.workload.name` | Pod workload label, when present | Workload identity |

Application output stays text: a JSON body cannot change its project routing.
Records without a project, upstream namespace, or instance are discarded. The
gateway must receive `project_name`; `datum.project.name` alone does not select
the customer tenant in telemetry bundle `v0.4.13`. Existing query authentication
and project filtering provide read access control; this collector adds no
customer-facing endpoint.

## Delivery limits

- Collection polls local CRI files every second. The reference gateway batches
  for up to 10 seconds before transport and indexing add further delay. The
  provider does not guarantee query latency.
- Persistent file offsets prevent routine rereads. The export queue holds up
  to 256 MiB of serialized data per node, plus storage overhead, and retries
  transient failures without a time limit. When storage or the queue fills,
  delivery depends on recovery before kubelet removes the retained files.
- Collection covers current files and uncompressed rotations. Compressed files
  are excluded. Records and reassembled CRI messages are limited to 1 MiB;
  larger messages can be split. This is a collector bound, not a promise about
  the gateway's accepted message size.
- The collector waits for Kubernetes metadata when starting. A new Pod can
  still write before its metadata reaches the watch cache; those records are
  discarded if identity is unavailable. Deleted-Pod metadata is temporary and
  does not survive collector restarts. Retained files alone cannot recover
  identity after deletion. Node loss also loses local files and queued data.
- Delivery is best effort, with retries. An interrupted acknowledgement can
  cause duplicates. The pipeline does not promise exactly-once delivery.
- The reference telemetry bundle `v0.4.13` retains customer logs for seven
  days. Retention is owned by the telemetry environment, not this provider;
  confirm the deployed ClickHouse policy before publishing customer limits.
  Also check gateway limits, kubelet rotation, and queue disk capacity.

## Verify a rollout

Run the collector integration test with the pinned executable:

```bash
OTELCOL_BIN=/path/to/otelcol-contrib make test-telemetry
```

The test uses real collector processing with local CRI files and simulated
Kubernetes and OTLP services. It covers startup and terminated containers,
multiple containers and projects, body spoofing, incomplete identity,
rotation, container and collector restarts, and an export outage. CI runs it
with OpenTelemetry Collector Contrib `0.144.0`.

Before enabling customers in an environment, create two project instances with
the same name and distinctive output. Verify logs through the project log API
and instance view, including that each project cannot read the other's logs.
Check delivery after a crash and temporary gateway outage, measure ingestion
delay, and inspect collector errors, refused records, queue usage, disk space,
and duplicate events. These environment checks are separate from the local
integration test.
