package main

import (
	"bufio"
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

// nvidiaBackend discovers real NVIDIA GPUs via nvidia-smi and prepares them
// for containers via nvidia-ctk (NVIDIA Container Toolkit's own CDI
// generator) — the same tools a real NVIDIA device plugin / GPU Operator
// deployment uses, instead of a GNWES-authored CDI spec. It requires the
// NVIDIA driver and nvidia-container-toolkit to already be installed on the
// node, and nvidia-smi/nvidia-ctk to be reachable inside this container
// (normally by bind-mounting them in from the host — see
// config/dra/driver-real-gpu.yaml and that directory's README for exactly
// what's required and how to verify it on real hardware).
type nvidiaBackend struct {
	nvidiaSMIPath string
	nvidiaCTKPath string
	cdiSpecPath   string

	mu        sync.Mutex
	generated bool // whether this process has (re)generated the CDI spec yet
}

func newNvidiaBackend() *nvidiaBackend {
	return &nvidiaBackend{
		nvidiaSMIPath: envOr("NVIDIA_SMI_PATH", "nvidia-smi"),
		nvidiaCTKPath: envOr("NVIDIA_CTK_PATH", "nvidia-ctk"),
		cdiSpecPath:   envOr("NVIDIA_CDI_SPEC_PATH", "/etc/cdi/nvidia.json"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func (b *nvidiaBackend) Name() string {
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

	devices := make([]resourceapi.Device, len(gpus))
	for i, g := range gpus {
		model, uuid := g.model, g.uuid
		devices[i] = resourceapi.Device{
			Name: fmt.Sprintf("gpu-%s", g.index),
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"uuid":  {StringValue: &uuid},
				"model": {StringValue: &model},
			},
			Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
				"memory": {Value: *resource.NewQuantity(g.memMiB*1024*1024, resource.BinarySI)},
			},
		}
	}
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
// this is the exact tool NVIDIA's own device plugin and GPU Operator use.
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
	if err := b.ensureCDISpec(); err != nil {
		return "", err
	}
	// nvidia-ctk's generated spec names devices by index (and also by UUID,
	// and "all") under kind "nvidia.com/gpu" — that kind/vendor is fixed by
	// nvidia-ctk itself, independent of this driver's own DRA driver name.
	index := strings.TrimPrefix(deviceName, "gpu-")
	return fmt.Sprintf("nvidia.com/gpu=%s", index), nil
}

func (b *nvidiaBackend) Unprepare(deviceName, claimUID string) {
	// The generated CDI spec covers every GPU on the node and is shared
	// across all claims; Prepare created nothing claim-scoped, so there's
	// nothing to clean up here (unlike mockBackend's one-spec-per-claim
	// transient file).
}
