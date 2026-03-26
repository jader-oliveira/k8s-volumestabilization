/*
Copyright 2024.

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

package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VolumeStabilizationPhase represents the phase of volume stabilization
type VolumeStabilizationPhase string

const (
	// VolumeStabilizationPhasePending means the stabilization is pending
	VolumeStabilizationPhasePending VolumeStabilizationPhase = "Pending"
	// VolumeStabilizationPhaseDiscovering means looking for the source PVC
	VolumeStabilizationPhaseDiscovering VolumeStabilizationPhase = "Discovering"
	// VolumeStabilizationPhaseCapturing means capturing PV information
	VolumeStabilizationPhaseCapturing VolumeStabilizationPhase = "Capturing"
	// VolumeStabilizationPhaseDeletingOriginal means deleting the original PVC/PV
	VolumeStabilizationPhaseDeletingOriginal VolumeStabilizationPhase = "DeletingOriginal"
	// VolumeStabilizationPhaseCreating means creating the new PV/PVC
	VolumeStabilizationPhaseCreating VolumeStabilizationPhase = "Creating"
	// VolumeStabilizationPhaseBound means the new PV/PVC are bound
	VolumeStabilizationPhaseBound VolumeStabilizationPhase = "Bound"
	// VolumeStabilizationPhaseFailed means the stabilization failed
	VolumeStabilizationPhaseFailed VolumeStabilizationPhase = "Failed"
)

// SourcePVCReference references the dynamically provisioned PVC to stabilize
type SourcePVCReference struct {
	// Name of the source PVC (dynamically created by CSI)
	Name string `json:"name"`
	// Namespace of the source PVC (defaults to CR namespace)
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// TargetVolumeSpec defines the target configuration for the stabilized volume
type TargetVolumeSpec struct {
	// PVCName is the desired name for the stabilized PVC
	PVCName string `json:"pvcName"`
	// Namespace for the stabilized PVC (defaults to source namespace)
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// PVName is an optional specific name for the PV (auto-generated if not specified)
	// +optional
	PVName string `json:"pvName,omitempty"`
	// StorageClassName for the new PVC (defaults to empty for static binding)
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// VolumeStabilizationSpec defines the desired state of VolumeStabilization
type VolumeStabilizationSpec struct {
	// SourcePVC is the reference to the dynamically provisioned PVC to stabilize
	SourcePVC SourcePVCReference `json:"sourcePVC"`

	// Target is the target configuration for the stabilized volume
	Target TargetVolumeSpec `json:"target"`

	// PreserveAnnotations preserves annotations from the original PVC
	// +optional
	// +kubebuilder:default=true
	PreserveAnnotations bool `json:"preserveAnnotations,omitempty"`

	// PreserveLabels preserves labels from the original PVC
	// +optional
	// +kubebuilder:default=true
	PreserveLabels bool `json:"preserveLabels,omitempty"`

	// WaitForDeletion waits for original PVC/PV to be fully deleted before creating new ones
	// +optional
	// +kubebuilder:default=true
	WaitForDeletion bool `json:"waitForDeletion,omitempty"`
}

// OriginalPVInfo stores information about the original PV
type OriginalPVInfo struct {
	// Name of the original PV
	Name string `json:"name,omitempty"`
	// Server is the NFS server address
	Server string `json:"server,omitempty"`
	// Path is the NFS export path
	Path string `json:"path,omitempty"`
	// Storage is the volume capacity
	Storage string `json:"storage,omitempty"`
	// AccessModes of the volume
	AccessModes []string `json:"accessModes,omitempty"`
	// StorageClassName of the original PV
	StorageClassName string `json:"storageClassName,omitempty"`
	// ReclaimPolicy of the original PV
	ReclaimPolicy string `json:"reclaimPolicy,omitempty"`
	// MountOptions for NFS
	MountOptions []string `json:"mountOptions,omitempty"`
}

// NewPVInfo stores information about the new PV
type NewPVInfo struct {
	// Name of the new PV
	Name string `json:"name,omitempty"`
}

// NewPVCInfo stores information about the new PVC
type NewPVCInfo struct {
	// Name of the new PVC
	Name string `json:"name,omitempty"`
	// Namespace of the new PVC
	Namespace string `json:"namespace,omitempty"`
}

// VolumeStabilizationCondition defines a condition of the volume stabilization
type VolumeStabilizationCondition struct {
	// Type of the condition
	Type string `json:"type"`

	// Status of the condition
	Status corev1.ConditionStatus `json:"status"`

	// LastTransitionTime is the last time the condition transitioned
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`

	// Reason is a brief reason for the condition's last transition
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message is a human-readable message about the condition
	// +optional
	Message string `json:"message,omitempty"`
}

// VolumeStabilizationStatus defines the observed state of VolumeStabilization
type VolumeStabilizationStatus struct {
	// Phase is the current phase of the stabilization process
	// +optional
	// +kubebuilder:default="Pending"
	Phase VolumeStabilizationPhase `json:"phase,omitempty"`

	// OriginalPV stores information about the original PV
	// +optional
	OriginalPV *OriginalPVInfo `json:"originalPV,omitempty"`

	// NewPV stores information about the new stabilized PV
	// +optional
	NewPV *NewPVInfo `json:"newPV,omitempty"`

	// NewPVC stores information about the new stabilized PVC
	// +optional
	NewPVC *NewPVCInfo `json:"newPVC,omitempty"`

	// Message is a status message or error details
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions are the conditions of the stabilization
	// +optional
	Conditions []VolumeStabilizationCondition `json:"conditions,omitempty"`

	// ObservedGeneration is the generation observed by the controller
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=vs;volstab
// +kubebuilder:printcolumn:name="Source PVC",type=string,JSONPath=`.spec.sourcePVC.name`,description="Original PVC name"
// +kubebuilder:printcolumn:name="Target PVC",type=string,JSONPath=`.spec.target.pvcName`,description="New PVC name"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`,description="Current phase"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// VolumeStabilization is the Schema for the volumestabilizations API
type VolumeStabilization struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VolumeStabilizationSpec   `json:"spec,omitempty"`
	Status VolumeStabilizationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VolumeStabilizationList contains a list of VolumeStabilization
type VolumeStabilizationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VolumeStabilization `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VolumeStabilization{}, &VolumeStabilizationList{})
}
