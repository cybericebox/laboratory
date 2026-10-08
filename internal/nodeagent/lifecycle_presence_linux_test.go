//go:build linux

package nodeagent

import (
	task "github.com/containerd/containerd/api/types/task"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeTaskPresenceUsesActualListProcessID(t *testing.T) {
	// containerd Tasks.List/getProcessState populates ID, not ContainerID.
	present, live, err := nativeTaskPresence([]*task.Process{{ID: "owned", Pid: 42, Status: task.Status_RUNNING}}, []string{"owned"})
	if err != nil || !present || !live {
		t.Fatalf("actual List row missed: %v %v %v", present, live, err)
	}
}
func TestNativeTaskPresenceNeverReleasesStoppedMetadata(t *testing.T) {
	for _, status := range []task.Status{task.Status_CREATED, task.Status_STOPPED, task.Status_PAUSED, task.Status_PAUSING} {
		present, live, err := nativeTaskPresence([]*task.Process{{ID: "owned", Pid: 42, Status: status}}, []string{"owned"})
		if err != nil || !present || live {
			t.Fatalf("nonrunning row misclassified: %v %v %v", present, live, err)
		}
	}
	for _, row := range []*task.Process{{ID: "owned", Status: task.Status_RUNNING}, {ID: "owned", Pid: 42, Status: task.Status_UNKNOWN}} {
		present, live, err := nativeTaskPresence([]*task.Process{row}, []string{"owned"})
		if !present || live || err == nil {
			t.Fatal("incomplete task row accepted")
		}
	}
	present, live, err := nativeTaskPresence([]*task.Process{{ID: "other", Pid: 42, Status: task.Status_RUNNING}}, []string{"owned"})
	if present || live || err != nil {
		t.Fatal("foreign runtime adopted")
	}
}

func TestNativeAllocatedCheckpointRequiresReadableOwnedCgroup(t *testing.T) {
	root := t.TempDir()
	owned := filepath.Join(root, "owned")
	if err := os.Mkdir(owned, 0700); err != nil {
		t.Fatal(err)
	}
	observer := &NativeRuntimeObserver{CgroupRoot: root}
	for _, test := range []struct {
		events               string
		populated, wantError bool
	}{{"populated 1\nfrozen 0\n", true, false}, {"populated 0\nfrozen 0\n", false, false}, {"frozen 0\n", false, true}} {
		if err := os.WriteFile(filepath.Join(owned, "cgroup.events"), []byte(test.events), 0600); err != nil {
			t.Fatal(err)
		}
		populated, err := observer.cgroupsPresent([]string{owned})
		if populated != test.populated || (err != nil) != test.wantError {
			t.Fatal(populated, err)
		}
	}
	if _, err := observer.cgroupsPresent([]string{root}); err == nil {
		t.Fatal("unowned cgroup root admitted")
	}
	if _, err := observer.cgroupsPresent([]string{filepath.Join(root, "..", "foreign")}); err == nil {
		t.Fatal("foreign cgroup path admitted")
	}
}
