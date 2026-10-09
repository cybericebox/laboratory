package laboratory

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

type serviceSnapshotClient struct {
	client.Client
	serviceLists   int
	serviceCreates int
	failList       bool
	failPolicy     bool
	collideCreate  bool
}

func (c *serviceSnapshotClient) List(ctx context.Context, out client.ObjectList, opts ...client.ListOption) error {
	if _, ok := out.(*corev1.ServiceList); ok {
		c.serviceLists++
		if c.failList {
			c.failList = false
			return errors.New("service list unavailable")
		}
	}
	return c.Client.List(ctx, out, opts...)
}
func (c *serviceSnapshotClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*corev1.Service); ok {
		c.serviceCreates++
		if c.collideCreate {
			c.collideCreate = false
			foreign := obj.(*corev1.Service).DeepCopy()
			foreign.OwnerReferences = nil
			foreign.Labels = map[string]string{names.LabelLab: "foreign"}
			if err := c.Client.Create(ctx, foreign); err != nil {
				return err
			}
		}
	}
	if _, ok := obj.(*networkingv1.NetworkPolicy); ok && c.failPolicy {
		c.failPolicy = false
		return errors.New("policy create unavailable")
	}
	return c.Client.Create(ctx, obj, opts...)
}
func snapshotReconcileFixture(t *testing.T, n int) (*LabReconciler, *api.Lab, *serviceSnapshotClient) {
	t.Helper()
	s := pruneScheme(t)
	_ = corev1.AddToScheme(s)
	_ = networkingv1.AddToScheme(s)
	lab := webLab("snapshot", "uid-snapshot")
	lab.Spec.Devices = nil
	for i := 0; i < n; i++ {
		lab.Spec.Devices = append(lab.Spec.Devices, api.DeviceTemplate{Name: fmt.Sprintf("web%d", i), Type: api.DeviceTypeContainer, Image: "example/image:stable", Exposure: &api.ExposureSpec{Web: &api.WebExposure{Port: 80}}})
	}
	c := &serviceSnapshotClient{Client: fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&api.Lab{}, &api.Device{}).WithObjects(lab).Build()}
	return &LabReconciler{Client: c, Scheme: s, Recorder: record.NewFakeRecorder(100), BaseDomain: "labs.example.com"}, lab, c
}
func snapshotReconcile(t *testing.T, r *LabReconciler, lab *api.Lab) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lab)})
	return err
}

func TestLabReconcileServicesUseOneWarmSnapshot(t *testing.T) {
	r, lab, c := snapshotReconcileFixture(t, 4)
	if err := snapshotReconcile(t, r, lab); err != nil {
		t.Fatal(err)
	}
	c.serviceLists = 0
	c.serviceCreates = 0
	if err := snapshotReconcile(t, r, lab); err != nil {
		t.Fatal(err)
	}
	if c.serviceLists != 1 {
		t.Fatalf("warm reconcile lists Services %d times, want once", c.serviceLists)
	}
	if c.serviceCreates != 0 {
		t.Fatalf("warm reconcile recreates %d Services", c.serviceCreates)
	}
	var got api.Lab
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(lab), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Access) != 4 {
		t.Fatalf("access entries: %+v", got.Status.Access)
	}
}
func TestLabReconcileCreatedServicesAppearBeforeCacheRefresh(t *testing.T) {
	r, lab, c := snapshotReconcileFixture(t, 2)
	// A configured Reader may lag created Services. The reconcile-local view must
	// retain successful server answers while code uniqueness still reads live state.
	r.Reader = &serviceLagReader{Reader: c}
	if err := snapshotReconcile(t, r, lab); err != nil {
		t.Fatal(err)
	}
	var got api.Lab
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(lab), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Access) != 2 {
		t.Fatalf("new Services lost before cache refresh: %+v", got.Status.Access)
	}
}

type serviceLagReader struct{ client.Reader }

func (r *serviceLagReader) List(ctx context.Context, out client.ObjectList, opts ...client.ListOption) error {
	if list, ok := out.(*corev1.ServiceList); ok {
		options := client.ListOptions{}
		for _, option := range opts {
			option.ApplyToList(&options)
		}
		if options.LabelSelector != nil && !options.LabelSelector.Empty() {
			list.Items = nil
			return nil
		}
	}
	return r.Reader.List(ctx, out, opts...)
}
func TestLabReconcileServiceErrorRetryLoadsFreshSnapshot(t *testing.T) {
	for _, failure := range []string{"list", "policy"} {
		t.Run(failure, func(t *testing.T) {
			r, lab, c := snapshotReconcileFixture(t, 2)
			c.failList = failure == "list"
			c.failPolicy = failure == "policy"
			if err := snapshotReconcile(t, r, lab); err == nil {
				t.Fatal("expected injected dependency error")
			}
			if err := snapshotReconcile(t, r, lab); err != nil {
				t.Fatal(err)
			}
			var services corev1.ServiceList
			if err := c.Client.List(context.Background(), &services, client.InNamespace(lab.Namespace)); err != nil {
				t.Fatal(err)
			}
			if len(services.Items) != 2 {
				t.Fatalf("retry duplicated/lost Services: %d", len(services.Items))
			}
			for _, svc := range services.Items {
				if !ownedByLab(&svc, lab) {
					t.Fatal("retry adopted foreign Service")
				}
			}
		})
	}
}
func TestLabReconcileSnapshotDoesNotCrossLabOrUID(t *testing.T) {
	r, lab, c := snapshotReconcileFixture(t, 1)
	if err := snapshotReconcile(t, r, lab); err != nil {
		t.Fatal(err)
	}
	second := lab.DeepCopy()
	second.Name = "second"
	second.UID = types.UID("uid-second")
	second.ResourceVersion = ""
	second.Finalizers = nil
	second.Status = api.LabStatus{}
	if err := c.Create(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if err := snapshotReconcile(t, r, second); err != nil {
		t.Fatal(err)
	}
	var first, other api.Lab
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(lab), &first)
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(second), &other)
	if len(first.Status.Access) != 1 || len(other.Status.Access) != 1 || first.Status.Access[0].URL == other.Status.Access[0].URL {
		t.Fatalf("labs shared host: first=%v other=%v", first.Status.Access, other.Status.Access)
	}
}
func TestLabReconcileSnapshotKeepsExposureUpdate(t *testing.T) {
	r, lab, c := snapshotReconcileFixture(t, 1)
	if err := snapshotReconcile(t, r, lab); err != nil {
		t.Fatal(err)
	}
	var current api.Lab
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(lab), &current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Devices[0].Exposure.Web.Port = 8080
	current.Spec.Devices[0].Exposure.Web.Protocol = "https"
	if err := c.Update(context.Background(), &current); err != nil {
		t.Fatal(err)
	}
	if err := snapshotReconcile(t, r, &current); err != nil {
		t.Fatal(err)
	}
	var services corev1.ServiceList
	_ = c.Client.List(context.Background(), &services, client.MatchingLabels{names.LabelLab: lab.Name})
	if len(services.Items) != 1 || services.Items[0].Spec.Ports[0].Port != 8080 || services.Items[0].Spec.Ports[0].Name != "https" {
		t.Fatalf("exposure did not converge: %+v", services.Items)
	}
}

