package grpc

import (
	"context"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

const (
	monitoringSchemaVersion   = 1
	minimumMonitoringPeriod   = 10 * time.Millisecond
	defaultMonitoringPeriod   = 5 * time.Second
	monitoringHeartbeatPeriod = 30 * time.Second
)

// snapshot builds a secret-free MonitoringUpdate covering all LabGroups
// (cluster-scoped) and, for each group with a provisioned namespace, its Labs
// and LabGroupClients (namespace-scoped).
func (h *Handler) snapshot(ctx context.Context) (*protobuf.MonitoringUpdate, error) {
	groups, err := h.cs.LaboratoryV1alpha1().LabGroups().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	upd := &protobuf.MonitoringUpdate{}
	for i := range groups.Items {
		g := &groups.Items[i]
		upd.Groups = append(upd.Groups, labGroupToProto(g))
		ns := g.Status.Namespace
		if ns == "" {
			continue
		}
		labs, err := h.cs.LaboratoryV1alpha1().Labs(ns).List(ctx, metav1.ListOptions{})
		if err == nil {
			usage := h.namespaceUsage(ctx, ns)
			for j := range labs.Items {
				p := labMonitoringToProto(&labs.Items[j], g.Name)
				fillLabUsage(p, usage)
				upd.Labs = append(upd.Labs, p)
			}
		}
		clients, err := h.cs.LaboratoryV1alpha1().LabGroupClients(ns).List(ctx, metav1.ListOptions{})
		if err == nil {
			for j := range clients.Items {
				upd.Clients = append(upd.Clients, clientMonitoringToProto(&clients.Items[j], g.Name))
			}
		}
	}
	sortMonitoringRecords(upd)
	return upd, nil
}

// Monitoring starts with a complete snapshot and then sends only changed
// records. It intentionally does not resume an old sequence: a reconnection
// receives a new snapshot so consumers can safely discard a partial delta set.
func (h *Handler) Monitoring(request *protobuf.MonitoringRequest, stream protobuf.LabManager_MonitoringServer) error {
	period := monitoringPeriod(request.GetMinIntervalMs())
	current, err := h.snapshot(stream.Context())
	if err != nil {
		return err
	}

	sequence := int64(1)
	if err := stream.Send(h.monitoringUpdate(current, sequence, true)); err != nil {
		return err
	}

	ticker := time.NewTicker(period)
	defer ticker.Stop()
	heartbeat := time.NewTimer(monitoringHeartbeatPeriod)
	defer heartbeat.Stop()

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-ticker.C:
			next, err := h.snapshot(stream.Context())
			if err != nil {
				return err
			}
			delta, changed := monitoringDelta(current, next)
			if !changed {
				continue
			}
			sequence++
			if err := stream.Send(h.monitoringUpdate(delta, sequence, false)); err != nil {
				return err
			}
			current = next
			resetMonitoringTimer(heartbeat, monitoringHeartbeatPeriod)
		case <-heartbeat.C:
			sequence++
			if err := stream.Send(h.monitoringUpdate(&protobuf.MonitoringUpdate{}, sequence, false)); err != nil {
				return err
			}
			resetMonitoringTimer(heartbeat, monitoringHeartbeatPeriod)
		}
	}
}

func (h *Handler) monitoringUpdate(update *protobuf.MonitoringUpdate, sequence int64, snapshot bool) *protobuf.MonitoringUpdate {
	update.AgentId = h.agentID
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
	sort.Slice(delta.DeletedKeys, func(i, j int) bool {
		return monitoringDeletedKeyString(delta.DeletedKeys[i]) < monitoringDeletedKeyString(delta.DeletedKeys[j])
	})
	return delta, len(delta.Groups) > 0 || len(delta.Labs) > 0 || len(delta.Clients) > 0 || len(delta.DeletedKeys) > 0
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
	records := make(map[string]monitoringRecord, len(update.Groups)+len(update.Labs)+len(update.Clients))
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
}

func monitoringDeletedKeyString(key *protobuf.MonitoringDeletedKey) string {
	return strings.Join([]string{key.GetKind(), key.GetLabGroupName(), key.GetNamespace(), key.GetName()}, "\x00")
}
