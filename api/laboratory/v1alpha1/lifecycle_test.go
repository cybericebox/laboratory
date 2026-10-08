package v1alpha1

import "testing"

func TestAbsentLifecycleRunsAndStoppedIntentDoesNot(t *testing.T) {
	var old *LabLifecycleSpec
	if old.IsStopped() {
		t.Fatal("legacy Lab unexpectedly stopped")
	}
	next := &LabLifecycleSpec{DesiredState: "Stopped", OperationID: "op", Revision: 1, SnapshotMode: "Skip"}
	if !next.IsStopped() {
		t.Fatal("stop intent ignored")
	}
	next.DesiredState = "Running"
	if next.IsStopped() {
		t.Fatal("running intent marked stopped")
	}
}
