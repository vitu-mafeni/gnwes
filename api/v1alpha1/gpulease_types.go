package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GPULeaseSpec requests that one warm slot from PoolRef be attached to
// TargetPod for the lifetime of this lease. This is the "lease a GPU by the
// second" primitive: creating it attaches a slot in sub-second time, deleting
// it (or letting the TTL expire) detaches and returns the slot to the pool
// for immediate reuse by the next actor.
type GPULeaseSpec struct {
	// PoolRef is the name of the GPUWorkerPool (same namespace) to lease from.
	PoolRef string `json:"poolRef"`

	// TargetPod is the name of the actor pod (same namespace) the leased slot
	// is attached to. In this demo build "attach" means annotating the pod
	// with the assigned slot id, simulating CDI device injection into an
	// already-running sandbox.
	TargetPod string `json:"targetPod"`

	// TTLSeconds, if set, auto-releases the lease this many seconds after it
	// became Bound, without the caller needing to delete it explicitly.
	// +optional
	TTLSeconds *int32 `json:"ttlSeconds,omitempty"`
}

// GPULeasePhase is the lifecycle phase of a lease.
type GPULeasePhase string

const (
	LeasePhasePending   GPULeasePhase = "Pending"
	LeasePhaseBound     GPULeasePhase = "Bound"
	LeasePhaseReleasing GPULeasePhase = "Releasing"
	LeasePhaseReleased  GPULeasePhase = "Released"
	LeasePhaseFailed    GPULeasePhase = "Failed"
)

// GPULeaseStatus reports the outcome of the attach/detach flow, including the
// measured attach latency the demo uses as its headline number.
type GPULeaseStatus struct {
	// +optional
	Phase GPULeasePhase `json:"phase,omitempty"`

	// SlotID is the warm pool slot assigned to this lease once Bound.
	// +optional
	SlotID string `json:"slotID,omitempty"`

	// +optional
	RequestedAt *metav1.Time `json:"requestedAt,omitempty"`

	// +optional
	AttachedAt *metav1.Time `json:"attachedAt,omitempty"`

	// +optional
	ReleasedAt *metav1.Time `json:"releasedAt,omitempty"`

	// AttachLatencyMillis is wall-clock time from lease creation to Bound.
	// +optional
	AttachLatencyMillis int64 `json:"attachLatencyMillis,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolRef`
// +kubebuilder:printcolumn:name="Pod",type=string,JSONPath=`.spec.targetPod`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Slot",type=string,JSONPath=`.status.slotID`
// +kubebuilder:printcolumn:name="AttachMs",type=integer,JSONPath=`.status.attachLatencyMillis`
// +kubebuilder:resource:shortName=gl

// GPULease is a request to attach one warm GPU slot to a running actor pod.
type GPULease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GPULeaseSpec   `json:"spec,omitempty"`
	Status GPULeaseStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GPULeaseList contains a list of GPULease.
type GPULeaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GPULease `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GPULease{}, &GPULeaseList{})
}
