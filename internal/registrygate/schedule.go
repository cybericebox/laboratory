package registrygate

import (
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/cybericebox/laboratory/internal/names"
)

// The schedule is what the gate knows about the cluster, kept by watches (Pods, Devices, ImagePulls, Tenants, Nodes) and never read per
// request from the API server: a decision of the gate is a lookup in memory. The sources (internal/cmds/registrygated, kubesource.go)
// feed it; the tests feed it directly.

// PodInfo is what the gate needs of a Pod.
type PodInfo struct {
	Namespace string
	Name      string
	UID       string
	Node      string
	Labels    map[string]string
	// Images are the references of every container and init container.
	Images []string
	// Phase is the pod phase ("Running", "Succeeded", "Failed", ...); Terminating is a set deletionTimestamp.
	Phase       string
	Terminating bool
}

// Ended is true for a pod that is terminating or has finished: the state in which a device pod may push its exit snapshot.
func (p PodInfo) Ended() bool { return p.Terminating || p.Phase == "Succeeded" || p.Phase == "Failed" }

// DeviceInfo is what the gate needs of a Device.
type DeviceInfo struct {
	Namespace string
	Lab       string
	Name      string
	// Node is the node the device runs on (Device.status.nodeName).
	Node string
	// StateEnabled is spec.state.enabled: only such a device has a snapshot repository.
	StateEnabled bool
	Tenant       string
	// TenantQuotaBytes is spec.state.tenantQuotaBytes (0: the tenant's or the platform's value applies).
	TenantQuotaBytes int64
	// WriteQuotaBytes is spec.state.writeQuotaBytes (0: none): the default ceiling of one exit push.
	WriteQuotaBytes int64
}

// ImagePullInfo is what the gate needs of an ImagePull request.
type ImagePullInfo struct {
	Name   string
	Nodes  []string
	Images []string
	Tenant string
}

// TenantInfo is what the gate needs of a Tenant.
type TenantInfo struct {
	Name string
	// RegistryQuotaBytes is spec.persistence.registryQuota in bytes (0: the platform's value).
	RegistryQuotaBytes int64
}

// NodeInfo is a node with its addresses.
type NodeInfo struct {
	Name  string
	Addrs []netip.Addr
}

type podKey struct{ ns, name string }
type devKey struct{ ns, lab, dev string }

// Schedule is the in-memory view. Every method is safe for concurrent use.
type Schedule struct {
	// Now is the clock (tests set it).
	Now func() time.Time

	mu       sync.RWMutex
	pods     map[podKey]PodInfo
	byNode   map[string]map[podKey]struct{}
	devices  map[devKey]DeviceInfo
	pulls    map[string]ImagePullInfo
	tenants  map[string]TenantInfo
	nodes    map[string]NodeInfo
	groups   map[string]string // group namespace -> tenant
	segTen   map[string]string // tenant segment -> tenant; "" for a segment two tenants share
	lastOK   time.Time         // the last time the sources confirmed they are connected to the API server
	everSync bool
}

// NewSchedule is an empty view.
func NewSchedule() *Schedule {
	return &Schedule{
		Now:  time.Now,
		pods: map[podKey]PodInfo{}, byNode: map[string]map[podKey]struct{}{}, devices: map[devKey]DeviceInfo{},
		pulls: map[string]ImagePullInfo{}, tenants: map[string]TenantInfo{}, nodes: map[string]NodeInfo{},
		groups: map[string]string{}, segTen: map[string]string{},
	}
}

func (s *Schedule) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Confirm records that the sources are in contact with the API server now: a list or a watch event succeeded, or the heartbeat did.
func (s *Schedule) Confirm() {
	s.mu.Lock()
	s.lastOK, s.everSync = s.now(), true
	s.mu.Unlock()
}

// Fresh says whether the view may be believed: the sources were in contact with the API server within maxStale. A view that never
// synced is not fresh. This is the outage rule of the gate (2.4.7): a decision that needs the view is answered 503 when it is not.
func (s *Schedule) Fresh(maxStale time.Duration) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.everSync && s.now().Sub(s.lastOK) <= maxStale
}

// LastContact is the time of the last confirmation (zero: never).
func (s *Schedule) LastContact() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastOK
}

// SetPod adds or replaces a pod; DeletePod removes it.
func (s *Schedule) SetPod(p PodInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := podKey{p.Namespace, p.Name}
	if old, ok := s.pods[k]; ok && old.Node != p.Node {
		s.dropFromNode(old.Node, k)
	}
	s.pods[k] = p
	if p.Node != "" {
		if s.byNode[p.Node] == nil {
			s.byNode[p.Node] = map[podKey]struct{}{}
		}
		s.byNode[p.Node][k] = struct{}{}
	}
}

func (s *Schedule) dropFromNode(node string, k podKey) {
	if m := s.byNode[node]; m != nil {
		delete(m, k)
		if len(m) == 0 {
			delete(s.byNode, node)
		}
	}
}

// DeletePod removes a pod from the view.
func (s *Schedule) DeletePod(ns, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := podKey{ns, name}
	if old, ok := s.pods[k]; ok {
		s.dropFromNode(old.Node, k)
		delete(s.pods, k)
	}
}

// Pod returns a pod by name.
func (s *Schedule) Pod(ns, name string) (PodInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.pods[podKey{ns, name}]
	return p, ok
}

