// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"go.miloapis.com/billing/emission"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha "go.datum.net/compute/api/v1alpha"
)

const (
	// usageFinalizer holds an instance Pod during deletion until its final
	// running usage window has been emitted.
	usageFinalizer = "kata.datumapis.com/usage-finalizer"

	// usageReportedUntilAnnotation records the RFC3339Nano UTC watermark
	// through which usage has already been emitted for this Pod. It is the
	// only metering state; the controller keeps nothing in memory.
	usageReportedUntilAnnotation = "kata.datumapis.com/usage-reported-until"

	// upstreamClusterNameLabel is stamped on the edge Namespace by federation
	// and encodes the owning project name as "cluster-<project>". The
	// Namespace's own name is a synthetic edge-local identifier and is never
	// used as the project.
	upstreamClusterNameLabel = "meta.datumapis.com/upstream-cluster-name"

	// MeterCPUMilliSeconds and MeterMemorySeconds are the canonical compute
	// instance usage meters declared in the compute ServiceConfiguration.
	// Units: millicore-seconds and byte-seconds.
	MeterCPUMilliSeconds = "compute.datumapis.com/instance/milli-cpu-seconds"
	MeterMemorySeconds   = "compute.datumapis.com/instance/memory-seconds"

	// UsageSource identifies the compute instance emitter in billing CloudEvents.
	UsageSource = "//compute.datumapis.com/instance"

	// runtimeClassDimension is the dimension every compute instance meter
	// declares, used by ServicePricing to rate each class separately.
	runtimeClassDimension = "runtime_class"

	// DefaultUsageFlushInterval is the width of the wall-clock aligned grid
	// that running usage is reported on.
	DefaultUsageFlushInterval = 5 * time.Minute

	// DefaultUsageMaxBackfill caps how far behind wall clock the first window
	// of a Pod the controller has never tracked may start. It keeps a rollout
	// of metering from billing the full history of long-running instances,
	// while still covering a controller outage around a Pod's start.
	DefaultUsageMaxBackfill = 15 * time.Minute

	// DefaultUsageTerminationGrace is how long past the Pod's
	// DeletionTimestamp the controller waits for the kubelet to report its
	// containers terminated before closing the window at the
	// DeletionTimestamp (e.g. when the node is lost).
	DefaultUsageTerminationGrace = 2 * time.Minute

	// DefaultUsageEmitGiveUp is how long past the Pod's DeletionTimestamp the
	// controller keeps retrying a failed emission before releasing the
	// finalizer and dropping the remaining usage. Without it, a billing
	// collector outage would block every instance deletion.
	DefaultUsageEmitGiveUp = 30 * time.Minute

	// maxWindowsPerReconcile bounds the catch-up work of a single reconcile
	// after a long controller outage. The remainder is picked up immediately
	// by a follow-up reconcile.
	maxWindowsPerReconcile = 48

	// requeueSlack makes a timed requeue land just after the grid boundary
	// it waits for rather than just before it.
	requeueSlack = 500 * time.Millisecond

	// UsageEventRecorderName is the reporting controller of usage Kubernetes Events.
	UsageEventRecorderName = "kata-provider-usage"
)

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// UsageReconciler meters compute resource usage (milli-CPU seconds and memory
// byte-seconds) across the lifecycle of Kata-isolated instance Pods.
//
// # Model
//
// One running stretch of an instance Pod's containers is one running stretch
// of its Kata guest. The controller bills [watermark, end] spans where the
// watermark is persisted on the Pod (usageReportedUntilAnnotation) and end is
// always a deterministic function of the Pod's state, never of when the
// reconcile happened to run:
//
//   - while running, spans end on a wall-clock aligned grid of FlushInterval
//     (e.g. :00, :05, :10 ...);
//   - when the containers stop, the final span ends at the container's
//     FinishedAt;
//   - when a deleted Pod's node never reports termination, the final span
//     ends at the Pod's DeletionTimestamp.
//
// Because every event's idempotency key is (pod UID, meter, start, end) and
// both ends are deterministic, a retry after a failed emission, a conflict on
// the watermark patch, a stale cache read or a leader change re-emits the
// exact same events, which the billing pipeline deduplicates. Usage is never
// counted twice, and the watermark is never advanced past usage that was not
// delivered (or deliberately dropped), so it is never lost either.
//
// The watermark is always persisted before the first emission, so even the
// start of the first span is deterministic.
type UsageReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	Recorder emission.Recorder

	// Events records Kubernetes Events on Pods for conditions an operator must
	// see (unattributable usage, dropped usage). Optional.
	Events events.EventRecorder

	// FlushInterval is the reporting grid width. Defaults to
	// DefaultUsageFlushInterval when zero or negative.
	FlushInterval time.Duration

	// MaxBackfill defaults to DefaultUsageMaxBackfill when zero or negative.
	MaxBackfill time.Duration

	// TerminationGrace defaults to DefaultUsageTerminationGrace when zero or negative.
	TerminationGrace time.Duration

	// EmitGiveUp defaults to DefaultUsageEmitGiveUp when zero or negative.
	EmitGiveUp time.Duration

	// Now returns the current wall-clock time. Defaults to time.Now when nil.
	Now func() time.Time
}

