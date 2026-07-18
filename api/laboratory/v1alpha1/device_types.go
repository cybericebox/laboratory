package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DeviceSpec defines the desired state of Device.
type DeviceSpec struct {
	// LabRef is the name of the parent Lab.
	// +kubebuilder:validation:Required
	LabRef string `json:"labRef"`
	// Name is the logical device name within the lab (matches DeviceTemplate.name).
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// +kubebuilder:validation:Required
	Type  DeviceType `json:"type"`
	Image string     `json:"image,omitempty"`
	// SecurityPreset names a capability profile (basic/service/net/debug); the
	// concrete capabilities are resolved internally by the operator.
	SecurityPreset SecurityPreset  `json:"securityPreset,omitempty"`
	Interfaces     []InterfaceSpec `json:"interfaces,omitempty"`
	Exposure       *ExposureSpec   `json:"exposure,omitempty"`
	// Resources sets the container resource requests/limits for this device.
	// +optional
	Resources *DeviceResources `json:"resources,omitempty"`
}

// DeviceResources sets container resource requests/limits for a device pod.
// Values are Kubernetes quantity strings (e.g. "250m", "256Mi"); empty fields
// are omitted so the scheduler treats them as best-effort. Requests are what
// capacity planning sums against node allocatable.
type DeviceResources struct {
	CPURequest    string `json:"cpuRequest,omitempty"`
	MemoryRequest string `json:"memoryRequest,omitempty"`
	CPULimit      string `json:"cpuLimit,omitempty"`
	MemoryLimit   string `json:"memoryLimit,omitempty"`
}

// DeviceStatus defines the observed state of Device.
type DeviceStatus struct {
	Ready    bool   `json:"ready,omitempty"`
	NodeName string `json:"nodeName,omitempty"`
	// NodeAddress is the node IP used as Geneve VTEP address.
	NodeAddress string `json:"nodeAddress,omitempty"`
	PodIP       string `json:"podIP,omitempty"`
	// VNI is set only for unmanaged-switch and hub device types.
	VNI    *uint  `json:"vni,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Device is the Schema for the devices API.
type Device struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	
	Spec   DeviceSpec   `json:"spec,omitempty"`
	Status DeviceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DeviceList contains a list of Device.
type DeviceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Device `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Device{}, &DeviceList{})
}
