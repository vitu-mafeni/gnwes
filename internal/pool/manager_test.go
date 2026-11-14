package pool

import (
	"fmt"
	"sync"
	"testing"
)

func newTestPool(t *testing.T) *Pool {
	t.Helper()
	m := NewManager()
	return m.EnsurePool("ns/test-pool", "mock-model")
}

func TestResizeGrowsImmediately(t *testing.T) {
	p := newTestPool(t)
	p.Resize(3)
	total, available, leased := p.Counts()
	if total != 3 || available != 3 || leased != 0 {
		t.Fatalf("got total=%d available=%d leased=%d, want 3/3/0", total, available, leased)
	}
}

func TestResizeIsIdempotentAtSameTarget(t *testing.T) {
	p := newTestPool(t)
	p.Resize(2)
	p.Resize(2)
	total, _, _ := p.Counts()
	if total != 2 {
		t.Fatalf("expected total=2, got %d", total)
	}
}

func TestResizeShrinkOnlyRemovesAvailableTickets(t *testing.T) {
	p := newTestPool(t)
	p.Resize(2)

	ticketID, ok := p.Acquire("lease/a")
	if !ok {
		t.Fatalf("expected to acquire a ticket")
	}

	// Shrink to 1: the leased ticket must survive; only the available one may go.
	p.Resize(1)

	total, available, leased := p.Counts()
	if total != 1 || leased != 1 || available != 0 {
		t.Fatalf("shrink removed the wrong ticket: total=%d available=%d leased=%d", total, available, leased)
	}

	p.Release(ticketID, "lease/a")
}

func TestAcquireAndRelease(t *testing.T) {
	p := newTestPool(t)
	p.Resize(1)

	ticketID, ok := p.Acquire("lease/a")
	if !ok || ticketID == "" {
		t.Fatalf("expected to acquire the single available ticket")
	}

	if _, ok := p.Acquire("lease/b"); ok {
		t.Fatalf("expected no ticket to be available for a second acquirer")
	}

	p.Release(ticketID, "lease/a")

	total, available, leased := p.Counts()
	if available != 1 || leased != 0 || total != 1 {
		t.Fatalf("expected the ticket back in the available budget, got available=%d leased=%d total=%d", available, leased, total)
	}
}

func TestAcquireWhenPoolEmpty(t *testing.T) {
	p := newTestPool(t)
	if _, ok := p.Acquire("lease/a"); ok {
		t.Fatalf("expected Acquire to fail against an empty pool")
	}
}

