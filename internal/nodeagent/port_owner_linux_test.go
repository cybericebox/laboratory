//go:build linux

package nodeagent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nstest"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func ownerPods(t *testing.T, component string) (client.Client, *corev1.Pod, *corev1.Pod) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = lab.AddToScheme(scheme)
	old := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "g", UID: "old-uid", Labels: map[string]string{names.LabelComponent: component}, Finalizers: []string{"fixture"}}, Spec: corev1.PodSpec{NodeName: "node"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	now := metav1.Now()
	old.DeletionTimestamp = &now
	replacement := old.DeepCopy()
	replacement.Name, replacement.UID, replacement.DeletionTimestamp = "replacement", "replacement-uid", nil
	legMeta := metav1.ObjectMeta{Name: "a", Namespace: "g"}
	objects := []client.Object{old, replacement, &lab.Lab{ObjectMeta: legMeta}}
	if component == names.ComponentVPN {
		objects = append(objects, &lab.LabVPN{ObjectMeta: legMeta, Spec: lab.LabVPNSpec{LabName: "a", NetworkIndex: 1}})
	} else {
		objects = append(objects, &lab.LabGateway{ObjectMeta: legMeta, Spec: lab.LabGatewaySpec{LabName: "a", NetworkIndex: 1}})
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), old, replacement
}

func TestNetnsPortOwnerOldCleanupKeepsReplacement(t *testing.T) {
	nstest.Require(t)
	ovs := testOVSDB(t)
	kube, old, replacement := ownerPods(t, names.ComponentVPN)
	key := names.VPNHostPortKey("g", 1)
	if err := ovs.AddVethPort(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ovs.DelVethPort(key) })
	ids, _ := ovsdb.NewOvsMap(map[string]string{portKeyExternalID: key, "pod-uid": string(replacement.UID)})
	ops := []ovsdb.Operation{{Op: ovsdb.OperationUpdate, Table: "Port", Where: []ovsdb.Condition{ovsdb.NewCondition("name", ovsdb.ConditionEqual, key)}, Row: ovsdb.Row{"external_ids": ids}}}
	if _, err := ovs.client.Transact(context.Background(), ops...); err != nil {
		t.Fatal(err)
	}
	r := &NetworkAttachReconciler{Client: kube, Reader: kube, NodeName: "node", OVS: ovs}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(old)})
	if err != nil {
		t.Logf("cleanup conservatively refused: %v", err)
	}
	if _, err := netlink.LinkByName(key); err != nil {
		t.Fatal("old terminating pod deleted replacement kernel port", err)
	}
}

func TestNetnsPortOwnerLegacyFenceAndUnknownACK(t *testing.T) {
	nstest.Require(t)
	kube, old, replacement := ownerPods(t, names.ComponentVPN)
	ovs := testOVSDB(t)
	key := names.VPNHostPortKey("g", 1)
	if err := ovs.AddVethPort(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ovs.DelVethPort(key) })
	r := &NetworkAttachReconciler{Client: kube, Reader: kube, NodeName: "node", OVS: ovs}
	if err := r.DelVethWithFlowsOwned(key, old.UID); !errors.Is(err, ErrPortOwnerUnknown) {
		t.Fatalf("legacy fixture without flow manager falsely acknowledged: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(old)}); !errors.Is(err, ErrPortOwnerUnknown) {
		t.Fatalf("live replacement needs legacy key: %v", err)
	}
	if _, err := netlink.LinkByName(key); err != nil {
		t.Fatal("legacy replacement leg deleted", err)
	}
	apiErr := errors.New("fixture direct API unavailable")
	r.Reader = portOwnerReader{Reader: kube, fail: apiErr}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(old)}); !errors.Is(err, apiErr) {
		t.Fatalf("legacy cleanup acknowledged API outage: %v", err)
	}
	if _, err := netlink.LinkByName(key); err != nil {
		t.Fatal("API uncertainty deleted legacy leg", err)
	}
	r.Reader = kube
	if err := kube.Delete(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	// Replacement is now terminating (the fixture finalizer retains it).
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(old)}); err != nil {
		t.Fatal(err)
	}
	if _, err := netlink.LinkByName(key); err == nil {
		t.Fatal("unneeded legacy leg not cleaned")
	}
	// This OVSDB-only fixture tests kernel/row idempotence, not a physical OpenFlow ACK.
	if err := r.delVethWithFlowsOwned(key, old.UID); err != nil {
		t.Fatal("confirmed absent old port must be idempotent", err)
	}
	// An orphan kernel link without an OVS owner cannot acknowledge cleanup.
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: key}, PeerName: VethPeerName(key)}); err != nil {
		t.Fatal(err)
	}
	if err := r.DelVethWithFlowsOwned(key, old.UID); !errors.Is(err, ErrPortOwnerUnknown) {
		t.Fatalf("orphan kernel link falsely acknowledged: %v", err)
	}
	if err := ovs.AddVethPortOwned(key, replacement.UID); !errors.Is(err, ErrPortOwnerUnknown) {
		t.Fatalf("unowned orphan kernel veth falsely claimed: %v", err)
	}
}

