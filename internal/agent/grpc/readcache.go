package grpc

import (
	"context"
	"log"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	informers "github.com/cybericebox/laboratory/clientset/informers/externalversions"
	listers "github.com/cybericebox/laboratory/clientset/listers/laboratory/v1alpha1"
)

// readCacheSyncTimeout bounds the first sync of the read cache; past it every read goes to the API server, as before.
const readCacheSyncTimeout = 30 * time.Second

// readCache serves the read-only calls of the agent (resolving a group, listing groups and labs) from informers, instead of
// one GET or LIST against the API server per backend call. The informers start with the first read and live as long as the
// agent. A cache may lag the API server by milliseconds, so it never decides a negative or an empty answer: a miss, an empty
// list and any error are read again from the API server. Writes and their guards (createLab, the lifecycle, admissions) keep
// reading the API server directly.
type readCache struct {
	enabled      bool
	once         sync.Once
	groups       listers.LabGroupLister
	labs         listers.LabLister
	devices      listers.DeviceLister
	reservations listers.ReservationLister
	ok           bool
}

// EnableReadCache turns the read cache on (the production wiring; a bare Handler reads the API server directly).
func (h *Handler) EnableReadCache() { h.reads.enabled = true }

// cached returns the listers once they have synced, nil when the cache is off or did not sync.
func (h *Handler) cached() *readCache {
	rc := &h.reads
	if !rc.enabled {
		return nil
	}
	rc.once.Do(func() {
		f := informers.NewSharedInformerFactory(h.cs, 0)
		lab := f.Laboratory().V1alpha1()
		rc.groups, rc.labs = lab.LabGroups().Lister(), lab.Labs().Lister()
		rc.devices, rc.reservations = lab.Devices().Lister(), lab.Reservations().Lister()
		syncs := []func() bool{lab.LabGroups().Informer().HasSynced, lab.Labs().Informer().HasSynced, lab.Devices().Informer().HasSynced, lab.Reservations().Informer().HasSynced}
		f.Start(context.Background().Done())
		deadline := time.Now().Add(readCacheSyncTimeout)
		for time.Now().Before(deadline) {
			all := true
			for _, s := range syncs {
				all = all && s()
			}
			if all {
				rc.ok = true
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		log.Printf("the read cache did not sync in %s: reading the API server directly", readCacheSyncTimeout)
	})
	if !rc.ok {
		return nil
	}
	return rc
}

// readGroup is getGroup for a read-only caller.
func (h *Handler) readGroup(ctx context.Context, id string) (*laboratoryv1alpha1.LabGroup, error) {
	if rc := h.cached(); rc != nil {
		// A group without a namespace is not ready yet: the cache may be about to learn it, so ask the API server.
		if g, err := rc.groups.Get(crName(id)); err == nil && g.Status.Namespace != "" && g.DeletionTimestamp == nil {
			if !ownedBy(tenantOf(ctx), g) {
				return nil, notFoundForeign(kindLabGroup, id)
			}
			return g.DeepCopy(), nil
		}
	}
	return h.getGroup(ctx, id)
}

// readLab reads one Lab; a miss is read again from the API server.
func (h *Handler) readLab(ctx context.Context, ns, name string) (*laboratoryv1alpha1.Lab, error) {
	if rc := h.cached(); rc != nil {
		if l, err := rc.labs.Labs(ns).Get(name); err == nil {
			return l.DeepCopy(), nil
		}
	}
	return h.cs.LaboratoryV1alpha1().Labs(ns).Get(ctx, name, metav1.GetOptions{})
}

// readGroups is listGroups for a read-only caller; an empty answer is read again from the API server.
func (h *Handler) readGroups(ctx context.Context, selector string) ([]laboratoryv1alpha1.LabGroup, error) {
	rc := h.cached()
	if rc == nil {
		return h.listGroups(ctx, selector)
	}
	tenant := tenantOf(ctx)
	sel, err := labels.Parse(selector)
	if err != nil {
		return h.listGroups(ctx, selector)
	}
	all, err := rc.groups.List(sel)
	if err != nil {
		return h.listGroups(ctx, selector)
	}
	var out []laboratoryv1alpha1.LabGroup
	for _, g := range all {
		if ownedBy(tenant, g) {
			out = append(out, *g.DeepCopy())
		}
	}
	if len(out) == 0 {
		return h.listGroups(ctx, selector)
	}
	return out, nil
}

// readLabs lists the Labs of a namespace by a label selector; an empty answer is read again from the API server.
func (h *Handler) readLabs(ctx context.Context, ns, selector string) ([]laboratoryv1alpha1.Lab, error) {
	if rc := h.cached(); rc != nil {
		if sel, err := labels.Parse(selector); err == nil {
			if items, err := rc.labs.Labs(ns).List(sel); err == nil && len(items) > 0 {
				out := make([]laboratoryv1alpha1.Lab, len(items))
				for i, l := range items {
					out[i] = *l.DeepCopy()
				}
				return out, nil
			}
		}
	}
	list, err := h.cs.LaboratoryV1alpha1().Labs(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// readDevices lists the Devices of a namespace; an empty answer is read again from the API server.
func (h *Handler) readDevices(ctx context.Context, ns string) ([]laboratoryv1alpha1.Device, error) {
	if rc := h.cached(); rc != nil {
		if items, err := rc.devices.Devices(ns).List(labels.Everything()); err == nil && len(items) > 0 {
			out := make([]laboratoryv1alpha1.Device, len(items))
			for i, d := range items {
				out[i] = *d.DeepCopy()
			}
			return out, nil
		}
	}
	list, err := h.cs.LaboratoryV1alpha1().Devices(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// readReservations is listReservations (no selector) for a read-only caller.
func (h *Handler) readReservations(ctx context.Context) ([]laboratoryv1alpha1.Reservation, error) {
	if rc := h.cached(); rc != nil {
		if items, err := rc.reservations.List(labels.Everything()); err == nil && len(items) > 0 {
			t := tenantOf(ctx)
			var out []laboratoryv1alpha1.Reservation
			for _, r := range items {
				if ownsReservation(t, r) {
					out = append(out, *r.DeepCopy())
				}
			}
			if len(out) > 0 {
				return out, nil
			}
		}
	}
	return h.listReservations(ctx, "")
}
