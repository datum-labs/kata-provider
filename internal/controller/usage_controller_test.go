// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"go.miloapis.com/billing/emission"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	computev1alpha "go.datum.net/compute/api/v1alpha"
)

const (
	testProjectName = "test-project"
	testPodUID      = types.UID("pod-uid-1234")
	testInstanceUID = types.UID("instance-uid-5678")

	testCPUMilli    = int64(2000)
	testMemoryBytes = int64(4 << 30)
)

// base is a wall-clock grid boundary for the 5m test interval.
var base = time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)

func at(minutes, seconds int) time.Time {
	return base.Add(time.Duration(minutes)*time.Minute + time.Duration(seconds)*time.Second)
}

type recordingFake struct {
	events []emission.UsageEvent
	failOn func(ev emission.UsageEvent, call int) error
	calls  int
}

func (f *recordingFake) Record(_ context.Context, ev emission.UsageEvent) error {
	f.calls++
	if f.failOn != nil {
		if err := f.failOn(ev, f.calls); err != nil {
			return err
		}
	}
	f.events = append(f.events, ev)
	return nil
}

func (f *recordingFake) keys() []string {
	out := make([]string, 0, len(f.events))
	for _, ev := range f.events {
		out = append(out, ev.IdempotencyKey)
	}
	return out
}

// billed sums the billed seconds of the CPU events.
func (f *recordingFake) billedSeconds() float64 {
	var total int64
	for _, ev := range f.events {
		if ev.Meter == MeterCPUMilliSeconds {
			total += ev.Quantity
		}
	}
	return float64(total) / float64(testCPUMilli)
}

func newTestNamespace(project string) *core.Namespace {
	ns := &core.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testInstanceNamespace}}
	if project != "" {
		ns.Labels = map[string]string{upstreamClusterNameLabel: "cluster-" + project}
	}
	return ns
}

func running(startedAt time.Time) core.ContainerState {
	return core.ContainerState{Running: &core.ContainerStateRunning{StartedAt: metav1.NewTime(startedAt)}}
}

func terminated(startedAt, finishedAt time.Time) core.ContainerState {
	return core.ContainerState{Terminated: &core.ContainerStateTerminated{
		StartedAt:  metav1.NewTime(startedAt),
		FinishedAt: metav1.NewTime(finishedAt),
	}}
}

func waiting() core.ContainerState {
	return core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
}

func withState(state core.ContainerState) func(*core.Pod) {
	return func(p *core.Pod) { p.Status.ContainerStatuses[0].State = state }
}

func withLastTermination(state core.ContainerState) func(*core.Pod) {
	return func(p *core.Pod) { p.Status.ContainerStatuses[0].LastTerminationState = state }
}

func withPhase(phase core.PodPhase) func(*core.Pod) {
	return func(p *core.Pod) { p.Status.Phase = phase }
}

func withWatermark(t time.Time) func(*core.Pod) {
	return func(p *core.Pod) {
		setReportedUntil(p, t)
		controllerutil.AddFinalizer(p, usageFinalizer)
	}
}

func withDeletion(t time.Time) func(*core.Pod) {
	return func(p *core.Pod) {
		dt := metav1.NewTime(t)
		p.DeletionTimestamp = &dt
	}
}

