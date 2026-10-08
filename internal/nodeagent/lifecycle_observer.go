//go:build linux

package nodeagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	tasks "github.com/containerd/containerd/api/services/tasks/v1"
	task "github.com/containerd/containerd/api/types/task"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/devicestate"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// NativeRuntimeObserver uses direct containerd task and cgroup views. API absence
// is deliberately irrelevant. It never fills missing ownership from absence.
type NativeRuntimeObserver struct {
	Runtime                                             *containerd.Client
	Namespace, NodeName, BootID, CgroupRoot, JournalDir string
	Network                                             *NetworkAttachReconciler
	Reader                                              runtimeclient.Reader
	mu                                                  sync.Mutex
}

func (o *NativeRuntimeObserver) ObserveOwnedRuntime(ctx context.Context, id lab.OwnedRuntimeIdentity) lab.OwnedRuntimeReport {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := metav1.Now()
	out := lab.OwnedRuntimeReport{Identity: id, RuntimeState: "Unknown", ObservedAt: &now}
	fail := func(err error) lab.OwnedRuntimeReport { out.Error = err.Error(); return out }
	if id.OwnerUID == "" || id.PodUID == "" || id.OperationID == "" || id.Revision <= 0 || id.NodeName != o.NodeName || id.NodeBootID != o.BootID || o.BootID == "" || o.Runtime == nil {
		return fail(fmt.Errorf("native ownership incomplete or node boot changed"))
	}
	ctx = namespaces.WithNamespace(ctx, o.Namespace)
	containers, err := o.Runtime.Containers(ctx)
	if err != nil {
		return fail(err)
	}
	owned := map[string]bool{}
	paths := map[string]bool{}
	for _, c := range containers {
		info, e := c.Info(ctx)
		if e != nil {
			return fail(e)
		}
		if info.Labels["io.kubernetes.pod.uid"] != id.PodUID {
			continue
		}
		owned[c.ID()] = true
		spec, e := c.Spec(ctx)
		if e != nil {
			return fail(e)
		}
		if spec.Linux != nil {
			if p := devicestate.CgroupDir(o.CgroupRoot, spec.Linux.CgroupsPath); p != "" {
				paths[p] = true
			}
		}
	}
	// First present inspection durably captures all containers, including sandbox
	// and init containers. It cannot create a release inventory from an empty list.
	if len(owned) > 0 {
		for k := range owned {
			if !runtimeContainsID(id.ContainerIDs, k) {
				id.ContainerIDs = append(id.ContainerIDs, k)
			}
		}
		for k := range paths {
			if !runtimeContainsID(id.CgroupPaths, k) {
				id.CgroupPaths = append(id.CgroupPaths, k)
			}
		}
		if len(id.CgroupPaths) == 0 {
			return fail(fmt.Errorf("owned cgroup path unavailable"))
		}
		sort.Strings(id.ContainerIDs)
		sort.Strings(id.CgroupPaths)
		sort.Strings(id.PortKeys)
		if err := o.writeRecord("inventory", id, id); err != nil {
			return fail(err)
		}
		if err := o.writeRecord("owner", lab.OwnedRuntimeIdentity{PodUID: id.PodUID, NodeName: id.NodeName, NodeBootID: id.NodeBootID}, id); err != nil {
			return fail(err)
		}
	} else {
		var saved lab.OwnedRuntimeIdentity
		if err := o.readRecord("inventory", id, &saved); err != nil {
			return fail(err)
		}
		if !sameRuntimeOwner(saved, id) {
			return fail(fmt.Errorf("native inventory mismatch"))
		}
		id = saved
	}
	out.Identity = id
	if len(id.ContainerIDs) == 0 || len(id.CgroupPaths) == 0 || len(id.PortKeys) == 0 && !id.AttachmentsComplete {
		return fail(fmt.Errorf("incomplete native process/attachment inventory"))
	}
	ts, err := o.Runtime.TaskService().List(ctx, &tasks.ListTasksRequest{})
	if err != nil {
		return fail(err)
	}
	present, live, err := nativeTaskPresence(ts.Tasks, id.ContainerIDs)
	if err != nil {
		return fail(err)
	}
	if present {
		populated, err := o.cgroupsPresent(id.CgroupPaths)
		if err != nil {
			return fail(err)
		}
		out.RuntimeState = "Releasing"
		if live {
			if !populated {
				return fail(fmt.Errorf("live owned task has no populated cgroup"))
			}
			out.RuntimeState = "Allocated"
		}
		return out
	}
	// STOPPED metadata alone is not death: all exact native task rows must disappear.
	out.RuntimeAbsentAt = &now
	for _, p := range id.CgroupPaths {
		rel, e := filepath.Rel(o.CgroupRoot, p)
		if e != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return fail(fmt.Errorf("unowned cgroup path"))
		}
		err = filepath.WalkDir(p, func(path string, d os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if d.IsDir() {
				b, e := os.ReadFile(filepath.Join(path, "cgroup.events"))
				if e != nil {
					return e
				}
				if strings.Contains(string(b), "populated 1") {
					return fmt.Errorf("owned cgroup still populated")
				}
			}
			return nil
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
	}
	cg := metav1.Now()
	out.CgroupAbsentAt = &cg
	if o.Network == nil {
		return fail(fmt.Errorf("native attachment observer unavailable"))
	}
	for _, key := range id.PortKeys {
		var receipt cleanupReceipt
		if e := o.readRecord("cleanup-"+key, id, &receipt); e == nil && receipt.PortUUID != "" && reflect.DeepEqual(receipt.Identity, id) {
			// Recovery consumes only this pre-retirement owned physical proof. A new
			// row (even the same stable key) or kernel link prevents completion.
			o.Network.OVS.mu.Lock()
			row, e := o.Network.OVS.portSnapshotLocked(key)
			o.Network.OVS.mu.Unlock()
			if e != nil {
				return fail(e)
			}
			_, e = netlink.LinkByName(key)
			var absent netlink.LinkNotFoundError
			if row == nil && errors.As(e, &absent) {
				receipt.Complete = true
				receipt.At = time.Now().UTC()
				if e = o.writeRecord("cleanup-"+key, id, receipt); e != nil {
					return fail(e)
				}
				continue
			}
			if receipt.Complete {
				return fail(fmt.Errorf("retired attachment reappeared"))
			}
		}
		// Persist the prepared retirement after flow barrier and kernel absence,
		// BEFORE deletion of the exact OVS owner row. A prepared receipt alone cannot
		// authorize release; recovery verifies the exact guarded retirement outcome.
		receipt = cleanupReceipt{Identity: id}
		e := o.Network.DelVethWithFlowsOwnedJournaled(key, types.UID(id.PodUID), func(row string) error { receipt.PortUUID = row; return o.writeRecord("cleanup-"+key, id, receipt) })
		if e != nil {
			return fail(e)
		}
		receipt.Complete = true
		receipt.At = time.Now().UTC()
		if e = o.writeRecord("cleanup-"+key, id, receipt); e != nil {
			return fail(e)
		}
	}
	at := metav1.Now()
	out.AttachmentsAbsentAt = &at
	out.RuntimeState = "Released"
	return out
}

type cleanupReceipt struct {
	Identity lab.OwnedRuntimeIdentity
	PortUUID string
	Complete bool
	At       time.Time
}

func runtimeContainsID(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
func sameRuntimeOwner(a, b lab.OwnedRuntimeIdentity) bool {
	return a.OwnerUID == b.OwnerUID && a.PodUID == b.PodUID && a.NodeName == b.NodeName && a.NodeBootID == b.NodeBootID && a.OperationID == b.OperationID && a.Revision == b.Revision && a.DeploymentUID == b.DeploymentUID && a.Epoch == b.Epoch && a.Incarnation == b.Incarnation
}
func (o *NativeRuntimeObserver) recordPath(kind string, id lab.OwnedRuntimeIdentity) string {
	b, _ := json.Marshal([]any{kind, id.OwnerUID, id.PodUID, id.ScopeKind, id.ScopeUID, id.Generation, id.OperationID, id.Revision, id.NodeName, id.NodeBootID, id.Epoch, id.Incarnation, id.DeploymentUID})
	h := sha256.Sum256(b)
	return filepath.Join(o.JournalDir, hex.EncodeToString(h[:])+".json")
}
func (o *NativeRuntimeObserver) readRecord(kind string, id lab.OwnedRuntimeIdentity, v any) error {
	b, e := os.ReadFile(o.recordPath(kind, id))
	if e != nil {
		return e
	}
	if len(b) > 1<<20 {
		return fmt.Errorf("runtime journal too large")
	}
	return json.Unmarshal(b, v)
}
func (o *NativeRuntimeObserver) writeRecord(kind string, id lab.OwnedRuntimeIdentity, v any) error {
	if o.JournalDir == "" {
		return fmt.Errorf("runtime journal directory missing")
	}
	if e := os.MkdirAll(o.JournalDir, 0700); e != nil {
		return e
	}
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(o.JournalDir, ".runtime-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e != nil {
		f.Close()
		return e
	}
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(name, o.recordPath(kind, id)); e != nil {
		return e
	}
	dir, e := os.Open(o.JournalDir)
	if e != nil {
		return e
	}
	defer dir.Close()
	return dir.Sync()
}

// preparePortRetirement is called by every cleanup caller under the OVS owner
// lock, including CNI/reconcile. No index means no lifecycle proof is produced.
func (o *NativeRuntimeObserver) preparePortRetirement(key string, uid types.UID, row string) error {
	var id lab.OwnedRuntimeIdentity
	e := o.readRecord("owner", lab.OwnedRuntimeIdentity{PodUID: string(uid), NodeName: o.NodeName, NodeBootID: o.BootID}, &id)
	if errors.Is(e, os.ErrNotExist) {
		return ErrPortOwnerUnknown
	}
	if e != nil {
		return e
	}
	if id.PodUID != string(uid) || id.NodeBootID != o.BootID || !runtimeContainsID(id.PortKeys, key) {
		return ErrPortOwnerUnknown
	}
	var obligations []lab.OwnedRuntimeIdentity
	physical := lab.OwnedRuntimeIdentity{PodUID: id.PodUID, NodeName: id.NodeName, NodeBootID: id.NodeBootID}
	_ = o.readRecord("obligations", physical, &obligations)
	obligations = append(obligations, id)
	for _, target := range obligations {
		if target.OwnerUID == id.OwnerUID && target.PodUID == id.PodUID && target.NodeBootID == id.NodeBootID && runtimeContainsID(target.PortKeys, key) {
			if err := o.writeRecord("cleanup-"+key, target, cleanupReceipt{Identity: target, PortUUID: row}); err != nil {
				return err
			}
		}
	}
	return nil
}

// nativeTaskPresence consumes the actual Tasks.List contract. Containerd's
// getProcessState fills Process.ID for the container's main task; ContainerID
// is normally empty. Any exact native row retains ownership, including STOPPED.
func nativeTaskPresence(rows []*task.Process, ids []string) (present, live bool, err error) {
	for _, row := range rows {
		if row == nil {
			continue
		}
		id := row.ContainerID
		if id == "" {
			id = row.ID
		}
		if !runtimeContainsID(ids, id) {
			continue
		}
		present = true
		if row.Status == task.Status_UNKNOWN || row.Status == task.Status_RUNNING && row.Pid == 0 {
			return true, false, fmt.Errorf("owned native task state incomplete")
		}
		if row.Status == task.Status_RUNNING && row.Pid > 0 {
			live = true
		}
	}
	return present, live, nil
}

func (o *NativeRuntimeObserver) cgroupsPresent(paths []string) (bool, error) {
	populated := false
	for _, path := range paths {
		relative, err := filepath.Rel(o.CgroupRoot, path)
		if err != nil || relative == "." || strings.HasPrefix(relative, "..") {
			return false, fmt.Errorf("unowned cgroup path")
		}
		data, err := os.ReadFile(filepath.Join(path, "cgroup.events"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		known := false
		for _, line := range strings.Split(string(data), "\n") {
			if line == "populated 1" {
				known = true
				populated = true
			}
			if line == "populated 0" {
				known = true
			}
		}
		if !known {
			return false, fmt.Errorf("owned cgroup observation incomplete")
		}
	}
	return populated, nil
}

// prepareRuntimeBeforeRetirement runs under the common OVS owner lock BEFORE
// deleting a flow, kernel link or row. Durable native inventory is recoverable
// even when NetAttach wins the stop watcher race and the API Pod later vanishes.
func (o *NativeRuntimeObserver) prepareRuntimeBeforeRetirement(key string, uid types.UID, row string) error {
	if o.Reader == nil || uid == "" || row == "" {
		return ErrPortOwnerUnknown
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var id lab.OwnedRuntimeIdentity
	_ = o.readRecord("owner", lab.OwnedRuntimeIdentity{PodUID: string(uid), NodeName: o.NodeName, NodeBootID: o.BootID}, &id)
	var pods corev1.PodList
	if err := o.Reader.List(ctx, &pods, runtimeclient.MatchingFields{"spec.nodeName": o.NodeName}); err != nil {
		return err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.UID != uid {
			continue
		}
		id.PodUID = string(uid)
		id.NodeName = o.NodeName
		id.NodeBootID = o.BootID
		id.Namespace = p.Namespace
		if p.Labels[names.LabelLab] != "" {
			var device lab.Device
			for _, owner := range p.OwnerReferences {
				if owner.Kind == "Device" {
					if err := o.Reader.Get(ctx, runtimeclient.ObjectKey{Namespace: p.Namespace, Name: owner.Name}, &device); err != nil {
						return err
					}
					if device.UID != owner.UID {
						return ErrPortOwnerChanged
					}
				}
			}
			if device.UID == "" {
				return ErrPortOwnerUnknown
			}
			var parent lab.Lab
			if err := o.Reader.Get(ctx, runtimeclient.ObjectKey{Namespace: p.Namespace, Name: device.Spec.LabRef}, &parent); err != nil {
				return err
			}
			owned := false
			for _, owner := range device.OwnerReferences {
				owned = owned || owner.Kind == "Lab" && owner.UID == parent.UID
			}
			if !owned {
				return ErrPortOwnerChanged
			}
			id.OwnerUID = string(parent.UID)
			id.ScopeUID = string(device.UID)
			id.LabName = parent.Name
			id.Generation = parent.Generation
			id.OperationID, id.Revision = nativeLabOperation(&parent)
			if device.Status.State != nil {
				id.Epoch = device.Status.State.Epoch
				id.Incarnation = device.Status.State.Incarnation
			}
		} else {
			var groups lab.LabGroupList
			if err := o.Reader.List(ctx, &groups); err != nil {
				return err
			}
			for j := range groups.Items {
				g := &groups.Items[j]
				if lab.LabGroupNamespaceOf(g) == p.Namespace {
					id.OwnerUID = string(g.UID)
					id.ScopeUID = string(g.UID)
					id.Generation = g.Generation
					id.Component = GroupComponent(p)
					if g.Spec.Lifecycle == nil {
						id.OperationID = "legacy-group-" + string(g.UID) + "-" + fmt.Sprint(g.Generation)
						id.Revision = 1
					} else {
						id.OperationID = g.Spec.Lifecycle.OperationID
						id.Revision = g.Spec.Lifecycle.Revision
					}
					break
				}
			}
		}
		break
	}
	if id.OwnerUID == "" || id.Namespace == "" || id.OperationID == "" || id.Revision < 1 || id.PodUID != string(uid) {
		return ErrPortOwnerUnknown
	}
	// A saved owner remains bound to the current exact lifecycle, never an old
	// operation merely inferred from a vanished API Pod.
	if id.LabName != "" {
		var parent lab.Lab
		if err := o.Reader.Get(ctx, runtimeclient.ObjectKey{Namespace: id.Namespace, Name: id.LabName}, &parent); err != nil {
			return err
		}
		if string(parent.UID) != id.OwnerUID {
			return ErrPortOwnerChanged
		}
		id.OperationID, id.Revision = nativeLabOperation(&parent)
		id.Generation = parent.Generation
	}
	owners, err := o.Network.OVS.portOwnersLocked()
	if err != nil {
		return err
	}
	for candidate, owner := range owners {
		if owner == uid && !runtimeContainsID(id.PortKeys, candidate) {
			id.PortKeys = append(id.PortKeys, candidate)
		}
	}
	if !runtimeContainsID(id.PortKeys, key) {
		return ErrPortOwnerChanged
	}
	id.AttachmentsComplete = true
	native := namespaces.WithNamespace(ctx, o.Namespace)
	containers, err := o.Runtime.Containers(native)
	if err != nil {
		return err
	}
	for _, container := range containers {
		info, err := container.Info(native)
		if err != nil {
			return err
		}
		if info.Labels["io.kubernetes.pod.uid"] != string(uid) {
			continue
		}
		if !runtimeContainsID(id.ContainerIDs, container.ID()) {
			id.ContainerIDs = append(id.ContainerIDs, container.ID())
		}
		spec, err := container.Spec(native)
		if err != nil {
			return err
		}
		if spec.Linux != nil {
			path := devicestate.CgroupDir(o.CgroupRoot, spec.Linux.CgroupsPath)
			if path != "" && !runtimeContainsID(id.CgroupPaths, path) {
				id.CgroupPaths = append(id.CgroupPaths, path)
			}
		}
	}
	if len(id.ContainerIDs) == 0 || len(id.CgroupPaths) == 0 {
		return ErrPortOwnerUnknown
	}
	sort.Strings(id.ContainerIDs)
	sort.Strings(id.CgroupPaths)
	sort.Strings(id.PortKeys)
	if err := o.writeRecord("inventory", id, id); err != nil {
		return err
	}
	physical := lab.OwnedRuntimeIdentity{PodUID: id.PodUID, NodeName: id.NodeName, NodeBootID: id.NodeBootID}
	var obligations []lab.OwnedRuntimeIdentity
	_ = o.readRecord("obligations", physical, &obligations)
	exists := false
	for _, old := range obligations {
		exists = exists || reflect.DeepEqual(old, id)
	}
	if !exists {
		obligations = append(obligations, id)
	}
	if err := o.writeRecord("obligations", physical, obligations); err != nil {
		return err
	}
	return o.writeRecord("owner", physical, id)
}
