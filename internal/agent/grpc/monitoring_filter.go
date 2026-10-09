package grpc

import (
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"

	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// selectorFilter cuts monitoring updates down to what matches a Kubernetes label
// selector. An empty selector matches everything and returns updates whole.
type selectorFilter struct {
	sel labels.Selector // nil: everything
}

// newSelectorFilter keeps what is of the tenant AND matches the user's selector. The tenant
// test comes first and cannot be escaped by the selector (which may not name reserved keys).
func newSelectorFilter(expr, tenant string) (*selectorFilter, error) {
	sel := labels.Everything()
	if strings.TrimSpace(expr) != "" {
		var err error
		if sel, err = labels.Parse(expr); err != nil {
			return nil, err
		}
		if err := checkSelectorKeys(expr); err != nil {
			return nil, err
		}
	}
	req, err := labels.NewRequirement(names.LabelTenant, selection.Equals, []string{tenant})
	if err != nil {
		return nil, err
	}
	return &selectorFilter{sel: sel.Add(*req)}, nil
}

func (f *selectorFilter) matches(l map[string]string) bool {
	return f.sel == nil || f.sel.Matches(labels.Set(l))
}

// snapshot filters a full state; the result is a new message the caller may stamp.
func (f *selectorFilter) snapshot(st *monState) *protobuf.MonitoringUpdate {
	return f.apply(st.update, func(key string) map[string]string { return st.labels[key] },
		func(group, lab string) map[string]string { return st.labLabels[labLabelKey(group, lab)] }, f.emptyTrafficGroups(st.update, st.labels, st.labLabels))
}

// entry filters one journal entry.
func (f *selectorFilter) entry(e *journalEntry) *protobuf.MonitoringUpdate {
	return f.apply(e.update, func(key string) map[string]string { return e.labels[key] },
		func(group, lab string) map[string]string { return e.labLabels[labLabelKey(group, lab)] }, f.emptyTrafficGroups(e.update, e.labels, e.labLabels))
}

func (f *selectorFilter) emptyTrafficGroups(u *protobuf.MonitoringUpdate, recordLabels, labLabels map[string]map[string]string) map[string]bool {
	var visible, needsLabs map[string]bool
	for _, r := range u.GetTraffic() {
		if visible == nil {
			visible = map[string]bool{}
			needsLabs = map[string]bool{}
		}
		group := r.GetLabGroupName()
		if f.matches(recordLabels[recordKey("lab_group", group, "", group)]) {
			visible[group] = true
		} else {
			needsLabs[group] = true
		}
	}
	if len(needsLabs) > 0 {
		for key, l := range labLabels {
			group, _, _ := strings.Cut(key, "\x00")
			if needsLabs[group] && f.matches(l) {
				visible[group] = true
			}
		}
	}
	return visible
}

// apply keeps the records, deletions and traffic touches of matching objects.
// Capacity is always kept. Records and reports are shared with the original,
// never modified.
func (f *selectorFilter) apply(u *protobuf.MonitoringUpdate, labelsOf func(key string) map[string]string, labOf func(group, lab string) map[string]string, emptyGroups map[string]bool) *protobuf.MonitoringUpdate {
	out := &protobuf.MonitoringUpdate{Capacity: u.GetCapacity(), Features: u.GetFeatures()}
	if f.sel == nil {
		out.Groups, out.Labs, out.Clients, out.Policies = u.Groups, u.Labs, u.Clients, u.Policies
		out.Traffic, out.DeletedKeys = u.Traffic, u.DeletedKeys
		return out
	}
	for _, g := range u.Groups {
		if f.matches(labelsOf(recordKey("lab_group", g.GetName(), "", g.GetName()))) {
			out.Groups = append(out.Groups, g)
		}
	}
	for _, l := range u.Labs {
		if f.matches(labelsOf(recordKey("lab", l.GetLabGroupName(), l.GetNamespace(), l.GetName()))) {
			out.Labs = append(out.Labs, l)
		}
	}
	for _, c := range u.Clients {
		if f.matches(labelsOf(recordKey("client", c.GetLabGroupName(), c.GetNamespace(), c.GetName()))) {
			out.Clients = append(out.Clients, c)
		}
	}
	for _, p := range u.Policies {
		if f.matches(labelsOf(recordKey("access_policy", p.GetLabGroupName(), p.GetNamespace(), "access-policy"))) {
			out.Policies = append(out.Policies, p)
		}
	}
	for _, r := range u.Traffic {
		if len(r.GetLedger()) == 0 {
			if emptyGroups[r.GetLabGroupName()] {
				out.Traffic = append(out.Traffic, r)
			}
			continue
		}
		if cut := f.cutTraffic(r, labOf); cut != nil {
			out.Traffic = append(out.Traffic, cut)
		} else if emptyGroups[r.GetLabGroupName()] {
			// Selected idle labs still retain their authorized observation header.
			out.Traffic = append(out.Traffic, cloneTrafficMetadata(r))
		}
	}
	for _, k := range u.DeletedKeys {
		// Labs, clients and policies of one group are deleted with their labels as
		// they were: the journal kept them, so a deletion matches like the object did.
		if f.matches(labelsOf(monitoringDeletedKeyString(k))) {
			out.DeletedKeys = append(out.DeletedKeys, k)
		}
	}
	return out
}

// cutTraffic keeps the touches of matching labs; nil when none remain.
func (f *selectorFilter) cutTraffic(r *protobuf.TrafficReport, labOf func(group, lab string) map[string]string) *protobuf.TrafficReport {
	var kept []*protobuf.TrafficTouch
	for _, t := range r.GetLedger() {
		if f.matches(labOf(r.GetLabGroupName(), t.GetLabName())) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	if len(kept) == len(r.GetLedger()) {
		return r
	}
	cut := cloneTrafficMetadata(r)
	cut.Ledger = kept
	return cut
}

// accumulator merges filtered updates into one: the newest version of every
// record wins and a deletion cancels the record (and the other way round).
type accumulator struct {
	records  map[string]monitoringRecord
	deleted  map[string]*protobuf.MonitoringDeletedKey
	capacity *protobuf.CapacityResponse
	features *protobuf.FeaturesResponse
}

func newAccumulator() *accumulator {
	return &accumulator{records: map[string]monitoringRecord{}, deleted: map[string]*protobuf.MonitoringDeletedKey{}}
}

func (a *accumulator) empty() bool {
	return len(a.records) == 0 && len(a.deleted) == 0 && a.capacity == nil && a.features == nil
}

func (a *accumulator) add(u *protobuf.MonitoringUpdate) {
	if u == nil {
		return
	}
	for key, rec := range monitoringRecordIndex(u) {
		a.records[key] = rec
		delete(a.deleted, key)
	}
	for _, k := range u.DeletedKeys {
		key := monitoringDeletedKeyString(k)
		delete(a.records, key)
		a.deleted[key] = k
	}
	if u.Capacity != nil {
		a.capacity = u.Capacity
	}
	if u.Features != nil {
		a.features = u.Features
	}
}

// take returns the merged update and empties the accumulator.
func (a *accumulator) take() *protobuf.MonitoringUpdate {
	out := &protobuf.MonitoringUpdate{Capacity: a.capacity, Features: a.features}
	keys := make([]string, 0, len(a.records))
	for k := range a.records {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		appendMonitoringRecord(out, a.records[k])
	}
	dk := make([]string, 0, len(a.deleted))
	for k := range a.deleted {
		dk = append(dk, k)
	}
	sort.Strings(dk)
	for _, k := range dk {
		out.DeletedKeys = append(out.DeletedKeys, a.deleted[k])
	}
	a.records = map[string]monitoringRecord{}
	a.deleted = map[string]*protobuf.MonitoringDeletedKey{}
	a.capacity, a.features = nil, nil
	return out
}
