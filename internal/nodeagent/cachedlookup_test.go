package nodeagent

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

// countingReader counts the calls that reach the wrapped reader.
type countingReader struct {
	client.Reader
	lists int
}

func (c *countingReader) List(ctx context.Context, l client.ObjectList, opts ...client.ListOption) error {
	c.lists++
	return c.Reader.List(ctx, l, opts...)
}

func lookupFake(t *testing.T, objs ...client.Object) *countingReader {
	t.Helper()
	s := runtime.NewScheme()
	if err := laboratoryv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return &countingReader{Reader: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()}
}

func connection(ns, name string) *laboratoryv1alpha1.Connection {
	return &laboratoryv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
}

// The uniqueness search over all Connections is answered by the cache; the API server is asked only when the cache would
// refuse (a Connection the cache has not seen yet).
func TestConnectionNameUniqueAsksTheAPIServerOnlyForARefusal(t *testing.T) {
	ctx := context.Background()
	cached, direct := lookupFake(t, connection("a", "c1"), connection("b", "c2")), lookupFake(t, connection("a", "c1"), connection("b", "c2"), connection("c", "late"))
	for i := 0; i < 50; i++ {
		ok, err := connectionNameUnique(ctx, cached, direct, "c1")
		if err != nil || !ok {
			t.Fatalf("%v %v", ok, err)
		}
	}
	if direct.lists != 0 || cached.lists != 50 {
		t.Fatalf("API server lists = %d (want 0), cache lists = %d", direct.lists, cached.lists)
	}
	ok, err := connectionNameUnique(ctx, cached, direct, "late")
	if err != nil || !ok || direct.lists != 1 {
		t.Fatalf("a Connection the cache lacks is found directly: %v %v lists=%d", ok, err, direct.lists)
	}
	// Two namespaces with the same name: ambiguous in both reads.
	dup := lookupFake(t, connection("a", "x"), connection("b", "x"))
	if ok, err := connectionNameUnique(ctx, dup, dup, "x"); err != nil || ok {
		t.Fatalf("ambiguous name accepted: %v %v", ok, err)
	}
}

func group(name, ns string) *laboratoryv1alpha1.LabGroup {
	return &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: laboratoryv1alpha1.LabGroupStatus{Namespace: ns}}
}

// The groups of a pod's namespace come from the cache, the API server is read again only when the cache's groups do not
// accept the pod.
func TestGroupsOfNamespaceAcceptsFromTheCache(t *testing.T) {
	ctx := context.Background()
	cached := lookupFake(t, group("g1", "ns1"), group("g2", "ns2"))
	direct := lookupFake(t, group("g1", "ns1"), group("g2", "ns2"), group("g3", "ns3"))
	accept := func(want string) func([]laboratoryv1alpha1.LabGroup) (bool, error) {
		return func(gs []laboratoryv1alpha1.LabGroup) (bool, error) {
			for _, g := range gs {
				if g.Name == want {
					return true, nil
				}
			}
			return false, nil
		}
	}
	for i := 0; i < 50; i++ {
		if ok, err := groupsOfNamespace(ctx, cached, direct, "ns1", accept("g1")); err != nil || !ok {
			t.Fatalf("%v %v", ok, err)
		}
	}
	if direct.lists != 0 {
		t.Fatalf("API server lists = %d, want 0", direct.lists)
	}
	if ok, err := groupsOfNamespace(ctx, cached, direct, "ns3", accept("g3")); err != nil || !ok || direct.lists != 1 {
		t.Fatalf("a group the cache lacks is found directly: %v %v lists=%d", ok, err, direct.lists)
	}
	if ok, _ := groupsOfNamespace(ctx, cached, direct, "ns1", accept("nope")); ok {
		t.Fatal("accepted a group that does not exist")
	}
}
