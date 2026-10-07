package grpc

import (
	"bytes"
	"context"
	"math"
	"testing"

	labv1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"time"
)

func coverageSpans(t *testing.T, r *protobuf.TrafficReport) protoreflect.List {
	t.Helper()
	m := r.ProtoReflect()
	fd := m.Descriptor().Fields().ByName("coverage_spans")
	if fd == nil || fd.Number() != 12 || !fd.IsList() {
		t.Fatal("public additive coverage_spans field12 is missing")
	}
	return m.Get(fd).List()
}
func TestTrafficCoverageContract(t *testing.T) {
	r := trafficReportToProto(&labv1.LabTrafficReport{ObjectMeta: metav1.ObjectMeta{Name: "vpn", Namespace: "ns"}, Spec: labv1.LabTrafficReportSpec{Kind: labv1.LabTrafficSurfaceVPN, Instance: "pod"}, Status: labv1.LabTrafficReportStatus{BootID: "boot", CoveredFromMs: 1000, CoveredToMs: 2000, Truncated: true}}, "g")
	raw, err := proto.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	wire := &protobuf.TrafficReport{}
	if err := proto.Unmarshal(raw, wire); err != nil {
		t.Fatal(err)
	}
	spans := coverageSpans(t, wire)
	if spans.Len() != 1 {
		t.Fatalf("idle coverage spans=%d", spans.Len())
	}
	s := spans.Get(0).Message()
	for key, want := range map[protoreflect.Name]int64{"from_unix_ms": 1000, "to_unix_ms": 2000} {
		if got := s.Get(s.Descriptor().Fields().ByName(key)).Int(); got != want {
			t.Fatalf("%s=%d", key, got)
		}
	}
	if !s.Get(s.Descriptor().Fields().ByName("partial")).Bool() || !wire.Partial {
		t.Fatal("truncated coverage must be incomplete")
	}
	if wire.GetCoveredFromUnixMs() != 1000 || wire.GetCoveredToUnixMs() != 2000 {
		t.Fatal("legacy scalar coverage changed")
	}
	for _, private := range []protoreflect.Name{"kernel_checkpoints", "client_cidr", "binding_id", "epoch"} {
		if wire.ProtoReflect().Descriptor().Fields().ByName(private) != nil {
			t.Fatalf("private field %s is public", private)
		}
	}
}

func TestExplicitCoverageKeepsHistoricalCompleteness(t *testing.T) {
	r := trafficReportToProto(&labv1.LabTrafficReport{Status: labv1.LabTrafficReportStatus{BootID: "new", CoveredFromMs: 4000, CoveredToMs: 5000, Partial: true, CoverageSpans: []labv1.LabTrafficCoverageSpan{{FromMs: 1000, ToMs: 2000, BootID: "old"}, {FromMs: 4000, ToMs: 5000, Partial: true, BootID: "new"}}}}, "g")
	if !r.Partial || len(r.CoverageSpans) != 2 || r.CoverageSpans[0].Partial || !r.CoverageSpans[1].Partial {
		t.Fatalf("current failure changed old interval facts: %v", r)
	}
}

func TestExplicitHistoricalCoverageDoesNotInventBoot(t *testing.T) {
	r := trafficReportToProto(&labv1.LabTrafficReport{ObjectMeta: metav1.ObjectMeta{Name: "vpn"}, Spec: labv1.LabTrafficReportSpec{Instance: "new-writer"}, Status: labv1.LabTrafficReportStatus{BootID: "new", CoverageSpans: []labv1.LabTrafficCoverageSpan{{FromMs: 1000, ToMs: 2000}}}}, "g")
	if r.CoverageSpans[0].BootId != "" || r.CoverageSpans[0].Instance != "" {
		t.Fatal("unknown historical identity replaced by new boot", r.CoverageSpans[0])
	}
}

