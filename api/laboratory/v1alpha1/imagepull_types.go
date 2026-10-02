package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ImagePullSpec asks the node-agents to pull images onto their nodes through the container
// runtime (CRI), so the images are there before the first lab pod starts. The scheduler
// creates one per launch class and removes it when the class has been prepared. No tenant
// code runs: only the runtime's image service is called.
type ImagePullSpec struct {
	// Images are the references to pull, as the nodes pull them (through the image cache
	// when the lab uses it).
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=512
	Images []string `json:"images"`
	// Nodes are the nodes asked to pull: the ones lab pods can run on.
	// +kubebuilder:validation:MaxItems=1000
	Nodes []string `json:"nodes"`
	// Tenant is the tenant the images belong to (informational).
	// +optional
	Tenant string `json:"tenant,omitempty"`
	// PullSecret names a kubernetes.io/dockerconfigjson Secret in the images namespace
	// (laboratory-images) with the tenant's registry credentials, created by the operator
	// for this ImagePull and owned by it. Absent: anonymous pulls.
	// +optional
	PullSecret string `json:"pullSecret,omitempty"`
}

// NodeImagePull is how far one node is.
type NodeImagePull struct {
	// Pulled lists the images the node holds.
	// +optional
	Pulled []string `json:"pulled,omitempty"`
	// Failed lists the images the node could not pull, with the runtime's message.
	// +optional
	Failed []ImagePullFailure `json:"failed,omitempty"`
	// Done is true once every image is either pulled or failed.
	Done bool `json:"done"`
	// UpdatedAt is when the node-agent last wrote this entry.
	// +optional
	UpdatedAt *metav1.Time `json:"updatedAt,omitempty"`
}

// ImagePullFailure is an image a node could not pull.
type ImagePullFailure struct {
	Image string `json:"image"`
	// +optional
	Message string `json:"message,omitempty"`
}

// ImagePullStatus is the progress per node, each node-agent writing its own entry.
type ImagePullStatus struct {
	// +optional
	Nodes map[string]NodeImagePull `json:"nodes,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// ImagePull is a request to pull images onto nodes, answered by the node-agents.
type ImagePull struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ImagePullSpec   `json:"spec,omitempty"`
	Status ImagePullStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ImagePullList contains a list of ImagePull.
type ImagePullList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ImagePull `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ImagePull{}, &ImagePullList{})
}
