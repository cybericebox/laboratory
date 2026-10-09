package grpc

import (
	"encoding/json"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/limits"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// labGroupToProto maps a LabGroup custom resource to its gRPC wire representation.
func labGroupToProto(g *laboratoryv1alpha1.LabGroup) *protobuf.LabGroup {
	dg, da := deployOf(g.Annotations)
	var created int64
	if !g.CreationTimestamp.IsZero() {
		created = g.CreationTimestamp.UnixMilli()
	}
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
			Resources:       groupAllocationToProto(g),
			Lifecycle:       groupLifecycleToProto(g),
			Retirement:      retirementToProto(g, g.Status.Retirement),
		},
		Labels:        userLabels(g.Labels),
		CreatedUnixMs: created,
		Uid:           string(g.UID),
		Generation:    g.Generation,
		Lifecycle:     groupIntentToProto(g.Spec.Lifecycle),
		VpnSize:       immutableGroupSize(g.Spec.VPN.Size),
		GatewaySize:   immutableGroupSize(g.Spec.Gateway.Size),
	}
}

// labToProto maps a Lab custom resource to its gRPC wire representation.
// Spec is passed through as opaque JSON since the agent is a thin wrapper.
func labToProto(l *laboratoryv1alpha1.Lab, sizing ...limits.Limits) *protobuf.Lab {
	return labProjection(l, true, sizing...)
}