func newManagedPod(mutators ...func(*core.Pod)) *core.Pod {
	pod := &core.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testInstanceName,
			Namespace: testInstanceNamespace,
			UID:       testPodUID,
			Labels: map[string]string{
				managedByLabel: managedByValue,
				instanceLabel:  testInstanceName,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: computev1alpha.GroupVersion.String(),
				Kind:       "Instance",
				Name:       testInstanceName,
				UID:        testInstanceUID,
			}},
		},
		Spec: core.PodSpec{
			InitContainers: []core.Container{{
				Name: "datum-shell-install",
				Resources: core.ResourceRequirements{Limits: core.ResourceList{
					core.ResourceCPU:    resource.MustParse("10"),
					core.ResourceMemory: resource.MustParse("64Gi"),
				}},
			}},
			Containers: []core.Container{{
				Name: testContainerName,
				Resources: core.ResourceRequirements{
					// Overcommitted requests vs the full instance size in limits.
					Requests: core.ResourceList{
						core.ResourceCPU:    resource.MustParse("200m"),
						core.ResourceMemory: resource.MustParse("2Gi"),
					},
					Limits: core.ResourceList{
						core.ResourceCPU:    resource.MustParse("2"),
						core.ResourceMemory: resource.MustParse("4Gi"),
					},
				},
			}},
		},
		Status: core.PodStatus{
			Phase:             core.PodRunning,
			ContainerStatuses: []core.ContainerStatus{{Name: testContainerName}},
		},
	}
	for _, m := range mutators {
		m(pod)
	}
	return pod
}

type usageHarness struct {
	t      *testing.T
	r      *UsageReconciler
	cl     client.Client
	rec    *recordingFake
	events *events.FakeRecorder
	now    time.Time
}

func newUsageHarness(t *testing.T, project string, pod *core.Pod) *usageHarness {
	t.Helper()
	s := testScheme(t)
	objs := []client.Object{newTestNamespace(project)}
	if pod != nil {
		objs = append(objs, pod)
	}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(&core.Pod{}).Build()
	h := &usageHarness{t: t, cl: cl, rec: &recordingFake{}, events: events.NewFakeRecorder(100)}
	h.r = &UsageReconciler{
		Client:        cl,
		Scheme:        s,
		Recorder:      h.rec,
		Events:        h.events,
		FlushInterval: 5 * time.Minute,
		Now:           func() time.Time { return h.now },
	}
	return h
}

func (h *usageHarness) reconcileAt(now time.Time) (ctrl.Result, error) {
	h.t.Helper()
	h.now = now
	return h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: testInstanceNamespace, Name: testInstanceName,
	}})
}

func (h *usageHarness) mustReconcileAt(now time.Time) ctrl.Result {
	h.t.Helper()
	res, err := h.reconcileAt(now)
	if err != nil {
		h.t.Fatalf("reconcile at %s: %v", now.Format(time.TimeOnly), err)
	}
	return res
}

func (h *usageHarness) pod() *core.Pod {
	h.t.Helper()
	var pod core.Pod
	if err := h.cl.Get(context.Background(), types.NamespacedName{Namespace: testInstanceNamespace, Name: testInstanceName}, &pod); err != nil {
		h.t.Fatalf("get pod: %v", err)
	}
	return &pod
}

func (h *usageHarness) podGone() bool {
	var pod core.Pod
	err := h.cl.Get(context.Background(), types.NamespacedName{Namespace: testInstanceNamespace, Name: testInstanceName}, &pod)
	return apierrors.IsNotFound(err)
}

// updateStatus mutates the stored Pod the way the kubelet would.
func (h *usageHarness) updatePod(mutate func(*core.Pod)) {
	h.t.Helper()
	pod := h.pod()
	mutate(pod)
	if err := h.cl.Status().Update(context.Background(), pod); err != nil {
		h.t.Fatalf("update pod status: %v", err)
	}
	pod = h.pod()
	mutate(pod)
	if err := h.cl.Update(context.Background(), pod); err != nil {
		h.t.Fatalf("update pod: %v", err)
	}
}

func (h *usageHarness) watermark() time.Time {
	h.t.Helper()
	w, ok := parseReportedUntil(h.pod())
	if !ok {
		h.t.Fatalf("pod has no watermark")
	}
	return w
}

func wantKey(meter string, start, end time.Time) string {
	return windowIdempotencyKey(testPodUID, meter, start, end)
}

func windowKeys(windows ...usageWindow) []string {
	out := make([]string, 0, 2*len(windows))
	for _, w := range windows {
		out = append(out, wantKey(MeterCPUMilliSeconds, w.start, w.end), wantKey(MeterMemorySeconds, w.start, w.end))
	}
	return out
}

