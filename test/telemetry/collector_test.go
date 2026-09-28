// SPDX-License-Identifier: AGPL-3.0-only

package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	collectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	_ "google.golang.org/grpc/encoding/gzip" // Match the shipped OTLP exporter's compression.
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"
)

const (
	fixtureStandalone = "standalone"
	collectorVersion  = "0.144.0"
)

// TestCollector exercises the shipped pipeline with the pinned stock collector,
// real CRI files, a Kubernetes API fixture, and an OTLP endpoint. It needs no
// cluster, credentials, container engine, or changes to the user's kubeconfig.
func TestCollector(t *testing.T) {
	binary := os.Getenv("OTELCOL_BIN")
	if binary == "" {
		t.Skip("set OTELCOL_BIN to the OpenTelemetry contrib 0.144.0 executable")
	}
	version, err := exec.Command(binary, "--version").CombinedOutput()
	if err != nil || !strings.Contains(string(version), collectorVersion) {
		t.Fatalf("need collector 0.144.0: %s (%v)", version, err)
	}
	dir := t.TempDir()
	pods := []corev1.Pod{
		pod("tenant-a", "11111111-1111-1111-1111-111111111111", true),
		pod("tenant-b", "22222222-2222-2222-2222-222222222222", true),
		pod("unmapped", "33333333-3333-3333-3333-333333333333", true),
		pod("tenant-a", "44444444-4444-4444-4444-444444444444", false),
		pod("tenant-a", "55555555-5555-5555-5555-555555555555", true),
		pod("tenant-a", "66666666-6666-6666-6666-666666666666", true),
		pod("tenant-a", "77777777-7777-7777-7777-777777777777", true),
	}
	pods[3].Name = "other-provider"
	pods[4].Name = fixtureStandalone
	pods[4].Labels["upstream.instance"] = fixtureStandalone
	delete(pods[4].Labels, "compute.datumapis.com/workload-name")
	pods[5].Name = "not-enabled"
	delete(pods[5].Labels, "telemetry.miloapis.com/otlp-native-logs")
	pods[6].Name = "other-node"
	pods[6].Spec.NodeName = "other-node"
	// Fast crashes must remain collectible even if the collector never observed
	// a Running container. Kubernetes discovery receivers miss this case.
	pods[1].Status.Phase = corev1.PodFailed
	pods[1].Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "app", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
	}}
	kube := kubernetesAPI(t, pods)
	t.Cleanup(kube.Close)
	write(t, filepath.Join(dir, "kubeconfig"), fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: fixture
  cluster:
    server: %s
contexts:
- name: fixture
  context:
    cluster: fixture
    user: fixture
current-context: fixture
users:
- name: fixture
  user: {}
`, kube.URL))

	sink := &logSink{}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	collectorlogs.RegisterLogsServiceServer(server, sink)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	config := collectorConfig(t, dir)
	env := append(os.Environ(), "KUBECONFIG="+filepath.Join(dir, "kubeconfig"),
		"K8S_NODE_NAME=fixture-node", "LOGS_OTLP_ENDPOINT="+listener.Addr().String())

	appLog := logPath(dir, pods[0], "app", "0.log")
	write(t, appLog, cri("stdout", "startup-a")+cri("stderr", "crash-a"))
	write(t, logPath(dir, pods[0], "worker", "0.log"), cri("stdout", "worker-a"))
	write(t, logPath(dir, pods[1], "app", "0.log"), cri("stderr", "startup-crash-b"))
	write(t, logPath(dir, pods[2], "app", "0.log"), cri("stdout", "missing-project"))
	write(t, logPath(dir, pods[3], "app", "0.log"), cri("stdout", "other-provider"))
	write(t, logPath(dir, pods[4], "app", "0.log"), cri("stdout", fixtureStandalone))
	write(t, logPath(dir, pods[5], "app", "0.log"), cri("stdout", "not-enabled"))
	write(t, logPath(dir, pods[6], "app", "0.log"), cri("stdout", "other-node"))
	spoof := `{"message":"spoof","project_name":"project-b","datum.project.name":"project-b"}`
	appendLog(t, appLog, cri("stdout", spoof))
	// CRI partial records are joined without losing their source or stream.
	appendLog(t, appLog, "2026-01-02T03:04:05.123456789Z stdout P partial-\n"+
		"2026-01-02T03:04:05.123456789Z stdout F complete\n")

	process := startCollector(t, binary, config, env)
	sink.wait(t, "startup-a", "crash-a", "worker-a", "startup-crash-b", spoof, "partial-complete", fixtureStandalone)
	sink.assertIdentity(t, "startup-a", "project-a", "app", "stdout")
	sink.assertIdentity(t, "crash-a", "project-a", "app", "stderr")
	sink.assertIdentity(t, "worker-a", "project-a", "worker", "stdout")
	sink.assertIdentity(t, "startup-crash-b", "project-b", "app", "stderr")
	sink.assertIdentity(t, spoof, "project-a", "app", "stdout")
	process.stop(t)

	// Rotate while the collector is stopped: the unread tail in the renamed
	// file and the replacement file must both arrive, with old entries skipped.
	appendLog(t, appLog, cri("stdout", "rotation-tail"))
	if err := os.Rename(appLog, appLog+".20260102"); err != nil {
		t.Fatal(err)
	}
	write(t, appLog, cri("stdout", "after-rotation"))
	write(t, logPath(dir, pods[0], "app", "1.log"), cri("stderr", "container-restart"))
	process = startCollector(t, binary, config, env)
	sink.wait(t, "rotation-tail", "after-rotation", "container-restart")
	process.stop(t)

	// Refuse delivery, persist the queue, then restart the collector while the
	// endpoint recovers. This tests disk-backed delivery, not just HTTP retry.
	sink.unavailable.Store(true)
	process = startCollector(t, binary, config, env)
	appendLog(t, appLog, cri("stdout", "outage-survivor"))
	eventually(t, func() bool { return sink.refusals.Load() > 0 }, "collector to attempt export during outage")
	process.stop(t)
	sink.unavailable.Store(false)
	process = startCollector(t, binary, config, env)
	sink.wait(t, "outage-survivor")
	// Let two more file polls detect an accidental replay after restart.
	time.Sleep(2200 * time.Millisecond)
	process.stop(t)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, body := range []string{"missing-project", "other-provider", "not-enabled", "other-node"} {
		if len(sink.records[body]) != 0 {
			t.Errorf("exported a record without trusted Kata project identity: %q", body)
		}
	}
	standalone := sink.records[fixtureStandalone][0].resource
	if standalone["datum.instance.name"] != fixtureStandalone || standalone["project_name"] != "project-a" {
		t.Errorf("standalone instance identity = %v", standalone)
	}
	for body, records := range sink.records {
		if len(records) != 1 {
			t.Errorf("%q exported %d times, want once in these acknowledged-delivery scenarios", body, len(records))
		}
	}
}

func collectorConfig(t *testing.T, dir string) string {
	t.Helper()
	manifest, err := os.ReadFile("../../config/dependencies/kata-telemetry/collector.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Spec struct {
			Image  string         `json:"image"`
			Config map[string]any `json:"config"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(manifest, &document); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(document.Spec.Image, ":"+collectorVersion) {
		t.Fatalf("test collector version %s does not match shipped image %s", collectorVersion, document.Spec.Image)
	}
	config, err := yaml.Marshal(document.Spec.Config)
	if err != nil {
		t.Fatal(err)
	}
	// Only replace infrastructure paths and Kubernetes authentication. Parsing,
	// filtering, routing, storage and export settings are the shipped settings.
	text := strings.ReplaceAll(string(config), "/var/log/pods", filepath.Join(dir, "pods"))
	text = strings.ReplaceAll(text, "/var/lib/otelcol/storage", filepath.Join(dir, "storage"))
	text = strings.ReplaceAll(text, "auth_type: serviceAccount", "auth_type: kubeConfig")
	path := filepath.Join(dir, "collector.yaml")
	write(t, path, text)
	if err := os.MkdirAll(filepath.Join(dir, "storage"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

type collectorProcess struct {
	cmd  *exec.Cmd
	done chan error
	log  string
	once sync.Once
}

func startCollector(t *testing.T, binary, config string, env []string) *collectorProcess {
	t.Helper()
	log, err := os.CreateTemp(t.TempDir(), "collector-*.log")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "--config", config)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &collectorProcess{cmd: cmd, done: make(chan error, 1), log: log.Name()}
	go func() { p.done <- cmd.Wait(); _ = log.Close() }()
	t.Cleanup(func() { p.stop(t) })
	return p
}

func (p *collectorProcess) stop(t *testing.T) {
	t.Helper()
	p.once.Do(func() {
		_ = p.cmd.Process.Signal(os.Interrupt)
		select {
		case err := <-p.done:
			if err != nil {
				output, _ := os.ReadFile(p.log)
				t.Errorf("collector failed: %v\n%s", err, output)
			}
		case <-time.After(15 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.done
			t.Error("collector did not stop within 15 seconds")
		}
		if t.Failed() {
			output, _ := os.ReadFile(p.log)
			t.Logf("collector output:\n%s", output)
		}
	})
}

type record struct {
	resource map[string]string
	attrs    map[string]string
	time     uint64
}

type logSink struct {
	collectorlogs.UnimplementedLogsServiceServer
	mu          sync.Mutex
	records     map[string][]record
	unavailable atomic.Bool
	refusals    atomic.Int64
}

func (s *logSink) Export(
	_ context.Context, request *collectorlogs.ExportLogsServiceRequest,
) (*collectorlogs.ExportLogsServiceResponse, error) {
	if s.unavailable.Load() {
		s.refusals.Add(1)
		return nil, status.Error(codes.Unavailable, "test outage")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.records == nil {
		s.records = make(map[string][]record)
	}
	for _, resource := range request.ResourceLogs {
		for _, scope := range resource.ScopeLogs {
			for _, log := range scope.LogRecords {
				body := log.Body.GetStringValue()
				s.records[body] = append(s.records[body], record{
					resource: attributes(resource.Resource.Attributes), attrs: attributes(log.Attributes), time: log.TimeUnixNano,
				})
			}
		}
	}
	return &collectorlogs.ExportLogsServiceResponse{}, nil
}

func (s *logSink) wait(t *testing.T, bodies ...string) {
	t.Helper()
	eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, body := range bodies {
			if len(s.records[body]) == 0 {
				return false
			}
		}
		return true
	}, fmt.Sprintf("logs %q", bodies))
}

func (s *logSink) assertIdentity(t *testing.T, body, project, container, stream string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.records[body][0]
	for key, want := range map[string]string{
		"project_name": project, "datum.project.name": project,
		"datum.instance.namespace": "default", "datum.instance.name": "same-name",
		"datum.workload.name": "same-workload", "k8s.container.name": container,
		"k8s.node.name": "fixture-node",
	} {
		if r.resource[key] != want {
			t.Errorf("%q: %s = %q, want %q", body, key, r.resource[key], want)
		}
	}
	if r.attrs["log.iostream"] != stream {
		t.Errorf("%q: stream = %q, want %q", body, r.attrs["log.iostream"], stream)
	}
	want, _ := time.Parse(time.RFC3339Nano, "2026-01-02T03:04:05.123456789Z")
	if r.time != uint64(want.UnixNano()) {
		t.Errorf("%q: original CRI timestamp was not preserved", body)
	}
}

func attributes(attrs []*common.KeyValue) map[string]string {
	result := make(map[string]string, len(attrs))
	for _, attr := range attrs {
		result[attr.Key] = attr.Value.GetStringValue()
	}
	return result
}

func kubernetesAPI(t *testing.T, pods []corev1.Pod) *httptest.Server {
	t.Helper()
	namespaces := make([]corev1.Namespace, 0, 3)
	for _, name := range []string{"tenant-a", "tenant-b", "unmapped"} {
		ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, ResourceVersion: "1"}}
		if name != "unmapped" {
			ns.Labels = map[string]string{
				"resourcemanager.miloapis.com/project-name": "project-" + strings.TrimPrefix(name, "tenant-"),
				"meta.datumapis.com/upstream-namespace":     "default",
			}
		}
		namespaces = append(namespaces, ns)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("watch") == "true" {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		switch r.URL.Path {
		case "/api/v1/pods":
			selector, err := labels.Parse(r.URL.Query().Get("labelSelector"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			fieldSelector, err := fields.ParseSelector(r.URL.Query().Get("fieldSelector"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			items := []corev1.Pod{}
			for _, pod := range pods {
				if selector.Matches(labels.Set(pod.Labels)) &&
					fieldSelector.Matches(fields.Set{"spec.nodeName": pod.Spec.NodeName}) {
					items = append(items, pod)
				}
			}
			_ = json.NewEncoder(w).Encode(corev1.PodList{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
				ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: items,
			})
		case "/api/v1/namespaces":
			_ = json.NewEncoder(w).Encode(corev1.NamespaceList{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NamespaceList"},
				ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: namespaces,
			})
		default:
			http.Error(w, "unsupported fixture endpoint: "+r.URL.Path, http.StatusNotFound)
		}
	}))
}

func pod(namespace, uid string, managed bool) corev1.Pod {
	owner := "kata-provider"
	if !managed {
		owner = "another-provider"
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "same-name", Namespace: namespace, UID: types.UID(uid), ResourceVersion: "1",
			Labels: map[string]string{
				"managed-by": owner, "upstream.instance": "same-name",
				"compute.datumapis.com/workload-name":     "same-workload",
				"telemetry.miloapis.com/otlp-native-logs": "true",
			},
		},
		Spec:   corev1.PodSpec{NodeName: "fixture-node", Containers: []corev1.Container{{Name: "app"}, {Name: "worker"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func logPath(dir string, pod corev1.Pod, container, file string) string {
	return filepath.Join(dir, "pods", pod.Namespace+"_"+pod.Name+"_"+string(pod.UID), container, file)
}

func cri(stream, body string) string {
	return "2026-01-02T03:04:05.123456789Z " + stream + " F " + body + "\n"
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func appendLog(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, check func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}
