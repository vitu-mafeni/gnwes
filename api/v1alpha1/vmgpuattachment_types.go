package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VMGPUAttachmentSpec requests that one real VFIO GPU device be hot-attached
// to (or detached from) an already-running cloud-hypervisor guest, without
// recreating that guest -- the real mechanism GPULease only simulates
// (sleep + pod annotation). See research/live-attach-poc/README.md for why
// this targets cloud-hypervisor specifically (Agent Substrate's microVM
// worker, ateworker-microvm, runs Kata on cloud-hypervisor, which already
// ships a live VFIO hotplug/hot-unplug API) rather than gVisor (confirmed
// blocked: nvproxy's device registry is built once, at sandbox creation,
// with no mechanism to add a device afterward).
type VMGPUAttachmentSpec struct {
	// PoolRef is the name of the VFIOGPUPool (same namespace) to acquire a
	// device from.
	PoolRef string `json:"poolRef"`

	// DeviceRef, if set, pins this attachment to one specific device ID
	// within PoolRef (e.g. because an orchestrator upstream already chose
	// it for hardware-compatibility reasons -- see "GPU checkpoint usually
	// restores only onto the same GPU model" in RUNBOOK.md/README.md's
	// discussion of Substrate's own stated constraints). Leave empty to let
	// the reconciler acquire any Available device in the pool.
	// +optional
	DeviceRef string `json:"deviceRef,omitempty"`

	// APISocket is the filesystem path to the target cloud-hypervisor
	// instance's unix api-socket: /run/vc/vm/<actor-uid>/clh-api.sock,
	// confirmed against agent-substrate's cmd/ateworker-microvm/internal/kata
	// (the same path Substrate's own worker drives for pause/snapshot/
	// resume). Keyed by actor UID, not by worker pod -- a worker pod reuses
	// one VM lifecycle per actor it ever hosts, so this path changes every
	// time a different actor resumes onto the same pod. Left empty until
	// cmd/substrate-gpu-bridge resolves the currently-assigned actor's UID;
	// it is written together with DesiredState (never DesiredState=Attached
	// with a stale or empty socket from a previous actor).
	// +optional
	APISocket string `json:"apiSocket,omitempty"`

	// DesiredState drives the reconciler: Attached hot-adds the acquired
	// device into the guest at APISocket; Detached hot-removes it and
	// returns the device to the pool. Flipping this (rather than
	// create/delete) is what lets the SAME VMGPUAttachment object track one
	// device across many attach/detach cycles as an actor's worker is
	// reused -- e.g. a poller watching Substrate actor resume/suspend
	// transitions via `kubectl-ate get actor -o json` can drive this field
	// directly, without this controller needing to know anything about
	// Substrate's own actor lifecycle. See research/live-attach-poc/README.md's
	// "Next steps" for that bridge; it is not implemented by this
	// reconciler, which only ever reacts to DesiredState.
	// +kubebuilder:validation:Enum=Attached;Detached
	DesiredState VMGPUDesiredState `json:"desiredState"`
}

// VMGPUDesiredState is what VMGPUAttachmentSpec.DesiredState may be set to.
type VMGPUDesiredState string

const (
	VMGPUDesiredAttached VMGPUDesiredState = "Attached"
	VMGPUDesiredDetached VMGPUDesiredState = "Detached"
)

// VMGPUAttachmentPhase is the lifecycle phase of an attachment.
type VMGPUAttachmentPhase string

const (
	VMGPUPhasePending   VMGPUAttachmentPhase = "Pending"
	VMGPUPhaseAttached  VMGPUAttachmentPhase = "Attached"
	VMGPUPhaseDetaching VMGPUAttachmentPhase = "Detaching"
	VMGPUPhaseDetached  VMGPUAttachmentPhase = "Detached"
	VMGPUPhaseFailed    VMGPUAttachmentPhase = "Failed"
)

// VMGPUAttachmentStatus reports the outcome of the real hot-attach/detach
// flow against cloud-hypervisor, including the measured latency -- this is
// where a genuine, hardware-measured "attach a real GPU to an
// already-running, reused sandbox" number can finally come from, unlike
// GPULease's simulated one.
type VMGPUAttachmentStatus struct {
	// +optional
	Phase VMGPUAttachmentPhase `json:"phase,omitempty"`

	// DeviceID is the vfiopool device this attachment is bound to, once
	// acquired.
	// +optional
	DeviceID string `json:"deviceID,omitempty"`

	// GuestBDF is the PCI address cloud-hypervisor assigned the device
	// inside the guest, once Attached (distinct from the device's host-side
	// sysfs path).
	// +optional
	GuestBDF string `json:"guestBDF,omitempty"`

	// +optional
	LastTransitionAt *metav1.Time `json:"lastTransitionAt,omitempty"`

	// AttachLatencyMillis is wall-clock time from DesiredState flipping to
	// Attached to the Attached phase being reached.
	// +optional
	AttachLatencyMillis int64 `json:"attachLatencyMillis,omitempty"`

	// ObservedDesiredState is the DesiredState value this status last
	// finished reconciling, so the controller can tell a fresh flip (needs
	// work) from a re-reconcile of one it already handled (no-op).
	// +optional
	ObservedDesiredState VMGPUDesiredState `json:"observedDesiredState,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolRef`
// +kubebuilder:printcolumn:name="Desired",type=string,JSONPath=`.spec.desiredState`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Device",type=string,JSONPath=`.status.deviceID`
// +kubebuilder:printcolumn:name="GuestBDF",type=string,JSONPath=`.status.guestBDF`
// +kubebuilder:printcolumn:name="AttachMs",type=integer,JSONPath=`.status.attachLatencyMillis`
// +kubebuilder:resource:shortName=vga

// VMGPUAttachment is a request to hot-attach (or hot-detach) one real VFIO
// GPU device to/from an already-running cloud-hypervisor guest, driven by
// flipping Spec.DesiredState rather than by creating/deleting the object --
// the object's identity persists across many attach/detach cycles, matching
// how Substrate reuses one worker across many actors.
type VMGPUAttachment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VMGPUAttachmentSpec   `json:"spec,omitempty"`
	Status VMGPUAttachmentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VMGPUAttachmentList contains a list of VMGPUAttachment.
type VMGPUAttachmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VMGPUAttachment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VMGPUAttachment{}, &VMGPUAttachmentList{})
}
