package pool

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func newTestPool(t *testing.T, slotInitMillis int32) *Pool {
	t.Helper()
	m := NewManager()
	return m.EnsurePool("ns/test-pool", "mock-model", slotInitMillis, 10)
}

func TestResizeGrowsAsInitializing(t *testing.T) {
	p := newTestPool(t, 10_000) // long enough that nothing promotes mid-test
	added := p.Resize(3)
	if len(added) != 3 {
		t.Fatalf("expected 3 newly added slots, got %d", len(added))
	}
	total, available, leased, initializing := p.Counts()
	if total != 3 || available != 0 || leased != 0 || initializing != 3 {
		t.Fatalf("got total=%d available=%d leased=%d initializing=%d, want 3/0/0/3", total, available, leased, initializing)
	}
}

func TestResizeIsIdempotentAtSameTarget(t *testing.T) {
	p := newTestPool(t, 10_000)
	p.Resize(2)
	added := p.Resize(2)
	if len(added) != 0 {
		t.Fatalf("resizing to the same target should add nothing, got %d", len(added))
	}
	total, _, _, _ := p.Counts()
	if total != 2 {
		t.Fatalf("expected total=2, got %d", total)
	}
}

func TestSlotPromotesToAvailableAfterInitDelay(t *testing.T) {
	p := newTestPool(t, 20) // 20ms simulated init cost
	p.Resize(1)

	_, available, _, initializing := p.Counts()
	if available != 0 || initializing != 1 {
		t.Fatalf("expected the slot to still be initializing immediately, got available=%d initializing=%d", available, initializing)
	}

	time.Sleep(40 * time.Millisecond)

	_, available, _, initializing = p.Counts()
	if available != 1 || initializing != 0 {
		t.Fatalf("expected the slot to have promoted to available, got available=%d initializing=%d", available, initializing)
	}
}

func TestResizeShrinkOnlyRemovesAvailableSlots(t *testing.T) {
	p := newTestPool(t, 1) // promotes to available almost immediately
	p.Resize(2)
	time.Sleep(10 * time.Millisecond) // let both slots become available

	slotID, ok := p.Acquire("lease/a")
	if !ok {
		t.Fatalf("expected to acquire a slot")
	}

	// Shrink to 1: the leased slot must survive; only the available one may go.
	p.Resize(1)

	total, available, leased, _ := p.Counts()
	if total != 1 || leased != 1 || available != 0 {
		t.Fatalf("shrink removed the wrong slot: total=%d available=%d leased=%d", total, available, leased)
	}

	p.Release(slotID, "lease/a")
}

func TestAcquireAndRelease(t *testing.T) {
	p := newTestPool(t, 1)
	p.Resize(1)
	time.Sleep(10 * time.Millisecond)

	slotID, ok := p.Acquire("lease/a")
	if !ok || slotID == "" {
		t.Fatalf("expected to acquire the single available slot")
	}

	if _, ok := p.Acquire("lease/b"); ok {
		t.Fatalf("expected no slot to be available for a second acquirer")
	}

	p.Release(slotID, "lease/a")

	total, available, leased, _ := p.Counts()
	if available != 1 || leased != 0 || total != 1 {
		t.Fatalf("expected the slot back in the available pool, got available=%d leased=%d total=%d", available, leased, total)
	}
}

func TestAcquireWhenNoneInitialized(t *testing.T) {
	p := newTestPool(t, 10_000)
	if _, ok := p.Acquire("lease/a"); ok {
		t.Fatalf("expected Acquire to fail against an empty pool")
	}
}

