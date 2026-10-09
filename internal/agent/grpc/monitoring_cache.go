package grpc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	kubeinformers "k8s.io/client-go/informers"
	corelisters "k8s.io/client-go/listers/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	informers "github.com/cybericebox/laboratory/clientset/informers/externalversions"
	listers "github.com/cybericebox/laboratory/clientset/listers/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// usageRefresh is how often the pod metrics are read: metrics-server itself only scrapes about every 15 s.
const usageRefresh = 15 * time.Second

// monCache serves the Monitoring poll from informer caches instead of listing every object of every group from the API
// server on each poll. Its informers run only while somebody is subscribed (the monitor creates it with the first subscriber
// and stops it with the last), so an agent nobody watches holds nothing. A list or watch error never changes what the
// caches hold: the poll keeps seeing the last known objects, never a deletion.
type monCache struct {
	h      *Handler
	cancel context.CancelFunc

	groups    listers.LabGroupLister
	labs      listers.LabLister
	clients   listers.LabGroupClientLister
	policies  listers.LabGroupAccessPolicyLister
	reports   listers.LabTrafficReportLister
	devices   listers.DeviceLister
	pods      corelisters.PodLister // nil without a core clientset
	proxyPods corelisters.PodLister // current L7 replica lifecycle, fixed proxy namespace

	usageMu sync.Mutex
	usage   map[string]map[usageKey]deviceUsage // namespace -> usage; nil until the first successful read
	usageAt time.Time
}

