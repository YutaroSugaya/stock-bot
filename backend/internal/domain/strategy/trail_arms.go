package strategy

import (
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
)

// トレンド系4戦略の **入口が完全に同一で出口だけが違う兄弟アーム**。
//
// 🛑 **v3(置き換え)にはしない。** v2 は v1 を置き換えた(v1 は測定しない)が、同じやり方で
// v3 を作ると v2 が消え、「固定 TP と トレール のどちらが良いか」という**問い自体が
// 測れなくなる**。`bnf_reversion` / `bnf_reversion_trail` と同じ兄弟にする。
//
// 構造的な根拠(**損益を見ずに言える**ことだけ): この 4 戦略はトレンドフォローで、
// SL に **Turtle の 2N ストップ(2.0×ATR)** を借りているのに、**Turtle が持っていなかった
// 固定 TP(3.0×ATR)を後から足している**。トレンドフォローの前提は「損益の大半は少数の
// 特大の勝ちから来る」で、固定 TP は**その特大の勝ちだけを狙い撃ちで切り落とす**。
// 出自の違う 2 つを組み合わせた幾何の矛盾で、約定を 1 本も見なくても指摘できる。
// 🛑 過去の損益は根拠に使わない(donchian_v2 が負けていることは検定の理由にならない)。
//
// 🛑 **これは (銘柄, 戦略) キーが入って初めて成立する。**
// 1銘柄1ポジのままだと 2 アームは同じトリガーを
// 奪い合うだけで、bnf で実際にそうなった(trigger は 118本/11銘柄で完全一致なのに
// picked 31 vs 10、**ペア 0 本**)。

// enterIfClearsTrail は 4 アーム共有の出口。**入口の判定は呼び手(v2 と同じヘルパ)が
// 済ませてあり、ここは幾何だけを差し替える**。
//
// 兄弟アームの表:
//   - 入口 / SL(2.0×ATR)/ MaxHold(戦略別)は兄弟と**同じ**
//   - TP は**無し**(uncapped)
//   - ratchet は BNF から **借用**: arm 1.0×ATR / giveback 1.5×ATR
//
// 🛑 **新しい自由パラメータはゼロ。** 係数を選び直すと「どの係数が良いか」の探索空間が
// 生まれ、多重検定への対抗手段(パラメータの事前固定)が 1 つ壊れる。
//
// 🚨 `applyCostFloor` は `max(TP, RatchetArm)` を床に当てるので、**trail 側の
// 対象は 1.0×ATR = v2 の 3 倍厳しい**。呼値の粗い/スプレッドの広い銘柄では v2 は建つのに
// trail は `tp_below_cost_floor` で落ちる。**床は緩めない** — trail の最初の利益目標が
// 往復コストを超えないなら、そのトレードは trail にとって成立していない。緩めて揃えるのは
// 嘘。代わりに**壊れたペアの件数を数える**(reject 理由がそのキー)。
func enterIfClearsTrail(in EvalInput, side order.Side, name config.StrategyName, reason string) Signal {
	sig := enterIfClears(in, side, name, reason, dcSlippageTicks, dcMinEdgeMultiple)
	if sig.Decision != DecisionEnter {
		return sig // 入口ゲート(direction / no_atr)で落ちた — 兄弟と同じ理由になる
	}
	atr := atrOf(in)
	if atr <= 0 {
		return noTradeSignal(in, name, "no_atr")
	}
	sig.TakeProfitJPY = 0 // uncapped: 固定 TP が特大の勝ちを切り落とすのを止める
	sig.RatchetArmJPY = bnfTrailArmATR * atr
	sig.RatchetGivebackJPY = bnfTrailGiveATR * atr
	// 床の再判定。TP を外して ratchet を付けたので、床に当てる対象が
	// 3.0×ATR → 1.0×ATR に変わる(非対称そのもの)。
	return applyCostFloor(in, sig, CostFloor{
		SpreadTicks:     in.Summary.CurrentRate.SpreadTicks,
		SlippageTicks:   dcSlippageTicks,
		MinEdgeMultiple: dcMinEdgeMultiple,
		TickSize:        summaryTickSize(in),
	})
}

// atrOf は enterIfClears が使うのと同じ ATR(期間も同じ)。2 箇所で別の期間を使うと
// 「SL は 2.0×ATR(14) なのに ratchet は別の ATR」という静かなズレになる。
func atrOf(in EvalInput) float64 { return atrExitPeriodATR(in.CandlesDaily) }

// AbsMomentumV2Trail は abs_momentum_v2 の uncapped 兄弟。
type AbsMomentumV2Trail struct{}

