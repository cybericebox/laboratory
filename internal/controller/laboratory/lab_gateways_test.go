package laboratory

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	allocationv1alpha1 "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
)

const testLabBase = "10.128.0.0/9"

type gatewayFixture struct {
	c client.Client
	r *LabReconciler
}

func newGatewayFixture(t *testing.T) *gatewayFixture {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{laboratoryv1alpha1.AddToScheme, allocationv1alpha1.AddToScheme, corev1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	c := fake.NewClientBuilder().WithScheme(s).
		WithStatusSubresource(&laboratoryv1alpha1.Lab{}, &allocationv1alpha1.Pool{}, &laboratoryv1alpha1.LabVPN{}, &laboratoryv1alpha1.LabGateway{}).Build()
	size, err := labSubnetPoolSize(testLabBase)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&LabGroupReconciler{Client: c}).ensurePool(context.Background(), "g", names.PoolLabSubnets, poolpkg.PoolTypeLabSubnets, 1, size); err != nil {
		t.Fatal(err)
	}
	return &gatewayFixture{c: c, r: &LabReconciler{Client: c, Scheme: s, LabBaseNetwork: testLabBase}}
}

func (f *gatewayFixture) lab(t *testing.T, name string, mutate func(*laboratoryv1alpha1.Lab)) *laboratoryv1alpha1.Lab {
	t.Helper()
	l := &laboratoryv1alpha1.Lab{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "g", UID: types.UID(name + "-uid")}}
	if mutate != nil {
		mutate(l)
	}
	status := l.Status
	if err := f.c.Create(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	l.Status = status
	if err := f.c.Status().Update(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	return l
}

func vpnGateway(name string) laboratoryv1alpha1.GatewayTemplate {
	return laboratoryv1alpha1.GatewayTemplate{Name: name, Kind: laboratoryv1alpha1.GatewayKindVPN}
}

func internetGateway(name string) laboratoryv1alpha1.GatewayTemplate {
	return laboratoryv1alpha1.GatewayTemplate{Name: name, Kind: laboratoryv1alpha1.GatewayKindInternet}
}

func TestLabSubnetPoolSizeIsTheSizeOfTheNetwork(t *testing.T) {
	for base, want := range map[string]uint{"10.128.0.0/9": 32767, "10.0.0.0/8": 65535, "10.128.0.0/10": 16383, "10.128.0.0/18": 63} {
		if got, err := labSubnetPoolSize(base); err != nil || got != want {
			t.Errorf("%s: %d, %v; want %d", base, got, err, want)
		}
	}
	if _, err := labSubnetPoolSize("10.0.0.0/24"); err == nil {
		t.Error("a /24 has no room for a gateway network besides the clients")
	}
	if _, err := labSubnetPoolSize("junk"); err == nil {
		t.Error("garbage")
	}
}

// Every gateway, VPN or Internet, takes its own number from the one pool; the numbers of one lab are unrelated, and a second call changes nothing.
func TestEveryGatewayGetsItsOwnNumber(t *testing.T) {
	f := newGatewayFixture(t)
	ctx := context.Background()
	l := f.lab(t, "a", func(l *laboratoryv1alpha1.Lab) {
		l.Spec.VPN.Enabled, l.Spec.Internet.Enabled = true, true
		l.Spec.Gateways = []laboratoryv1alpha1.GatewayTemplate{vpnGateway("attack"), internetGateway("out")}
	})
	if updated, err := f.r.ensureSubnetAllocation(ctx, l); err != nil || !updated {
		t.Fatalf("%v %v", updated, err)
	}
	var got laboratoryv1alpha1.Lab
	if err := f.c.Get(ctx, client.ObjectKeyFromObject(l), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		kind laboratoryv1alpha1.GatewayKind
		n    uint
	}{
		"vpn": {laboratoryv1alpha1.GatewayKindVPN, 1}, "internet": {laboratoryv1alpha1.GatewayKindInternet, 2},
		"attack": {laboratoryv1alpha1.GatewayKindVPN, 3}, "out": {laboratoryv1alpha1.GatewayKindInternet, 4},
	}
	if len(got.Status.Gateways) != len(want) {
		t.Fatalf("gateways: %+v", got.Status.Gateways)
	}
	for _, g := range got.Status.Gateways {
		w, ok := want[g.Name]
		if !ok || g.Kind != w.kind || g.NetworkIndex != w.n {
			t.Errorf("%+v, want %+v", g, w)
		}
		cidr, _ := netip.ParsePrefix(g.CIDR)
		if cidr.Addr().As4() != [4]byte{10, 128, byte(w.n), 0} || cidr.Bits() != 24 {
			t.Errorf("%s: CIDR %s for number %d", g.Name, g.CIDR, w.n)
		}
	}
	if got.Status.VPN.CIDR != "10.128.1.0/24" || got.Status.Internet.CIDR != "10.128.2.0/24" {
		t.Errorf("the mirror fields: %q %q", got.Status.VPN.CIDR, got.Status.Internet.CIDR)
	}
	if updated, err := f.r.ensureSubnetAllocation(ctx, &got); err != nil || updated {
		t.Fatalf("a second pass must change nothing: %v %v", updated, err)
	}
	// The next lab continues the numbers.
	m := f.lab(t, "b", func(l *laboratoryv1alpha1.Lab) {
		l.Spec.Gateways = []laboratoryv1alpha1.GatewayTemplate{vpnGateway("only")}
	})
	if _, err := f.r.ensureSubnetAllocation(ctx, m); err != nil {
		t.Fatal(err)
	}
	if g := m.Status.GatewayStatus("only"); g == nil || g.NetworkIndex != 5 {
		t.Fatalf("%+v", m.Status.Gateways)
	}
}

// A lab made before the one pool keeps its networks and its numbers; what is new is taken from the free numbers.
func TestALabFromBeforeTheOnePoolKeepsItsNetworks(t *testing.T) {
	f := newGatewayFixture(t)
	ctx := context.Background()
	old := f.lab(t, "old", func(l *laboratoryv1alpha1.Lab) {
		l.Spec.VPN.Enabled, l.Spec.Internet.Enabled = true, true
		l.Status.VPN.CIDR, l.Status.Internet.CIDR = "10.128.1.0/24", "10.192.1.0/24"
		l.Spec.Gateways = []laboratoryv1alpha1.GatewayTemplate{vpnGateway("extra")}
	})
	if _, err := f.r.ensureSubnetAllocation(ctx, old); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]struct {
		cidr string
		n    uint
	}{"vpn": {"10.128.1.0/24", 1}, "internet": {"10.192.1.0/24", 1}} {
		g := old.Status.GatewayStatus(name)
		if g == nil || g.CIDR != want.cidr || g.NetworkIndex != want.n {
			t.Errorf("%s: %+v, want %+v", name, g, want)
		}
	}
	extra := old.Status.GatewayStatus("extra")
	if extra == nil || extra.NetworkIndex < 2 || extra.CIDR == "" {
		t.Fatalf("a new gateway takes a free number: %+v", extra)
	}
	if old.Status.VPN.CIDR != "10.128.1.0/24" || old.Status.Internet.CIDR != "10.192.1.0/24" {
		t.Fatal("the old fields are untouched")
	}
}

