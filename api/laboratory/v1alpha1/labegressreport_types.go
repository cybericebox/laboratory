package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// LabEgressReportName is the name of the one LabEgressReport of a group namespace.
const LabEgressReportName = "gateway"

// LabEgressReportSpec is written by the operator only. It carries the acknowledgement that lets the gateway forget what has been read.
type LabEgressReportSpec struct {
	// AckedThrough is the start of the newest minute the operator has logged: every minute up to and including it is in the connection
	// log, and the gateway removes it from the status. The operator moves it forward only.
	// +optional
	AckedThrough *metav1.Time `json:"ackedThrough,omitempty"`
}

// LabEgressRow is one lab's traffic to one host name in one minute (no address of a person, no content).
type LabEgressRow struct {
	// +kubebuilder:validation:MaxLength=253
	Lab string `json:"lab"`
	// +kubebuilder:validation:MaxLength=253
	Host   string `json:"host"`
	Conns  int64  `json:"conns"`
	Up     int64  `json:"up"`
	Down   int64  `json:"down"`
	Denied int64  `json:"denied,omitempty"`
}

// LabEgressCap counts how often a lab sat at one of its caps in a minute.
type LabEgressCap struct {
	// +kubebuilder:validation:MaxLength=253
	Lab string `json:"lab"`
	// +kubebuilder:validation:MaxLength=64
	Reason string `json:"reason"`
	Hits   int64  `json:"hits"`
}

// LabEgressDenial counts the refused connections of a lab by reason in a minute.
type LabEgressDenial struct {
	// +kubebuilder:validation:MaxLength=253
	Lab string `json:"lab"`
	// +kubebuilder:validation:MaxLength=64
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
}

// LabEgressFlow is one lab's traffic from one source address to one destination address and port in one minute. It exists only when
// activity reports are on.
type LabEgressFlow struct {
	// +kubebuilder:validation:MaxLength=253
	Lab string `json:"lab"`
	// +kubebuilder:validation:MaxLength=45
	Source string `json:"src"`
	// +kubebuilder:validation:MaxLength=45
	Dest    string `json:"dst"`
	Port    int32  `json:"port"`
	Conns   int64  `json:"conns"`
	Up      int64  `json:"up"`
	Down    int64  `json:"down"`
	FirstMs int64  `json:"firstMs"`
	LastMs  int64  `json:"lastMs"`
}

// LabEgressMinute is one finished minute of the gateway. The limits are the same as the ones the writer keeps (internal/egressproxy
// MaxReport*); a test compares them.
type LabEgressMinute struct {
	Start metav1.Time `json:"start"`
	// +kubebuilder:validation:MaxItems=500
	// +optional
	Rows []LabEgressRow `json:"rows,omitempty"`
	// +kubebuilder:validation:MaxItems=500
	// +optional
	Caps []LabEgressCap `json:"caps,omitempty"`
	// +kubebuilder:validation:MaxItems=500
	// +optional
	Denials []LabEgressDenial `json:"denials,omitempty"`
	// +kubebuilder:validation:MaxItems=200
	// +optional
	Flows []LabEgressFlow `json:"flows,omitempty"`
}

// LabEgressReportStatus is written by the gateway pod only.
type LabEgressReportStatus struct {
	// Instance is the gateway pod that wrote the report last.
	// +optional
	Instance string `json:"instance,omitempty"`
	// BootID identifies the run of that pod.
	// +optional
	BootID string `json:"bootID,omitempty"`
	// Minutes are the minutes that the operator has not acknowledged, oldest first. A restarted gateway keeps the ones of its
	// predecessor.
	// +kubebuilder:validation:MaxItems=30
	// +optional
	Minutes []LabEgressMinute `json:"minutes,omitempty"`
	// DroppedMinutes counts the minutes the writer dropped because a bound was reached before the operator took them.
	// +optional
	DroppedMinutes int64 `json:"droppedMinutes,omitempty"`
	// UpdatedAt is the time of the last write.
	// +optional
	UpdatedAt *metav1.Time `json:"updatedAt,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=ler
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'gateway'",message="the report of a group is named gateway"

// LabEgressReport is the hand-off of the per-minute egress summaries from the gateway pod of a group to the operator: the gateway writes
// the finished minutes to the status, the operator logs them and writes spec.ackedThrough, and the gateway then removes what is acknowledged.
// There is one per group namespace, named "gateway". It replaces the page the operator used to pull from the gateway pod.
type LabEgressReport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              LabEgressReportSpec   `json:"spec,omitempty"`
	Status            LabEgressReportStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type LabEgressReportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LabEgressReport `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LabEgressReport{}, &LabEgressReportList{})
}
