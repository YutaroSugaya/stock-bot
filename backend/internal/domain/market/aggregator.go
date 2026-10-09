package market

import (
	"sync"
	"time"
)

// tick を rolling OHLCV へ畳む。domain で mutex を許す 2 つ目の例外(rolling window は本質的に stateful)。
type Aggregator struct {
	mu        sync.Mutex
	symbol    string
	intervals []time.Duration
	buffers   map[time.Duration]*RingBuffer
	current   map[time.Duration]*Candle
	last      Ticker
	// 直前ティックの**累計**出来高。バーに積むのはここからの増分。
	// 0 = まだ 1 本も出来高付きのティックを見ていない。
	lastCumVolume float64
}

func NewAggregator(symbol string, capacity int, intervals ...time.Duration) *Aggregator {
	if len(intervals) == 0 {
		intervals = []time.Duration{time.Minute, 5 * time.Minute, time.Hour}
	}
	a := &Aggregator{
		symbol:    symbol,
		intervals: intervals,
		buffers:   make(map[time.Duration]*RingBuffer, len(intervals)),
		current:   make(map[time.Duration]*Candle, len(intervals)),
	}
	for _, iv := range intervals {
		a.buffers[iv] = NewRingBuffer(capacity)
	}
	return a
}

func (a *Aggregator) OnTick(t Ticker, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.last = t
	price := t.Mid()
	if price <= 0 {
		return
	}
	// broker が返すのは当日の**累計**。バーには増分を積む — 累計をそのまま入れると
	// 「5分足の出来高」が「その時刻までの累計」になり、25日平均との比較が壊れる。
	// 寄り付き直後(前回値なし)は累計そのもの = 寄りからの合計。日跨ぎでリセット
	// されると増分が負になるので、そのときも累計そのものを採る(負を書かない)。
	var dv float64
	if t.Volume > 0 {
		dv = t.Volume - a.lastCumVolume
		if dv < 0 {
			dv = t.Volume
		}
		a.lastCumVolume = t.Volume
	}
	for _, iv := range a.intervals {
		bucket := now.Truncate(iv)
		cur := a.current[iv]
		if cur == nil || !cur.OpenTime.Equal(bucket) {
			if cur != nil {
				a.buffers[iv].Push(*cur)
			}
			a.current[iv] = &Candle{
				Symbol:   a.symbol,
				Interval: iv,
				OpenTime: bucket,
				Open:     price,
				High:     price,
				Low:      price,
				Close:    price,
				Volume:   dv,
			}
			continue
		}
		if price > cur.High {
			cur.High = price
		}
		if price < cur.Low {
			cur.Low = price
		}
		cur.Close = price
		cur.Volume += dv
	}
}

// 完成済みバー(古い順)。形成中バーを末尾に足すので戦略は live バーも見える。
func (a *Aggregator) Candles(iv time.Duration) []Candle {
	a.mu.Lock()
	defer a.mu.Unlock()
	buf := a.buffers[iv]
	if buf == nil {
		return nil
	}
	out := buf.Snapshot()
	if cur := a.current[iv]; cur != nil {
		out = append(out, *cur)
	}
	return out
}

func (a *Aggregator) Last() Ticker {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last
}
