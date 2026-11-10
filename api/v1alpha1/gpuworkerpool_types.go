package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GPUWorkerPoolSpec defines a pool of pre-warmed, simulated GPU slots that
// GPULeases attach to and detach from in sub-second time. The pool itself is
// slow to stand up (SlotInitMillis per slot) — that cost is paid once, ahead
// of any agent needing a GPU, not on every agent's critical path.
type GPUWorkerPoolSpec struct {
	// PoolSize is the number of warm GPU slots this pool maintains.
	// +kubebuilder:validation:Minimum=1
	PoolSize int32 `json:"poolSize"`

	// GPUModel is a label for which simulated GPU model this pool represents
	// (e.g. "A100-mock-40GB"). Cosmetic only in this demo build.
	// +optional
	GPUModel string `json:"gpuModel,omitempty"`

	// SlotInitMillis simulates the one-time cost of standing up a single warm
	// slot (device init, driver load, VRAM clear) when the pool is created or
	// scaled up. This models the real "cold" GPU-provisioning path.
	// +kubebuilder:default=4000
	// +optional
	SlotInitMillis int32 `json:"slotInitMillis,omitempty"`

	// AttachLatencyMillis simulates the CDI-injection attach cost paid when a
	// GPULease binds an already-warm slot to a running actor pod. This models
	// the fast path GNWES exists to deliver.
	// +kubebuilder:default=150
	// +optional
	AttachLatencyMillis int32 `json:"attachLatencyMillis,omitempty"`
}

// GPUWorkerPoolPhase is the coarse lifecycle phase of a pool.
type GPUWorkerPoolPhase string

const (
	PoolPhaseWarming GPUWorkerPoolPhase = "Warming"
	PoolPhaseReady   GPUWorkerPoolPhase = "Ready"
)

// GPUWorkerPoolStatus reports the current state of the warm pool.
type GPUWorkerPoolStatus struct {
	// Phase is Warming while slots are still being initialized, Ready once
	// every configured slot exists and can be leased.
	// +optional
	Phase GPUWorkerPoolPhase `json:"phase,omitempty"`

	// TotalSlots is the number of slots the pool has finished initializing.
	// +optional
	TotalSlots int32 `json:"totalSlots"`

	// AvailableSlots is the number of warm slots not currently leased.
	// +optional
	AvailableSlots int32 `json:"availableSlots"`

	// LeasedSlots is the number of warm slots currently attached to an actor.
	// +optional
	LeasedSlots int32 `json:"leasedSlots"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Total",type=integer,JSONPath=`.status.totalSlots`
// +kubebuilder:printcolumn:name="Available",type=integer,JSONPath=`.status.availableSlots`
// +kubebuilder:printcolumn:name="Leased",type=integer,JSONPath=`.status.leasedSlots`
// +kubebuilder:resource:shortName=gwp

// GPUWorkerPool is a warm pool of simulated GPU slots that GPULeases attach
// to. It is the GNWES control-plane object representing "N GPUs kept warm
// and ready" ahead of any actor needing one.
type GPUWorkerPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GPUWorkerPoolSpec   `json:"spec,omitempty"`
	Status GPUWorkerPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GPUWorkerPoolList contains a list of GPUWorkerPool.
type GPUWorkerPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GPUWorkerPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GPUWorkerPool{}, &GPUWorkerPoolList{})
}
