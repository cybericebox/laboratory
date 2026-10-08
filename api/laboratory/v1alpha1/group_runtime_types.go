package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"reflect"
)

// OwnedRuntimeIdentity is captured before stop. An absent API Pod cannot create
// this inventory. NodeBootID is the kernel boot, not the node-agent process.
type OwnedRuntimeIdentity struct {
	PortRows    []OwnedFabricPort `json:"portRows,omitempty"`
	VNIBindings []OwnedVNI        `json:"vniBindings,omitempty"`
	FabricPorts []OwnedFabricPort `json:"fabricPorts,omitempty"`
	// ScopeKind is Pod, LabFabric or NeverMaterialized. Non-Pod scopes have
	// actual object ScopeUID, never synthetic Pod/container/cgroup identities.
	ScopeKind           string          `json:"scopeKind,omitempty"`
	ScopeUID            string          `json:"scopeUID,omitempty"`
	Namespace           string          `json:"namespace,omitempty"`
	LabName             string          `json:"labName,omitempty"`
	Generation          int64           `json:"generation,omitempty"`
	AttachmentsComplete bool            `json:"attachmentsComplete,omitempty"`
	VNIs                []uint          `json:"vnis,omitempty"`
	OwnerUID            string          `json:"ownerUID"`
	OperationID         string          `json:"operationId"`
	Revision            int64           `json:"revision"`
	DeploymentUID       string          `json:"deploymentUID,omitempty"`
	PodUID              string          `json:"podUID"`
	NodeName            string          `json:"nodeName"`
	NodeBootID          string          `json:"nodeBootID"`
	ContainerIDs        []string        `json:"containerIDs"`
	CgroupPaths         []string        `json:"cgroupPaths"`
	PortKeys            []string        `json:"portKeys"`
	Epoch               int32           `json:"epoch,omitempty"`
	Incarnation         int32           `json:"incarnation,omitempty"`
	Component           string          `json:"component,omitempty"`
	Requests            ResourceAmounts `json:"requests"`
	Limits              ResourceAmounts `json:"limits"`
}

// OwnedRuntimeReport is node-owned; identity is immutable controller inventory.
// Independent timestamps never impose a cross-owner clock ordering.
type OwnedRuntimeReport struct {
	// ReleasedVNIs are exact object/node-owned correlated-barrier receipts.
	// They retire a lease independently of live process allocation.
	ReleasedVNIs []OwnedVNI `json:"releasedVNIs,omitempty"`
	// Echoed only after a new native sample for this permanent retirement intent.
	RetirementOperationID string               `json:"retirementOperationId,omitempty"`
	RetirementRevision    int64                `json:"retirementRevision,omitempty"`
	Identity              OwnedRuntimeIdentity `json:"identity"`
	RuntimeState          string               `json:"runtimeState"`
	RuntimeAbsentAt       *metav1.Time         `json:"runtimeAbsentAt,omitempty"`
	CgroupAbsentAt        *metav1.Time         `json:"cgroupAbsentAt,omitempty"`
	AttachmentsAbsentAt   *metav1.Time         `json:"attachmentsAbsentAt,omitempty"`
	ObservedAt            *metav1.Time         `json:"observedAt,omitempty"`
	Error                 string               `json:"error,omitempty"`
}

type OwnedFabricPort struct {
	Key      string `json:"key"`
	OwnerUID string `json:"ownerUID"`
	RowUUID  string `json:"rowUUID"`
}

// OwnedVNI binds a numeric flow key to the actual allocation-holding object.
type VNILease struct {
	PoolUID    string `json:"poolUID"`
	Generation int64  `json:"generation"`
}
type OwnedVNI struct {
	PoolUID         string `json:"poolUID"`
	LeaseGeneration int64  `json:"leaseGeneration"`
	OwnerUID        string `json:"ownerUID"`
	OperationID     string `json:"operationID"`
	Revision        int64  `json:"revision"`
	Generation      int64  `json:"generation"`
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace"`
	Name            string `json:"name"`
	UID             string `json:"uid"`
	VNI             uint   `json:"vni"`
}

// VNILeaseReleased consumes every actual placement-node obligation for one
// exact allocation epoch. It never treats API absence as a physical ACK.
func VNILeaseReleased(l *Lab, binding OwnedVNI) bool {
	found := false
	for _, scope := range l.Status.ScopeInventory {
		member := false
		for _, v := range scope.VNIBindings {
			member = member || reflect.DeepEqual(v, binding)
		}
		if !member {
			continue
		}
		found = true
		done := false
		for _, report := range l.Status.ScopeReports {
			id := report.Identity
			if id.OwnerUID != scope.OwnerUID || id.ScopeUID != scope.ScopeUID || id.NodeName != scope.NodeName || id.NodeBootID != scope.NodeBootID || id.OperationID != scope.OperationID || id.Revision != scope.Revision || id.Generation != scope.Generation || report.ObservedAt == nil || report.ObservedAt.IsZero() {
				continue
			}
			for _, v := range report.ReleasedVNIs {
				done = done || reflect.DeepEqual(v, binding)
			}
		}
		if !done {
			return false
		}
	}
	return found
}
