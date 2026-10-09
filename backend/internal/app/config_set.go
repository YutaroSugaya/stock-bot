package app

import (
	"sort"
	"sync"

	"stockbot/backend/internal/config"
)

// ConfigSet は 1 銘柄に載る active config の集合。
//
// 🚨 なぜ集合なのか: 建玉の一意性キーが 銘柄 →(銘柄, 戦略)に
// 変わった。入口が同一で出口だけが違う 2 アーム(`bnf_reversion` / `_trail`)は、
// 1 銘柄 1 config のままだと同じトリガーを奪い合うだけで、**ペアが 1 本も成立しない**
// (trigger は完全一致なのに picked が片側に偏る)。holders のキーだけ変えても、bundle が
// 1 config しか実行しないなら 2 本目のアームは動かない。
//
// base は「arm が 1 つも無いときの 1 本」= 静的 yaml / no_trade。arm があるあいだは
// base を混ぜない(毎ティック no_trade を評価するだけの無駄になる)。
//
// 🛑 評価順は**戦略名の昇順で決定的**。同じ入力なら同じ順に発注される(再現性)。
type ConfigSet struct {
	mu   sync.RWMutex
	base *ActiveConfigHolder
	arms map[config.StrategyName]*ActiveConfigHolder
}

func NewConfigSet(base *config.StrategyConfig) *ConfigSet {
	return &ConfigSet{
		base: NewActiveConfigHolder(base),
		arms: map[config.StrategyName]*ActiveConfigHolder{},
	}
}

// Base は arm が無いときに走る 1 本の holder。静的 config 経路(selector / advisor OFF)と
// 起動時の永続化がここを使う。
func (s *ConfigSet) Base() *ActiveConfigHolder { return s.base }

// Arm は (銘柄, 戦略) に config を載せる。同一戦略の再 arm は置き換え。
func (s *ConfigSet) Arm(cfg *config.StrategyConfig) {
	if cfg == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, ok := s.arms[cfg.StrategyName]; ok {
		h.Set(cfg)
		return
	}
	s.arms[cfg.StrategyName] = NewActiveConfigHolder(cfg)
}

// Disarm は **その戦略だけ**を外す。もう一方のアームは走り続ける。
func (s *ConfigSet) Disarm(name config.StrategyName) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.arms, name)
}

// Active はこのティックで評価する config を昇順で返す。arm が 1 つも無ければ base 1 本。
func (s *ConfigSet) Active() []*config.StrategyConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.arms) == 0 {
		if c := s.base.Get(); c != nil {
			return []*config.StrategyConfig{c}
		}
		return nil
	}
	names := make([]config.StrategyName, 0, len(s.arms))
	for n := range s.arms {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	out := make([]*config.StrategyConfig, 0, len(names))
	for _, n := range names {
		if c := s.arms[n].Get(); c != nil {
			out = append(out, c)
		}
	}
	return out
}

// IsArmed は「no_trade 以外の戦略が 1 つでも載っているか」= 発注を試みうる銘柄。
func (s *ConfigSet) IsArmed() bool {
	for _, c := range s.Active() {
		if c != nil && c.StrategyName != config.StrategyNoTrade {
			return true
		}
	}
	return false
}

// ConfigIDs は Active() と同じ順の config_id。ダッシュボードと reconcile 用。
func (s *ConfigSet) ConfigIDs() []string {
	act := s.Active()
	out := make([]string, 0, len(act))
	for _, c := range act {
		if c != nil {
			out = append(out, c.ConfigID)
		}
	}
	return out
}
