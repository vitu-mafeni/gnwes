// Command mock-gpu-driver is a real Kubernetes DRA (Dynamic Resource
// Allocation) kubelet plugin. It registers with kubelet, publishes a fixed
// number of devices per node via a real ResourceSlice, and on
// PrepareResourceClaims (the gRPC NodePrepareResources call) writes a real
// CDI (Container Device Interface) spec file and returns its CDI device ID,
// which kubelet/containerd then actually inject into the container.
//
// Only the device itself is fake: there is no real NVIDIA GPU behind it.
// Everything else here — ResourceSlice publication, the kubelet plugin
// registration handshake, PrepareResourceClaims/UnprepareResourceClaims, the
// CDI spec file, and its injection into the container — is the real
// mechanism, built on the same k8s.io/dynamic-resource-allocation helper
// library and tags.cncf.io/container-device-interface spec types a real GPU
// vendor driver would use.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	cdi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

const (
	driverName  = "mock-gpu.gnwes.io"
	cdiVendor   = "gnwes.io"
	cdiClass    = "mockgpu"
	numDevices  = 4
	poolNameFmt = "mock-gpu-%s" // formatted with node name
)

func main() {
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		log.Fatal("NODE_NAME env var is required")
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("in-cluster config: %v", err)
	}
	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("kube client: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// kubeletplugin.Start requires this directory to already exist; the
	// kubelet only pre-creates /var/lib/kubelet/plugins itself, not our
	// driver-specific subdirectory under it.
	pluginDir := kubeletplugin.KubeletPluginsDir + "/" + driverName
	if err := os.MkdirAll(pluginDir, 0o750); err != nil {
		log.Fatalf("creating plugin directory %s: %v", pluginDir, err)
	}

	plugin := &driver{nodeName: nodeName, prepared: map[types.UID]string{}}

	helper, err := kubeletplugin.Start(
		ctx,
		plugin,
		kubeletplugin.DriverName(driverName),
		kubeletplugin.NodeName(nodeName),
		kubeletplugin.KubeClient(kubeClient),
	)
	if err != nil {
		log.Fatalf("starting kubelet plugin: %v", err)
	}
	plugin.helper = helper

	devices := make([]resourceapi.Device, numDevices)
	for i := range devices {
		devices[i] = resourceapi.Device{Name: fmt.Sprintf("mock-gpu-%d", i)}
	}
	resources := resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			fmt.Sprintf(poolNameFmt, nodeName): {
				Slices: []resourceslice.Slice{{Devices: devices}},
			},
		},
	}
	if err := helper.PublishResources(ctx, resources); err != nil {
		log.Fatalf("publishing ResourceSlice: %v", err)
	}

	log.Printf("mock-gpu-driver ready on node %s, published %d devices", nodeName, numDevices)
	<-ctx.Done()
	log.Printf("shutting down")
}

// driver implements kubeletplugin.DRAPlugin.
type driver struct {
	nodeName string
	helper   *kubeletplugin.Helper

	mu       sync.Mutex
	prepared map[types.UID]string // claim UID -> device name, for idempotent unprepare
}

func (d *driver) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	result := make(map[types.UID]kubeletplugin.PrepareResult, len(claims))

	for _, claim := range claims {
		if claim.Status.Allocation == nil {
			result[claim.UID] = kubeletplugin.PrepareResult{Err: fmt.Errorf("claim %s/%s has no allocation", claim.Namespace, claim.Name)}
			continue
		}

		var devices []kubeletplugin.Device
		for _, alloc := range claim.Status.Allocation.Devices.Results {
			if alloc.Driver != driverName {
				continue
			}

			deviceName := alloc.Device
			cdiDeviceID := fmt.Sprintf("%s/%s=%s", cdiVendor, cdiClass, deviceName)

			if err := d.writeCDISpec(deviceName, string(claim.UID)); err != nil {
				result[claim.UID] = kubeletplugin.PrepareResult{Err: fmt.Errorf("writing CDI spec for %s: %w", deviceName, err)}
				continue
			}

			d.mu.Lock()
			d.prepared[claim.UID] = deviceName
			d.mu.Unlock()

			devices = append(devices, kubeletplugin.Device{
				Requests:     []string{alloc.Request},
				PoolName:     alloc.Pool,
				DeviceName:   deviceName,
				CDIDeviceIDs: []string{cdiDeviceID},
			})
			log.Printf("prepared claim %s/%s: device=%s cdi=%s", claim.Namespace, claim.Name, deviceName, cdiDeviceID)
		}

		if _, failed := result[claim.UID]; !failed {
			result[claim.UID] = kubeletplugin.PrepareResult{Devices: devices}
		}
	}

	return result, nil
}

func (d *driver) UnprepareResourceClaims(ctx context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	result := make(map[types.UID]error, len(claims))
	for _, claim := range claims {
		d.mu.Lock()
		deviceName, ok := d.prepared[claim.UID]
		delete(d.prepared, claim.UID)
		d.mu.Unlock()

		if ok {
			if err := cdi.GetDefaultCache().RemoveSpec(cdi.GenerateTransientSpecName(cdiVendor, cdiClass, string(claim.UID))); err != nil {
				log.Printf("removing CDI spec for claim %s: %v (non-fatal)", claim.UID, err)
			}
			log.Printf("unprepared claim %s/%s: released device=%s", claim.Namespace, claim.Name, deviceName)
		}
		result[claim.UID] = nil
	}
	return result, nil
}

func (d *driver) HandleError(ctx context.Context, err error, msg string) {
	log.Printf("background error: %s: %v", msg, err)
}

func (d *driver) WatchHealthStatus(ctx context.Context, reports chan<- kubeletplugin.DeviceHealthReport) error {
	return kubeletplugin.ErrHealthNotSupported
}

// writeCDISpec writes a real CDI spec file for deviceName. The only thing
// mocked here is the edit itself (an env var, not a real device node) —
// writing the file, naming it uniquely per claim (per CDI's own guidance to
// avoid stale container-runtime cache hits), and registering it in a
// directory containerd actually watches (/etc/cdi, see config/dra/README.md)
// are all real.
func (d *driver) writeCDISpec(deviceName, claimUID string) error {
	spec := &cdispec.Spec{
		Version: cdispec.CurrentVersion,
		Kind:    cdiVendor + "/" + cdiClass,
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
	name := cdi.GenerateTransientSpecName(cdiVendor, cdiClass, claimUID)
	return cdi.GetDefaultCache().WriteSpec(spec, name+".json")
}