// usageWindow is one billable span of a Pod's running time.
type usageWindow struct {
	start, end time.Time
}

// podRunState is what the Pod's status says about its containers' running time.
type podRunState struct {
	// everStarted reports whether any application container has ever started.
	everStarted bool
	// firstStart is the earliest StartedAt across current and last-terminated
	// application container states.
	firstStart time.Time

	// running reports whether any application container is running now.
	running bool
	// runStart is the start of the current running stretch: the earliest
	// StartedAt among currently running containers.
	runStart time.Time

	// stopped reports that the Pod can never run again (terminal phase) or
	// that every application container is terminated.
	stopped bool
	// lastFinished is the latest FinishedAt across terminated container
	// states (current and last termination).
	lastFinished time.Time
}

func (r *UsageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pod core.Pod
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get pod: %w", err)
	}

	// The cache only holds managed Pods; this guards other clients.
	if pod.Labels[managedByLabel] != managedByValue {
		return ctrl.Result{}, nil
	}

	now := r.now()
	deleting := !pod.DeletionTimestamp.IsZero()
	terminal := pod.Status.Phase == core.PodSucceeded || pod.Status.Phase == core.PodFailed
	hasFinalizer := controllerutil.ContainsFinalizer(&pod, usageFinalizer)
	watermark, hasWatermark := parseReportedUntil(&pod)

	// A finalizer cannot be added to a Pod that is being deleted. A deleting
	// Pod without it was either never tracked or is already finalized.
	if deleting && !hasFinalizer {
		return ctrl.Result{}, nil
	}
	// The final span of a terminal Pod has been emitted.
	if terminal && hasWatermark && !hasFinalizer {
		return ctrl.Result{}, nil
	}

	state := observeRunState(&pod)

	// Nothing has ever run, so nothing is billable. Hold the Pod with the
	// finalizer from the start so a stretch that begins and ends between two
	// reconciles still has its final window emitted before the Pod is gone.
	if !state.everStarted {
		if deleting || terminal {
			return r.patchPod(ctx, &pod, func(p *core.Pod) { controllerutil.RemoveFinalizer(p, usageFinalizer) })
		}
		return r.patchPod(ctx, &pod, func(p *core.Pod) { controllerutil.AddFinalizer(p, usageFinalizer) })
	}

	if !hasWatermark {
		return r.startTracking(ctx, &pod, state, now, deleting, hasFinalizer)
	}

	plan := r.planUsage(&pod, state, watermark, now, deleting, terminal)

	if err := r.deliver(ctx, &pod, plan, watermark, now, deleting); err != nil {
		return ctrl.Result{}, err
	}
	// Every planned window was delivered or deliberately dropped.
	newWatermark := maxTime(lastEnd(plan.windows, watermark), plan.jumpTo, watermark)

	res, err := r.patchPod(ctx, &pod, func(p *core.Pod) {
		if !newWatermark.Equal(watermark) {
			setReportedUntil(p, newWatermark)
		}
		switch {
		case plan.finalize:
			controllerutil.RemoveFinalizer(p, usageFinalizer)
		case !deleting:
			controllerutil.AddFinalizer(p, usageFinalizer)
		}
	})
	if err != nil || res.RequeueAfter > 0 {
		return res, err
	}
	return plan.result(now), nil
}

