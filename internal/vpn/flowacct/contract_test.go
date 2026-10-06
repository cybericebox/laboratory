package flowacct

import (
	"encoding/json"
	"testing"

	lab "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

func TestTrafficStatusRetainsLabInitiationAndPartialCoverage(t *testing.T) {
	report := Report{BootID: "boot", Partial: true, Ledger: []Touch{{Key: Key{Subject: "p1", Lab: "a"}, Attempts: 2, LabInitiatedAttempts: 3, PacketsOut: 300, PacketsIn: 200, BytesOut: 3000, BytesIn: 2000}}}
	status := ToStatus(report)
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip lab.LabTrafficReportStatus
	if err := json.Unmarshal(encoded, &roundtrip); err != nil {
		t.Fatal(err)
	}
	if !roundtrip.Partial || len(roundtrip.Ledger) != 1 {
		t.Fatalf("coverage/row lost: %+v", roundtrip)
	}
	r := roundtrip.Ledger[0]
	if r.Attempts != 2 || r.LabInitiatedAttempts != 3 || r.PacketsOut != 300 || r.PacketsIn != 200 || r.BytesOut != 3000 || r.BytesIn != 2000 {
		t.Fatalf("traffic directions or initiatives lost: %+v", r)
	}
	var old lab.LabTrafficTouch
	if err := json.Unmarshal([]byte(`{"subject":"p1","labName":"a","attempts":2,"firstSeenMs":1,"lastSeenMs":2}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Attempts != 2 || old.LabInitiatedAttempts != 0 {
		t.Fatal("old report compatibility changed")
	}
}

func TestPrivateCheckpointPreservesUnsignedKernelValue(t *testing.T) {
	status := ToStatus(Report{KernelCheckpoints: []PairCounters{{Key: Key{Subject: "p1", Lab: "a"}, BindingID: "binding", Epoch: "epoch", PacketsOut: ^uint64(0)}}})
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var decoded lab.LabTrafficReportStatus
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.KernelCheckpoints) != 1 || decoded.KernelCheckpoints[0].PacketsOut != "18446744073709551615" {
		t.Fatalf("unsigned checkpoint lost precision: %+v", decoded.KernelCheckpoints)
	}
}