type portOwnerReader struct {
	client.Reader
	fail    error
	liveUID types.UID
}

func (r portOwnerReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*corev1.PodList); ok && r.fail != nil {
		return r.fail
	}
	if err := r.Reader.List(ctx, list, opts...); err != nil {
		return err
	}
	if pods, ok := list.(*corev1.PodList); ok {
		for i := range pods.Items {
			if pods.Items[i].UID == r.liveUID {
				pods.Items[i].DeletionTimestamp = nil
			}
		}
	}
	return nil
}

func TestNetnsPortOwnerReplacementOnlyTakesTerminatingOwner(t *testing.T) {
	nstest.Require(t)
	nstest.NS(t, "cice-t6-old")
	nstest.NS(t, "cice-t6-new")
	ovs := testOVSDB(t)
	kube, old, replacement := ownerPods(t, names.ComponentVPN)
	key := names.VPNHostPortKey("g", 1)
	if err := ovs.AddVethPortOwned(key, old.UID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ovs.DelVethPort(key) })
	if err := MoveToNetNS(VethPeerName(key), "/run/netns/cice-t6-old"); err != nil {
		t.Fatal(err)
	}
	r := &NetworkAttachReconciler{Client: kube, Reader: portOwnerReader{Reader: kube, liveUID: old.UID}, NodeName: "node", OVS: ovs, CRISock: testNamespaceCRI(t, "/run/netns/cice-t6-new")}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(replacement)}
	if _, err := r.Reconcile(context.Background(), req); !errors.Is(err, ErrPortOwnerChanged) {
		t.Fatalf("replacement stole live owner: %v", err)
	}
	owners, err := ovs.PortOwners()
	if err != nil || owners[key] != old.UID {
		t.Fatalf("live owner was overwritten: %v %v", owners, err)
	}
	apiErr := errors.New("fixture direct API unavailable")
	r.Reader = portOwnerReader{Reader: kube, fail: apiErr}
	if _, err := r.Reconcile(context.Background(), req); !errors.Is(err, apiErr) {
		t.Fatalf("API outage allowed takeover: %v", err)
	}
	if err := CheckInNetNS("/run/netns/cice-t6-old", VethPeerName(key)); err != nil {
		t.Fatal("API failure lost old port", err)
	}
	r.Reader = kube
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	owners, err = ovs.PortOwners()
	if err != nil || owners[key] != replacement.UID {
		t.Fatalf("terminating owner not replaced: %v %v", owners, err)
	}
	if err := CheckInNetNS("/run/netns/cice-t6-new", "lab1"); err != nil {
		t.Fatal("replacement actual peer missing", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(old)}); err != nil {
		t.Fatal(err)
	}
	if err := CheckInNetNS("/run/netns/cice-t6-new", "lab1"); err != nil {
		t.Fatal("old cleanup deleted new peer", err)
	}
}

func nativeOwnerOVS(t *testing.T) (*OVSManager, *FlowManager, string, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cice-t6-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	db, sock := filepath.Join(dir, "conf.db"), filepath.Join(dir, "db.sock")
	nstest.Run(t, "", "ovsdb-tool", "create", db, "/usr/share/openvswitch/vswitch.ovsschema")
	start := func(cmd *exec.Cmd) {
		t.Helper()
		log, err := os.Create(filepath.Join(dir, filepath.Base(cmd.Path)+".log"))
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); _ = log.Close() })
	}
	start(exec.Command("ovsdb-server", db, "--remote=punix:"+sock, "--no-chdir", "--unixctl="+filepath.Join(dir, "db.ctl")))
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
	ovs, err := NewOVSManager("t6-br", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ovs.Close)
	cmd := exec.Command("ovs-vswitchd", "unix:"+sock, "--no-chdir", "--unixctl="+filepath.Join(dir, "vs.ctl"))
	cmd.Env = append(os.Environ(), "OVS_RUNDIR="+dir)
	start(cmd)
	flows, err := NewFlowManager(dir, "t6-br")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = flows.Close() })
	t.Cleanup(func() {
		out, err := nstest.Try("", "ovs-vsctl", "--db=unix:"+sock, "del-br", "t6-br")
		if err != nil {
			t.Errorf("owned bridge cleanup: %v %s", err, out)
		}
		t.Logf("native OVS cleanup exact bridge=t6-br socket=%s: %v", sock, err)
	})
	return ovs, flows, dir, sock
}

