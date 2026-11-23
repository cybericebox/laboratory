package nodeagent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/consolespec"
	"github.com/cybericebox/laboratory/internal/deviceaccess/wire"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/portkeeper"
	labstatus "github.com/cybericebox/laboratory/internal/status"
)

// The reasons of a line are the reasons of the ConsoleCable's Ready condition.
func TestLineReasonsAreTheCableReasons(t *testing.T) {
	if wire.LineWaitingForDevice != laboratoryv1alpha1.ConsoleCableReasonWaitingForDevice || wire.LinePortDown != laboratoryv1alpha1.ConsoleCableReasonPortDown ||
		wire.LinePeerUnreachable != laboratoryv1alpha1.ConsoleCableReasonPeerUnreachable || wire.LineNodeLineLimit != laboratoryv1alpha1.ConsoleCableReasonNodeLineLimit {
		t.Fatal("the line reasons differ from the ConsoleCable reasons")
	}
}

type fakeCable struct {
	mu    sync.Mutex
	last  wire.LinesRequest
	calls int
	err   error
	// old: a service of a release without the kernel log (no Features in its answer)
	old bool
	// state answers a line
	state func(l wire.LineSpec) wire.LineState
}

func (f *fakeCable) PutLines(_ context.Context, req wire.LinesRequest) (wire.LinesReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = req
	if f.err != nil {
		return wire.LinesReply{}, f.err
	}
	out := wire.LinesReply{Relay: wire.RelayVersion, RelayKey: relayKeyOf("n1")}
	if !f.old {
		out.Features = []string{wire.FeatureKernelLog}
	}
	for _, l := range req.Lines {
		st := wire.LineState{Namespace: l.Namespace, Name: l.Name, UID: l.UID, Role: l.Role, Reason: wire.LineWaitingForDevice}
		if f.state != nil {
			st = f.state(l)
		}
		out.Lines = append(out.Lines, st)
	}
	return out, nil
}

func upState(l wire.LineSpec) wire.LineState {
	return wire.LineState{Namespace: l.Namespace, Name: l.Name, UID: l.UID, Role: l.Role, ConsoleAttached: true, SerialAttached: true, Up: true}
}

const ccNS = "lab-group-a"

func ccScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := laboratoryv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func ccDevice(name, lab, node string, serial bool) *laboratoryv1alpha1.Device {
	d := &laboratoryv1alpha1.Device{ObjectMeta: metav1.ObjectMeta{Name: lab + "-" + name, Namespace: ccNS, Labels: map[string]string{names.LabelLab: lab}},
		Spec:   laboratoryv1alpha1.DeviceSpec{LabRef: lab, Name: name, Type: laboratoryv1alpha1.DeviceTypeContainer},
		Status: laboratoryv1alpha1.DeviceStatus{NodeName: node}}
	if serial {
		d.Spec.SerialPorts = []laboratoryv1alpha1.SerialPortSpec{{Name: "serial0"}}
	} else {
		d.Spec.ConsolePorts = []laboratoryv1alpha1.ConsolePortSpec{{Name: "console0"}}
	}
	return d
}

// ccCable is a cable whose console end the operator stamped on n1.
func ccCable(lab string) *laboratoryv1alpha1.ConsoleCable {
	return &laboratoryv1alpha1.ConsoleCable{
		ObjectMeta: metav1.ObjectMeta{Name: lab + "-cable", Namespace: ccNS, UID: types.UID(lab + "-cable-uid"), Labels: map[string]string{names.LabelLab: lab},
			Annotations:       map[string]string{names.AnnotationConsoleNode: "n1"},
			CreationTimestamp: metav1.Unix(1_700_000_000, 0),
			OwnerReferences:   []metav1.OwnerReference{{APIVersion: "laboratory.cybericebox.com/v1alpha1", Kind: "Lab", Name: lab, UID: types.UID(lab + "-uid"), Controller: ptrBool(true)}}},
		Spec: laboratoryv1alpha1.ConsoleCableSpec{LabRef: lab, Serial: laboratoryv1alpha1.PortRef{Device: "pc1", Port: "serial0"},
			Console: laboratoryv1alpha1.PortRef{Device: "r1", Port: "console0"}},
	}
}

func ptrBool(b bool) *bool { return &b }

