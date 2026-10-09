//go:build linux

package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	tasks "github.com/containerd/containerd/api/services/tasks/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/filters"
	"google.golang.org/grpc"

	containers "github.com/containerd/containerd/api/services/containers/v1"
	task "github.com/containerd/containerd/api/types/task"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/types/known/anypb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type stalledHistoryReader struct{ client.Client }

func (r stalledHistoryReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, history := list.(*lab.LabList); history {
		<-ctx.Done()
		return ctx.Err()
	}
	return r.Client.List(ctx, list, opts...)
}

func currentReporterFixture(t *testing.T) (*LifecycleReporter, client.Client, *lab.Device) {
	t.Helper()
	o, _ := scopeProducerAdapter(t, nil, nil)
	parent := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "live", Namespace: "ns", UID: "lab", Generation: 2}, Spec: lab.LabSpec{Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "stop", Revision: 2, SnapshotMode: "Required"}}}
	d := &lab.Device{ObjectMeta: metav1.ObjectMeta{Name: "device", Namespace: "ns", UID: "device", OwnerReferences: []metav1.OwnerReference{{Kind: "Lab", Name: parent.Name, UID: parent.UID}}}, Spec: lab.DeviceSpec{Name: "writer", LabRef: parent.Name}, Status: lab.DeviceStatus{PodName: "writer", NodeName: "node"}}
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "writer", Namespace: "ns", UID: "pod", Labels: map[string]string{names.LabelDevice: "writer", names.LabelLab: parent.Name}, OwnerReferences: []metav1.OwnerReference{{Kind: "Device", Name: d.Name, UID: d.UID}}}, Spec: corev1.PodSpec{NodeName: "node"}}
	c := fake.NewClientBuilder().WithScheme(o.Reader.(client.Client).Scheme()).WithStatusSubresource(&lab.Lab{}, &lab.Device{}).WithIndex(&corev1.Pod{}, "spec.nodeName", func(object client.Object) []string { return []string{object.(*corev1.Pod).Spec.NodeName} }).WithObjects(parent, d, p).Build()
	path := filepath.Join(o.CgroupRoot, "live")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "cgroup.events"), []byte("populated 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(specs.Spec{Linux: &specs.Linux{CgroupsPath: "live"}})
	o.Runtime = scopeRuntimeAdapter(t, []*containers.Container{{ID: "live", Labels: map[string]string{"io.kubernetes.pod.uid": "pod"}, Spec: &anypb.Any{TypeUrl: "types.containerd.io/opencontainers/runtime-spec/1/Spec", Value: spec}}}, &task.Process{ID: "live", Pid: 42, Status: task.Status_RUNNING})
	o.Reader = c
	return &LifecycleReporter{Client: c, Reader: c, Observer: o}, c, d
}

func TestReporterHistoryTimeoutDoesNotStarveCurrentNativeReport(t *testing.T) {
	r, c, d := currentReporterFixture(t)
	r.Reader = stalledHistoryReader{c}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := r.sync(ctx); err == nil {
		t.Fatal("history timeout was hidden")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(d), d); err != nil {
		t.Fatal(err)
	}
	if len(d.Status.RuntimeReports) != 1 || d.Status.RuntimeReports[0].RuntimeState != "Allocated" || d.Status.RuntimeReports[0].Error != "" || d.Status.RuntimeReports[0].ObservedAt == nil {
		t.Fatalf("slow unrelated Lab history starved genuine current native report: %+v", d.Status.RuntimeReports)
	}
}

// Enforce the actual containerd selector at the native RPC boundary. The
// producer must still verify returned labels before attributing an owner.
type selectedOwnerContainers struct {
	containers.UnimplementedContainersServer
	item     *containers.Container
	observed []string
}