// What is in use is the position of every /24 and the number of every leg: for an old Internet gateway they differ.
func TestSubnetsInUseCountsPositionsAndLegNumbers(t *testing.T) {
	f := newGatewayFixture(t)
	ctx := context.Background()
	f.lab(t, "old", func(l *laboratoryv1alpha1.Lab) {
		l.Status.VPN.CIDR, l.Status.Internet.CIDR = "10.128.5.0/24", "10.192.5.0/24"
	})
	f.lab(t, "new", func(l *laboratoryv1alpha1.Lab) {
		l.Status.Gateways = []laboratoryv1alpha1.GatewayStatus{{Name: "attack", Kind: laboratoryv1alpha1.GatewayKindVPN, CIDR: "10.128.9.0/24", NetworkIndex: 9}}
	})
	if err := f.c.Create(ctx, &laboratoryv1alpha1.LabGateway{ObjectMeta: metav1.ObjectMeta{Name: "labgw-old", Namespace: "g"}, Spec: laboratoryv1alpha1.LabGatewaySpec{LabName: "old", NetworkIndex: 5}}); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Create(ctx, &laboratoryv1alpha1.LabVPN{ObjectMeta: metav1.ObjectMeta{Name: "labvpn-x", Namespace: "g"}, Spec: laboratoryv1alpha1.LabVPNSpec{LabName: "x", NetworkIndex: 77}}); err != nil {
		t.Fatal(err)
	}
	used, err := f.r.subnetsInUse("g")(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []uint{5, 16384 + 5, 9, 77} {
		if !used[n] {
			t.Errorf("%d must be in use: %v", n, used)
		}
	}
	if used[6] {
		t.Error("6 is free")
	}
}

func TestObjectsAndDHCPPoolsPerGateway(t *testing.T) {
	f := newGatewayFixture(t)
	ctx := context.Background()
	dhcp := &laboratoryv1alpha1.DHCPServer{Enabled: true, Ranges: []laboratoryv1alpha1.DHCPRange{{Start: 2, End: 9}}}
	l := f.lab(t, "a", func(l *laboratoryv1alpha1.Lab) {
		l.Spec.VPN = laboratoryv1alpha1.LabNetworkSpec{Enabled: true, DHCPServer: dhcp}
		l.Spec.Gateways = []laboratoryv1alpha1.GatewayTemplate{
			{Name: "attack", Kind: laboratoryv1alpha1.GatewayKindVPN, DHCPServer: dhcp},
			internetGateway("out"),
		}
	})
	if _, err := f.r.ensureSubnetAllocation(ctx, l); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // twice: nothing is created twice
		if err := f.r.ensureLabNetworkObjects(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	var vpns laboratoryv1alpha1.LabVPNList
	var gws laboratoryv1alpha1.LabGatewayList
	_ = f.c.List(ctx, &vpns)
	_ = f.c.List(ctx, &gws)
	if len(vpns.Items) != 2 || len(gws.Items) != 1 {
		t.Fatalf("objects: %d LabVPN, %d LabGateway", len(vpns.Items), len(gws.Items))
	}
	var shorthand, declared laboratoryv1alpha1.LabVPN
	if err := f.c.Get(ctx, types.NamespacedName{Namespace: "g", Name: "labvpn-a"}, &shorthand); err != nil || shorthand.Spec.Device != "" || shorthand.Spec.NetworkIndex != 1 {
		t.Fatalf("the shorthand gateway keeps its name: %+v %v", shorthand.Spec, err)
	}
	if err := f.c.Get(ctx, types.NamespacedName{Namespace: "g", Name: "labvpn-a.attack"}, &declared); err != nil || declared.Spec.Device != "attack" || declared.Spec.LabName != "a" || declared.Spec.NetworkIndex != 2 {
		t.Fatalf("a declared gateway: %+v %v", declared.Spec, err)
	}
	var out laboratoryv1alpha1.LabGateway
	if err := f.c.Get(ctx, types.NamespacedName{Namespace: "g", Name: "labgw-a.out"}, &out); err != nil || out.Spec.Device != "out" || out.Spec.NetworkIndex != 3 {
		t.Fatalf("a declared Internet gateway: %+v %v", out.Spec, err)
	}
	for _, pool := range []string{"dhcp-vpn-a-0", "dhcp-vpn-a.attack-0"} {
		var p allocationv1alpha1.Pool
		if err := f.c.Get(ctx, types.NamespacedName{Namespace: "g", Name: pool}, &p); err != nil {
			t.Errorf("pool %s: %v", pool, err)
		}
	}
	var none allocationv1alpha1.Pool
	if err := f.c.Get(ctx, types.NamespacedName{Namespace: "g", Name: "dhcp-inet-a.out-0"}, &none); err == nil {
		t.Error("a gateway without DHCP gets no pool")
	}
}

func TestGatewayObjectsAreDeletedWithTheLab(t *testing.T) {
	f := newGatewayFixture(t)
	ctx := context.Background()
	l := f.lab(t, "a", func(l *laboratoryv1alpha1.Lab) {
		l.Spec.VPN.Enabled = true
		l.Spec.Gateways = []laboratoryv1alpha1.GatewayTemplate{vpnGateway("attack"), internetGateway("out")}
	})
	other := f.lab(t, "b", func(l *laboratoryv1alpha1.Lab) {
		l.Spec.Gateways = []laboratoryv1alpha1.GatewayTemplate{vpnGateway("attack")}
	})
	for _, lab := range []*laboratoryv1alpha1.Lab{l, other} {
		if _, err := f.r.ensureSubnetAllocation(ctx, lab); err != nil {
			t.Fatal(err)
		}
		if err := f.r.ensureLabNetworkObjects(ctx, lab); err != nil {
			t.Fatal(err)
		}
	}
	// Each pass deletes, strips the controller finalizer, and finally sees nothing of this lab.
	var done bool
	for i := 0; i < 5 && !done; i++ {
		var err error
		if done, err = f.r.ensureGatewaysDeleted(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	if !done {
		t.Fatal("the objects of the lab must go")
	}
	var vpns laboratoryv1alpha1.LabVPNList
	_ = f.c.List(ctx, &vpns)
	if len(vpns.Items) != 1 || vpns.Items[0].Spec.LabName != "b" {
		t.Fatalf("another lab's object must stay: %+v", vpns.Items)
	}
}

func TestGatewayReadinessIsPerGateway(t *testing.T) {
	f := newGatewayFixture(t)
	ctx := context.Background()
	l := f.lab(t, "a", func(l *laboratoryv1alpha1.Lab) {
		l.Spec.VPN.Enabled = true
		l.Spec.Gateways = []laboratoryv1alpha1.GatewayTemplate{vpnGateway("attack"), internetGateway("out")}
	})
	if _, err := f.r.ensureSubnetAllocation(ctx, l); err != nil {
		t.Fatal(err)
	}
	if err := f.r.ensureLabNetworkObjects(ctx, l); err != nil {
		t.Fatal(err)
	}
	got, err := f.r.readGatewayReadiness(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	if got.AllReady || got.VPNReady || len(got.Status) != 3 || got.Status[1].CIDR == "" || got.Status[1].NetworkIndex != 2 {
		t.Fatalf("nothing is ready yet: %+v", got)
	}
	ready := func(name string) {
		var o laboratoryv1alpha1.LabVPN
		if err := f.c.Get(ctx, types.NamespacedName{Namespace: "g", Name: name}, &o); err != nil {
			t.Fatal(err)
		}
		o.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: metav1.Now()}}
		o.Status.LabInitiatedBlocked, o.Status.LabInitiatedDropped = true, 4
		if err := f.c.Status().Update(ctx, &o); err != nil {
			t.Fatal(err)
		}
	}
	ready("labvpn-a.attack")
	got, _ = f.r.readGatewayReadiness(ctx, l)
	if got.AllReady || got.VPNReady || !got.Status[1].Ready || !got.Status[1].LabInitiatedBlocked || got.Dropped != 4 || !got.Blocked {
		t.Fatalf("the declared VPN gateway alone: %+v", got)
	}
	ready("labvpn-a")
	got, _ = f.r.readGatewayReadiness(ctx, l)
	if !got.VPNReady || !got.LabInitiatedBlocked || got.Dropped != 8 || got.AllReady {
		t.Fatalf("the shorthand mirror follows the shorthand gateway: %+v", got)
	}
}

func TestValidateGatewaysHonoursTheConfiguredLimit(t *testing.T) {
	l := &laboratoryv1alpha1.Lab{}
	for _, name := range []string{"a", "b", "c"} {
		l.Spec.Gateways = append(l.Spec.Gateways, vpnGateway(name))
	}
	if err := (&LabReconciler{}).validateGateways(l); err != nil {
		t.Fatalf("under the default limit: %v", err)
	}
	if err := (&LabReconciler{MaxLabGateways: 2}).validateGateways(l); err == nil || !strings.Contains(err.Error(), "TooManyGateways") {
		t.Fatalf("over a configured limit of 2: %v", err)
	}
}

func gatewayGraphLab() *laboratoryv1alpha1.Lab {
	return &laboratoryv1alpha1.Lab{Spec: laboratoryv1alpha1.LabSpec{
		Gateways: []laboratoryv1alpha1.GatewayTemplate{vpnGateway("attack"), vpnGateway("user"), internetGateway("out")},
		Devices: []laboratoryv1alpha1.DeviceTemplate{
			{Name: "r1", Type: laboratoryv1alpha1.DeviceTypeContainer, Interfaces: []laboratoryv1alpha1.InterfaceSpec{{Name: "eth0"}, {Name: "eth1"}}},
			{Name: "r2", Type: laboratoryv1alpha1.DeviceTypeContainer, Interfaces: []laboratoryv1alpha1.InterfaceSpec{{Name: "eth0"}}},
			{Name: "sw", Type: laboratoryv1alpha1.DeviceTypeUnmanagedSwitch},
		},
	}}
}

func TestDeclaredGatewaysAreEndpointsOfConnections(t *testing.T) {
	l := gatewayGraphLab()
	l.Spec.Connections = []laboratoryv1alpha1.ConnectionTemplate{
		{Endpoints: []laboratoryv1alpha1.EndpointSpec{endpoint("attack", "eth0"), endpoint("r1", "eth0")}},
		{Endpoints: []laboratoryv1alpha1.EndpointSpec{endpoint("user", "eth0"), endpoint("r2", "eth0")}},
		{Endpoints: []laboratoryv1alpha1.EndpointSpec{endpoint("out", "eth0"), endpoint("r1", "eth1")}},
	}
	if err := (&LabReconciler{}).validateGraph(l); err != nil {
		t.Fatalf("each gateway in a network of its own: %v", err)
	}
	for name, tc := range map[string]struct {
		conns []laboratoryv1alpha1.ConnectionTemplate
		want  string
	}{
		"a port other than eth0": {[]laboratoryv1alpha1.ConnectionTemplate{{Endpoints: []laboratoryv1alpha1.EndpointSpec{endpoint("attack", "eth1"), endpoint("r1", "eth0")}}}, "InvalidGatewayPort"},
		"a gateway port twice": {[]laboratoryv1alpha1.ConnectionTemplate{
			{Endpoints: []laboratoryv1alpha1.EndpointSpec{endpoint("attack", "eth0"), endpoint("r1", "eth0")}},
			{Endpoints: []laboratoryv1alpha1.EndpointSpec{endpoint("attack", "eth0"), endpoint("r2", "eth0")}},
		}, "DuplicateGatewayPort"},
		"two gateways in one network (through a switch)": {[]laboratoryv1alpha1.ConnectionTemplate{
			{Endpoints: []laboratoryv1alpha1.EndpointSpec{endpoint("attack", "eth0"), endpoint("sw", "GigabitEthernet0/1")}},
			{Endpoints: []laboratoryv1alpha1.EndpointSpec{endpoint("out", "eth0"), endpoint("sw", "GigabitEthernet0/2")}},
		}, "BroadcastDomainHasSeveralGateways"},
		"two gateways joined directly": {[]laboratoryv1alpha1.ConnectionTemplate{{Endpoints: []laboratoryv1alpha1.EndpointSpec{endpoint("attack", "eth0"), endpoint("user", "eth0")}}}, "BroadcastDomainHasSeveralGateways"},
		"an unknown device":            {[]laboratoryv1alpha1.ConnectionTemplate{{Endpoints: []laboratoryv1alpha1.EndpointSpec{endpoint("ghost", "eth0"), endpoint("r1", "eth0")}}}, "UnknownEndpointDevice"},
	} {
		l := gatewayGraphLab()
		l.Spec.Connections = tc.conns
		if err := (&LabReconciler{}).validateGraph(l); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

func TestTwoDHCPServersInOneNetworkAreStillRefused(t *testing.T) {
	l := gatewayGraphLab()
	l.Spec.Gateways[0].DHCPServer = &laboratoryv1alpha1.DHCPServer{Enabled: true}
	l.Spec.Devices[0].Interfaces = []laboratoryv1alpha1.InterfaceSpec{{Name: "eth0"}}
	l.Spec.Connections = []laboratoryv1alpha1.ConnectionTemplate{
		{Endpoints: []laboratoryv1alpha1.EndpointSpec{endpoint("attack", "eth0"), endpoint("r1", "eth0")}},
	}
	if err := (&LabReconciler{}).validateGraph(l); err != nil {
		t.Fatalf("one DHCP source: %v", err)
	}
}

func TestAddressRefsNameDeclaredGateways(t *testing.T) {
	l := gatewayGraphLab()
	l.Status.Gateways = []laboratoryv1alpha1.GatewayStatus{
		{Name: "attack", CIDR: "10.128.3.0/24"}, {Name: "user", CIDR: "10.128.4.0/24"}, {Name: "out", CIDR: "10.128.5.0/24"},
	}
	ip := func(network, gateway string, host int32) (string, error) {
		return resolveNetworkIP(l, laboratoryv1alpha1.NetworkIPRef{Network: network, Gateway: gateway, Host: host}, true)
	}
	if got, err := ip("", "attack", 10); err != nil || got != "10.128.3.10" {
		t.Fatalf("%q %v", got, err)
	}
	if got, err := ip("", "user", 10); err != nil || got != "10.128.4.10" {
		t.Fatalf("each gateway has its own network: %q %v", got, err)
	}
	if _, err := ip("", "ghost", 10); err == nil || !strings.Contains(err.Error(), "unknown gateway") {
		t.Fatalf("%v", err)
	}
	if _, err := ip("vpn", "", 10); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("spec.vpn is off: %v", err)
	}
	if _, err := ip("vpn", "attack", 10); err == nil {
		t.Fatal("both is refused")
	}
	if _, err := ip("", "", 10); err == nil {
		t.Fatal("neither is refused")
	}
	prefix, err := resolveNetworkPrefix(l, "", "out")
	if err != nil || prefix.String() != "10.128.5.0/24" {
		t.Fatalf("%v %v", prefix, err)
	}
}

// Every VPN gateway of a lab gives the devices on ITS network the way back to the participants.
func TestReturnRoutePerVPNGateway(t *testing.T) {
	l := gatewayGraphLab()
	l.Status.Gateways = []laboratoryv1alpha1.GatewayStatus{
		{Name: "attack", CIDR: "10.128.3.0/24"}, {Name: "user", CIDR: "10.128.4.0/24"}, {Name: "out", CIDR: "10.128.5.0/24"},
	}
	l.Spec.Connections = []laboratoryv1alpha1.ConnectionTemplate{
		{Endpoints: []laboratoryv1alpha1.EndpointSpec{endpoint("attack", "eth0"), endpoint("r1", "eth0")}},
		{Endpoints: []laboratoryv1alpha1.EndpointSpec{endpoint("user", "eth0"), endpoint("sw", "GigabitEthernet0/1")}},
		{Endpoints: []laboratoryv1alpha1.EndpointSpec{endpoint("r2", "eth0"), endpoint("sw", "GigabitEthernet0/2")}},
		{Endpoints: []laboratoryv1alpha1.EndpointSpec{endpoint("out", "eth0"), endpoint("r1", "eth1")}},
	}
	resolved := map[string][]laboratoryv1alpha1.InterfaceSpec{
		"r1": {staticIface("eth0", "10.128.3.10/24", ""), staticIface("eth1", "10.128.5.10/24", "")},
		"r2": {staticIface("eth0", "10.128.4.10/24", "")},
	}
	addVPNReturnRoutes(l, resolved, netip.MustParsePrefix("10.128.0.0/24"))
	check := func(device, iface, via string) {
		t.Helper()
		for _, i := range resolved[device] {
			if i.Name != iface {
				continue
			}
			if via == "" && len(i.Addr.Routes) != 0 {
				t.Errorf("%s/%s: routes %v, want none", device, iface, i.Addr.Routes)
			}
			if via != "" && (len(i.Addr.Routes) != 1 || i.Addr.Routes[0].Via != via || i.Addr.Routes[0].Dst != "10.128.0.0/24") {
				t.Errorf("%s/%s: routes %v, want via %s", device, iface, i.Addr.Routes, via)
			}
		}
	}
	check("r1", "eth0", "10.128.3.1")
	check("r2", "eth0", "10.128.4.1")
	check("r1", "eth1", "") // the network of the Internet gateway needs no way back to clients
}
