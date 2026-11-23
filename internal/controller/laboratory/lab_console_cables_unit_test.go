package laboratory

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/switches"
)

func cableClient(t *testing.T, listErr error) (client.Client, *int) {
	t.Helper()
	calls := 0
	c := fake.NewClientBuilder().WithScheme(pruneScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*laboratoryv1alpha1.ConsoleCableList); ok {
				calls++
				if listErr != nil {
					return listErr
				}
			}
			return cl.List(ctx, list, opts...)
		},
	}).Build()
	return c, &calls
}

// An upgrade that did not apply the new CRDs, or a laboratory with the switch off, must not break every Lab: a lab without cables makes
// no ConsoleCable call while the switch is off, and a missing ConsoleCable kind is no cable at all.
func TestConsoleCablesOfALabWithoutCables(t *testing.T) {
	lab := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns"}}
	off := switches.Everything()
	off.ConsoleCables = false
	c, calls := cableClient(t, errors.New("the ConsoleCable kind must not be asked for"))
	r := &LabReconciler{Client: c, Scheme: c.Scheme(), Switches: &off}
	if err := r.syncConsoleCables(context.Background(), lab); err != nil || *calls != 0 {
		t.Fatalf("switch off: err %v, %d calls", err, *calls)
	}
	if refs, err := r.consoleCableRefs(context.Background(), lab); err != nil || refs != nil || *calls != 0 {
		t.Fatalf("switch off refs: %v %v %d", refs, err, *calls)
	}

	noKind := &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "laboratory.cybericebox.com", Kind: "ConsoleCable"}}
	c, calls = cableClient(t, noKind)
	r = &LabReconciler{Client: c, Scheme: c.Scheme()}
	if err := r.syncConsoleCables(context.Background(), lab); err != nil {
		t.Fatalf("switch on, CRD missing: %v (%d calls)", err, *calls)
	}
	lab.Status.ConsoleCables = []laboratoryv1alpha1.ConsoleCableRef{{Name: "old"}}
	if refs, err := r.consoleCableRefs(context.Background(), lab); err != nil || refs != nil {
		t.Fatalf("CRD missing refs: %v %v", refs, err)
	}
}

func TestConsoleCableWatchFollowsTheSwitch(t *testing.T) {
	off := switches.Everything()
	off.ConsoleCables = false
	if (&LabReconciler{Switches: &off}).consoleCablesOn() || !(&LabReconciler{}).consoleCablesOn() {
		t.Fatal("the switch decides the ConsoleCable watch (nil switches: everything on)")
	}
}

// The operator stamps each cable with the node of its console end (the admission policy of the node-agent reads it), and moves the stamp
// when the device moves.
func TestConsoleCablesCarryTheConsoleNode(t *testing.T) {
	ctx := context.Background()
	lab := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns", UID: "lab-uid"}, Spec: laboratoryv1alpha1.LabSpec{
		ConsoleCables: []laboratoryv1alpha1.ConsoleCableTemplate{{Serial: laboratoryv1alpha1.PortRef{Device: "pc1", Port: "serial0"},
			Console: laboratoryv1alpha1.PortRef{Device: "r1", Port: "console0"}}}}}
	r1 := &laboratoryv1alpha1.Device{ObjectMeta: metav1.ObjectMeta{Name: "l-r1", Namespace: "ns", Labels: map[string]string{names.LabelLab: "l"}},
		Spec: laboratoryv1alpha1.DeviceSpec{LabRef: "l", Name: "r1"}, Status: laboratoryv1alpha1.DeviceStatus{NodeName: "n1"}}
	c, _ := cableClient(t, nil)
	if err := c.Create(ctx, r1); err != nil {
		t.Fatal(err)
	}
	r := &LabReconciler{Client: c, Scheme: c.Scheme()}
	if err := r.materializeConsoleCables(ctx, lab); err != nil {
		t.Fatal(err)
	}
	name := consoleCableName("l", lab.Spec.ConsoleCables[0])
	var cc laboratoryv1alpha1.ConsoleCable
	if err := c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: name}, &cc); err != nil || cc.Annotations[names.AnnotationConsoleNode] != "n1" {
		t.Fatalf("%v %v", err, cc.Annotations)
	}
	r1.Status.NodeName = "n2"
	if err := c.Update(ctx, r1); err != nil {
		t.Fatal(err)
	}
	if err := r.materializeConsoleCables(ctx, lab); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: name}, &cc); err != nil || cc.Annotations[names.AnnotationConsoleNode] != "n2" {
		t.Fatalf("the stamp did not follow the device: %v %v", err, cc.Annotations)
	}
}