// startTracking persists the start of tracking before anything is emitted, so
// the first window's start is as deterministic as every later one.
func (r *UsageReconciler) startTracking(ctx context.Context, pod *core.Pod, state podRunState, now time.Time, deleting, hasFinalizer bool) (ctrl.Result, error) {
	start := state.firstStart
	if !hasFinalizer {
		// The controller never saw this Pod before it ran, e.g. Pods that
		// were already running when metering was rolled out.
		if floor := now.Add(-r.maxBackfill()); start.Before(floor) {
			log.FromContext(ctx).Info("capping usage backfill for a pod first seen after it started",
				"pod", pod.Name, "namespace", pod.Namespace,
				"startedAt", start.Format(time.RFC3339Nano), "billedFrom", floor.Format(time.RFC3339Nano))
			usageBackfillCapped.Inc()
			start = floor
		}
	}
	res, err := r.patchPod(ctx, pod, func(p *core.Pod) {
		if !deleting {
			controllerutil.AddFinalizer(p, usageFinalizer)
		}
		setReportedUntil(p, start)
	})
	if err != nil || res.RequeueAfter > 0 {
		return res, err
	}
	return ctrl.Result{RequeueAfter: time.Millisecond}, nil
}

// deliver emits the planned windows in order. It returns an error only when a
// window must be retried, after persisting the progress made so far. Usage
// that cannot be attributed, or that could not be delivered by the give-up
// deadline of a deleted Pod, is dropped loudly and counts as delivered.
func (r *UsageReconciler) deliver(ctx context.Context, pod *core.Pod, plan usagePlan, watermark, now time.Time, deleting bool) error {
	if len(plan.windows) == 0 {
		return nil
	}

	project, attributable, err := r.resolveProject(ctx, pod.Namespace)
	if err != nil {
		return err
	}
	if !attributable {
		r.dropWindows(ctx, pod, plan.windows, "no_project", "UsageUnattributable",
			fmt.Sprintf("Namespace %q has no %s label; %s of usage were not billed.",
				pod.Namespace, upstreamClusterNameLabel, totalDuration(plan.windows)))
		return nil
	}

	delivered := watermark
	for i, w := range plan.windows {
		emitErr := r.emitWindow(ctx, pod, project, w)
		if emitErr == nil {
			delivered = w.end
			continue
		}
		usageEmitErrors.Inc()
		if reason, drop := r.unrecoverable(emitErr, w, now); drop {
			// Retrying can never deliver this window. Skip it loudly rather
			// than stall every later window of the Pod behind it.
			r.dropWindows(ctx, pod, []usageWindow{w}, reason, "UsageRejected",
				fmt.Sprintf("The billing pipeline rejected %s of usage (%s to %s), which was not billed: %v",
					w.end.Sub(w.start), w.start.UTC().Format(time.RFC3339), w.end.UTC().Format(time.RFC3339), emitErr))
			delivered = w.end
			continue
		}
		if !deleting || now.Before(pod.DeletionTimestamp.Add(r.emitGiveUp())) {
			// Keep what was delivered; the rest is retried with the same
			// deterministic windows.
			if delivered.After(watermark) {
				if _, err := r.patchPod(ctx, pod, func(p *core.Pod) { setReportedUntil(p, delivered) }); err != nil {
					log.FromContext(ctx).Error(err, "failed to persist partial usage progress", "pod", pod.Name)
				}
			}
			return emitErr
		}
		remaining := plan.windows[i:]
		r.dropWindows(ctx, pod, remaining, "emit_give_up", "UsageDropped",
			fmt.Sprintf("Billing collector unavailable for %s after deletion; %s of usage were not billed: %v",
				r.emitGiveUp(), totalDuration(remaining), emitErr))
		return nil
	}
	return nil
}

// emissionRejectedPrefix is how the billing SDK words a 4xx (other than 429)
// from the collector. The SDK exposes no typed error for it, so it is matched
// by message; TestUnrecoverable pins the wording against the SDK version.
const emissionRejectedPrefix = "emission: permanently rejected event"

