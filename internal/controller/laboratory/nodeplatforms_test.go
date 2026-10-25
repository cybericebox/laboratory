package laboratory

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPlatformsOfNodes(t *testing.T) {
	node := func(os, arch string) corev1.Node {
		return corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{corev1.LabelOSStable: os, corev1.LabelArchStable: arch}}}
	}
	got := platformsOf([]corev1.Node{node("linux", "arm64"), node("linux", "arm64"), {}})
	if len(got) != 1 || got[0].Architecture != "arm64" || got[0].OS != "linux" {
		t.Fatalf("one architecture: %v", got)
	}
	if got := platformsOf([]corev1.Node{node("linux", "arm64"), node("linux", "amd64")}); len(got) != 2 {
		t.Fatalf("mixed: %v", got)
	}
}

func TestNodePlatformsWithoutLabNodes(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	node := func(name, arch string, lab bool) *corev1.Node {
		labels := map[string]string{corev1.LabelOSStable: "linux", corev1.LabelArchStable: arch}
		if lab {
			labels["lab"] = "true"
		}
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	}
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node("cp", "amd64", false)).Build()
	platforms := NodePlatforms(c, map[string]string{"lab": "true"})
	if got := platforms(ctx); len(got) != 1 || got[0].Architecture != "amd64" {
		t.Fatalf("no lab node yet: the Linux nodes stand in, got %v", got)
	}
	worker := node("w1", "arm64", true)
	if err := c.Create(ctx, worker); err != nil {
		t.Fatal(err)
	}
	if got := platforms(ctx); len(got) != 1 || got[0].Architecture != "arm64" {
		t.Fatalf("a lab node: its platform, got %v", got)
	}
	if err := c.Delete(ctx, worker, &client.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := platforms(ctx); len(got) != 1 || got[0].Architecture != "arm64" {
		t.Fatalf("lab nodes scaled to zero: the last platform is kept, got %v", got)
	}
}
