package nodeagent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

type recordingPuller struct {
	mu    sync.Mutex
	pulls map[string]bool
	fail  map[string]error
}

func (p *recordingPuller) Present(context.Context, string) (bool, error) { return false, nil }
func (p *recordingPuller) Pull(_ context.Context, ref string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pulls == nil {
		p.pulls = map[string]bool{}
	}
	p.pulls[ref] = true
	return p.fail[ref]
}

func pullFixture(t *testing.T, node string, ip *laboratoryv1alpha1.ImagePull, objs ...client.Object) (*ImagePullReconciler, *recordingPuller, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	if err := laboratoryv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	// The node-agent reads no Secret: any access to one fails the test.
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(append(objs, ip)...).
		WithStatusSubresource(&laboratoryv1alpha1.ImagePull{}).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, secret := obj.(*corev1.Secret); secret {
				t.Errorf("the node-agent must not read a Secret: %v", key)
			}
			return cl.Get(ctx, key, obj, opts...)
		}}).Build()
	p := &recordingPuller{}
	return &ImagePullReconciler{Client: c, NodeName: node, Puller: p}, p, c
}

func request(nodes ...string) *laboratoryv1alpha1.ImagePull {
	return &laboratoryv1alpha1.ImagePull{
		ObjectMeta: metav1.ObjectMeta{Name: "prepull-x"},
		Spec:       laboratoryv1alpha1.ImagePullSpec{Images: []string{"ghcr.io/o/a:1", "ghcr.io/o/b:1"}, Nodes: nodes},
	}
}

func runReconcile(t *testing.T, r *ImagePullReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "prepull-x"}}); err != nil {
		t.Fatal(err)
	}
}

func status(t *testing.T, c client.Client) map[string]laboratoryv1alpha1.NodeImagePull {
	t.Helper()
	var got laboratoryv1alpha1.ImagePull
	if err := c.Get(context.Background(), types.NamespacedName{Name: "prepull-x"}, &got); err != nil {
		t.Fatal(err)
	}
	return got.Status.Nodes
}

func TestImagePullReconcilerPullsForItsNodeAndReports(t *testing.T) {
	r, p, c := pullFixture(t, "n1", request("n1", "n2"))
	p.fail = map[string]error{"ghcr.io/o/b:1": errors.New("manifest unknown")}
	runReconcile(t, r)
	n := status(t, c)["n1"]
	if !n.Done || len(n.Pulled) != 1 || n.Pulled[0] != "ghcr.io/o/a:1" || len(n.Failed) != 1 || n.Failed[0].Image != "ghcr.io/o/b:1" || n.Failed[0].Message != "manifest unknown" {
		t.Fatalf("n1: %+v", n)
	}
	if _, other := status(t, c)["n2"]; other {
		t.Fatal("a node-agent writes only its own entry")
	}

	// Done is final: a second reconcile pulls nothing.
	p.pulls = nil
	runReconcile(t, r)
	if len(p.pulls) != 0 {
		t.Fatalf("pulled again: %v", p.pulls)
	}
}

func TestImagePullReconcilerIgnoresRequestsForOtherNodes(t *testing.T) {
	r, p, c := pullFixture(t, "n3", request("n1", "n2"))
	runReconcile(t, r)
	if len(p.pulls) != 0 || len(status(t, c)) != 0 {
		t.Fatalf("not asked: pulls %v status %v", p.pulls, status(t, c))
	}
}

// The references of the request already carry the tenant; they are pulled as they are, and the request names no Secret.
func TestImagePullReconcilerPullsTheReferencesAsTheyAre(t *testing.T) {
	ip := request("n1")
	ip.Spec.Tenant = "acme"
	ip.Spec.Images = []string{"localhost:5035/c/acme/ghcr.io/o/a@sha256:" + strings.Repeat("a", 64)}
	r, p, c := pullFixture(t, "n1", ip)
	runReconcile(t, r)
	if !p.pulls[ip.Spec.Images[0]] || len(p.pulls) != 1 {
		t.Fatalf("%v", p.pulls)
	}
	if n := status(t, c)["n1"]; !n.Done || len(n.Failed) != 0 {
		t.Fatalf("%+v", n)
	}
}
