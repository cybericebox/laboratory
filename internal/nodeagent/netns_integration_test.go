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
	criapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
	kube := fake.NewClientBuilder().WithObjects(pod).Build()
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
	// Deleting the pod-side peer removes the pair; the OVS record is retired,
	// then the next pass recreates and attaches it to the same pod namespace.
	if err := DeleteInNetNS(path, "eth0"); err != nil {
		t.Fatal(err)
	}
	if res := reconcile(); res.RequeueAfter != time.Second {
		t.Fatalf("lost peer did not schedule recreation: %+v", res)
	}
	reconcile()
	if err := CheckInNetNS(path, "eth0"); err != nil {
		t.Fatalf("lost peer was not restored: %v", err)
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
