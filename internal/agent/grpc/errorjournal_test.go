package grpc

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/errorlog"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

func obsOf(component, instance, fp string, total int64, first time.Time) errorlog.Observation {
	return errorlog.Observation{Component: component, Instance: instance, Fingerprint: fp, Kind: "reconcile", Normalized: "boom <n>", Total: total,
		First: first, Last: first.Add(time.Second), Samples: []string{"boom 1"}}
}

func TestCollectorTurnsTotalsIntoDeltas(t *testing.T) {
	c := newErrorCollector()
	now := time.Now()
	// a group that is older than this agent: its total is only the baseline
	c.Observe(obsOf("operator", "p1", "aaaa", 100, now.Add(-time.Hour)))
	if c.Cursor() != 0 {
		t.Fatal("the history of a group older than the agent is not ours")
	}
	c.Observe(obsOf("operator", "p1", "aaaa", 103, now.Add(-time.Hour)))
	comps, cur := c.Since(0)
	if len(comps) != 1 || comps[0].Groups[0].Count != 3 || cur != 1 {
		t.Fatalf("%+v cursor %d", comps, cur)
	}
	// a new group (it began after the agent) counts in full
	c.Observe(obsOf("node-agent", "n1", "bbbb", 4, now))
	// the component restarted: the total went down and counts again from zero
	c.Observe(obsOf("operator", "p1", "aaaa", 2, now.Add(-time.Hour)))
	comps, _ = c.Since(0)
	got := map[string]int64{}
	for _, ce := range comps {
		for _, g := range ce.Groups {
			got[ce.Component+"/"+g.Fingerprint] = g.Count
		}
	}
	if got["operator/aaaa"] != 5 || got["node-agent/bbbb"] != 4 {
		t.Fatalf("%v", got)
	}
	// the same total again is no new error
	before := c.Cursor()
	c.Observe(obsOf("node-agent", "n1", "bbbb", 4, now))
	if c.Cursor() != before {
		t.Fatal("an unchanged total adds nothing")
	}
	// only what is after the cursor
	if comps, _ = c.Since(before); len(comps) != 0 {
		t.Fatalf("%+v", comps)
	}
}

func TestCollectorMergesByInstanceAndKeepsTheRingBounded(t *testing.T) {
	c := newErrorCollector()
	now := time.Now()
	c.Observe(obsOf("l7-proxy", "p-1", "cc", 1, now))
	c.Observe(obsOf("l7-proxy", "p-2", "cc", 2, now))
	c.Observe(obsOf("l7-proxy", "p-1", "cc", 3, now))
	comps, _ := c.Since(0)
	if len(comps) != 2 || comps[0].Instance != "p-1" || comps[0].Groups[0].Count != 3 || comps[1].Groups[0].Count != 2 {
		t.Fatalf("%+v", comps)
	}
	for i := 0; i < errorRing+100; i++ {
		c.Observe(obsOf("operator", "p", "dd", int64(i+1), now))
	}
	if len(c.ring) > errorRing {
		t.Fatalf("ring = %d", len(c.ring))
	}
}

func journalHandler(t *testing.T, tenant *laboratoryv1alpha1.Tenant) *Handler {
	t.Helper()
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{tenant})
	h.SetErrorJournal(ErrorJournalConfig{ReleaseNamespace: "laboratory-system"})
	return h
}

// Only the platform's tenant receives the laboratory's own errors; everyone gets the failed deploys of their own labs.
func TestOnlyTheFlaggedTenantReceivesComponentErrors(t *testing.T) {
	platform := newTenantTenant("platform", false, nil)
	platform.Spec.ReceivesLabErrors = true
	customer := newTenantTenant("customer", false, nil)
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{platform, customer})
	h.SetErrorJournal(ErrorJournalConfig{ReleaseNamespace: "laboratory-system"})
	h.errors.Observe(obsOf("operator", "p1", "aaaa", 1, time.Now()))

	for tenant, want := range map[string]bool{"platform": true, "customer": false} {
		u := &protobuf.MonitoringUpdate{}
		h.fillErrors(asClient(tenant), newErrorStream(), u)
		has := len(u.GetErrors().GetComponents()) > 0
		if has != want {
			t.Errorf("%s: components %v, want %v: %+v", tenant, has, want, u.GetErrors())
		}
	}
}

