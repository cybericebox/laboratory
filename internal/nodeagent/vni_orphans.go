//go:build linux

package nodeagent

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/internal/vniledger"
	poolpkg "github.com/cybericebox/laboratory/pkg/api/pool"
)

var (
	vniOrphansDropped = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "laboratory_node_vni_orphans_dropped_total",
		Help: "VNIs whose flows this node removed because no lease holds the number any more.",
	})
	vniOrphansUnknown = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "laboratory_node_vni_orphans_undecided",
		Help: "VNIs of this node whose lease could not be judged in the last pass (left untouched).",
	})
)

func init() { metrics.Registry.MustRegister(vniOrphansDropped, vniOrphansUnknown) }

// VNIOrphanReconciler compares, at start and then every Interval, the VNIs this node carries flows for (OpenFlow and the node's ledger)
// with the leases of the pool, and removes the flows of a VNI that no lease holds: the number is retired (its owner let it go) or back
// in the pool. This is the cleanup for an owner that is gone for good (a Lab deleted with the node away, a release that never ran).
//
// It touches nothing it is not sure about. A number whose slot is held (a lease record, or a legacy hold) belongs to someone and is
// left alone. A number the pool cannot judge (unreadable pool, a Device or Connection still naming a slot the pool says is free) is
// left alone and counted as undecided. The removal happens under the same lock as the wiring of the pods, with the slot read again
// under it, and the ledger entry goes only after the flows are confirmed gone.
type VNIOrphanReconciler struct {
	Reader   client.Reader
	OVS      *OVSManager
	Flows    *FlowManager
	Ledger   *vniledger.Ledger
	Recorder record.EventRecorder
	NodeName string
	Interval time.Duration
}

// NeedLeaderElection: every node-agent runs it for its own node.
func (r *VNIOrphanReconciler) NeedLeaderElection() bool { return false }

// Start runs a pass now (the start of the node-agent) and then every Interval until ctx ends.
func (r *VNIOrphanReconciler) Start(ctx context.Context) error {
	log := ctrl.Log.WithName("vni-orphans")
	every := r.Interval
	if every <= 0 {
		every = 2 * time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		dropped, undecided, err := r.Sweep(ctx)
		switch {
		case err != nil:
			log.Error(err, "VNI orphan pass failed; nothing was removed on a doubt")
		case dropped > 0 || undecided > 0:
			log.Info("VNI orphan pass", "dropped", dropped, "undecided", undecided)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// orphan says whether no lease holds the number. An error is "cannot tell".
func (r *VNIOrphanReconciler) orphan(ctx context.Context, vni uint) (bool, error) {
	state, err := poolpkg.SlotStateOf(ctx, r.Reader, names.VNIPoolPrefix, names.SystemNamespace, names.VNIPoolSize, vni)
	if err != nil {
		return false, err
	}
	switch state {
	case poolpkg.SlotHeld:
		return false, nil
	case poolpkg.SlotRetired:
		return true, nil
	}
	// Free: the pool has no memory of an owner. An object that still names the number says the pool and the objects disagree: not an
	// orphan to remove on a doubt.
	var devices lab.DeviceList
	if err := r.Reader.List(ctx, &devices); err != nil {
		return false, err
	}
	for i := range devices.Items {
		if v := devices.Items[i].Status.VNI; v != nil && *v == vni {
			return false, fmt.Errorf("VNI %d is free in the pool but Device %s/%s names it", vni, devices.Items[i].Namespace, devices.Items[i].Name)
		}
	}
	var connections lab.ConnectionList
	if err := r.Reader.List(ctx, &connections); err != nil {
		return false, err
	}
	for i := range connections.Items {
		if v := connections.Items[i].Status.VNI; v != nil && *v == vni {
			return false, fmt.Errorf("VNI %d is free in the pool but Connection %s/%s names it", vni, connections.Items[i].Namespace, connections.Items[i].Name)
		}
	}
	return true, nil
}

// Sweep does one pass; it returns how many VNIs it removed the flows of and how many it could not judge.
func (r *VNIOrphanReconciler) Sweep(ctx context.Context) (dropped, undecided int, err error) {
	flows, err := r.Flows.readNativeFlows(ctx)
	if err != nil {
		return 0, 0, err
	}
	entries, err := r.Ledger.Entries(ctx)
	if err != nil {
		return 0, 0, err
	}
	candidates := map[uint]bool{}
	for _, v := range entries {
		candidates[v] = true
	}
	// A flow of a VNI the ledger does not know (installed before the ledger existed, or by a node-agent that lost it) is written into
	// the ledger first: from now on the pool keeps the number from anyone else, and the pass below judges it like any other.
	for _, f := range flows {
		if !f.HasVNI || candidates[uint(f.VNI)] {
			continue
		}
		if err := r.Ledger.Note(uint(f.VNI)); err != nil {
			return 0, 0, err
		}
		candidates[uint(f.VNI)] = true
	}
	order := make([]uint, 0, len(candidates))
	for v := range candidates {
		order = append(order, v)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	log := ctrl.Log.WithName("vni-orphans")
	for _, v := range order {
		gone, err := r.orphan(ctx, v)
		if err != nil {
			log.Info("VNI left untouched: cannot tell whose it is", "vni", v, "reason", err.Error())
			undecided++
			continue
		}
		if !gone {
			continue
		}
		removed, err := r.drop(ctx, v)
		if err != nil {
			log.Info("VNI orphan not removed", "vni", v, "reason", err.Error())
			undecided++
			continue
		}
		if !removed {
			continue
		}
		dropped++
		vniOrphansDropped.Inc()
		log.Info("removed the flows of a VNI no lease holds", "vni", v)
		if r.Recorder != nil {
			r.Recorder.Eventf(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: r.NodeName}}, corev1.EventTypeNormal, "VNIOrphanRemoved", "removed the flows of VNI %d: no lease holds it", v)
		}
	}
	vniOrphansUnknown.Set(float64(undecided))
	return dropped, undecided, nil
}

// drop removes the flows of the number under the lock of the pod wiring, after reading the slot again.
func (r *VNIOrphanReconciler) drop(ctx context.Context, v uint) (bool, error) {
	r.OVS.vethMu.Lock()
	defer r.OVS.vethMu.Unlock()
	still, err := r.orphan(ctx, v)
	if err != nil || !still {
		return false, err
	}
	return true, r.Flows.RetireVNI(v)
}
