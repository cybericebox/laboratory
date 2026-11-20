package registrygate

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPodInfoKeepsWhatTheGateReads(t *testing.T) {
	now := metav1.NewTime(time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC))
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", UID: "u1", Labels: map[string]string{"a": "b"}, DeletionTimestamp: &now},
		Spec: corev1.PodSpec{
			NodeName:       "n1",
			InitContainers: []corev1.Container{{Name: "i", Image: "localhost:5035/c/t/docker.io/x/init:1"}},
			Containers:     []corev1.Container{{Name: "c", Image: "localhost:5035/c/t/docker.io/x/app:1", Env: []corev1.EnvVar{{Name: "S", Value: "secret"}}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	got, err := trimPod(p)
	if err != nil {
		t.Fatal(err)
	}
	tp := got.(*corev1.Pod)
	if len(tp.Spec.Containers[0].Env) != 0 {
		t.Fatal("the cache must not keep environment variables")
	}
	info := podInfo(tp)
	if info.Node != "n1" || info.UID != "u1" || !info.Terminating || info.Phase != "Running" || len(info.Images) != 2 || info.Labels["a"] != "b" {
		t.Fatalf("%+v", info)
	}
	if info.Images[0] != "localhost:5035/c/t/docker.io/x/init:1" {
		t.Fatalf("init containers come first: %v", info.Images)
	}
	if !info.Ended() {
		t.Fatal("a terminating pod is ended")
	}
}

func TestNodeInfoKeepsAddressesOnly(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: "10.0.0.5"}, {Type: corev1.NodeHostName, Address: "n1"}, {Type: corev1.NodeExternalIP, Address: "bad"},
	}}}
	tn, _ := trimNode(n)
	info := nodeInfo(tn.(*corev1.Node))
	if info.Name != "n1" || len(info.Addrs) != 1 || info.Addrs[0].String() != "10.0.0.5" {
		t.Fatalf("%+v", info)
	}
}
