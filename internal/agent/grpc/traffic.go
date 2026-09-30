package grpc

import (
	"sort"

	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
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
		merged.Partial = merged.Partial || r.GetPartial()
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
			row.Attempts += t.GetAttempts()
			row.PacketsOut += t.GetPacketsOut()
			row.PacketsIn += t.GetPacketsIn()
			row.BytesOut += t.GetBytesOut()
			row.BytesIn += t.GetBytesIn()
			if t.GetFirstSeenUnixMs() < row.FirstSeenUnixMs {
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