func win(start, end time.Time) usageWindow { return usageWindow{start: start, end: end} }

func assertKeys(t *testing.T, got []string, want ...usageWindow) {
	t.Helper()
	if w := windowKeys(want...); !slices.Equal(got, w) {
		t.Fatalf("emitted keys:\n got %v\nwant %v", got, w)
	}
}

func TestUsageReconciler_IgnoresUnmanagedPod(t *testing.T) {
	pod := newManagedPod(withState(running(at(0, 0))))
	delete(pod.Labels, managedByLabel)
	h := newUsageHarness(t, testProjectName, pod)

	h.mustReconcileAt(at(20, 0))

	if len(h.rec.events) != 0 {
		t.Fatalf("expected no events, got %d", len(h.rec.events))
	}
	if controllerutil.ContainsFinalizer(h.pod(), usageFinalizer) {
		t.Fatal("unmanaged pod must not get the usage finalizer")
	}
}

func TestUsageReconciler_PendingPodIsHeldButNotBilled(t *testing.T) {
	h := newUsageHarness(t, testProjectName, newManagedPod(withPhase(core.PodPending), withState(core.ContainerState{
		Waiting: &core.ContainerStateWaiting{Reason: "ContainerCreating"},
	})))

	h.mustReconcileAt(at(1, 0))

	pod := h.pod()
	if !controllerutil.ContainsFinalizer(pod, usageFinalizer) {
		t.Fatal("expected the finalizer on a pending pod")
	}
	if _, ok := parseReportedUntil(pod); ok {
		t.Fatal("a pod that never ran must not carry a watermark")
	}
	if len(h.rec.events) != 0 {
		t.Fatalf("expected no usage, got %d events", len(h.rec.events))
	}
}

// The running stretch is reported on a wall-clock grid, the first window
// ending at the first boundary after the container started.
func TestUsageReconciler_RunningPodReportsOnGrid(t *testing.T) {
	h := newUsageHarness(t, testProjectName, newManagedPod(withState(running(at(1, 30)))))

	// First sight persists the watermark at the container start, emits nothing.
	h.mustReconcileAt(at(2, 0))
	if got := h.watermark(); !got.Equal(at(1, 30)) {
		t.Fatalf("watermark = %s, want container start", got)
	}
	if len(h.rec.events) != 0 {
		t.Fatalf("first sight must not emit, got %d events", len(h.rec.events))
	}

	res := h.mustReconcileAt(at(12, 0))
	assertKeys(t, h.rec.keys(), win(at(1, 30), at(5, 0)), win(at(5, 0), at(10, 0)))
	if got := h.watermark(); !got.Equal(at(10, 0)) {
		t.Fatalf("watermark = %s, want 10:10", got)
	}
	if want := 3*time.Minute + requeueSlack; res.RequeueAfter != want {
		t.Fatalf("RequeueAfter = %s, want %s", res.RequeueAfter, want)
	}

	first := h.rec.events[0]
	if first.Meter != MeterCPUMilliSeconds || first.Quantity != testCPUMilli*210 {
		t.Fatalf("first cpu event = %s %d, want %d millicore-seconds", first.Meter, first.Quantity, testCPUMilli*210)
	}
	mem := h.rec.events[1]
	if mem.Meter != MeterMemorySeconds || mem.Quantity != testMemoryBytes*210 {
		t.Fatalf("first memory event = %s %d, want %d byte-seconds", mem.Meter, mem.Quantity, testMemoryBytes*210)
	}
	if first.Project.Name != testProjectName {
		t.Fatalf("project = %q, want %q", first.Project.Name, testProjectName)
	}
	if first.Dimensions[runtimeClassDimension] != RuntimeClassName {
		t.Fatalf("dimensions = %v", first.Dimensions)
	}
	if first.Source != UsageSource || !first.OccurredAt.Equal(at(5, 0)) {
		t.Fatalf("source/occurredAt = %s %s", first.Source, first.OccurredAt)
	}
	if first.Resource == nil || first.Resource.UID != testInstanceUID || first.Resource.Kind != "Instance" ||
		first.Resource.Group != computev1alpha.GroupVersion.Group || first.Resource.Name != testInstanceName {
		t.Fatalf("resource = %+v", first.Resource)
	}

	// Nothing is due before the next boundary.
	h.mustReconcileAt(at(14, 59))
	if len(h.rec.events) != 4 {
		t.Fatalf("expected no new events before the boundary, got %d", len(h.rec.events))
	}
}