func ownerFixtureBind(t *testing.T, flows *FlowManager, key, sibling string, vni uint) {
	t.Helper()
	for _, port := range []string{key, sibling} {
		if err := flows.AddT0Port(port, vni); err != nil {
			t.Fatal(err)
		}
	}
	if err := flows.RebuildT6Flood(vni, []string{key, sibling}, nil); err != nil {
		t.Fatal(err)
	}
}

// This helper runs in a NEW OS process. Kubernetes objects/CRI resolution are
// test adapters; OVSDB, OpenFlow, datapath, namespaces and packets stay native.
func TestPortOwnerRestartProcess(t *testing.T) {
	if os.Getenv("CICE_TASK6_RESTART") != "1" {
		t.Skip("owned parent-only process helper")
	}
	dir := os.Getenv("CICE_TASK6_DIR")
	if !strings.HasPrefix(dir, "/tmp/cice-t6-") {
		t.Fatal("not an owned fixture")
	}
	component := os.Getenv("CICE_TASK6_COMPONENT")
	ovs, err := NewOVSManager("t6-br", filepath.Join(dir, "db.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ovs.Close()
	flows, err := NewFlowManager(dir, "t6-br")
	if err != nil {
		t.Fatal(err)
	}
	defer flows.Close()
	kube, old, replacement := ownerPods(t, component)
	key, sibling := groupPortKey(component, "g", 1), names.DevicePortKey("g", "sibling", "eth1")
	r := &NetworkAttachReconciler{Client: kube, Reader: kube, NodeName: "node", OVS: ovs, Flows: flows, CRISock: testNamespaceCRI(t, "/run/netns/cice-t6-new")}
	for _, pod := range []*corev1.Pod{old, replacement} {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pod)}); err != nil {
			t.Fatal(err)
		}
	}
	ownerFixtureBind(t, flows, key, sibling, 22)
	owners, err := ovs.PortOwners()
	if err != nil || owners[key] != replacement.UID {
		t.Fatalf("restart owner changed: %v %v", owners, err)
	}
	t.Logf("fresh node-network process PID=%d controller reconciled old=%s replacement=%s OVS-owner=%s", os.Getpid(), old.UID, replacement.UID, owners[key])
}

