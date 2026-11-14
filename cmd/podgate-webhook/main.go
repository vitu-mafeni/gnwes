// Command podgate-webhook is a mutating admission webhook. Agent Substrate's
// WorkerPool API has no field for scheduling gates, so pods it generates from
// a GNWES-gated WorkerPool template (carrying the gnwes.io/gpu-pool label)
// never get GNWES's gnwes.io/gpu-capacity gate added at creation time by any
// other means. This webhook is the standard Kubernetes extension point for
// exactly that: it injects the gate on Pod CREATE for any pod labeled
// gnwes.io/gpu-pool, before the apiserver persists it. PodGateReconciler
// (internal/controller) is still the only thing that ever removes the gate.
//
// It also injects cmd/substrate-gpu-bridge as a sidecar into any
// Backend=Exclusive worker pod, for the same reason as the gate: Substrate's
// WorkerPoolPodTemplate has no field to add a sidecar container either, so
// this is the only real extension point. This is the one deployment step
// the bridge could not do for itself, since its own binary has no way to
// get built into a pod that doesn't exist yet.
//
// It also hostPath-mounts the worker container's /run/vc/vm -- the
// directory ateworker-microvm writes its cloud-hypervisor api-sockets
// into -- onto a per-pod subdirectory of GNWESHostVCVMDir on the node, so
// that cmd/manager's VMGPUAttachmentReconciler (which runs as its own
// process, outside every worker pod) can actually dial that socket from
// the host filesystem. The bridge sidecar itself needs no such mount: it
// only ever writes the deterministic HOST path as a string
// (substratebridge.HostAPISocketPath), it never opens the socket itself.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"

	"gnwes/internal/substratebridge"
)

const (
	labelPoolRef  = "gnwes.io/gpu-pool"
	labelBackend  = "gnwes.io/gpu-backend"
	labelAtespace = "gnwes.io/atespace"
	gateName      = "gnwes.io/gpu-capacity"

	backendExclusive = "Exclusive"

	// workerContainerName is the container Agent Substrate's own WorkerPool
	// controller always names the actor-hosting container (confirmed in
	// agent-substrate's cmd/atecontroller/internal/controllers/workerpool_apply.go,
	// WithContainerName("worker")) -- the one that needs the hostPath mount
	// so its cloud-hypervisor api-sockets land somewhere cmd/manager can
	// reach from outside the pod.
	workerContainerName = "worker"
	vcVMVolumeName      = "gnwes-vc-vm"
	vcVMMountPath       = "/run/vc/vm"
	bridgeContainerName = "gnwes-substrate-gpu-bridge"

	// devVFIOPath is bind-mounted, host path to container path identical,
	// into the "worker" container so cloud-hypervisor's own vm.add-device
	// can actually open the VFIO container/group device nodes it needs --
	// see exclusiveSidecarPatches.
	devVFIOVolumeName = "gnwes-dev-vfio"
	devVFIOPath       = "/dev/vfio"

	// vfioDevicesAnnotation is CRI-O's own experimental admission
	// annotation (recognized when the node's runtime handler config lists
	// it in allowed_annotations, and the specific device paths requested
	// are listed in that node's allowed_devices) that grants the "worker"
	// container's device cgroup access to specific host device nodes
	// without needing privileged:true -- which ateworker-microvm's own
	// startup check refuses to run under, see exclusiveSidecarPatches's
	// comment on the /dev/vfio mount below. Confirmed live (2026-11-14):
	// the /dev/vfio hostPath mount alone makes the device paths resolve,
	// but the kernel's device cgroup still blocks open() with EPERM unless
	// explicitly allowed this way. Value format and the allowed_devices
	// gate confirmed against CRI-O 1.35's own `crio config` dump on
	// dcn-gpu-0; see config/vmgpu/README.md.
	vfioDevicesAnnotation = "io.kubernetes.cri-o.Devices"

	// vfioContainerDevice is the one shared VFIO control device every
	// vfio-pci-bound GPU needs opened, regardless of which IOMMU group it
	// ends up in.
	vfioContainerDevice = "/dev/vfio/vfio"

	// labelVGPUMemoryUnits/labelVGPUCores opt a Shared-tier pod into a real
	// Volcano vGPU slice (volcano.sh/vgpu-*) instead of the whole-physical-
	// GPU DRA+CDI path (config/dra) -- set directly on the Substrate
	// WorkerPool's own pod template, the same convention as gnwes.io/gpu-pool
	// etc. Confirmed live (2026-11-15) against this project's own real
	// deployment: Volcano scheduler + volcano-vgpu-device-plugin, with
	// dcn-gpu-0's node allocatable actually showing volcano.sh/vgpu-number:
	// 20, volcano.sh/vgpu-memory: 768 (2 physical GPUs x this cluster's
	// configured deviceSplitCount=10 and gpuMemoryFactor=64 MiB/unit). See
	// volcanoVGPUPatches.
	labelVGPUMemoryUnits = "gnwes.io/vgpu-memory-units"
	labelVGPUCores       = "gnwes.io/vgpu-cores"

	// defaultVGPUSchedulerName/defaultVGPURuntimeClassName match this
	// project's own real cluster (see labelVGPUMemoryUnits's doc comment)
	// but are NOT assumed true of every cluster -- a different Volcano
	// install could name its runtime handler differently, so both are
	// overridable via VGPU_SCHEDULER_NAME/VGPU_RUNTIME_CLASS_NAME (see
	// main), the same env-var-with-default pattern as BRIDGE_IMAGE.
	defaultVGPUSchedulerName    = "volcano"
	defaultVGPURuntimeClassName = "nvidia-legacy"

	resourceVGPUNumber = "volcano.sh/vgpu-number"
	resourceVGPUMemory = "volcano.sh/vgpu-memory"
	resourceVGPUCores  = "volcano.sh/vgpu-cores"

	// vgpuDebugVolumeNamePrefix names each per-path hostPath volume
	// vgpuDebugMountPatches creates -- see its own doc comment.
	vgpuDebugVolumeNamePrefix = "gnwes-vgpu-debug-"
)

