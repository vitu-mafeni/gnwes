package main

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// nvidiaBackend discovers real NVIDIA GPUs via nvidia-smi. It prepares them
// for containers one of two ways, chosen by NVIDIA_CDI_MODE:
//
//   - "generate" (default) — calls `nvidia-ctk cdi generate` itself to write
//     a fresh CDI spec, for a bare-metal node with the NVIDIA driver and
//     nvidia-container-toolkit installed but no GPU-management operator.
//   - "existing" — reuses a CDI spec some other, already-running system
//     manages (e.g. NVIDIA GPU Operator's own device plugin, which writes a
//     fully correct, hook-laden spec to /var/run/cdi as part of normal
//     operation on any node it manages). This avoids generating a second,
//     redundant spec next to one already proven working, and means this
//     backend needs no nvidia-ctk access at all in that mode.
//
// Either way, device discovery (nvidia-smi) and the DRA/CDI mechanism
// (ResourceSlice, kubelet plugin protocol, CDI device ID handed to
// kubelet/containerd-or-CRI-O) are unchanged — see config/dra/README.md's
// "Running on real GPU nodes" section for prerequisites and how to verify.
type nvidiaBackend struct {
	nvidiaSMIPath string
	nvidiaCTKPath string
	cdiSpecPath   string
	cdiMode       string // "generate" or "existing"
	existingSpec  string
	existingKind  string

	mu         sync.Mutex
	generated  bool              // "generate" mode: whether this process has (re)generated the spec yet
	uuidByName map[string]string // DRA device name (e.g. "gpu-0") -> real GPU UUID, populated by Devices()
}

func newNvidiaBackend() *nvidiaBackend {
	return &nvidiaBackend{
		nvidiaSMIPath: envOr("NVIDIA_SMI_PATH", "nvidia-smi"),
		nvidiaCTKPath: envOr("NVIDIA_CTK_PATH", "nvidia-ctk"),
		cdiSpecPath:   envOr("NVIDIA_CDI_SPEC_PATH", "/etc/cdi/nvidia.json"),
		cdiMode:       envOr("NVIDIA_CDI_MODE", "generate"),
		existingSpec:  envOr("NVIDIA_CDI_EXISTING_SPEC", "/var/run/cdi/k8s.device-plugin.nvidia.com-gpu.json"),
		existingKind:  envOr("NVIDIA_CDI_KIND", "k8s.device-plugin.nvidia.com/gpu"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func (b *nvidiaBackend) Name() string {
	if b.cdiMode == "existing" {
		return fmt.Sprintf("nvidia (real GPU via %s, reusing externally-managed CDI spec %s)", b.nvidiaSMIPath, b.existingSpec)
	}
	return fmt.Sprintf("nvidia (real GPU via %s + %s)", b.nvidiaSMIPath, b.nvidiaCTKPath)
}

// nvidiaGPU is one row of `nvidia-smi --query-gpu=index,uuid,name,memory.total
// --format=csv,noheader,nounits` output.
type nvidiaGPU struct {
	index  string
	uuid   string
	model  string
	memMiB int64
}

func (b *nvidiaBackend) Devices() ([]resourceapi.Device, error) {
	gpus, err := b.queryGPUs()
	if err != nil {
		return nil, err
	}
	if len(gpus) == 0 {
		return nil, fmt.Errorf("nvidia-smi reported zero GPUs on this node")
	}

	b.mu.Lock()
	b.uuidByName = make(map[string]string, len(gpus))
	devices := make([]resourceapi.Device, len(gpus))
	for i, g := range gpus {
		name := fmt.Sprintf("gpu-%s", g.index)
		b.uuidByName[name] = g.uuid
		model, uuid := g.model, g.uuid
		devices[i] = resourceapi.Device{
			Name: name,
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"uuid":  {StringValue: &uuid},
				"model": {StringValue: &model},
			},
			Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
				"memory": {Value: *resource.NewQuantity(g.memMiB*1024*1024, resource.BinarySI)},
			},
		}
	}
	b.mu.Unlock()
	return devices, nil
}

func (b *nvidiaBackend) queryGPUs() ([]nvidiaGPU, error) {
	out, err := exec.Command(b.nvidiaSMIPath, "--query-gpu=index,uuid,name,memory.total", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, fmt.Errorf("running %s: %w (is the NVIDIA driver installed on this node, and %s reachable inside this container?)", b.nvidiaSMIPath, err, b.nvidiaSMIPath)
	}
	return parseNvidiaSMIOutput(string(out))
}

// parseNvidiaSMIOutput parses `nvidia-smi --query-gpu=index,uuid,name,memory.total
// --format=csv,noheader,nounits` output. Split out as a pure function so the
// parsing logic has test coverage without needing real GPU hardware to run
// nvidia-smi against — queryGPUs itself (the exec.Command call) is not unit
// tested for the same reason the rest of this backend isn't: there is no
// NVIDIA GPU in this development environment.
func parseNvidiaSMIOutput(output string) ([]nvidiaGPU, error) {
	var gpus []nvidiaGPU
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) != 4 {
			return nil, fmt.Errorf("unexpected nvidia-smi output line %q: want 4 comma-separated fields, got %d", line, len(fields))
		}
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		memMiB, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parsing memory.total %q: %w", fields[3], err)
		}
		gpus = append(gpus, nvidiaGPU{
			index:  fields[0],
			uuid:   fields[1],
			model:  fields[2],
			memMiB: memMiB,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return gpus, nil
}

// ensureCDISpec generates the real CDI spec covering every GPU on this node,
// once per process, using nvidia-ctk itself rather than hand-writing one —
// used only in "generate" mode.
func (b *nvidiaBackend) ensureCDISpec() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.generated {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(b.cdiSpecPath), 0o755); err != nil {
		return fmt.Errorf("creating CDI spec directory: %w", err)
	}
	cmd := exec.Command(b.nvidiaCTKPath, "cdi", "generate", "--output="+b.cdiSpecPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("running %s cdi generate: %w: %s", b.nvidiaCTKPath, err, string(out))
	}
	log.Printf("generated real CDI spec at %s via %s:\n%s", b.cdiSpecPath, b.nvidiaCTKPath, string(out))
	b.generated = true
	return nil
}

