// Package pool holds the in-memory source of truth for warm GPU slots.
//
// Slot state lives here, not purely in CR status, so that Acquire/Release are
// atomic even when many GPULease reconciles race against the same pool. The
// GPUWorkerPool controller mirrors this state into the CR's status for
// visibility; the GPULease controller is the only thing that mutates it.
package pool

import (
	"fmt"
	"sync"
	"time"
)

// SlotState is the lifecycle state of one simulated GPU slot.
type SlotState string

const (
	SlotInitializing SlotState = "Initializing"
	SlotAvailable    SlotState = "Available"
	SlotLeased       SlotState = "Leased"
)

// Slot is one simulated warm GPU slot.
type Slot struct {
	ID          string
	State       SlotState
	CreatedAt   time.Time
	ReadyAt     time.Time // CreatedAt + the pool's SlotInitMillis
	LeaseKey    string    // namespace/name of the GPULease currently holding it
	LeasedSince time.Time
}

// Pool tracks the slots for a single GPUWorkerPool.
type Pool struct {
	mu                  sync.Mutex
	Name                string
	GPUModel            string
	SlotInitMillis      int32
	AttachLatencyMillis int32
	slots               map[string]*Slot
	nextSlotNum         int
}

// Manager holds one Pool per GPUWorkerPool, keyed by "namespace/name".
type Manager struct {
	mu    sync.Mutex
	pools map[string]*Pool
}

func NewManager() *Manager {
	return &Manager{pools: make(map[string]*Pool)}
}

// EnsurePool returns the Pool for key, creating it (with zero slots) if it
// does not exist yet.
func (m *Manager) EnsurePool(key, gpuModel string, slotInitMillis, attachLatencyMillis int32) *Pool {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pools[key]
	if !ok {
		p = &Pool{
			Name:                key,
			GPUModel:            gpuModel,
			SlotInitMillis:      slotInitMillis,
			AttachLatencyMillis: attachLatencyMillis,
			slots:               make(map[string]*Slot),
		}
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

// refreshLocked promotes any Initializing slot whose simulated init delay has
// elapsed into Available. Must be called with p.mu held.
func (p *Pool) refreshLocked() {
	now := time.Now()
	for _, s := range p.slots {
		if s.State == SlotInitializing && !now.Before(s.ReadyAt) {
			s.State = SlotAvailable
		}
	}
}

// Resize grows or shrinks the pool toward targetSize. Growing adds slots in
// SlotInitializing state; they lazily promote to Available once SlotInitMillis
// has elapsed since creation, simulating the one-time cold-start cost paid
// ahead of any lease request. Shrinking only removes Available slots.
// Returns the IDs of newly added (still-initializing) slots.
func (p *Pool) Resize(targetSize int32) (added []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshLocked()

	for int32(len(p.slots)) < targetSize {
		id := fmt.Sprintf("mock-gpu-%d", p.nextSlotNum)
		p.nextSlotNum++
		now := time.Now()
		p.slots[id] = &Slot{
			ID:        id,
			State:     SlotInitializing,
			CreatedAt: now,
			ReadyAt:   now.Add(time.Duration(p.SlotInitMillis) * time.Millisecond),
		}
		added = append(added, id)
	}

	if int32(len(p.slots)) > targetSize {
		removed := int32(len(p.slots)) - targetSize
		for id, s := range p.slots {
			if removed == 0 {
				break
			}
			if s.State == SlotAvailable {
				delete(p.slots, id)
				removed--
			}
		}
	}
	return added
}

// Acquire claims one Available slot for leaseKey. Returns ok=false if none
// are free right now.
func (p *Pool) Acquire(leaseKey string) (slotID string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshLocked()
	for _, s := range p.slots {
		if s.State == SlotAvailable {
			s.State = SlotLeased
			s.LeaseKey = leaseKey
			s.LeasedSince = time.Now()
			return s.ID, true
		}
	}
	return "", false
}

// Release returns a leased slot to the Available pool. No-op if the slot
// isn't currently leased by leaseKey (idempotent against repeat reconciles).
func (p *Pool) Release(slotID, leaseKey string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshLocked()
	if s, ok := p.slots[slotID]; ok && s.State == SlotLeased && s.LeaseKey == leaseKey {
		s.State = SlotAvailable
		s.LeaseKey = ""
	}
}

// Transfer moves a Leased slot's ownership from fromKey to toKey, succeeding
// only if the slot is currently leased under fromKey. This hands a
// scheduling-gate reservation off to the GPULease that follows it without
// releasing and re-acquiring, which would race against another actor
// grabbing the slot in between.
func (p *Pool) Transfer(slotID, fromKey, toKey string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshLocked()
	if s, ok := p.slots[slotID]; ok && s.State == SlotLeased && s.LeaseKey == fromKey {
		s.LeaseKey = toKey
		return true
	}
	return false
}

// Counts returns (total, available, leased, initializing) slot counts.
func (p *Pool) Counts() (total, available, leased, initializing int32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshLocked()
	for _, s := range p.slots {
		total++
		switch s.State {
		case SlotAvailable:
			available++
		case SlotLeased:
			leased++
		case SlotInitializing:
			initializing++
		}
	}
	return
}