// A retry after the watermark patch was lost (conflict, stale cache read,
// leader change, crash) re-emits exactly the same events, later in wall
// clock, so the pipeline deduplicates them.
func TestUsageReconciler_ReplayAfterLostWatermarkIsIdentical(t *testing.T) {
	pod := newManagedPod(withState(running(at(1, 30))), withWatermark(at(1, 30)))
	h := newUsageHarness(t, testProjectName, pod)

	h.mustReconcileAt(at(12, 0))
	first := h.rec.keys()

	// Roll the watermark back as if the patch had never landed.
	h.updatePod(func(p *core.Pod) { setReportedUntil(p, at(1, 30)) })
	h.rec.events = nil

	h.mustReconcileAt(at(14, 37))
	if !slices.Equal(h.rec.keys(), first) {
		t.Fatalf("replay emitted different keys:\n got %v\nwant %v", h.rec.keys(), first)
	}
}

func TestUsageReconciler_ConflictingPatchRequeuesWithoutAdvancing(t *testing.T) {
	pod := newManagedPod(withState(running(at(0, 0))), withWatermark(at(0, 0)))
	h := newUsageHarness(t, testProjectName, pod)

	// Make the reconciler patch against a stale resourceVersion.
	stale := h.pod()
	h.updatePod(func(p *core.Pod) { p.Labels["touched"] = "true" })
	h.r.Client = &staleGetClient{Client: h.cl, pod: stale}

	res := h.mustReconcileAt(at(6, 0))
	if res.RequeueAfter == 0 {
		t.Fatal("expected a requeue after a conflict")
	}
	if got := h.watermark(); !got.Equal(at(0, 0)) {
		t.Fatalf("watermark moved to %s on conflict", got)
	}
	emitted := h.rec.keys()

	h.r.Client = h.cl
	h.mustReconcileAt(at(7, 0))
	if got := h.rec.keys()[len(emitted):]; !slices.Equal(got, emitted) {
		t.Fatalf("retry keys %v, want %v", got, emitted)
	}
	if got := h.watermark(); !got.Equal(at(5, 0)) {
		t.Fatalf("watermark = %s, want 10:05", got)
	}
}

type staleGetClient struct {
	client.Client
	pod *core.Pod
}

