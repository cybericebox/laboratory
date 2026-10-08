package flowacct

import "testing"

func TestPairCounterDeltaPreservesDirectionAndEpoch(t *testing.T) {
	previous := PairCounters{Key: Key{Subject: "p1", Lab: "a"}, BindingID: "binding", Epoch: "e1",
		PacketsOut: 100, PacketsIn: 50, BytesOut: 1000, BytesIn: 500, Attempts: 2, LabInitiatedAttempts: 1}
	current := previous
	current.PacketsOut = 300
	current.PacketsIn = 200
	current.BytesOut = 3000
	current.BytesIn = 2000
	current.Attempts = 3
	current.LabInitiatedAttempts = 4
	d, partial, err := PairCounterDelta(current, previous)
	if err != nil || partial || d.PacketsOut != 200 || d.PacketsIn != 150 || d.BytesOut != 2000 || d.BytesIn != 1500 || d.Attempts != 1 || d.LabInitiatedAttempts != 3 {
		t.Fatalf("wrong directional deltas: %+v, partial=%v, err=%v", d, partial, err)
	}
	d, partial, err = PairCounterDelta(current, current)
	if err != nil || partial || d.PacketsOut != 0 || d.PacketsIn != 0 || d.Attempts != 0 || d.LabInitiatedAttempts != 0 {
		t.Fatalf("same reading counted twice: %+v %v %v", d, partial, err)
	}
	current.Epoch = "e2"
	current.PacketsOut = 1
	d, partial, err = PairCounterDelta(current, previous)
	if err != nil || !partial || d.PacketsOut != 1 {
		t.Fatalf("reset epoch was hidden: %+v %v %v", d, partial, err)
	}
}

func TestPairCounterDeltaRejectsReassignedOwner(t *testing.T) {
	old := PairCounters{Key: Key{Subject: "p1", Lab: "a"}, BindingID: "binding1", Epoch: "e1", PacketsOut: 50}
	next := old
	next.Subject = "p2"
	next.BindingID = "binding2"
	if _, _, err := PairCounterDelta(next, old); err == nil {
		t.Fatal("old packets could be reassigned to a new client")
	}
	next = old
	next.PacketsOut = 2
	d, partial, err := PairCounterDelta(next, old)
	if err != nil || !partial || d.PacketsOut != 2 {
		t.Fatalf("regressed raw counter looked like normal traffic: %+v %v %v", d, partial, err)
	}
}
