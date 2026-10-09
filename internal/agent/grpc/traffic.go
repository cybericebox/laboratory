package grpc

import (
	"math"
	"sort"
	"strconv"

	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const (
	mergedProxySource = "proxy"
	// replicaLiveWindow: a replica whose report is older than this is gone (its
	// totals still count, its coverage does not).
	replicaLiveWindowMs = 3 * 60 * 1000
)

// mergeProxyReports folds the reports of every proxy replica of one group into
// one series. Each replica counts only the requests it served, so the totals
// add up; a replica that was replaced keeps its last report, which keeps the
// sum from ever going down. Coverage is the union of the live replicas: a group
// is watched while at least one replica is.
func mergeProxyReports(reports []*protobuf.TrafficReport) *protobuf.TrafficReport {
	if len(reports) == 0 {
		return nil
	}
	reports = distinctProxyReports(reports)
	first := reports[0]
	merged := &protobuf.TrafficReport{
		LabGroupName: first.GetLabGroupName(), Namespace: first.GetNamespace(),
		Source: mergedProxySource, Kind: "proxy", BootId: mergedProxySource,
	}
	var latest int64
	for _, r := range reports {
		if r.GetCoveredToUnixMs() > latest {
			latest = r.GetCoveredToUnixMs()
		}
	}
	type key struct{ subject, lab string }
	rows := map[key]*protobuf.TrafficTouch{}
	for _, r := range reports {
		merged.Truncated = merged.Truncated || r.GetTruncated()
		merged.Partial = merged.Partial || r.GetPartial() || r.GetTruncated()
		coverage := reportCoverage(r)
		for _, span := range coverage {
			merged.CoverageSpans = append(merged.CoverageSpans, span)
			merged.Partial = merged.Partial || span.GetPartial()
		}
		// Another replica observing this interval cannot recover requests that
		// the restarted writer may have served in its own unobserved gap.
		merged.Partial = merged.Partial || coverageHistoryHasGap(coverage)
		if latest-r.GetCoveredToUnixMs() <= replicaLiveWindowMs {
			if merged.CoveredFromUnixMs == 0 || (r.GetCoveredFromUnixMs() > 0 && r.GetCoveredFromUnixMs() < merged.CoveredFromUnixMs) {
				merged.CoveredFromUnixMs = r.GetCoveredFromUnixMs()
			}
		}
		for _, t := range r.GetLedger() {
			k := key{t.GetSubject(), t.GetLabName()}
			row := rows[k]
			if row == nil {
				row = &protobuf.TrafficTouch{Subject: t.GetSubject(), LabName: t.GetLabName(), FirstSeenUnixMs: t.GetFirstSeenUnixMs()}
				rows[k] = row
			}
			add := func(dst *int64, value int64) {
				var incomplete bool
				*dst, incomplete = addTrafficCounter(*dst, value)
				merged.Partial = merged.Partial || incomplete
			}
			add(&row.Attempts, t.GetAttempts())
			add(&row.LabInitiatedAttempts, t.GetLabInitiatedAttempts())
			add(&row.PacketsOut, t.GetPacketsOut())
			add(&row.PacketsIn, t.GetPacketsIn())
			add(&row.BytesOut, t.GetBytesOut())
			add(&row.BytesIn, t.GetBytesIn())
			if ms := t.GetFirstSeenUnixMs(); ms > 0 && (row.FirstSeenUnixMs <= 0 || ms < row.FirstSeenUnixMs) {
				row.FirstSeenUnixMs = t.GetFirstSeenUnixMs()
			}
			if t.GetLastSeenUnixMs() > row.LastSeenUnixMs {
				row.LastSeenUnixMs = t.GetLastSeenUnixMs()
			}
			if ms := t.GetFirstRespondedUnixMs(); ms > 0 && (row.FirstRespondedUnixMs == 0 || ms < row.FirstRespondedUnixMs) {
				row.FirstRespondedUnixMs = ms
			}
		}
	}
	merged.CoveredToUnixMs = latest
	sort.Slice(merged.CoverageSpans, func(i, j int) bool {
		return coverageSpanLess(merged.CoverageSpans[i], merged.CoverageSpans[j])
	})
	merged.Partial = merged.Partial || coverageHasGap(merged.CoverageSpans, merged.CoveredFromUnixMs, latest)
	for _, row := range rows {
		merged.Ledger = append(merged.Ledger, row)
	}
	sort.Slice(merged.Ledger, func(i, j int) bool {
		a, b := merged.Ledger[i], merged.Ledger[j]
		if a.GetSubject() != b.GetSubject() {
			return a.GetSubject() < b.GetSubject()
		}
		return a.GetLabName() < b.GetLabName()
	})
	return merged
}

// A report key is one writer's cumulative state, not another contribution on
// replay. Prefer its newest observation if it appeared more than once. Anonymous
// legacy inputs cannot safely be identified as one writer and remain distinct.
func distinctProxyReports(reports []*protobuf.TrafficReport) []*protobuf.TrafficReport {
	type identity struct{ group, namespace, source string }
	out := make([]*protobuf.TrafficReport, 0, len(reports))
	positions := map[identity]int{}
	for i, r := range reports {
		source := r.GetSource()
		if source == "" {
			source = r.GetInstance()
		}
		if source == "" {
			source = r.GetBootId()
		}
		if source == "" {
			source = "\x00anonymous-" + strconv.Itoa(i)
		}
		key := identity{r.GetLabGroupName(), r.GetNamespace(), source}
		if at, ok := positions[key]; ok {
			if r.GetCoveredToUnixMs() >= out[at].GetCoveredToUnixMs() {
				out[at] = r
			}
			continue
		}
		positions[key] = len(out)
		out = append(out, r)
	}
	return out
}

func addTrafficCounter(a, b int64) (int64, bool) {
	if b < 0 {
		return a, true
	}
	if a > math.MaxInt64-b {
		return math.MaxInt64, true
	}
	return a + b, false
}

// reportCoverage never mutates a published report. Unknown/future span metadata
// survives cloning; current writer identity fills only missing legacy fields.
func reportCoverage(r *protobuf.TrafficReport) []*protobuf.TrafficCoverageSpan {
	spans := r.GetCoverageSpans()
	legacy := len(spans) == 0
	if legacy {
		spans = []*protobuf.TrafficCoverageSpan{{FromUnixMs: r.GetCoveredFromUnixMs(), ToUnixMs: r.GetCoveredToUnixMs()}}
	}
	out := make([]*protobuf.TrafficCoverageSpan, 0, len(spans))
	for _, raw := range spans {
		s := proto.Clone(raw).(*protobuf.TrafficCoverageSpan)
		if legacy && s.Source == "" {
			s.Source = r.GetSource()
		}
		if legacy && s.Instance == "" {
			s.Instance = r.GetInstance()
		}
		if legacy && s.BootId == "" {
			s.BootId = r.GetBootId()
		}
		s.Partial = s.Partial || (legacy && (r.GetPartial() || r.GetTruncated())) || s.FromUnixMs <= 0 || s.ToUnixMs < s.FromUnixMs
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].GetFromUnixMs() != out[j].GetFromUnixMs() {
			return out[i].GetFromUnixMs() < out[j].GetFromUnixMs()
		}
		return out[i].GetToUnixMs() < out[j].GetToUnixMs()
	})
	return out
}