func daPod(node, ip string, ready bool) *corev1.Pod {
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "da-" + node, Namespace: "laboratory-device-access", Labels: map[string]string{"app": deviceAccessApp}},
		Spec:   corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}}}
}

// relayKeyOf is the key the node-agent of a test node published.
func relayKeyOf(node string) string { return strings.Repeat(node[len(node)-1:], 43) }

// ccNode is a node whose node-agent published its relay key.
func ccNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{names.AnnotationConsoleRelayKey: relayKeyOf(name)}}}
}

func ccSync(t *testing.T, node string, cable *fakeCable, objs ...client.Object) (*ConsoleCableSync, client.Client, *string) {
	t.Helper()
	objs = append(objs, ccNode("n1"), ccNode("n2"), ccNode("n3"))
	c := fake.NewClientBuilder().WithScheme(ccScheme(t)).WithObjects(objs...).WithStatusSubresource(&laboratoryv1alpha1.ConsoleCable{}).Build()
	relay := new(string)
	return &ConsoleCableSync{Client: c, NodeName: node, Cable: cable, DeviceAccessNamespace: "laboratory-device-access", CablePort: 8081,
		Relay: func(v string) { *relay = v }}, c, relay
}

func cableStatus(t *testing.T, c client.Client, lab string) laboratoryv1alpha1.ConsoleCableStatus {
	t.Helper()
	var cc laboratoryv1alpha1.ConsoleCable
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ccNS, Name: lab + "-cable"}, &cc); err != nil {
		t.Fatal(err)
	}
	return cc.Status
}

// Both ends on this node: one owner line without a peer, the ends with their full port lists (whose digest is the pod label), and the
// status of the cable from the service's answer.
func TestConsoleCableSyncSameNode(t *testing.T) {
	cable := &fakeCable{state: upState}
	s, c, relay := ccSync(t, "n1", cable, ccCable("l1"), ccDevice("r1", "l1", "n1", false), ccDevice("pc1", "l1", "n1", true))
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(cable.last.Lines) != 1 {
		t.Fatalf("lines %+v", cable.last.Lines)
	}
	l := cable.last.Lines[0]
	if l.Role != wire.RoleOwner || l.Peer != "" || l.Lab != "l1" || l.LabUID != "l1-uid" || l.UID != "l1-cable-uid" ||
		l.Serial.Device != "pc1" || l.Serial.Port != "serial0" || l.Console.Device != "r1" || l.Console.Port != "console0" {
		t.Fatalf("line %+v", l)
	}
	ks, err := portkeeper.Parse(l.Serial.Ports)
	if err != nil || portkeeper.Hash(ks) != consolespec.PortsHash(consolespec.KeeperSpec(&ccDevice("pc1", "l1", "n1", true).Spec)) {
		t.Fatalf("the port list of the serial end does not have the digest of the pod label: %v", err)
	}
	st := cableStatus(t, c, "l1")
	cond := meta.FindStatusCondition(st.Conditions, labstatus.ConditionReady)
	if !st.LineUp || !st.Serial.Attached || !st.Console.Attached || st.Serial.NodeName != "n1" || st.Console.NodeName != "n1" || cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("status %+v", st)
	}
	if *relay != PreflightReady {
		t.Fatalf("relay annotation %q", *relay)
	}
}

