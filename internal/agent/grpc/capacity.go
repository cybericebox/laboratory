package grpc

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/grouppods"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/nodecap"
	"github.com/cybericebox/laboratory/internal/tenant"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
)

// capacityTTL is how long a tenant's capacity is reused: it costs a list of the tenant's
// pods and of their metrics, and Monitoring asks for it with every message.
const capacityTTL = 5 * time.Second

type capacityEntry struct {
	at  time.Time
	cap *protobuf.CapacityResponse
}

type capacityCache struct {
	mu sync.Mutex
	m  map[string]capacityEntry
}

// SetGroupOverhead sets what the VPN and gateway pods of one LabGroup request together: the
// service overhead of every group, which a reservation must add to its labs.
func (h *Handler) SetGroupOverhead(o grouppods.Overhead) { h.groupOverhead = o }

// SetLabScheduling tells the agent which nodes lab pods run on, so a percentage quota
// resolves against the same allocatable as the operator's scheduler sees.
func (h *Handler) SetLabScheduling(selector map[string]string, tolerations []corev1.Toleration) {
	h.labSelector, h.labTolerations = selector, tolerations
}

// SetNodeReserve sets the platform reserve the scheduler keeps free on the lab nodes, so the largest device the agent
// reports is the room the scheduler would give a lab.
func (h *Handler) SetNodeReserve(r nodecap.Reserve) { h.nodeReserve = r }

// GetCapacity is the caller's tenant view: its quota, what its pods reserve and use, what is
// free. It never reveals cluster-wide numbers.
func (h *Handler) GetCapacity(ctx context.Context, _ *protobuf.Empty) (*protobuf.CapacityResponse, error) {
	return h.tenantCapacity(ctx)
}

func (h *Handler) tenantCapacity(ctx context.Context) (*protobuf.CapacityResponse, error) {
	name := tenantOf(ctx)
	h.capCache.mu.Lock()
	if e, ok := h.capCache.m[name]; ok && time.Since(e.at) < capacityTTL {
		h.capCache.mu.Unlock()
		return e.cap, nil
	}
	h.capCache.mu.Unlock()
	ten, err := h.tenantObject(ctx, name)
	if err != nil {
		return nil, err
	}
	load, err := h.tenantLoad(ctx, name)
	if err != nil {
		return nil, err
	}
	limits, err := h.tenantQuota(ctx, ten)
	if err != nil {
		return nil, err
	}
	resp := capacityOf(name, limits, load, h.groupOverhead)
	if v := h.room(ctx); v.ok {
		resp.HasMaxDevice, resp.MaxDeviceCpuMillicores, resp.MaxDeviceMemoryBytes = true, v.largest.CPU, v.largest.Memory
	}
	h.capCache.mu.Lock()
	if h.capCache.m == nil {
		h.capCache.m = map[string]capacityEntry{}
	}
	h.capCache.m[name] = capacityEntry{at: time.Now(), cap: resp}
	h.capCache.mu.Unlock()
	return resp, nil
}

// tenantQuota is the capacity the tenant is told about: its quota (nil = the tenant has no object: the platform's policy,
// no limit) resolved against the allocatable of the nodes lab pods run on, limited by the real room and net of the hidden
// packing reserve (see reported).
func (h *Handler) tenantQuota(ctx context.Context, ten *laboratoryv1alpha1.Tenant) (tenant.Limits, error) {
	var alloc tenant.Totals
	var quota *laboratoryv1alpha1.TenantQuota
	if ten != nil {
		quota = ten.Spec.Quota
	}
	if ten != nil && tenant.NeedsAllocatable(quota) {
		var err error
		if alloc, err = h.allocatable(ctx); err != nil {
			return tenant.Limits{}, err
		}
	}
	return h.reported(ctx, tenant.ResolveQuota(quota, alloc)), nil
}

// load is what a tenant's pods reserve and use.
type load struct {
	reserved       tenant.Totals
	used           tenant.Totals
	usageAvailable bool
}

// capacityOf assembles the answer; free is quota - reserved, never negative.
func capacityOf(name string, l tenant.Limits, ld load, overhead grouppods.Overhead) *protobuf.CapacityResponse {
	r := &protobuf.CapacityResponse{
		Tenant:                name,
		CpuReservedMillicores: ld.reserved.CPU, MemoryReservedBytes: ld.reserved.Memory,
		UsageAvailable: ld.usageAvailable, CpuUsedMillicores: ld.used.CPU, MemoryUsedBytes: ld.used.Memory,
		GroupOverheadCpuMillicores: overhead.CPU, GroupOverheadMemoryBytes: overhead.Memory,
	}
	if l.HasCPU {
		r.HasCpuQuota, r.CpuQuotaMillicores = true, l.CPU
		r.CpuFreeMillicores = max(l.CPU-ld.reserved.CPU, 0)
	}
	if l.HasMemory {
		r.HasMemoryQuota, r.MemoryQuotaBytes = true, l.Memory
		r.MemoryFreeBytes = max(l.Memory-ld.reserved.Memory, 0)
	}
	return r
}