var codecs = serializer.NewCodecFactory(runtime.NewScheme())

func main() {
	certFile := envOr("TLS_CERT_FILE", "/certs/tls.crt")
	keyFile := envOr("TLS_KEY_FILE", "/certs/tls.key")
	bridgeImage := envOr("BRIDGE_IMAGE", "localhost:5001/substrate-gpu-bridge:latest")
	// Empty by default, meaning "don't set it" -- Kubernetes then falls
	// back to its own normal default (Always for a :latest-tagged image).
	// An operator can override this (e.g. to IfNotPresent) when a registry
	// is temporarily unreachable and the image has instead been loaded
	// directly onto each node's container runtime -- confirmed needed the
	// hard way (2026-11-13): Harbor intermittently timed out committing
	// this image's two largest layers, well after upload, for reasons
	// unrelated to this code (its own registry pod's logs showed the
	// failing requests never even arriving) -- see
	// config/substrate-integration/REPRODUCTION-GUIDE.md's Phase 4 for the
	// skopeo-to-containers-storage workaround this unblocks. Deliberately
	// NOT the new permanent default: for this sidecar's own normal
	// rebuild-push-redeploy workflow, IfNotPresent would make a node keep
	// reusing a stale cached :latest image instead of re-pulling after a
	// genuine rebuild, which is worse than today's one-off registry hiccup.
	bridgeImagePullPolicy := envOr("BRIDGE_IMAGE_PULL_POLICY", "")

	// vfioIOMMUGroups is hardware/BIOS-topology-specific -- the IOMMU
	// group a given GPU's PCI functions land in depends on the host's
	// motherboard/BIOS ACS settings, not on anything GNWES controls, so
	// it's an env var rather than a constant. Discover it per node via:
	//   for d in /sys/bus/pci/devices/<BDF>.*; do readlink -f $d/iommu_group; done
	// Empty by default: a cluster with no Exclusive-tier VFIO pools at all
	// (or one that hasn't set this yet) just never gets the annotation.
	vfioIOMMUGroups := parseCommaList(envOr("VFIO_IOMMU_GROUPS", ""))

	vgpuSchedulerName := envOr("VGPU_SCHEDULER_NAME", defaultVGPUSchedulerName)
	vgpuRuntimeClassName := envOr("VGPU_RUNTIME_CLASS_NAME", defaultVGPURuntimeClassName)

	// vgpuDebugHostMounts is host-filesystem-layout-specific (which distro,
	// which libc, where the vendor puts its driver tools), exactly like
	// vfioIOMMUGroups above, so it's an env var with NO default rather than
	// a guess baked in as a constant -- see vgpuDebugMountPatches's own doc
	// comment for what to put here and why. Empty by default: a cluster
	// that hasn't set this just never gets the extra mounts, same as
	// VFIO_IOMMU_GROUPS.
	vgpuDebugHostMounts := parseCommaList(envOr("VGPU_DEBUG_HOST_MOUNTS", ""))

	mux := http.NewServeMux()
	mux.HandleFunc("/mutate", func(w http.ResponseWriter, r *http.Request) {
		handleMutate(w, r, bridgeImage, bridgeImagePullPolicy, vfioIOMMUGroups, vgpuSchedulerName, vgpuRuntimeClassName, vgpuDebugHostMounts)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	srv := &http.Server{Addr: ":8443", Handler: mux}
	log.Printf("gnwes podgate-webhook listening on :8443 (cert=%s, bridgeImage=%s)", certFile, bridgeImage)
	log.Fatal(srv.ListenAndServeTLS(certFile, keyFile))
}

func handleMutate(w http.ResponseWriter, r *http.Request, bridgeImage, bridgeImagePullPolicy string, vfioIOMMUGroups []string, vgpuSchedulerName, vgpuRuntimeClassName string, vgpuDebugHostMounts []string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var review admissionv1.AdmissionReview
	if _, _, err := codecs.UniversalDeserializer().Decode(body, nil, &review); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	resp := &admissionv1.AdmissionResponse{UID: review.Request.UID, Allowed: true}

	var pod corev1.Pod
	if err := json.Unmarshal(review.Request.Object.Raw, &pod); err == nil {
		var patches []map[string]any

		if _, needsGating := pod.Labels[labelPoolRef]; needsGating {
			if !hasGate(&pod) {
				patches = append(patches, jsonPatch("add", "/spec/schedulingGates", []map[string]string{{"name": gateName}}))
				log.Printf("gating pod %s/%s (label %s=%s)", pod.Namespace, pod.Name, labelPoolRef, pod.Labels[labelPoolRef])
			}

			// vgpuPatches must be computed (and, if non-empty, applied)
			// before workerEnvPatches below: vgpuDisableEnvVars only needs
			// adding when this pod actually opted into a vGPU slice, and
			// workerEnvPatches is the ONE place that appends to the worker
			// container's env array, so it must see every env var this
			// request needs in one pass -- see workerEnvPatches's own doc
			// comment for why two independent append-tracking passes on
			// the same array would corrupt each other.
			var extraEnv []corev1.EnvVar
			if vgpuPatches, err := volcanoVGPUPatches(&pod, vgpuSchedulerName, vgpuRuntimeClassName); err != nil {
				log.Printf("not requesting a Volcano vGPU slice for pod %s/%s: %v", pod.Namespace, pod.Name, err)
			} else if len(vgpuPatches) > 0 {
				patches = append(patches, vgpuPatches...)
				extraEnv = vgpuDisableEnvVars()
				log.Printf("requesting a Volcano vGPU slice for pod %s/%s (%s=%s, %s=%s)",
					pod.Namespace, pod.Name, labelVGPUMemoryUnits, pod.Labels[labelVGPUMemoryUnits], labelVGPUCores, pod.Labels[labelVGPUCores])

				if debugPatches, err := vgpuDebugMountPatches(&pod, vgpuDebugHostMounts); err != nil {
					log.Printf("not injecting vGPU debug-tool mounts for pod %s/%s: %v", pod.Namespace, pod.Name, err)
				} else {
					patches = append(patches, debugPatches...)
				}
			}

			if envPatches, err := workerEnvPatches(&pod, extraEnv...); err != nil {
				log.Printf("not injecting worker env vars for pod %s/%s: %v", pod.Namespace, pod.Name, err)
			} else {
				patches = append(patches, envPatches...)
			}
		}

		if pod.Labels[labelBackend] == backendExclusive {
			atespace := pod.Labels[labelAtespace]
			if atespace == "" {
				log.Printf("pod %s/%s is %s=%s but has no %s label; not injecting the substrate-gpu-bridge sidecar",
					pod.Namespace, pod.Name, labelBackend, backendExclusive, labelAtespace)
			} else if sidecarPatches, err := exclusiveSidecarPatches(&pod, atespace, bridgeImage, bridgeImagePullPolicy, vfioIOMMUGroups); err != nil {
				log.Printf("not injecting substrate-gpu-bridge sidecar for pod %s/%s: %v", pod.Namespace, pod.Name, err)
			} else {
				patches = append(patches, sidecarPatches...)
				log.Printf("injecting substrate-gpu-bridge sidecar + shared %s volume into pod %s/%s", vcVMMountPath, pod.Namespace, pod.Name)
			}
		}

		if len(patches) > 0 {
			patchBytes, _ := json.Marshal(patches)
			pt := admissionv1.PatchTypeJSONPatch
			resp.Patch = patchBytes
			resp.PatchType = &pt
		}
	}

	review.Response = resp
	out, _ := json.Marshal(review)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

// podNameEnvVar is the Downward API env var the bridge sidecar needs for
// its -worker-pod flag -- the pod's own final name isn't known until after
// admission completes, so this is the only way it can learn it. The worker
// container no longer needs this: its volumeMount's SubPath is now the
// literal vcVMID generateVCVMID hands back below, not $(POD_NAME).
func podNameEnvVar() corev1.EnvVar {
	return corev1.EnvVar{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}
}

// findWorkerContainerIndex returns the index of pod's "worker" container
// (see workerContainerName's doc comment), or an error if none exists.
func findWorkerContainerIndex(pod *corev1.Pod) (int, error) {
	for i, c := range pod.Spec.Containers {
		if c.Name == workerContainerName {
			return i, nil
		}
	}
	return -1, fmt.Errorf("no container named %q found in pod spec", workerContainerName)
}

// gpuVisibilityEnvVars are the Downward-API env vars that let an actor's
// own code discover which GPU pool it belongs to, without needing a
// separate API call just to learn that much. Confirmed live (2026-11-15)
// as the fix for a real, concrete gap: nothing previously told an actor
// which pool it was in, even though its own pod's ServiceAccount already
// has (once granted -- see config/webhook/bridge-rbac.yaml) RBAC read
// access to that pool's real-time status (GPUWorkerPool.status /
// VFIOGPUPool.status). With GNWES_GPU_POOL in hand, an actor's own
// in-cluster Kubernetes client (every pod already mounts its own
// ServiceAccount token) can GET that object directly and read real
// total/available counts -- there was previously no way for an actor to
// learn even the pool's name, let alone its capacity.
func gpuVisibilityEnvVars() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "GNWES_GPU_POOL", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
			FieldPath: fmt.Sprintf("metadata.labels['%s']", labelPoolRef),
		}}},
		{Name: "GNWES_GPU_BACKEND", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
			FieldPath: fmt.Sprintf("metadata.labels['%s']", labelBackend),
		}}},
	}
}

