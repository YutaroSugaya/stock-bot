package port

// PendingPositionTracker records entry sagas that are placed but not yet
// DB-inserted, so reconcile never mis-adopts an in-progress entry as a naked
// external position.
//
// The symbol-scoped family exists because the broker position id is unknown
// until the fill resolves (up to ~15s live) — id-scoped marks alone leave that
// window racy. The saga marks the SYMBOL before PlaceOrder instead.
type PendingPositionTracker interface {
	MarkPending(brokerPositionID string)
	MarkResolved(brokerPositionID string)
	IsPending(brokerPositionID string) bool

	MarkPendingSymbol(symbol string)
	MarkResolvedSymbol(symbol string)
	IsPendingSymbol(symbol string) bool
}
