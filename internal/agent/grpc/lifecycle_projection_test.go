package grpc

import (
	"testing"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/limits"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// These are supplied observations, not a fabricated native producer. The public
// projection must reject contradictions even while native capabilities stay off.
func projectionCertificate() *lab.Lab {
	// Independently published timestamps intentionally have no aggregate ordering.
	observed := metav1.Unix(1000, 0)
	released := metav1.Unix(900, 0)
	fenced := metav1.Unix(1100, 0)
	return &lab.Lab{ObjectMeta: metav1.ObjectMeta{UID: "current-lab", Generation: 7}, Spec: lab.LabSpec{Devices: []lab.DeviceTemplate{{Name: "web", Type: lab.DeviceTypeContainer}}, Lifecycle: &lab.LabLifecycleSpec{DesiredState: "Stopped", OperationID: "stop", Revision: 4, SnapshotMode: "Skip"}}, Status: lab.LabStatus{Lifecycle: &lab.LabLifecycleStatus{LabUID: "current-lab", ObservedGeneration: 7, OperationID: "stop", Revision: 4, ObservedState: "Stopped", SnapshotComplete: true, AccessFenced: true, AccessFencedAt: &fenced, AccessFenceVPNBootID: "boot"}, Resources: &lab.RuntimeAllocation{OperationID: "stop", Revision: 4, RuntimeState: "Released", ObservedAt: &observed, ReleasedAt: &released, StorageState: "Retained", PhysicalStorageBytesAvailable: true, PhysicalStorageBytes: 512, SnapshotQuotaBytes: 4096}}}
}

func projectListAndMonitoring(t *testing.T, l *lab.Lab) *protobuf.ResourceAllocation {
	t.Helper()
	sizing := limits.Limits{DeviceDefaultCPU: 75, DeviceDefaultMemory: 96 << 20}
	listed := labToProto(l, sizing)
	monitored := labMonitoringToProto(l, "group", sizing)
	if len(monitored.SpecJson) != 0 || !proto.Equal(listed.Status.Lifecycle, monitored.Status.Lifecycle) || !proto.Equal(listed.Status.Resources, monitored.Status.Resources) {
		t.Fatal("List/Monitoring diverged or monitoring exposed spec")
	}
	return listed.Status.Resources
}

func TestLifecycleProjectionStaleStorageAndMissingObservation(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*lab.Lab)
	}{
		{"stale operation", func(l *lab.Lab) { l.Status.Resources.OperationID = "old" }},
		{"stale revision", func(l *lab.Lab) { l.Status.Resources.Revision = 3 }},
		{"stale UID", func(l *lab.Lab) { l.Status.Lifecycle.LabUID = "old-lab" }},
		{"stale generation", func(l *lab.Lab) { l.Status.Lifecycle.ObservedGeneration = 6 }},
		{"missing lifecycle identity", func(l *lab.Lab) { l.Status.Lifecycle = nil }},
		{"missing allocated observation time", func(l *lab.Lab) { l.Status.Resources.RuntimeState = "Allocated"; l.Status.Resources.ObservedAt = nil }},
		{"missing releasing observation time", func(l *lab.Lab) { l.Status.Resources.RuntimeState = "Releasing"; l.Status.Resources.ObservedAt = nil }},
		{"missing usage observation time", func(l *lab.Lab) { l.Status.Resources.RuntimeState = "Unknown"; l.Status.Resources.ObservedAt = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := projectionCertificate()
			l.Status.Resources.StorageState = "Deleted"
			l.Status.Resources.PhysicalStorageBytes = 0
			l.Status.Resources.AllocatedRequests = lab.ResourceAmounts{CPUMillicores: 100, MemoryBytes: 128 << 20}
			l.Status.Resources.UsageAvailable = true
			l.Status.Resources.Used = &lab.ResourceAmounts{CPUMillicores: 12, MemoryBytes: 8 << 20}
			tc.mut(l)
			got := projectListAndMonitoring(t, l)
			if got.StorageState != "Unknown" || got.PhysicalStorageBytesAvailable || got.PhysicalStorageBytes != 0 {
				t.Errorf("stale storage certificate remained available: %v", got)
			}
			if got.RuntimeState != "Unknown" || got.UsageAvailable || got.Used != nil || got.ObservedUnixMs != 0 || got.ReleasedUnixMs != 0 {
				t.Errorf("inexact observation remained available: %v", got)
			}
			if got.SnapshotQuotaBytes != 4096 || got.AllocatedRequests.GetCpuMillicores() != 100 || got.AllocatedRequests.GetMemoryBytes() != 128<<20 {
				t.Errorf("known quota/holdings lost: %v", got)
			}
		})
	}
	t.Run("missing resource observation", func(t *testing.T) {
		l := projectionCertificate()
		l.Status.Resources = nil
		got := projectListAndMonitoring(t, l)
		if got.RuntimeState != "Unknown" || got.StorageState != "Unknown" || got.PhysicalStorageBytesAvailable || got.UsageAvailable || got.AllocatedRequests.GetCpuMillicores() != 75 || got.AllocatedRequests.GetMemoryBytes() != 96<<20 {
			t.Fatalf("missing observation became free/available: %v", got)
		}
	})
}