func TestErrorJournalCarriesTheCertificateExpiryAndRepeatsItHourly(t *testing.T) {
	h := journalHandler(t, newTenantTenant("platform", false, nil))
	st := newErrorStream()
	u := &protobuf.MonitoringUpdate{}
	h.fillErrors(asClient("platform"), st, u)
	if u.GetErrors().GetClientCertNotAfterUnix() == 0 {
		t.Fatal("the first message carries the expiry")
	}
	u = &protobuf.MonitoringUpdate{}
	h.fillErrors(asClient("platform"), st, u)
	if u.Errors != nil {
		t.Fatalf("nothing new, nothing sent: %+v", u.Errors)
	}
	st.certSent = time.Now().Add(-2 * time.Hour)
	u = &protobuf.MonitoringUpdate{}
	h.fillErrors(asClient("platform"), st, u)
	if u.GetErrors().GetClientCertNotAfterUnix() == 0 {
		t.Fatal("repeated about hourly")
	}
}

func failedLab(ns, name, device, reason, msg string) *protobuf.Lab {
	d := &protobuf.LabDeviceStatus{Name: device, Scheduling: &protobuf.PodScheduling{Failure: &protobuf.PodFailure{Reason: reason, Message: msg}}}
	return &protobuf.Lab{Namespace: ns, Name: name, LabGroupName: "grp-1", Status: &protobuf.LabStatus{Phase: "Failed", Devices: []*protobuf.LabDeviceStatus{d}}}
}

func TestDeployFailuresHaveReasonCodesAndAreReportedOnce(t *testing.T) {
	st := newErrorStream()
	now := time.Now()
	u := &protobuf.MonitoringUpdate{Labs: []*protobuf.Lab{failedLab("lg-x", "lab-1", "web", "ImagePull", `pull "ghcr.io/acme/secret:1" from 10.0.0.9: denied`)}}
	got := st.newFailures(u, now)
	if len(got) != 1 || got[0].ReasonCode != "ImagePull" || got[0].Lab != "lab-1" || got[0].Device != "web" || got[0].LabGroup != "grp-1" {
		t.Fatalf("%+v", got)
	}
	if strings.Contains(got[0].Message, "acme") || strings.Contains(got[0].Message, "10.0.0.9") {
		t.Fatalf("the message must be redacted: %q", got[0].Message)
	}
	if again := st.newFailures(u, now); len(again) != 0 {
		t.Fatalf("a failure is reported once: %+v", again)
	}
	// a different reason is a new report
	u2 := &protobuf.MonitoringUpdate{Labs: []*protobuf.Lab{failedLab("lg-x", "lab-1", "web", "StartupTimeout", "")}}
	if got = st.newFailures(u2, now); len(got) != 1 || got[0].ReasonCode != "StartupTimeout" {
		t.Fatalf("%+v", got)
	}
	// recovered, then failed again: reported again
	ok := &protobuf.MonitoringUpdate{Labs: []*protobuf.Lab{{Namespace: "lg-x", Name: "lab-1", Status: &protobuf.LabStatus{Phase: "Ready", Devices: []*protobuf.LabDeviceStatus{{Name: "web"}}}}}}
	st.newFailures(ok, now)
	if got = st.newFailures(u, now); len(got) != 1 {
		t.Fatalf("a failure after recovery is new: %+v", got)
	}
}

func TestLabInFailedOrErrorPhaseWithoutADeviceToBlame(t *testing.T) {
	st := newErrorStream()
	u := &protobuf.MonitoringUpdate{Labs: []*protobuf.Lab{
		{Namespace: "ns", Name: "a", LabGroupName: "g", Status: &protobuf.LabStatus{Phase: "Failed"}},
		{Namespace: "ns", Name: "b", LabGroupName: "g", Status: &protobuf.LabStatus{Phase: "Error"}},
		{Namespace: "ns", Name: "c", LabGroupName: "g", Status: &protobuf.LabStatus{Phase: "Ready"}},
	}}
	got := st.newFailures(u, time.Now())
	if len(got) != 2 || got[0].ReasonCode != "LabFailed" || got[1].ReasonCode != "LabError" {
		t.Fatalf("%+v", got)
	}
}

func TestGroupPodFailures(t *testing.T) {
	st := newErrorStream()
	g := &protobuf.LabGroup{Name: "grp-9", Status: &protobuf.LabGroupStatus{Namespace: "lg-9", Pods: []*protobuf.LabGroupPod{
		{Name: "vpn", Scheduling: &protobuf.PodScheduling{Failure: &protobuf.PodFailure{Reason: "DoesNotFit", Message: "needs 4 CPU"}}},
		{Name: "gateway"},
	}}}
	got := st.newFailures(&protobuf.MonitoringUpdate{Groups: []*protobuf.LabGroup{g}}, time.Now())
	if len(got) != 1 || got[0].LabGroup != "grp-9" || got[0].Lab != "" || got[0].Device != "vpn" || got[0].ReasonCode != "DoesNotFit" {
		t.Fatalf("%+v", got)
	}
}

