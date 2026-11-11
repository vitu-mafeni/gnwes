// Package vfiopool is the in-memory source of truth for real, physical VFIO
// (GPU) devices available for live hot-attach/detach into a running
// cloud-hypervisor guest, mirroring internal/pool's Acquire/Release/Transfer
// shape (same race-free-under-concurrent-reconciles property, verified the
// same way: see manager_test.go's TestConcurrentAcquireReleaseNeverDoubleBooks)
// but for real hardware instead of simulated slots.
//
// The difference from internal/pool is deliberate: a simulated GPU slot is
// manufactured on demand (Resize creates it, paying a simulated init cost);
// a real VFIO device is physical hardware that either exists on a node or
// doesn't, so membership here is static (RegisterDevice), not elastic.
package vfiopool

import (
	"fmt"
	"sync"
)

// DeviceState is the lifecycle state of one real VFIO device.
type DeviceState string

const (
	DeviceAvailable DeviceState = "Available"
	DeviceAttached  DeviceState = "Attached"
)

// Device is one real, physical VFIO-bound GPU known to this pool.
type Device struct {
	ID        string // stable logical name, e.g. "gpu-0"
	PCIPath   string // host sysfs path, e.g. "/sys/bus/pci/devices/0000:01:00.0/"
	State     DeviceState
	AttachKey string // namespace/name of the VMGPUAttachment currently holding it
	GuestBDF  string // PCI address inside the guest, once AddDevice confirms it
}

// Pool tracks the real VFIO devices available on a single node.
type Pool struct {
	mu      sync.Mutex
	Name    string
	devices map[string]*Device
}

// Manager holds one Pool per node (or other grouping key the caller picks),
// keyed by whatever string the caller uses consistently (e.g. node name).
type Manager struct {
	mu    sync.Mutex
	pools map[string]*Pool
}

func NewManager() *Manager {
	return &Manager{pools: make(map[string]*Pool)}
}

// EnsurePool returns the Pool for key, creating it (with no devices) if it
// does not exist yet.
func (m *Manager) EnsurePool(key string) *Pool {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pools[key]
	if !ok {
		p = &Pool{Name: key, devices: make(map[string]*Device)}
		m.pools[key] = p
	}
	return p
}

// GetPool returns the Pool for key, or nil if it hasn't been created yet.
func (m *Manager) GetPool(key string) *Pool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pools[key]
}

// DeletePool removes a pool's in-memory state entirely.
func (m *Manager) DeletePool(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pools, key)
}

// RegisterDevice adds a real device to the pool, available for attachment.
// A no-op if a device with this ID is already registered (idempotent against
// repeat discovery/reconciles).
func (p *Pool) RegisterDevice(id, pciPath string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.devices[id]; exists {
		return
	}
	p.devices[id] = &Device{ID: id, PCIPath: pciPath, State: DeviceAvailable}
}

// UnregisterDevice removes a device from the pool entirely (e.g. the node
// reported it's gone). Refuses to remove one that's currently attached --
// callers must detach first, same spirit as internal/pool never deleting a
// Leased slot out from under its lease.
func (p *Pool) UnregisterDevice(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	d, ok := p.devices[id]
	if !ok {
		return nil
	}
	if d.State == DeviceAttached {
		return fmt.Errorf("device %q is still attached (held by %q); detach it first", id, d.AttachKey)
	}
	delete(p.devices, id)
	return nil
}

// Acquire claims one Available device for attachKey. Returns ok=false if
// none are free right now.
func (p *Pool) Acquire(attachKey string) (device Device, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range p.devices {
		if d.State == DeviceAvailable {
			d.State = DeviceAttached
			d.AttachKey = attachKey
			return *d, true
		}
	}
	return Device{}, false
}

// AcquireSpecific claims device id for attachKey if it's Available.
// Returns ok=false if it doesn't exist or isn't Available.
func (p *Pool) AcquireSpecific(id, attachKey string) (device Device, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d, exists := p.devices[id]
	if !exists || d.State != DeviceAvailable {
		return Device{}, false
	}
	d.State = DeviceAttached
	d.AttachKey = attachKey
	return *d, true
}

// SetGuestBDF records the guest-side PCI address cloud-hypervisor reported
// after a successful hot add. No-op if the device isn't held by attachKey
// (stale call after a release/re-acquire elsewhere).
func (p *Pool) SetGuestBDF(id, attachKey, bdf string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d, ok := p.devices[id]; ok && d.State == DeviceAttached && d.AttachKey == attachKey {
		d.GuestBDF = bdf
	}
}

// Release returns an attached device to Available. No-op if the device
// isn't currently held by attachKey (idempotent against repeat reconciles).
func (p *Pool) Release(id, attachKey string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d, ok := p.devices[id]; ok && d.State == DeviceAttached && d.AttachKey == attachKey {
		d.State = DeviceAvailable
		d.AttachKey = ""
		d.GuestBDF = ""
	}
}

// Get returns a copy of device id's current state, or ok=false if unknown.
func (p *Pool) Get(id string) (device Device, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d, exists := p.devices[id]
	if !exists {
		return Device{}, false
	}
	return *d, true
}

// Counts returns (total, available, attached) device counts.
func (p *Pool) Counts() (total, available, attached int32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range p.devices {
		total++
		switch d.State {
		case DeviceAvailable:
			available++
		case DeviceAttached:
			attached++
		}
	}
	return
}
