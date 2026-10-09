package v1alpha1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LifecycleRetirementIntent is stored by metadata CAS so acceptance preserves
// the original stopped spec/generation/native certificate. Tombstones persist.
type LifecycleRetirementIntent struct {
	NamespaceUID    string      `json:"namespaceUID,omitempty"`
	ExpectedUID     string      `json:"expectedUID"`
	StopOperationID string      `json:"stopOperationId"`
	StopRevision    int64       `json:"stopRevision"`
	Generation      int64       `json:"generation"`
	OperationID     string      `json:"operationId"`
	Revision        int64       `json:"revision"`
	RequestedAt     metav1.Time `json:"requestedAt"`
}

// RetirementObjectIdentity is an exact pre-cleanup object receipt, never data.
type RetirementObjectIdentity struct {
	GroupNamespaceOwned bool   `json:"groupNamespaceOwned,omitempty"`
	Kind                string `json:"kind"`
	Name                string `json:"name"`
	UID                 string `json:"uid"`
	OwnerUID            string `json:"ownerUID"`
}

type LifecycleRetirementStatus struct {
	Objects                       []RetirementObjectIdentity `json:"objects,omitempty"`
	ExpectedUID                   string                     `json:"expectedUID"`
	StopOperationID               string                     `json:"stopOperationId"`
	StopRevision                  int64                      `json:"stopRevision"`
	OperationID                   string                     `json:"operationId"`
	Revision                      int64                      `json:"revision"`
	ObservedGeneration            int64                      `json:"observedGeneration"`
	State                         string                     `json:"state"`
	ObservedAt                    *metav1.Time               `json:"observedAt,omitempty"`
	RequestedAt                   *metav1.Time               `json:"requestedAt,omitempty"`
	RuntimeAbsent                 bool                       `json:"runtimeAbsent"`
	StorageState                  string                     `json:"storageState"`
	CleanupComplete               bool                       `json:"cleanupComplete"`
	PhysicalStorageBytesAvailable bool                       `json:"physicalStorageBytesAvailable"`
	PhysicalStorageBytes          int64                      `json:"physicalStorageBytes"`
	Error                         string                     `json:"error,omitempty"`
}

func ParseLifecycleRetirement(raw string) (LifecycleRetirementIntent, bool) {
	var in LifecycleRetirementIntent
	err := json.Unmarshal([]byte(raw), &in)
	return in, err == nil && in.ExpectedUID != "" && in.StopOperationID != "" && in.StopRevision > 0 && in.Generation > 0 && in.OperationID != "" && in.Revision > in.StopRevision && in.OperationID != in.StopOperationID && !in.RequestedAt.IsZero()
}
