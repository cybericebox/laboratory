package grpc

import (
	"context"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

const (
	monitoringSchemaVersion   = 3
	minimumMonitoringPeriod   = 10 * time.Millisecond
	defaultMonitoringPeriod   = 5 * time.Second
	monitoringHeartbeatPeriod = 30 * time.Second
)

// monState is one complete observation of the platform: the secret-free
// MonitoringUpdate and, kept beside it, the Kubernetes labels of every record,
// which the selector of a subscriber is matched against.
type monState struct {
	update *protobuf.MonitoringUpdate
	// labels is keyed like monitoringRecord.key().
	labels map[string]map[string]string
	// labLabels is keyed "<group>\x00<lab>": the labels of a Lab, which decide
	// whether the touches of a traffic report are visible to a subscriber.
	labLabels map[string]map[string]string
}

func recordKey(kind, group, namespace, name string) string {
	return strings.Join([]string{kind, group, namespace, name}, "\x00")
}

func labLabelKey(group, lab string) string { return group + "\x00" + lab }

// snapshot returns the current MonitoringUpdate (see collect).
func (h *Handler) snapshot(ctx context.Context) (*protobuf.MonitoringUpdate, error) {
	st, err := h.collect(ctx)
	if err != nil {
		return nil, err
	}
	return st.update, nil
}

// collect builds a secret-free observation covering all LabGroups
// (cluster-scoped) and, for each group with a provisioned namespace, its Labs
// and LabGroupClients (namespace-scoped).
func (h *Handler) collect(ctx context.Context) (*monState, error) {
	groups, err := h.cs.LaboratoryV1alpha1().LabGroups().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	st := &monState{labels: map[string]map[string]string{}, labLabels: map[string]map[string]string{}}
	upd := &protobuf.MonitoringUpdate{}
	st.update = upd
	for i := range groups.Items {
		g := &groups.Items[i]
		gid := names.IDOf(g)
		upd.Groups = append(upd.Groups, labGroupToProto(g))
		st.labels[recordKey("lab_group", gid, "", gid)] = g.Labels
		ns := g.Status.Namespace
		if ns == "" {
			continue
		}
		// CR name -> id, to give the ids back in what the collectors report by CR name.
		labIDs, clientIDs := map[string]string{}, map[string]string{}
		labs, err := h.cs.LaboratoryV1alpha1().Labs(ns).List(ctx, metav1.ListOptions{})
		if err == nil {
			usage := h.namespaceUsage(ctx, ns)
			pods := h.namespacePodStatus(ctx, ns)
			sched := h.namespaceDeviceScheduling(ctx, ns)
			for j := range labs.Items {
				lab := &labs.Items[j]
				p := labMonitoringToProto(lab, gid)
				fillLabUsage(p, usage, lab.Name)
				fillLabPodStatus(p, pods, lab.Name)
				fillDeviceScheduling(p, sched, lab.Name)
				upd.Labs = append(upd.Labs, p)
				labIDs[lab.Name] = p.Name
				st.labels[recordKey("lab", gid, ns, p.Name)] = lab.Labels
				st.labLabels[labLabelKey(gid, p.Name)] = lab.Labels
			}
		}
		clients, err := h.cs.LaboratoryV1alpha1().LabGroupClients(ns).List(ctx, metav1.ListOptions{})
		if err == nil {
			for j := range clients.Items {
				p := clientMonitoringToProto(&clients.Items[j], gid)
				upd.Clients = append(upd.Clients, p)
				clientIDs[clients.Items[j].Name] = p.Name
				st.labels[recordKey("client", gid, ns, p.Name)] = clients.Items[j].Labels
			}
		}
		policy, err := h.cs.LaboratoryV1alpha1().LabGroupAccessPolicies(ns).Get(ctx, "access-policy", metav1.GetOptions{})
		if err == nil {
			upd.Policies = append(upd.Policies, accessPolicyToProto(policy, gid))
			st.labels[recordKey("access_policy", gid, ns, "access-policy")] = policy.Labels
		}
		reports, err := h.cs.LaboratoryV1alpha1().LabTrafficReports(ns).List(ctx, metav1.ListOptions{})
		if err == nil {
			var proxies []*protobuf.TrafficReport
			for j := range reports.Items {
				report := trafficReportToProto(&reports.Items[j], gid)
				restoreTrafficIDs(report, labIDs, clientIDs)
				if report.GetKind() == "proxy" {
					proxies = append(proxies, report)
					continue
				}
				upd.Traffic = append(upd.Traffic, report)
			}
			if merged := mergeProxyReports(proxies); merged != nil {
				upd.Traffic = append(upd.Traffic, merged)
			}
		}
	}
	capacity, err := h.GetCapacity(ctx, &protobuf.Empty{})
	if err != nil {
		return nil, err
	}
	upd.Capacity = capacity
	sortMonitoringRecords(upd)
	sortTraffic(upd)
	return st, nil
}

// Monitoring streams the platform state to one subscriber. It starts with a full
// snapshot, or, when the subscriber names an agent epoch and sequence the journal
// still covers, with the updates it missed; then it sends only changes. All
// subscribers share one poller and one journal (see monitor), each gets its own
// selector, minimum interval and position.
func (h *Handler) Monitoring(request *protobuf.MonitoringRequest, stream protobuf.LabManager_MonitoringServer) error {
	filter, err := newSelectorFilter(request.GetSelector())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "selector: %v", err)
	}
	period := monitoringPeriod(request.GetMinIntervalMs())
	sub, start, err := h.monitor().subscribe(stream.Context(), request)
	if err != nil {
		return err
	}
	defer h.monitor().unsubscribe(sub)

	// The loop below never blocks on the network: a goroutine does the sends, so a
	// subscriber stuck in Send fills its journal buffer, is dropped by the monitor, and
	// the loop (watching sub.dropped) ends the call, which unblocks the sender.
	out := make(chan *protobuf.MonitoringUpdate, 1)
	sendErr := make(chan error, 1)
	go func() {
		for u := range out {
			if err := stream.Send(u); err != nil {
				sendErr <- err
				return
			}
		}
		sendErr <- nil
	}()
	defer func() {
		close(out)
	}()
	emit := func(u *protobuf.MonitoringUpdate) error {
		select {
		case out <- u:
			return nil
		case err := <-sendErr:
			if err == nil {
				err = context.Canceled
			}
			return err
		case <-sub.dropped:
			return sub.dropErr()
		case <-stream.Context().Done():
			return nil
		}
	}

	processed := start.sequence
	if start.snapshot != nil {
		if err := emit(h.monitoringUpdateAt(filter.snapshot(start.snapshot), processed, true)); err != nil {
			return err
		}
	}
	acc := newAccumulator()
	for _, e := range start.replay {
		acc.add(filter.entry(e))
		processed = e.seq
	}
	last := time.Now()
	if !acc.empty() {
		if err := emit(h.monitoringUpdateAt(acc.take(), processed, false)); err != nil {
			return err
		}
	}

	heartbeat := time.NewTimer(monitoringHeartbeatPeriod)
	defer heartbeat.Stop()
	flush := time.NewTimer(time.Hour)
	if !flush.Stop() {
		<-flush.C
	}
	defer flush.Stop()
	flushArmed := false
	arm := func() {
		wait := period - time.Since(last)
		if wait < 0 {
			wait = 0
		}
		resetMonitoringTimer(flush, wait)
		flushArmed = true
	}

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case err := <-sendErr:
			return err
		case <-sub.dropped:
			return sub.dropErr()
		case e := <-sub.ch:
			processed = e.seq
			acc.add(filter.entry(e))
			if !acc.empty() && !flushArmed {
				arm()
			}
		case <-flush.C:
			flushArmed = false
			if acc.empty() {
				continue
			}
			if err := emit(h.monitoringUpdateAt(acc.take(), processed, false)); err != nil {
				return err
			}
			last = time.Now()
			resetMonitoringTimer(heartbeat, monitoringHeartbeatPeriod)
		case <-heartbeat.C:
			// A quiet stream still reports how far it has processed, so the
			// subscriber's resume position stays inside the journal window.
			if err := emit(h.monitoringUpdateAt(&protobuf.MonitoringUpdate{}, processed, false)); err != nil {
				return err
			}
			resetMonitoringTimer(heartbeat, monitoringHeartbeatPeriod)
		}
	}
}