// newMonCache starts the informers and waits (bounded by ctx) until they have listed once.
func newMonCache(ctx context.Context, h *Handler) (*monCache, error) {
	run, cancel := context.WithCancel(context.Background())
	f := informers.NewSharedInformerFactory(h.cs, 0)
	lab := f.Laboratory().V1alpha1()
	c := &monCache{
		h: h, cancel: cancel,
		groups: lab.LabGroups().Lister(), labs: lab.Labs().Lister(), clients: lab.LabGroupClients().Lister(),
		policies: lab.LabGroupAccessPolicies().Lister(), reports: lab.LabTrafficReports().Lister(), devices: lab.Devices().Lister(),
	}
	informersToSync := []func() bool{
		lab.LabGroups().Informer().HasSynced, lab.Labs().Informer().HasSynced, lab.LabGroupClients().Informer().HasSynced,
		lab.LabGroupAccessPolicies().Informer().HasSynced, lab.LabTrafficReports().Informer().HasSynced, lab.Devices().Informer().HasSynced,
	}
	var kf kubeinformers.SharedInformerFactory
	var proxyFactory kubeinformers.SharedInformerFactory
	if h.k8s != nil {
		// Only the pods of devices (they carry the lab label) are of interest.
		kf = kubeinformers.NewSharedInformerFactoryWithOptions(h.k8s, 0, kubeinformers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.LabelSelector = names.LabelLab
		}))
		pods := kf.Core().V1().Pods()
		c.pods = pods.Lister()
		informersToSync = append(informersToSync, pods.Informer().HasSynced)
		proxyFactory = kubeinformers.NewSharedInformerFactoryWithOptions(h.k8s, 0, kubeinformers.WithNamespace(names.ProxyNamespace), kubeinformers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = "app=laboratory-proxy-l7" }))
		proxies := proxyFactory.Core().V1().Pods()
		c.proxyPods = proxies.Lister()
		informersToSync = append(informersToSync, proxies.Informer().HasSynced)
	}
	f.Start(run.Done())
	if kf != nil {
		kf.Start(run.Done())
	}
	if proxyFactory != nil {
		proxyFactory.Start(run.Done())
	}
	for {
		synced := true
		for _, has := range informersToSync {
			synced = synced && has()
		}
		if synced {
			return c, nil
		}
		select {
		case <-ctx.Done():
			cancel()
			return nil, fmt.Errorf("the monitoring caches did not sync: %w", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (c *monCache) stop() { c.cancel() }

// Each Ready L7 container can serve traffic. Its report must include this boot
// through the merged observation's end; an old boot or another healthy replica
// cannot prove absence of requests on this instance. Read cached Pods only.

type proxyCoverageView struct {
	ready       map[string]int64
	unavailable bool
}

func (c *monCache) proxyCoverage() proxyCoverageView {
	if c.proxyPods == nil {
		return proxyCoverageView{}
	} // legacy readers without a core clientset
	pods, err := c.proxyPods.List(labels.Everything())
	if err != nil {
		return proxyCoverageView{unavailable: true}
	}
	v := proxyCoverageView{ready: map[string]int64{}}
	for _, p := range pods {
		for _, s := range p.Status.ContainerStatuses {
			if s.Name != "l7" || !s.Ready {
				continue
			}
			if s.State.Running == nil || s.State.Running.StartedAt.IsZero() || s.State.Running.StartedAt.UnixMilli() <= 0 {
				v.unavailable = true
				continue
			}
			v.ready[p.Name] = s.State.Running.StartedAt.UnixMilli()
		}
	}
	return v
}

func (v proxyCoverageView) incomplete(reports []*protobuf.TrafficReport, to int64) bool {
	if v.unavailable {
		return true
	}
	if len(v.ready) == 0 {
		return false
	}
	byInstance := make(map[string][]*protobuf.TrafficReport, len(reports))
	for _, r := range reports {
		byInstance[r.GetInstance()] = append(byInstance[r.GetInstance()], r)
	}
	for instance, started := range v.ready {
		covered := false
		for _, r := range byInstance[instance] {
			for _, span := range r.GetCoverageSpans() {
				if !span.GetPartial() && span.GetFromUnixMs() >= started && span.GetFromUnixMs() <= span.GetToUnixMs() && span.GetToUnixMs() >= to {
					covered = true
					break
				}
			}
			if covered {
				break
			}
		}
		if !covered {
			return true
		}
	}
	return false
}

// usageByNamespace returns the live usage of the device pods, by namespace and (lab, device); nil when the metrics are not
// available. The pod metrics are read at most every usageRefresh, in one call for the whole cluster; when a read fails the last
// good answer stays (and is logged), so a failing metrics API never flips every device to "usage unavailable".
func (c *monCache) usageByNamespace(ctx context.Context) map[string]map[usageKey]deviceUsage {
	c.usageMu.Lock()
	defer c.usageMu.Unlock()
	if c.h.metrics == nil || (!c.usageAt.IsZero() && time.Since(c.usageAt) < usageRefresh) {
		return c.usage
	}
	c.usageAt = time.Now()
	usage, err := c.h.fetchUsage(ctx)
	if err != nil {
		ctrl.Log.WithName("monitoring").Error(err, "read the pod metrics; keeping the last ones")
		return c.usage
	}
	c.usage = usage
	return c.usage
}

// fetchUsage reads the pod metrics of all device pods of the cluster in one call.
func (h *Handler) fetchUsage(ctx context.Context) (map[string]map[usageKey]deviceUsage, error) {
	if h.metrics == nil {
		return nil, errors.New("no metrics client")
	}
	list, err := h.metrics.MetricsV1beta1().PodMetricses(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: names.LabelLab + "," + names.LabelDevice})
	if err != nil {
		return nil, err
	}
	return foldUsage(list.Items), nil
}

// byNamespace groups objects by their namespace.
func byNamespace[T interface{ GetNamespace() string }](items []T) map[string][]T {
	out := map[string][]T{}
	for _, it := range items {
		out[it.GetNamespace()] = append(out[it.GetNamespace()], it)
	}
	return out
}

// podStatuses returns the device pods' status by namespace and (lab, device).
func (c *monCache) podStatuses() map[string]map[usageKey]devicePodStatus {
	if c.pods == nil {
		return nil
	}
	pods, err := c.pods.List(labels.Everything())
	if err != nil {
		return nil
	}
	out := map[string]map[usageKey]devicePodStatus{}
	for _, pod := range pods {
		key := usageKey{lab: pod.Labels[names.LabelLab], device: pod.Labels[names.LabelDevice]}
		if key.lab == "" || key.device == "" {
			continue
		}
		if out[pod.Namespace] == nil {
			out[pod.Namespace] = map[usageKey]devicePodStatus{}
		}
		out[pod.Namespace][key] = devicePodStatus{phase: string(pod.Status.Phase), reason: podReason(pod), restartCount: podRestartCount(pod)}
	}
	return out
}

// deviceSchedules returns the scheduler state of the Devices by namespace and (lab CR name, device name).
func (c *monCache) deviceSchedules() map[string]map[usageKey]*laboratoryv1alpha1.PodSchedule {
	devices, err := c.devices.List(labels.Everything())
	if err != nil {
		return nil
	}
	out := map[string]map[usageKey]*laboratoryv1alpha1.PodSchedule{}
	for _, d := range devices {
		if out[d.Namespace] == nil {
			out[d.Namespace] = map[usageKey]*laboratoryv1alpha1.PodSchedule{}
		}
		out[d.Namespace][usageKey{lab: d.Spec.LabRef, device: d.Spec.Name}] = d.Status.Scheduling
	}
	return out
}