func TestProxyMergeDeduplicatesSource(t *testing.T) {
	a := &protobuf.TrafficReport{LabGroupName: "g", Namespace: "ns", Source: "proxy-a", Instance: "a", BootId: "boot-a", CoveredFromUnixMs: 1000, CoveredToUnixMs: 2000, Ledger: []*protobuf.TrafficTouch{{Subject: "p", LabName: "l", Attempts: 3}}}
	b := proto.Clone(a).(*protobuf.TrafficReport)
	b.Source = "proxy-b"
	b.Instance = "b"
	b.BootId = "boot-b"
	b.Ledger[0].Attempts = 4
	got := mergeProxyReports([]*protobuf.TrafficReport{a, proto.Clone(a).(*protobuf.TrafficReport), b})
	if got.Ledger[0].Attempts != 7 {
		t.Fatalf("duplicate replica counted: got=%d want=7", got.Ledger[0].Attempts)
	}
	if coverageSpans(t, got).Len() != 2 {
		t.Fatal("duplicate replica coverage")
	}
}
func TestProxyMergeSaturatesSignedCounters(t *testing.T) {
	a := &protobuf.TrafficReport{Source: "a", Ledger: []*protobuf.TrafficTouch{{Subject: "p", LabName: "l", Attempts: math.MaxInt64, LabInitiatedAttempts: math.MaxInt64, PacketsOut: math.MaxInt64, PacketsIn: math.MaxInt64, BytesOut: math.MaxInt64, BytesIn: math.MaxInt64}}}
	b := &protobuf.TrafficReport{Source: "b", Ledger: []*protobuf.TrafficTouch{{Subject: "p", LabName: "l", Attempts: 1, LabInitiatedAttempts: 1, PacketsOut: 1, PacketsIn: 1, BytesOut: 1, BytesIn: 1}}}
	got := mergeProxyReports([]*protobuf.TrafficReport{a, b})
	row := got.Ledger[0]
	for _, v := range []int64{row.Attempts, row.LabInitiatedAttempts, row.PacketsOut, row.PacketsIn, row.BytesOut, row.BytesIn} {
		if v != math.MaxInt64 {
			t.Fatalf("counter overflowed/lost: %d", v)
		}
	}
	if !got.Partial {
		t.Fatal("saturation must mark incomplete facts")
	}
}
func TestProxyMergeKeepsDisjointCoverage(t *testing.T) {
	a := &protobuf.TrafficReport{Source: "a", CoveredFromUnixMs: 1000, CoveredToUnixMs: 2000}
	b := &protobuf.TrafficReport{Source: "b", CoveredFromUnixMs: 4000, CoveredToUnixMs: 5000}
	got := mergeProxyReports([]*protobuf.TrafficReport{a, b})
	spans := coverageSpans(t, got)
	if spans.Len() != 2 || !got.Partial {
		t.Fatal("disjoint spans must not become complete scalar envelope")
	}
	if a.Partial || b.Partial {
		t.Fatal("merge changed cached replica facts")
	}
}

func TestHealthyReplicaDoesNotHideOtherWritersCoverageGap(t *testing.T) {
	a := &protobuf.TrafficReport{Source: "healthy", CoveredFromUnixMs: 1000, CoveredToUnixMs: 5000}
	b := &protobuf.TrafficReport{Source: "restarted", CoveredFromUnixMs: 4000, CoveredToUnixMs: 5000, CoverageSpans: []*protobuf.TrafficCoverageSpan{{FromUnixMs: 1000, ToUnixMs: 2000}, {FromUnixMs: 4000, ToUnixMs: 5000}}}
	got := mergeProxyReports([]*protobuf.TrafficReport{a, b})
	if !got.Partial {
		t.Fatal("healthy replica concealed unobserved writer interval")
	}
	if len(got.CoverageSpans) != 3 || got.CoverageSpans[0].Partial || got.CoverageSpans[2].Partial {
		t.Fatal("known complete individual spans were rewritten")
	}
}