// unrecoverable reports whether a failed window should be dropped instead of
// retried, and the metric reason to drop it under.
//
//   - A validation error is raised locally before any request and is
//     deterministic: no retry can succeed, so the window is dropped at once.
//   - A rejection by the collector is usually systemic (e.g. a collector
//     misconfiguration rejecting every event) and goes away once fixed, so it
//     is retried like any failure and nothing is lost. Only once the window is
//     older than EmitGiveUp is it treated as a poison window and dropped, so
//     it stalls the Pod's later usage for a bounded time.
func (r *UsageReconciler) unrecoverable(err error, w usageWindow, now time.Time) (string, bool) {
	var invalid *emission.ValidationError
	if errors.As(err, &invalid) {
		return "invalid", true
	}
	if strings.Contains(err.Error(), emissionRejectedPrefix) && !now.Before(w.end.Add(r.emitGiveUp())) {
		return "rejected", true
	}
	return "", false
}

// usagePlan is the deterministic outcome of one reconcile.
type usagePlan struct {
	// windows are emitted in order, each advancing the watermark to its end.
	windows []usageWindow
	// jumpTo is the watermark to persist when no window is emitted, after
	// skipping time in which no container ran.
	jumpTo time.Time
	// finalize releases the finalizer once windows are delivered.
	finalize bool
	// truncated reports more windows remain past maxWindowsPerReconcile.
	truncated bool
	// waitUntil is when the next window becomes due, if the Pod is running.
	waitUntil time.Time
}

func (p usagePlan) result(now time.Time) ctrl.Result {
	switch {
	case p.truncated:
		return ctrl.Result{RequeueAfter: time.Millisecond}
	case p.finalize, p.waitUntil.IsZero():
		// Done, or not running: the next container state change triggers a
		// reconcile.
		return ctrl.Result{}
	default:
		return ctrl.Result{RequeueAfter: max(p.waitUntil.Sub(now), 0) + requeueSlack}
	}
}

func (r *UsageReconciler) planUsage(pod *core.Pod, state podRunState, watermark, now time.Time, deleting, terminal bool) usagePlan {
	interval := r.flushInterval()
	plan := usagePlan{jumpTo: watermark}
	cursor := watermark
	budget := maxWindowsPerReconcile

	add := func(from, to time.Time, final bool) bool {
		ws, more := gridWindows(from, to, interval, final, budget)
		plan.windows = append(plan.windows, ws...)
		budget -= len(ws)
		if more {
			plan.truncated = true
		}
		return !more
	}

	running := state.running && !terminal

	// Containers restarted since the watermark. Bill the stretch that ended
	// (if the controller missed its termination) and skip the gap in which
	// nothing ran, e.g. CrashLoopBackOff.
	if running && state.runStart.After(cursor) {
		if end := minTime(state.lastFinished, state.runStart); end.After(cursor) {
			if !add(cursor, end, true) {
				return plan
			}
		}
		cursor = state.runStart
		plan.jumpTo = cursor
	}

	switch {
	case running:
		limit := now
		if deleting {
			// Never bill past the DeletionTimestamp on the grid, so the
			// deadline fallback below can always close the window there.
			limit = minTime(now, pod.DeletionTimestamp.UTC())
			deadline := pod.DeletionTimestamp.Add(r.terminationGrace())
			if !now.Before(deadline) {
				// The kubelet never reported the containers terminated
				// (e.g. node lost). Close at the DeletionTimestamp.
				usageForcedClose.Inc()
				if add(cursor, pod.DeletionTimestamp.UTC(), true) {
					plan.finalize = true
				}
				return plan
			}
			if !add(cursor, limit, false) {
				return plan
			}
			plan.waitUntil = minTime(nextBoundary(lastEnd(plan.windows, cursor), interval), deadline)
			return plan
		}
		if !add(cursor, limit, false) {
			return plan
		}
		plan.waitUntil = nextBoundary(lastEnd(plan.windows, cursor), interval)

	case state.stopped || terminal:
		end := stopTime(pod, state)
		if end.After(cursor) && !add(cursor, end, true) {
			return plan
		}
		plan.finalize = true

	default:
		// Not running and not every container terminated: waiting to (re)start.
		// Bill up to the most recent exit, then wait for the next start.
		if state.lastFinished.After(cursor) && !add(cursor, state.lastFinished, true) {
			return plan
		}
		if deleting {
			plan.finalize = true
		}
	}
	return plan
}