// Ends on two nodes: the owner gets the address of the device access pod of the serial end's node; that node gets the line with the
// serial role and writes nothing.
func TestConsoleCableSyncTwoNodes(t *testing.T) {
	objs := []client.Object{ccCable("l1"), ccDevice("r1", "l1", "n1", false), ccDevice("pc1", "l1", "n2", true),
		daPod("n1", "10.1.0.1", true), daPod("n2", "10.1.0.2", true)}
	owner := &fakeCable{state: upState}
	s, c, _ := ccSync(t, "n1", owner, objs...)
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(owner.last.Lines) != 1 || owner.last.Lines[0].Role != wire.RoleOwner || owner.last.Lines[0].Peer != "10.1.0.2:8081" || owner.last.Lines[0].PeerKey != relayKeyOf("n2") {
		t.Fatalf("owner lines (with the key the serial end's node published) %+v", owner.last.Lines)
	}
	if st := cableStatus(t, c, "l1"); !st.LineUp || st.Serial.NodeName != "n2" || st.Console.NodeName != "n1" {
		t.Fatalf("status %+v", st)
	}

	serial := &fakeCable{}
	s2, c2, _ := ccSync(t, "n2", serial, objs...)
	if err := s2.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(serial.last.Lines) != 1 || serial.last.Lines[0].Role != wire.RoleSerial || serial.last.Lines[0].Peer != "" || serial.last.Lines[0].ConsoleNode != "n1" ||
		serial.last.Lines[0].ConsoleNodeKey != relayKeyOf("n1") {
		t.Fatalf("serial lines (the acceptor must know the console end's node) %+v", serial.last.Lines)
	}
	if st := cableStatus(t, c2, "l1"); st.Console.NodeName != "" || len(st.Conditions) != 0 {
		t.Fatalf("the serial node wrote the status: %+v", st)
	}

	// a node with neither end gets no line
	other := &fakeCable{}
	s3, _, _ := ccSync(t, "n3", other, objs...)
	if err := s3.Sync(context.Background()); err != nil || len(other.last.Lines) != 0 || other.calls != 1 {
		t.Fatalf("n3: %v %+v", err, other.last)
	}
}

// No ready device access pod on the serial end's node: the line is not given and the cable says PeerUnreachable; an end without a node:
// WaitingForDevice; a down line carries the service's reason.
func TestConsoleCableSyncReasons(t *testing.T) {
	cable := &fakeCable{}
	s, c, _ := ccSync(t, "n1", cable, ccCable("l1"), ccDevice("r1", "l1", "n1", false), ccDevice("pc1", "l1", "n2", true), daPod("n2", "10.1.0.2", false))
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(cable.last.Lines) != 0 {
		t.Fatalf("a line without a reachable peer was given: %+v", cable.last.Lines)
	}
	if cond := meta.FindStatusCondition(cableStatus(t, c, "l1").Conditions, labstatus.ConditionReady); cond == nil || cond.Reason != wire.LinePeerUnreachable {
		t.Fatalf("condition %+v", cond)
	}

	s, c, _ = ccSync(t, "n1", &fakeCable{}, ccCable("l1"), ccDevice("r1", "l1", "n1", false), ccDevice("pc1", "l1", "", true))
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cond := meta.FindStatusCondition(cableStatus(t, c, "l1").Conditions, labstatus.ConditionReady); cond == nil || cond.Reason != wire.LineWaitingForDevice {
		t.Fatalf("condition %+v", cond)
	}

	down := &fakeCable{state: func(l wire.LineSpec) wire.LineState {
		return wire.LineState{Namespace: l.Namespace, Name: l.Name, UID: l.UID, Role: l.Role, ConsoleAttached: true, Reason: wire.LinePortDown}
	}}
	s, c, _ = ccSync(t, "n1", down, ccCable("l1"), ccDevice("r1", "l1", "n1", false), ccDevice("pc1", "l1", "n1", true))
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := cableStatus(t, c, "l1")
	if cond := meta.FindStatusCondition(st.Conditions, labstatus.ConditionReady); st.LineUp || !st.Console.Attached || st.Serial.Attached || cond == nil || cond.Reason != wire.LinePortDown {
		t.Fatalf("status %+v", st)
	}
}

