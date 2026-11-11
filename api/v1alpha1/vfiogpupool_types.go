package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VFIOGPUPoolSpec declares a fixed inventory of real, physical VFIO-bound
// GPUs on one node. Unlike GPUWorkerPoolSpec (which conjures simulated slots
// on demand via PoolSize), real devices either exist on the node or don't --
// this just lists the ones VMGPUAttachments on this pool may acquire.
type VFIOGPUPoolSpec struct {
	// NodeName is the node these devices physically live on. A
	// VFIOGPUPoolReconciler/bridge is expected to run with access to that
	// node (or be told its device inventory out of band) -- this type only
	// declares the inventory, it does not discover it.
	NodeName string `json:"nodeName"`

	// Devices is the fixed set of real VFIO devices this pool manages.
	// +kubebuilder:validation:MinItems=1
	Devices []VFIODeviceSpec `json:"devices"`
}

// VFIODeviceSpec names one real, physical VFIO-bound GPU.
type VFIODeviceSpec struct {
	// ID is a stable logical name for this device within the pool (e.g.
	// "gpu-0"), used by VMGPUAttachmentStatus.DeviceID and by
	// VMGPUAttachmentSpec.DeviceRef to pin a specific device.
	ID string `json:"id"`

	// PCIPath is the device's host sysfs path, e.g.
	// "/sys/bus/pci/devices/0000:01:00.0/" -- what gets passed as
	// chclient.DeviceConfig.Path on hot-attach.
	PCIPath string `json:"pciPath"`
}

// VFIOGPUPoolStatus mirrors the live vfiopool.Pool state for visibility,
// the same relationship GPUWorkerPoolStatus has to internal/pool.
type VFIOGPUPoolStatus struct {
	// +optional
	TotalDevices int32 `json:"totalDevices"`

	// +optional
	AvailableDevices int32 `json:"availableDevices"`

	// +optional
	AttachedDevices int32 `json:"attachedDevices"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="Total",type=integer,JSONPath=`.status.totalDevices`
// +kubebuilder:printcolumn:name="Available",type=integer,JSONPath=`.status.availableDevices`
// +kubebuilder:printcolumn:name="Attached",type=integer,JSONPath=`.status.attachedDevices`
// +kubebuilder:resource:shortName=vgp

// VFIOGPUPool is a fixed inventory of real, physical VFIO-bound GPUs on one
// node, available for live hot-attach/detach into a cloud-hypervisor guest
// via VMGPUAttachment. This is the "real hardware" counterpart to
// GPUWorkerPool -- same shape, same race-free pool-bookkeeping property
// (see internal/vfiopool), but naming actual devices instead of simulated
// slots, and feeding VMGPUAttachment's live hotplug instead of GPULease's
// sleep-and-annotate.
type VFIOGPUPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VFIOGPUPoolSpec   `json:"spec,omitempty"`
	Status VFIOGPUPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VFIOGPUPoolList contains a list of VFIOGPUPool.
type VFIOGPUPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VFIOGPUPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VFIOGPUPool{}, &VFIOGPUPoolList{})
}