func TestLabReconcileServiceCreateCollisionRetriesWithoutAdoption(t *testing.T) {
	r, lab, c := snapshotReconcileFixture(t, 1)
	draw, _ := scriptedCodes("aaa", "bbb")
	r.newWebCode = draw
	c.collideCreate = true
	if err := snapshotReconcile(t, r, lab); err == nil {
		t.Fatal("expected authoritative AlreadyExists")
	}
	if err := snapshotReconcile(t, r, lab); err != nil {
		t.Fatal(err)
	}
	var foreign corev1.Service
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: lab.Namespace, Name: "web0-aaa"}, &foreign); err != nil {
		t.Fatal(err)
	}
	if foreign.Labels[names.LabelLab] != "foreign" || len(foreign.OwnerReferences) != 0 {
		t.Fatal("collision adopted another Service")
	}
	var current api.Lab
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(lab), &current)
	if len(current.Status.Access) != 1 || current.Status.Access[0].URL != "https://web0-bbb.labs.example.com" {
		t.Fatalf("retry host: %+v", current.Status.Access)
	}
}

func TestLabReconcileCodeExhaustionKeepsForeignServices(t *testing.T) {
	r, lab, c := snapshotReconcileFixture(t, 1)
	for _, name := range []string{"web0-aaa", "web0-aaaa"} {
		if err := c.Create(context.Background(), &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: lab.Namespace}}); err != nil {
			t.Fatal(err)
		}
	}
	r.newWebCode = func(n int) (string, error) { return "aaaa"[:n], nil }
	if err := snapshotReconcile(t, r, lab); err == nil {
		t.Fatal("expected code exhaustion")
	}
	var services corev1.ServiceList
	_ = c.Client.List(context.Background(), &services, client.InNamespace(lab.Namespace))
	if len(services.Items) != 2 {
		t.Fatalf("exhaustion changed Services: %d", len(services.Items))
	}
	for _, svc := range services.Items {
		if len(svc.OwnerReferences) != 0 {
			t.Fatal("foreign service adopted")
		}
	}
}

func TestLabSnapshotSelectionKeepsOwnerUIDOldestTieAndDeletion(t *testing.T) {
	r, lab, c := snapshotReconcileFixture(t, 1)
	start := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	makeService := func(name, uid, kind string, age time.Duration) *corev1.Service {
		return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: lab.Namespace, CreationTimestamp: metav1.NewTime(start.Add(age)), Labels: map[string]string{names.LabelLab: lab.Name, names.LabelDevice: "web0"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: api.SchemeGroupVersion.String(), Kind: kind, Name: lab.Name, UID: types.UID(uid)}}}}
	}
	objects := []*corev1.Service{makeService("web0-bbb", string(lab.UID), "Lab", 0), makeService("web0-aaa", string(lab.UID), "Lab", 0), makeService("web0-zzz", "wrong-uid", "Lab", -time.Hour), makeService("web0-yyy", string(lab.UID), "Device", -time.Hour)}
	objects[1].DeletionTimestamp = &metav1.Time{Time: start}
	objects[1].Finalizers = []string{"hold-for-test"}
	for _, svc := range objects {
		if err := c.Create(context.Background(), svc); err != nil {
			t.Fatal(err)
		}
	}
	if err := snapshotReconcile(t, r, lab); err != nil {
		t.Fatal(err)
	}
	var current api.Lab
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(lab), &current)
	if len(current.Status.Access) != 1 || current.Status.Access[0].URL != "https://web0-aaa.labs.example.com" {
		t.Fatalf("selection changed: %v", current.Status.Access)
	}
	recreated := lab.DeepCopy()
	recreated.UID = "new-owner"
	selected, err := r.serviceSnapshot(recreated, r.serviceSnapshot(lab)).find(context.Background(), "web0")
	if err != nil || selected != nil {
		t.Fatalf("snapshot crossed Lab UID: %v %v", selected, err)
	}
}
