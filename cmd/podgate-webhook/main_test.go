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
	"k8s.io/apimachinery/pkg/api/resource"
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
	// Both tiers get the GPU-visibility env vars, not just Exclusive.
	assertGPUVisibilityEnv(t, got.Spec.Containers[0].Env)
}

func TestHandleMutate_RequestsVolcanoVGPUSliceWhenLabeled(t *testing.T) {
	pod := workerPod(map[string]string{
		labelPoolRef:         "pool-a",
		labelVGPUMemoryUnits: "12",
		labelVGPUCores:       "30",
	})
	pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Limits:   corev1.ResourceList{"cpu": resource.MustParse("1")},
		Requests: corev1.ResourceList{"cpu": resource.MustParse("250m")},
	}
	got := postReview(t, pod, "unused:latest")

	if got.Spec.SchedulerName != vgpuSchedulerName {
		t.Errorf("SchedulerName = %q, want %q", got.Spec.SchedulerName, vgpuSchedulerName)
	}
	if got.Spec.RuntimeClassName == nil || *got.Spec.RuntimeClassName != vgpuRuntimeClassName {
		t.Errorf("RuntimeClassName = %v, want %q", got.Spec.RuntimeClassName, vgpuRuntimeClassName)
	}
	worker := got.Spec.Containers[0]
	if worker.Resources.Limits[resourceVGPUMemory] != resource.MustParse("12") {
		t.Errorf("Limits[%s] = %v, want 12", resourceVGPUMemory, worker.Resources.Limits[resourceVGPUMemory])
	}
	if worker.Resources.Limits[resourceVGPUCores] != resource.MustParse("30") {
		t.Errorf("Limits[%s] = %v, want 30", resourceVGPUCores, worker.Resources.Limits[resourceVGPUCores])
	}
	// Both the always-on visibility vars and the vGPU-specific disable
	// vars must land together in the one worker container.
	assertGPUVisibilityEnv(t, worker.Env)
	wantDisableVars := map[string]bool{"NVIDIA_GDS": true, "NVIDIA_GDRCOPY": true, "NVIDIA_MOFED": true}
	found := map[string]bool{}
	for _, e := range worker.Env {
		if e.Value == "disabled" {
			found[e.Name] = true
		}
	}
	for name := range wantDisableVars {
		if !found[name] {
			t.Errorf("missing disabled env var %s, got env: %+v", name, worker.Env)
		}
	}
}

// assertGPUVisibilityEnv checks that env carries exactly the two
// Downward-API env vars gpuVisibilityPatches injects, pointed at the
// right pod labels.
func assertGPUVisibilityEnv(t *testing.T, env []corev1.EnvVar) {
	t.Helper()
	want := map[string]string{
		"GNWES_GPU_POOL":    "metadata.labels['" + labelPoolRef + "']",
		"GNWES_GPU_BACKEND": "metadata.labels['" + labelBackend + "']",
	}
	got := map[string]string{}
	for _, e := range env {
		if e.ValueFrom != nil && e.ValueFrom.FieldRef != nil {
			got[e.Name] = e.ValueFrom.FieldRef.FieldPath
		}
	}
	for name, fieldPath := range want {
		if got[name] != fieldPath {
			t.Errorf("env %s fieldPath = %q, want %q (env: %+v)", name, got[name], fieldPath, env)
		}
	}
}

func TestHandleMutate_NoGPUPoolLabelSkipsVisibilityEnv(t *testing.T) {
	pod := workerPod(nil)
	got := postReview(t, pod, "unused:latest")

	if len(got.Spec.Containers[0].Env) != 0 {
		t.Errorf("expected no GPU-visibility env vars on a pod with no %s label, got %+v", labelPoolRef, got.Spec.Containers[0].Env)
	}
}

func TestWorkerEnvPatches_AppendsToExistingEnv(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
		{Name: workerContainerName, Env: []corev1.EnvVar{{Name: "EXISTING_ENV", Value: "x"}}},
	}}}
	patches, err := workerEnvPatches(pod)
	if err != nil {
		t.Fatalf("workerEnvPatches: %v", err)
	}
	for _, p := range patches {
		if path, _ := p["path"].(string); path == "/spec/containers/0/env" {
			t.Errorf("expected an append path (trailing /-) when env already exists, got op on %q", path)
		}
	}
}

func TestWorkerEnvPatches_ErrorsWithNoWorkerContainer(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "not-worker"}}}}
	if _, err := workerEnvPatches(pod); err == nil {
		t.Fatal("expected an error when no container is named \"worker\"")
	}
}

func TestWorkerEnvPatches_IncludesExtraVarsInOneAppendPass(t *testing.T) {
	// Regression guard for the cross-function append-tracking hazard
	// workerEnvPatches's own doc comment describes: gpuVisibilityEnvVars
	// and any extra vars (e.g. vgpuDisableEnvVars) must land in ONE
	// combined pass, not two independently-tracked ones.
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: workerContainerName}}}}
	patches, err := workerEnvPatches(pod, vgpuDisableEnvVars()...)
	if err != nil {
		t.Fatalf("workerEnvPatches: %v", err)
	}
	// Starting from an empty env array: exactly one "add whole array" op,
	// then append ("/-") ops for every remaining var -- never a second
	// "add whole array" op, which would silently drop everything the
	// first one put there.
	wantCount := len(gpuVisibilityEnvVars()) + len(vgpuDisableEnvVars())
	if len(patches) != wantCount {
		t.Fatalf("got %d patches, want %d (one per env var)", len(patches), wantCount)
	}
	wholeArrayAdds := 0
	for _, p := range patches {
		if path, _ := p["path"].(string); path == "/spec/containers/0/env" {
			wholeArrayAdds++
		}
	}
	if wholeArrayAdds != 1 {
		t.Errorf("expected exactly 1 whole-array add, got %d", wholeArrayAdds)
	}
}

