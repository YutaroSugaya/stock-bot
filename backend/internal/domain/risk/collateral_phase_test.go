package risk

import (
	"strings"
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
)

func phaseCfg() *config.StrategyConfig {
	c := &config.StrategyConfig{ConfigID: "cfg-1", Symbol: "7203"}
	c.Entry.Direction = config.DirectionBoth
	c.Risk.MaxOpenPositions = 1
	return c
}

func phaseSig() strategy.Signal {
	return strategy.Signal{Decision: strategy.DecisionEnter, Side: order.SideBuy, Symbol: "7203", EntryPrice: 2500, Quantity: 100}
}

// EvaluateStructural は broker 由来のフィールドを **一切読まない**。読まないことの
// 証明として、余力が「不明」(= 未照会。full ゲートなら即 reject)でも構造的な理由が
// 返ることを確かめる。これが成り立つから、呼び出し側は照会前に構造ゲートを回せる。
func TestEvaluateStructural_IgnoresMarginFields(t *testing.T) {
	snap := AccountSnapshot{
		MarginStatusUnknown:     true, // 未照会
		AccountMaxOpenPositions: 2,
		AccountOpenPositions:    2, // 口座枠が満杯
		CollateralRequiredJPY:   75000,
		AvailableToTradeJPY:     0,
	}
	d := EvaluateStructural(phaseSig(), phaseCfg(), snap, nil)
	if d.Allowed {
		t.Fatal("口座枠が満杯なのに構造ゲートが通した")
	}
	if !strings.HasPrefix(d.Reason, "account_open_positions") {
		t.Fatalf("構造ゲートが余力の理由を返した(broker 由来フィールドを読んでいる): %q", d.Reason)
	}
}

// 構造ゲートを通っても、余力が未照会なら full ゲートは fail-close する。
// 「照会を遅延させたら担保チェックが消えた」を防ぐ不変条件。
func TestEvaluateSignal_UnfetchedMarginFailsClose(t *testing.T) {
	snap := AccountSnapshot{
		MarginStatusUnknown:   true, // BuildStructural 直後の状態
		CollateralRequiredJPY: 75000,
	}
	d := EvaluateSignal(phaseSig(), phaseCfg(), snap, nil)
	if d.Allowed {
		t.Fatal("余力未照会の snapshot でエントリーが通った(担保チェックが素通り)")
	}
	if d.Reason != "margin_status_unavailable" {
		t.Fatalf("reason = %q, want margin_status_unavailable", d.Reason)
	}
}

// EvaluateCollateral は担保まわりだけを見る。構造的に問題があっても、そちらは
// 構造ゲートの担当なので通す(二重に判定して理由が入れ替わるのを防ぐ)。
func TestEvaluateCollateral_OnlyJudgesCollateral(t *testing.T) {
	snap := AccountSnapshot{
		AccountMaxOpenPositions: 2,
		AccountOpenPositions:    2, // 構造的には NG だが担保ゲートの担当外
		CollateralRequiredJPY:   75000,
		AvailableToTradeJPY:     100000,
	}
	if d := EvaluateCollateral(phaseSig(), snap); !d.Allowed {
		t.Fatalf("担保は足りているのに reject された: %q", d.Reason)
	}

	snap.AvailableToTradeJPY = 1000
	if d := EvaluateCollateral(phaseSig(), snap); d.Allowed {
		t.Fatal("余力不足を通した")
	}
}
