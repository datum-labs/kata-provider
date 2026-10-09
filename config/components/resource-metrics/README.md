# Kata instance CPU and memory metrics

This component records per-container Kata usage into the existing Datum
`datum_compute_instance_*` metric names. The cell overlay installs the `VMRule`
in the provider namespace. It is useful only where infra scrapes kubelet
`/metrics/resource` and an edge `VMAlert` selects rules from that namespace.

## Source and identity

The source is kubelet `/metrics/resource`, scraped with `job="kubelet-resource"`:

- `container_cpu_usage_seconds_total` is a cumulative CPU-seconds counter.
- `container_memory_working_set_bytes` is a working-set gauge in bytes.

The rule rejects empty and `POD` container names. It joins `kube_pod_info`
for node identity, `kube_pod_runtimeclass_name_info` to limit the series to
Kata handlers, and `kube_pod_labels` for `upstream.instance` (the Datum
`Instance.metadata.name`). It derives the project from the mapped namespace's
`meta.datumapis.com/upstream-cluster-name=cluster-<project>` label, following
the existing Unikraft recording-rule contract. The output has
`resource_name`, `resourcemanager_datumapis_com_project_name`, `container`,
`node`, `region`, and `runtime_class="general-purpose"`. The customer metrics
federation maps `container` to `instance_container` and drops the raw label.

Kube-state-metrics must allowlist the Pod `upstream.instance` label and the
mapped namespace labels. The compute Prometheus Adapter uses the edge-local
series with `namespace`, `pod`, `container`, and `node` for `metrics.k8s.io`.

## Infra dependencies

1. Add a `VMNodeScrape` for kubelet `/metrics/resource` on Kata-capable nodes.
   Set `job="kubelet-resource"` and avoid a second scrape of the same source.
2. Extend the edge `VMAlert` namespace selector to include the Kata provider
   namespace. Its current selector admits only the Unikraft provider namespace.
3. Keep the existing edge and hub `VMAlert` rule label selector
   `telemetry.miloapis.com/edge-hpa-resource-metrics=true` and the project
   federation path. Infra PR [#6819](https://github.com/datum-cloud/infra/pull/6819)
   preserves `instance_container` in the customer metrics store.

The rule is deliberately included in the cell overlay, but it publishes no
series until the scrape and VMAlert selector are installed. The provider's
`node-telemetry` component handles logs and does not collect these metrics.

## Validation and limits

The `us-central-1-staging-lab` prototype verified that the `katars` handler
exposes a named app container through `/metrics/resource`, while
`/metrics/cadvisor` only exposed Pod and sandbox rows for the sampled Pod.
The recording queries produced project-scoped CPU and memory series at the
edge, the Prometheus Adapter returned `PodMetrics`, and the customer metrics
store retained `resource_name`, project, region, runtime, and
`instance_container`. The rule expressions in this component are copied from
that prototype.

Before production rollout, run a controlled two-container Kata instance to
verify that CPU and memory change independently, that restarts do not create
duplicate active series, and how guest VM/kernel overhead is charged. The
current validation covers a single quiet app container and does not establish
those semantics. Record observed scrape-to-query delay and verify that a
missing collector is shown as a collection gap rather than zero usage.

Sample customer selector:

```promql
datum_compute_instance_memory_working_set_bytes{
  resourcemanager_datumapis_com_project_name="<project>",
  resource_name="<instance>",
  instance_container="<container>"
}
```

## Autoscaling freshness

The CPU and memory recording rules also emit
`datum_compute_instance_{cpu,memory}_source_timestamp_seconds`, with the same
container identity and the Kubernetes Pod UID. Their values contain the source
sample timestamp, rather than the recording-rule evaluation time. The source
scrape uses `honorTimestamps: false`, so this is the kubelet collection time,
not a guest-provided clock. Invalid or non-finite usage samples are excluded.

The compute adapter can use these gauges to omit an entire Kata Pod if an
expected container lacks CPU or memory data, either source is older than 90
seconds, or the sample predates the current Pod. Omitting incomplete data
prevents the adapter from filling a missing resource with zero. This contract
requires current kube-state-metrics Pod UID, creation, and container identity
series. The adapter query still reports query time in `PodMetrics.Timestamp`;
these gauges enforce a bounded collection age and do not repair that API field.

Run the exact shipped recording rules with `promtool` on PATH:

```sh
go test -count=1 ./test/resource-metrics
```
