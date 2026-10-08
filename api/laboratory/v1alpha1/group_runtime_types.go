package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// OwnedRuntimeIdentity is captured before stop. An absent API Pod cannot create
// this inventory. NodeBootID is the kernel boot, not the node-agent process.
type OwnedRuntimeIdentity struct {
	OwnerUID      string          `json:"ownerUID"`
	OperationID   string          `json:"operationId"`
	Revision      int64           `json:"revision"`
	DeploymentUID string          `json:"deploymentUID,omitempty"`
	PodUID        string          `json:"podUID"`
	NodeName      string          `json:"nodeName"`
	NodeBootID    string          `json:"nodeBootID"`
	ContainerIDs  []string        `json:"containerIDs"`
	CgroupPaths   []string        `json:"cgroupPaths"`
	PortKeys      []string        `json:"portKeys"`
	Epoch         int32           `json:"epoch,omitempty"`
	Incarnation   int32           `json:"incarnation,omitempty"`
	Component     string          `json:"component,omitempty"`
	Requests      ResourceAmounts `json:"requests"`
	Limits        ResourceAmounts `json:"limits"`
}

// OwnedRuntimeReport is node-owned; identity is immutable controller inventory.
// Independent timestamps never impose a cross-owner clock ordering.
type OwnedRuntimeReport struct {
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