func (b *nvidiaBackend) Prepare(deviceName, claimUID string) (string, error) {
	if b.cdiMode == "existing" {
		return b.prepareExisting(deviceName)
	}
	return b.prepareGenerated(deviceName)
}

func (b *nvidiaBackend) prepareGenerated(deviceName string) (string, error) {
	if err := b.ensureCDISpec(); err != nil {
		return "", err
	}
	// nvidia-ctk's generated spec names devices by index (and also by UUID,
	// and "all") under kind "nvidia.com/gpu" — that kind/vendor is fixed by
	// nvidia-ctk itself, independent of this driver's own DRA driver name.
	index := strings.TrimPrefix(deviceName, "gpu-")
	return fmt.Sprintf("nvidia.com/gpu=%s", index), nil
}

func (b *nvidiaBackend) prepareExisting(deviceName string) (string, error) {
	b.mu.Lock()
	uuid, ok := b.uuidByName[deviceName]
	b.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("no known GPU UUID for device %q (Devices() not called yet, or name mismatch)", deviceName)
	}

	// Sanity-check the externally-managed spec is actually there and really
	// defines this device, rather than silently trusting a path that might
	// be stale, empty, or belong to a differently-configured node.
	data, err := os.ReadFile(b.existingSpec)
	if err != nil {
		return "", fmt.Errorf("reading externally-managed CDI spec %s: %w", b.existingSpec, err)
	}
	if !specDefinesDevice(data, uuid) {
		return "", fmt.Errorf("externally-managed CDI spec %s has no device named %s", b.existingSpec, uuid)
	}

	return fmt.Sprintf("%s=%s", b.existingKind, uuid), nil
}

// specDefinesDevice reports whether specJSON (an already-generated CDI spec
// file's raw bytes) defines a device with the given CDI device name. A
// lightweight sanity check, not a full JSON parse, so this backend doesn't
// need a dependency on whatever CDI spec-go version the external tool that
// wrote the file happened to use.
func specDefinesDevice(specJSON []byte, deviceName string) bool {
	return bytes.Contains(specJSON, []byte(`"name":"`+deviceName+`"`))
}

func (b *nvidiaBackend) Unprepare(deviceName, claimUID string) {
	// Both modes' CDI specs cover every GPU on the node and are shared
	// across all claims; Prepare created nothing claim-scoped, so there's
	// nothing to clean up here (unlike mockBackend's one-spec-per-claim
	// transient file).
}