func TestReadyProxyWithoutBootTimeCannotProveComplete(t *testing.T) {
	r := newMonRig(t, MonitoringConfig{})
	r.h.SetErrorJournal(ErrorJournalConfig{ReleaseNamespace: names.SystemNamespace})
	r.group("g", "ns", nil)
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "proxy", Namespace: names.ProxyNamespace, Labels: map[string]string{"app": "laboratory-proxy-l7"}}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "l7", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	if _, err := r.h.k8s.CoreV1().Pods(names.ProxyNamespace).Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.cs.LaboratoryV1alpha1().LabTrafficReports("ns").Create(context.Background(), &labv1.LabTrafficReport{ObjectMeta: metav1.ObjectMeta{Name: "proxy-pod", Namespace: "ns"}, Spec: labv1.LabTrafficReportSpec{Kind: labv1.LabTrafficSurfaceProxy, Instance: "proxy"}, Status: labv1.LabTrafficReportStatus{CoveredFromMs: 1000, CoveredToMs: 5000}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	r.poll()
	if !r.h.monitor().state.update.Traffic[0].Partial {
		t.Fatal("unknown current boot treated as reported")
	}
}

func TestEmptyCoverageMatchesAuthorizedScope(t *testing.T) {
	r := &protobuf.TrafficReport{LabGroupName: "g", Namespace: "ns", Source: "vpn", Kind: "vpn", CoveredFromUnixMs: 1000, CoveredToUnixMs: 2000}
	st := &monState{update: &protobuf.MonitoringUpdate{Traffic: []*protobuf.TrafficReport{r}}, labels: map[string]map[string]string{recordKey("lab_group", "g", "", "g"): {names.LabelTenant: "owner", "instance": "group"}}, labLabels: map[string]map[string]string{labLabelKey("g", "lab"): {names.LabelTenant: "owner", "instance": "selected"}}}
	for _, test := range []struct {
		name, selector, tenant string
		visible                bool
	}{{"own-group", "", "owner", true}, {"selected-lab", "instance=selected", "owner", true}, {"nonmatching", "instance=other", "owner", false}, {"foreign", "", "foreign", false}} {
		t.Run(test.name, func(t *testing.T) {
			f, err := newSelectorFilter(test.selector, test.tenant)
			if err != nil {
				t.Fatal(err)
			}
			got := f.snapshot(st)
			if (len(got.Traffic) == 1) != test.visible {
				t.Fatalf("empty coverage visibility=%v want=%v", len(got.Traffic) == 1, test.visible)
			}
		})
	}
}

func TestPartialTrafficSharesRowsPreservesMetadata(t *testing.T) {
	r := &protobuf.TrafficReport{LabGroupName: "g", Namespace: "ns", Source: "vpn", Kind: "vpn", BootId: "boot", CoveredFromUnixMs: 1000, CoveredToUnixMs: 2000, Ledger: []*protobuf.TrafficTouch{{LabName: "keep", Attempts: 1}, {LabName: "drop", Attempts: 2}}}
	r.CoverageSpans = []*protobuf.TrafficCoverageSpan{{FromUnixMs: 1000, ToUnixMs: 2000, BootId: "old"}}
	unknown := protowire.AppendTag(nil, 101, protowire.BytesType)
	unknown = protowire.AppendBytes(unknown, []byte("future metadata"))
	r.ProtoReflect().SetUnknown(unknown)
	r.CoverageSpans[0].ProtoReflect().SetUnknown(unknown)
	before, err := proto.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	f := &selectorFilter{sel: nil}
	f, err = newSelectorFilter("instance=keep", "owner")
	if err != nil {
		t.Fatal(err)
	}
	cut := f.cutTraffic(r, func(g, l string) map[string]string {
		return map[string]string{names.LabelTenant: "owner", "instance": l}
	})
	if cut == nil || len(cut.Ledger) != 1 || cut.Ledger[0] != r.Ledger[0] {
		t.Fatal("partial filter deep-copied immutable touch")
	}
	if cut.BootId != "boot" || cut.CoveredToUnixMs != 2000 || !bytes.Equal(cut.ProtoReflect().GetUnknown(), unknown) {
		t.Fatal("metadata lost")
	}
	cut.Source = "other"
	if len(cut.CoverageSpans) != 1 || cut.CoverageSpans[0] == r.CoverageSpans[0] || !bytes.Equal(cut.CoverageSpans[0].ProtoReflect().GetUnknown(), unknown) {
		t.Fatal("nested coverage metadata lost or shared mutably")
	}
	cut.CoverageSpans[0].BootId = "different"
	cut.CoverageSpans[0].ProtoReflect().GetUnknown()[len(unknown)-1] ^= 1
	cut.ProtoReflect().GetUnknown()[len(unknown)-1] ^= 1
	after, err := proto.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("filter changed source metadata")
	}
}

