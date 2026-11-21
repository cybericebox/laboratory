package grpc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func activityEvent(seq, at int64, kind laboratoryv1alpha1.ActivityKind) laboratoryv1alpha1.ActivityEvent {
	return laboratoryv1alpha1.ActivityEvent{Seq: seq, AtMs: at, Kind: kind, LabName: "web", Client: "p1", Device: "kali", SessionID: "s", DstAddr: "10.8.1.5", SourceAddr: "10.8.1.9", DstPort: 443, BytesUp: seq}
}

func activityFixture(t *testing.T, enabled bool) (*Handler, string) {
	t.Helper()
	h, k8s := newTestHandler(t)
	h.features.ActivityReports = enabled
	const ns = "team-activity"
	readyGroup(t, h, k8s, "act", ns, nil)
	return h, ns
}

func putReport(t *testing.T, h *Handler, ns, name string, updated int64, events ...laboratoryv1alpha1.ActivityEvent) {
	t.Helper()
	r := &laboratoryv1alpha1.LabActivityReport{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Status: laboratoryv1alpha1.LabActivityReportStatus{Events: events, UpdatedMs: updated}}
	created, err := h.cs.LaboratoryV1alpha1().LabActivityReports(ns).Create(context.Background(), r, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// the status is its own subresource: it is written the way the writers write it
	created.Status = r.Status
	if _, err := h.cs.LaboratoryV1alpha1().LabActivityReports(ns).UpdateStatus(context.Background(), created, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func pull(t *testing.T, h *Handler, req *protobuf.GetLabGroupActivityRequest) *protobuf.GetLabGroupActivityResponse {
	t.Helper()
	req.LabGroupName = "act"
	res, err := h.GetLabGroupActivity(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestActivityReportsAreOffByDefault(t *testing.T) {
	h, _ := activityFixture(t, false)
	_, err := h.GetLabGroupActivity(context.Background(), &protobuf.GetLabGroupActivityRequest{LabGroupName: "act"})
	if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != "activity reports disabled" {
		t.Fatalf("err = %v", err)
	}
	if f := h.activityFeature(); f.Enabled || f.MaxEvents != 1000 || f.RingMinutes != 60 {
		t.Fatalf("feature = %+v", f)
	}
	h.features.ActivityReports = true
	if !h.activityFeature().Enabled {
		t.Fatal("the feature must follow the switch")
	}
}

func TestActivityPullIsIncrementalAndMergedInTimeOrder(t *testing.T) {
	h, ns := activityFixture(t, true)
	now := time.Now().UnixMilli()
	putReport(t, h, ns, "vpn", now, activityEvent(10, now-300, laboratoryv1alpha1.ActivityTouch), activityEvent(11, now-100, laboratoryv1alpha1.ActivityTouch))
	putReport(t, h, ns, "proxy-a", now, activityEvent(5, now-200, laboratoryv1alpha1.ActivityConsole))

	first := pull(t, h, &protobuf.GetLabGroupActivityRequest{})
	if len(first.Events) != 3 || first.HasMore || first.Gap {
		t.Fatalf("first = %+v", first)
	}
	if first.Events[0].AtMs > first.Events[1].AtMs || first.Events[1].AtMs > first.Events[2].AtMs {
		t.Fatalf("not in time order: %+v", first.Events)
	}
	if first.Events[1].GetConsole() == nil || first.Events[0].GetTouch() == nil || first.Events[0].GetTouch().DstAddress != "10.8.1.5" || first.Events[0].LabName != "web" {
		t.Fatalf("event shapes: %+v", first.Events)
	}

	again := pull(t, h, &protobuf.GetLabGroupActivityRequest{Cursor: first.NextCursor})
	if len(again.Events) != 0 || again.NextCursor != first.NextCursor {
		t.Fatalf("nothing is new: %+v", again)
	}

	// a new event of one writer only
	r, _ := h.cs.LaboratoryV1alpha1().LabActivityReports(ns).Get(context.Background(), "vpn", metav1.GetOptions{})
	r.Status.Events = append(r.Status.Events, activityEvent(12, now, laboratoryv1alpha1.ActivityTouch))
	if _, err := h.cs.LaboratoryV1alpha1().LabActivityReports(ns).UpdateStatus(context.Background(), r, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	next := pull(t, h, &protobuf.GetLabGroupActivityRequest{Cursor: first.NextCursor})
	if len(next.Events) != 1 || next.Events[0].GetTouch().BytesOut != 12 {
		t.Fatalf("next = %+v", next)
	}
}

func TestActivityPagesAndKindFilter(t *testing.T) {
	h, ns := activityFixture(t, true)
	now := time.Now().UnixMilli()
	var evs []laboratoryv1alpha1.ActivityEvent
	for i := 1; i <= 7; i++ {
		kind := laboratoryv1alpha1.ActivityTouch
		if i%2 == 0 {
			kind = laboratoryv1alpha1.ActivityEgress
		}
		evs = append(evs, activityEvent(int64(i), now+int64(i), kind))
	}
	putReport(t, h, ns, "vpn", now, evs...)
	seen := 0
	cursor := ""
	for page := 0; page < 10; page++ {
		res := pull(t, h, &protobuf.GetLabGroupActivityRequest{Cursor: cursor, Limit: 3})
		seen += len(res.Events)
		cursor = res.NextCursor
		if !res.HasMore {
			break
		}
		if len(res.Events) != 3 {
			t.Fatalf("a page with more behind it is full: %d", len(res.Events))
		}
	}
	if seen != 7 {
		t.Fatalf("pages gave %d events", seen)
	}
	only := pull(t, h, &protobuf.GetLabGroupActivityRequest{Kinds: []protobuf.ActivityKind{protobuf.ActivityKind_ACTIVITY_KIND_INTERNET_EGRESS}})
	if len(only.Events) != 3 || only.Events[0].GetEgress() == nil || only.Events[0].GetEgress().SourceAddress != "10.8.1.9" || only.Events[0].GetEgress().DstPort != 443 {
		t.Fatalf("egress only: %+v", only.Events)
	}
}

func TestActivityGapAndBadCursorAndForeignGroup(t *testing.T) {
	h, ns := activityFixture(t, true)
	now := time.Now().UnixMilli()
	putReport(t, h, ns, "vpn", now, activityEvent(50, now, laboratoryv1alpha1.ActivityTouch))
	r, _ := h.cs.LaboratoryV1alpha1().LabActivityReports(ns).Get(context.Background(), "vpn", metav1.GetOptions{})
	r.Status.DroppedThrough = 40
	if _, err := h.cs.LaboratoryV1alpha1().LabActivityReports(ns).UpdateStatus(context.Background(), r, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if res := pull(t, h, &protobuf.GetLabGroupActivityRequest{}); res.Gap {
		t.Fatal("the first pull has no cursor to have missed anything")
	}
	if res := pull(t, h, &protobuf.GetLabGroupActivityRequest{Cursor: encodeActivityCursor(map[string]int64{"vpn": 30})}); !res.Gap {
		t.Fatal("a cursor below what was dropped must report the gap")
	}
	if res := pull(t, h, &protobuf.GetLabGroupActivityRequest{Cursor: encodeActivityCursor(map[string]int64{"vpn": 45})}); res.Gap {
		t.Fatal("a cursor past the dropped events has no gap")
	}
	for _, bad := range []string{"junk", "a1.!!!", activityCursorTag + "bm90anNvbg"} {
		_, err := h.GetLabGroupActivity(context.Background(), &protobuf.GetLabGroupActivityRequest{LabGroupName: "act", Cursor: bad})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("cursor %q: %v", bad, err)
		}
	}
	_, err := h.GetLabGroupActivity(context.Background(), &protobuf.GetLabGroupActivityRequest{LabGroupName: "nope"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("missing group: %v", err)
	}
	_, err = h.GetLabGroupActivity(context.Background(), &protobuf.GetLabGroupActivityRequest{LabGroupName: "act", Kinds: []protobuf.ActivityKind{protobuf.ActivityKind_ACTIVITY_KIND_UNSPECIFIED}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unspecified kind: %v", err)
	}
}

func TestActivityReapsReportsOfWritersThatAreGone(t *testing.T) {
	h, ns := activityFixture(t, true)
	old := time.Now().Add(-3 * time.Hour).UnixMilli()
	putReport(t, h, ns, "proxy-gone", old)
	putReport(t, h, ns, "proxy-gone-with-events", old, activityEvent(1, old, laboratoryv1alpha1.ActivityConsole))
	putReport(t, h, ns, "proxy-live", time.Now().UnixMilli())
	pull(t, h, &protobuf.GetLabGroupActivityRequest{})
	list, _ := h.cs.LaboratoryV1alpha1().LabActivityReports(ns).List(context.Background(), metav1.ListOptions{})
	var got []string
	for _, r := range list.Items {
		got = append(got, r.Name)
	}
	if fmt.Sprint(got) != "[proxy-gone-with-events proxy-live]" {
		t.Fatalf("reports left: %v", got)
	}
}

// The feature tells the daemon how long a console session can live (an open one older than that has ended) and whether devices-touched
// events exist at all, so it reports "not reported" and not zero when they cannot.
func TestActivityFeatureReportsConsoleLifetimeAndDeviceTouch(t *testing.T) {
	h, _ := activityFixture(t, true)
	h.features.TerminalEnabled, h.features.TerminalServed, h.features.TerminalMaxLifetime = true, true, 8*time.Hour
	h.features.VPNFirewall = "iptables"
	f := h.activityFeature()
	if !f.Enabled || f.ConsoleMaxLifetimeS != 8*3600 || !f.DeviceTouch {
		t.Fatalf("iptables: %+v", f)
	}
	h.features.VPNFirewall = "" // the default is iptables
	if !h.activityFeature().DeviceTouch {
		t.Fatal("an unset firewall is iptables")
	}
	h.features.VPNFirewall = "nft"
	if f := h.activityFeature(); f.DeviceTouch || !f.Enabled {
		t.Fatalf("nft keeps no connection tracking, so no touch events: %+v", f)
	}
	h.features.VPNFirewall = "iptables"
	h.features.TerminalEnabled = false
	if f := h.activityFeature(); f.ConsoleMaxLifetimeS != 0 {
		t.Fatalf("no console, no lifetime: %+v", f)
	}
	h.features.ActivityReports = false
	if f := h.activityFeature(); f.Enabled || f.DeviceTouch {
		t.Fatalf("off: %+v", f)
	}
}