// monitoringUpdateAt stamps an update with the agent identity and the position it covers.
func (h *Handler) monitoringUpdateAt(update *protobuf.MonitoringUpdate, sequence int64, snapshot bool) *protobuf.MonitoringUpdate {
	update.AgentId = h.agentID
	update.AgentEpoch = h.monitor().epoch
	update.Sequence = sequence
	update.ObservedAtUnixMs = time.Now().UnixMilli()
	update.SchemaVersion = monitoringSchemaVersion
	update.Snapshot = snapshot
	return update
}

func monitoringPeriod(requestedMillis int64) time.Duration {
	if requestedMillis <= 0 {
		return defaultMonitoringPeriod
	}
	requested := time.Duration(requestedMillis) * time.Millisecond
	if requested < minimumMonitoringPeriod {
		return minimumMonitoringPeriod
	}
	return requested
}

func resetMonitoringTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}

func monitoringDelta(previous, next *protobuf.MonitoringUpdate) (*protobuf.MonitoringUpdate, bool) {
	previousRecords := monitoringRecordIndex(previous)
	nextRecords := monitoringRecordIndex(next)
	delta := &protobuf.MonitoringUpdate{}
	keys := make([]string, 0, len(nextRecords))
	for key := range nextRecords {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		nextRecord := nextRecords[key]
		previousRecord, found := previousRecords[key]
		if found && proto.Equal(previousRecord.value, nextRecord.value) {
			continue
		}
		appendMonitoringRecord(delta, nextRecord)
	}
	for key, record := range previousRecords {
		if _, found := nextRecords[key]; found {
			continue
		}
		delta.DeletedKeys = append(delta.DeletedKeys, record.deletedKey())
	}
	if !proto.Equal(previous.GetCapacity(), next.GetCapacity()) {
		delta.Capacity = next.GetCapacity()
	}
	sort.Slice(delta.DeletedKeys, func(i, j int) bool {
		return monitoringDeletedKeyString(delta.DeletedKeys[i]) < monitoringDeletedKeyString(delta.DeletedKeys[j])
	})
	return delta, len(delta.Groups) > 0 || len(delta.Labs) > 0 || len(delta.Clients) > 0 || len(delta.Policies) > 0 || len(delta.Traffic) > 0 || len(delta.DeletedKeys) > 0 || delta.Capacity != nil
}

