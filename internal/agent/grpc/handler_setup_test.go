package grpc

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	versioned "github.com/cybericebox/laboratory/clientset/client/versioned"
)

// One API server (envtest: kube-apiserver and etcd with the laboratory CRDs) serves the whole package. Starting one takes seconds and
// every test used to start its own (more than a hundred); the package then took more than ten minutes on a CI runner. A test gets a
// clean cluster instead: resetCluster, registered by newTestHandler, deletes everything the test left (see its comment).
var shared struct {
	once sync.Once
	env  *envtest.Environment
	cfg  *rest.Config
	err  error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if shared.env != nil {
		_ = shared.env.Stop()
	}
	os.Exit(code)
}

func sharedConfig(t *testing.T) *rest.Config {
	t.Helper()
	shared.once.Do(func() {
		env := &envtest.Environment{
			CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
			ErrorIfCRDPathMissing: true,
		}
		if dir := firstEnvTestBinaryDir(); dir != "" {
			env.BinaryAssetsDirectory = dir
		}
		shared.cfg, shared.err = env.Start()
		if shared.err != nil {
			return
		}
		shared.env = env
		cs, err := NewVersionedClientset(shared.cfg)
		if err != nil {
			shared.err = err
			return
		}
		waitForCRDsServed(t, cs)
	})
	if shared.err != nil {
		t.Fatalf("start envtest: %v", shared.err)
	}
	return shared.cfg
}

// newTestHandler returns a Handler backed by a real typed clientset talking to the shared envtest API server with the laboratory
// CRDs, along with a plain kubernetes clientset for setting up core resources (e.g. namespaces) that tests need but the Handler itself
// doesn't manage. The cluster is emptied when the test ends. This package uses plain go test (not Ginkgo).
func newTestHandler(t *testing.T) (*Handler, kubernetes.Interface) {
	t.Helper()

	cfg := sharedConfig(t)
	// Every call gets an empty cluster (a few tests ask for several handlers, each of which used to have an API server of its own), and
	// the test leaves one behind.
	resetCluster(t, cfg)
	t.Cleanup(func() { resetCluster(t, cfg) })

	// The generated typed clientset was produced with client-gen's
	// --prefers-protobuf flag (see hack/update-codegen.sh), which is meant
	// for built-in API types. Our CRDs don't implement the protobuf
	// marshalling interface and the API server does not serve CRDs over
	// protobuf, so force JSON content-type for this clientset specifically
	// (leaving cfg itself untouched for the built-in kubernetes clientset).
	cs, err := NewVersionedClientset(cfg)
	if err != nil {
		t.Fatalf("build versioned clientset: %v", err)
	}

	// Namespaces (and other core resources) are built-in API types and are
	// served over protobuf, so the plain cfg is fine here.
	k8s, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("build kubernetes clientset: %v", err)
	}

	// nil metrics client: envtest has no metrics-server, so live usage
	// degrades to zero. namespaceUsage tests inject a fake metrics client.
	h := NewHandler(cs, k8s, nil)
	// The tests of other behaviour create groups and labs without reservations (reservation_fit_test.go turns it back on).
	h.reservationsOptional = true
	return h, k8s
}

// mustNamespace creates a Namespace via the plain kubernetes clientset,
// failing the test on error. Used to set up namespaces that namespace-scoped
// custom resources (e.g. Lab) are created in.
func mustNamespace(t *testing.T, k8s kubernetes.Interface, name string) {
	t.Helper()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if _, err := k8s.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace %q: %v", name, err)
	}
}

// firstEnvTestBinaryDir locates the first binary dir under bin/k8s, mirroring
// getFirstFoundEnvTestBinaryDir in the controller suite so tests also work
// from IDEs without KUBEBUILDER_ASSETS set.
func firstEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}

// waitForCRDsServed blocks until the API server serves every CRD the agent lists.
// envtest.Start returns once the CRDs are created, but the first request for a
// freshly registered kind is refused with a one second Retry-After, which the
// client obeys. Tests that time a first call against a short deadline were flaky
// because that call paid the second; paying it here makes them deterministic.
func waitForCRDsServed(t *testing.T, cs versioned.Interface) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lab := cs.LaboratoryV1alpha1()
	lists := map[string]func() error{
		"LabGroups": func() error { _, err := lab.LabGroups().List(ctx, metav1.ListOptions{}); return err },
		"Labs":      func() error { _, err := lab.Labs(metav1.NamespaceDefault).List(ctx, metav1.ListOptions{}); return err },
		"LabGroupClients": func() error {
			_, err := lab.LabGroupClients(metav1.NamespaceDefault).List(ctx, metav1.ListOptions{})
			return err
		},
		"LabGroupAccessPolicies": func() error {
			_, err := lab.LabGroupAccessPolicies(metav1.NamespaceDefault).List(ctx, metav1.ListOptions{})
			return err
		},
		"LabTrafficReports": func() error {
			_, err := lab.LabTrafficReports(metav1.NamespaceDefault).List(ctx, metav1.ListOptions{})
			return err
		},
	}
	for name, list := range lists {
		if err := list(); err != nil {
			t.Fatalf("wait for %s to be served: %v", name, err)
		}
	}
}
