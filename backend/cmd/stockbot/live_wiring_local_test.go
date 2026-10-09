package main

import (
	"os"
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/strategy"
)

// live トラックの配線ガード。**この機体にしか無い gitignored な設定**
// (configs/bot_config.live.yaml / configs/strategy_config.live.yaml)を読むので、
// 無い環境では skip する。
//
// 🚨 なぜ要るか: live で戦略を差し替えるとき、**通らなければならないゲートが 4 つ**
// あり(カタログの LiveArmable / live allowlist / engine 登録 / hard limits)、どれか
// 1 つでも欠けると **bot は起動時に error で落ちるか、起動しても永久に arm しない**。
// v2 の 3 本が「menu と screener には載っているのに engine 未登録」で
// 標本ゼロのまま気づけなかった事故があり、live で同じことが起きると
// 「動いているのに 1 本も建たない日」が黙って続く。
//
// 🛑 skip は「合格」ではない。live 設定を持つ機体でだけ意味がある。
func TestLiveTrackWiringForCommittedTemplate(t *testing.T) {
	const (
		hlPath  = "../../../configs/hard_limits.yaml"
		botPath = "../../../configs/bot_config.live.yaml"
		stgPath = "../../../configs/strategy_config.live.yaml"
	)
	for _, p := range []string{botPath, stgPath} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("live 設定が無い環境なので skip(%s)", p)
		}
	}
	hl, err := config.LoadHardLimits(hlPath)
	if err != nil {
		t.Fatalf("hard_limits: %v", err)
	}
	botCfg, err := config.LoadBotConfig(botPath)
	if err != nil {
		t.Fatalf("bot_config.live: %v", err)
	}
	cfg, err := config.LoadStrategyConfig(stgPath)
	if err != nil {
		t.Fatalf("strategy_config.live: %v", err)
	}
	if botCfg.Mode != config.ModeLive {
		t.Fatalf("bot_config.live の mode = %q(live_config のはず)", botCfg.Mode)
	}
	if !botCfg.Selector.Enabled {
		// selector 無しなら symbols を直接見る構成なので、この検査の前提が違う。
		t.Skip("selector.enabled が false — このガードは selector 構成専用")
	}

	// ①② selector が arm できる戦略か(出口を自前計算し、ランキング用の screener がある)。
	// arm できないと live track は起動時に error で落ちる / 永久に建たない。
	if !strategy.LiveArmable(cfg.StrategyName) {
		t.Fatalf("%s は live で arm できない(戦略カタログの LiveArmable)", cfg.StrategyName)
	}
	// ③ 実弾の人間ゲート。allowlist が空なら live は no_trade 以外を起動しない構成なので、
	// ローカルの live config との整合を見る意味が無い。
	if len(hl.LiveAllowedStrategies) == 0 {
		t.Skip("live_allowed_strategies が空 — live_config は起動しない構成")
	}
	if !hl.AllowsLiveStrategy(cfg.StrategyName) {
		t.Fatalf("%s が live_allowed_strategies に無い — 昇格は人間 commit", cfg.StrategyName)
	}
	// ④ engine 登録(漏れると Arm で unregistered として弾かれる)
	if eng := buildStrategyEngine(func() string { return "wiring-guard" }); !eng.Has(cfg.StrategyName) {
		t.Fatalf("%s が engine に未登録 — 枠を消費した末に弾かれる", cfg.StrategyName)
	}
	// ⑤ "*" のユニバース config は per-symbol 展開してから hard limits を当てる
	// (起動時と同じ経路)。展開前に当てると "*" が銘柄ホワイトリストに無くて落ちる。
	sym := "7203"
	if len(botCfg.Symbols) > 0 {
		sym = botCfg.Symbols[0]
	}
	per := configForSymbol(cfg, sym)
	if per == nil {
		t.Fatalf("configForSymbol(%s) が nil", sym)
	}
	if err := per.ValidateAgainstHardLimits(hl); err != nil {
		t.Fatalf("hard limits を通らない: %v", err)
	}
}