func (s *selectedOwnerContainers) List(_ context.Context, r *containers.ListContainersRequest) (*containers.ListContainersResponse, error) {
	s.observed = append([]string(nil), r.Filters...)
	if len(r.Filters) != 1 || r.Filters[0] != `labels."io.kubernetes.pod.uid"=="pod"` {
		return nil, fmt.Errorf("unbounded metadata scan")
	}
	parsed, err := filters.ParseAll(r.Filters...)
	if err != nil {
		return nil, err
	}
	matches := func(owner string) bool {
		return parsed.Match(filters.AdapterFunc(func(path []string) (string, bool) {
			if len(path) == 2 && path[0] == "labels" && path[1] == "io.kubernetes.pod.uid" {
				return owner, true
			}
			return "", false
		}))
	}
	if !matches("pod") || matches("foreign") {
		return nil, fmt.Errorf("selector did not match only its immutable owner")
	}
	return &containers.ListContainersResponse{Containers: []*containers.Container{s.item}}, nil
}
func (s *selectedOwnerContainers) Get(_ context.Context, r *containers.GetContainerRequest) (*containers.GetContainerResponse, error) {
	if r.ID != s.item.ID {
		return nil, fmt.Errorf("foreign metadata queried")
	}
	return &containers.GetContainerResponse{Container: s.item}, nil
}
func TestReporterCurrentObservationQueriesOnlyOwnedContainerMetadata(t *testing.T) {
	r, c, d := currentReporterFixture(t)
	spec, _ := json.Marshal(specs.Spec{Linux: &specs.Linux{CgroupsPath: "live"}})
	server := grpc.NewServer()
	adapter := &selectedOwnerContainers{item: &containers.Container{ID: "live", Labels: map[string]string{"io.kubernetes.pod.uid": "pod"}, Spec: &anypb.Any{TypeUrl: "types.containerd.io/opencontainers/runtime-spec/1/Spec", Value: spec}}}
	containers.RegisterContainersServer(server, adapter)
	tasks.RegisterTasksServer(server, &scopeTaskAdapter{items: []*task.Process{{ID: "live", Pid: 42, Status: task.Status_RUNNING}}})
	dir, err := os.MkdirTemp("", "cice-select-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "runtime.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(ln)
	t.Cleanup(server.Stop)
	rt, err := containerd.New(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	r.Observer.Runtime = rt
	if err := r.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(d), d); err != nil {
		t.Fatal(err)
	}
	if len(d.Status.RuntimeReports) != 1 || d.Status.RuntimeReports[0].RuntimeState != "Allocated" || d.Status.RuntimeReports[0].Error != "" {
		t.Fatalf("current owner observation scanned foreign metadata: %+v; filters=%v", d.Status.RuntimeReports, adapter.observed)
	}
}

type stalledDeviceReader struct{ client.Client }

func (r stalledDeviceReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	if _, isLab := object.(*lab.Lab); isLab && key.Name == "slow" {
		<-ctx.Done()
		return ctx.Err()
	}
	return r.Client.Get(ctx, key, object, opts...)
}
func TestReporterCurrentTimeoutResumesAtFollowingDevice(t *testing.T) {
	r, c, d := currentReporterFixture(t)
	slow := &lab.Device{ObjectMeta: metav1.ObjectMeta{Name: "aaa", Namespace: "ns", UID: "slow", OwnerReferences: []metav1.OwnerReference{{Kind: "Lab", Name: "slow", UID: "slow"}}}, Spec: lab.DeviceSpec{Name: "slow", LabRef: "slow"}}
	if err := c.Create(context.Background(), slow); err != nil {
		t.Fatal(err)
	}
	r.Reader = stalledDeviceReader{c}
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods); err != nil {
		t.Fatal(err)
	}
	first, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_ = r.syncDevices(first, &pods, true)
	cancel()
	second, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	_ = r.syncDevices(second, &pods, true)
	cancel()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(d), d); err != nil {
		t.Fatal(err)
	}
	if len(d.Status.RuntimeReports) != 1 || d.Status.RuntimeReports[0].RuntimeState != "Allocated" || d.Status.RuntimeReports[0].Error != "" {
		t.Fatalf("first timed-out object continually starved following live owner: %+v", d.Status.RuntimeReports)
	}
}
func TestReporterCancellationStopsWithoutWaitingForAnotherTick(t *testing.T) {
	r, c, _ := currentReporterFixture(t)
	r.Reader = stalledHistoryReader{c}
	r.Interval = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled reporter kept waiting/scanning")
	}
}
func TestReporterCurrentObservationPreservesRetainedAndForeignRows(t *testing.T) {
	r, c, d := currentReporterFixture(t)
	old := lab.OwnedRuntimeIdentity{OwnerUID: "lab", PodUID: "old", NodeName: "node", NodeBootID: "boot", OperationID: "initial", Revision: 1}
	foreign := old
	foreign.NodeName = "other"
	d.Status.RuntimeReports = []lab.OwnedRuntimeReport{{Identity: old, RuntimeState: "Unknown", Error: "unresolved retained debt"}, {Identity: foreign, RuntimeState: "Unknown", Error: "other node debt"}}
	if err := c.Status().Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods); err != nil {
		t.Fatal(err)
	}
	if err := r.syncDevices(context.Background(), &pods, true); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(d), d); err != nil {
		t.Fatal(err)
	}
	if len(d.Status.RuntimeReports) != 3 || d.Status.RuntimeReports[0].Error != "unresolved retained debt" || d.Status.RuntimeReports[1].Error != "other node debt" || d.Status.RuntimeReports[2].RuntimeState != "Allocated" {
		t.Fatalf("current publication erased honest history or foreign node: %+v", d.Status.RuntimeReports)
	}
}