// The agent reads the components' events from the release namespace and from every group namespace, and its own errors directly.
func TestAgentReadsTheEventsOfTheComponents(t *testing.T) {
	platform := newTenantTenant("platform", false, nil)
	platform.Spec.ReceivesLabErrors = true
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{platform})
	self := errorlog.New("agent", "agent-0")
	h.SetErrorJournal(ErrorJournalConfig{ReleaseNamespace: "laboratory-system", Self: self})
	h.errors.started = time.Now().Add(-time.Hour) // the agent has been up a while: new groups count in full
	ctx := context.Background()

	_, err := h.cs.LaboratoryV1alpha1().LabGroups().Create(ctx, &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: "g1"},
		Status: laboratoryv1alpha1.LabGroupStatus{Namespace: "lg-g1"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for ns, comp := range map[string]string{"laboratory-system": "operator", "lg-g1": "vpn"} {
		agg := errorlog.New(comp, comp+"-1")
		agg.Record("reconcile", "boom 7 for 10.1.1.1")
		(&errorlog.Publisher{Client: h.k8s, Namespace: ns, Agg: agg}).Flush(ctx)
	}
	// an unrelated event is ignored
	_, _ = h.k8s.CoreV1().Events("lg-g1").Create(ctx, &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "lg-g1"}, Reason: "Pulled"}, metav1.CreateOptions{})
	self.Record("grpc", "stream broke")

	h.pollErrors(ctx, true)
	u := &protobuf.MonitoringUpdate{}
	h.fillErrors(asClient("platform"), newErrorStream(), u)
	seen := map[string]int64{}
	for _, ce := range u.GetErrors().GetComponents() {
		for _, g := range ce.Groups {
			seen[ce.Component] += g.Count
			for _, s := range g.Samples {
				if strings.Contains(s, "10.1.1.1") {
					t.Errorf("address in %q", s)
				}
			}
		}
	}
	if seen["operator"] != 1 || seen["vpn"] != 1 || seen["agent"] != 1 || len(seen) != 3 {
		t.Fatalf("%v", seen)
	}
	// a second poll with nothing new adds nothing
	h.pollErrors(ctx, true)
	u = &protobuf.MonitoringUpdate{}
	st := newErrorStream()
	st.started, st.cursor = true, h.errors.Cursor()
	h.fillErrors(asClient("platform"), st, u)
	if len(u.GetErrors().GetComponents()) != 0 {
		t.Fatalf("%+v", u.Errors)
	}
}

// End to end on the stream: the first message carries the journal (the components for the flagged tenant, the certificate expiry),
// and a component error that comes later arrives as a delta.
func TestMonitoringStreamCarriesTheErrorJournal(t *testing.T) {
	platform := newTenantTenant("platform", false, nil)
	platform.Spec.ReceivesLabErrors = true
	h := tenantHandler(t, []*laboratoryv1alpha1.Tenant{platform})
	h.SetMonitoringConfig(MonitoringConfig{PollInterval: 50 * time.Millisecond})
	h.SetErrorJournal(ErrorJournalConfig{ReleaseNamespace: "laboratory-system"})
	h.errors.started = time.Now().Add(-time.Hour)
	ctx, cancel := context.WithCancel(asClient("platform"))
	defer cancel()
	s := &fakeMonStream{ctx: ctx, sent: make(chan *protobuf.MonitoringUpdate, 8)}
	done := make(chan error, 1)
	go func() { done <- h.Monitoring(&protobuf.MonitoringRequest{MinIntervalMs: 1}, s) }()

	first := receiveMonitoringUpdate(t, s.sent)
	if first.GetErrors().GetClientCertNotAfterUnix() == 0 {
		t.Fatalf("the first message carries the certificate expiry: %+v", first.GetErrors())
	}
	h.errors.Observe(obsOf("operator", "p1", "aaaa", 2, time.Now()))
	deadline := time.After(5 * time.Second)
	for {
		select {
		case u := <-s.sent:
			if cs := u.GetErrors().GetComponents(); len(cs) == 1 && cs[0].Component == "operator" && cs[0].Groups[0].Count == 2 {
				cancel()
				<-done
				return
			}
		case <-deadline:
			t.Fatal("the operator error did not arrive on the stream")
		}
	}
}
