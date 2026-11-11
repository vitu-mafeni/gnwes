package main

import (
	"fmt"

	resourceapi "k8s.io/api/resource/v1"
	cdi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

// gpuBackend abstracts how devices are discovered and how a device is made
// ready for a container (its CDI spec). Two implementations exist:
// mockBackend below (a fake device, for environments with no GPU hardware —
// this is the default, and what every other part of this demo was built and
// verified against) and nvidiaBackend (nvidia.go — real GPUs via nvidia-smi
// + nvidia-ctk, for running this same DRA/CDI mechanism on a real GPU node).
// Selected in main.go via the GNWES_GPU_BACKEND env var.
type gpuBackend interface {
	// Name identifies the backend, for the startup log line only.
	Name() string
	// Devices returns the devices to publish in this node's ResourceSlice.
	Devices() ([]resourceapi.Device, error)
	// Prepare ensures deviceName has a usable CDI spec on this node and
	// returns the CDI device ID kubelet/containerd should inject into the
	// container of the pod that claimUID's claim was allocated to.
	Prepare(deviceName, claimUID string) (cdiDeviceID string, err error)
	// Unprepare releases any claim-scoped state Prepare created. Safe to
	// call even if Prepare was never called for this (deviceName, claimUID).
	Unprepare(deviceName, claimUID string)
}

const (
	numMockDevices = 4
	mockCDIVendor  = "gnwes.io"
	mockCDIClass   = "mockgpu"
)

// mockBackend is the original simulated device: no real GPU, no real driver,
// no shelling out to any external tool — just a CDI spec whose only edit is
// an environment variable, clearly labeled as a stand-in. This is the path
// every other README/RUNBOOK claim in this repo about config/dra was
// verified against.
type mockBackend struct{}

func (b *mockBackend) Name() string { return "mock (no real GPU hardware)" }

func (b *mockBackend) Devices() ([]resourceapi.Device, error) {
	devices := make([]resourceapi.Device, numMockDevices)
	for i := range devices {
		devices[i] = resourceapi.Device{Name: fmt.Sprintf("mock-gpu-%d", i)}
	}
	return devices, nil
}

func (b *mockBackend) Prepare(deviceName, claimUID string) (string, error) {
	if err := b.writeCDISpec(deviceName, claimUID); err != nil {
		return "", fmt.Errorf("writing CDI spec for %s: %w", deviceName, err)
	}
	return fmt.Sprintf("%s/%s=%s", mockCDIVendor, mockCDIClass, deviceName), nil
}

func (b *mockBackend) Unprepare(deviceName, claimUID string) {
	if err := cdi.GetDefaultCache().RemoveSpec(cdi.GenerateTransientSpecName(mockCDIVendor, mockCDIClass, claimUID)); err != nil {
		// Non-fatal: logged by the caller, which also owns claim bookkeeping.
		_ = err
	}
}

// writeCDISpec writes a real CDI spec file for deviceName. The only thing
// mocked here is the edit itself (an env var, not a real device node) —
// writing the file, naming it uniquely per claim (per CDI's own guidance to
// avoid stale container-runtime cache hits), and registering it in a
// directory containerd actually watches (/etc/cdi, see config/dra/README.md)
// are all real.
func (b *mockBackend) writeCDISpec(deviceName, claimUID string) error {
	spec := &cdispec.Spec{
		Version: cdispec.CurrentVersion,
		Kind:    mockCDIVendor + "/" + mockCDIClass,
		Devices: []cdispec.Device{{
			Name: deviceName,
			ContainerEdits: cdispec.ContainerEdits{
				Env: []string{
					"GNWES_MOCK_GPU_SLOT=" + deviceName,
					"GNWES_MOCK_GPU_NOTE=stand-in-device-not-real-nvidia-hardware",
				},
			},
		}},
	}
	name := cdi.GenerateTransientSpecName(mockCDIVendor, mockCDIClass, claimUID)
	return cdi.GetDefaultCache().WriteSpec(spec, name+".json")
}