func TestHeartbeatLedgerSharingPreservesOldJournal(t *testing.T) {
	r := newMonRig(t, MonitoringConfig{})
	r.group("g", "ns", nil)
	r.lab("ns", "lab", nil)
	obj := &labv1.LabTrafficReport{ObjectMeta: metav1.ObjectMeta{Name: "vpn", Namespace: "ns"}, Spec: labv1.LabTrafficReportSpec{Kind: labv1.LabTrafficSurfaceVPN}, Status: labv1.LabTrafficReportStatus{BootID: "boot", CoveredFromMs: 1000, CoveredToMs: 2000, Ledger: []labv1.LabTrafficTouch{{Subject: "p", LabName: "lab", Attempts: 1}}}}
	api := r.cs.LaboratoryV1alpha1().LabTrafficReports("ns")
	if _, err := api.Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	r.poll()
	prev := r.h.monitor().state.update.Traffic[0]
	obj.Status.CoveredToMs = 3000
	if _, err := api.UpdateStatus(context.Background(), obj, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	r.poll()
	next := r.h.monitor().state.update.Traffic[0]
	if prev == next || prev.CoveredToUnixMs != 2000 || next.CoveredToUnixMs != 3000 {
		t.Fatal("heartbeat overwrote old header")
	}
	if prev.Ledger[0] != next.Ledger[0] || &prev.Ledger[0] != &next.Ledger[0] {
		t.Fatal("heartbeat cloned unchanged ledger")
	}
	obj.Status.Ledger[0].Attempts = 2
	obj.Status.CoveredToMs = 4000
	if _, err := api.UpdateStatus(context.Background(), obj, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	r.poll()
	changed := r.h.monitor().state.update.Traffic[0]
	if changed.Ledger[0] == next.Ledger[0] || next.Ledger[0].Attempts != 1 || changed.Ledger[0].Attempts != 2 {
		t.Fatal("new counter changed history")
	}
	if len(r.h.monitor().journal) != 2 || r.h.monitor().journal[0].update.Traffic[0].CoveredToUnixMs != 3000 {
		t.Fatal("journal replay lost heartbeat")
	}
}

func TestReadyProxyReplicaRequiresCurrentCoverage(t *testing.T) {
	for _, test := range []struct {
		name     string
		ready    bool
		from, to int64
		partial  bool
	}{{"missing-ready", true, 0, 0, true}, {"missing-not-ready", false, 0, 0, false}, {"old-boot", true, 1000, 3000, true}, {"current-boot", true, 4100, 5000, false}, {"lagging-coverage", true, 4100, 4500, true}} {
		t.Run(test.name, func(t *testing.T) {
			r := newMonRig(t, MonitoringConfig{})
			r.h.SetErrorJournal(ErrorJournalConfig{ReleaseNamespace: names.SystemNamespace})
			r.group("g", "ns", nil)
			for i, instance := range []string{"first", "second"} {
				ready := true
				start := int64(1000)
				if i == 1 {
					ready = test.ready
					start = 4000
				}
				p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: instance, Namespace: names.ProxyNamespace, Labels: map[string]string{"app": "laboratory-proxy-l7"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "l7", Image: "image"}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "l7", Ready: ready, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.UnixMilli(start))}}}}}}
				if _, err := r.h.k8s.CoreV1().Pods(names.ProxyNamespace).Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			create := func(instance string, from, to int64) {
				if _, err := r.cs.LaboratoryV1alpha1().LabTrafficReports("ns").Create(context.Background(), &labv1.LabTrafficReport{ObjectMeta: metav1.ObjectMeta{Name: "proxy-" + instance, Namespace: "ns"}, Spec: labv1.LabTrafficReportSpec{Kind: labv1.LabTrafficSurfaceProxy, Instance: instance}, Status: labv1.LabTrafficReportStatus{BootID: instance, CoveredFromMs: from, CoveredToMs: to}}, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			create("first", 1000, 5000)
			if test.from > 0 {
				create("second", test.from, test.to)
			}
			r.poll()
			got := r.h.monitor().state.update.Traffic[0]
			if got.Partial != test.partial {
				t.Fatalf("current replica completeness=%v want=%v", got.Partial, test.partial)
			}
		})
	}
}

func TestProxyCoverageWatchUsesChartNamespace(t *testing.T) {
	for _, test := range []struct {
		name, release, container string
		partial                  bool
	}{{"default-release", "laboratory-system", "l7", true}, {"custom-journal-release", "custom-system", "l7", true}, {"l7-disabled", "laboratory-system", "wg-demux", false}} {
		t.Run(test.name, func(t *testing.T) {
			r := newMonRig(t, MonitoringConfig{})
			r.h.SetErrorJournal(ErrorJournalConfig{ReleaseNamespace: test.release})
			r.group("g", "ns", nil)
			// Literal chart namespace: this must not follow the error-journal namespace.
			p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unreported-replica", Namespace: "laboratory-proxy", Labels: map[string]string{"app": "laboratory-proxy-l7"}}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: test.container, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.UnixMilli(1000))}}}}}}
			if _, err := r.h.k8s.CoreV1().Pods("laboratory-proxy").Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := r.cs.LaboratoryV1alpha1().LabTrafficReports("ns").Create(context.Background(), &labv1.LabTrafficReport{ObjectMeta: metav1.ObjectMeta{Name: "proxy-healthy", Namespace: "ns"}, Spec: labv1.LabTrafficReportSpec{Kind: labv1.LabTrafficSurfaceProxy, Instance: "healthy"}, Status: labv1.LabTrafficReportStatus{CoveredFromMs: 1000, CoveredToMs: 5000}}, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			r.poll()
			got := r.h.monitor().state.update.Traffic[0]
			if got.Partial != test.partial {
				t.Fatalf("chart namespace watcher incomplete=%v want=%v", got.Partial, test.partial)
			}
		})
	}
}
