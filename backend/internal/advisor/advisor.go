// Package advisor builds a READ-ONLY "decision packet" for a single symbol: the
// deterministic facts a human (or an LLM go/no-go step) needs to decide whether
// to enter and, if so, what OCO to place — computed from local daily candles
// with the SAME screeners and exit geometry the bot uses.
//
// It is deliberately NOT a broker and NOT an order path. It places nothing,
// needs no account, and reuses strategy.DefaultScreeners + strategy.BNFReversionExit
// so a manually-executed OCO on SBI mirrors exactly what the bot would freeze at
// entry. The fresh news/catalyst (the LLM + Playwright step) is layered on top of
// this packet OUTSIDE the binary — keeping the LLM out of any real-time order
// path (CLAUDE.md: advisor only, 既定 OFF; TP/SL は必ず broker 側).
//
// **幾何の契約**: BNFReversionExit / DefaultBNFStopATR / BNFATRPeriod を消費する
// ことで「Evaluate が建玉時に凍結するのと同一の出口」をミラーしている。bnf.go の
// 出口だけを変えるとこの契約が黙って破れるので、必ず両方を同時に動かす。
package advisor

import (
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/domain/ta"
)

// OCOLeg is a candidate bracket for a manual OCO: absolute broker prices already
// rounded to the 呼値 grid. TakeProfit/StopLoss == 0 means "not quoted" (no valid
// setup). Alignment flags are a self-check that the rounding landed on a tick.
type OCOLeg struct {
	Strategy   string  `json:"strategy"`
	Side       string  `json:"side"`
	Entry      float64 `json:"entry"`
	TakeProfit float64 `json:"take_profit"`
	StopLoss   float64 `json:"stop_loss"`
	TPJPY      float64 `json:"tp_jpy"` // 建値からの利確幅(円/株)
	SLJPY      float64 `json:"sl_jpy"` // 建値からの損切幅(円/株)
	TPAligned  bool    `json:"tp_aligned"`
	SLAligned  bool    `json:"sl_aligned"`
}

// Packet is the full advisory view for one symbol at one point in time.
type Packet struct {
	Symbol    string  `json:"symbol"`
	AsOf      string  `json:"as_of"`
	Bars      int     `json:"bars"`
	LastClose float64 `json:"last_close"`
	Entry     float64 `json:"entry"`
	TickSize  float64 `json:"tick_size"`
	SMA25     float64 `json:"sma25"`
	// ATR14 は出口幅のスケール。BNF の SL は **建値の固定率では
	// なく 2.0·ATR** なので、手動執行者が同じ SL を再現するには packet に ATR が
	// 要る(Evaluate が凍結するのと同一の幾何をミラーする契約)。
	ATR14    float64              `json:"atr14"`
	Screens  []strategy.Candidate `json:"screens"`
	Best     *strategy.Candidate  `json:"best,omitempty"`
	OCO      OCOLeg               `json:"oco"`
	Advisory bool                 `json:"advisory"`
	Notes    []string             `json:"notes"`
}

// Build assembles the packet from `daily` candles (oldest→newest). entryOverride
// > 0 quotes the OCO off that price instead of the last close (e.g. to model a
// limit entry). It performs no I/O and mutates nothing.
func Build(symbol string, daily []market.Candle, asof string, entryOverride float64) Packet {
	p := Packet{Symbol: symbol, AsOf: asof, Bars: len(daily), Advisory: true}
	p.Notes = disciplineNotes()
	if len(daily) == 0 {
		p.Notes = append(p.Notes, "no_daily_data")
		return p
	}

	p.LastClose = daily[len(daily)-1].Close
	p.Entry = p.LastClose
	if entryOverride > 0 {
		p.Entry = entryOverride
	}
	p.TickSize = market.TickSizeOf(symbol, p.Entry)

	p.Screens = strategy.RankCandidates(
		map[string][]market.Candle{symbol: daily}, strategy.DefaultScreeners())
	if len(p.Screens) > 0 {
		best := p.Screens[0]
		p.Best = &best
	}

	if len(daily) >= strategy.BNFSMAPeriod {
		p.SMA25 = ta.SMACloses(daily, strategy.BNFSMAPeriod)
	}
	p.ATR14 = ta.ATR(daily, strategy.BNFATRPeriod)
	p.OCO = bnfLongOCO(symbol, p.Entry, p.SMA25, p.ATR14, &p.Notes)
	return p
}

// bnfLongOCO quotes the capped-BNF long bracket (TP toward the 25-MA, 2.0·ATR
// stop). It declines to quote — appending a note — when the 25-MA or the ATR is
// unavailable, or the price is not below the MA (no panic-crash long setup),
// rather than emit a TP that sits below the entry or an SL of zero width.
func bnfLongOCO(symbol string, entry, sma25, atr float64, notes *[]string) OCOLeg {
	leg := OCOLeg{Strategy: string(config.StrategyBNFReversion), Side: string(order.SideBuy), Entry: entry}
	switch {
	case sma25 <= 0:
		*notes = append(*notes, "sma25_unavailable__oco_omitted")
		return leg
	case sma25 <= entry:
		*notes = append(*notes, "price_at_or_above_25ma__no_bnf_long_setup__oco_omitted")
		return leg
	case atr <= 0:
		*notes = append(*notes, "atr_unavailable__oco_omitted")
		return leg
	}
	leg.TPJPY, leg.SLJPY = strategy.BNFReversionExit(entry, sma25, strategy.DefaultBNFStopATR, atr)
	leg.TakeProfit, leg.StopLoss = position.TPSLPricesFromJPY(symbol, order.SideBuy, entry, leg.TPJPY, leg.SLJPY)
	leg.TPAligned = market.IsTickAlignedOf(symbol, leg.TakeProfit)
	leg.SLAligned = market.IsTickAlignedOf(symbol, leg.StopLoss)
	return leg
}

// disciplineNotes are the fixed guardrails stamped on every packet — the
// invariants a manual executor must not forget (CLAUDE.md エッジ規律 / 不変条件).
func disciplineNotes() []string {
	return []string{
		"ADVISORY ONLY — this tool places NO order and needs no account; you execute manually.",
		"Edge UNPROVEN: bnf_reversion is a candidate, not a proven edge. Do NOT size up to chase.",
		"If you enter: set BOTH TP and SL broker-side (SBI 逆指値/OCO). Never rely on watching the screen.",
		"TP/SL are rounded to the 呼値 tick grid; verify tp_aligned/sl_aligned are true before sending.",
		"Fresh news/catalyst is NOT in this packet — that is the separate LLM + Playwright go/no-go step.",
	}
}