type monitoringRecord struct {
	kind      string
	groupName string
	namespace string
	name      string
	value     proto.Message
}

func (r monitoringRecord) key() string {
	return strings.Join([]string{r.kind, r.groupName, r.namespace, r.name}, "\x00")
}

func (r monitoringRecord) deletedKey() *protobuf.MonitoringDeletedKey {
	return &protobuf.MonitoringDeletedKey{Kind: r.kind, LabGroupName: r.groupName, Namespace: r.namespace, Name: r.name}
}

func monitoringRecordIndex(update *protobuf.MonitoringUpdate) map[string]monitoringRecord {
	records := make(map[string]monitoringRecord, len(update.Groups)+len(update.Labs)+len(update.Clients)+len(update.Policies))
	for _, group := range update.Groups {
		record := monitoringRecord{kind: "lab_group", groupName: group.GetName(), name: group.GetName(), value: group}
		records[record.key()] = record
	}
	for _, lab := range update.Labs {
		record := monitoringRecord{kind: "lab", groupName: lab.GetLabGroupName(), namespace: lab.GetNamespace(), name: lab.GetName(), value: lab}
		records[record.key()] = record
	}
	for _, client := range update.Clients {
		record := monitoringRecord{kind: "client", groupName: client.GetLabGroupName(), namespace: client.GetNamespace(), name: client.GetName(), value: client}
		records[record.key()] = record
	}
	for _, policy := range update.Policies {
		record := monitoringRecord{kind: "access_policy", groupName: policy.GetLabGroupName(), namespace: policy.GetNamespace(), name: "access-policy", value: policy}
		records[record.key()] = record
	}
	for _, report := range update.Traffic {
		record := monitoringRecord{kind: "traffic_report", groupName: report.GetLabGroupName(), namespace: report.GetNamespace(), name: report.GetSource(), value: report}
		records[record.key()] = record
	}
	return records
}