func coverageHistoryHasGap(spans []*protobuf.TrafficCoverageSpan) bool {
	if len(spans) < 2 {
		return false
	}
	var to int64
	for _, s := range spans {
		to = max(to, s.GetToUnixMs())
	}
	return coverageHasGap(spans, spans[0].GetFromUnixMs(), to)
}

func coverageHasGap(spans []*protobuf.TrafficCoverageSpan, from, to int64) bool {
	if from <= 0 || to < from {
		return true
	}
	covered := from
	for _, s := range spans {
		if s.GetPartial() || s.GetFromUnixMs() <= 0 || s.GetToUnixMs() < s.GetFromUnixMs() {
			continue
		}
		if s.GetToUnixMs() < covered {
			continue
		}
		if s.GetFromUnixMs() > covered {
			return true
		}
		covered = max(covered, s.GetToUnixMs())
		if covered >= to {
			return false
		}
	}
	return covered < to
}

// Published rows are immutable. Clone metadata generically so additive fields,
// nested coverage and unknown bytes survive without a deep clone of the ledger.
func cloneTrafficMetadata(r *protobuf.TrafficReport) *protobuf.TrafficReport {
	src := r.ProtoReflect()
	header := src.New()
	ledger := src.Descriptor().Fields().ByName("ledger")
	src.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd != ledger {
			header.Set(fd, v)
		}
		return true
	})
	header.SetUnknown(src.GetUnknown())
	return proto.Clone(header.Interface()).(*protobuf.TrafficReport)
}

// Call only after conversion, ID restoration and replica folding, before the
// next state is published. A new header preserves every heartbeat's own facts.
func shareTrafficLedgers(previous, next *protobuf.MonitoringUpdate) {
	type identity struct{ group, namespace, source string }
	old := make(map[identity]*protobuf.TrafficReport, len(previous.GetTraffic()))
	for _, r := range previous.GetTraffic() {
		old[identity{r.GetLabGroupName(), r.GetNamespace(), r.GetSource()}] = r
	}
	for _, r := range next.GetTraffic() {
		p := old[identity{r.GetLabGroupName(), r.GetNamespace(), r.GetSource()}]
		if p == nil || p.GetBootId() != r.GetBootId() || p.GetKind() != r.GetKind() || p.GetInstance() != r.GetInstance() || len(p.GetLedger()) != len(r.GetLedger()) {
			continue
		}
		equal := true
		for i, t := range r.GetLedger() {
			if !proto.Equal(p.Ledger[i], t) {
				equal = false
				break
			}
		}
		if equal {
			r.Ledger = p.Ledger
		}
	}
}

func coverageSpanLess(a, b *protobuf.TrafficCoverageSpan) bool {
	if a.GetFromUnixMs() != b.GetFromUnixMs() {
		return a.GetFromUnixMs() < b.GetFromUnixMs()
	}
	if a.GetToUnixMs() != b.GetToUnixMs() {
		return a.GetToUnixMs() < b.GetToUnixMs()
	}
	if a.GetSource() != b.GetSource() {
		return a.GetSource() < b.GetSource()
	}
	if a.GetInstance() != b.GetInstance() {
		return a.GetInstance() < b.GetInstance()
	}
	return a.GetBootId() < b.GetBootId()
}