func (c *staleGetClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if p, ok := obj.(*core.Pod); ok {
		c.pod.DeepCopyInto(p)
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func TestUsageReconciler_PartialFailureKeepsDeliveredProgress(t *testing.T) {
	pod := newManagedPod(withState(running(at(0, 0))), withWatermark(at(0, 0)))
	h := newUsageHarness(t, testProjectName, pod)
	h.rec.failOn = func(ev emission.UsageEvent, _ int) error {
		if ev.Meter == MeterMemorySeconds && ev.OccurredAt.Equal(at(10, 0)) {
			return errors.New("collector unavailable")
		}
		return nil
	}

	if _, err := h.reconcileAt(at(11, 0)); err == nil {
		t.Fatal("expected the emission error to surface for backoff")
	}
	if got := h.watermark(); !got.Equal(at(5, 0)) {
		t.Fatalf("watermark = %s, want the end of the delivered window", got)
	}

	h.rec.failOn = nil
	h.rec.events = nil
	h.mustReconcileAt(at(11, 30))
	// The CPU event of the failed window is re-sent with the same key.
	assertKeys(t, h.rec.keys(), win(at(5, 0), at(10, 0)))
}

// A crash-looping container is billed only while it runs; the back-off gap is not.
func TestUsageReconciler_CrashLoopGapIsNotBilled(t *testing.T) {
	pod := newManagedPod(withState(running(at(0, 0))), withWatermark(at(5, 0)))
	h := newUsageHarness(t, testProjectName, pod)

	// The container exits at 10:06 and waits to restart.
	h.updatePod(func(p *core.Pod) {
		p.Status.ContainerStatuses[0].State = waiting()
		p.Status.ContainerStatuses[0].LastTerminationState = terminated(at(0, 0), at(6, 0))
	})
	h.mustReconcileAt(at(7, 0))
	assertKeys(t, h.rec.keys(), win(at(5, 0), at(6, 0)))

	// Still waiting much later: nothing accrues.
	h.mustReconcileAt(at(19, 0))
	if len(h.rec.events) != 2 {
		t.Fatalf("waiting container billed: %v", h.rec.keys())
	}

	// It restarts at 10:20.
	h.updatePod(func(p *core.Pod) { p.Status.ContainerStatuses[0].State = running(at(20, 0)) })
	h.mustReconcileAt(at(21, 0))
	if got := h.watermark(); !got.Equal(at(20, 0)) {
		t.Fatalf("watermark = %s, want the restart time", got)
	}
	h.mustReconcileAt(at(26, 0))
	assertKeys(t, h.rec.keys(), win(at(5, 0), at(6, 0)), win(at(20, 0), at(25, 0)))
}

// A restart the controller never observed (it was down) still bills the run
// that ended and skips the gap.
func TestUsageReconciler_MissedRestartBillsEndedRunAndSkipsGap(t *testing.T) {
	pod := newManagedPod(
		withState(running(at(20, 0))),
		withLastTermination(terminated(at(0, 0), at(7, 0))),
		withWatermark(at(5, 0)),
	)
	h := newUsageHarness(t, testProjectName, pod)

	h.mustReconcileAt(at(22, 0))
	assertKeys(t, h.rec.keys(), win(at(5, 0), at(7, 0)))
	if got := h.watermark(); !got.Equal(at(20, 0)) {
		t.Fatalf("watermark = %s, want the current run start", got)
	}
}

// Deletion keeps the clock running through the guest's graceful shutdown and
// closes the window when the kubelet reports the container terminated.
func TestUsageReconciler_DeletionWaitsForContainerTermination(t *testing.T) {
	pod := newManagedPod(withState(running(at(0, 0))), withWatermark(at(25, 0)), withDeletion(at(29, 0)))
	h := newUsageHarness(t, testProjectName, pod)

	res := h.mustReconcileAt(at(28, 40))
	if len(h.rec.events) != 0 {
		t.Fatalf("billed before the guest stopped: %v", h.rec.keys())
	}
	if !controllerutil.ContainsFinalizer(h.pod(), usageFinalizer) {
		t.Fatal("finalizer released while the guest was still running")
	}
	if res.RequeueAfter == 0 {
		t.Fatal("expected a requeue while waiting for termination")
	}

	h.updatePod(func(p *core.Pod) { p.Status.ContainerStatuses[0].State = terminated(at(0, 0), at(29, 10)) })
	h.mustReconcileAt(at(29, 12))
	assertKeys(t, h.rec.keys(), win(at(25, 0), at(29, 10)))
	if !h.podGone() {
		t.Fatal("expected the finalizer released and the pod removed")
	}
}

func TestUsageReconciler_DeletionOnLostNodeClosesAtDeletionTimestamp(t *testing.T) {
	pod := newManagedPod(withState(running(at(0, 0))), withWatermark(at(25, 0)), withDeletion(at(29, 0)))
	h := newUsageHarness(t, testProjectName, pod)

	// The kubelet never reports; at the grid boundary the window is flushed
	// only up to the DeletionTimestamp side of the grid.
	h.mustReconcileAt(at(30, 30))
	if len(h.rec.events) != 0 {
		t.Fatalf("billed past the DeletionTimestamp: %v", h.rec.keys())
	}

	h.mustReconcileAt(at(29, 0).Add(DefaultUsageTerminationGrace))
	assertKeys(t, h.rec.keys(), win(at(25, 0), at(29, 0)))
	if !h.podGone() {
		t.Fatal("expected the finalizer released")
	}
}

// A window the collector always rejects is retried like any failure (a
// systemic misconfiguration loses nothing once fixed), but once it is older
// than EmitGiveUp it is dropped loudly so the Pod's later usage flows again.
func TestUsageReconciler_RejectedWindowStallsOnlyUntilGiveUp(t *testing.T) {
	pod := newManagedPod(withState(running(at(0, 0))), withWatermark(at(0, 0)))
	h := newUsageHarness(t, testProjectName, pod)
	h.rec.failOn = func(ev emission.UsageEvent, _ int) error {
		if ev.OccurredAt.Equal(at(5, 0)) {
			return errors.New(emissionRejectedPrefix + ": HTTP 400")
		}
		return nil
	}

	if _, err := h.reconcileAt(at(6, 0)); err == nil {
		t.Fatal("expected a fresh rejection to be retried")
	}
	if got := h.watermark(); !got.Equal(at(0, 0)) {
		t.Fatalf("watermark = %s, want it held while the rejection is retried", got)
	}

	h.mustReconcileAt(at(5, 0).Add(DefaultUsageEmitGiveUp))
	if got := h.watermark(); !got.Equal(at(35, 0)) {
		t.Fatalf("watermark = %s, want the rejected window skipped and the rest delivered", got)
	}
	if got := len(h.rec.events); got != 2*6 {
		t.Fatalf("delivered %d events, want the 6 windows after the rejected one", got)
	}
	if ev := <-h.events.Events; !strings.Contains(ev, "UsageRejected") {
		t.Fatalf("expected a UsageRejected event, got %q", ev)
	}
}

// TestUnrecoverable pins the classification against the billing SDK itself:
// the SDK has no typed error for a collector rejection, so a reworded message
// in a new SDK version must fail here rather than silently stall Pods.
func TestUnrecoverable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	sdk, err := emission.NewUsageRecorder(emission.WithEndpoint(srv.URL))
	if err != nil {
		t.Fatalf("new recorder: %v", err)
	}
	valid := emission.UsageEvent{
		Meter:      MeterCPUMilliSeconds,
		Project:    emission.ProjectRef{Name: testProjectName},
		Source:     UsageSource,
		Quantity:   1,
		OccurredAt: at(5, 0),
	}

	rejected := sdk.Record(context.Background(), valid)
	invalidEv := valid
	invalidEv.Quantity = 0
	invalid := sdk.Record(context.Background(), invalidEv)

	r := &UsageReconciler{}
	w := win(at(0, 0), at(5, 0))
	tests := []struct {
		name       string
		err        error
		now        time.Time
		wantReason string
		wantDrop   bool
	}{
		{"validation error drops at once", invalid, at(6, 0), "invalid", true},
		{"fresh rejection is retried", rejected, at(6, 0), "", false},
		{"old rejection is dropped", rejected, at(5, 0).Add(DefaultUsageEmitGiveUp), "rejected", true},
		{"transient failure is retried", errors.New("emission: endpoint unavailable after 5 attempts"), at(5, 0).Add(48 * time.Hour), "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err == nil {
				t.Fatal("expected the SDK to return an error")
			}
			reason, drop := r.unrecoverable(tt.err, w, tt.now)
			if reason != tt.wantReason || drop != tt.wantDrop {
				t.Fatalf("unrecoverable(%v) = %q, %v; want %q, %v", tt.err, reason, drop, tt.wantReason, tt.wantDrop)
			}
		})
	}
}

