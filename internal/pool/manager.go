// Package pool is GNWES's admission-budget tracker for the Shared
// (gVisor + real Kubernetes-scheduled GPU resource) tier.
//
// It deliberately does NOT model device identity, warm-up time, or attach
// latency — those are real and already handled by Kubernetes' own scheduler
// and whichever device plugin (NVIDIA's, or HAMi for sliced sharing) is
// installed on the cluster. GNWES's only real job here is admission control:
// never let more pods carry a real GPU resource request than PoolSize
// allows, so the cluster can't be overcommitted past what this pool was
// provisioned for. The GPULease controller records which real node/resource
// Kubernetes actually bound once the pod is Running — see
// internal/controller/gpulease_controller.go.
//
// The Exclusive (microVM + VFIO) tier does not use this package at all: its
// real source of truth is the VFIOGPUPool/VMGPUAttachment CRs themselves,
// reconciled in internal/controller directly. See internal/vfiopool.
package pool

import (
	"fmt"
	"sync"
)

// TicketState is the admission state of one budget ticket.
type TicketState string

const (
	TicketAvailable TicketState = "Available"
	TicketLeased    TicketState = "Leased"
)

// Ticket is one unit of admission budget. It is not a device: it exists only
// to bound concurrent admission and to let Acquire/Release/Transfer be
// atomic under concurrent reconciles.
type Ticket struct {
	ID       string
	State    TicketState
	LeaseKey string // namespace/name of the GPULease (or podgate reservation) currently holding it
}

// Pool tracks the admission tickets for a single GPUWorkerPool.
type Pool struct {
	mu         sync.Mutex
	Name       string
	GPUModel   string
	tickets    map[string]*Ticket
	nextTicket int
}

// Manager holds one Pool per GPUWorkerPool, keyed by "namespace/name".
type Manager struct {
	mu    sync.Mutex
	pools map[string]*Pool
}

func NewManager() *Manager {
	return &Manager{pools: make(map[string]*Pool)}
}

// EnsurePool returns the Pool for key, creating it (with zero tickets) if it
// does not exist yet.
func (m *Manager) EnsurePool(key, gpuModel string) *Pool {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pools[key]
	if !ok {
		p = &Pool{
			Name:     key,
			GPUModel: gpuModel,
			tickets:  make(map[string]*Ticket),
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

// Resize grows or shrinks the pool's admission budget toward targetSize.
// Growing adds Available tickets immediately — there is no warm-up delay to
// simulate, since admission budget is just a count, not a device. Shrinking
// only removes Available tickets.
func (p *Pool) Resize(targetSize int32) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for int32(len(p.tickets)) < targetSize {
		id := fmt.Sprintf("budget-%d", p.nextTicket)
		p.nextTicket++
		p.tickets[id] = &Ticket{ID: id, State: TicketAvailable}
	}

	if int32(len(p.tickets)) > targetSize {
		removed := int32(len(p.tickets)) - targetSize
		for id, t := range p.tickets {
			if removed == 0 {
				break
			}
			if t.State == TicketAvailable {
				delete(p.tickets, id)
				removed--
			}
		}
	}
}

// Acquire claims one Available ticket for leaseKey. Returns ok=false if none
// are free right now (the pool is at its admission budget).
func (p *Pool) Acquire(leaseKey string) (ticketID string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, t := range p.tickets {
		if t.State == TicketAvailable {
			t.State = TicketLeased
			t.LeaseKey = leaseKey
			return t.ID, true
		}
	}
	return "", false
}

// Release returns a leased ticket to the Available budget. No-op if the
// ticket isn't currently leased by leaseKey (idempotent against repeat
// reconciles).
func (p *Pool) Release(ticketID, leaseKey string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t, ok := p.tickets[ticketID]; ok && t.State == TicketLeased && t.LeaseKey == leaseKey {
		t.State = TicketAvailable
		t.LeaseKey = ""
	}
}

// Transfer moves a Leased ticket's ownership from fromKey to toKey,
// succeeding only if the ticket is currently leased under fromKey. This
// hands a scheduling-gate reservation off to the GPULease that follows it
// without releasing and re-acquiring, which would race against another actor
// grabbing the budget in between.
func (p *Pool) Transfer(ticketID, fromKey, toKey string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t, ok := p.tickets[ticketID]; ok && t.State == TicketLeased && t.LeaseKey == fromKey {
		t.LeaseKey = toKey
		return true
	}
	return false
}

// Counts returns (total, available, leased) ticket counts.
func (p *Pool) Counts() (total, available, leased int32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, t := range p.tickets {
		total++
		switch t.State {
		case TicketAvailable:
			available++
		case TicketLeased:
			leased++
		}
	}
	return
}
