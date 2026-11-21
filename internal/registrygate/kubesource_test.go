package registrygate

import (
	"errors"
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

// Review finding 3: one dead watch makes the view stale, however alive the API server and the other kinds are.
func TestOneFailingKindMakesTheViewUnhealthy(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	h := &informerHealth{Now: func() time.Time { return now }, Window: time.Minute}
	for _, k := range []string{"Pod", "Device", "Tenant"} {
		h.ListResult(k, nil)
	}
	if ok, _ := h.Healthy(); !ok {
		t.Fatal("all kinds list fine")
	}
	h.WatchError("*v1.Pod")
	if ok, k := h.Healthy(); ok || k != "*v1.Pod" {
		t.Fatalf("a watch error: %v %q", ok, k)
	}
	now = now.Add(2 * time.Minute)
	if ok, _ := h.Healthy(); !ok {
		t.Fatal("the watch has been quiet for longer than the window")
	}
	h.ListResult("Device", errors.New("forbidden"))
	if ok, k := h.Healthy(); ok || k != "Device" {
		t.Fatalf("a failing list: %v %q", ok, k)
	}
	h.ListResult("Device", nil)
	if ok, _ := h.Healthy(); !ok {
		t.Fatal("the list recovered")
	}
}

// Review 2, item 2: a watch that stalled without an error is seen by comparing the informer's resourceVersion with the list's.
func TestASilentlyStalledInformerMakesTheViewUnhealthy(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	h := &informerHealth{Now: func() time.Time { return now }, StallAfter: 3 * time.Minute}
	h.ListResult("Pod", nil)
	h.Observe("Pod", "100", "100") // caught up
	now = now.Add(time.Minute)
	h.Observe("Pod", "100", "180") // behind, but only just seen
	if ok, _ := h.Healthy(); !ok {
		t.Fatal("a moment behind is not a stall")
	}
	now = now.Add(2 * time.Minute)
	h.Observe("Pod", "100", "260") // behind and stuck at 100 since
	now = now.Add(2 * time.Minute)
	h.Observe("Pod", "100", "300")
	if ok, k := h.Healthy(); ok || k != "Pod" {
		t.Fatalf("stuck behind for longer than the stall time: %v %q", ok, k)
	}
	h.Observe("Pod", "290", "300") // it moves again
	if ok, _ := h.Healthy(); !ok {
		t.Fatal("an informer that advances is healthy again")
	}
	h.Observe("Pod", "not-a-number", "300") // unreadable versions are ignored
	if ok, _ := h.Healthy(); !ok {
		t.Fatal("unreadable versions say nothing")
	}
	// a quiet kind that is caught up stays healthy however long nothing happens
	h.Observe("Pod", "300", "300")
	h.Observe("Tenant", "50", "50")
	now = now.Add(time.Hour)
	h.Observe("Pod", "300", "300")
	h.Observe("Tenant", "50", "50")
	if ok, _ := h.Healthy(); !ok {
		t.Fatal("caught up is healthy")
	}
}
