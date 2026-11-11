package vfiopool

import (
	"fmt"
	"sync"
	"testing"
)

func newTestPool(t *testing.T, devices int) *Pool {
	t.Helper()
	p := NewManager().EnsurePool("test-node")
	for i := 0; i < devices; i++ {
		p.RegisterDevice(fmt.Sprintf("gpu-%d", i), fmt.Sprintf("/sys/bus/pci/devices/0000:0%d:00.0/", i))
	}
	return p
}

func TestRegisterDeviceIsIdempotent(t *testing.T) {
	p := newTestPool(t, 0)
	p.RegisterDevice("gpu-0", "/sys/bus/pci/devices/0000:01:00.0/")
	p.RegisterDevice("gpu-0", "/sys/bus/pci/devices/0000:99:00.0/") // should be a no-op

	d, ok := p.Get("gpu-0")
	if !ok {
		t.Fatal("expected gpu-0 to be registered")
	}
	if d.PCIPath != "/sys/bus/pci/devices/0000:01:00.0/" {
		t.Errorf("re-registering changed PCIPath to %q, want the original path kept", d.PCIPath)
	}
	total, available, _ := p.Counts()
	if total != 1 || available != 1 {
		t.Fatalf("total=%d available=%d, want 1/1", total, available)
	}
}

func TestAcquireAndRelease(t *testing.T) {
	p := newTestPool(t, 1)

	d, ok := p.Acquire("attachment/a")
	if !ok {
		t.Fatal("expected Acquire to succeed with one Available device")
	}
	if d.State != DeviceAttached || d.AttachKey != "attachment/a" {
		t.Errorf("acquired device = %+v, want State=Attached AttachKey=attachment/a", d)
	}

	if _, ok := p.Acquire("attachment/b"); ok {
		t.Fatal("expected Acquire to fail: pool exhausted")
	}

	p.Release(d.ID, "attachment/a")
	got, _ := p.Get(d.ID)
	if got.State != DeviceAvailable || got.AttachKey != "" {
		t.Errorf("after Release: %+v, want Available with no AttachKey", got)
	}
}

func TestReleaseWithWrongKeyIsNoop(t *testing.T) {
	p := newTestPool(t, 1)
	d, _ := p.Acquire("attachment/a")

	p.Release(d.ID, "attachment/wrong")

	got, _ := p.Get(d.ID)
	if got.State != DeviceAttached || got.AttachKey != "attachment/a" {
		t.Errorf("wrong-key Release changed state: %+v", got)
	}
}

func TestAcquireSpecific(t *testing.T) {
	p := newTestPool(t, 2)

	d, ok := p.AcquireSpecific("gpu-1", "attachment/a")
	if !ok || d.ID != "gpu-1" {
		t.Fatalf("AcquireSpecific(gpu-1) = %+v, ok=%v", d, ok)
	}

	if _, ok := p.AcquireSpecific("gpu-1", "attachment/b"); ok {
		t.Fatal("expected AcquireSpecific to fail: gpu-1 already attached")
	}
	if _, ok := p.AcquireSpecific("gpu-missing", "attachment/b"); ok {
		t.Fatal("expected AcquireSpecific to fail: unknown device id")
	}
}

func TestSetGuestBDFOnlyAppliesUnderCurrentHolder(t *testing.T) {
	p := newTestPool(t, 1)
	d, _ := p.Acquire("attachment/a")

	p.SetGuestBDF(d.ID, "attachment/wrong", "0000:00:08.0")
	got, _ := p.Get(d.ID)
	if got.GuestBDF != "" {
		t.Errorf("wrong-key SetGuestBDF applied: %+v", got)
	}

	p.SetGuestBDF(d.ID, "attachment/a", "0000:00:08.0")
	got, _ = p.Get(d.ID)
	if got.GuestBDF != "0000:00:08.0" {
		t.Errorf("GuestBDF = %q, want 0000:00:08.0", got.GuestBDF)
	}

	// Release must clear it, so a stale BDF from a previous attachment is
	// never mistaken for the current one.
	p.Release(d.ID, "attachment/a")
	got, _ = p.Get(d.ID)
	if got.GuestBDF != "" {
		t.Errorf("GuestBDF survived Release: %q", got.GuestBDF)
	}
}

func TestUnregisterDeviceRefusesWhileAttached(t *testing.T) {
	p := newTestPool(t, 1)
	d, _ := p.Acquire("attachment/a")

	if err := p.UnregisterDevice(d.ID); err == nil {
		t.Fatal("expected UnregisterDevice to refuse an attached device")
	}

	p.Release(d.ID, "attachment/a")
	if err := p.UnregisterDevice(d.ID); err != nil {
		t.Fatalf("UnregisterDevice after release: %v", err)
	}
	if _, ok := p.Get(d.ID); ok {
		t.Fatal("device still present after UnregisterDevice")
	}
}

func TestUnregisterUnknownDeviceIsNoop(t *testing.T) {
	p := newTestPool(t, 0)
	if err := p.UnregisterDevice("gpu-missing"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestManagerEnsurePoolIsIdempotentPerKey(t *testing.T) {
	m := NewManager()
	p1 := m.EnsurePool("node-a")
	p1.RegisterDevice("gpu-0", "/sys/bus/pci/devices/0000:01:00.0/")

	p2 := m.EnsurePool("node-a")
	if p2 != p1 {
		t.Fatal("EnsurePool returned a different Pool for the same key")
	}
	if _, ok := p2.Get("gpu-0"); !ok {
		t.Fatal("expected the same pool's state to be visible via the second EnsurePool call")
	}
}

func TestManagerGetPoolMissingReturnsNil(t *testing.T) {
	m := NewManager()
	if p := m.GetPool("nope"); p != nil {
		t.Fatalf("expected nil for an unknown pool key, got %+v", p)
	}
}

func TestManagerDeletePool(t *testing.T) {
	m := NewManager()
	m.EnsurePool("node-a")
	m.DeletePool("node-a")
	if p := m.GetPool("node-a"); p != nil {
		t.Fatalf("expected nil after DeletePool, got %+v", p)
	}
}

// TestConcurrentAcquireReleaseNeverDoubleBooks is the same race-safety
// property internal/pool proves for simulated slots, proven here for real
// device bookkeeping instead.
func TestConcurrentAcquireReleaseNeverDoubleBooks(t *testing.T) {
	const poolSize = 5
	const workers = 50
	const iterationsPerWorker = 200

	p := newTestPool(t, poolSize)

	var heldMu sync.Mutex
	held := make(map[string]string) // deviceID -> holder key, while "checked out" in this test

	var wg sync.WaitGroup
	errs := make(chan error, workers*iterationsPerWorker)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < iterationsPerWorker; i++ {
				key := fmt.Sprintf("attachment/w%d/i%d", worker, i)
				d, ok := p.Acquire(key)
				if !ok {
					continue // pool momentarily exhausted; not an error
				}

				heldMu.Lock()
				if owner, already := held[d.ID]; already {
					heldMu.Unlock()
					errs <- fmt.Errorf("device %s double-booked: held by %q and now acquired by %q", d.ID, owner, key)
					continue
				}
				held[d.ID] = key
				heldMu.Unlock()

				heldMu.Lock()
				delete(held, d.ID)
				heldMu.Unlock()

				p.Release(d.ID, key)
			}
		}(w)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}

	total, available, attached := p.Counts()
	if total != poolSize || attached != 0 || available != poolSize {
		t.Fatalf("pool not clean after concurrent load: total=%d available=%d attached=%d (want %d/%d/0)", total, available, attached, poolSize, poolSize)
	}
}
