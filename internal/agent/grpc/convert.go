package grpc

import (
	"encoding/json"

	"k8s.io/apimachinery/pkg/api/resource"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// labGroupToProto maps a LabGroup custom resource to its gRPC wire representation.
func labGroupToProto(g *laboratoryv1alpha1.LabGroup) *protobuf.LabGroup {
	return &protobuf.LabGroup{
		Name: g.Name,
		Status: &protobuf.LabGroupStatus{
			Phase:           string(g.Status.Phase),
			Namespace:       g.Status.Namespace,
			VpnRegistered:   g.Status.VPN.Registered,
			Suspended:       g.Status.Suspended,
			VpnClientSubnet: g.Status.VPN.ClientSubnet,
			ImageWarning:    g.Status.ImageWarning,
		},
		Labels: copyLabels(g.Labels),
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
		ImageWarning:  st.ImageWarning,
	}
	status.Queue = labQueueToProto(st.Scheduling)
	for i := range st.Devices {
		status.Devices = append(status.Devices, &protobuf.LabDeviceStatus{
			Name: st.Devices[i].Name, Ready: st.Devices[i].Ready,
			Snapshot: snapshotStatusToProto(st.Devices[i].State),
		})
	}
	for _, device := range l.Spec.Devices {
		if device.Resources == nil {
			continue
		}
		for _, statusDevice := range status.Devices {
			if statusDevice.Name != device.Name {
				continue
			}
			statusDevice.CpuRequestMillicores = quantityMilliValue(device.Resources.CPURequest)
			statusDevice.MemoryRequestBytes = quantityValue(device.Resources.MemoryRequest)
			statusDevice.CpuLimitMillicores = quantityMilliValue(device.Resources.CPULimit)
			statusDevice.MemoryLimitBytes = quantityValue(device.Resources.MemoryLimit)
		}
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
		Labels:    copyLabels(l.Labels),
	}
}

// labQueueToProto maps the scheduler queue place; nil for a lab that has none.
// The proto still has the shape of the old launch queue: launch_class carries the
// deploy group, and admitted_at is not reported.
func labQueueToProto(l *laboratoryv1alpha1.SchedulingStatus) *protobuf.LabQueueStatus {
	if l == nil {
		return nil
	}
	return &protobuf.LabQueueStatus{Position: l.Position, Length: l.Length, Reason: l.Reason, LaunchClass: l.Group}
}

func quantityMilliValue(value string) int64 {
	if value == "" {
		return 0
	}
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return 0
	}
	return quantity.MilliValue()
}

func quantityValue(value string) int64 {
	if value == "" {
		return 0
	}
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return 0
	}
	return quantity.Value()
}

// labMonitoringToProto projects runtime state without exposing the Lab spec
// or write-only device environment values through the monitoring stream.
func labMonitoringToProto(l *laboratoryv1alpha1.Lab, labGroupName string) *protobuf.Lab {
	p := labToProto(l)
	p.SpecJson = nil
	p.Env = nil
	p.LabGroupName = labGroupName
	return p
}

// protoToLab maps a gRPC Lab message back to a Lab custom resource,
// unmarshalling spec_json into the typed Spec field.
func protoToLab(p *protobuf.Lab) (*laboratoryv1alpha1.Lab, error) {
	l := &laboratoryv1alpha1.Lab{}
	l.Name = p.Name
	l.Namespace = p.Namespace
	l.Labels = copyLabels(p.Labels)
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
func clientToProto(c *laboratoryv1alpha1.LabGroupClient) *protobuf.LabGroupClient {
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
			Ready:      c.Status.AssignedIP != "",
			Config:     c.Status.Config,
			Statistics: stats,
		},
		Labels: copyLabels(c.Labels),
	}
}

// clientMonitoringToProto omits both rendered client configuration and public
// key material. Monitoring needs connection and transfer status only.
func clientMonitoringToProto(c *laboratoryv1alpha1.LabGroupClient, labGroupName string) *protobuf.LabGroupClient {
	p := clientToProto(c)
	p.PublicKey = ""
	p.Status.Config = ""
	p.LabGroupName = labGroupName
	return p
}

func accessPolicyToProto(policy *laboratoryv1alpha1.LabGroupAccessPolicy, labGroupName string) *protobuf.LabGroupAccessPolicy {
	p := &protobuf.LabGroupAccessPolicy{
		LabGroupName: labGroupName,
		Namespace:    policy.Namespace,
		Labels:       copyLabels(policy.Labels),
		Status: &protobuf.LabGroupAccessPolicyStatus{
			ObservedGeneration: policy.Status.ObservedGeneration,
			State:              policy.Status.State,
			LastError:          policy.Status.LastError,
		},
	}
	if !policy.Status.AppliedAt.IsZero() {
		p.Status.AppliedAtUnixMs = policy.Status.AppliedAt.UnixMilli()
	}
	for _, rule := range policy.Spec.Rules {
		action := protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_UNSPECIFIED
		if rule.Action == laboratoryv1alpha1.LabGroupAccessAllow {
			action = protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_ALLOW
		} else if rule.Action == laboratoryv1alpha1.LabGroupAccessDeny {
			action = protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_DENY
		}
		p.Rules = append(p.Rules, &protobuf.LabGroupAccessRule{Action: action, ClientNames: rule.ClientNames, LabNames: rule.LabNames})
	}
	for _, rule := range policy.Status.Rules {
		action := protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_UNSPECIFIED
		if rule.Action == laboratoryv1alpha1.LabGroupAccessAllow {
			action = protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_ALLOW
		} else if rule.Action == laboratoryv1alpha1.LabGroupAccessDeny {
			action = protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_DENY
		}
		p.Status.Rules = append(p.Status.Rules, &protobuf.LabGroupAccessRuleStatistics{
			ClientName: rule.ClientName, LabName: rule.LabName, Action: action,
			Packets: rule.Packets, Bytes: rule.Bytes, CounterReset: rule.CounterReset,
		})
	}
	return p
}

// trafficReportToProto relays a collector's report. The namespace decides the
// group; nothing inside the report is trusted to name one.
func trafficReportToProto(report *laboratoryv1alpha1.LabTrafficReport, labGroupName string) *protobuf.TrafficReport {
	p := &protobuf.TrafficReport{
		LabGroupName:      labGroupName,
		Namespace:         report.Namespace,
		Source:            report.Name,
		Kind:              string(report.Spec.Kind),
		Instance:          report.Spec.Instance,
		BootId:            report.Status.BootID,
		CoveredFromUnixMs: report.Status.CoveredFromMs,
		CoveredToUnixMs:   report.Status.CoveredToMs,
		Partial:           report.Status.Partial,
		Truncated:         report.Status.Truncated,
		Ledger:            make([]*protobuf.TrafficTouch, 0, len(report.Status.Ledger)),
	}
	for _, t := range report.Status.Ledger {
		p.Ledger = append(p.Ledger, &protobuf.TrafficTouch{
			Subject: t.Subject, LabName: t.LabName, Device: t.Device, DstIp: t.DstIP, Proto: t.Proto,
			DstPort: uint32(t.DstPort), Attempts: t.Attempts,
			PacketsOut: t.PacketsOut, PacketsIn: t.PacketsIn, BytesOut: t.BytesOut, BytesIn: t.BytesIn,
			FirstSeenUnixMs: t.FirstSeenMs, LastSeenUnixMs: t.LastSeenMs, FirstRespondedUnixMs: t.FirstRespondedMs,
		})
	}
	return p
}
