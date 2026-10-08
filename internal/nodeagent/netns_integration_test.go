//go:build linux

package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	criapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/netattach"
	"github.com/cybericebox/laboratory/internal/nstest"
	nodev1 "github.com/cybericebox/laboratory/pkg/rpc/node/v1"
)

// Only the unavailable container runtime is replaced: namespace and OVSDB
// operations exercise the same kernel and protocol clients as the node-agent.
type namespaceRuntime struct {
	criapi.UnimplementedRuntimeServiceServer
	path string
}

func (r *namespaceRuntime) ListPodSandbox(context.Context, *criapi.ListPodSandboxRequest) (*criapi.ListPodSandboxResponse, error) {
	return &criapi.ListPodSandboxResponse{Items: []*criapi.PodSandbox{{Id: "sandbox"}}}, nil
}

func (r *namespaceRuntime) PodSandboxStatus(context.Context, *criapi.PodSandboxStatusRequest) (*criapi.PodSandboxStatusResponse, error) {
	b, _ := json.Marshal(map[string]any{"runtimeSpec": map[string]any{"linux": map[string]any{"namespaces": []map[string]string{{"type": "network", "path": r.path}}}}})
	return &criapi.PodSandboxStatusResponse{Info: map[string]string{"info": string(b)}}, nil
}

