package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	jsonpatch "github.com/evanphx/json-patch"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"gnwes/internal/substratebridge"
)

func workerPod(labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ate-demo", Labels: labels},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: workerContainerName, Image: "ko://go.ate.dev/substrate/cmd/ateworker-microvm"}},
		},
	}
}

// postReview sends pod through handleMutate exactly as the apiserver would,
// and returns the resulting pod with the admission patch applied -- the
// real end-to-end check that the JSON Patch this webhook emits is actually
// well-formed and does what it claims, not just that the Go values look
// right in isolation.
func postReview(t *testing.T, pod *corev1.Pod, bridgeImage string, vfioIOMMUGroups ...string) *corev1.Pod {
	t.Helper()

	podRaw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID:    types.UID("test-uid"),
			Object: runtime.RawExtension{Raw: podRaw},
		},
	}
	reviewRaw, err := json.Marshal(review)
	if err != nil {
		t.Fatalf("marshal review: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(reviewRaw))
	rw := httptest.NewRecorder()
	handleMutate(rw, req, bridgeImage, "", vfioIOMMUGroups)

	var gotReview admissionv1.AdmissionReview
	if err := json.Unmarshal(rw.Body.Bytes(), &gotReview); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !gotReview.Response.Allowed {
		t.Fatalf("expected Allowed=true")
	}
	if len(gotReview.Response.Patch) == 0 {
		return pod // no patch emitted
	}
	patch, err := jsonpatch.DecodePatch(gotReview.Response.Patch)
	if err != nil {
		t.Fatalf("decoding emitted patch: %v (patch=%s)", err, gotReview.Response.Patch)
	}
	patched, err := patch.Apply(podRaw)
	if err != nil {
		t.Fatalf("applying emitted patch to the original pod: %v (patch=%s)", err, gotReview.Response.Patch)
	}
	var result corev1.Pod
	if err := json.Unmarshal(patched, &result); err != nil {
		t.Fatalf("unmarshal patched pod: %v", err)
	}
	return &result
}

func TestHandleMutate_GatesSharedTierPod(t *testing.T) {
	pod := workerPod(map[string]string{labelPoolRef: "pool-a"})
	got := postReview(t, pod, "unused:latest")

	if !hasGate(got) {
		t.Fatalf("expected the pod to carry the scheduling gate, got spec: %+v", got.Spec)
	}
	if len(got.Spec.Containers) != 1 {
		t.Fatalf("expected no sidecar for a Shared-tier pod, got %d containers", len(got.Spec.Containers))
	}
}

func TestHandleMutate_InjectsBridgeSidecarForExclusiveTier(t *testing.T) {
	pod := workerPod(map[string]string{
		labelPoolRef:  "pool-a",
		labelBackend:  backendExclusive,
		labelAtespace: "ate-demo-counter",
	})
	got := postReview(t, pod, "localhost:5001/substrate-gpu-bridge:test")

	if !hasGate(got) {
		t.Fatalf("expected the pod to still carry the scheduling gate")
	}
	if len(got.Spec.Containers) != 2 {
		t.Fatalf("expected worker + sidecar, got %d containers: %+v", len(got.Spec.Containers), got.Spec.Containers)
	}

	var worker, sidecar *corev1.Container
	for i := range got.Spec.Containers {
		c := &got.Spec.Containers[i]
		switch c.Name {
		case workerContainerName:
			worker = c
		case bridgeContainerName:
			sidecar = c
		}
	}
	if worker == nil || sidecar == nil {
		t.Fatalf("expected both %q and %q containers, got %+v", workerContainerName, bridgeContainerName, got.Spec.Containers)
	}
	if sidecar.Image != "localhost:5001/substrate-gpu-bridge:test" {
		t.Errorf("sidecar image = %q, want the injected bridgeImage", sidecar.Image)
	}
	if len(worker.VolumeMounts) != 2 {
		t.Fatalf("worker container volumeMounts = %+v, want exactly 2 (vc-vm + /dev/vfio)", worker.VolumeMounts)
	}
	var vcVMMount, vfioMount *corev1.VolumeMount
	for i := range worker.VolumeMounts {
		m := &worker.VolumeMounts[i]
		switch m.Name {
		case vcVMVolumeName:
			vcVMMount = m
		case devVFIOVolumeName:
			vfioMount = m
		}
	}
	if vcVMMount == nil || vcVMMount.MountPath != vcVMMountPath || vcVMMount.SubPath == "" {
		t.Errorf("worker container vc-vm mount = %+v, want %q at %q with a non-empty literal SubPath", vcVMMount, vcVMVolumeName, vcVMMountPath)
	}
	if vfioMount == nil || vfioMount.MountPath != devVFIOPath {
		t.Errorf("worker container /dev/vfio mount = %+v, want %q at %q", vfioMount, devVFIOVolumeName, devVFIOPath)
	}
	vcVMID := vcVMMount.SubPath
	if len(sidecar.VolumeMounts) != 0 {
		t.Errorf("sidecar volumeMounts = %+v, want none (it only writes the host path as a string)", sidecar.VolumeMounts)
	}
	if len(got.Spec.Volumes) != 2 {
		t.Fatalf("pod volumes = %+v, want exactly 2 (vc-vm + /dev/vfio)", got.Spec.Volumes)
	}
	var vcVMVolume, vfioVolume *corev1.Volume
	for i := range got.Spec.Volumes {
		v := &got.Spec.Volumes[i]
		switch v.Name {
		case vcVMVolumeName:
			vcVMVolume = v
		case devVFIOVolumeName:
			vfioVolume = v
		}
	}
	if vcVMVolume == nil || vcVMVolume.HostPath == nil || vcVMVolume.HostPath.Path != substratebridge.GNWESHostVCVMDir {
		t.Errorf("pod volumes = %+v, want a hostPath volume %q at %q", got.Spec.Volumes, vcVMVolumeName, substratebridge.GNWESHostVCVMDir)
	}
	if vfioVolume == nil || vfioVolume.HostPath == nil || vfioVolume.HostPath.Path != devVFIOPath {
		t.Errorf("pod volumes = %+v, want a hostPath volume %q at %q", got.Spec.Volumes, devVFIOVolumeName, devVFIOPath)
	}
	// privileged:true was tried and reverted (2026-11-13) -- see
	// exclusiveSidecarPatches's doc comment at the /dev/vfio volume: it
	// breaks ateworker-microvm's own startup. No securityContext assertion
	// here on purpose.

	// The worker container no longer carries a POD_NAME env var -- its
	// volumeMount's SubPath is now the literal vcVMID above, not
	// $(POD_NAME), so there is nothing left for it to resolve.
	if len(worker.Env) != 0 {
		t.Errorf("expected the worker container to carry no env vars, got %+v", worker.Env)
	}

	wantArgs := map[string]bool{
		"-kubectl-ate-bin=/usr/local/bin/kubectl-ate": true,
		"-atespace=ate-demo-counter":                  true,
		"-worker-pod=$(POD_NAME)":                     true,
		"-vcvm-id=" + vcVMID:                          true,
		"-attachment-namespace=$(POD_NAMESPACE)":      true,
	}
	if len(sidecar.Args) != len(wantArgs) {
		t.Fatalf("sidecar args = %v, want exactly %v", sidecar.Args, wantArgs)
	}
	for _, a := range sidecar.Args {
		if !wantArgs[a] {
			t.Errorf("unexpected sidecar arg %q", a)
		}
	}

	foundPodName, foundPodNamespace := false, false
	for _, e := range sidecar.Env {
		if e.Name == "POD_NAME" && e.ValueFrom != nil && e.ValueFrom.FieldRef != nil && e.ValueFrom.FieldRef.FieldPath == "metadata.name" {
			foundPodName = true
		}
		if e.Name == "POD_NAMESPACE" && e.ValueFrom != nil && e.ValueFrom.FieldRef != nil && e.ValueFrom.FieldRef.FieldPath == "metadata.namespace" {
			foundPodNamespace = true
		}
	}
	if !foundPodName || !foundPodNamespace {
		t.Errorf("expected POD_NAME/POD_NAMESPACE Downward API env vars, got %+v", sidecar.Env)
	}
}

func TestHandleMutate_ExclusiveTierWithoutAtespaceLabelSkipsSidecarButStillGates(t *testing.T) {
	pod := workerPod(map[string]string{
		labelPoolRef: "pool-a",
		labelBackend: backendExclusive,
		// no labelAtespace
	})
	got := postReview(t, pod, "unused:latest")

	if !hasGate(got) {
		t.Fatalf("expected the pod to still carry the scheduling gate")
	}
	if len(got.Spec.Containers) != 1 {
		t.Fatalf("expected no sidecar without an atespace label, got %d containers", len(got.Spec.Containers))
	}
}

func TestHandleMutate_InjectsVFIODevicesAnnotationWhenIOMMUGroupsConfigured(t *testing.T) {
	pod := workerPod(map[string]string{
		labelPoolRef:  "pool-a",
		labelBackend:  backendExclusive,
		labelAtespace: "ate-demo-counter",
	})
	got := postReview(t, pod, "localhost:5001/substrate-gpu-bridge:test", "8")

	want := "/dev/vfio/vfio:/dev/vfio/vfio:rwm,/dev/vfio/8:/dev/vfio/8:rwm"
	if got.Annotations[vfioDevicesAnnotation] != want {
		t.Errorf("pod annotation %q = %q, want %q", vfioDevicesAnnotation, got.Annotations[vfioDevicesAnnotation], want)
	}
}

func TestHandleMutate_NoVFIODevicesAnnotationWhenNoIOMMUGroupsConfigured(t *testing.T) {
	pod := workerPod(map[string]string{
		labelPoolRef:  "pool-a",
		labelBackend:  backendExclusive,
		labelAtespace: "ate-demo-counter",
	})
	got := postReview(t, pod, "localhost:5001/substrate-gpu-bridge:test")

	if _, ok := got.Annotations[vfioDevicesAnnotation]; ok {
		t.Errorf("expected no %q annotation when VFIO_IOMMU_GROUPS is unset, got %q", vfioDevicesAnnotation, got.Annotations[vfioDevicesAnnotation])
	}
}

func TestExclusiveSidecarPatches_SetsImagePullPolicyOnlyWhenRequested(t *testing.T) {
	newPod := func() *corev1.Pod {
		return &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: workerContainerName}}}}
	}
	findSidecar := func(patches []map[string]any) corev1.Container {
		for _, p := range patches {
			if p["path"] != "/spec/containers/-" {
				continue
			}
			b, _ := json.Marshal(p["value"])
			var c corev1.Container
			_ = json.Unmarshal(b, &c)
			return c
		}
		t.Fatal("no /spec/containers/- patch found")
		return corev1.Container{}
	}

	patches, err := exclusiveSidecarPatches(newPod(), "ate-demo", "image:latest", "", nil)
	if err != nil {
		t.Fatalf("exclusiveSidecarPatches: %v", err)
	}
	if got := findSidecar(patches).ImagePullPolicy; got != "" {
		t.Errorf("ImagePullPolicy = %q, want empty (unset) when bridgeImagePullPolicy is \"\"", got)
	}

	patches, err = exclusiveSidecarPatches(newPod(), "ate-demo", "image:latest", "IfNotPresent", nil)
	if err != nil {
		t.Fatalf("exclusiveSidecarPatches: %v", err)
	}
	if got := findSidecar(patches).ImagePullPolicy; got != corev1.PullIfNotPresent {
		t.Errorf("ImagePullPolicy = %q, want %q", got, corev1.PullIfNotPresent)
	}
}

func TestExclusiveSidecarPatches_ErrorsWithNoWorkerContainer(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "not-worker"}}}}
	if _, err := exclusiveSidecarPatches(pod, "ate-demo", "image:latest", "", nil); err == nil {
		t.Fatal("expected an error when no container is named \"worker\"")
	}
}

func TestExclusiveSidecarPatches_AppendsToExistingVolumesMountsAndEnv(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{Name: "existing-vol"}},
			Containers: []corev1.Container{
				{
					Name:         workerContainerName,
					VolumeMounts: []corev1.VolumeMount{{Name: "existing-vol", MountPath: "/existing"}},
					Env:          []corev1.EnvVar{{Name: "EXISTING_ENV", Value: "x"}},
				},
			},
		},
	}
	patches, err := exclusiveSidecarPatches(pod, "ate-demo", "image:latest", "", nil)
	if err != nil {
		t.Fatalf("exclusiveSidecarPatches: %v", err)
	}
	for _, p := range patches {
		path, _ := p["path"].(string)
		if path == "/spec/volumes" || path == "/spec/containers/0/volumeMounts" || path == "/spec/containers/0/env" {
			t.Errorf("expected an append path (trailing /-) when volumes/volumeMounts/env already exist, got op on %q", path)
		}
	}
}
