package v1alpha1

import "encoding/json"

// SnapshotRetirementFence records manifest deletion separately from runtime
// stop and from physical blob GC. Metadata CAS serializes it against new starts.
type SnapshotRetirementFence struct {
	LabUID      string `json:"labUID"`
	OperationID string `json:"operationId"`
	Revision    int64  `json:"revision"`
	State       string `json:"state"`
}

func ParseSnapshotRetirement(raw string) (SnapshotRetirementFence, bool) {
	var f SnapshotRetirementFence
	err := json.Unmarshal([]byte(raw), &f)
	return f, err == nil && f.LabUID != "" && f.OperationID != "" && f.Revision > 0 && (f.State == "DeleteRequested" || f.State == "CleanupPending" || f.State == "Deleted")
}
