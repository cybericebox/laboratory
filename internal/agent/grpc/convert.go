package grpc

import (
	"encoding/json"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// labGroupToProto maps a LabGroup custom resource to its gRPC wire representation.
func labGroupToProto(g *laboratoryv1alpha1.LabGroup) *protobuf.LabGroup {
	return &protobuf.LabGroup{
		Name: g.Name,
		Status: &protobuf.LabGroupStatus{
			Phase:         string(g.Status.Phase),
			Namespace:     g.Status.Namespace,
			VpnRegistered: g.Status.VPN.Registered,
		},
	}
}

// labToProto maps a Lab custom resource to its gRPC wire representation.
// Spec is passed through as opaque JSON since the agent is a thin wrapper.
func labToProto(l *laboratoryv1alpha1.Lab) *protobuf.Lab {
	specJSON, _ := json.Marshal(l.Spec)
	st := l.Status
	status := &protobuf.LabStatus{
		Phase:         string(st.Phase),
		VpnCidr:       st.VPN.CIDR,
		InternetCidr:  st.Internet.CIDR,
		Ready:         st.Phase == laboratoryv1alpha1.PhaseReady,
		VpnReady:      st.VPN.Ready,
		InternetReady: st.Internet.Ready,
	}
	for i := range st.Devices {
		status.Devices = append(status.Devices, &protobuf.LabDeviceStatus{Name: st.Devices[i].Name, Ready: st.Devices[i].Ready})
	}
	for i := range st.Connections {
		status.Connections = append(status.Connections, &protobuf.LabConnectionStatus{Name: st.Connections[i].Name, Ready: st.Connections[i].Ready})
	}
	for i := range st.Access {
		a := &st.Access[i]
		status.Access = append(status.Access, &protobuf.LabAccessEntry{Device: a.Device, Port: a.Port, Protocol: a.Protocol, Url: a.URL})
		status.AccessUrls = append(status.AccessUrls, a.URL)
	}
	return &protobuf.Lab{
		Namespace: l.Namespace,
		Name:      l.Name,
		SpecJson:  specJSON,
		Status:    status,
	}
}

// protoToLab maps a gRPC Lab message back to a Lab custom resource,
// unmarshalling spec_json into the typed Spec field.
func protoToLab(p *protobuf.Lab) (*laboratoryv1alpha1.Lab, error) {
	l := &laboratoryv1alpha1.Lab{}
	l.Name = p.Name
	l.Namespace = p.Namespace
	if len(p.SpecJson) > 0 {
		if err := json.Unmarshal(p.SpecJson, &l.Spec); err != nil {
			return nil, err
		}
	}
	return l, nil
}

// clientToProto maps a LabGroupClient custom resource to its gRPC wire
// representation. wgConf carries the rendered WireGuard client config,
// which is not stored on the CR itself (it lives in a Secret) so it is
// supplied by the caller.
func clientToProto(c *laboratoryv1alpha1.LabGroupClient, wgConf []byte) *protobuf.LabGroupClient {
	// WireGuard exposes transfer bytes + last handshake per peer only (no packet
	// counts). A zero handshake time (never connected) maps to 0.
	stats := &protobuf.LabGroupClientStatistics{
		RxBytes: c.Status.Statistics.RxBytes,
		TxBytes: c.Status.Statistics.TxBytes,
	}
	if !c.Status.Statistics.LastHandshake.IsZero() {
		stats.LastHandshakeUnix = c.Status.Statistics.LastHandshake.Unix()
	}
	return &protobuf.LabGroupClient{
		Namespace: c.Namespace,
		Name:      c.Name,
		PublicKey: c.Spec.PublicKey,
		Status: &protobuf.LabGroupClientStatus{
			AssignedIp: c.Status.AssignedIP,
			SecretRef:  c.Status.SecretRef,
			Ready:      c.Status.AssignedIP != "",
			WgConf:     wgConf,
			Statistics: stats,
		},
	}
}