// gridWindows splits [from, to] at wall-clock multiples of interval. A non-
// final span stops at the last boundary not after to; a final span includes
// the trailing partial window ending at to. It returns at most budget windows
// and whether more remain.
func gridWindows(from, to time.Time, interval time.Duration, final bool, budget int) ([]usageWindow, bool) {
	var out []usageWindow
	cur := from
	for cur.Before(to) {
		if len(out) >= budget {
			return out, true
		}
		b := nextBoundary(cur, interval)
		if b.After(to) {
			if final {
				out = append(out, usageWindow{start: cur, end: to})
			}
			break
		}
		out = append(out, usageWindow{start: cur, end: b})
		cur = b
	}
	return out, false
}

// nextBoundary returns the first multiple of interval strictly after t.
func nextBoundary(t time.Time, interval time.Duration) time.Time {
	return t.UTC().Truncate(interval).Add(interval)
}

func (r *UsageReconciler) emitWindow(ctx context.Context, pod *core.Pod, project string, w usageWindow) error {
	durationSeconds := w.end.Sub(w.start).Seconds()
	if durationSeconds <= 0 {
		return nil
	}

	vcpuMilli, memoryBytes := podResources(pod)
	if vcpuMilli <= 0 && memoryBytes <= 0 {
		r.dropWindows(ctx, pod, []usageWindow{w}, "no_resources", "UsageUnsized",
			"Pod declares no CPU or memory limits; usage could not be sized and was not billed.")
		return nil
	}

	resourceRef := podResourceRef(pod)
	dimensions := map[string]string{runtimeClassDimension: RuntimeClassName}
	occurredAt := w.end.UTC()

	cpuQuantity := int64(math.Round(float64(vcpuMilli) * durationSeconds))
	memoryQuantity := int64(math.Round(float64(memoryBytes) * durationSeconds))

	for _, m := range []struct {
		meter    string
		quantity int64
	}{
		{MeterCPUMilliSeconds, cpuQuantity},
		{MeterMemorySeconds, memoryQuantity},
	} {
		if m.quantity <= 0 {
			continue
		}
		ev := emission.UsageEvent{
			IdempotencyKey: windowIdempotencyKey(pod.UID, m.meter, w.start, w.end),
			Meter:          m.meter,
			Project:        emission.ProjectRef{Name: project},
			Source:         UsageSource,
			Quantity:       m.quantity,
			Dimensions:     dimensions,
			Resource:       resourceRef,
			OccurredAt:     occurredAt,
		}
		if err := r.recorder().Record(ctx, ev); err != nil {
			return fmt.Errorf("recording %s for pod %s: %w", m.meter, pod.Name, err)
		}
	}
	usageWindowsEmitted.Inc()

	log.FromContext(ctx).V(1).Info("emitted compute usage window",
		"pod", pod.Name,
		"namespace", pod.Namespace,
		"project", project,
		"start", w.start.UTC().Format(time.RFC3339Nano),
		"end", occurredAt.Format(time.RFC3339Nano),
		"durationSeconds", durationSeconds,
		"vcpuMilli", vcpuMilli,
		"memoryBytes", memoryBytes,
		"cpuMilliSeconds", cpuQuantity,
		"memoryByteSeconds", memoryQuantity,
	)
	return nil
}

// dropWindows makes usage that will deliberately not be billed loud: an error
// log, a Warning Event on the Pod and a counter operators can alert on.
func (r *UsageReconciler) dropWindows(ctx context.Context, pod *core.Pod, windows []usageWindow, reason, eventReason, message string) {
	if len(windows) == 0 {
		return
	}
	usageWindowsDropped.WithLabelValues(reason).Add(float64(len(windows)))
	log.FromContext(ctx).Error(nil, "dropping unbillable usage",
		"reason", reason,
		"pod", pod.Name,
		"namespace", pod.Namespace,
		"windows", len(windows),
		"seconds", totalDuration(windows).Seconds(),
		"from", windows[0].start.UTC().Format(time.RFC3339Nano),
		"to", windows[len(windows)-1].end.UTC().Format(time.RFC3339Nano),
	)
	if r.Events != nil {
		r.Events.Eventf(pod, nil, core.EventTypeWarning, eventReason, "MeterUsage", "%s", message)
	}
}

func (r *UsageReconciler) resolveProject(ctx context.Context, namespace string) (string, bool, error) {
	var ns core.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: namespace}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("failed to get namespace %q: %w", namespace, err)
	}

	project := decodeProjectName(ns.Labels[upstreamClusterNameLabel])
	if project == "" {
		return "", false, nil
	}
	return project, true, nil
}