func testOVSDB(t *testing.T) *OVSManager {
	t.Helper()
	dir := t.TempDir()
	db, sock := filepath.Join(dir, "conf.db"), filepath.Join(dir, "db.sock")
	nstest.Run(t, "", "ovsdb-tool", "create", db, "/usr/share/openvswitch/vswitch.ovsschema")
	cmd := exec.Command("ovsdb-server", db, "--remote=punix:"+sock, "--no-chdir", "--unixctl="+filepath.Join(dir, "ctl"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.Dial("unix", sock)
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	nstest.Run(t, "", "ovs-vsctl", "--db=unix:"+sock, "--no-wait", "init")
	m, err := NewOVSManager("audit-br", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

func TestNetnsAttachmentRecoveryAndSteadyLatency(t *testing.T) {
	nstest.Require(t)
	nstest.NS(t, "cice-peer")
	path := "/run/netns/cice-peer"
	ovs := testOVSDB(t)
	var attachments []netattach.Attachment
	for i := 0; i < 4; i++ {
		attachments = append(attachments, netattach.Attachment{Iface: fmt.Sprintf("eth%d", i), MAC: fmt.Sprintf("02:00:00:00:00:%02x", i+1)})
	}
	t.Cleanup(func() {
		for _, att := range attachments {
			_ = ovs.DelVethPort(names.DevicePortKey("ns", "pod", att.Iface))
		}
	})
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns", UID: "pod-uid",
		Labels:      map[string]string{names.LabelLab: "lab", names.LabelDevice: "dev"},
		Annotations: map[string]string{names.AnnotationNetworks: netattach.Encode(attachments), names.AnnotationDefaultNetwork: ""}},
		Spec: corev1.PodSpec{NodeName: "node"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = laboratoryv1alpha1.AddToScheme(scheme)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod, &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: "lab", Namespace: "ns"}}).Build()
	s := NewNodeAgentServer(ovs, nil)
	s.SetK8sClient(kube)
	req := &nodev1.SetupNetworksRequest{Namespace: pod.Namespace, Name: pod.Name, PodUid: string(pod.UID), NetnsPath: path}
	setup := func() {
		t.Helper()
		res, err := s.SetupNetworks(context.Background(), req)
		if err != nil || res.DefaultNetwork != "stub" {
			t.Fatalf("setup: %v, result=%v", err, res)
		}
	}
	setup()
	start := time.Now()
	setup()
	took := time.Since(start)
	t.Logf("repeated CNI setup, four attachments: %s", took)
	if took >= 600*time.Millisecond {
		t.Fatalf("CNI repeated setup waited for already-moved peers: %s", took)
	}
	// A peer moved before rename is recovered, and a down interface is raised.
	peer := VethPeerName(names.DevicePortKey("ns", "pod", "eth0"))
	if err := RenameInNetNS(path, "eth0", peer); err != nil {
		t.Fatal(err)
	}
	nstest.Run(t, "cice-peer", "ip", "link", "set", peer, "down")
	setup()
	nstest.Run(t, "cice-peer", "ip", "link", "show", "eth0")
	// The periodic reconciler exercises its own existing-port lookup branch.
	sock := filepath.Join(t.TempDir(), "cri.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	criapi.RegisterRuntimeServiceServer(srv, &namespaceRuntime{path: path})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	r := &NetworkAttachReconciler{Client: kube, NodeName: "node", OVS: ovs, CRISock: sock}
	reconcile := func() ctrl.Result {
		t.Helper()
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pod)})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	start = time.Now()
	if res := reconcile(); res.RequeueAfter != 30*time.Second {
		t.Fatalf("healthy periodic result: %+v", res)
	}
	took = time.Since(start)
	t.Logf("periodic reconcile, four attachments: %s", took)
	if took >= 600*time.Millisecond {
		t.Fatalf("periodic reconcile waited for already-moved peers: %s", took)
	}
	// A replaced pod loses all peers together. Retire every stale OVS record
	// in one pass, then restore every attachment on the next pass: recovery
	// must not require an additional one-second requeue for each interface.
	for _, att := range attachments {
		if err := DeleteInNetNS(path, att.Iface); err != nil {
			t.Fatal(err)
		}
	}
	if res := reconcile(); res.RequeueAfter != time.Second {
		t.Fatalf("lost peer did not schedule recreation: %+v", res)
	}
	for _, att := range attachments {
		key := names.DevicePortKey("ns", "pod", att.Iface)
		if _, exists, err := ovs.FindPortByKey(key); err != nil || exists {
			t.Fatalf("stale peer %s was not retired in the same pass: exists=%t err=%v", att.Iface, exists, err)
		}
	}
	reconcile()
	for _, att := range attachments {
		if err := CheckInNetNS(path, att.Iface); err != nil {
			t.Fatalf("lost peer %s was not restored: %v", att.Iface, err)
		}
	}
	// A later peer may already be in cooldown. It must stay capped without
	// delaying the earlier peers this pass retired and can restore next pass.
	r.guard = newRecreationGuard(1, 10*time.Minute)
	blocked := attachments[len(attachments)-1]
	blockedKey := names.DevicePortKey(pod.Namespace, pod.Name, blocked.Iface)
	if ok, _ := r.mayRecreate(string(pod.UID) + "/" + blockedKey); !ok {
		t.Fatal("could not consume the blocked peer's recreation allowance")
	}
	for _, att := range attachments {
		if err := DeleteInNetNS(path, att.Iface); err != nil {
			t.Fatal(err)
		}
	}
	if res := reconcile(); res.RequeueAfter != time.Second {
		t.Fatalf("a later peer's cooldown delayed already-retired peers: %+v", res)
	}
	if _, exists, err := ovs.FindPortByKey(blockedKey); err != nil || !exists {
		t.Fatalf("the capped peer must not be retired: exists=%t err=%v", exists, err)
	}
	if res := reconcile(); res.RequeueAfter <= time.Minute {
		t.Fatalf("the capped peer's cooldown was bypassed: %+v", res)
	}
	for _, att := range attachments[:len(attachments)-1] {
		if err := CheckInNetNS(path, att.Iface); err != nil {
			t.Fatalf("allowed peer %s was not restored before cooldown: %v", att.Iface, err)
		}
	}
	if err := CheckInNetNS(path, blocked.Iface); err == nil {
		t.Fatal("the capped peer was recreated during cooldown")
	}
}

func TestNetnsNewPeerCanAppearAfterFirstLookup(t *testing.T) {
	nstest.Require(t)
	name := "cice-delayed"
	done := make(chan error, 1)
	go func() {
		time.Sleep(40 * time.Millisecond)
		done <- netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}})
	}()
	found := peerInRoot(name, true)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if link, err := netlink.LinkByName(name); err == nil {
			_ = netlink.LinkDel(link)
		}
	})
	if !found {
		t.Fatal("a new peer appearing during the creation wait was missed")
	}
}