func TestLifecycleProjectionReleaseRequiresCaptureAndVPNFence(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*lab.Lab)
	}{
		{"required capture pending", func(l *lab.Lab) {
			l.Spec.Lifecycle.SnapshotMode = "Required"
			l.Status.Lifecycle.SnapshotComplete = false
		}},
		{"required capture failed", func(l *lab.Lab) {
			l.Spec.Lifecycle.SnapshotMode = "Required"
			l.Status.Lifecycle.ObservedState = "StopFailed"
			l.Status.Lifecycle.SnapshotComplete = false
			l.Status.Lifecycle.Error = "capture failed"
		}},
		{"contradictory required capture failure", func(l *lab.Lab) {
			l.Spec.Lifecycle.SnapshotMode = "Required"
			l.Status.Lifecycle.Error = "capture failed"
		}},
		{"VPN unfenced", func(l *lab.Lab) { l.Spec.VPN.Enabled = true; l.Status.Lifecycle.AccessFenced = false }},
		{"VPN fence time missing", func(l *lab.Lab) { l.Spec.VPN.Enabled = true; l.Status.Lifecycle.AccessFencedAt = nil }},
		{"VPN boot missing", func(l *lab.Lab) { l.Spec.VPN.Enabled = true; l.Status.Lifecycle.AccessFenceVPNBootID = "" }},
		{"VPN stale fence identity", func(l *lab.Lab) { l.Spec.VPN.Enabled = true; l.Status.Lifecycle.OperationID = "old" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := projectionCertificate()
			tc.mut(l)
			got := projectListAndMonitoring(t, l)
			if got.RuntimeState != "Unknown" || got.ReleasedUnixMs != 0 || got.AllocatedRequests.GetCpuMillicores() != 75 || got.AllocatedRequests.GetMemoryBytes() != 96<<20 {
				t.Fatalf("incoherent certificate released capacity: %v", got)
			}
		})
	}
}

func TestLifecycleProjectionValidCurrentAndLegacyObservations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mut      func(*lab.Lab)
		runtime  string
		cpu, mem int64
		usage    bool
	}{
		{"current no VPN skip", func(l *lab.Lab) {
			l.Status.Lifecycle.SnapshotComplete = false
			l.Status.Lifecycle.AccessFenced = false
			l.Status.Lifecycle.AccessFencedAt = nil
			l.Status.Lifecycle.AccessFenceVPNBootID = ""
		}, "Released", 0, 0, false},
		{"current no VPN required", func(l *lab.Lab) {
			l.Spec.Lifecycle.SnapshotMode = "Required"
			l.Status.Lifecycle.AccessFenced = false
			l.Status.Lifecycle.AccessFencedAt = nil
			l.Status.Lifecycle.AccessFenceVPNBootID = ""
		}, "Released", 0, 0, false},
		{"current VPN required", func(l *lab.Lab) { l.Spec.VPN.Enabled = true; l.Spec.Lifecycle.SnapshotMode = "Required" }, "Released", 0, 0, false},
		{"current VPN skip", func(l *lab.Lab) { l.Spec.VPN.Enabled = true; l.Status.Lifecycle.SnapshotComplete = false }, "Released", 0, 0, false},
		{"switch-only no VPN", func(l *lab.Lab) {
			l.Spec.Devices = []lab.DeviceTemplate{{Name: "switch", Type: lab.DeviceTypeUnmanagedSwitch}}
			l.Spec.Lifecycle.SnapshotMode = "Required"
			l.Status.Lifecycle.AccessFenced = false
			l.Status.Lifecycle.AccessFencedAt = nil
			l.Status.Lifecycle.AccessFenceVPNBootID = ""
		}, "Released", 0, 0, false},
		{"current allocated usage", func(l *lab.Lab) {
			l.Status.Resources.RuntimeState = "Allocated"
			l.Status.Resources.AllocatedRequests = lab.ResourceAmounts{CPUMillicores: 100, MemoryBytes: 128 << 20}
			l.Status.Resources.UsageAvailable = true
			l.Status.Resources.Used = &lab.ResourceAmounts{CPUMillicores: 12, MemoryBytes: 8 << 20}
		}, "Allocated", 100, 128 << 20, true},
		{"current releasing usage", func(l *lab.Lab) {
			l.Status.Resources.RuntimeState = "Releasing"
			l.Status.Resources.AllocatedRequests = lab.ResourceAmounts{CPUMillicores: 100, MemoryBytes: 128 << 20}
			l.Status.Resources.UsageAvailable = true
			l.Status.Resources.Used = &lab.ResourceAmounts{CPUMillicores: 12, MemoryBytes: 8 << 20}
		}, "Releasing", 100, 128 << 20, true},
		{"legacy resource observation", func(l *lab.Lab) { l.Spec.Lifecycle = nil; l.Status.Lifecycle = nil }, "Released", 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := projectionCertificate()
			tc.mut(l)
			got := projectListAndMonitoring(t, l)
			if got.RuntimeState != tc.runtime || got.AllocatedRequests.GetCpuMillicores() != tc.cpu || got.AllocatedRequests.GetMemoryBytes() != tc.mem || got.UsageAvailable != tc.usage || got.StorageState != "Retained" || !got.PhysicalStorageBytesAvailable || got.PhysicalStorageBytes != 512 || got.SnapshotQuotaBytes != 4096 {
				t.Fatalf("valid certificate/legacy semantics lost: %v", got)
			}
			if tc.usage && (got.Used.GetCpuMillicores() != 12 || got.Used.GetMemoryBytes() != 8<<20) {
				t.Fatal("current measured usage lost")
			}
		})
	}
	t.Run("legacy missing observation remains absent", func(t *testing.T) {
		l := projectionCertificate()
		l.Spec.Lifecycle = nil
		l.Status.Lifecycle = nil
		l.Status.Resources = nil
		if got := projectListAndMonitoring(t, l); got != nil {
			t.Fatal("legacy absence acquired a release certificate")
		}
	})
}