// decodeProjectName reverses go.datum.net/compute's EncodeClusterName
// ("cluster-" + strings.ReplaceAll(name, "/", "_")) and returns the plain
// Milo project name. That name becomes the CloudEvent subject
// "projects/<name>" (built by the billing SDK, which rejects names containing
// "/"), and the billing consumer attributes the event by matching it against
// BillingAccountBinding.spec.projectRef.name.
//
// Like compute's own federator (projectClusterNameFromLabel), it keeps only
// the final path segment, so a path-shaped cluster name such as
// "/<project>" or "<org>/<project>" still resolves to the bare project name.
func decodeProjectName(encoded string) string {
	trimmed := strings.TrimSpace(encoded)
	if !strings.HasPrefix(trimmed, "cluster-") {
		return ""
	}
	decoded := strings.ReplaceAll(strings.TrimPrefix(trimmed, "cluster-"), "_", "/")
	if i := strings.LastIndex(decoded, "/"); i >= 0 {
		decoded = decoded[i+1:]
	}
	return decoded
}

// podResources returns the billed vCPU millicores and memory bytes: the
// instance size the Kata guest is sized from. That size is the application
// containers' limits; requests are overcommitted for scheduling and are only
// a fallback when a limit is absent. Init containers are ignored. Every
// application container carries the full instance size, so the largest is the
// instance size.
func podResources(pod *core.Pod) (vcpuMilli, memoryBytes int64) {
	for _, c := range pod.Spec.Containers {
		cpu, ok := c.Resources.Limits[core.ResourceCPU]
		if !ok || cpu.IsZero() {
			cpu = c.Resources.Requests[core.ResourceCPU]
		}
		vcpuMilli = max(vcpuMilli, milliQuantity(cpu))

		mem, ok := c.Resources.Limits[core.ResourceMemory]
		if !ok || mem.IsZero() {
			mem = c.Resources.Requests[core.ResourceMemory]
		}
		memoryBytes = max(memoryBytes, mem.Value())
	}
	return vcpuMilli, memoryBytes
}

func milliQuantity(q resource.Quantity) int64 {
	if q.IsZero() {
		return 0
	}
	return q.MilliValue()
}

func podResourceRef(pod *core.Pod) *emission.ResourceRef {
	name := pod.Labels[instanceLabel]
	if name == "" {
		name = pod.Name
	}
	uid := pod.UID
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "Instance" && owner.UID != "" {
			uid = owner.UID
			if owner.Name != "" {
				name = owner.Name
			}
			break
		}
	}
	return &emission.ResourceRef{
		Group:     computev1alpha.GroupVersion.Group,
		Kind:      "Instance",
		Namespace: pod.Namespace,
		Name:      name,
		UID:       uid,
	}
}

func windowIdempotencyKey(podUID types.UID, meter string, start, end time.Time) string {
	return fmt.Sprintf("%s:%s:%s:%s",
		podUID,
		meter,
		start.UTC().Format(time.RFC3339Nano),
		end.UTC().Format(time.RFC3339Nano),
	)
}

