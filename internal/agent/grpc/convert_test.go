package grpc

import (
	"testing"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func TestLabToProtoStatus(t *testing.T) {
	lab := &laboratoryv1alpha1.Lab{}
	lab.Name = "ctf1"
	lab.Namespace = "team-alpha"
	lab.Status.Phase = laboratoryv1alpha1.PhaseReady
	lab.Status.VPN.CIDR = "10.128.1.0/24"

	p := labToProto(lab)
	if p.Name != "ctf1" || p.Namespace != "team-alpha" {
		t.Errorf("name/ns wrong: %+v", p)
	}
	if p.Status.Phase != "Ready" || p.Status.VpnCidr != "10.128.1.0/24" {
		t.Errorf("status wrong: %+v", p.Status)
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
	c.Status.SecretRef = "team-alpha/client-client1"

	wgConf := []byte("wireguard-config")
	p := clientToProto(c, wgConf)
	if p.Name != "client1" || p.Namespace != "team-alpha" || p.PublicKey != "pubkey123" {
		t.Errorf("client fields wrong: %+v", p)
	}
	if p.Status.AssignedIp != "10.8.0.5/32" || p.Status.SecretRef != "team-alpha/client-client1" {
		t.Errorf("status wrong: %+v", p.Status)
	}
	if !p.Status.Ready {
		t.Errorf("expected ready true when AssignedIP set: %+v", p.Status)
	}
	if string(p.Status.WgConf) != "wireguard-config" {
		t.Errorf("wgConf wrong: %+v", p.Status)
	}
}
