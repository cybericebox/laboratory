//go:build linux

package nodeagent

import (
	"context"
	"fmt"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"os"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sort"
	"strings"
	"time"
)

// LifecycleReporter polls direct API identities and direct native state. Every
// mutation is an optimistic status merge limited to this node's report rows.
type LifecycleReporter struct {
	Owner    *os.File
	Client   client.Client
	Reader   client.Reader
	Observer *NativeRuntimeObserver
	Interval time.Duration
}

func (r *LifecycleReporter) Start(ctx context.Context) error {
	interval := r.Interval
	if interval < time.Second || interval > time.Minute {
		return fmt.Errorf("runtime observation interval outside 1s..1m")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer r.Observer.Runtime.Close()
	if r.Owner != nil {
		defer r.Owner.Close()
	}
	for {
		scan, cancel := context.WithTimeout(ctx, 30*time.Second)
		_ = r.sync(scan)
		cancel()
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
func (r *LifecycleReporter) sync(ctx context.Context) error {
	var pods corev1.PodList
	if err := r.Reader.List(ctx, &pods, client.MatchingFields{"spec.nodeName": r.Observer.NodeName}); err != nil {
		return err
	}
	var groups lab.LabGroupList
	if err := r.Reader.List(ctx, &groups); err != nil {
		return err
	}
	for i := range groups.Items {
		g := &groups.Items[i]
		if g.Spec.Lifecycle == nil {
			continue
		}
		rows := append([]lab.OwnedRuntimeIdentity(nil), g.Status.ServiceRuntime...)
		for j := range pods.Items {
			p := &pods.Items[j]
			component := GroupComponent(p)
			if p.Namespace != g.Status.Namespace || component == "" {
				continue
			}
			var dep appsv1.Deployment
			depUID := ""
			for _, owner := range p.OwnerReferences {
				if owner.Kind != "ReplicaSet" {
					continue
				}
				var rs appsv1.ReplicaSet
				if e := r.Reader.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: owner.Name}, &rs); e != nil {
					return e
				}
				if rs.UID != owner.UID {
					continue
				}
				for _, d := range rs.OwnerReferences {
					if d.Kind == "Deployment" {
						if e := r.Reader.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: d.Name}, &dep); e != nil {
							return e
						}
						if dep.UID == d.UID {
							depUID = string(dep.UID)
						}
					}
				}
			}
			if depUID == "" {
				continue
			}
			groupOwned := false
			for _, c := range dep.Spec.Template.Spec.Containers {
				for _, env := range c.Env {
					if env.Name == "GROUP_UID" && env.Value == string(g.UID) {
						groupOwned = true
					}
				}
			}
			if !groupOwned {
				continue
			}
			id := r.podIdentity(p, string(g.UID), g.Spec.Lifecycle.OperationID, g.Spec.Lifecycle.Revision)
			id.DeploymentUID = depUID
			id.Component = component
			rows = appendUniqueRow(rows, id)
		}
		base := g.DeepCopy()
		reports := otherNodeReports(g.Status.ServiceReports, r.Observer.NodeName)
		for _, id := range rows {
			if id.NodeName == r.Observer.NodeName {
				reports = append(reports, r.Observer.ObserveOwnedRuntime(ctx, id))
			}
		}
		g.Status.ServiceReports = reports
		if !reflect.DeepEqual(base.Status.ServiceReports, reports) {
			if e := r.Client.Status().Patch(ctx, g, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); e != nil {
				return e
			}
		}
	}
	var devices lab.DeviceList
	if e := r.Reader.List(ctx, &devices); e != nil {
		return e
	}
	for i := range devices.Items {
		d := &devices.Items[i]
		var parent lab.Lab
		if e := r.Reader.Get(ctx, client.ObjectKey{Namespace: d.Namespace, Name: d.Spec.LabRef}, &parent); e != nil {
			continue
		}
		if parent.Spec.Lifecycle == nil {
			continue
		}
		owned := false
		for _, o := range d.OwnerReferences {
			owned = owned || o.Kind == "Lab" && o.UID == parent.UID
		}
		if !owned {
			continue
		}
		rows := append([]lab.OwnedRuntimeIdentity(nil), d.Status.RuntimeInventory...)
		for j := range pods.Items {
			p := &pods.Items[j]
			if p.Namespace != d.Namespace || p.Labels[names.LabelDevice] != d.Spec.Name || p.Labels[names.LabelLab] != parent.Name {
				continue
			}
			id := r.podIdentity(p, string(parent.UID), parent.Spec.Lifecycle.OperationID, parent.Spec.Lifecycle.Revision)
			if d.Status.State != nil {
				id.Epoch = d.Status.State.Epoch
				id.Incarnation = d.Status.State.Incarnation
			}
			rows = appendUniqueRow(rows, id)
		}
		base := d.DeepCopy()
		reports := otherNodeReports(d.Status.RuntimeReports, r.Observer.NodeName)
		for _, id := range rows {
			if id.NodeName == r.Observer.NodeName {
				reports = append(reports, r.Observer.ObserveOwnedRuntime(ctx, id))
			}
		}
		d.Status.RuntimeReports = reports
		if !reflect.DeepEqual(base.Status.RuntimeReports, reports) {
			if e := r.Client.Status().Patch(ctx, d, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); e != nil {
				return e
			}
		}
	}
	return nil
}
func appendUniqueRow(rows []lab.OwnedRuntimeIdentity, id lab.OwnedRuntimeIdentity) []lab.OwnedRuntimeIdentity {
	for _, old := range rows {
		if sameRuntimeOwner(old, id) {
			return rows
		}
	}
	return append(rows, id)
}
func otherNodeReports(in []lab.OwnedRuntimeReport, node string) []lab.OwnedRuntimeReport {
	var out []lab.OwnedRuntimeReport
	for _, x := range in {
		if x.Identity.NodeName != node {
			out = append(out, x)
		}
	}
	return out
}
func (r *LifecycleReporter) podIdentity(p *corev1.Pod, owner, op string, rev int64) lab.OwnedRuntimeIdentity {
	id := lab.OwnedRuntimeIdentity{OwnerUID: owner, OperationID: op, Revision: rev, PodUID: string(p.UID), NodeName: p.Spec.NodeName, NodeBootID: r.Observer.BootID}
	for _, c := range append(append([]corev1.ContainerStatus(nil), p.Status.ContainerStatuses...), p.Status.InitContainerStatuses...) {
		if c.ContainerID != "" {
			id.ContainerIDs = append(id.ContainerIDs, strings.TrimPrefix(c.ContainerID, "containerd://"))
		}
	}
	for _, c := range p.Spec.Containers {
		addQuantity(&id.Requests, c.Resources.Requests)
		addQuantity(&id.Limits, c.Resources.Limits)
	}
	owners, e := r.Observer.Network.OVS.PortOwners()
	if e == nil {
		for key, uid := range owners {
			if uid == p.UID {
				id.PortKeys = append(id.PortKeys, key)
			}
		}
	}
	sort.Strings(id.ContainerIDs)
	sort.Strings(id.PortKeys)
	return id
}
func addQuantity(out *lab.ResourceAmounts, rl corev1.ResourceList) {
	if q, ok := rl[corev1.ResourceCPU]; ok {
		out.CPUMillicores += q.MilliValue()
	}
	if q, ok := rl[corev1.ResourceMemory]; ok {
		out.MemoryBytes += q.Value()
	}
}