func labProjection(l *laboratoryv1alpha1.Lab, includeSpec bool, sizing ...limits.Limits) *protobuf.Lab {
	var specJSON []byte
	if includeSpec {
		specJSON, _ = json.Marshal(l.Spec)
	}
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
	status.Retirement = retirementToProto(l, l.Status.Retirement)
	status.Lifecycle = lifecycleToProto(l)
	status.Resources = retiredStorageToProto(l, labAllocationToProto(l, sizing...))
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
	// Retained configuration or an old Ready status must never expose stopped
	// intent as accessible runtime. Keep phases/CIDRs/snapshot history intact.
	if l.Spec.Lifecycle != nil && !exactRunningProjection(l) {
		status.Ready, status.VpnReady, status.InternetReady = false, false, false
		status.Access, status.AccessUrls = nil, nil
		for _, d := range status.Devices {
			d.Ready = false
		}
		for _, c := range status.Connections {
			c.Ready = false
		}
	}
	dg, da := deployOf(l.Annotations)
	return &protobuf.Lab{
		Namespace:       l.Namespace,
		Uid:             string(l.UID),
		CreationReceipt: creationReceiptToProto(l),
		Generation:      l.Generation,
		Name:            names.IDOf(l),
		SpecJson:        specJSON,
		Status:          status,
		Labels:          userLabels(l.Labels),
		DeployGroup:     dg,
		DeployAfter:     da,
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
	if len(pods) == 0 {
		return nil
	}
	out := make([]*protobuf.LabGroupPod, 0, len(pods))
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
func labMonitoringToProto(l *laboratoryv1alpha1.Lab, labGroupName string, sizing ...limits.Limits) *protobuf.Lab {
	p := labProjection(l, false, sizing...)
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
		LabGroupName:     labGroupName,
		OperationId:      policy.Spec.OperationID,
		DesiredRevision:  policy.Spec.Revision,
		Generation:       policy.Generation,
		PolicyUid:        string(policy.UID),
		ExpectedGroupUid: policy.Spec.ExpectedGroupUID,
		Namespace:        policy.Namespace,
		Labels:           userLabels(policy.Labels),
		Status: &protobuf.LabGroupAccessPolicyStatus{
			ObservedGeneration: policy.Status.ObservedGeneration,
			State:              policy.Status.State,
			LastError:          policy.Status.LastError,
			AppliedRevision:    policy.Status.AppliedRevision,
			OperationId:        policy.Status.OperationID,
			VpnBootId:          policy.Status.VPNBootID,
		},
	}
	if !policy.Status.AppliedAt.IsZero() {
		p.Status.AppliedAtUnixMs = policy.Status.AppliedAt.UnixMilli()
	}
	for _, rule := range policy.Spec.Rules {
		action := protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_UNSPECIFIED
		switch rule.Action {
		case laboratoryv1alpha1.LabGroupAccessAllow:
			action = protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_ALLOW
		case laboratoryv1alpha1.LabGroupAccessDeny:
			action = protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_DENY
		}
		p.Rules = append(p.Rules, &protobuf.LabGroupAccessRule{Action: action, ClientNames: orig(rule.ClientNames), LabNames: orig(rule.LabNames)})
	}
	for _, rule := range policy.Status.Rules {
		action := protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_UNSPECIFIED
		switch rule.Action {
		case laboratoryv1alpha1.LabGroupAccessAllow:
			action = protobuf.LabGroupAccessAction_LAB_GROUP_ACCESS_ACTION_ALLOW
		case laboratoryv1alpha1.LabGroupAccessDeny:
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
		Partial:           report.Status.Partial || report.Status.Truncated,
		Truncated:         report.Status.Truncated,
		Ledger:            make([]*protobuf.TrafficTouch, 0, len(report.Status.Ledger)),
	}
	for _, t := range report.Status.Ledger {
		p.Ledger = append(p.Ledger, &protobuf.TrafficTouch{
			Subject: t.Subject, LabName: t.LabName, Device: t.Device, DstIp: t.DstIP, Proto: t.Proto,
			DstPort: uint32(t.DstPort), Attempts: t.Attempts, LabInitiatedAttempts: t.LabInitiatedAttempts,
			PacketsOut: t.PacketsOut, PacketsIn: t.PacketsIn, BytesOut: t.BytesOut, BytesIn: t.BytesIn,
			FirstSeenUnixMs: t.FirstSeenMs, LastSeenUnixMs: t.LastSeenMs, FirstRespondedUnixMs: t.FirstRespondedMs,
		})
	}
	for _, s := range report.Status.CoverageSpans {
		p.CoverageSpans = append(p.CoverageSpans, &protobuf.TrafficCoverageSpan{FromUnixMs: s.FromMs, ToUnixMs: s.ToMs, Partial: s.Partial, Source: s.Source, Instance: s.Instance, BootId: s.BootID})
	}
	p.CoverageSpans = reportCoverage(p)
	for _, s := range p.CoverageSpans {
		p.Partial = p.Partial || s.Partial
	}
	p.Partial = p.Partial || coverageHistoryHasGap(p.CoverageSpans)
	return p
}

// lifecycleToProto never acknowledges a newer intent using an older observation.
// Absent legacy intent keeps the old projection exactly as before.
func lifecycleToProto(l *laboratoryv1alpha1.Lab) *protobuf.LabLifecycleStatus {
	intent := l.Spec.Lifecycle
	if intent == nil {
		return nil
	}
	out := &protobuf.LabLifecycleStatus{DesiredState: intent.DesiredState, ObservedState: allocationUnknown, OperationId: intent.OperationID, LifecycleRevision: intent.Revision, LabUid: string(l.UID), RetentionUntilUnixMs: ms(intent.RetentionUntil), Terminal: intent.Terminal}
	observed := l.Status.Lifecycle
	if observed == nil || observed.LabUID != string(l.UID) || observed.OperationID != intent.OperationID || observed.Revision != intent.Revision || observed.ObservedGeneration != l.Generation {
		return out
	}
	if observed.ObservedState != "" {
		out.ObservedState = observed.ObservedState
	}
	out.ObservedGeneration = observed.ObservedGeneration
	out.Reason, out.Error = observed.Reason, observed.Error
	out.RequestedUnixMs, out.StoppedUnixMs = ms(observed.RequestedAt), ms(observed.StoppedAt)
	out.SnapshotComplete = observed.SnapshotComplete
	out.AccessFenced, out.AccessFencedUnixMs, out.AccessFenceVpnBootId = observed.AccessFenced, ms(observed.AccessFencedAt), observed.AccessFenceVPNBootID
	return out
}

func resourceAmountsToProto(a laboratoryv1alpha1.ResourceAmounts) *protobuf.ResourceAmounts {
	return &protobuf.ResourceAmounts{CpuMillicores: a.CPUMillicores, MemoryBytes: a.MemoryBytes}
}

func allocationToProto(a *laboratoryv1alpha1.RuntimeAllocation) *protobuf.ResourceAllocation {
	if a == nil {
		return nil
	}
	out := &protobuf.ResourceAllocation{
		ConfiguredRequests: resourceAmountsToProto(a.ConfiguredRequests), ConfiguredLimits: resourceAmountsToProto(a.ConfiguredLimits), AllocatedRequests: resourceAmountsToProto(a.AllocatedRequests),
		RuntimeState: a.RuntimeState, ObservedUnixMs: ms(a.ObservedAt), ReleasedUnixMs: ms(a.ReleasedAt), UsageAvailable: a.UsageAvailable,
		SnapshotQuotaBytes: a.SnapshotQuotaBytes, StorageState: a.StorageState, PhysicalStorageBytesAvailable: a.PhysicalStorageBytesAvailable, PhysicalStorageBytes: a.PhysicalStorageBytes,
		OperationId: a.OperationID, LifecycleRevision: a.Revision,
	}
	if out.RuntimeState == "" {
		out.RuntimeState = allocationUnknown
	}
	if out.StorageState == "" {
		out.StorageState = allocationUnknown
	}
	if a.UsageAvailable && a.Used != nil {
		out.Used = resourceAmountsToProto(*a.Used)
	}
	return out
}

// Unknown observations retain configured and previously held amounts. Only an
// exact, timestamped current observation can expose measurements. Release also
// requires a coherent Stopped/capture certificate and the VPN fence when enabled.
func labAllocationToProto(l *laboratoryv1alpha1.Lab, sizing ...limits.Limits) *protobuf.ResourceAllocation {
	a := l.Status.Resources
	intent := l.Spec.Lifecycle
	if intent == nil {
		return allocationToProto(a)
	}
	out := allocationToProto(a)
	if out == nil {
		out = &protobuf.ResourceAllocation{RuntimeState: allocationUnknown, StorageState: allocationUnknown}
	}
	lim := limits.Limits{}
	if len(sizing) > 0 {
		lim = sizing[0]
	}
	cpu, mem, _, _ := lim.SpecTotals(&l.Spec)
	if out.ConfiguredRequests == nil {
		out.ConfiguredRequests = &protobuf.ResourceAmounts{}
	}
	if out.ConfiguredLimits == nil {
		out.ConfiguredLimits = &protobuf.ResourceAmounts{}
	}
	out.ConfiguredRequests.CpuMillicores = max(cpu, out.ConfiguredRequests.CpuMillicores)
	out.ConfiguredRequests.MemoryBytes = max(mem, out.ConfiguredRequests.MemoryBytes)
	out.ConfiguredLimits.CpuMillicores = max(cpu, out.ConfiguredLimits.CpuMillicores)
	out.ConfiguredLimits.MemoryBytes = max(mem, out.ConfiguredLimits.MemoryBytes)
	observed := l.Status.Lifecycle
	current := allocationObservationCurrent(a, intent, observed, l)
	snapshotSucceeded := intent.SnapshotMode != "Required" || observed != nil && observed.SnapshotComplete && observed.Error == ""
	accessFenced := !l.Spec.VPN.Enabled || observed != nil && observed.AccessFenced && observed.AccessFencedAt != nil && !observed.AccessFencedAt.IsZero() && observed.AccessFenceVPNBootID != ""
	released := allocationReleaseComplete(current, intent, observed, a, snapshotSucceeded, accessFenced)
	if !current {
		out.StorageState = allocationUnknown
		out.PhysicalStorageBytesAvailable = false
		out.PhysicalStorageBytes = 0
	}
	if !current || out.RuntimeState == "Released" && !released {
		out.RuntimeState = allocationUnknown
		out.ObservedUnixMs = 0
		out.ReleasedUnixMs = 0
		out.UsageAvailable = false
		out.Used = nil
	}
	if !released {
		if out.AllocatedRequests == nil {
			out.AllocatedRequests = &protobuf.ResourceAmounts{}
		}
		out.AllocatedRequests.CpuMillicores = max(out.AllocatedRequests.CpuMillicores, out.ConfiguredRequests.CpuMillicores)
		out.AllocatedRequests.MemoryBytes = max(out.AllocatedRequests.MemoryBytes, out.ConfiguredRequests.MemoryBytes)
	}
	return out
}

// immutableGroupSize never infers a chart default or a share of aggregate resources.
func immutableGroupSize(s *laboratoryv1alpha1.GroupPodSize) *protobuf.PodSize {
	if s == nil || s.CPUMillicores <= 0 || s.MemoryBytes <= 0 {
		return nil
	}
	return &protobuf.PodSize{CpuMillicores: s.CPUMillicores, MemoryBytes: s.MemoryBytes}
}

func exactRunningProjection(l *laboratoryv1alpha1.Lab) bool {
	i, o := l.Spec.Lifecycle, l.Status.Lifecycle
	return i != nil && i.DesiredState == lifecycleRunning && o != nil && o.ObservedState == lifecycleRunning && o.LabUID == string(l.UID) && o.OperationID == i.OperationID && o.Revision == i.Revision && o.ObservedGeneration == l.Generation
}

func groupAllocationToProto(g *laboratoryv1alpha1.LabGroup) *protobuf.ResourceAllocation {
	out := allocationToProto(g.Status.Resources)
	i := g.Spec.Lifecycle
	if i == nil {
		return out
	}
	if out == nil {
		out = &protobuf.ResourceAllocation{RuntimeState: allocationUnknown, StorageState: allocationUnknown}
	}
	if out.GetOperationId() != i.OperationID || out.GetLifecycleRevision() != i.Revision || !i.IsStopped() && out.RuntimeState == "Released" {
		out.RuntimeState = allocationUnknown
		out.ObservedUnixMs = 0
		out.ReleasedUnixMs = 0
		out.OperationId = i.OperationID
		out.LifecycleRevision = i.Revision
		if out.AllocatedRequests == nil {
			out.AllocatedRequests = &protobuf.ResourceAmounts{}
		}
		if out.ConfiguredRequests != nil {
			out.AllocatedRequests.CpuMillicores = max(out.AllocatedRequests.CpuMillicores, out.ConfiguredRequests.CpuMillicores)
			out.AllocatedRequests.MemoryBytes = max(out.AllocatedRequests.MemoryBytes, out.ConfiguredRequests.MemoryBytes)
		}
	}
	return out
}

func allocationObservationCurrent(a *laboratoryv1alpha1.RuntimeAllocation, intent *laboratoryv1alpha1.LabLifecycleSpec, observed *laboratoryv1alpha1.LabLifecycleStatus, l *laboratoryv1alpha1.Lab) bool {
	return a != nil && a.ObservedAt != nil && !a.ObservedAt.IsZero() && a.OperationID == intent.OperationID && a.Revision == intent.Revision && observed != nil && observed.LabUID == string(l.UID) && observed.OperationID == intent.OperationID && observed.Revision == intent.Revision && observed.ObservedGeneration == l.Generation
}

func allocationReleaseComplete(current bool, intent *laboratoryv1alpha1.LabLifecycleSpec, observed *laboratoryv1alpha1.LabLifecycleStatus, a *laboratoryv1alpha1.RuntimeAllocation, snapshotSucceeded bool, accessFenced bool) bool {
	return current && intent.IsStopped() && observed.ObservedState == "Stopped" && a.RuntimeState == "Released" && a.ReleasedAt != nil && !a.ReleasedAt.IsZero() && a.AllocatedRequests == (laboratoryv1alpha1.ResourceAmounts{}) && snapshotSucceeded && accessFenced
}
