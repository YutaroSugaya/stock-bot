package app

import (
	"sync"

	"stockbot/backend/internal/config"
)

// 差し替えは既存の建玉に影響しない(建玉は entry 時に凍結した config で走る)。
type ActiveConfigHolder struct {
	mu  sync.RWMutex
	cfg *config.StrategyConfig
}

func NewActiveConfigHolder(cfg *config.StrategyConfig) *ActiveConfigHolder {
	return &ActiveConfigHolder{cfg: cfg}
}

func (h *ActiveConfigHolder) Get() *config.StrategyConfig {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cfg
}

func (h *ActiveConfigHolder) Set(cfg *config.StrategyConfig) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg = cfg
}

func (h *ActiveConfigHolder) ConfigID() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.cfg == nil {
		return ""
	}
	return h.cfg.ConfigID
}
