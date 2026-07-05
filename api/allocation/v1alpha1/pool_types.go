package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PoolSpec defines the immutable configuration of a Pool.
type PoolSpec struct {
	// Size is the total number of slots in this pool.
	// +kubebuilder:validation:Minimum=1
	Size uint `json:"size"`
	// Offset shifts allocated indices: real value = Offset + bit_index.
	// +kubebuilder:default=0
	Offset uint `json:"offset,omitempty"`
}

// PoolStatus holds the mutable allocation state.
type PoolStatus struct {
	// Free is the count of unallocated slots; mirrored to label pool.cybericebox.com/free.
	Free uint `json:"free,omitempty"`
	// BitMap is a base64-encoded bitset (one bit per slot, 1 = allocated).
	BitMap string `json:"bitMap,omitempty"`
}

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Pool is the Schema for the pools API.
type Pool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	
	Spec   PoolSpec   `json:"spec,omitempty"`
	Status PoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PoolList contains a list of Pool.
type PoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Pool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Pool{}, &PoolList{})
}