func TestNetnsPortOwnerNativeRecycledOfportAndRestartConnectivity(t *testing.T) {
	nstest.Require(t)
	for _, component := range []string{names.ComponentVPN, names.ComponentGateway} {
		t.Run(component, func(t *testing.T) {
			t.Cleanup(func() {
				for _, ns := range []string{"cice-t6-old", "cice-t6-new", "cice-t6-peer"} {
					if _, err := os.Stat("/run/netns/" + ns); !os.IsNotExist(err) {
						t.Errorf("owned namespace retained %s: %v", ns, err)
					}
				}
				for _, port := range []string{groupPortKey(component, "g", 1), names.DevicePortKey("g", "sibling", "eth1")} {
					if _, err := netlink.LinkByName(port); err == nil {
						t.Errorf("owned kernel port retained %s", port)
					}
				}
				t.Logf("native cleanup component=%s: exact owned namespaces and kernel ports absent", component)
			})
			for _, ns := range []string{"cice-t6-old", "cice-t6-new", "cice-t6-peer"} {
				nstest.NS(t, ns)
			}
			ovs, flows, dir, sock := nativeOwnerOVS(t)
			kube, old, replacement := ownerPods(t, component)
			key, sibling := groupPortKey(component, "g", 1), names.DevicePortKey("g", "sibling", "eth1")
			for _, port := range []string{key, sibling} {
				t.Cleanup(func() { _ = ovs.DelVethPort(port) })
			}
			add := func(port string, uid types.UID, ns, addr string, requested int) {
				t.Helper()
				if err := ovs.AddVethPortOwned(port, uid); err != nil {
					t.Fatal(err)
				}
				nstest.Run(t, "", "ovs-vsctl", "--db=unix:"+sock, "set", "Interface", port, fmt.Sprintf("ofport_request=%d", requested))
				if err := MoveToNetNS(VethPeerName(port), "/run/netns/"+ns); err != nil {
					t.Fatal(err)
				}
				nstest.Run(t, ns, "ip", "addr", "add", addr, "dev", VethPeerName(port))
				nstest.Run(t, ns, "ip", "link", "set", VethPeerName(port), "up")
			}
			add(key, old.UID, "cice-t6-old", "198.18.6.1/24", 7)
			add(sibling, "sibling-uid", "cice-t6-peer", "198.18.6.2/24", 8)
			ownerFixtureBind(t, flows, key, sibling, 11)
			if !ownerPacketReady(t, "cice-t6-old") {
				t.Fatal("positive old owner packet control failed")
			}
			t.Logf("old UID=%s key=%s row=%s ofport=%s", old.UID, key, nstest.Run(t, "", "ovs-vsctl", "--db=unix:"+sock, "get", "Port", key, "_uuid"), nstest.Run(t, "", "ovs-vsctl", "--db=unix:"+sock, "get", "Interface", key, "ofport"))
			r := &NetworkAttachReconciler{Client: kube, Reader: kube, NodeName: "node", OVS: ovs, Flows: flows}
			if err := r.DelVethWithFlowsOwned(key, old.UID); err != nil {
				t.Fatal(err)
			}
			if err := r.DelVethWithFlowsOwned(key, old.UID); !errors.Is(err, ErrPortOwnerUnknown) {
				t.Fatalf("missing owner row cannot create a fresh cleanup ACK: %v", err)
			}
			retired := nstest.Run(t, "", "ovs-ofctl", "-O", "OpenFlow13", "dump-flows", "unix:"+filepath.Join(dir, "t6-br.mgmt"))
			t.Logf("confirmed retirement before recycle: %s", retired)
			if strings.Contains(retired, "in_port=7") || strings.Contains(retired, "output:7") {
				t.Fatal("old flows survived owned port deletion")
			}
			add(key, replacement.UID, "cice-t6-new", "198.18.6.1/24", 7)
			// The dynamic group attachment reconciler expects the peer's lab iface.
			if err := RenameInNetNS("/run/netns/cice-t6-new", VethPeerName(key), "lab1"); err != nil {
				t.Fatal(err)
			}
			ownerFixtureBind(t, flows, key, sibling, 22)
			no := strings.TrimSpace(nstest.Run(t, "", "ovs-vsctl", "--db=unix:"+sock, "get", "Interface", key, "ofport"))
			if no != "7" {
				t.Fatalf("ofport not actually recycled: %s", no)
			}
			t.Logf("replacement UID=%s key=%s row=%s recycled ofport=%s externalIDs=%s", replacement.UID, key, nstest.Run(t, "", "ovs-vsctl", "--db=unix:"+sock, "get", "Port", key, "_uuid"), no, nstest.Run(t, "", "ovs-vsctl", "--db=unix:"+sock, "get", "Port", key, "external_ids"))
			for pass := 0; pass < 3; pass++ {
				if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(old)}); err != nil {
					t.Fatal(err)
				}
				if err := r.DelVethWithFlowsOwned(key, old.UID); !errors.Is(err, ErrPortOwnerChanged) {
					t.Fatalf("old cleanup false ACK: %v", err)
				}
				if err := ovs.AddVethPortOwned(key, old.UID); !errors.Is(err, ErrPortOwnerChanged) {
					t.Fatalf("old writer stole replacement: %v", err)
				}
				if !ownerPacketReady(t, "cice-t6-new") {
					t.Fatal("replacement packet lost after old cleanup")
				}
			}
			_ = flows.Close()
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "-test.run=^TestPortOwnerRestartProcess$", "-test.v")
			cmd.Env = append(os.Environ(), "CICE_TASK6_RESTART=1", "CICE_TASK6_DIR="+dir, "CICE_TASK6_COMPONENT="+component)
			output, err := cmd.CombinedOutput()
			t.Logf("restart process evidence: %s", output)
			if err != nil {
				t.Fatal(err)
			}
			if !ownerPacketReady(t, "cice-t6-new") {
				t.Log(nstest.Run(t, "", "ovs-ofctl", "-O", "OpenFlow13", "dump-flows", "unix:"+filepath.Join(dir, "t6-br.mgmt")))
				t.Fatal("replacement lost connectivity after fresh network process restart")
			}
			t.Logf("post-restart packets successful; native flows: %s", nstest.Run(t, "", "ovs-ofctl", "-O", "OpenFlow13", "dump-flows", "unix:"+filepath.Join(dir, "t6-br.mgmt")))
		})
	}
}

func ownerPacketReady(t *testing.T, ns string) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		output, err := nstest.Try(ns, "ping", "-c", "1", "-W", "1", "198.18.6.2")
		t.Logf("native packet probe namespace=%s error=%v\n%s", ns, err, output)
		if err == nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPortOwnerCleanupWithoutFlowManagerCannotACK(t *testing.T) {
	r := &NetworkAttachReconciler{OVS: &OVSManager{}}
	if err := r.DelVethWithFlowsOwned(names.VPNHostPortKey("g", 1), "pod"); !errors.Is(err, ErrPortOwnerUnknown) {
		t.Fatalf("missing physical flow manager acknowledged cleanup: %v", err)
	}
}
