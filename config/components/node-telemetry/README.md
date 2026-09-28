# Kata node telemetry

This Kustomize component adds project instance logs to the shared compute node
collector. It reads application standard output and standard error, including
retained startup and crash output. Customers do not need to change their images.

## Composition contract

Infra composes this component with the telemetry repository's
`config/node-collector` base and the selected runtime components. This
component patches `compute-node-collector`; it creates no DaemonSet, namespace,
RBAC, receiver, or exporter. The platform owns scheduling, storage, resource
limits, destinations, and credentials.

Contract version 1 requires Collector Contrib `0.144.0`,
`filelog/kubernetes`, `memory_limiter`, and `otlp_grpc/project`. Kata adds
`k8sattributes/kata`, `filter/kata`, `resource/kata`, and `logs/kata`.
The platform and Kata pipelines share one CRI file reader. See
[`manifest.json`](manifest.json) for the machine-readable contract.

The Kubernetes Pod UID associates each entry with platform-owned Pod and
namespace labels. The pipeline preserves timestamps, stream, and container
name, adds the `datum.*` instance attributes, and sets `project_name` for
gateway routing. Workload identity is optional for standalone instances.
Application bodies cannot set routing attributes. Records without project,
upstream namespace, or instance identity are discarded.

## Enable an environment

1. In infra, compose the pinned platform base and this component into one
   compute collector. Match its scheduling to every node that can run Kata.
   Prepare namespace project and upstream namespace labels.
2. Deploy and verify the shared collector. Its generic platform pipeline must
   exclude `telemetry.miloapis.com/otlp-native-logs=true`. Retire any previous
   overlapping node collectors through the environment's migration plan.
3. Set `downstreamResourceManagement.instanceLogs: true` and roll out the
   provider. It marks instance Pods for the Kata pipeline. The setting remains
   false by default; publishing this component does not switch production.

For rollback, disable the setting before removing the component. Keep the
generic platform pipeline available and drain queued logs. Switching pipelines
is not atomic; check for gaps and duplicates. Preserve shared collector storage
across updates.

## Delivery and validation

The platform base controls file polling, rotation, record limits, buffering,
and retries. Kubernetes metadata is synchronized at startup, but later watch
lag can still discard entries with unresolved identity. Deleted-Pod metadata
does not survive collector restarts. Node loss can lose local files and queued
logs; interrupted acknowledgements can cause duplicates. Retention and query
latency remain environment policies.

Run `OTELCOL_BIN=/path/to/otelcol-contrib make test-telemetry` to compose the
component against its local fixture and test real collector processing. Set
`TELEMETRY_BASE_DIR` to an absolute platform base path to test that composition
instead. The tests cover projects with identical instance names, body spoofing,
startup/crash output, containers, rotation, restarts, and an export outage.

Before customer rollout, verify instance logs through the project API and UI,
including denial of cross-project reads. Measure delay and check collector
errors, queue usage, storage, and duplicate events. Local tests do not validate
an environment's query authorization or retention settings.
