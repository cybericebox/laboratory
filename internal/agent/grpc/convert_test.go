package grpc

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func TestLabToProtoIncludesConfiguredResourceBounds(t *testing.T) {
	lab := &laboratoryv1alpha1.Lab{
		Spec: laboratoryv1alpha1.LabSpec{Devices: []laboratoryv1alpha1.DeviceTemplate{{
			Name:      "web",
			Resources: &laboratoryv1alpha1.DeviceResources{CPURequest: "250m", MemoryRequest: "64Mi", CPULimit: "1", MemoryLimit: "128Mi"},
		}}},
		Status: laboratoryv1alpha1.LabStatus{Devices: []laboratoryv1alpha1.DeviceRef{{Name: "web"}}},
	}

	got := labToProto(lab).GetStatus().GetDevices()[0]
	if got.GetCpuRequestMillicores() != 250 || got.GetMemoryRequestBytes() != 64*1024*1024 || got.GetCpuLimitMillicores() != 1000 || got.GetMemoryLimitBytes() != 128*1024*1024 {
		t.Fatalf("resource bounds = %+v", got)
	}
}

func TestLabToProtoStatus(t *testing.T) {
	lab := &laboratoryv1alpha1.Lab{}
	lab.Name = "ctf1"
	lab.Namespace = "team-alpha"
	lab.Status.Phase = laboratoryv1alpha1.PhaseReady
	lab.Status.VPN.CIDR = "10.128.1.0/24"
	lab.Status.VPN.Ready = true
	lab.Status.Internet.Ready = true
	lab.Status.Devices = []laboratoryv1alpha1.DeviceRef{
		{Name: "attacker", Ready: true},
		{Name: "victim", Ready: false},
	}
	lab.Status.Connections = []laboratoryv1alpha1.ConnectionRef{
		{Name: "link-0", Ready: true},
	}
	lab.Status.Access = []laboratoryv1alpha1.AccessEntry{
		{Device: "web", Port: 80, Protocol: "http", URL: "https://web.lab.test"},
	}

	p := labToProto(lab)
	if p.Name != "ctf1" || p.Namespace != "team-alpha" {
		t.Errorf("name/ns wrong: %+v", p)
	}
	if p.Status.Phase != "Ready" || p.Status.VpnCidr != "10.128.1.0/24" {
		t.Errorf("status wrong: %+v", p.Status)
	}
	if !p.Status.VpnReady || !p.Status.InternetReady {
		t.Errorf("network readiness lost: %+v", p.Status)
	}
	if len(p.Status.Devices) != 2 || p.Status.Devices[0].Name != "attacker" || !p.Status.Devices[0].Ready || p.Status.Devices[1].Ready {
		t.Errorf("devices wrong: %+v", p.Status.Devices)
	}
	if len(p.Status.Connections) != 1 || p.Status.Connections[0].Name != "link-0" || !p.Status.Connections[0].Ready {
		t.Errorf("connections wrong: %+v", p.Status.Connections)
	}
	if len(p.Status.Access) != 1 || p.Status.Access[0].Device != "web" || p.Status.Access[0].Port != 80 || p.Status.Access[0].Url != "https://web.lab.test" {
		t.Errorf("access wrong: %+v", p.Status.Access)
	}
	if len(p.Status.AccessUrls) != 1 || p.Status.AccessUrls[0] != "https://web.lab.test" {
		t.Errorf("access_urls wrong: %+v", p.Status.AccessUrls)
	}
}

func TestProtoToLabRoundtrip(t *testing.T) {
	in := &laboratoryv1alpha1.Lab{}
	in.Name = "ctf1"
	in.Namespace = "team-alpha"
	in.Spec.VPN.Enabled = true

	pb := labToProto(in)
	out, err := protoToLab(pb)
	if err != nil {
		t.Fatalf("protoToLab: %v", err)
	}
	if out.Name != "ctf1" || !out.Spec.VPN.Enabled {
		t.Errorf("roundtrip lost data: %+v", out.Spec)
	}
}

func TestLabGroupToProto(t *testing.T) {
	g := &laboratoryv1alpha1.LabGroup{}
	g.Name = "group1"
	g.Status.Phase = laboratoryv1alpha1.PhaseReady
	g.Status.Namespace = "team-alpha"
	g.Status.VPN.Registered = true

	p := labGroupToProto(g)
	if p.Name != "group1" {
		t.Errorf("name wrong: %+v", p)
	}
	if p.Status.Phase != "Ready" || p.Status.Namespace != "team-alpha" || !p.Status.VpnRegistered {
		t.Errorf("status wrong: %+v", p.Status)
	}
}

func TestClientToProto(t *testing.T) {
	c := &laboratoryv1alpha1.LabGroupClient{}
	c.Name = "client1"
	c.Namespace = "team-alpha"
	c.Spec.PublicKey = "pubkey123"
	c.Status.AssignedIP = "10.8.0.5/32"
	c.Status.Config = "wireguard-config"
	c.Status.Statistics = laboratoryv1alpha1.LabGroupClientStatistics{
		LastHandshake: metav1.Unix(1700000000, 0),
		RxBytes:       4096,
		TxBytes:       8192,
	}

	p := clientToProto(c)
	if p.Name != "client1" || p.Namespace != "team-alpha" || p.PublicKey != "pubkey123" {
		t.Errorf("client fields wrong: %+v", p)
	}
	if p.Status.AssignedIp != "10.8.0.5/32" {
		t.Errorf("status wrong: %+v", p.Status)
	}
	if !p.Status.Ready {
		t.Errorf("expected ready true when AssignedIP set: %+v", p.Status)
	}
	if p.Status.Config != "wireguard-config" {
		t.Errorf("config wrong: %+v", p.Status)
	}
	if p.Status.Statistics == nil {
		t.Fatalf("expected statistics, got nil")
	}
	if p.Status.Statistics.RxBytes != 4096 || p.Status.Statistics.TxBytes != 8192 {
		t.Errorf("stats bytes wrong: %+v", p.Status.Statistics)
	}
	if p.Status.Statistics.LastHandshakeUnix != 1700000000 {
		t.Errorf("last handshake wrong: %+v", p.Status.Statistics)
	}
}

// A never-connected client must report a zero handshake (not a negative/garbage
// unix stamp from a zero metav1.Time).
func TestClientToProtoZeroHandshake(t *testing.T) {
	c := &laboratoryv1alpha1.LabGroupClient{}
	c.Name = "fresh"
	c.Namespace = "team-alpha"

	p := clientToProto(c)
	if p.Status.Statistics == nil {
		t.Fatalf("expected statistics, got nil")
	}
	if p.Status.Statistics.LastHandshakeUnix != 0 {
		t.Errorf("expected 0 handshake for fresh client, got %d", p.Status.Statistics.LastHandshakeUnix)
	}
	if p.Status.Statistics.RxBytes != 0 || p.Status.Statistics.TxBytes != 0 {
		t.Errorf("expected zero bytes, got %+v", p.Status.Statistics)
	}
}