func TestVolcanoVGPUPatches_NoLabelsReturnsNilNil(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: workerContainerName}}}}
	patches, err := volcanoVGPUPatches(pod)
	if err != nil || patches != nil {
		t.Fatalf("got patches=%v err=%v, want nil, nil when neither vGPU label is set", patches, err)
	}
}

func TestVolcanoVGPUPatches_OnlyOneLabelErrors(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{labelVGPUMemoryUnits: "12"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: workerContainerName}}},
	}
	if _, err := volcanoVGPUPatches(pod); err == nil {
		t.Fatal("expected an error when only one of the two vGPU labels is set")
	}
}

func TestVolcanoVGPUPatches_SetsSchedulerRuntimeClassAndResources(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{labelVGPUMemoryUnits: "12", labelVGPUCores: "30"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: workerContainerName,
			Resources: corev1.ResourceRequirements{
				Limits:   corev1.ResourceList{"cpu": resource.MustParse("1"), "memory": resource.MustParse("1Gi")},
				Requests: corev1.ResourceList{"cpu": resource.MustParse("250m"), "memory": resource.MustParse("1Gi")},
			},
		}}},
	}
	patches, err := volcanoVGPUPatches(pod)
	if err != nil {
		t.Fatalf("volcanoVGPUPatches: %v", err)
	}

	patched, err := applyPatches(t, pod, patches)
	if err != nil {
		t.Fatalf("applying patches: %v", err)
	}
	if patched.Spec.SchedulerName != vgpuSchedulerName {
		t.Errorf("SchedulerName = %q, want %q", patched.Spec.SchedulerName, vgpuSchedulerName)
	}
	if patched.Spec.RuntimeClassName == nil || *patched.Spec.RuntimeClassName != vgpuRuntimeClassName {
		t.Errorf("RuntimeClassName = %v, want %q", patched.Spec.RuntimeClassName, vgpuRuntimeClassName)
	}
	worker := patched.Spec.Containers[0]
	for _, c := range []struct {
		name string
		list corev1.ResourceList
	}{{"limits", worker.Resources.Limits}, {"requests", worker.Resources.Requests}} {
		if c.list[resourceVGPUNumber] != resource.MustParse("1") {
			t.Errorf("%s[%s] = %v, want 1", c.name, resourceVGPUNumber, c.list[resourceVGPUNumber])
		}
		if c.list[resourceVGPUMemory] != resource.MustParse("12") {
			t.Errorf("%s[%s] = %v, want 12", c.name, resourceVGPUMemory, c.list[resourceVGPUMemory])
		}
		if c.list[resourceVGPUCores] != resource.MustParse("30") {
			t.Errorf("%s[%s] = %v, want 30", c.name, resourceVGPUCores, c.list[resourceVGPUCores])
		}
		// Pre-existing keys must survive.
		if cpu, ok := c.list["cpu"]; !ok || cpu.IsZero() {
			t.Errorf("%s lost its pre-existing cpu entry: %v", c.name, c.list)
		}
	}
}

func TestVolcanoVGPUPatches_CreatesResourcesWhenAbsent(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{labelVGPUMemoryUnits: "12", labelVGPUCores: "30"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: workerContainerName}}},
	}
	patches, err := volcanoVGPUPatches(pod)
	if err != nil {
		t.Fatalf("volcanoVGPUPatches: %v", err)
	}
	patched, err := applyPatches(t, pod, patches)
	if err != nil {
		t.Fatalf("applying patches: %v", err)
	}
	if len(patched.Spec.Containers[0].Resources.Limits) != 3 {
		t.Errorf("Limits = %v, want exactly the 3 vgpu keys", patched.Spec.Containers[0].Resources.Limits)
	}
}

// applyPatches marshals pod, applies patches as a real JSON Patch (the same
// library postReview uses), and unmarshals the result -- an end-to-end
// check that the patches are well-formed, not just structurally plausible.
func applyPatches(t *testing.T, pod *corev1.Pod, patches []map[string]any) (*corev1.Pod, error) {
	t.Helper()
	podRaw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	patchBytes, err := json.Marshal(patches)
	if err != nil {
		t.Fatalf("marshal patches: %v", err)
	}
	patch, err := jsonpatch.DecodePatch(patchBytes)
	if err != nil {
		t.Fatalf("decoding patch: %v (patch=%s)", err, patchBytes)
	}
	patchedRaw, err := patch.Apply(podRaw)
	if err != nil {
		return nil, err
	}
	var result corev1.Pod
	if err := json.Unmarshal(patchedRaw, &result); err != nil {
		t.Fatalf("unmarshal patched pod: %v", err)
	}
	return &result, nil
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
	// $(POD_NAME), so there is nothing left for it to resolve. It does now
	// carry the GPU-visibility env vars (gpuVisibilityPatches), injected
	// for every GNWES-gated pod regardless of tier.
	if len(worker.Env) != 2 {
		t.Fatalf("worker container env = %+v, want exactly 2 (GNWES_GPU_POOL + GNWES_GPU_BACKEND)", worker.Env)
	}
	assertGPUVisibilityEnv(t, worker.Env)

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