// vgpuDisableEnvVars are required on the worker container whenever it
// requests a real Volcano vGPU slice (volcanoVGPUPatches) -- confirmed
// against this project's own real deployment (D:\codes\ui-ml-platform-
// postgres's api/gpu/gpu_backend.py, container_env()): the vGPU plugin's
// own hook library preloads into every process in the container, and
// without these set to "disabled" it can break the NVIDIA driver mount
// entirely ("Verified with a probe pod" per that reference implementation's
// own comment).
func vgpuDisableEnvVars() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "NVIDIA_GDS", Value: "disabled"},
		{Name: "NVIDIA_GDRCOPY", Value: "disabled"},
		{Name: "NVIDIA_MOFED", Value: "disabled"},
	}
}

// workerEnvPatches injects gpuVisibilityEnvVars (always, for any
// GNWES-gated pod, Shared or Exclusive tier alike) plus extra (e.g.
// vgpuDisableEnvVars, only when the pod opted into a Volcano vGPU slice)
// into the "worker" container's env array.
//
// This is the ONE place that appends to that array: both env-var sources
// must be combined into a single append-tracking pass here, not two
// separate functions each independently checking "does env already
// exist" -- if gpuVisibilityEnvVars's own patches ran first and decided
// the (originally empty) array needed a whole-array "add", a second,
// independently-tracked append for vgpuDisableEnvVars would either
// wrongly re-issue that same whole-array "add" (overwriting what the
// first write's patches, applied earlier in the same ordered JSON Patch
// list, will already have put there) or wrongly assume "/-" append
// semantics against what the ORIGINAL (pre-patch) pod spec showed, which
// is exactly the same cross-function append-tracking hazard
// exclusiveSidecarPatches's own volumesExist/mountsExist comments already
// warn about for volumes/mounts.
func workerEnvPatches(pod *corev1.Pod, extra ...corev1.EnvVar) ([]map[string]any, error) {
	workerIdx, err := findWorkerContainerIndex(pod)
	if err != nil {
		return nil, err
	}

	vars := append(gpuVisibilityEnvVars(), extra...)
	envPath := fmt.Sprintf("/spec/containers/%d/env", workerIdx)
	envExists := len(pod.Spec.Containers[workerIdx].Env) > 0
	var patches []map[string]any
	for _, e := range vars {
		if envExists {
			patches = append(patches, jsonPatch("add", envPath+"/-", e))
		} else {
			patches = append(patches, jsonPatch("add", envPath, []corev1.EnvVar{e}))
			envExists = true
		}
	}
	return patches, nil
}