func TestReleaseWithWrongKeyIsNoop(t *testing.T) {
	p := newTestPool(t)
	p.Resize(1)

	ticketID, _ := p.Acquire("lease/a")
	p.Release(ticketID, "lease/someone-else")

	_, available, leased := p.Counts()
	if available != 0 || leased != 1 {
		t.Fatalf("a release under the wrong key must not free the ticket; got available=%d leased=%d", available, leased)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	p := newTestPool(t)
	p.Resize(1)

	ticketID, _ := p.Acquire("lease/a")
	p.Release(ticketID, "lease/a")
	p.Release(ticketID, "lease/a") // must not panic or double-count

	_, available, leased := p.Counts()
	if available != 1 || leased != 0 {
		t.Fatalf("expected one available ticket after idempotent release, got available=%d leased=%d", available, leased)
	}
}

func TestTransferSucceedsUnderMatchingKey(t *testing.T) {
	p := newTestPool(t)
	p.Resize(1)

	ticketID, _ := p.Acquire("podgate/ns/pod-1")
	if !p.Transfer(ticketID, "podgate/ns/pod-1", "lease/ns/pod-1-lease") {
		t.Fatalf("expected transfer to succeed when the fromKey matches the current owner")
	}

	// The ticket must now be released only under the NEW key.
	p.Release(ticketID, "podgate/ns/pod-1")
	if _, available, leased := p.Counts(); available != 0 || leased != 1 {
		t.Fatalf("release under the old (pre-transfer) key must be a no-op; got available=%d leased=%d", available, leased)
	}
	p.Release(ticketID, "lease/ns/pod-1-lease")
	if _, available, leased := p.Counts(); available != 1 || leased != 0 {
		t.Fatalf("release under the new (post-transfer) key must free the ticket; got available=%d leased=%d", available, leased)
	}
}

func TestTransferFailsUnderWrongKey(t *testing.T) {
	p := newTestPool(t)
	p.Resize(1)

	ticketID, _ := p.Acquire("podgate/ns/pod-1")
	if p.Transfer(ticketID, "not-the-owner", "lease/ns/pod-1-lease") {
		t.Fatalf("transfer must fail when fromKey does not match the current owner")
	}
	// Ownership must be unchanged: the original key can still release it.
	p.Release(ticketID, "podgate/ns/pod-1")
	if _, available, _ := p.Counts(); available != 1 {
		t.Fatalf("expected the original owner to still be able to release the ticket")
	}
}

func TestTransferFailsOnUnknownTicket(t *testing.T) {
	p := newTestPool(t)
	if p.Transfer("no-such-ticket", "a", "b") {
		t.Fatalf("transfer of a nonexistent ticket must fail")
	}
}

func TestManagerEnsurePoolIsIdempotentPerKey(t *testing.T) {
	m := NewManager()
	p1 := m.EnsurePool("ns/pool", "model")
	p1.Resize(2)
	p2 := m.EnsurePool("ns/pool", "a different model string")
	if p1 != p2 {
		t.Fatalf("EnsurePool must return the same *Pool for the same key")
	}
	total, _, _ := p2.Counts()
	if total != 2 {
		t.Fatalf("expected the existing pool's tickets to be preserved, got total=%d", total)
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
	m.EnsurePool("ns/pool", "model")
	m.DeletePool("ns/pool")
	if p := m.GetPool("ns/pool"); p != nil {
		t.Fatalf("expected the pool to be gone after DeletePool")
	}
}

// TestConcurrentAcquireReleaseNeverDoubleBooks hammers a small pool with many
// concurrent goroutines acquiring and releasing tickets, and fails the test
// if any two "concurrently held" views ever claim the same ticket ID at
// once. Run with -race to also catch any unsynchronized access.
func TestConcurrentAcquireReleaseNeverDoubleBooks(t *testing.T) {
	const poolSize = 5
	const workers = 50
	const iterationsPerWorker = 200

	p := newTestPool(t)
	p.Resize(poolSize)

	var heldMu sync.Mutex
	held := make(map[string]string) // ticketID -> holder key, while "checked out" in this test

	var wg sync.WaitGroup
	errs := make(chan error, workers*iterationsPerWorker)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < iterationsPerWorker; i++ {
				key := fmt.Sprintf("lease/w%d/i%d", worker, i)
				ticketID, ok := p.Acquire(key)
				if !ok {
					continue // pool momentarily exhausted; not an error
				}

				heldMu.Lock()
				if owner, already := held[ticketID]; already {
					heldMu.Unlock()
					errs <- fmt.Errorf("ticket %s double-booked: held by %q and now acquired by %q", ticketID, owner, key)
					continue
				}
				held[ticketID] = key
				heldMu.Unlock()

				heldMu.Lock()
				delete(held, ticketID)
				heldMu.Unlock()

				p.Release(ticketID, key)
			}
		}(w)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}

	// Every ticket must have made it back to Available; none leaked as Leased
	// or vanished.
	total, available, leased := p.Counts()
	if total != poolSize || leased != 0 || available != poolSize {
		t.Fatalf("pool not clean after concurrent load: total=%d available=%d leased=%d (want %d/%d/0)", total, available, leased, poolSize, poolSize)
	}
}

// TestConcurrentTransferIsExclusive checks that when many goroutines race to
// Transfer the same ticket away from a single owner key, exactly one
// succeeds.
func TestConcurrentTransferIsExclusive(t *testing.T) {
	p := newTestPool(t)
	p.Resize(1)

	ticketID, ok := p.Acquire("owner")
	if !ok {
		t.Fatalf("setup: expected to acquire the ticket")
	}

	const contenders = 50
	var successCount int32
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if p.Transfer(ticketID, "owner", fmt.Sprintf("new-owner-%d", i)) {
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
