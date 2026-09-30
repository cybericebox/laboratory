package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// LabTrafficSurface names the collector that produced a report.
// +kubebuilder:validation:Enum=vpn;proxy
type LabTrafficSurface string

const (
	LabTrafficSurfaceVPN   LabTrafficSurface = "vpn"
	LabTrafficSurfaceProxy LabTrafficSurface = "proxy"
)

// LabTrafficReportSpec identifies the writer of a report. The report carries
// facts only; nothing here configures behaviour.
type LabTrafficReportSpec struct {
	Kind LabTrafficSurface `json:"kind"`
	// Instance is the writing pod (the VPN pod, or one proxy replica).
	// +optional
	Instance string `json:"instance,omitempty"`
}

// LabTrafficTouch is one cumulative aggregate row: what one VPN config (or one
// proxy token) did against one lab target since the collector booted. There is
// no time series. Times are Unix milliseconds. Only lab-internal addresses are
// ever recorded, never the address of a user.
type LabTrafficTouch struct {
	// Subject is the LabGroupClient name (VPN) or the token subject, a user id
	// (proxy). An empty subject is a legacy token.
	// +optional
	Subject string `json:"subject,omitempty"`
	LabName string `json:"labName"`
	// Device is the device or route inside the lab, when known.
	// +optional
	Device string `json:"device,omitempty"`
	// +optional
	DstIP string `json:"dstIP,omitempty"`
	// +optional
	Proto string `json:"proto,omitempty"`
	// +optional
	DstPort int32 `json:"dstPort,omitempty"`
	// Attempts counts new connections (VPN) or requests (proxy).
	Attempts int64 `json:"attempts"`
	// +optional
	PacketsOut int64 `json:"packetsOut,omitempty"`
	// +optional
	PacketsIn int64 `json:"packetsIn,omitempty"`
	// +optional
	BytesOut int64 `json:"bytesOut,omitempty"`
	// +optional
	BytesIn int64 `json:"bytesIn,omitempty"`
	// FirstSeenMs is the first attempt, LastSeenMs the last one.
	FirstSeenMs int64 `json:"firstSeenMs"`
	LastSeenMs  int64 `json:"lastSeenMs"`
	// FirstRespondedMs is when the lab first answered; zero means it never did.
	// +optional
	FirstRespondedMs int64 `json:"firstRespondedMs,omitempty"`
}

// LabTrafficReportStatus is written only by the collector.
type LabTrafficReportStatus struct {
	// BootID changes whenever the collector restarts; the ledger is cumulative
	// within one boot.
	BootID string `json:"bootID,omitempty"`
	// CoveredFromMs..CoveredToMs is the span the collector actually observed.
	// CoveredToMs advances as a heartbeat even when nothing happened.
	CoveredFromMs int64 `json:"coveredFromMs,omitempty"`
	CoveredToMs   int64 `json:"coveredToMs,omitempty"`
	// Partial is set when part of the span could not be read.
	Partial bool `json:"partial,omitempty"`
	// Truncated is set when the ledger hit its size cap and rows were dropped.
	Truncated bool              `json:"truncated,omitempty"`
	Ledger    []LabTrafficTouch `json:"ledger,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=ltr
// +kubebuilder:subresource:status

// LabTrafficReport is the namespaced hand-off between a traffic collector (the
// VPN pod, a proxy replica) and the agent, which relays it to the platform.
// The custom resource is the buffer: the agent keeps no traffic state.
type LabTrafficReport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              LabTrafficReportSpec   `json:"spec,omitempty"`
	Status            LabTrafficReportStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type LabTrafficReportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LabTrafficReport `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LabTrafficReport{}, &LabTrafficReportList{})
}