// jsonPointerEscape escapes a string for use as one segment of a JSON
// Pointer path (RFC 6901: "~" -> "~0" first, then "/" -> "~1" -- in that
// order, since the "/" escape sequence itself contains a "~" that must
// not be re-escaped). Needed for resource keys like "volcano.sh/vgpu-number",
// which contain a literal "/".
func jsonPointerEscape(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	s = strings.ReplaceAll(s, "/", "~1")
	return s
}

// volcanoVGPUPatches builds the JSON Patch operations that request a real
// Volcano vGPU slice for a Shared-tier worker pod, confirmed live
// (2026-11-15) against this project's own real Volcano deployment (see
// labelVGPUMemoryUnits's doc comment). Opt-in via labelVGPUMemoryUnits +
// labelVGPUCores; returns nil, nil (not an error) when neither label is
// present -- this is the common case, every pod using the whole-physical-
// GPU DRA+CDI path instead.
//
// vgpu-memory is counted in this cluster's configured gpuMemoryFactor
// units (64 MiB each here, confirmed against dcn-gpu-0's real allocatable
// volcano.sh/vgpu-memory: 768 = 2 physical 24576 MiB GPUs / 64), not raw
// MiB -- labelVGPUMemoryUnits's value is passed straight through as that
// unit count, not converted.
func volcanoVGPUPatches(pod *corev1.Pod, vgpuSchedulerName, vgpuRuntimeClassName string) ([]map[string]any, error) {
	memUnits, hasMem := pod.Labels[labelVGPUMemoryUnits]
	cores, hasCores := pod.Labels[labelVGPUCores]
	if !hasMem && !hasCores {
		return nil, nil
	}
	if !hasMem || !hasCores {
		return nil, fmt.Errorf("pod carries only one of %s/%s; both are required together", labelVGPUMemoryUnits, labelVGPUCores)
	}

	workerIdx, err := findWorkerContainerIndex(pod)
	if err != nil {
		return nil, err
	}

	patches := []map[string]any{
		// "add" on an existing scalar field replaces it (RFC 6902) --
		// this pod's schedulerName is ordinarily "default-scheduler"
		// (Kubernetes' own default, always explicitly set once the
		// apiserver defaults it), confirmed live, not absent.
		jsonPatch("add", "/spec/schedulerName", vgpuSchedulerName),
		jsonPatch("add", "/spec/runtimeClassName", vgpuRuntimeClassName),
	}

	resourceValues := map[string]string{
		resourceVGPUNumber: "1",
		resourceVGPUMemory: memUnits,
		resourceVGPUCores:  cores,
	}
	container := pod.Spec.Containers[workerIdx]
	for _, section := range []struct {
		name   string
		exists bool
	}{
		{"limits", len(container.Resources.Limits) > 0},
		{"requests", len(container.Resources.Requests) > 0},
	} {
		base := fmt.Sprintf("/spec/containers/%d/resources/%s", workerIdx, section.name)
		if section.exists {
			// The map already has entries (confirmed live: this project's
			// own worker containers already carry real cpu/memory limits
			// and requests) -- add each key individually so they're
			// preserved, rather than overwriting the whole map.
			for _, key := range []string{resourceVGPUNumber, resourceVGPUMemory, resourceVGPUCores} {
				patches = append(patches, jsonPatch("add", base+"/"+jsonPointerEscape(key), resourceValues[key]))
			}
		} else {
			patches = append(patches, jsonPatch("add", base, resourceValues))
		}
	}

	return patches, nil
}