func TestUsageReconciler_CollectorOutageDoesNotBlockDeletionForever(t *testing.T) {
	pod := newManagedPod(withState(terminated(at(0, 0), at(29, 10))), withWatermark(at(25, 0)), withDeletion(at(29, 30)))
	h := newUsageHarness(t, testProjectName, pod)
	h.rec.failOn = func(emission.UsageEvent, int) error { return errors.New("collector unavailable") }

	if _, err := h.reconcileAt(at(30, 0)); err == nil {
		t.Fatal("expected the error to surface while within the give-up window")
	}
	if h.podGone() {
		t.Fatal("finalizer released before the give-up deadline")
	}

	h.mustReconcileAt(at(29, 30).Add(DefaultUsageEmitGiveUp))
	if !h.podGone() {
		t.Fatal("expected the finalizer released after the give-up deadline")
	}
	if ev := <-h.events.Events; !strings.Contains(ev, "UsageDropped") {
		t.Fatalf("expected a UsageDropped event, got %q", ev)
	}
}

func TestUsageReconciler_TerminalPodFlushesOnceAndReleases(t *testing.T) {
	pod := newManagedPod(
		withPhase(core.PodSucceeded),
		withState(terminated(at(1, 0), at(8, 20))),
		withWatermark(at(5, 0)),
	)
	h := newUsageHarness(t, testProjectName, pod)

	h.mustReconcileAt(at(9, 0))
	assertKeys(t, h.rec.keys(), win(at(5, 0), at(8, 20)))
	got := h.pod()
	if controllerutil.ContainsFinalizer(got, usageFinalizer) {
		t.Fatal("finalizer must be released on a terminal pod")
	}

	h.mustReconcileAt(at(30, 0))
	if len(h.rec.events) != 2 {
		t.Fatalf("terminal pod billed twice: %v", h.rec.keys())
	}
}