func TestReporterHistoryDoesNotResampleOrEraseCurrentOwner(t *testing.T) {
	r, c, d := currentReporterFixture(t)
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods); err != nil {
		t.Fatal(err)
	}
	if err := r.syncDevices(context.Background(), &pods, true); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(d), d); err != nil {
		t.Fatal(err)
	}
	if len(d.Status.RuntimeReports) != 1 {
		t.Fatal("missing current native report")
	}
	before := *d.Status.RuntimeReports[0].DeepCopy()
	d.Status.RuntimeInventory = []lab.OwnedRuntimeIdentity{before.Identity}
	if err := c.Status().Update(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	// Closing native transport makes any repeated current observation fail.
	if err := r.Observer.Runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.syncDevices(context.Background(), &pods, false); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(d), d); err != nil {
		t.Fatal(err)
	}
	if len(d.Status.RuntimeReports) != 1 || !reflect.DeepEqual(d.Status.RuntimeReports[0], before) {
		t.Fatalf("historical work replaced or erased unsampled current report: %+v", d.Status.RuntimeReports)
	}
}

func TestReporterCurrentFailedObservationReplacesOlderSuccess(t *testing.T) {
	r, c, d := currentReporterFixture(t)
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods); err != nil {
		t.Fatal(err)
	}
	if err := r.syncDevices(context.Background(), &pods, true); err != nil {
		t.Fatal(err)
	}
	if err := r.Observer.Runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.syncDevices(context.Background(), &pods, true); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(d), d); err != nil {
		t.Fatal(err)
	}
	if len(d.Status.RuntimeReports) != 1 || d.Status.RuntimeReports[0].RuntimeState != "Unknown" || d.Status.RuntimeReports[0].Error == "" {
		t.Fatalf("genuine current failure retained stale success: %+v", d.Status.RuntimeReports)
	}
}
func TestReporterContainerSelectorDoesNotGrantOwnership(t *testing.T) {
	r, c, d := currentReporterFixture(t)
	r.Observer.Runtime = scopeRuntimeAdapter(t, []*containers.Container{{ID: "foreign", Labels: map[string]string{"io.kubernetes.pod.uid": "foreign"}}}, &task.Process{ID: "foreign", Pid: 42, Status: task.Status_RUNNING})
	if err := r.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(d), d); err != nil {
		t.Fatal(err)
	}
	if len(d.Status.RuntimeReports) != 1 || d.Status.RuntimeReports[0].RuntimeState != "Unknown" || d.Status.RuntimeReports[0].Error == "" {
		t.Fatalf("untrusted selector result attributed foreign owner: %+v", d.Status.RuntimeReports)
	}
}

func TestReporterEmptyDevicePassHasNoCursorArithmetic(t *testing.T) {
	o, _ := scopeProducerAdapter(t, nil, nil)
	c := o.Reader.(client.Client)
	r := &LifecycleReporter{Reader: c, Client: c, Observer: o, currentDeviceCursor: 7, historyDeviceCursor: 9}
	var pods corev1.PodList
	for _, current := range []bool{true, false} {
		if err := r.syncDevices(context.Background(), &pods, current); err != nil {
			t.Fatal(err)
		}
	}
	if r.currentDeviceCursor != 7 || r.historyDeviceCursor != 9 {
		t.Fatal("empty pass unexpectedly advanced cursor")
	}
}