// vgpuDebugMountPatches bind-mounts a configurable list of host paths
// (each at the identical container path, read-only -- the same
// host-path-equals-container-path convention devVFIOPath already uses)
// into a Shared-tier worker container that just requested a real Volcano
// vGPU slice, so tools like nvidia-smi can actually run inside it.
//
// Confirmed live (2026-11-15), root-caused via crictl inspect + reading
// a container's own /dev and /lib through /proc/<pid>/root on the host
// (no in-container shell needed): this project's own vgpu-device-plugin
// already bind-mounts the real NVIDIA driver library stack
// (libnvidia-ml.so.1, libcuda.so.1, etc., matching the host's driver
// version) into the worker container, and real /dev/nvidia* device
// nodes ARE present and accessible -- GPU access itself already works.
// The ONLY missing piece is glibc (libc.so.6, ld-linux-x86-64.so.2, and a
// handful of other .so's) and the nvidia-smi binary itself: Agent
// Substrate's ateworker-gvisor image is a statically-linked Go binary
// with no userspace libraries of its own at all (confirmed: no shell, no
// coreutils, no libc anywhere in the image), so nothing in that image
// could run a dynamically-linked tool even once the driver's own
// libraries are already mounted in.
//
// Deliberately NOT a list of hardcoded paths: which libc, and where the
// distro puts it (glibc's multiarch path varies, musl is laid out
// differently again), is a property of the HOST, not of GNWES -- same
// reasoning as vfioIOMMUGroups. An operator enables this by setting
// VGPU_DEBUG_HOST_MOUNTS to nvidia-smi's own real path plus every path
// `ldd $(which nvidia-smi)` lists (see config/webhook/podgate-webhook.yaml
// for this project's own real, confirmed list on dcn-gpu-0). Returns
// nil, nil (not an error) when the list is empty -- the common case,
// since most clusters have no reason to debug inside the sandbox at all.
func vgpuDebugMountPatches(pod *corev1.Pod, hostPaths []string) ([]map[string]any, error) {
	if len(hostPaths) == 0 {
		return nil, nil
	}

	workerIdx, err := findWorkerContainerIndex(pod)
	if err != nil {
		return nil, err
	}

	// Fresh tracking, not shared with exclusiveSidecarPatches's own
	// volumesExist/mountsExist -- safe because a pod is never both
	// Backend=Exclusive and carrying the vGPU labels that lead here (see
	// volcanoVGPUPatches's own doc comment), so the two functions never
	// run against the same pod.
	volumesExist := len(pod.Spec.Volumes) > 0
	mountsExist := len(pod.Spec.Containers[workerIdx].VolumeMounts) > 0
	mountsPath := fmt.Sprintf("/spec/containers/%d/volumeMounts", workerIdx)

	var patches []map[string]any
	for i, hostPath := range hostPaths {
		volName := fmt.Sprintf("%s%d", vgpuDebugVolumeNamePrefix, i)
		v := corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: hostPath, Type: hostPathTypePtr(corev1.HostPathFile)},
			},
		}
		if volumesExist {
			patches = append(patches, jsonPatch("add", "/spec/volumes/-", v))
		} else {
			patches = append(patches, jsonPatch("add", "/spec/volumes", []corev1.Volume{v}))
			volumesExist = true
		}

		m := corev1.VolumeMount{Name: volName, MountPath: hostPath, ReadOnly: true}
		if mountsExist {
			patches = append(patches, jsonPatch("add", mountsPath+"/-", m))
		} else {
			patches = append(patches, jsonPatch("add", mountsPath, []corev1.VolumeMount{m}))
			mountsExist = true
		}
	}
	return patches, nil
}

