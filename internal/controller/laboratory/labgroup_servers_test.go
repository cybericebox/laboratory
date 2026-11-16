package laboratory

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	allocation "github.com/cybericebox/laboratory/api/allocation/v1alpha1"
	api "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

func boolPtr(b bool) *bool { return &b }

// serversFixture is a group (with the given switches) and its reconciler on a fake client.
func serversFixture(t *testing.T, scheduled bool, spec api.LabGroupSpec) (*LabGroupReconciler, client.Client, *api.LabGroup) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = api.AddToScheme(scheme)
	_ = allocation.AddToScheme(scheme)
	group := &api.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "servers"}, Spec: spec}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(group).WithStatusSubresource(&api.LabGroup{}, &allocation.Pool{}, &appsv1.Deployment{}).Build()
	r := &LabGroupReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(1000), LabBaseNetwork: "10.128.0.0/10",
		VPNImage: "lab:v1", GatewayImage: "lab:v1", SupportEmail: "help@example.com", Scheduled: scheduled}
	return r, c, group
}

func reconcileServers(t *testing.T, r *LabGroupReconciler, g *api.LabGroup) api.LabGroup {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(g)}); err != nil {
		t.Fatal(err)
	}
	var got api.LabGroup
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(g), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func deployed(t *testing.T, c client.Client, ns, name string) bool {
	t.Helper()
	var d appsv1.Deployment
	err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &d)
	if errors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

func setSwitches(t *testing.T, c client.Client, g *api.LabGroup, vpn, internet *bool) {
	t.Helper()
	var cur api.LabGroup
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(g), &cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.VPN.Enabled, cur.Spec.Gateway.Enabled = vpn, internet
	if err := c.Update(context.Background(), &cur); err != nil {
		t.Fatal(err)
	}
}

// A group's server pods exist only while their switches are on: off means no Deployment (and no scheduling record), on makes it, off
// again removes it, and the group is Ready without a VPN server.
func TestGroupServersFollowTheirSwitches(t *testing.T) {
	r, c, group := serversFixture(t, false, api.LabGroupSpec{VPN: api.LabGroupVPNSpec{Enabled: boolPtr(false)}, Gateway: api.LabGroupGatewaySpec{Enabled: boolPtr(false)}})
	ns := api.LabGroupNamespace(group.Name)

	got := reconcileServers(t, r, group)
	if deployed(t, c, ns, names.ComponentVPN) || deployed(t, c, ns, names.ComponentGateway) {
		t.Fatal("both servers off: no Deployment")
	}
	if got.Status.Phase != api.PhaseReady || got.Status.VPN.Registered {
		t.Fatalf("a group with its VPN off is Ready and not registered: %s %v", got.Status.Phase, got.Status.VPN.Registered)
	}
	if got.Status.VPN.PublicKey == "" || got.Status.VPN.ClientSubnet == "" {
		t.Errorf("the group keeps its keypair and subnet, so turning the VPN on later changes nothing for clients: %+v", got.Status.VPN)
	}

	setSwitches(t, c, group, boolPtr(true), boolPtr(false))
	reconcileServers(t, r, group)
	if !deployed(t, c, ns, names.ComponentVPN) || deployed(t, c, ns, names.ComponentGateway) {
		t.Fatal("the VPN switch on makes the VPN pod only")
	}
	setSwitches(t, c, group, boolPtr(true), boolPtr(true))
	reconcileServers(t, r, group)
	if !deployed(t, c, ns, names.ComponentVPN) || !deployed(t, c, ns, names.ComponentGateway) {
		t.Fatal("both on: both pods")
	}
	setSwitches(t, c, group, boolPtr(false), boolPtr(false))
	got = reconcileServers(t, r, group)
	if deployed(t, c, ns, names.ComponentVPN) || deployed(t, c, ns, names.ComponentGateway) {
		t.Fatal("both off again: both removed")
	}
	if got.Status.VPN.Registered || got.Status.Phase != api.PhaseReady {
		t.Fatalf("after turning the VPN off: %s registered=%v", got.Status.Phase, got.Status.VPN.Registered)
	}
}

// A group made before the switches (nothing set) keeps both pods; the old disabled still turns the VPN off; Enabled wins over it.
func TestGroupServersMigration(t *testing.T) {
	for name, tc := range map[string]struct {
		spec         api.LabGroupSpec
		vpn, gateway bool
	}{
		"nothing set":                 {api.LabGroupSpec{}, true, true},
		"old disabled":                {api.LabGroupSpec{VPN: api.LabGroupVPNSpec{Disabled: true}}, false, true},
		"enabled wins over disabled":  {api.LabGroupSpec{VPN: api.LabGroupVPNSpec{Disabled: true, Enabled: boolPtr(true)}}, true, true},
		"enabled off, disabled false": {api.LabGroupSpec{VPN: api.LabGroupVPNSpec{Enabled: boolPtr(false)}}, false, true},
		"gateway explicitly off":      {api.LabGroupSpec{Gateway: api.LabGroupGatewaySpec{Enabled: boolPtr(false)}}, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			r, c, group := serversFixture(t, false, tc.spec)
			reconcileServers(t, r, group)
			ns := api.LabGroupNamespace(group.Name)
			if got := deployed(t, c, ns, names.ComponentVPN); got != tc.vpn {
				t.Errorf("VPN pod %v, want %v", got, tc.vpn)
			}
			if got := deployed(t, c, ns, names.ComponentGateway); got != tc.gateway {
				t.Errorf("gateway pod %v, want %v", got, tc.gateway)
			}
		})
	}
}

