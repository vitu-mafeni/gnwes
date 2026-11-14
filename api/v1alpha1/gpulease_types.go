package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GPULeaseSpec requests that one unit of real GPU capacity from PoolRef be
// bound to TargetPod for the lifetime of this lease. What "bound" means
// depends on the referenced GPUWorkerPool's Backend:
//   - Shared: TargetPod's real GPU resource request (already present on its
//     spec since pod creation) is admitted against the pool's budget; this
//     lease records which real node/resource Kubernetes actually bound.
//   - Exclusive: a real physical GPU is live hot-attached via VFIO into
//     TargetPod's already-running microVM guest (no restart). Deleting this
//     lease (or letting the TTL expire) hot-detaches it and returns the
//     device to the pool for immediate reuse by the next actor.
type GPULeaseSpec struct {
	// PoolRef is the name of the GPUWorkerPool (same namespace) to lease from.
	PoolRef string `json:"poolRef"`

	// TargetPod is the name of the actor pod (same namespace) the lease
	// binds real GPU capacity to.
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

// GPULeaseStatus reports the outcome of the attach/detach flow, including
// the REAL measured attach latency (wall-clock, no injected delay).
type GPULeaseStatus struct {
	// +optional
	Phase GPULeasePhase `json:"phase,omitempty"`

	// SlotID identifies the real GPU capacity bound once Bound: for
	// Backend=Shared, "<node>/<resourceName>" for the node and extended
	// resource Kubernetes actually scheduled; for Backend=Exclusive, the
	// real VFIOGPUPool device ID that was hot-attached. This is a
	// human-facing descriptor; it is not necessarily the key GNWES itself
	// uses to release capacity — see AdmissionTicket.
	// +optional
	SlotID string `json:"slotID,omitempty"`

	// AdmissionTicket is GNWES's own internal admission-budget ticket ID
	// (Backend=Shared only; see internal/pool). It is not a device
	// identity — SlotID is the human-facing real node/resource descriptor.
	// +optional
	AdmissionTicket string `json:"admissionTicket,omitempty"`

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

// GPULease is a request to bind one unit of real GPU capacity to an actor
// pod. See GPULeaseSpec's doc comment for what "bind" means per backend.
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