// generateVCVMID returns a short random token identifying this one pod's
// host-side api-socket subdirectory -- deliberately NOT the pod's own name
// (see substratebridge.HostAPISocketPath's doc comment for why: the real
// name is both unknown at this point -- Agent Substrate's WorkerPool
// controller creates these pods via GenerateName, so the apiserver only
// assigns the final name and UID *after* mutating webhooks like this one
// run -- and, even where it is known, routinely too long once combined
// with GNWESHostVCVMDir and an actor UID to fit a unix socket path's
// 108-byte limit). 5 random bytes (10 hex chars) is plenty to keep
// concurrently-admitted exclusive-tier pods from colliding.
func generateVCVMID() (string, error) {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating vcvm id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// exclusiveSidecarPatches builds the JSON Patch operations that add: a
// hostPath volume at substratebridge.GNWESHostVCVMDir, a per-pod
// subdirectory of it (via a literal SubPath, generateVCVMID's result)
// mounted into the real "worker" container at /run/vc/vm -- so
// ateworker-microvm's cloud-hypervisor api-sockets land somewhere
// cmd/manager can reach from outside the pod, see
// substratebridge.HostAPISocketPath -- and a substrate-gpu-bridge sidecar
// container (no mount needed: it only ever writes that same HOST path as a
// string, never opens the socket itself) watching this exact pod, via the
// Downward API for its real name and this same literal vcVMID for the
// socket path, for whichever actor Substrate currently assigns to it.
func exclusiveSidecarPatches(pod *corev1.Pod, atespace, bridgeImage, bridgeImagePullPolicy string, vfioIOMMUGroups []string) ([]map[string]any, error) {
	workerIdx, err := findWorkerContainerIndex(pod)
	if err != nil {
		return nil, err
	}

	vcVMID, err := generateVCVMID()
	if err != nil {
		return nil, err
	}

	var patches []map[string]any

	// volumesExist tracks whether /spec/volumes is already a non-empty array
	// AS SEEN BY THE NEXT PATCH IN THIS SEQUENCE -- not just the original
	// pod's state. JSON Patch ops apply in order against the result of the
	// previous one, so once this function's own first "add" has created the
	// array, every later volume addition here must append via ".../-"
	// rather than re-testing the pod's original (still-empty) Volumes and
	// wrongly re-issuing a whole-array "add" that would overwrite it.
	volumesExist := len(pod.Spec.Volumes) > 0
	addVolume := func(v corev1.Volume) {
		if volumesExist {
			patches = append(patches, jsonPatch("add", "/spec/volumes/-", v))
		} else {
			patches = append(patches, jsonPatch("add", "/spec/volumes", []corev1.Volume{v}))
			volumesExist = true
		}
	}

	// mountsExist is the same tracking, for the worker container's own
	// volumeMounts array specifically.
	mountsExist := len(pod.Spec.Containers[workerIdx].VolumeMounts) > 0
	mountsPath := fmt.Sprintf("/spec/containers/%d/volumeMounts", workerIdx)
	addWorkerMount := func(m corev1.VolumeMount) {
		if mountsExist {
			patches = append(patches, jsonPatch("add", mountsPath+"/-", m))
		} else {
			patches = append(patches, jsonPatch("add", mountsPath, []corev1.VolumeMount{m}))
			mountsExist = true
		}
	}

	addVolume(corev1.Volume{
		Name: vcVMVolumeName,
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: substratebridge.GNWESHostVCVMDir,
				Type: hostPathTypePtr(corev1.HostPathDirectoryOrCreate),
			},
		},
	})
	addWorkerMount(corev1.VolumeMount{Name: vcVMVolumeName, MountPath: vcVMMountPath, SubPath: vcVMID})

	// /dev/vfio: cloud-hypervisor (running as the "worker" container's own
	// process, not inside its Kata guest) needs this host device directory
	// visible in its own mount namespace to service vm.add-device at all --
	// confirmed live (2026-11-13): without it, a hot-add that successfully
	// reaches cloud-hypervisor's real API still fails with "failed to open
	// /dev/vfio/vfio container: No such file or directory", even though the
	// worker container already runs as root with a broad capability set
	// (SYS_ADMIN, MKNOD, DAC_OVERRIDE, FOWNER, ...) from agent-substrate's
	// own pod template. The real per-IOMMU-group device node (e.g.
	// /dev/vfio/<group>) is created dynamically by the host kernel once a
	// device is bound to vfio-pci and isn't known in advance, so this mounts
	// the whole directory (host confirmed /dev/vfio/vfio itself is 0666,
	// world-rw) rather than a specific device file.
	addVolume(corev1.Volume{
		Name: devVFIOVolumeName,
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: devVFIOPath,
				Type: hostPathTypePtr(corev1.HostPathDirectory),
			},
		},
	})
	addWorkerMount(corev1.VolumeMount{Name: devVFIOVolumeName, MountPath: devVFIOPath})

	// privileged:true was tried and reverted (2026-11-13): it does lift the
	// device-cgroup restriction blocking /dev/vfio/vfio (EPERM otherwise),
	// but agent-substrate's own ateworker-microvm binary refuses to start
	// under it -- confirmed live, its own startup check detects it's
	// sharing the host's cgroup namespace (a side effect of running
	// privileged under CRI-O) and aborts with "while delegating the worker
	// cgroup: worker needs a private cgroup namespace ... workers must run
	// unprivileged". So privileged and VFIO-device access are mutually
	// exclusive for this specific workload, not just a security tradeoff.
	// Fixed instead (2026-11-14) via the CRI-O io.kubernetes.cri-o.Devices
	// annotation below, once the worker container's own runtime handler
	// (confirmed via `crictl inspect` + no runtimeClassName on any pod on
	// this cluster -> CRI-O's resolved default_runtime, "runc") has that
	// annotation (and the specific device paths) allow-listed in its own
	// crio.conf.d config -- see config/vmgpu/README.md.
	if len(vfioIOMMUGroups) > 0 {
		annotationsExist := len(pod.Annotations) > 0
		addAnnotation := func(key, value string) {
			if annotationsExist {
				patches = append(patches, jsonPatch("add", "/metadata/annotations/"+key, value))
			} else {
				patches = append(patches, jsonPatch("add", "/metadata/annotations", map[string]string{key: value}))
				annotationsExist = true
			}
		}
		addAnnotation(vfioDevicesAnnotation, vfioDevicesAnnotationValue(vfioIOMMUGroups))
	}

	sidecar := corev1.Container{
		Name:  bridgeContainerName,
		Image: bridgeImage,
		Args: []string{
			// Matches cmd/substrate-gpu-bridge/Dockerfile's COPY destination
			// for the cross-compiled kubectl-ate-linux binary -- the base
			// image is distroless/static, which sets no PATH for the bare
			// "kubectl-ate" the flag defaults to otherwise.
			"-kubectl-ate-bin=/usr/local/bin/kubectl-ate",
			"-atespace=" + atespace,
			"-worker-pod=$(POD_NAME)",
			"-vcvm-id=" + vcVMID,
			"-attachment-namespace=$(POD_NAMESPACE)",
		},
		Env: []corev1.EnvVar{
			podNameEnvVar(),
			{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
		},
	}
	if bridgeImagePullPolicy != "" {
		sidecar.ImagePullPolicy = corev1.PullPolicy(bridgeImagePullPolicy)
	}
	patches = append(patches, jsonPatch("add", "/spec/containers/-", sidecar))

	return patches, nil
}

// vfioDevicesAnnotationValue builds the io.kubernetes.cri-o.Devices
// annotation value: a comma-separated list of "$HOSTPATH:$CONTAINERPATH:$MODE"
// entries (confirmed against CRI-O's pkg/annotations doc comments). Always
// includes the one shared /dev/vfio/vfio control device, plus one entry per
// IOMMU group device node (/dev/vfio/<group>).
func vfioDevicesAnnotationValue(iommuGroups []string) string {
	devices := []string{vfioContainerDevice + ":" + vfioContainerDevice + ":rwm"}
	for _, g := range iommuGroups {
		p := "/dev/vfio/" + g
		devices = append(devices, p+":"+p+":rwm")
	}
	return strings.Join(devices, ",")
}

func parseCommaList(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func hostPathTypePtr(t corev1.HostPathType) *corev1.HostPathType { return &t }

func jsonPatch(op, path string, value any) map[string]any {
	return map[string]any{"op": op, "path": path, "value": value}
}

func hasGate(pod *corev1.Pod) bool {
	for _, g := range pod.Spec.SchedulingGates {
		if g.Name == gateName {
			return true
		}
	}
	return false
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
