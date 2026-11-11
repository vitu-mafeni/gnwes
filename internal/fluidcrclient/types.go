// Package fluidcrclient is a minimal Go client for the subset of FluidCR's
// real, already-deployed CRDs (group fluidcr.dcnlab.com/v1alpha1) that GNWES
// needs to drive GPU checkpoint/restore for the Shared (HAMi) tier:
// FluidCRCheckpoint (save one pod's GPU work and free its GPU) and
// FluidCRRestore (bring it back, continuing from the saved state).
//
// This deliberately does NOT import FluidCR's own Go module
// (github.com/lehuannhatrang/fluidcr/operator) -- consistent with how this
// repo talks to every other external system (Substrate via `kubectl-ate`,
// cloud-hypervisor via its REST API, not their Go packages): only the wire
// format (the CRD's JSON shape, confirmed by reading FluidCR's own
// api/v1alpha1 types) is depended on, not FluidCR's internal source. These
// types carry only the fields GNWES actually reads or writes; FluidCR's own
// CRD has more (see its own operator/api/v1alpha1 package for the full
// schema), and the extra fields simply round-trip through unknown-field
// pruning as zero values, which is fine since this client never sets them.
// +kubebuilder:object:generate=true
package fluidcrclient

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is FluidCR's own API group, confirmed from its real
// operator/api/v1alpha1/groupversion_info.go.
var GroupVersion = schema.GroupVersion{Group: "fluidcr.dcnlab.com", Version: "v1alpha1"}

var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

// AddToScheme adds FluidCRCheckpoint/FluidCRRestore to a scheme, so a
// controller-runtime client can Get/Create/Update them like any other typed
// object.
var AddToScheme = SchemeBuilder.AddToScheme

// WorkloadReference identifies the workload FluidCRCheckpoint targets.
// Mirrors FluidCR's own WorkloadReference (fluidcrmigration_types.go);
// GNWES only ever targets a single Pod directly.
type WorkloadReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace,omitempty"`
}

// FluidCRCheckpointSpec mirrors FluidCR's own type -- see
// operator/api/v1alpha1/fluidcrcheckpoint_types.go for the authoritative,
// fuller schema. GNWES only sets the fields it needs; the rest take
// FluidCR's own defaults.
type FluidCRCheckpointSpec struct {
	WorkloadRef WorkloadReference `json:"workloadRef"`
}

// FluidCRCheckpointPhase mirrors FluidCR's own phase constants.
type FluidCRCheckpointPhase string

const (
	FCPhasePending                FluidCRCheckpointPhase = "Pending"
	FCPhaseAppCheckpointing       FluidCRCheckpointPhase = "AppCheckpointing"
	FCPhaseWaitingForPark         FluidCRCheckpointPhase = "WaitingForPark"
	FCPhaseContainerCheckpointing FluidCRCheckpointPhase = "ContainerCheckpointing"
	FCPhaseTearingDown            FluidCRCheckpointPhase = "TearingDown"
	FCPhaseCompleted              FluidCRCheckpointPhase = "Completed"
	FCPhaseFailed                 FluidCRCheckpointPhase = "Failed"
)

// CheckpointFile mirrors FluidCR's own type -- the CRIU archive a checkpoint
// produced, and the only thing a restore needs to find it again.
type CheckpointFile struct {
	ContainerName  string       `json:"containerName"`
	FilePath       string       `json:"filePath"`
	CheckpointTime *metav1.Time `json:"checkpointTime,omitempty"`
}

// PodCheckpointStatus mirrors FluidCR's own type -- GNWES reads NodeName and
// CheckpointFiles from this after a checkpoint completes, to construct the
// matching FluidCRRestore.
type PodCheckpointStatus struct {
	PodName         string           `json:"podName"`
	NodeName        string           `json:"nodeName,omitempty"`
	ContainerName   string           `json:"containerName,omitempty"`
	Phase           string           `json:"phase,omitempty"`
	Message         string           `json:"message,omitempty"`
	CheckpointFiles []CheckpointFile `json:"checkpointFiles,omitempty"`
}

type FluidCRCheckpointStatus struct {
	Phase   FluidCRCheckpointPhase `json:"phase,omitempty"`
	Message string                 `json:"message,omitempty"`
	Pods    []PodCheckpointStatus  `json:"pods,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// FluidCRCheckpoint mirrors FluidCR's own CRD (kind FluidCRCheckpoint,
// group fluidcr.dcnlab.com/v1alpha1) -- GNWES creates this to checkpoint one
// actor's pod and free its GPU; see FluidCR's own
// operator/api/v1alpha1/fluidcrcheckpoint_types.go for the full, real
// semantics this client relies on without re-implementing.
type FluidCRCheckpoint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FluidCRCheckpointSpec   `json:"spec,omitempty"`
	Status FluidCRCheckpointStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type FluidCRCheckpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FluidCRCheckpoint `json:"items"`
}

// ArchiveSource mirrors FluidCR's own type -- locates one CRIU archive by
// its host path and the container it came from. FluidCR does not pin a
// node here; the caller's Template.Spec.NodeSelector is what actually
// decides where the restored pod lands, and it must match the node the
// archive's Path actually lives on (same-node restore is the proven path;
// see research notes on cross-node restore remaining unverified).
type ArchiveSource struct {
	Path      string `json:"path"`
	Container string `json:"container,omitempty"`
}

// RestorePodTemplateMeta mirrors FluidCR's own type.
type RestorePodTemplateMeta struct {
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// RestorePodTemplate mirrors FluidCR's own type -- the pod to create,
// almost exactly as it would be written by hand. A CRIU restore rebuilds
// the process from the archive, so Spec's command/args/env are never
// executed; only the sandbox (resourceClaims, runtimeClassName, volumes,
// nodeSelector) matters.
type RestorePodTemplate struct {
	Metadata RestorePodTemplateMeta `json:"metadata,omitempty"`
	Spec     corev1.PodSpec         `json:"spec"`
}

// FluidCRRestoreSpec mirrors FluidCR's own type.
type FluidCRRestoreSpec struct {
	Archive  ArchiveSource      `json:"archive"`
	PodName  string             `json:"podName,omitempty"`
	Template RestorePodTemplate `json:"template"`
	Resume   *bool              `json:"resume,omitempty"`
}

// FluidCRRestorePhase mirrors FluidCR's own phase constants.
type FluidCRRestorePhase string

const (
	FRPhaseCreating        FluidCRRestorePhase = "Creating"
	FRPhaseWaitingForReady FluidCRRestorePhase = "WaitingForReady"
	FRPhaseResuming        FluidCRRestorePhase = "Resuming"
	FRPhaseCompleted       FluidCRRestorePhase = "Completed"
	FRPhaseFailed          FluidCRRestorePhase = "Failed"
)

type FluidCRRestoreStatus struct {
	Phase    FluidCRRestorePhase `json:"phase,omitempty"`
	Message  string              `json:"message,omitempty"`
	PodName  string              `json:"podName,omitempty"`
	NodeName string              `json:"nodeName,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// FluidCRRestore mirrors FluidCR's own CRD -- GNWES creates this to bring
// an actor's checkpointed pod back, continuing its GPU work from where it
// was saved.
type FluidCRRestore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FluidCRRestoreSpec   `json:"spec,omitempty"`
	Status FluidCRRestoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type FluidCRRestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FluidCRRestore `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FluidCRCheckpoint{}, &FluidCRCheckpointList{}, &FluidCRRestore{}, &FluidCRRestoreList{})
}