func appendMonitoringRecord(update *protobuf.MonitoringUpdate, record monitoringRecord) {
	switch value := record.value.(type) {
	case *protobuf.LabGroup:
		update.Groups = append(update.Groups, value)
	case *protobuf.Lab:
		update.Labs = append(update.Labs, value)
	case *protobuf.LabGroupClient:
		update.Clients = append(update.Clients, value)
	case *protobuf.LabGroupAccessPolicy:
		update.Policies = append(update.Policies, value)
	case *protobuf.TrafficReport:
		update.Traffic = append(update.Traffic, value)
	}
}

func sortMonitoringRecords(update *protobuf.MonitoringUpdate) {
	sort.Slice(update.Groups, func(i, j int) bool { return update.Groups[i].GetName() < update.Groups[j].GetName() })
	sort.Slice(update.Labs, func(i, j int) bool {
		return update.Labs[i].GetLabGroupName()+"\x00"+update.Labs[i].GetNamespace()+"\x00"+update.Labs[i].GetName() < update.Labs[j].GetLabGroupName()+"\x00"+update.Labs[j].GetNamespace()+"\x00"+update.Labs[j].GetName()
	})
	sort.Slice(update.Clients, func(i, j int) bool {
		return update.Clients[i].GetLabGroupName()+"\x00"+update.Clients[i].GetNamespace()+"\x00"+update.Clients[i].GetName() < update.Clients[j].GetLabGroupName()+"\x00"+update.Clients[j].GetNamespace()+"\x00"+update.Clients[j].GetName()
	})
	sort.Slice(update.Policies, func(i, j int) bool {
		return update.Policies[i].GetLabGroupName()+"\x00"+update.Policies[i].GetNamespace() < update.Policies[j].GetLabGroupName()+"\x00"+update.Policies[j].GetNamespace()
	})
}

func sortTraffic(update *protobuf.MonitoringUpdate) {
	sort.Slice(update.Traffic, func(i, j int) bool {
		return update.Traffic[i].GetLabGroupName()+"\x00"+update.Traffic[i].GetSource() < update.Traffic[j].GetLabGroupName()+"\x00"+update.Traffic[j].GetSource()
	})
}

func monitoringDeletedKeyString(key *protobuf.MonitoringDeletedKey) string {
	return strings.Join([]string{key.GetKind(), key.GetLabGroupName(), key.GetNamespace(), key.GetName()}, "\x00")
}

// restoreTrafficIDs replaces the CR names a collector reports (labs, and VPN clients as
// subjects) by their ids.
func restoreTrafficIDs(r *protobuf.TrafficReport, labIDs, clientIDs map[string]string) {
	for _, t := range r.GetLedger() {
		if id, ok := labIDs[t.LabName]; ok {
			t.LabName = id
		}
		if r.GetKind() == "vpn" {
			if id, ok := clientIDs[t.Subject]; ok {
				t.Subject = id
			}
		}
	}
}
