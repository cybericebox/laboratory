package devicestate

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAppArmorBlocksSnapshots(t *testing.T) {
	for label, want := range map[string]bool{
		"cri-containerd.apparmor.d (enforce)":     true,
		"cri-containerd.apparmor.d (enforce)\n":   true,
		"cri-containerd.apparmor.d (enforce)\x00": true,
		"cri-containerd.apparmor.d (complain)":    false,
		"unconfined":                              false,
		"":                                        false,
		"localhost-snapshots (enforce)":           false,
		"system_u:system_r:container_t:s0":        false,
	} {
		if got := AppArmorBlocksSnapshots(label); got != want {
			t.Errorf("AppArmorBlocksSnapshots(%q) = %v, want %v", label, got, want)
		}
	}
}

func TestEngineBlockedSnapshotsWarnAndNeverDiff(t *testing.T) {
	const msg = "AppArmor blocks snapshots: test"
	r := newRig(t, time.Hour, 1<<20)
	r.e.SnapshotsBlocked = msg
	tr := r.track(t)
	defer r.e.stopAll()

	// The device shows the warning as soon as it is followed, before any write.
	if _, warns, _ := r.cl.snapshot(); len(warns) != 1 || warns[0] != msg {
		t.Fatalf("warns %v", warns)
	}
	r.rt.setDiff(tarOf(map[string]string{"srv/state": "x"}))
	if err := tr.snapshot(context.Background(), true); !errors.Is(err, ErrSnapshotsBlocked) {
		t.Fatalf("live snapshot: %v", err)
	}
	r.e.finish(context.Background(), tr)

	recs, warns, exits := r.cl.snapshot()
	if len(recs) != 0 || len(exits) != 1 {
		t.Fatalf("records %v exits %v", recs, exits)
	}
	if len(warns) != 1 {
		t.Fatalf("the warning is reported once: %v", warns)
	}
	if r.rt.diffs != 0 {
		t.Fatalf("a blocked node must not diff (or freeze) the container: %d diffs", r.rt.diffs)
	}
}

func TestRequiredCaptureBlockedFailsWithoutQuiescing(t *testing.T) {
	r := newRig(t, time.Hour, 1<<20)
	r.e.SnapshotsBlocked = "AppArmor blocks snapshots: test"
	req := captureRequest(r)
	r.rt.setDiff(tarOf(map[string]string{"work": "changed"}))
	result, err := r.e.CaptureRequired(context.Background(), r.pod, req)
	if !errors.Is(err, ErrSnapshotsBlocked) || result.Result != "Failed" || result.Error != r.e.SnapshotsBlocked || result.Quiesced {
		t.Fatalf("blocked capture: %+v %v", result, err)
	}
	if r.rt.held || r.rt.diffs != 0 {
		t.Fatalf("a blocked capture must not freeze or diff: held %v diffs %d", r.rt.held, r.rt.diffs)
	}
}
