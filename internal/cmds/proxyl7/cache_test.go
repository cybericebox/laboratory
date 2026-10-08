package proxyl7

import (
	"context"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/proxy/l7"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"os"
	"path/filepath"
	"runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"testing"
	"time"
)

func TestProxyWarmCachePreservesAccessAndNamespace(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		assets = filepath.Join("..", "..", "..", "bin", "k8s", "1.33.0-"+runtime.GOOS+"-"+runtime.GOARCH)
	}
	if _, err := os.Stat(filepath.Join(assets, "kube-apiserver")); err != nil {
		t.Skip("envtest assets unavailable")
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	cfg, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	writer, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	group := &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g1", Labels: map[string]string{names.LabelTenant: "acme"}}, Spec: lab.LabGroupSpec{VPN: lab.LabGroupVPNSpec{Disabled: true}}}
	if err := writer.Create(ctx, group); err != nil {
		t.Fatal(err)
	}
	group.Status.Namespace = "legacy"
	group.Status.Phase = lab.PhaseReady
	if err := writer.Status().Update(ctx, group); err != nil {
		t.Fatal(err)
	}
	if err := writer.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: names.AccessKeysNamespace}}); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tenant-key", Namespace: names.AccessKeysNamespace}, Data: map[string][]byte{"key": []byte("public")}}
	if err := writer.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	options := proxyCacheOptions()
	options.Scheme = scheme
	reader, err := cache.New(cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := warmProxyCache(ctx, reader); err != nil {
		t.Fatal(err)
	}
	running, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reader.Start(running) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("cache did not stop")
		}
	})
	if !reader.WaitForCacheSync(ctx) {
		t.Fatal("required caches failed to sync")
	}
	access := &l7.AccessReader{Reader: reader}
	got, err := access.Group(ctx, "g1")
	if err != nil || got.Namespace != "legacy" || got.Tenant != "acme" {
		t.Fatal(got, err)
	}
	var cached lab.LabGroup
	if err := reader.Get(ctx, types.NamespacedName{Name: "g1"}, &cached); err != nil {
		t.Fatal(err)
	}
	if cached.Spec.VPN.Disabled || cached.Status.Phase != "" || cached.Status.Namespace != "legacy" {
		t.Fatal(cached)
	}
	var original lab.LabGroup
	if err := writer.Get(ctx, types.NamespacedName{Name: "g1"}, &original); err != nil || !original.Spec.VPN.Disabled || original.Status.Phase != lab.PhaseReady {
		t.Fatal("transform changed source", original, err)
	}
	var key corev1.Secret
	if err := reader.Get(ctx, types.NamespacedName{Namespace: names.AccessKeysNamespace, Name: "tenant-key"}, &key); err != nil || string(key.Data["key"]) != "public" {
		t.Fatal(key, err)
	}
	// The compact informer retains desired stop and its own Lab watch observes it.
	target := &lab.Lab{ObjectMeta: metav1.ObjectMeta{Name: "stop-target", Namespace: "default"}}
	if err := writer.Create(ctx, target); err != nil {
		t.Fatal(err)
	}
	for {
		var got lab.Lab
		err := reader.Get(ctx, client.ObjectKeyFromObject(target), &got)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	started := time.Now()
	target.Spec.Lifecycle = &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "op", Revision: 1, SnapshotMode: "Skip"}
	if err := writer.Update(ctx, target); err != nil {
		t.Fatal(err)
	}
	for {
		var got lab.Lab
		err := reader.Get(ctx, client.ObjectKeyFromObject(target), &got)
		if err == nil && got.Spec.Lifecycle.IsStopped() {
			if len(got.Spec.Devices) != 0 || got.Status.Phase != "" {
				t.Fatal("compact cache retained unrelated fields")
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("Lab stop watch did not reach proxy", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Logf("API desired stop to compact proxy informer observation: %s", time.Since(started))

	var missing lab.LabGroupClient
	if err := reader.Get(ctx, types.NamespacedName{Namespace: "legacy", Name: "absent"}, &missing); !apierrors.IsNotFound(err) {
		t.Fatalf("client informer was not ready: %v", err)
	}
	var policy lab.LabGroupAccessPolicy
	if err := reader.Get(ctx, types.NamespacedName{Namespace: "legacy", Name: "absent"}, &policy); !apierrors.IsNotFound(err) {
		t.Fatalf("policy informer was not ready: %v", err)
	}
	var service corev1.Service
	if err := reader.Get(ctx, types.NamespacedName{Namespace: "legacy", Name: "absent"}, &service); !apierrors.IsNotFound(err) {
		t.Fatalf("service informer was not ready: %v", err)
	}
}

func TestNamespaceInventoryKeepsFallbackGroup(t *testing.T) {
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&lab.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g1"}, Status: lab.LabGroupStatus{Namespace: "legacy"}}, &lab.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g2"}}).Build()
	got := groupNamespaces(context.Background(), reader)
	if len(got) != 2 {
		t.Fatal(got)
	}
	for _, ns := range got {
		if ns == "" {
			t.Fatal("group counters would retire before its namespace status arrives", got)
		}
	}
	empty := fake.NewClientBuilder().WithScheme(scheme).Build()
	if groups := groupNamespaces(context.Background(), empty); groups == nil || len(groups) != 0 {
		t.Fatal("empty valid inventory confused with failed read", groups)
	}
}
