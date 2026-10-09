package safety

import "sync"

// PendingPositions tracks broker position ids whose entry saga has placed an order
// but not yet completed the DB insert. Reconcile consults IsPending to avoid
// mis-adopting an in-flight entry as a naked external position.
type PendingPositions struct {
	mu      sync.RWMutex
	pending map[string]struct{}
	symbols map[string]int // symbol -> in-flight entry sagas (counted, not boolean)
}

// NewPendingPositions builds an empty tracker.
func NewPendingPositions() *PendingPositions {
	return &PendingPositions{pending: make(map[string]struct{}), symbols: make(map[string]int)}
}

// MarkPending records that brokerPositionID's saga is in flight.
func (p *PendingPositions) MarkPending(brokerPositionID string) {
	if brokerPositionID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending[brokerPositionID] = struct{}{}
}

// MarkResolved clears a completed (or aborted) saga.
func (p *PendingPositions) MarkResolved(brokerPositionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.pending, brokerPositionID)
}

// IsPending reports whether brokerPositionID's saga is still in flight.
func (p *PendingPositions) IsPending(brokerPositionID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.pending[brokerPositionID]
	return ok
}

// MarkPendingSymbol records an entry saga in flight for a symbol. Marked BEFORE
// PlaceOrder — the broker position id does not exist yet, so the id-scoped mark cannot
// cover the placement/resolve window (reconcile would mis-adopt a just-filled entry).
func (p *PendingPositions) MarkPendingSymbol(symbol string) {
	if symbol == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.symbols[symbol]++
}

// MarkResolvedSymbol clears one completed (or aborted) saga for a symbol.
func (p *PendingPositions) MarkResolvedSymbol(symbol string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.symbols[symbol] > 1 {
		p.symbols[symbol]--
		return
	}
	delete(p.symbols, symbol)
}

// IsPendingSymbol reports whether any entry saga is in flight for a symbol.
func (p *PendingPositions) IsPendingSymbol(symbol string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.symbols[symbol] > 0
}