// PodsOnNode returns the pods bound to a node, in a stable order.
func (s *Schedule) PodsOnNode(node string) []PodInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]PodInfo, 0, len(s.byNode[node]))
	for k := range s.byNode[node] {
		out = append(out, s.pods[k])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// SetDevice / DeleteDevice / Device keep the devices.
func (s *Schedule) SetDevice(d DeviceInfo) {
	s.mu.Lock()
	s.devices[devKey{d.Namespace, d.Lab, d.Name}] = d
	s.mu.Unlock()
}

// DeleteDevice removes a device.
func (s *Schedule) DeleteDevice(ns, lab, dev string) {
	s.mu.Lock()
	delete(s.devices, devKey{ns, lab, dev})
	s.mu.Unlock()
}

// Device returns a device by namespace, lab and name.
func (s *Schedule) Device(ns, lab, dev string) (DeviceInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.devices[devKey{ns, lab, dev}]
	return d, ok
}

// DevicesOfTenant returns the devices of a tenant that are snapshot-backed.
func (s *Schedule) DevicesOfTenant(tenant string) []DeviceInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []DeviceInfo
	for _, d := range s.devices {
		if d.Tenant == tenant && d.StateEnabled {
			out = append(out, d)
		}
	}
	return out
}

// SetImagePull / DeleteImagePull / ImagePull keep the ImagePulls.
func (s *Schedule) SetImagePull(p ImagePullInfo) {
	s.mu.Lock()
	s.pulls[p.Name] = p
	s.mu.Unlock()
}

// DeleteImagePull removes an ImagePull.
func (s *Schedule) DeleteImagePull(name string) {
	s.mu.Lock()
	delete(s.pulls, name)
	s.mu.Unlock()
}

// ImagePull returns an ImagePull by name.
func (s *Schedule) ImagePull(name string) (ImagePullInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.pulls[name]
	return p, ok
}

// ImagePullsForNode returns the ImagePulls that list the node.
func (s *Schedule) ImagePullsForNode(node string) []ImagePullInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []ImagePullInfo
	for _, p := range s.pulls {
		for _, n := range p.Nodes {
			if n == node {
				out = append(out, p)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SetTenant / DeleteTenant / Tenant keep the tenants. The map from a tenant segment (names.TenantSegment) back to the tenant is rebuilt
// with every change; a segment two tenants share maps to nobody.
func (s *Schedule) SetTenant(t TenantInfo) {
	s.mu.Lock()
	s.tenants[t.Name] = t
	s.rebuildSegmentsLocked()
	s.mu.Unlock()
}

// DeleteTenant removes a tenant.
func (s *Schedule) DeleteTenant(name string) {
	s.mu.Lock()
	delete(s.tenants, name)
	s.rebuildSegmentsLocked()
	s.mu.Unlock()
}

func (s *Schedule) rebuildSegmentsLocked() {
	seg := map[string]string{}
	add := func(name string) {
		k := names.TenantSegment(name)
		if prev, ok := seg[k]; ok && prev != name {
			seg[k] = "" // a collision: both are refused
			return
		}
		seg[k] = name
	}
	add(names.DefaultTenant) // an object without a tenant label is the default tenant, which need not have a Tenant object
	for n := range s.tenants {
		add(n)
	}
	s.segTen = seg
}

// TenantOfSegment maps a tenant segment of an image reference to its tenant. ok is false for an unknown segment and for one that two
// tenants share. The segment "platform" is not a tenant: callers handle it before.
func (s *Schedule) TenantOfSegment(seg string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.segTen[seg]
	return t, ok && t != ""
}

// SetGroup / DeleteGroup / TenantOfNamespace keep which tenant a group namespace belongs to (the tenant label of its LabGroup).
func (s *Schedule) SetGroup(namespace, tenant string) {
	s.mu.Lock()
	s.groups[namespace] = tenant
	s.mu.Unlock()
}

// DeleteGroup forgets a group namespace.
func (s *Schedule) DeleteGroup(namespace string) {
	s.mu.Lock()
	delete(s.groups, namespace)
	s.mu.Unlock()
}

// TenantOfNamespace is the tenant of a group namespace; ok is false for a namespace that is no group's.
func (s *Schedule) TenantOfNamespace(namespace string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.groups[namespace]
	return t, ok
}

// Tenant returns a tenant by name.
func (s *Schedule) Tenant(name string) (TenantInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tenants[name]
	return t, ok
}

// SetNode / DeleteNode / Node keep the nodes.
func (s *Schedule) SetNode(n NodeInfo) {
	s.mu.Lock()
	s.nodes[n.Name] = n
	s.mu.Unlock()
}

// DeleteNode removes a node.
func (s *Schedule) DeleteNode(name string) {
	s.mu.Lock()
	delete(s.nodes, name)
	s.mu.Unlock()
}

// Node returns a node by name.
func (s *Schedule) Node(name string) (NodeInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.nodes[name]
	return n, ok
}

// knownRepo says whether a snapshot repository belongs to a device the view still has (any device, snapshot-backed or not).
func (s *Schedule) knownRepo(repo string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for k := range s.devices {
		if "lab/"+k.ns+"/"+k.lab+"/"+k.dev == repo {
			return true
		}
	}
	return false
}