// With the scheduler on, a server switched on again is queued afresh: the record of the pod that was removed is dropped, so the pod does
// not start ahead of the scheduler on an old "started" record.
func TestServerSwitchedOnAgainIsQueuedAgain(t *testing.T) {
	ctx := context.Background()
	r, c, group := serversFixture(t, true, api.LabGroupSpec{VPN: api.LabGroupVPNSpec{Enabled: boolPtr(false)}, Gateway: api.LabGroupGatewaySpec{Enabled: boolPtr(true)}})
	ns := api.LabGroupNamespace(group.Name)
	record := func(g api.LabGroup, name string) *api.NamedPodSchedule {
		for i := range g.Status.Pods {
			if g.Status.Pods[i].Name == name {
				return &g.Status.Pods[i]
			}
		}
		return nil
	}
	got := reconcileServers(t, r, group)
	if record(got, names.ComponentVPN) != nil || record(got, names.ComponentGateway) == nil || got.Status.Pods[0].State != api.PodQueued {
		t.Fatalf("only the gateway is queued: %+v", got.Status.Pods)
	}
	for i := range got.Status.Pods {
		got.Status.Pods[i].State = api.PodStarted
	}
	if err := c.Status().Update(ctx, &got); err != nil {
		t.Fatal(err)
	}
	reconcileServers(t, r, group)
	if !deployed(t, c, ns, names.ComponentGateway) {
		t.Fatal("a started pod has its Deployment")
	}
	setSwitches(t, c, group, boolPtr(false), boolPtr(false))
	got = reconcileServers(t, r, group)
	if deployed(t, c, ns, names.ComponentGateway) || record(got, names.ComponentGateway) != nil {
		t.Fatalf("switched off: no pod, no record: %+v", got.Status.Pods)
	}
	setSwitches(t, c, group, boolPtr(false), boolPtr(true))
	got = reconcileServers(t, r, group)
	if rec := record(got, names.ComponentGateway); rec == nil || rec.State != api.PodQueued || deployed(t, c, ns, names.ComponentGateway) {
		t.Fatalf("switched on again: queued afresh, not started: %+v", got.Status.Pods)
	}
}

// A lab that declares a connector whose server its group has switched off is refused with the reason; the same lab is fine once the
// switch is on; a lab with no connector, and a lab outside any group namespace, are not checked.
func TestLabIsRefusedWhileItsGroupServerIsOff(t *testing.T) {
	ctx := context.Background()
	r, c, group := serversFixture(t, false, api.LabGroupSpec{VPN: api.LabGroupVPNSpec{Enabled: boolPtr(false)}, Gateway: api.LabGroupGatewaySpec{Enabled: boolPtr(false)}})
	reconcileServers(t, r, group) // makes the namespace with its group label
	ns := api.LabGroupNamespace(group.Name)
	lr := &LabReconciler{Client: c}
	lab := func(ns string, kinds ...api.GatewayKind) *api.Lab {
		l := &api.Lab{ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: ns}}
		for i, k := range kinds {
			l.Spec.Gateways = append(l.Spec.Gateways, api.GatewayTemplate{Name: string(k) + string(rune('a'+i)), Kind: k})
		}
		return l
	}
	check := func(l *api.Lab) string {
		t.Helper()
		off, err := lr.groupServerOff(ctx, l)
		if err != nil {
			t.Fatal(err)
		}
		if off == nil {
			return ""
		}
		return off.Error()
	}
	if msg := check(lab(ns, api.GatewayKindVPN)); !strings.HasPrefix(msg, "GroupVPNOff:") {
		t.Errorf("VPN gateway, VPN off: %q", msg)
	}
	if msg := check(lab(ns, api.GatewayKindInternet)); !strings.HasPrefix(msg, "GroupInternetOff:") {
		t.Errorf("Internet gateway, internet off: %q", msg)
	}
	if msg := check(lab(ns)); msg != "" {
		t.Errorf("a lab with no connector is fine: %q", msg)
	}
	if msg := check(lab("not-a-group", api.GatewayKindVPN)); msg != "" {
		t.Errorf("outside a group namespace nothing is checked: %q", msg)
	}
	setSwitches(t, c, group, boolPtr(true), boolPtr(false))
	if msg := check(lab(ns, api.GatewayKindVPN)); msg != "" {
		t.Errorf("VPN on: %q", msg)
	}
	if msg := check(lab(ns, api.GatewayKindInternet)); !strings.HasPrefix(msg, "GroupInternetOff:") {
		t.Errorf("internet still off: %q", msg)
	}
	setSwitches(t, c, group, nil, nil) // a group made before the switches: both on
	if msg := check(lab(ns, api.GatewayKindVPN, api.GatewayKindInternet)); msg != "" {
		t.Errorf("a legacy group runs both servers: %q", msg)
	}
}