// The console end moves to another node: the old owner stops giving the line and writes it down once; once the new owner has written
// the cable, the old one leaves it alone.
func TestConsoleCableSyncReschedule(t *testing.T) {
	r1 := ccDevice("r1", "l1", "n1", false)
	cable := &fakeCable{state: upState}
	s, c, _ := ccSync(t, "n1", cable, ccCable("l1"), r1, ccDevice("pc1", "l1", "n1", true))
	ctx := context.Background()
	if err := s.Sync(ctx); err != nil || !cableStatus(t, c, "l1").LineUp {
		t.Fatalf("%v %+v", err, cableStatus(t, c, "l1"))
	}
	var moved laboratoryv1alpha1.Device
	if err := c.Get(ctx, client.ObjectKeyFromObject(r1), &moved); err != nil {
		t.Fatal(err)
	}
	moved.Status.NodeName = "n3"
	if err := c.Update(ctx, &moved); err != nil {
		t.Fatal(err)
	}
	// the operator has not moved the stamp yet: the old owner (still stamped) writes the line down
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if len(cable.last.Lines) != 1 || cable.last.Lines[0].Role != wire.RoleSerial {
		t.Fatalf("n1 keeps only the serial end: %+v", cable.last.Lines)
	}
	st := cableStatus(t, c, "l1")
	if st.LineUp || st.Console.NodeName != "n3" {
		t.Fatalf("the old owner must write the line down: %+v", st)
	}
	// the operator stamps the new node and the new owner writes; the old one does not touch it any more
	var cc laboratoryv1alpha1.ConsoleCable
	_ = c.Get(ctx, types.NamespacedName{Namespace: ccNS, Name: "l1-cable"}, &cc)
	cc.Annotations[names.AnnotationConsoleNode] = "n3"
	if err := c.Update(ctx, &cc); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(ctx, types.NamespacedName{Namespace: ccNS, Name: "l1-cable"}, &cc)
	cc.Status.LineUp = true
	if err := c.Status().Update(ctx, &cc); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if !cableStatus(t, c, "l1").LineUp {
		t.Fatal("the old owner overwrote the new owner's status")
	}
}

// A device access service that does not answer: the relay annotation says so, and nothing is written.
func TestConsoleCableSyncRelayDown(t *testing.T) {
	s, c, relay := ccSync(t, "n1", &fakeCable{err: errors.New("dial unix: no such file")}, ccCable("l1"), ccDevice("r1", "l1", "n1", false), ccDevice("pc1", "l1", "n1", true))
	if err := s.Sync(context.Background()); err == nil {
		t.Fatal("no error")
	}
	if code := RelayCodeOf(*relay, true); code != CodeRelayUnreachable || strings.Contains(*relay, "dial unix") {
		t.Fatalf("relay annotation %q", *relay)
	}
	if st := cableStatus(t, c, "l1"); len(st.Conditions) != 0 {
		t.Fatalf("status written without the service: %+v", st)
	}
}

func TestRelayCodeOf(t *testing.T) {
	for v, want := range map[string]string{
		PreflightReady: "", PreflightDisabled: CodeRelayDisabled, Unavailable(CodeRelayOld, "x"): CodeRelayOld,
		Unavailable(CodeRuntimeSocket, "x"): CodeNodeUnconfirmed, "junk": CodeNodeUnconfirmed,
	} {
		if got := RelayCodeOf(v, true); got != want {
			t.Errorf("%q: %q, want %q", v, got, want)
		}
	}
	if RelayCodeOf("", false) != CodeNodeUnconfirmed {
		t.Error("a node without the annotation")
	}
}

// A node-agent writes only the cables the operator stamped with its node (the admission policy refuses the others): before the stamp
// follows the console end it gives the line but leaves the status alone.
func TestConsoleCableSyncWritesOnlyStampedCables(t *testing.T) {
	cc := ccCable("l1")
	cc.Annotations[names.AnnotationConsoleNode] = "n9"
	cable := &fakeCable{state: upState}
	s, c, _ := ccSync(t, "n1", cable, cc, ccDevice("r1", "l1", "n1", false), ccDevice("pc1", "l1", "n1", true))
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(cable.last.Lines) != 1 {
		t.Fatal("the line is still given")
	}
	if st := cableStatus(t, c, "l1"); len(st.Conditions) != 0 || st.LineUp {
		t.Fatalf("a cable stamped with another node was written: %+v", st)
	}
}

// The lines go in the order of the cables' creation, and a line the service refuses for the limit of the node says NodeLineLimit.
func TestConsoleCableSyncOrderAndNodeLimit(t *testing.T) {
	old, young := ccCable("l1"), ccCable("l2")
	young.CreationTimestamp = metav1.Unix(1_800_000_000, 0)
	cable := &fakeCable{state: func(l wire.LineSpec) wire.LineState {
		if l.Lab == "l2" {
			return wire.LineState{Namespace: l.Namespace, Name: l.Name, UID: l.UID, Role: l.Role, Reason: wire.LineNodeLineLimit}
		}
		return upState(l)
	}}
	s, c, _ := ccSync(t, "n1", cable, young, old, ccDevice("r1", "l1", "n1", false), ccDevice("pc1", "l1", "n1", true),
		ccDevice("r1", "l2", "n1", false), ccDevice("pc1", "l2", "n1", true))
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(cable.last.Lines) != 2 || cable.last.Lines[0].Lab != "l1" || cable.last.Lines[0].Created != 1_700_000_000 {
		t.Fatalf("order %+v", cable.last.Lines)
	}
	cond := meta.FindStatusCondition(cableStatus(t, c, "l2").Conditions, labstatus.ConditionReady)
	if cond == nil || cond.Reason != laboratoryv1alpha1.ConsoleCableReasonNodeLineLimit {
		t.Fatalf("condition %+v", cond)
	}
}