func parseReportedUntil(pod *core.Pod) (time.Time, bool) {
	raw := pod.Annotations[usageReportedUntilAnnotation]
	if raw == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func setReportedUntil(pod *core.Pod, t time.Time) {
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[usageReportedUntilAnnotation] = t.UTC().Format(time.RFC3339Nano)
}

// observeRunState reads the application containers' running time from the
// Pod status. Only container states count: a Pod's StartTime is when the
// kubelet accepted it, before image pulls, so it is not running time.
func observeRunState(pod *core.Pod) podRunState {
	var s podRunState
	statuses := pod.Status.ContainerStatuses
	allTerminated := len(statuses) > 0

	consider := func(started time.Time) {
		if started.IsZero() {
			return
		}
		s.everStarted = true
		if s.firstStart.IsZero() || started.Before(s.firstStart) {
			s.firstStart = started
		}
	}

	for _, cs := range statuses {
		switch {
		case cs.State.Running != nil:
			allTerminated = false
			started := cs.State.Running.StartedAt.UTC()
			consider(started)
			if !started.IsZero() {
				s.running = true
				if s.runStart.IsZero() || started.Before(s.runStart) {
					s.runStart = started
				}
			}
		case cs.State.Terminated != nil:
			consider(cs.State.Terminated.StartedAt.UTC())
			if f := cs.State.Terminated.FinishedAt.UTC(); f.After(s.lastFinished) {
				s.lastFinished = f
			}
		default:
			allTerminated = false
		}
		if t := cs.LastTerminationState.Terminated; t != nil {
			consider(t.StartedAt.UTC())
			if f := t.FinishedAt.UTC(); f.After(s.lastFinished) {
				s.lastFinished = f
			}
		}
	}

	s.stopped = allTerminated || pod.Status.Phase == core.PodSucceeded || pod.Status.Phase == core.PodFailed
	if s.stopped {
		s.running = false
	}
	return s
}

// stopTime is the end of the final span of a Pod that can no longer run. It
// prefers the containers' FinishedAt. A terminal Pod whose container
// statuses carry no termination (e.g. failed by the node lifecycle
// controller) falls back to when it stopped being Ready, then to its
// DeletionTimestamp. Each is a deterministic function of the Pod.
func stopTime(pod *core.Pod, s podRunState) time.Time {
	if !s.lastFinished.IsZero() && !s.running {
		return s.lastFinished
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == core.PodReady && c.Status != core.ConditionTrue && !c.LastTransitionTime.IsZero() {
			return c.LastTransitionTime.UTC()
		}
	}
	if !pod.DeletionTimestamp.IsZero() {
		return pod.DeletionTimestamp.UTC()
	}
	return s.lastFinished
}

// patchPod applies mutate and patches the Pod when it changed. The optimistic
// lock keeps a stale cache read from moving the watermark backwards; a
// conflict is retried, which is safe because the windows are deterministic.
func (r *UsageReconciler) patchPod(ctx context.Context, pod *core.Pod, mutate func(*core.Pod)) (ctrl.Result, error) {
	base := pod.DeepCopy()
	mutate(pod)
	if equalUsageMeta(base, pod) {
		return ctrl.Result{}, nil
	}
	if err := r.Patch(ctx, pod, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: time.Millisecond}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to update usage metering state on pod %s: %w", pod.Name, err)
	}
	return ctrl.Result{}, nil
}

func equalUsageMeta(a, b *core.Pod) bool {
	return a.Annotations[usageReportedUntilAnnotation] == b.Annotations[usageReportedUntilAnnotation] &&
		controllerutil.ContainsFinalizer(a, usageFinalizer) == controllerutil.ContainsFinalizer(b, usageFinalizer)
}

func totalDuration(windows []usageWindow) time.Duration {
	var d time.Duration
	for _, w := range windows {
		d += w.end.Sub(w.start)
	}
	return d
}

func maxTime(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}

func lastEnd(windows []usageWindow, fallback time.Time) time.Time {
	if len(windows) == 0 {
		return fallback
	}
	return windows[len(windows)-1].end
}

func minTime(a, b time.Time) time.Time {
	if a.IsZero() {
		return b
	}
	if b.IsZero() || a.Before(b) {
		return a
	}
	return b
}

func positiveOr(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}

func (r *UsageReconciler) flushInterval() time.Duration {
	return positiveOr(r.FlushInterval, DefaultUsageFlushInterval)
}

func (r *UsageReconciler) maxBackfill() time.Duration {
	return positiveOr(r.MaxBackfill, DefaultUsageMaxBackfill)
}

func (r *UsageReconciler) terminationGrace() time.Duration {
	return positiveOr(r.TerminationGrace, DefaultUsageTerminationGrace)
}

func (r *UsageReconciler) emitGiveUp() time.Duration {
	return positiveOr(r.EmitGiveUp, DefaultUsageEmitGiveUp)
}

func (r *UsageReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *UsageReconciler) recorder() emission.Recorder {
	if r.Recorder != nil {
		return r.Recorder
	}
	return emission.NoopRecorder{}
}

// SetupWithManager registers the UsageReconciler with the Manager.
func (r *UsageReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Client == nil {
		r.Client = mgr.GetClient()
	}
	if r.Scheme == nil {
		r.Scheme = mgr.GetScheme()
	}
	if r.Events == nil {
		r.Events = mgr.GetEventRecorder(UsageEventRecorderName)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&core.Pod{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 5}).
		Named("usage").
		Complete(r)
}
