package grpc

import (
	"encoding/json"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// labGroupToProto maps a LabGroup custom resource to its gRPC wire representation.
func labGroupToProto(g *laboratoryv1alpha1.LabGroup) *protobuf.LabGroup {
	dg, da := deployOf(g.Annotations)
	return &protobuf.LabGroup{
		Name:        names.IDOf(g),
		DeployGroup: dg,
		DeployAfter: da,
		Status: &protobuf.LabGroupStatus{
			Phase:           string(g.Status.Phase),
			Namespace:       g.Status.Namespace,
			VpnRegistered:   g.Status.VPN.Registered,
			Suspended:       g.Status.Suspended,
			VpnClientSubnet: g.Status.VPN.ClientSubnet,
			ImageWarning:    g.Status.ImageWarning,
			Scheduling:      schedulingToProto(g.Status.Scheduling, g.Annotations),
			Pods:            groupPodsToProto(g.Status.Pods),
		},
		Labels: userLabels(g.Labels),
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
	status.Scheduling = schedulingToProto(st.Scheduling, l.Annotations)
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
	dg, da := deployOf(l.Annotations)
	return &protobuf.Lab{
		Namespace:   l.Namespace,
		Name:        names.IDOf(l),
		SpecJson:    specJSON,
		Status:      status,
		Labels:      userLabels(l.Labels),
		DeployGroup: dg,
		DeployAfter: da,
	}
}

// schedulingToProto maps the scheduler queue place of a Lab or LabGroup; nil when it has
// none. The group is the original deploy key from the object's annotation (the status
// holds the encoded label value).
func schedulingToProto(s *laboratoryv1alpha1.SchedulingStatus, annotations map[string]string) *protobuf.Scheduling {
	if s == nil {
		return nil
	}
	group, _ := deployOf(annotations)
	if group == "" {
		group = s.Group
	}
	return &protobuf.Scheduling{Group: group, Position: s.Position, Length: s.Length, Reason: s.Reason, Message: s.Message, Pods: s.Pods, Pending: s.Pending}
}

func ms(t *metav1.Time) int64 {
	if t == nil || t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// podScheduleToProto maps the scheduler state of one pod; nil when untracked.
func podScheduleToProto(p *laboratoryv1alpha1.PodSchedule) *protobuf.PodScheduling {
	if p == nil || p.State == "" {
		return nil
	}
	out := &protobuf.PodScheduling{QueuedUnixMs: ms(p.QueuedAt), DispatchedUnixMs: ms(p.DispatchedAt), StartedUnixMs: ms(p.StartedAt)}
	switch p.State {
	case laboratoryv1alpha1.PodQueued:
		out.State = protobuf.PodState_POD_STATE_QUEUED
	case laboratoryv1alpha1.PodStarting:
		out.State = protobuf.PodState_POD_STATE_STARTING
	case laboratoryv1alpha1.PodStarted:
		out.State = protobuf.PodState_POD_STATE_STARTED
	case laboratoryv1alpha1.PodFailed:
		out.State = protobuf.PodState_POD_STATE_FAILED
	}
	if f := p.Failure; f != nil {
		out.Failure = &protobuf.PodFailure{Reason: f.Reason, Message: f.Message, RestartCount: f.RestartCount, AtUnixMs: ms(f.At)}
	}
	return out
}

// fillDeviceScheduling sets the scheduler state of each device of a proto Lab from the
// Device objects of its namespace, keyed by device name (the lab's CR name is lab.crName).
func fillDeviceScheduling(lab *protobuf.Lab, devices map[usageKey]*laboratoryv1alpha1.PodSchedule, crName string) {
	if lab.GetStatus() == nil || devices == nil {
		return
	}
	for _, d := range lab.Status.Devices {
		d.Scheduling = podScheduleToProto(devices[usageKey{lab: crName, device: d.Name}])
	}
}

// groupPodsToProto maps the scheduler state of the pods a LabGroup runs itself.
func groupPodsToProto(pods []laboratoryv1alpha1.NamedPodSchedule) []*protobuf.LabGroupPod {
	var out []*protobuf.LabGroupPod
	for i := range pods {
		out = append(out, &protobuf.LabGroupPod{Name: pods[i].Name, Scheduling: podScheduleToProto(&pods[i].PodSchedule)})
	}
	return out
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
	p.LabGroupName = labGroupName
	return p
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
		Name:      names.IDOf(c),
		PublicKey: c.Spec.PublicKey,
		Status: &protobuf.LabGroupClientStatus{
			AssignedIp: c.Status.AssignedIP,
			Ready:      c.Status.AssignedIP != "",
			Config:     c.Status.Config,
			Statistics: stats,
		},
		Labels: userLabels(c.Labels),
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

// policyIDMap reads the {encoded name: original id} map of an access policy.
func policyIDMap(policy *laboratoryv1alpha1.LabGroupAccessPolicy) map[string]string {
	var m map[string]string
	if raw := policy.Annotations[names.AnnotationIDMap]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &m)
	}
	return m
}

func accessPolicyToProto(policy *laboratoryv1alpha1.LabGroupAccessPolicy, labGroupName string) *protobuf.LabGroupAccessPolicy {
	idMap := policyIDMap(policy)
	orig := func(ns []string) []string {
		out := make([]string, len(ns))
		for i, n := range ns {
			out[i] = n
			if o, ok := idMap[n]; ok {
				out[i] = o
			}
		}
		return out
	}
	origOne := func(n string) string {
		if o, ok := idMap[n]; ok {
			return o
		}
		return n
	}
	p := &protobuf.LabGroupAccessPolicy{
		LabGroupName: labGroupName,
		Namespace:    policy.Namespace,
		Labels:       userLabels(policy.Labels),
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
		p.Rules = append(p.Rules, &protobuf.LabGroupAccessRule{Action: action, ClientNames: orig(rule.ClientNames), LabNames: orig(rule.LabNames)})
	}
	for _, rule := range policy.Status.Rules {
		action := protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_UNSPECIFIED
		if rule.Action == laboratoryv1alpha1.LabGroupAccessAllow {
			action = protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_ALLOW
		} else if rule.Action == laboratoryv1alpha1.LabGroupAccessDeny {
			action = protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_DENY
		}
		p.Status.Rules = append(p.Status.Rules, &protobuf.LabGroupAccessRuleStatistics{
			ClientName: origOne(rule.ClientName), LabName: origOne(rule.LabName), Action: action,
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
