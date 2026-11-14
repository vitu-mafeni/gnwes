package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GPUBackend selects which real GPU attachment mechanism a GPUWorkerPool
// uses. Both are real — neither is simulated — but they make fundamentally
// different tradeoffs, matching GNWES's two-tier design:
type GPUBackend string

const (
	// BackendShared requests a real, Kubernetes-scheduled GPU resource (a
	// whole device via the NVIDIA device plugin, or a sliced virtual device
	// via HAMi) directly on the actor pod's spec, at pod-creation time. This
	// is the right fit for gVisor-sandboxed actors: cheap, shareable, but
	// bound once when the pod is admitted — it cannot hot-attach to a pod
	// that is already running. GNWES's role for this tier is admission
	// control (never let more pods request GPU capacity than the pool has
	// budget for) — the actual device binding is correctly left to
	// Kubernetes' own, already-real GPU scheduling.
	BackendShared GPUBackend = "Shared"

	// BackendExclusive hot-attaches/detaches a real, dedicated physical GPU
	// (via VFIO passthrough into a Cloud Hypervisor microVM guest) to an
	// ALREADY-RUNNING worker pod, with no restart — the one case where a
	// live mid-flight attach is genuinely possible, because a VM boundary
	// (unlike a gVisor sandbox) supports PCI hotplug. See
	// config/vmgpu/README.md for the real, hardware-verified mechanism this
	// backend drives (VFIOGPUPool + VMGPUAttachment).
	BackendExclusive GPUBackend = "Exclusive"
)

// GPUWorkerPoolSpec defines a pool of real GPU capacity that GPULeases draw
// from. Which tier's real attachment mechanism is used is controlled by
// Backend — see GPUBackend's doc comments for what each one actually does
// and does not provide.
type GPUWorkerPoolSpec struct {
	// PoolSize is the number of concurrent GPU-bearing actors this pool
	// admits. For Backend=Shared this is an admission budget (Kubernetes'
	// own scheduler still does the real device binding). For
	// Backend=Exclusive this must not exceed the referenced VFIOGPUPool's
	// real device count.
	// +kubebuilder:validation:Minimum=1
	PoolSize int32 `json:"poolSize"`

	// GPUModel is a free-text label for which GPU model this pool represents
	// (e.g. "A100-40GB"). Cosmetic/informational only — it does not affect
	// scheduling; GPUResourceName/GPUResourceQuantity do that.
	// +optional
	GPUModel string `json:"gpuModel,omitempty"`

	// Backend selects the real attachment mechanism. Defaults to Shared.
	// +kubebuilder:validation:Enum=Shared;Exclusive
	// +kubebuilder:default=Shared
	// +optional
	Backend GPUBackend `json:"backend,omitempty"`

	// GPUResourceName is the Kubernetes extended-resource key a Shared-tier
	// actor pod requests, e.g. "nvidia.com/gpu" for a whole device via the
	// NVIDIA device plugin, or a HAMi virtual-slice resource key (e.g.
	// "nvidia.com/gpucores") for shared GPU slicing. Ignored for
	// Backend=Exclusive.
	// +kubebuilder:default="nvidia.com/gpu"
	// +optional
	GPUResourceName string `json:"gpuResourceName,omitempty"`

	// GPUResourceQuantity is how much of GPUResourceName one admitted slot
	// represents, e.g. "1" for a whole device or "25" for a HAMi
	// core-percentage slice. Ignored for Backend=Exclusive.
	// +kubebuilder:default="1"
	// +optional
	GPUResourceQuantity string `json:"gpuResourceQuantity,omitempty"`

	// VFIOPoolRef names the VFIOGPUPool (same namespace) this pool draws
	// real physical devices from. Required for Backend=Exclusive, ignored
	// otherwise.
	// +optional
	VFIOPoolRef string `json:"vfioPoolRef,omitempty"`
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
// +kubebuilder:printcolumn:name="Backend",type=string,JSONPath=`.spec.backend`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Total",type=integer,JSONPath=`.status.totalSlots`
// +kubebuilder:printcolumn:name="Available",type=integer,JSONPath=`.status.availableSlots`
// +kubebuilder:printcolumn:name="Leased",type=integer,JSONPath=`.status.leasedSlots`
// +kubebuilder:resource:shortName=gwp

// GPUWorkerPool is a pool of real GPU capacity that GPULeases draw from. It
// is the GNWES control-plane object representing "N GPUs this many actors
// can concurrently hold," backed by whichever real mechanism Spec.Backend
// selects.
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
