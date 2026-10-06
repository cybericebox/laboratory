package grpc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	versioned "github.com/cybericebox/laboratory/clientset/client/versioned"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// keyed returns the objects of a list as name -> object, for a comparison that ignores order.
func settled[T interface {
	GetName() string
	GetNamespace() string
}](cache []T, live []T) bool {
	if len(cache) != len(live) {
		return false
	}
	key := func(o T) string { return o.GetNamespace() + "/" + o.GetName() }
	sort.Slice(cache, func(i, j int) bool { return key(cache[i]) < key(cache[j]) })
	sort.Slice(live, func(i, j int) bool { return key(live[i]) < key(live[j]) })
	for i := range cache {
		if !apiequality.Semantic.DeepEqual(cache[i], live[i]) {
			return false
		}
	}
	return true
}

// settle waits until the informers show what the (fake) API server holds: the informers are asynchronous, a test that changed an object
// and polls by hand must not race them.
func (c *monCache) settle(t *testing.T, cs versioned.Interface) {
	t.Helper()
	ctx := context.Background()
	laboratory := cs.LaboratoryV1alpha1()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ok := true
		if l, err := laboratory.LabGroups().List(ctx, metav1.ListOptions{}); err == nil {
			cached, _ := c.groups.List(labels.Everything())
			live := make([]*laboratoryv1alpha1.LabGroup, 0)
			for i := range l.Items {
				live = append(live, &l.Items[i])
			}
			ok = ok && settled(cached, live)
		}
		if l, err := laboratory.Labs("").List(ctx, metav1.ListOptions{}); err == nil {
			cached, _ := c.labs.List(labels.Everything())
			live := make([]*laboratoryv1alpha1.Lab, 0)
			for i := range l.Items {
				live = append(live, &l.Items[i])
			}
			ok = ok && settled(cached, live)
		}
		if l, err := laboratory.LabGroupClients("").List(ctx, metav1.ListOptions{}); err == nil {
			cached, _ := c.clients.List(labels.Everything())
			live := make([]*laboratoryv1alpha1.LabGroupClient, 0)
			for i := range l.Items {
				live = append(live, &l.Items[i])
			}
			ok = ok && settled(cached, live)
		}
		if l, err := laboratory.LabGroupAccessPolicies("").List(ctx, metav1.ListOptions{}); err == nil {
			cached, _ := c.policies.List(labels.Everything())
			live := make([]*laboratoryv1alpha1.LabGroupAccessPolicy, 0)
			for i := range l.Items {
				live = append(live, &l.Items[i])
			}
			ok = ok && settled(cached, live)
		}
		if l, err := laboratory.LabTrafficReports("").List(ctx, metav1.ListOptions{}); err == nil {
			cached, _ := c.reports.List(labels.Everything())
			live := make([]*laboratoryv1alpha1.LabTrafficReport, 0)
			for i := range l.Items {
				live = append(live, &l.Items[i])
			}
			ok = ok && settled(cached, live)
		}
		if l, err := laboratory.Devices("").List(ctx, metav1.ListOptions{}); err == nil {
			cached, _ := c.devices.List(labels.Everything())
			live := make([]*laboratoryv1alpha1.Device, 0)
			for i := range l.Items {
				live = append(live, &l.Items[i])
			}
			ok = ok && settled(cached, live)
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the monitoring caches did not catch up with the API server")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A-3: after the caches are up, a poll makes no call to the API server, however many groups there are.
func TestMonitoringPollsReadOnlyTheCaches(t *testing.T) {
	r := newMonRig(t, MonitoringConfig{})
	for i := 0; i < 40; i++ {
		ns := fmt.Sprintf("ns-%d", i)
		r.group(fmt.Sprintf("g%d", i), ns, nil)
		r.lab(ns, "l1", nil)
		r.lab(ns, "l2", nil)
	}
	r.poll() // starts the informers: their lists and watches are the only calls
	r.cs.ClearActions()
	for i := 0; i < 10; i++ {
		if err := r.h.monitor().poll(context.Background()); err != nil { // not r.poll: its settle lists the fake API server itself
			t.Fatal(err)
		}
	}
	for _, a := range r.cs.Actions() {
		t.Errorf("a poll called the API server: %s %s", a.GetVerb(), a.GetResource().Resource)
	}
	if got := len(r.h.monitor().state.update.Labs); got != 80 {
		t.Fatalf("the observation holds %d labs, want 80", got)
	}
}

// A-3: the informers run only while somebody is subscribed.
func TestMonitoringCacheStopsWithTheLastSubscriber(t *testing.T) {
	r := newMonRig(t, MonitoringConfig{PollInterval: 20 * time.Millisecond})
	r.group("g1", "ns", nil)
	c := r.subscribe(&protobuf.MonitoringRequest{})
	c.next(t)
	m := r.h.monitor()
	m.pollMu.Lock()
	running := m.cache != nil
	m.pollMu.Unlock()
	if !running {
		t.Fatal("a subscriber needs the caches")
	}
	c.cancel()
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.pollMu.Lock()
		stopped := m.cache == nil
		m.pollMu.Unlock()
		if stopped {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the caches keep running with nobody subscribed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A-2: a poll that cannot observe (the caches cannot list) changes nothing: no update and, above all, no record is deleted.
func TestMonitoringListFailureIsNotADeletion(t *testing.T) {
	r := newMonRig(t, MonitoringConfig{})
	r.group("g", "ns", nil)
	r.lab("ns", "l", nil)
	r.poll() // baseline
	var failing atomic.Bool
	r.cs.PrependReactor("list", "labs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return failing.Load(), nil, errors.New("the API server is busy")
	})
	m := r.h.monitor()
	m.pollMu.Lock()
	m.cache.stop() // the agent restarted its caches (the last subscriber left and a new one came) while the API is failing
	m.cache = nil
	m.pollMu.Unlock()

	failing.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := m.poll(ctx); err == nil {
		t.Fatal("a poll that cannot list must fail")
	}
	if got := m.journalLen(); got != 0 {
		t.Fatalf("a failed poll produced %d journal entries", got)
	}
	if len(m.state.update.Labs) != 1 {
		t.Fatalf("the state lost its lab: %+v", m.state.update.Labs)
	}

	failing.Store(false)
	r.poll()
	if got := m.journalLen(); got != 0 {
		t.Fatalf("recovery produced %d journal entries: the lab was deleted and recreated", got)
	}
}

// A-2: when the metrics API fails, the last usage stays: devices do not flip to "usage unavailable" and back.
func TestMonitoringKeepsLastUsageWhenMetricsFail(t *testing.T) {
	r := newMonRig(t, MonitoringConfig{})
	var failing atomic.Bool
	items := []metricsv1beta1.PodMetrics{*podMetrics("ns", "web-pod", "l", "web", rl("150m", "64Mi"))}
	mc := metricsfake.NewSimpleClientset()
	mc.PrependReactor("list", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failing.Load() {
			return true, nil, errors.New("metrics-server is down")
		}
		return true, &metricsv1beta1.PodMetricsList{Items: items}, nil
	})
	r.h.metrics = mc
	r.group("g", "ns", nil)
	if _, err := r.cs.LaboratoryV1alpha1().Labs("ns").Create(context.Background(), &laboratoryv1alpha1.Lab{
		ObjectMeta: metav1.ObjectMeta{Name: "l", Namespace: "ns"},
		Status:     laboratoryv1alpha1.LabStatus{Devices: []laboratoryv1alpha1.DeviceRef{{Name: "web"}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	usage := func() *protobuf.LabDeviceStatus {
		return r.h.monitor().state.update.Labs[0].Status.Devices[0]
	}
	r.poll()
	if d := usage(); !d.UsageAvailable || d.CpuMillicores != 150 {
		t.Fatalf("baseline usage: %+v", d)
	}

	failing.Store(true)
	m := r.h.monitor()
	m.cache.usageMu.Lock()
	m.cache.usageAt = time.Time{} // due for a refresh
	m.cache.usageMu.Unlock()
	r.poll()
	if d := usage(); !d.UsageAvailable || d.CpuMillicores != 150 {
		t.Fatalf("usage after a failed read: %+v", d)
	}
	if got := m.journalLen(); got != 0 {
		t.Fatalf("a failed metrics read produced %d journal entries", got)
	}
}
