package risk

import (
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/order"
)

// 日中版は「**同日 1 回転**・持ち越し無し」。14:25 の時間切れで閉じた直後に
// 同じ戦略で買い直す経路がある(事前登録に反する 2 回転目)。
// 利益で閉じた直後は cooldown(損失後 30 分)が効かないので、回数で止める。
// 数えるのは (銘柄, **戦略**) — 兄弟の `_trail` は別戦略なのでペアの 2 本目は止めない。
func TestIntradayEntersAtMostOncePerDayPerStrategy(t *testing.T) {
	cfg := nanpinCfg(config.ModePaper)
	cfg.HoldingMode = order.HoldingIntraday
	cfg.Risk.MaxOpenPositions = 0
	sig := nanpinSig(config.StrategyBNFIntradayReversion, order.SideBuy)
	sig.HoldingMode = order.HoldingIntraday

	snap := nanpinSnap()
	if d := EvaluateStructural(sig, cfg, snap, nil); !d.Allowed {
		t.Fatalf("その日初めての日中の建ては通る: reason=%q", d.Reason)
	}
	snap.EntriesTodaySameStrategy = 1
	d := EvaluateStructural(sig, cfg, snap, nil)
	if d.Allowed || d.Reason != "intraday_once_per_day (1 entries today)" {
		t.Fatalf("同日 2 回目の日中の建てを止めていない: allowed=%v reason=%q", d.Allowed, d.Reason)
	}

	// 多日建玉は対象外(多日の再入場はナンピン禁止と cooldown の管轄)。
	multi := nanpinSig(config.StrategyBNFReversion, order.SideBuy)
	if d := EvaluateStructural(multi, nanpinCfg(config.ModePaper), snap, nil); d.Reason == "intraday_once_per_day (1 entries today)" {
		t.Fatal("多日の建てを日中の回数上限で止めている")
	}
}
