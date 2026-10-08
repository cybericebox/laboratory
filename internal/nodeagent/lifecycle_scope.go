//go:build linux

package nodeagent

import (
	"context"
	"fmt"
	tasks "github.com/containerd/containerd/api/services/tasks/v1"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/devicestate"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
	"time"
)

func (o *NativeRuntimeObserver) scopeCurrent(ctx context.Context, id lab.OwnedRuntimeIdentity) (bool, error) {
	if o.Reader == nil || id.NodeName != o.NodeName || id.NodeBootID != o.BootID || id.ScopeUID != id.OwnerUID || id.Generation < 1 {
		return false, ErrPortOwnerUnknown
	}
	if id.ScopeKind == "LabFabric" {
		var l lab.Lab
		if err := o.Reader.Get(ctx, client.ObjectKey{Namespace: id.Namespace, Name: id.LabName}, &l); err != nil {
			return false, err
		}
		if string(l.UID) != id.OwnerUID || l.Spec.Lifecycle == nil {
			return false, ErrPortOwnerChanged
		}
		current := l.Generation == id.Generation && l.Spec.Lifecycle.OperationID == id.OperationID && l.Spec.Lifecycle.Revision == id.Revision
		if !current {
			retained := false
			for _, old := range l.Status.ScopeInventory {
				retained = retained || reflect.DeepEqual(old, id)
			}
			if !retained {
				return false, ErrPortOwnerChanged
			}
		}
		return l.Spec.Lifecycle.IsStopped() || !current, nil
	}
	var groups lab.LabGroupList
	if err := o.Reader.List(ctx, &groups); err != nil {
		return false, err
	}
	for _, g := range groups.Items {
		if string(g.UID) == id.OwnerUID {
			if g.Generation != id.Generation || lab.LabGroupNamespaceOf(&g) != id.Namespace || g.Spec.Lifecycle == nil || g.Spec.Lifecycle.OperationID != id.OperationID || g.Spec.Lifecycle.Revision != id.Revision {
				return false, ErrPortOwnerChanged
			}
			return g.Spec.Lifecycle.IsStopped(), nil
		}
	}
	return false, ErrPortOwnerUnknown
}
func envValue(env []string, key string) string {
	for _, value := range env {
		if strings.HasPrefix(value, key+"=") {
			return strings.TrimPrefix(value, key+"=")
		}
	}
	return ""
}
func (o *NativeRuntimeObserver) ObserveScope(ctx context.Context, id lab.OwnedRuntimeIdentity) lab.OwnedRuntimeReport {
	now := metav1.Now()
	out := lab.OwnedRuntimeReport{Identity: id, RuntimeState: "Unknown", ObservedAt: &now}
	fail := func(err error) lab.OwnedRuntimeReport { out.Error = err.Error(); return out }
	stopped, err := o.scopeCurrent(ctx, id)
	if err != nil {
		return fail(err)
	}
	if !stopped {
		return fail(fmt.Errorf("scope operation is not stopped"))
	}
	native := namespaces.WithNamespace(ctx, o.Namespace)
	containers, err := o.Runtime.Containers(native)
	if err != nil {
		return fail(err)
	}
	metadata := map[string]bool{}
	owned := []string{}
	cgroups := []string{}
	for _, container := range containers {
		info, err := container.Info(native)
		if err != nil {
			return fail(err)
		}
		metadata[container.ID()] = true
		if info.Labels["io.kubernetes.pod.namespace"] != id.Namespace {
			continue
		}
		spec, err := container.Spec(native)
		if err != nil {
			return fail(err)
		}
		owner := ""
		group := ""
		if spec.Process != nil {
			owner = envValue(spec.Process.Env, "LIFECYCLE_LAB_UID")
			group = envValue(spec.Process.Env, "GROUP_UID")
		}
		match := id.ScopeKind == "LabFabric" && owner == id.OwnerUID || id.ScopeKind == "GroupScope" && group == id.OwnerUID
		if !match && owner == "" && group == "" {
			// A current API owner may attribute legacy runtime; absent APIs never do.
			var pod corev1.Pod
			if err := o.Reader.Get(ctx, client.ObjectKey{Namespace: id.Namespace, Name: info.Labels["io.kubernetes.pod.name"]}, &pod); err != nil {
				return fail(fmt.Errorf("unattributed legacy native runtime in scope"))
			}
			if string(pod.UID) != info.Labels["io.kubernetes.pod.uid"] {
				return fail(ErrPortOwnerChanged)
			}
			for _, reference := range pod.OwnerReferences {
				if reference.Kind == "Device" {
					var d lab.Device
					if err := o.Reader.Get(ctx, client.ObjectKey{Namespace: id.Namespace, Name: reference.Name}, &d); err != nil {
						return fail(err)
					}
					if d.UID != reference.UID {
						return fail(ErrPortOwnerChanged)
					}
					for _, parent := range d.OwnerReferences {
						match = match || id.ScopeKind == "LabFabric" && parent.Kind == "Lab" && string(parent.UID) == id.OwnerUID
					}
				}
			}
		}
		if !match {
			continue
		}
		owned = append(owned, container.ID())
		if spec.Linux != nil {
			path := devicestate.CgroupDir(o.CgroupRoot, spec.Linux.CgroupsPath)
			if path != "" {
				cgroups = append(cgroups, path)
			}
		}
	}
	list, err := o.Runtime.TaskService().List(native, &tasks.ListTasksRequest{})
	if err != nil {
		return fail(err)
	}
	for _, row := range list.Tasks {
		if row == nil {
			continue
		}
		key := row.ContainerID
		if key == "" {
			key = row.ID
		}
		if !metadata[key] {
			return fail(fmt.Errorf("native task has no attributable container metadata"))
		}
	}
	if o.Network == nil || o.Network.OVS == nil {
		return fail(fmt.Errorf("scope fabric observer unavailable"))
	}
	if err := o.Network.OVS.Ping(ctx); err != nil {
		return fail(err)
	}
	if err := o.captureScopeFabric(ctx, &id); err != nil {
		return fail(err)
	}
	out.Identity = id
	if err := o.writeRecord("scope", id, id); err != nil {
		return fail(err)
	}
	present, _, err := nativeTaskPresence(list.Tasks, owned)
	if err != nil {
		return fail(err)
	}
	if present {
		out.RuntimeState = "Allocated"
		return out
	}
	populated, err := o.cgroupsPresent(cgroups)
	if err != nil {
		return fail(err)
	}
	if populated {
		return fail(fmt.Errorf("scope cgroup remains populated"))
	}
	out.RuntimeAbsentAt = &now
	out.CgroupAbsentAt = &now
	if o.Network == nil || o.Network.OVS == nil || o.Network.Flows == nil {
		return fail(fmt.Errorf("scope fabric observer unavailable"))
	}
	if err := o.Network.OVS.Ping(ctx); err != nil {
		return fail(err)
	}
	if err := o.captureScopeFabric(ctx, &id); err != nil {
		return fail(err)
	}
	out.Identity = id
	// Durable declaration/native actual inventory before any fabric retirement.
	if err := o.writeRecord("scope", id, id); err != nil {
		return fail(err)
	}
	for _, port := range id.FabricPorts {
		if _, err := o.scopeCurrent(ctx, id); err != nil {
			return fail(err)
		}
		if err := o.Network.OVS.DelFabricPortOwned(port.Key, types.UID(port.OwnerUID), o.Network.Flows); err != nil {
			return fail(err)
		}
	}
	o.Network.OVS.vethMu.Lock()
	defer o.Network.OVS.vethMu.Unlock()
	if _, err := o.scopeCurrent(ctx, id); err != nil {
		return fail(err)
	}
	for _, vni := range id.VNIs {
		if err := o.Network.Flows.RetireVNI(vni); err != nil {
			return fail(err)
		}
	}
	// The correlated barrier is the physical flow authority; fsync the exact
	// object/node/operation scope before publishing its release acknowledgement.
	if err := o.writeRecord("scope-fabric-released", id, id); err != nil {
		return fail(err)
	}
	out.AttachmentsAbsentAt = &now
	out.RuntimeState = "Released"
	return out
}
func (o *NativeRuntimeObserver) captureScopeFabric(ctx context.Context, id *lab.OwnedRuntimeIdentity) error {
	if id.ScopeKind == "GroupScope" {
		id.AttachmentsComplete = true
		return nil
	}
	var devices lab.DeviceList
	if err := o.Reader.List(ctx, &devices, client.InNamespace(id.Namespace)); err != nil {
		return err
	}
	add := func(vni uint) {
		for _, old := range id.VNIs {
			if old == vni {
				return
			}
		}
		id.VNIs = append(id.VNIs, vni)
	}
	for _, binding := range id.VNIBindings {
		found := false
		if binding.Kind == "Device" {
			for _, d := range devices.Items {
				if d.Namespace == binding.Namespace && d.Name == binding.Name && string(d.UID) == binding.UID && d.Status.VNI != nil && *d.Status.VNI == binding.VNI {
					found = true
				}
			}
		}
		if binding.Kind == "Device" && !found {
			return fmt.Errorf("original Device VNI lease owner unavailable or replaced")
		}
		add(binding.VNI)
	}
	var connections lab.ConnectionList
	if err := o.Reader.List(ctx, &connections, client.InNamespace(id.Namespace)); err != nil {
		return err
	}
	for _, binding := range id.VNIBindings {
		if binding.Kind == "Connection" {
			found := false
			for _, c := range connections.Items {
				if c.Namespace == binding.Namespace && c.Name == binding.Name && string(c.UID) == binding.UID && c.Status.VNI != nil && *c.Status.VNI == binding.VNI {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("original Connection VNI lease owner unavailable or replaced")
			}
		}
	}
	for _, connection := range connections.Items {
		owned := false
		for _, parent := range connection.OwnerReferences {
			owned = owned || parent.Kind == "Lab" && string(parent.UID) == id.OwnerUID
		}
		if !owned {
			continue
		}
		if connection.Status.VNI != nil {
			add(*connection.Status.VNI)
		}
		for _, endpoint := range connection.Spec.Endpoints {
			key := patchPortName(connection.Namespace, connection.Name, endpoint.Device)
			o.Network.OVS.mu.Lock()
			row, err := o.Network.OVS.findPort(key)
			o.Network.OVS.mu.Unlock()
			if err != nil {
				return err
			}
			if row != nil {
				if row.ExternalIDs[fabricOwnerExternalID] != string(connection.UID) {
					return ErrPortOwnerUnknown
				}
				entry := lab.OwnedFabricPort{Key: key, OwnerUID: string(connection.UID), RowUUID: row.UUID}
				exists := false
				for _, old := range id.FabricPorts {
					exists = exists || reflect.DeepEqual(entry, old)
				}
				if !exists {
					id.FabricPorts = append(id.FabricPorts, entry)
				}
			}
		}
	}
	for _, binding := range id.VNIBindings {
		count := 0
		for _, d := range devices.Items {
			if d.Status.VNI != nil && *d.Status.VNI == binding.VNI {
				count++
			}
		}
		for _, c := range connections.Items {
			if c.Status.VNI != nil && *c.Status.VNI == binding.VNI {
				count++
			}
		}
		if count != 1 {
			return fmt.Errorf("VNI lease owner is not unique")
		}
	}
	id.AttachmentsComplete = true
	return nil
}

type fabricReceipt struct {
	ConnectionUID, LabUID, Namespace, LabName, OperationID, RowUUID, Key string
	Revision, Generation                                                 int64
}

func (o *NativeRuntimeObserver) prepareFabric(key string, uid types.UID, row string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var connections lab.ConnectionList
	if err := o.Reader.List(ctx, &connections); err != nil {
		return err
	}
	for _, conn := range connections.Items {
		if conn.UID != uid {
			continue
		}
		var parent lab.Lab
		if err := o.Reader.Get(ctx, client.ObjectKey{Namespace: conn.Namespace, Name: conn.Spec.LabRef}, &parent); err != nil {
			return err
		}
		owned := false
		for _, reference := range conn.OwnerReferences {
			owned = owned || reference.Kind == "Lab" && reference.UID == parent.UID
		}
		if !owned {
			return ErrPortOwnerChanged
		}
		op, rev := nativeLabOperation(&parent)
		receipt := fabricReceipt{ConnectionUID: string(uid), LabUID: string(parent.UID), Namespace: conn.Namespace, LabName: parent.Name, OperationID: op, Revision: rev, Generation: parent.Generation, RowUUID: row, Key: key}
		return o.writeRecord("fabric-inventory-"+key, lab.OwnedRuntimeIdentity{ScopeUID: string(uid), NodeName: o.NodeName, NodeBootID: o.BootID}, receipt)
	}
	return ErrPortOwnerUnknown
}
func (o *NativeRuntimeObserver) commitFabric(key string, uid types.UID, row string) error {
	id := lab.OwnedRuntimeIdentity{ScopeUID: string(uid), NodeName: o.NodeName, NodeBootID: o.BootID}
	var receipt fabricReceipt
	if err := o.readRecord("fabric-inventory-"+key, id, &receipt); err != nil {
		return err
	}
	if receipt.ConnectionUID != string(uid) || receipt.Key != key || receipt.RowUUID != row {
		return ErrPortOwnerChanged
	}
	return o.writeRecord("fabric-released-"+key, id, receipt)
}
func (o *NativeRuntimeObserver) fabricAbsent(key string, uid types.UID) error {
	var receipt fabricReceipt
	if err := o.readRecord("fabric-released-"+key, lab.OwnedRuntimeIdentity{ScopeUID: string(uid), NodeName: o.NodeName, NodeBootID: o.BootID}, &receipt); err != nil {
		return err
	}
	if receipt.ConnectionUID != string(uid) || receipt.Key != key || receipt.RowUUID == "" {
		return ErrPortOwnerUnknown
	}
	return nil
}
func (o *NativeRuntimeObserver) portAbsent(key string, uid types.UID) error {
	var id lab.OwnedRuntimeIdentity
	if err := o.readRecord("owner", lab.OwnedRuntimeIdentity{PodUID: string(uid), NodeName: o.NodeName, NodeBootID: o.BootID}, &id); err != nil {
		return err
	}
	var receipt cleanupReceipt
	if err := o.readRecord("cleanup-"+key, id, &receipt); err != nil {
		return err
	}
	if receipt.PortUUID == "" || !reflect.DeepEqual(receipt.Identity, id) {
		return ErrPortOwnerUnknown
	}
	return nil
}
