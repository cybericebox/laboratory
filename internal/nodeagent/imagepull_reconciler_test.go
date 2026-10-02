package nodeagent

import (
	"context"
	"errors"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/imagepull"
)

type recordingPuller struct {
	mu    sync.Mutex
	pulls map[string]*imagepull.Auth
	fail  map[string]error
}

func (p *recordingPuller) Present(context.Context, string) (bool, error) { return false, nil }
func (p *recordingPuller) Pull(_ context.Context, ref string, a *imagepull.Auth) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pulls == nil {
		p.pulls = map[string]*imagepull.Auth{}
	}
	p.pulls[ref] = a
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
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(append(objs, ip)...).
		WithStatusSubresource(&laboratoryv1alpha1.ImagePull{}).Build()
	p := &recordingPuller{}
	return &ImagePullReconciler{Client: c, Reader: c, NodeName: node, ImagesNamespace: "laboratory-images", Puller: p}, p, c
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

func TestImagePullReconcilerUsesTheRequestsCredentialsOnly(t *testing.T) {
	ip := request("n1")
	ip.Spec.PullSecret = "prepull-x"
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "prepull-x", Namespace: "laboratory-images"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"ghcr.io":{"username":"tenant","password":"tpass"}}}`)},
	}
	// A platform secret in another namespace must never be read.
	platform := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "prepull-x", Namespace: "laboratory-system"},
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"ghcr.io":{"username":"platform","password":"ppass"}}}`)},
	}
	r, p, _ := pullFixture(t, "n1", ip, creds, platform)
	runReconcile(t, r)
	if a := p.pulls["ghcr.io/o/a:1"]; a == nil || a.Username != "tenant" {
		t.Fatalf("auth: %+v", a)
	}

	// Without the Secret the pull is anonymous.
	r2, p2, _ := pullFixture(t, "n1", ip)
	runReconcile(t, r2)
	if a, ok := p2.pulls["ghcr.io/o/a:1"]; !ok || a != nil {
		t.Fatalf("anonymous expected: %+v", a)
	}
}