// The node-agent publishes the key the device access process of its node reports, and a peer whose node published no key gets no line.
func TestConsoleCableSyncRelayKeys(t *testing.T) {
	cable := &fakeCable{state: upState}
	cc := ccCable("l1")
	cc.Labels[names.LabelTenant] = "acme"
	s, c, _ := ccSync(t, "n1", cable, cc, ccDevice("r1", "l1", "n1", false), ccDevice("pc1", "l1", "n4", true), daPod("n4", "10.1.0.4", true))
	var published string
	s.RelayKey = func(v string) { published = v }
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if published != relayKeyOf("n1") {
		t.Fatalf("published %q", published)
	}
	if len(cable.last.Lines) != 0 {
		t.Fatalf("a line to a node without a published key: %+v", cable.last.Lines)
	}
	if cond := meta.FindStatusCondition(cableStatus(t, c, "l1").Conditions, labstatus.ConditionReady); cond == nil || cond.Reason != wire.LinePeerUnreachable {
		t.Fatalf("condition %+v", cond)
	}
	// the tenant reaches the service (the line cap is shared per tenant)
	s2, _, _ := ccSync(t, "n1", cable, cc, ccDevice("r1", "l1", "n1", false), ccDevice("pc1", "l1", "n1", true))
	if err := s2.Sync(context.Background()); err != nil || len(cable.last.Lines) != 1 || cable.last.Lines[0].Tenant != "acme" {
		t.Fatalf("%v %+v", err, cable.last.Lines)
	}
}

// The peer address is the canonical form of the pod IP (the device access service takes nothing else); a pod IP that is not an IP is
// skipped.
func TestDeviceAccessPodAddresses(t *testing.T) {
	s, _, _ := ccSync(t, "n1", &fakeCable{}, daPod("n2", "fd00:10:244:0::7", true), daPod("n3", "not-an-ip", true), daPod("n4", "10.1.0.4", true))
	peers, err := s.deviceAccessPods(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if peers["n2"] != "[fd00:10:244::7]:8081" || peers["n4"] != "10.1.0.4:8081" || peers["n3"] != "" {
		t.Fatalf("%v", peers)
	}
}

// The node-agent publishes whether the device access service of its node understands port lists with the kernel log (it answers with
// wire.FeatureKernelLog): an older service would refuse such a list, so the management agent must not offer the field before every node has
// it.
func TestConsoleCableSyncKernelLogAnnotation(t *testing.T) {
	for name, c := range map[string]struct {
		cable *fakeCable
		code  string
	}{
		"a service of this release": {&fakeCable{state: upState}, ""},
		"an older service":          {&fakeCable{state: upState, old: true}, CodeKernelLogUnsupported},
		"no service":                {&fakeCable{err: errors.New("dial unix: no such file")}, CodeRelayUnreachable},
	} {
		s, _, _ := ccSync(t, "n1", c.cable, ccCable("l1"), ccDevice("r1", "l1", "n1", false), ccDevice("pc1", "l1", "n1", true))
		var got string
		s.KernelLog = func(v string) { got = v }
		_ = s.Sync(context.Background())
		if code := KernelLogCodeOf(got, got != ""); code != c.code {
			t.Errorf("%s: annotation %q, code %q, want %q", name, got, code, c.code)
		}
	}
	if KernelLogCodeOf("", false) != CodeNodeUnconfirmed || KernelLogCodeOf("whatever", true) != CodeNodeUnconfirmed || KernelLogCodeOf("ready", true) != "" {
		t.Fatal("KernelLogCodeOf")
	}
}
