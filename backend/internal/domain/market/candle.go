package market

import (
	"sync"
	"time"
)

type Candle struct {
	Symbol   string
	Interval time.Duration
	OpenTime time.Time // bucket start (UTC)
	Open     float64
	High     float64
	Low      float64
	Close    float64
	Volume   float64
}

// 満杯時に最古を捨てる固定長バッファ。domain で mutex を許す例外の 1 つ。
type RingBuffer struct {
	mu   sync.RWMutex
	cap  int
	data []Candle
	head int
	full bool
}

func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = 1
	}
	return &RingBuffer{cap: capacity, data: make([]Candle, capacity)}
}

func (r *RingBuffer) Push(c Candle) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data[r.head] = c
	r.head = (r.head + 1) % r.cap
	if r.head == 0 {
		r.full = true
	}
}

// 時系列順(古い順)で返す。
func (r *RingBuffer) Snapshot() []Candle {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := r.head
	if r.full {
		n = r.cap
	}
	out := make([]Candle, 0, n)
	if r.full {
		for i := 0; i < r.cap; i++ {
			out = append(out, r.data[(r.head+i)%r.cap])
		}
		return out
	}
	for i := 0; i < r.head; i++ {
		out = append(out, r.data[i])
	}
	return out
}
