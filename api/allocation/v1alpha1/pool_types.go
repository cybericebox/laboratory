/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PoolSpec defines the desired state of Pool.
type PoolSpec struct {
	// Size is the desired size of the pool.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	Size int32 `json:"size,omitempty"`
	// Offset is the offset for the pool.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	Offset int32 `json:"offset,omitempty"`
}

// PoolStatus defines the observed state of Pool.
type PoolStatus struct {
	// Available is the number of available resources in the pool.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	Available int32 `json:"available,omitempty"`
	// Allocated is the number of allocated resources in the pool.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	Allocated int32 `json:"allocated,omitempty"`
	// BitMap is a string representation of the pool's bitmap. Base64 encoded string of a byte array
	// where each bit represents the availability of a resource in the pool.
	BitMap string `json:"bitMap,omitempty"`
}

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

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
