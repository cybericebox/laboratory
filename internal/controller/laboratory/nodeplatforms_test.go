package laboratory

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