func testNamespaceCRI(t *testing.T, path string) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "cri.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	criapi.RegisterRuntimeServiceServer(srv, &namespaceRuntime{path: path})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return sock
}

// Exercise the shared VPN ports at the larger group sizes used in recovery
// measurements. Destroying the old namespace removes every kernel peer, while
// its stale OVS records remain for the replacement pod's reconciler to retire.
func TestNetnsGroupReplacementRecovery(t *testing.T) {
	nstest.Require(t)
	for _, count := range []int{70, 100} {
		t.Run(fmt.Sprintf("ports%d", count), func(t *testing.T) {
			nstest.NS(t, "cice-old")
			nstest.NS(t, "cice-new")
			ovs := testOVSDB(t)
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := laboratoryv1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "vpn-old", Namespace: "ns", UID: "old-uid",
				Labels: map[string]string{names.LabelComponent: names.ComponentVPN}},
				Spec: corev1.PodSpec{NodeName: "node"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
			objects := []client.Object{pod}
			for n := 1; n <= count; n++ {
				objects = append(objects, &laboratoryv1alpha1.LabVPN{
					ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("lab%d", n), Namespace: pod.Namespace},
					Spec:       laboratoryv1alpha1.LabVPNSpec{LabName: fmt.Sprintf("lab%d", n), NetworkIndex: uint(n)},
				})
			}
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			r := &NetworkAttachReconciler{Client: kube, NodeName: "node", OVS: ovs, CRISock: testNamespaceCRI(t, "/run/netns/cice-old")}
			reconcile := func() ctrl.Result {
				t.Helper()
				res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pod)})
				if err != nil {
					t.Fatal(err)
				}
				return res
			}
			checkPeers := func(path string) bool {
				t.Helper()
				for n := 1; n <= count; n++ {
					if err := CheckInNetNS(path, names.LabIfaceNameByIndex(uint(n))); err != nil {
						return false
					}
				}
				return true
			}
			t.Cleanup(func() {
				for n := 1; n <= count; n++ {
					_ = ovs.DelVethPort(names.VPNHostPortKey(pod.Namespace, uint(n)))
				}
			})
			if res := reconcile(); res.RequeueAfter != 30*time.Second || !checkPeers("/run/netns/cice-old") {
				t.Fatalf("initial group wiring incomplete: %+v", res)
			}
			nstest.Run(t, "", "ip", "netns", "del", "cice-old")
			if err := kube.Delete(context.Background(), pod); err != nil {
				t.Fatal(err)
			}
			pod.Name, pod.UID, pod.ResourceVersion = "vpn-new", "new-uid", ""
			if err := kube.Create(context.Background(), pod); err != nil {
				t.Fatal(err)
			}
			r.CRISock = testNamespaceCRI(t, "/run/netns/cice-new")
			start := time.Now()
			var passes int
			var scheduledWait time.Duration
			for {
				passes++
				res := reconcile()
				if checkPeers("/run/netns/cice-new") {
					if res.RequeueAfter != 30*time.Second {
						t.Fatalf("restored group did not return to steady state: %+v", res)
					}
					break
				}
				if res.RequeueAfter != time.Second || passes > count {
					t.Fatalf("replacement recovery made no bounded progress: pass=%d result=%+v", passes, res)
				}
				scheduledWait += res.RequeueAfter
				time.Sleep(res.RequeueAfter)
			}
			t.Logf("replacement recovery: ports=%d passes=%d scheduled_wait=%s elapsed=%s", count, passes, scheduledWait, time.Since(start))
			if passes != 2 || scheduledWait != time.Second {
				t.Fatalf("replacement peers were not recovered as one batch: passes=%d scheduled_wait=%s", passes, scheduledWait)
			}
		})
	}
}
