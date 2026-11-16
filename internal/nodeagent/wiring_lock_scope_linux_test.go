//go:build linux

package nodeagent

import (
	"context"
	"errors"
	"testing"
	"time"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nstest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// blockingReader is an API reader whose read of a Lab does not return until released: a slow API server.
type blockingReader struct {
	client.Reader
	entered chan struct{}
	release chan struct{}
}

func (b *blockingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*lab.Lab); ok {
		select {
		case b.entered <- struct{}{}:
		default:
		}
		select {
		case <-b.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return b.Reader.Get(ctx, key, obj, opts...)
}

func wiringScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = lab.AddToScheme(s)
	return s
}

// The reconcile of a pod reads the API before it plans anything. Those reads must not hold the node's wiring lock: while one is
// stuck, a CNI ADD (which takes the same lock) goes on.
func TestReconcileDoesNotHoldTheWiringLockDuringAPIReads(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "g", UID: "uid", Labels: map[string]string{names.LabelLab: "a", names.LabelDevice: "web"}},
		Spec:       corev1.PodSpec{NodeName: "node"},
	}
	kube := fake.NewClientBuilder().WithScheme(wiringScheme()).WithObjects(pod).Build()
	slow := &blockingReader{Reader: kube, entered: make(chan struct{}, 1), release: make(chan struct{})}
	r := &NetworkAttachReconciler{Client: kube, Reader: slow, NodeName: "node", OVS: &OVSManager{}}

	done := make(chan error, 1)
	go func() {
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pod)})
		done <- err
	}()
	select {
	case <-slow.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the reconcile never reached its API read")
	}

	// What SetupNetworks does first: ask for the lock, bounded by the CNI call.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.OVS.vethMu.LockCtx(ctx); err != nil {
		t.Fatalf("the wiring lock is held while the reconcile waits for the API: %v", err)
	}
	r.OVS.vethMu.Unlock()

	close(slow.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reconcile did not finish after its read returned")
	}
}

// The check made under the lock: the same incarnation, not terminating. A pod replaced or going away is a plan that no longer holds.
func TestPodStillCurrent(t *testing.T) {
	now := metav1.Now()
	planned := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "g", UID: "old"}}
	cases := []struct {
		name string
		live *corev1.Pod
		ok   bool
	}{
		{"same incarnation", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "g", UID: "old"}}, true},
		{"replaced", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "g", UID: "new"}}, false},
		{"terminating", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "g", UID: "old", DeletionTimestamp: &now, Finalizers: []string{"x"}}}, false},
		{"gone", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(wiringScheme())
			if c.live != nil {
				b = b.WithObjects(c.live)
			}
			err := podStillCurrent(context.Background(), b.Build(), planned)
			if c.ok && err != nil {
				t.Fatalf("plan holds, got %v", err)
			}
			if !c.ok && !errors.Is(err, ErrPortOwnerChanged) {
				t.Fatalf("plan no longer holds, want ErrPortOwnerChanged, got %v", err)
			}
		})
	}
}

// A pod that was replaced between the plan and the lock wires nothing: the pass fails with ErrPortOwnerChanged (and is retried),
// and no port of the old incarnation appears.
func TestNetnsWireLegsRequeuesWhenThePodWasReplacedBeforeTheLock(t *testing.T) {
	nstest.Require(t)
	ovs := testOVSDB(t)
	planned := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "g", UID: "old-uid", Labels: map[string]string{names.LabelComponent: names.ComponentGateway}}}
	replacement := planned.DeepCopy()
	replacement.UID = "new-uid"
	kube := fake.NewClientBuilder().WithScheme(wiringScheme()).WithObjects(replacement).Build()
	key := names.GWHostPortKey("g", 1)
	t.Cleanup(func() { _ = ovs.DelVethPort(key) })

	r := &NetworkAttachReconciler{Client: kube, Reader: kube, NodeName: "node", OVS: ovs}
	_, err := r.wireLegs(context.Background(), planned, names.ComponentGateway, []NetAttachment{{Name: key}}, "/nonexistent/netns")
	if !errors.Is(err, ErrPortOwnerChanged) {
		t.Fatalf("a replaced pod must fail the pass so it is retried: %v", err)
	}
	owners, err := ovs.PortOwners()
	if err != nil {
		t.Fatal(err)
	}
	if _, present := owners[key]; present {
		t.Fatalf("a replaced incarnation must not create the port: %v", owners)
	}
}

// The check of the port row under the lock: free or this incarnation's passes, another incarnation's does not.
func TestNetnsRequireFreeOrOwned(t *testing.T) {
	nstest.Require(t)
	ovs := testOVSDB(t)
	key := names.GWHostPortKey("g", 2)
	t.Cleanup(func() { _ = ovs.DelVethPort(key) })
	if err := ovs.requireFreeOrOwned(key, "a"); err != nil {
		t.Fatalf("a free key is claimable: %v", err)
	}
	if err := ovs.AddVethPortOwned(key, "a"); err != nil {
		t.Fatal(err)
	}
	if err := ovs.requireFreeOrOwned(key, "a"); err != nil {
		t.Fatalf("the owner may go on: %v", err)
	}
	if err := ovs.requireFreeOrOwned(key, "b"); !errors.Is(err, ErrPortOwnerChanged) {
		t.Fatalf("another incarnation must not wire a held key: %v", err)
	}
}
