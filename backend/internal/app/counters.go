package app

import "sync/atomic"

type Counters struct {
	PriceTicks        atomic.Int64
	Entries           atomic.Int64
	Rejections        atomic.Int64
	ForcedFlats       atomic.Int64
	EmergencyTrips    atomic.Int64
	TickerErrors      atomic.Int64
	StaleQuotes       atomic.Int64 // indicative quotes (前日終値 fallback) skipped
	StaleDaily        atomic.Int64 // day-horizon evals skipped because the daily feed was stale
	Compensations     atomic.Int64 // entry-saga rollbacks (post-fill failure → close)
	ExternalAdoptions atomic.Int64 // naked broker positions adopted at reconcile
	// 値幅制限の外に出た利確指値を落とし、SL のみ broker 側に置いた回数。
	// この状態の建玉は **TP が板に無く bot の OnTick が見ている**。
	ProtectiveTPDropped atomic.Int64
	// これが無いと、毎ティック panic している銘柄が /api/status 上では
	// 「静かな健全銘柄」と区別できない(ループは "running" のまま何もしない)。
	LoopPanics atomic.Int64
}

func (c *Counters) IncrCompensations()       { c.Compensations.Add(1) }
func (c *Counters) IncrExternalAdoptions()   { c.ExternalAdoptions.Add(1) }
func (c *Counters) IncrProtectiveTPDropped() { c.ProtectiveTPDropped.Add(1) }

type CountersSnapshot struct {
	PriceTicks          int64 `json:"price_ticks"`
	Entries             int64 `json:"entries"`
	Rejections          int64 `json:"rejections"`
	LoopPanics          int64 `json:"loop_panics"`
	ForcedFlats         int64 `json:"forced_flats"`
	EmergencyTrips      int64 `json:"emergency_trips"`
	TickerErrors        int64 `json:"ticker_errors"`
	StaleQuotes         int64 `json:"stale_quotes"`
	StaleDaily          int64 `json:"stale_daily"`
	Compensations       int64 `json:"compensations"`
	ExternalAdoptions   int64 `json:"external_adoptions"`
	ProtectiveTPDropped int64 `json:"protective_tp_dropped"`
}

func (c *Counters) Snapshot() CountersSnapshot {
	return CountersSnapshot{
		PriceTicks:          c.PriceTicks.Load(),
		Entries:             c.Entries.Load(),
		Rejections:          c.Rejections.Load(),
		LoopPanics:          c.LoopPanics.Load(),
		ForcedFlats:         c.ForcedFlats.Load(),
		EmergencyTrips:      c.EmergencyTrips.Load(),
		TickerErrors:        c.TickerErrors.Load(),
		StaleQuotes:         c.StaleQuotes.Load(),
		StaleDaily:          c.StaleDaily.Load(),
		Compensations:       c.Compensations.Load(),
		ExternalAdoptions:   c.ExternalAdoptions.Load(),
		ProtectiveTPDropped: c.ProtectiveTPDropped.Load(),
	}
}