func TestReleaseWithWrongKeyIsNoop(t *testing.T) {
	p := newTestPool(t, 1)
	p.Resize(1)
	time.Sleep(10 * time.Millisecond)

	slotID, _ := p.Acquire("lease/a")
	p.Release(slotID, "lease/someone-else")

	_, available, leased, _ := p.Counts()
	if available != 0 || leased != 1 {
		t.Fatalf("a release under the wrong key must not free the slot; got available=%d leased=%d", available, leased)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	p := newTestPool(t, 1)
	p.Resize(1)
	time.Sleep(10 * time.Millisecond)

	slotID, _ := p.Acquire("lease/a")
	p.Release(slotID, "lease/a")
	p.Release(slotID, "lease/a") // must not panic or double-count

	_, available, leased, _ := p.Counts()
	if available != 1 || leased != 0 {
		t.Fatalf("expected one available slot after idempotent release, got available=%d leased=%d", available, leased)
	}
}

func TestTransferSucceedsUnderMatchingKey(t *testing.T) {
	p := newTestPool(t, 1)
	p.Resize(1)
	time.Sleep(10 * time.Millisecond)

	slotID, _ := p.Acquire("podgate/ns/pod-1")
	if !p.Transfer(slotID, "podgate/ns/pod-1", "lease/ns/pod-1-lease") {
		t.Fatalf("expected transfer to succeed when the fromKey matches the current owner")
	}

	// The slot must now be released only under the NEW key.
	p.Release(slotID, "podgate/ns/pod-1")
	if _, available, leased, _ := p.Counts(); available != 0 || leased != 1 {
		t.Fatalf("release under the old (pre-transfer) key must be a no-op; got available=%d leased=%d", available, leased)
	}
	p.Release(slotID, "lease/ns/pod-1-lease")
	if _, available, leased, _ := p.Counts(); available != 1 || leased != 0 {
		t.Fatalf("release under the new (post-transfer) key must free the slot; got available=%d leased=%d", available, leased)
	}
}

func TestTransferFailsUnderWrongKey(t *testing.T) {
	p := newTestPool(t, 1)
	p.Resize(1)
	time.Sleep(10 * time.Millisecond)

	slotID, _ := p.Acquire("podgate/ns/pod-1")
	if p.Transfer(slotID, "not-the-owner", "lease/ns/pod-1-lease") {
		t.Fatalf("transfer must fail when fromKey does not match the current owner")
	}
	// Ownership must be unchanged: the original key can still release it.
	p.Release(slotID, "podgate/ns/pod-1")
	if _, available, _, _ := p.Counts(); available != 1 {
		t.Fatalf("expected the original owner to still be able to release the slot")
	}
}

func TestTransferFailsOnUnknownSlot(t *testing.T) {
	p := newTestPool(t, 1)
	if p.Transfer("no-such-slot", "a", "b") {
		t.Fatalf("transfer of a nonexistent slot must fail")
	}
}

func TestManagerEnsurePoolIsIdempotentPerKey(t *testing.T) {
	m := NewManager()
	p1 := m.EnsurePool("ns/pool", "model", 100, 10)
	p1.Resize(2)
	p2 := m.EnsurePool("ns/pool", "model", 999, 999) // different params, same key
	if p1 != p2 {
		t.Fatalf("EnsurePool must return the same *Pool for the same key")
	}
	total, _, _, _ := p2.Counts()
	if total != 2 {
		t.Fatalf("expected the existing pool's slots to be preserved, got total=%d", total)
	}
}

func TestManagerGetPoolMissingReturnsNil(t *testing.T) {
	m := NewManager()
	if p := m.GetPool("ns/does-not-exist"); p != nil {
		t.Fatalf("expected nil for a pool that was never created")
	}
}

func TestManagerDeletePool(t *testing.T) {
	m := NewManager()
	m.EnsurePool("ns/pool", "model", 100, 10)
	m.DeletePool("ns/pool")
	if p := m.GetPool("ns/pool"); p != nil {
		t.Fatalf("expected the pool to be gone after DeletePool")
	}
}

// TestConcurrentAcquireReleaseNeverDoubleBooks hammers a small pool with many
// concurrent goroutines acquiring and releasing slots, and fails the test if
// any two "concurrently held" views ever claim the same slot ID at once.
// Run with -race to also catch any unsynchronized access.
func TestConcurrentAcquireReleaseNeverDoubleBooks(t *testing.T) {
	const poolSize = 5
	const workers = 50
	const iterationsPerWorker = 200

	p := newTestPool(t, 1)
	p.Resize(poolSize)
	time.Sleep(10 * time.Millisecond) // let all slots become available

	var heldMu sync.Mutex
	held := make(map[string]string) // slotID -> holder key, while "checked out" in this test

	var wg sync.WaitGroup
	errs := make(chan error, workers*iterationsPerWorker)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < iterationsPerWorker; i++ {
				key := fmt.Sprintf("lease/w%d/i%d", worker, i)
				slotID, ok := p.Acquire(key)
				if !ok {
					continue // pool momentarily exhausted; not an error
				}

				heldMu.Lock()
				if owner, already := held[slotID]; already {
					heldMu.Unlock()
					errs <- fmt.Errorf("slot %s double-booked: held by %q and now acquired by %q", slotID, owner, key)
					continue
				}
				held[slotID] = key
				heldMu.Unlock()

				// Simulate doing a little work while holding the slot.
				time.Sleep(time.Microsecond)

				heldMu.Lock()
				delete(held, slotID)
				heldMu.Unlock()

				p.Release(slotID, key)
			}
		}(w)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}

	// Every slot must have made it back to Available; none leaked as Leased
	// or vanished.
	total, available, leased, _ := p.Counts()
	if total != poolSize || leased != 0 || available != poolSize {
		t.Fatalf("pool not clean after concurrent load: total=%d available=%d leased=%d (want %d/%d/0)", total, available, leased, poolSize, poolSize)
	}
}

// TestConcurrentTransferIsExclusive checks that when many goroutines race to
// Transfer the same slot away from a single owner key, exactly one succeeds.
func TestConcurrentTransferIsExclusive(t *testing.T) {
	p := newTestPool(t, 1)
	p.Resize(1)
	time.Sleep(10 * time.Millisecond)

	slotID, ok := p.Acquire("owner")
	if !ok {
		t.Fatalf("setup: expected to acquire the slot")
	}

	const contenders = 50
	var successCount int32
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if p.Transfer(slotID, "owner", fmt.Sprintf("new-owner-%d", i)) {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if successCount != 1 {
		t.Fatalf("expected exactly one Transfer to succeed under concurrent contention, got %d", successCount)
	}
}
