package laboratory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	allocation "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	api "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

type groupStatusCounter struct {
	client.Client
	updates    int
	failUpdate bool
}

func (c *groupStatusCounter) Status() client.SubResourceWriter {
	return &groupStatusWriter{SubResourceWriter: c.Client.Status(), counter: c}
}

type groupStatusWriter struct {
	client.SubResourceWriter
	counter *groupStatusCounter
}

func (w *groupStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if _, ok := obj.(*api.LabGroup); ok {
		w.counter.updates++
		if w.counter.failUpdate {
			w.counter.failUpdate = false
			return errors.New("status update unavailable")
		}
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func groupStatusFixture(t *testing.T) (*LabGroupReconciler, *groupStatusCounter, *api.LabGroup) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = api.AddToScheme(scheme)
	_ = allocation.AddToScheme(scheme)
	group := &api.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "status-transitions"}}
	c := &groupStatusCounter{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(group).WithStatusSubresource(&api.LabGroup{}, &allocation.Pool{}, &appsv1.Deployment{}).Build()}
	r := &LabGroupReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(100), VPNBaseNetwork: "10.128.0.0/10", VPNImage: "lab:v1", GatewayImage: "lab:v1", SupportEmail: "help@example.com"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(group)}); err != nil {
		t.Fatal(err)
	}
	return r, c, group
}

func TestLabGroupStatusGuardKeepsReadinessLossAndFailureRetry(t *testing.T) {
	r, c, group := groupStatusFixture(t)
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(group)}
	ns := api.LabGroupNamespace(group.Name)
	var dep appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: names.ComponentVPN}, &dep); err != nil {
		t.Fatal(err)
	}
	dep.Status.ReadyReplicas = 1
	if err := c.Client.Status().Update(ctx, &dep); err != nil {
		t.Fatal(err)
	}
	c.failUpdate = true
	c.updates = 0
	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatal("changed status write error was hidden")
	}
	if c.updates != 1 {
		t.Fatalf("attempts=%d", c.updates)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	var registered api.LabGroup
	_ = c.Get(ctx, req.NamespacedName, &registered)
	if !registered.Status.VPN.Registered {
		t.Fatal("retry lost VPN registration")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(&dep), &dep); err != nil {
		t.Fatal(err)
	}
	dep.Status.ReadyReplicas = 0
	if err := c.Client.Status().Update(ctx, &dep); err != nil {
		t.Fatal(err)
	}
	c.updates = 0
	result, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	var lost api.LabGroup
	_ = c.Get(ctx, req.NamespacedName, &lost)
	if lost.Status.VPN.Registered || c.updates != 1 || result.RequeueAfter != 5*time.Second {
		t.Fatalf("readiness loss: registered=%t writes=%d retry=%v", lost.Status.VPN.Registered, c.updates, result.RequeueAfter)
	}
	seenWarning := false
	events := r.Recorder.(*record.FakeRecorder).Events
	for len(events) > 0 {
		if strings.Contains(<-events, "WaitingForVPNServer") {
			seenWarning = true
		}
	}
	if !seenWarning {
		t.Fatal("readiness loss warning disappeared")
	}
}

func TestLabGroupStatusGuardKeepsWarningAndSchedulerFields(t *testing.T) {
	r, c, group := groupStatusFixture(t)
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(group)}
	var current api.LabGroup
	if err := c.Get(ctx, req.NamespacedName, &current); err != nil {
		t.Fatal(err)
	}
	current.Status.Scheduling = &api.SchedulingStatus{Position: 7, Length: 9, Pending: 1}
	current.Status.Pods = []api.NamedPodSchedule{{Name: names.ComponentVPN, PodSchedule: api.PodSchedule{State: api.PodStarted}}, {Name: names.ComponentGateway, PodSchedule: api.PodSchedule{State: api.PodStarted}}}
	if err := c.Client.Status().Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	r.Scheduled = true
	r.notePinFailure(api.LabGroupNamespace(group.Name), "new warning")
	c.updates = 0
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	var got api.LabGroup
	_ = c.Get(ctx, req.NamespacedName, &got)
	if c.updates != 1 || !strings.Contains(got.Status.ImageWarning, "new warning") || got.Status.Scheduling.Position != 7 || len(got.Status.Pods) != 2 || got.Status.Pods[0].State != api.PodStarted {
		t.Fatalf("pre-final mutations/fields lost: writes=%d status=%+v", c.updates, got.Status)
	}
	c.updates = 0
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if c.updates != 0 {
		t.Fatal("retained warning causes duplicate status write")
	}
}
func TestLabGroupUnchangedReadyAndSuspendedStatusDoesNotWrite(t *testing.T) {
	for _, suspended := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "suspended"}[suspended], func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)
			_ = api.AddToScheme(scheme)
			_ = allocation.AddToScheme(scheme)
			group := &api.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "status-work"}, Spec: api.LabGroupSpec{Suspended: suspended}}
			group.Spec.VPN.Disabled = true
			c := &groupStatusCounter{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(group).WithStatusSubresource(&api.LabGroup{}, &allocation.Pool{}).Build()}
			r := &LabGroupReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(100), VPNBaseNetwork: "10.128.0.0/10", VPNImage: "lab:v1", GatewayImage: "lab:v1", SupportEmail: "help@example.com"}
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(group)}
			if _, err := r.Reconcile(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			c.updates = 0
			if _, err := r.Reconcile(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if c.updates != 0 {
				t.Fatalf("unchanged group status made %d API writes", c.updates)
			}
		})
	}
}
