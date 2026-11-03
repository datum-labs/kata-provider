# Runbook: usage metering

The usage controller in the Kata provider bills each instance for the time its
containers run. It sends two billing CloudEvents per window (milli-CPU seconds
and memory byte-seconds) to the node-local billing collector (Vector). Vector
forwards them to the billing gateway. For the model, see
[Usage metering](../../README.md#usage-metering) in the README.

The examples assume:

```sh
PROVIDER_NS=kata-provider-system   # namespace of the kata-provider Deployment
```

## Where the state lives

All metering state is on the instance Pod. The controller keeps nothing in
memory, so restarting it is always safe.

| What | Where | Meaning |
|---|---|---|
| Watermark | annotation `kata.datumapis.com/usage-reported-until` | Usage has been billed up to this instant (RFC 3339, UTC). |
| Finalizer | `kata.datumapis.com/usage-finalizer` | Holds a deleted Pod until its last window has been sent, or deliberately dropped. |

```sh
kubectl get pods -A -l managed-by=kata-provider -o custom-columns=\
NS:.metadata.namespace,POD:.metadata.name,PHASE:.status.phase,\
BILLED_UNTIL:'.metadata.annotations.kata\.datumapis\.com/usage-reported-until',\
DELETING:.metadata.deletionTimestamp,FINALIZERS:.metadata.finalizers
```

On a running Pod, `BILLED_UNTIL` should never be more than one flush interval
(`usageMetering.flushInterval`, 5m by default) behind the current time.

## Signals

### Metrics

The metrics are served on the manager's metrics endpoint.

| Metric | Healthy | Investigate when |
|---|---|---|
| `kata_provider_usage_windows_emitted_total` | Rises with the number of running instances | It flatlines while instances are running |
| `kata_provider_usage_emit_errors_total` | Flat | It rises steadily. See [Collector unavailable](#collector-unavailable-or-rejecting-events) |
| `kata_provider_usage_windows_dropped_total{reason}` | Flat | It rises at all. This is unbilled usage, broken down by `reason` below |
| `kata_provider_usage_forced_close_total` | Rare | It rises. Nodes are being lost, or the kubelet is not reporting terminations |
| `kata_provider_usage_backfill_capped_total` | Rises only on first rollout | It rises later. The controller saw Pods long after they started (it was down) |

Values of the `reason` label on `windows_dropped_total`:

| `reason` | Cause | Pod Event |
|---|---|---|
| `no_project` | The Namespace has no `meta.datumapis.com/upstream-cluster-name` label | `UsageUnattributable` |
| `no_resources` | The Pod declares no CPU or memory limits or requests | `UsageUnsized` |
| `invalid` | The billing SDK rejected the event locally | `UsageRejected` |
| `rejected` | The collector answered 4xx for longer than `emitGiveUp` | `UsageRejected` |
| `emit_give_up` | A deleted Pod could not be delivered for `emitGiveUp` after its deletion | `UsageDropped` |

Suggested alerts:

- `increase(kata_provider_usage_windows_dropped_total[15m]) > 0`. Usage was lost.
- `rate(kata_provider_usage_emit_errors_total[10m]) > 0` for 15m. Delivery is
  failing, and usage is queueing on the Pods.

### Events and logs

```sh
# Unbilled usage, per Pod
kubectl get events -A --field-selector reason=UsageUnattributable
kubectl get events -A --field-selector reason=UsageDropped
kubectl get events -A --field-selector reason=UsageRejected
kubectl get events -A --field-selector reason=UsageUnsized

# Controller view
kubectl -n "$PROVIDER_NS" logs deploy/kata-provider | grep -E \
  'dropping unbillable usage|emitted compute usage window|recording compute|capping usage backfill'
```

## Scenarios

### A Pod is stuck in Terminating

The usage finalizer holds a deleted Pod in three situations:

- while its containers are still shutting down;
- until `DeletionTimestamp` + `usageMetering.terminationGrace` (2m), when the
  kubelet never reports the containers terminated. The controller then closes
  the window at the `DeletionTimestamp`;
- until `DeletionTimestamp` + `usageMetering.emitGiveUp` (30m), while delivery
  keeps failing. Retries back off, so the Pod can be released up to about 15
  minutes after that deadline.

So a Pod held only by this finalizer should be gone within about 45 minutes of
its deletion. If it is held longer:

1. Confirm the finalizer is the one holding the Pod:
   ```sh
   kubectl -n <ns> get pod <pod> -o jsonpath='{.metadata.finalizers}{"\n"}{.metadata.deletionTimestamp}{"\n"}'
   ```
2. Check that the controller is running and is the leader:
   ```sh
   kubectl -n "$PROVIDER_NS" get pods
   kubectl -n "$PROVIDER_NS" logs deploy/kata-provider --tail=200 | grep -i '<pod>'
   ```
3. Look for errors that the give-up deadline does not cover. Both are
   misconfigurations:
   - `failed to get namespace`: the controller cannot read Namespaces. Check
     the `namespaces` get/list/watch rule in the provider ClusterRole.
   - `failed to update usage metering state on pod`: the controller cannot
     patch Pods. Check the RBAC and any admission webhook that rejects the
     patch.
4. Fix the cause, and the controller releases the Pod on its next reconcile. If
   the Pod has to go *now*, remove the finalizer by hand. See
   [Removing the usage finalizer by hand](#removing-the-usage-finalizer-by-hand).

### Collector unavailable or rejecting events

Symptoms: `usage_emit_errors_total` is rising, watermarks on running Pods are
falling behind, and the logs show `recording compute.datumapis.com/...` errors.

- **Unreachable or 5xx.** Nothing is lost. The watermarks hold, and the
  controller catches up once the collector is back (up to 48 windows per Pod
  per reconcile). Deleted Pods wait up to `emitGiveUp`, then their remaining
  usage is dropped (`emit_give_up`).
- **4xx (`emission: permanently rejected event: HTTP <code>`).** This usually
  means the collector is misconfigured. The window is retried until it is older
  than `emitGiveUp`, then dropped (`rejected`). Fix the collector within that
  window and nothing is lost.

Check the collector on the Pod's node:

```sh
kubectl -n billing-system get pods -l app.kubernetes.io/instance=billing-usage-collector -o wide
kubectl -n billing-system logs <vector-pod-on-that-node>
```

The Service uses `internalTrafficPolicy: Local`, so an instance's node must
run a collector Pod. Also check that the provider Pod carries the label
`billing.miloapis.com/emits-usage: "true"`, which the collector's
NetworkPolicy requires.

### Usage is not attributed to a project

Symptoms: `UsageUnattributable` Events and `dropped_total{reason="no_project"}`.
The instance's Namespace lacks the label that federation stamps.

```sh
kubectl get namespace <ns> -o jsonpath='{.metadata.labels.meta\.datumapis\.com/upstream-cluster-name}{"\n"}'
```

This is a federation or compute misconfiguration. The controller drops this
usage rather than guess at a project, and does not retry it once the label is
added. Report the Namespace to the compute owners.

### The controller was down or restarted

No action is needed. The new leader reconciles every Pod from its watermark and
sends the windows that were missed, with the same event ids, so the gateway
deduplicates anything that was already delivered. Expect a burst of
late-arriving events and the watermarks catching up.

Pods deleted during the outage stay in Terminating until the controller is
back. A crash-looping container that restarted several times during the outage
may have the back-off gaps between the earlier restarts billed, because the
kubelet keeps only the last termination.

## Removing the usage finalizer by hand

Removing the finalizer while the controller is healthy loses the usage since
the watermark. Do it only when a Pod must go immediately, or the controller
will not come back.

Remove only `kata.datumapis.com/usage-finalizer`, and keep any other
finalizers. For one Pod:

```sh
NS=<ns>; POD=<pod>
kubectl -n "$NS" get pod "$POD" -o json |
  jq -c '{metadata: {finalizers: [.metadata.finalizers[]? | select(. != "kata.datumapis.com/usage-finalizer")]}}' |
  kubectl -n "$NS" patch pod "$POD" --type=merge --patch-file /dev/stdin
```

Record the Pod's `usage-reported-until` annotation first. It is the point from
which usage was lost.

## Decommissioning the provider from a cell

Only the controller removes the usage finalizer. If the provider is removed
while instance Pods exist, those Pods, and the Namespaces holding them, hang in
Terminating. The Instance finalizer `kata.datumapis.com/finalizer` behaves the
same way.

1. Drain the instances while the provider is still running: delete or move
   them, and wait for their Pods to disappear. The final usage is delivered
   normally.
2. Only if Pods remain after the provider is gone, strip the usage finalizer
   from every provider Pod:
   ```sh
   kubectl get pods -A -l managed-by=kata-provider -o json |
     jq -r '.items[] | select(.metadata.finalizers // [] | index("kata.datumapis.com/usage-finalizer")) |
            "\(.metadata.namespace) \(.metadata.name)"' |
     while read -r ns pod; do
       kubectl -n "$ns" get pod "$pod" -o json |
         jq -c '{metadata: {finalizers: [.metadata.finalizers[]? | select(. != "kata.datumapis.com/usage-finalizer")]}}' |
         kubectl -n "$ns" patch pod "$pod" --type=merge --patch-file /dev/stdin
     done
   ```
   The usage those Pods accrued after their last watermark is not billed.
