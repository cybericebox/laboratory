package nodeagent

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

// A port whose Connection is being retired is left to that Connection's finalizer: when the Pod path deleted it first,
// the finalizer found no owner row and could never prove the cleanup, so the Lab and its group stayed forever.
func TestConnectionRetiresPort(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := laboratoryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	const ns, node, key, uid = "ns", "node-a", "g0368a4467443", "pod-uid"
	lab := func(name string, deleting, stopped bool) *laboratoryv1alpha1.Lab {
		l := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		if deleting {
			l.DeletionTimestamp = &now
			l.Finalizers = []string{names.FinalizerLab}
		}
		if stopped {
			l.Spec.Lifecycle = &laboratoryv1alpha1.LabLifecycleSpec{DesiredState: "Stopped"}
		}
		return l
	}
	conn := func(labName string, deleting bool, finalizer bool, port laboratoryv1alpha1.ConnectionPortStatus) *laboratoryv1alpha1.Connection {
		c := &laboratoryv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: "c-" + labName, Namespace: ns}}
		c.Spec.LabRef = labName
		if finalizer {
			c.Finalizers = []string{names.FinalizerOVSCleanup}
		}
		if deleting {
			c.DeletionTimestamp = &now
		}
		c.Status.Ports = []laboratoryv1alpha1.ConnectionPortStatus{port}
		return c
	}
	mine := laboratoryv1alpha1.ConnectionPortStatus{PortID: key, NodeName: node, PodUID: uid}
	for name, tc := range map[string]struct {
		objs []client.Object
		want bool
	}{
		"lab deleting":          {[]client.Object{lab("l", true, false), conn("l", false, true, mine)}, true},
		"connection deleting":   {[]client.Object{lab("l", false, false), conn("l", true, true, mine)}, true},
		"lab stopped":           {[]client.Object{lab("l", false, true), conn("l", false, true, mine)}, true},
		"lab gone":              {[]client.Object{conn("l", true, true, mine)}, true},
		"lab running":           {[]client.Object{lab("l", false, false), conn("l", false, true, mine)}, false},
		"no finalizer":          {[]client.Object{lab("l", true, false), conn("l", false, false, mine)}, false},
		"other node":            {[]client.Object{lab("l", true, false), conn("l", false, true, laboratoryv1alpha1.ConnectionPortStatus{PortID: key, NodeName: "node-b", PodUID: uid})}, false},
		"other pod incarnation": {[]client.Object{lab("l", true, false), conn("l", false, true, laboratoryv1alpha1.ConnectionPortStatus{PortID: key, NodeName: node, PodUID: "old"})}, false},
		"other port":            {[]client.Object{lab("l", true, false), conn("l", false, true, laboratoryv1alpha1.ConnectionPortStatus{PortID: "other", NodeName: node, PodUID: uid})}, false},
		"no connection":         {nil, false},
	} {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objs...).Build()
		got, err := connectionRetiresPort(context.Background(), c, ns, node, key, uid)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}
