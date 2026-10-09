package port

import "context"

// Notifier delivers operational alerts (emergency trips, forced-flatten
// failures). Best-effort: a failed notify never blocks the trading path.
type Notifier interface {
	Notify(ctx context.Context, level, title, message string) error
}