// tenantObject reads the Tenant named like the caller's tenant. The default tenant has no
// object in a bare cluster (the chart creates it): then nil, and the platform's policy applies.
func (h *Handler) tenantObject(ctx context.Context, name string) (*laboratoryv1alpha1.Tenant, error) {
	t, err := h.cs.LaboratoryV1alpha1().Tenants().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return t, nil
}

// allocatable sums the allocatable CPU and memory of the nodes lab pods can run on.
func (h *Handler) allocatable(ctx context.Context) (tenant.Totals, error) {
	nodes, err := h.k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return tenant.Totals{}, err
	}
	var t tenant.Totals
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if !nodecap.Schedulable(n, h.labSelector, h.labTolerations) {
			continue
		}
		t.CPU += n.Status.Allocatable.Cpu().MilliValue()
		t.Memory += n.Status.Allocatable.Memory().Value()
	}
	return t, nil
}

// tenantPods lists the lab pods of a tenant: the ones carrying its tenant label, and for the
// default tenant also the lab pods of before tenancy (no label).
func (h *Handler) tenantPods(ctx context.Context, name string) ([]corev1.Pod, error) {
	pods := h.k8s.CoreV1().Pods(metav1.NamespaceAll)
	list, err := pods.List(ctx, metav1.ListOptions{LabelSelector: names.LabelTenant + "=" + name})
	if err != nil {
		return nil, err
	}
	out := list.Items
	if name == names.DefaultTenant {
		legacy, err := pods.List(ctx, metav1.ListOptions{LabelSelector: "!" + names.LabelTenant + "," + names.LabelLab})
		if err != nil {
			return nil, err
		}
		out = append(out, legacy.Items...)
	}
	return out, nil
}

// tenantLoad sums the container requests of the tenant's unfinished pods and, when
// metrics-server is there, their live usage.
func (h *Handler) tenantLoad(ctx context.Context, name string) (load, error) {
	var ld load
	pods, err := h.tenantPods(ctx, name)
	if err != nil {
		return ld, err
	}
	live := map[string]bool{}
	for i := range pods {
		p := &pods[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		live[p.Namespace+"/"+p.Name] = true
		for c := range p.Spec.Containers {
			req := p.Spec.Containers[c].Resources.Requests
			ld.reserved.CPU += req.Cpu().MilliValue()
			ld.reserved.Memory += req.Memory().Value()
		}
	}
	if h.metrics == nil {
		return ld, nil
	}
	metrics, err := h.metrics.MetricsV1beta1().PodMetricses(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: names.LabelTenant + "=" + name})
	if err != nil {
		return ld, nil // metrics are optional
	}
	ld.usageAvailable = true
	for i := range metrics.Items {
		pm := &metrics.Items[i]
		if !live[pm.Namespace+"/"+pm.Name] {
			continue
		}
		for c := range pm.Containers {
			u := pm.Containers[c].Usage
			ld.used.CPU += u.Cpu().MilliValue()
			ld.used.Memory += u.Memory().Value()
		}
	}
	return ld, nil
}

// RunTenantStatus keeps the status of every Tenant (reserved, used) fresh until ctx ends.
func (h *Handler) RunTenantStatus(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		h.refreshTenantStatus(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (h *Handler) refreshTenantStatus(ctx context.Context) {
	tenants, err := h.cs.LaboratoryV1alpha1().Tenants().List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	for i := range tenants.Items {
		t := &tenants.Items[i]
		ld, err := h.tenantLoad(ctx, t.Name)
		if err != nil {
			continue
		}
		now := metav1.Now()
		t.Status.Reserved = ld.reserved.Usage()
		t.Status.Used = laboratoryv1alpha1.TenantUsage{}
		if ld.usageAvailable {
			t.Status.Used = ld.used.Usage()
		}
		t.Status.ObservedAt = &now
		_, _ = h.cs.LaboratoryV1alpha1().Tenants().UpdateStatus(ctx, t, metav1.UpdateOptions{})
	}
}
