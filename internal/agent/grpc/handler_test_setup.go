package grpc

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	versioned "github.com/cybericebox/laboratory/clientset/client/versioned"
)

// newTestHandler bootstraps an envtest environment with the laboratory CRDs
// and returns a Handler backed by a real typed clientset talking to it.
// Mirrors the envtest bootstrap in internal/controller/laboratory/suite_test.go,
// but this package uses plain go test (not Ginkgo).
func newTestHandler(t *testing.T) *Handler {
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
	csCfg := *cfg
	csCfg.ContentType = "application/json"
	cs, err := versioned.NewForConfig(&csCfg)
	if err != nil {
		t.Fatalf("build versioned clientset: %v", err)
	}

	// Built for parity with the controller suite bootstrap and for reuse by
	// later handler tasks; not used by Task 6's Handler itself.
	if _, err := kubernetes.NewForConfig(cfg); err != nil {
		t.Fatalf("build kubernetes clientset: %v", err)
	}

	return NewHandler(cs)
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