func (AbsMomentumV2Trail) Name() config.StrategyName { return config.StrategyAbsMomentumV2Trail }

func (s AbsMomentumV2Trail) Evaluate(in EvalInput) Signal {
	name := s.Name()
	if sig, ok := evalGuard(in, name, absTrendSMA+2); !ok {
		return sig
	}
	d := in.CandlesDaily
	side := absMomentumSideAt(d) // ★ 兄弟と**同じ関数**。分岐させると「入口同一」が崩れる
	if side == "" {
		return noTradeSignal(in, name, "no_abs_momentum")
	}
	if absMomentumSideAt(d[:len(d)-1]) == side {
		return noTradeSignal(in, name, "not_fresh_cross")
	}
	return enterIfClearsTrail(in, side, name, "abs_momentum_fresh_cross")
}

func (AbsMomentumV2Trail) Screen(symbol string, d []market.Candle) Candidate {
	return relabel(AbsMomentumV2{}.Screen(symbol, d), config.StrategyAbsMomentumV2Trail)
}

// ATRBreakoutV2Trail は atr_breakout_v2 の uncapped 兄弟。
type ATRBreakoutV2Trail struct{}

func (ATRBreakoutV2Trail) Name() config.StrategyName { return config.StrategyATRBreakoutV2Trail }

func (s ATRBreakoutV2Trail) Evaluate(in EvalInput) Signal {
	name := s.Name()
	if sig, ok := evalGuard(in, name, atrTrendSMA+1); !ok {
		return sig
	}
	side, reason := atrBreakoutSideAt(in.CandlesDaily)
	if side == "" {
		return noTradeSignal(in, name, reason)
	}
	return enterIfClearsTrail(in, side, name, "atr_breakout_from_compression")
}

func (ATRBreakoutV2Trail) Screen(symbol string, d []market.Candle) Candidate {
	return relabel(ATRBreakoutV2{}.Screen(symbol, d), config.StrategyATRBreakoutV2Trail)
}

// DonchianBreakoutV2Trail は donchian_breakout_v2 の uncapped 兄弟。
type DonchianBreakoutV2Trail struct{}

func (DonchianBreakoutV2Trail) Name() config.StrategyName {
	return config.StrategyDonchianBreakoutV2Trail
}

func (s DonchianBreakoutV2Trail) Evaluate(in EvalInput) Signal {
	name := s.Name()
	if sig, ok := evalGuard(in, name, dbWindow+2); !ok {
		return sig
	}
	d := in.CandlesDaily
	side := donchianSideAt(d) // ★ 兄弟と同じ関数
	if side == "" {
		return noTradeSignal(in, name, "no_breakout")
	}
	if donchianSideAt(d[:len(d)-1]) == side {
		return noTradeSignal(in, name, "not_first_breakout")
	}
	return enterIfClearsTrail(in, side, name, "donchian_first_breakout")
}

func (DonchianBreakoutV2Trail) Screen(symbol string, d []market.Candle) Candidate {
	return relabel(DonchianBreakoutV2{}.Screen(symbol, d), config.StrategyDonchianBreakoutV2Trail)
}

// High52wMomentumTrail は high_52w_momentum の uncapped 兄弟。
//
// ⚠ この 1 本は **配線バグ修正(日足 250 → 253 本)が効いて初めて標本が出る**。
// 追加以来、基のアームも長く建玉ゼロだった。
type High52wMomentumTrail struct{}

func (High52wMomentumTrail) Name() config.StrategyName { return config.StrategyHigh52wMomentumTrail }

func (s High52wMomentumTrail) Evaluate(in EvalInput) Signal {
	name := s.Name()
	if sig, ok := evalGuard(in, name, h52Window+1); !ok {
		return sig
	}
	side, reason := high52wSideAt(in.CandlesDaily)
	if side == "" {
		return noTradeSignal(in, name, reason)
	}
	return enterIfClearsTrail(in, side, name, "high_52w_momentum")
}

func (High52wMomentumTrail) Screen(symbol string, d []market.Candle) Candidate {
	return relabel(High52wMomentum{}.Screen(symbol, d), config.StrategyHigh52wMomentumTrail)
}

// relabel は兄弟の Screen 結果のラベルだけを張り替える。**独自実装しない** —
// スクリーン(画面と枠配布)と入口(発注判定)がズレると、枠は取れるのに永久に
// 建たないアームができる(2026-07〜08 の high_52w がまさにそれ)。
func relabel(c Candidate, name config.StrategyName) Candidate {
	c.Strategy = name
	return c
}