// A stretch that started and ended between two reconciles is still billed.
func TestUsageReconciler_ShortLivedPodIsBilledExactly(t *testing.T) {
	pod := newManagedPod(withPhase(core.PodPending))
	h := newUsageHarness(t, testProjectName, pod)
	h.mustReconcileAt(at(0, 0))

	h.updatePod(func(p *core.Pod) {
		p.Status.Phase = core.PodSucceeded
		p.Status.ContainerStatuses[0].State = terminated(at(0, 10), at(0, 30))
	})
	h.mustReconcileAt(at(1, 0)) // persists the watermark
	h.mustReconcileAt(at(1, 0))

	assertKeys(t, h.rec.keys(), win(at(0, 10), at(0, 30)))
	if got := h.rec.billedSeconds(); got != 20 {
		t.Fatalf("billed %v seconds, want 20", got)
	}
}

func TestUsageReconciler_UnattributableUsageIsDroppedLoudly(t *testing.T) {
	pod := newManagedPod(
		withPhase(core.PodFailed),
		withState(terminated(at(0, 0), at(3, 0))),
		withWatermark(at(0, 0)),
	)
	h := newUsageHarness(t, "", pod)

	h.mustReconcileAt(at(4, 0))
	if len(h.rec.events) != 0 {
		t.Fatalf("unattributable usage emitted: %v", h.rec.keys())
	}
	got := h.pod()
	if controllerutil.ContainsFinalizer(got, usageFinalizer) {
		t.Fatal("an unlabeled namespace must not block teardown")
	}
	if w, _ := parseReportedUntil(got); !w.Equal(at(3, 0)) {
		t.Fatalf("watermark = %s, want dropped usage marked handled", w)
	}
	if ev := <-h.events.Events; !strings.Contains(ev, "UsageUnattributable") {
		t.Fatalf("expected a UsageUnattributable event, got %q", ev)
	}
}

