package grpc

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// newTestHandler bootstraps an envtest environment with the laboratory CRDs
// and returns a Handler backed by a real typed clientset talking to it,
// along with a plain kubernetes clientset for setting up core resources
// (e.g. namespaces) that tests need but the Handler itself doesn't manage.
// Mirrors the envtest bootstrap in internal/controller/laboratory/suite_test.go,
// but this package uses plain go test (not Ginkgo).
func newTestHandler(t *testing.T) (*Handler, kubernetes.Interface) {
	t.Helper()

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	if dir := firstEnvTestBinaryDir(); dir != "" {
		testEnv.BinaryAssetsDirectory = dir
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})

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

	return NewHandler(cs, k8s), k8s
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
