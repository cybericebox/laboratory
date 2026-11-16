package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ActivityKind names the fact an ActivityEvent carries.
// +kubebuilder:validation:Enum=egress;console;touch
type ActivityKind string

const (
	// ActivityEgress: the internet egress of a lab in one minute: from a source address to a destination address and port.
	ActivityEgress ActivityKind = "egress"
	// ActivityConsole: the opening or the closing of a console session of one client on one device.
	ActivityConsole ActivityKind = "console"
	// ActivityTouch: the bytes one VPN client exchanged with one lab address in one minute.
	ActivityTouch ActivityKind = "touch"
)

// ActivityEvent is one raw fact of the ring of a LabActivityReport. Seq grows by one per event of a writer; a writer never reuses it. The
// fields that do not belong to the Kind are empty. Times are Unix milliseconds. Egress and touch events are deltas of the minute that ends at
// AtMs, so a reader sums them.
type ActivityEvent struct {
	Seq  int64        `json:"seq"`
	AtMs int64        `json:"atMs"`
	Kind ActivityKind `json:"kind"`
	// LabName is the lab the fact belongs to.
	LabName string `json:"labName,omitempty"`
	// Client is the LabGroupClient (touch) or the client of the console session.
	Client string `json:"client,omitempty"`
	// Device is the device of a console session.
	Device string `json:"device,omitempty"`
	// SessionID identifies a console session; the open and the close event of one session carry the same value.
	SessionID   string `json:"sessionID,omitempty"`
	OpenedAtMs  int64  `json:"openedAtMs,omitempty"`
	ClosedAtMs  int64  `json:"closedAtMs,omitempty"`
	SourceAddr  string `json:"sourceAddress,omitempty"`
	DstAddr     string `json:"dstAddress,omitempty"`
	DstPort     int32  `json:"dstPort,omitempty"`
	Protocol    string `json:"protocol,omitempty"`
	BytesUp     int64  `json:"bytesUp,omitempty"`
	BytesDown   int64  `json:"bytesDown,omitempty"`
	Connections int64  `json:"connections,omitempty"`
	FirstSeenMs int64  `json:"firstSeenMs,omitempty"`
	LastSeenMs  int64  `json:"lastSeenMs,omitempty"`
}

// LabActivityReportSpec names the writer of a report; nothing here configures behaviour.
type LabActivityReportSpec struct {
	// Writer is the writing process: "vpn", "egress", or "proxy-<pod>".
	// +optional
	Writer string `json:"writer,omitempty"`
}

// LabActivityReportStatus is the ring of one writer. It keeps at most 1000 events and one hour of them.
type LabActivityReportStatus struct {
	// DroppedThrough is the highest Seq the writer dropped to keep the ring within its bounds (zero: none). A reader whose cursor is below it
	// has missed events.
	// +optional
	DroppedThrough int64 `json:"droppedThrough,omitempty"`
	// UpdatedMs is the last write of the writer.
	// +optional
	UpdatedMs int64 `json:"updatedMs,omitempty"`
	// +optional
	Events []ActivityEvent `json:"events,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=lar
// +kubebuilder:subresource:status

// LabActivityReport is a bounded ring of raw activity facts one writer (the VPN pod, a proxy replica, the operator for the internet egress)
// keeps in the group namespace until the agent hands them to the platform. It exists only when activity reports are enabled. The facts are
// handed only to your own platform through the agent API; the laboratory sends nothing anywhere by itself.
type LabActivityReport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              LabActivityReportSpec   `json:"spec,omitempty"`
	Status            LabActivityReportStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type LabActivityReportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LabActivityReport `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LabActivityReport{}, &LabActivityReportList{})
}