func TestUsageReconciler_BackfillIsCappedForUntrackedPods(t *testing.T) {
	pod := newManagedPod(withState(running(base.Add(-72 * time.Hour))))
	h := newUsageHarness(t, testProjectName, pod)

	h.mustReconcileAt(at(0, 0))
	if got, want := h.watermark(), at(0, 0).Add(-DefaultUsageMaxBackfill); !got.Equal(want) {
		t.Fatalf("watermark = %s, want %s", got, want)
	}
}

func TestUsageReconciler_CatchUpIsBoundedPerReconcile(t *testing.T) {
	pod := newManagedPod(withState(running(base.Add(-24*time.Hour))), withWatermark(base.Add(-24*time.Hour)))
	h := newUsageHarness(t, testProjectName, pod)

	res := h.mustReconcileAt(at(0, 0))
	if got := len(h.rec.events); got != 2*maxWindowsPerReconcile {
		t.Fatalf("emitted %d events, want %d", got, 2*maxWindowsPerReconcile)
	}
	if res.RequeueAfter == 0 || res.RequeueAfter > time.Second {
		t.Fatalf("expected an immediate requeue, got %s", res.RequeueAfter)
	}
	for range 100 {
		h.mustReconcileAt(at(0, 0))
	}
	if got := h.rec.billedSeconds(); got != (24 * time.Hour).Seconds() {
		t.Fatalf("billed %v seconds, want a full day", got)
	}
}

func TestGridWindows(t *testing.T) {
	interval := 5 * time.Minute
	tests := []struct {
		name     string
		from, to time.Time
		final    bool
		want     []usageWindow
	}{
		{"before first boundary", at(1, 0), at(4, 0), false, nil},
		{"final partial", at(1, 0), at(4, 0), true, []usageWindow{win(at(1, 0), at(4, 0))}},
		{"exact boundary", at(1, 0), at(5, 0), false, []usageWindow{win(at(1, 0), at(5, 0))}},
		{"across boundaries", at(1, 0), at(11, 0), true, []usageWindow{
			win(at(1, 0), at(5, 0)), win(at(5, 0), at(10, 0)), win(at(10, 0), at(11, 0)),
		}},
		{"empty", at(5, 0), at(5, 0), true, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, more := gridWindows(tt.from, tt.to, interval, tt.final, 100)
			if more || !slices.Equal(got, tt.want) {
				t.Fatalf("got %v (more=%v), want %v", got, more, tt.want)
			}
		})
	}
}

func TestPodResourcesUsesLimitsAndIgnoresInitContainers(t *testing.T) {
	cpu, mem := podResources(newManagedPod())
	if cpu != testCPUMilli || mem != testMemoryBytes {
		t.Fatalf("got cpu=%d mem=%d, want limits %d/%d", cpu, mem, testCPUMilli, testMemoryBytes)
	}

	noLimits := newManagedPod(func(p *core.Pod) { p.Spec.Containers[0].Resources.Limits = nil })
	cpu, mem = podResources(noLimits)
	if cpu != 200 || mem != 2<<30 {
		t.Fatalf("without limits got cpu=%d mem=%d, want the requests", cpu, mem)
	}
}

func TestDecodeProjectName(t *testing.T) {
	// Mirrors compute's projectClusterNameFromLabel cases.
	tests := map[string]string{
		"cluster-project-htxrg":          "project-htxrg",
		"cluster-datum-cloud":            "datum-cloud",
		"cluster-test-project-cluster":   "test-project-cluster",
		"cluster-projects_project-htxrg": "project-htxrg",
		"cluster-_test-project-abc":      "test-project-abc",
		"cluster-org_project":            "project",
		" cluster-p1 ":                   "p1",
		"project-htxrg":                  "",
		"cluster-":                       "",
		"cluster-org_":                   "",
		"":                               "",
	}
	for in, want := range tests {
		if got := decodeProjectName(in); got != want {
			t.Errorf("decodeProjectName(%q) = %q, want %q", in, got, want)
		}
	}
}
