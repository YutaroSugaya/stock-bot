package query

import (
	"context"
	"sort"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/port"
)

// 日次総評の段1 = **決定論パケット**。数字はここで作り、解釈は段2
// (LLM)が JSON だけを入力に書く。LLM に数字を作らせない — advisor.Build で確立して
// いる規律と同じ。
//
// 🛑 これは日記の材料であって**エッジの証拠ではない**。撤退・継続・ロットの判定は
// 事前にコミットした基準と cmd/edge-judge が出す。

type DailyJournal struct {
	Date       string                 `json:"date"` // JST の YYYY-MM-DD
	Trades     JournalTradeStats      `json:"trades"`
	Entries    []JournalEntryView     `json:"entries"`
	Open       JournalOpenStats       `json:"open"`
	Rejections []JournalRejectionView `json:"rejections"`
	Screens    []JournalScreenView    `json:"screens"`
}

// JournalTradeStats は**口座ベース**の戦績。
// `entry_compensated`(約定後に守りを置けず巻き戻した往復)と `external_close`
// (人間が建てた建玉の決済)も**普通のトレードとして戦略に計上する** — 別枠に逃がすと、
// 全件がそれだった日に「今日は何も無かった」と読める総評になる。
// **何がどう閉じたかは ByReason と Trades の close_reason に残る**ので、段2 は
// 「戦略の出口だったか」をそこから読む。
//
// 🛑 エッジ標本(cmd/forward-report → cmd/edge-judge)は別で、そちらは戦略の出口だけ
// (`port.IsNonStrategyClose` で除外・事前コミット)。**日記の数字を判定に持ち込まない。**
type JournalTradeStats struct {
	N      int `json:"n"`
	Wins   int `json:"wins"`
	Losses int `json:"losses"`
	Flat   int `json:"flat"`
	// net = gross − fee + carry(台帳の規約)。gross だけを見て判断しない。
	GrossJPY   float64               `json:"gross_jpy"`
	FeeJPY     float64               `json:"fee_jpy"`
	CarryJPY   float64               `json:"carry_jpy"`
	NetJPY     float64               `json:"net_jpy"`
	ByReason   map[string]int        `json:"by_reason"`
	ByStrategy []JournalStrategyStat `json:"by_strategy"`
	Trades     []JournalClosedTrade  `json:"closed"`
}

// port の DTO をそのまま JSON にしない: 段2(LLM)への契約はここのタグが決める。
type JournalRejectionView struct {
	Reason string `json:"reason"`
	N      int    `json:"n"`
}

type JournalScreenView struct {
	Strategy   string   `json:"strategy"`
	Triggered  int      `json:"triggered"`
	Picked     int      `json:"picked"`
	TopSymbols []string `json:"top_symbols"`
}

type JournalStrategyStat struct {
	Strategy string  `json:"strategy"`
	N        int     `json:"n"`
	Wins     int     `json:"wins"`
	NetJPY   float64 `json:"net_jpy"`
}

type JournalClosedTrade struct {
	Symbol      string  `json:"symbol"`
	Strategy    string  `json:"strategy"`
	Quantity    int     `json:"quantity"`
	EntryPrice  float64 `json:"entry_price"`
	ClosePrice  float64 `json:"close_price"`
	NetJPY      float64 `json:"net_jpy"`
	CloseReason string  `json:"close_reason"`
	ClosedAt    string  `json:"closed_at"`
}

type JournalEntryView struct {
	Symbol     string  `json:"symbol"`
	Strategy   string  `json:"strategy"`
	ConfigID   string  `json:"config_id"`
	Side       string  `json:"side"`
	Quantity   int     `json:"quantity"`
	EntryPrice float64 `json:"entry_price"`
	OpenedAt   string  `json:"opened_at"`
}

type JournalOpenStats struct {
	N         int               `json:"n"`
	Positions []JournalOpenView `json:"positions"`
}

type JournalOpenView struct {
	Symbol     string  `json:"symbol"`
	Strategy   string  `json:"strategy"`
	Side       string  `json:"side"`
	Quantity   int     `json:"quantity"`
	EntryPrice float64 `json:"entry_price"`
	OpenedAt   string  `json:"opened_at"`
	HeldDays   int     `json:"held_days"` // 暦日

	// 凍結した出口幾何(絶対価格)。**いまの config ではなく建玉時の値**。
	TakeProfitPrice float64 `json:"take_profit_price"`
	StopLossPrice   float64 `json:"stop_loss_price"`

	// 🚨 **円/株**。MFE/MAE(migration 0010)は「そのトレードが実際どこまで動いたか」で、
	// 幅(TP/SL)が届く幅なのかを読む唯一の材料。
	PeakPerShareJPY   float64 `json:"peak_per_share_jpy"`
	TroughPerShareJPY float64 `json:"trough_per_share_jpy"`

	// 以下は app 層が当日の日足から埋める(台帳には無い)。query は触らない。
	ClosePrice            float64 `json:"close_price,omitempty"`
	UnrealizedPerShareJPY float64 `json:"unrealized_per_share_jpy,omitempty"`
	UnrealizedJPY         float64 `json:"unrealized_jpy,omitempty"`
	// 🛑 当日の日足が無い日は数字を 0 で埋めず、ここに理由を入れる。
	// 0 のままだと「含みゼロ」に化ける(引け直後は必ずこの状態になる)。
	Unavailable string `json:"unavailable,omitempty"`
}

type BuildDailyJournal struct{ src port.JournalSource }

func NewBuildDailyJournal(src port.JournalSource) *BuildDailyJournal {
	return &BuildDailyJournal{src: src}
}

// Execute assembles one JST day. day は同日中のどの時刻でもよい(JST 日へ丸める)。
func (b *BuildDailyJournal) Execute(ctx context.Context, day time.Time) (DailyJournal, error) {
	from := jstDayStart(day)
	to := from.AddDate(0, 0, 1)
	out := DailyJournal{
		Date:       from.Format("2006-01-02"),
		Entries:    []JournalEntryView{},
		Rejections: []JournalRejectionView{},
		Screens:    []JournalScreenView{},
	}

	trades, err := b.src.JournalClosedTrades(ctx, from, to)
	if err != nil {
		return out, err
	}
	out.Trades = summariseTrades(trades)

	opened, err := b.src.JournalOpenedPositions(ctx, from, to)
	if err != nil {
		return out, err
	}
	for _, p := range opened {
		out.Entries = append(out.Entries, JournalEntryView{
			Symbol: p.Symbol, Strategy: p.Strategy, ConfigID: p.ConfigID, Side: p.Side,
			Quantity: p.Quantity, EntryPrice: p.EntryPrice, OpenedAt: p.OpenedAt.In(clock.JST).Format(time.RFC3339),
		})
	}

	// 引け時点 = その日の終わり。走るのは 15:40 だが、遅れて走った日でも同じ数字になる。
	open, err := b.src.JournalOpenPositionsAsOf(ctx, to)
	if err != nil {
		return out, err
	}
	out.Open = JournalOpenStats{N: len(open), Positions: []JournalOpenView{}}
	for _, p := range open {
		out.Open.Positions = append(out.Open.Positions, JournalOpenView{
			Symbol: p.Symbol, Strategy: p.Strategy, Side: p.Side, Quantity: p.Quantity, EntryPrice: p.EntryPrice,
			OpenedAt:        p.OpenedAt.In(clock.JST).Format(time.RFC3339),
			HeldDays:        int(jstDayStart(to.Add(-time.Nanosecond)).Sub(jstDayStart(p.OpenedAt)).Hours() / 24),
			TakeProfitPrice: p.TakeProfitPrice, StopLossPrice: p.StopLossPrice,
			PeakPerShareJPY: p.PeakUnrealizedJPY, TroughPerShareJPY: p.TroughUnrealizedJPY,
		})
	}

	rej, err := b.src.JournalRejections(ctx, from, to)
	if err != nil {
		return out, err
	}
	sort.SliceStable(rej, func(i, j int) bool {
		if rej[i].N != rej[j].N {
			return rej[i].N > rej[j].N
		}
		return rej[i].Reason < rej[j].Reason
	})
	for _, r := range rej {
		out.Rejections = append(out.Rejections, JournalRejectionView{Reason: r.Reason, N: r.N})
	}

	scr, err := b.src.JournalScreens(ctx, from, to)
	if err != nil {
		return out, err
	}
	sort.SliceStable(scr, func(i, j int) bool {
		if scr[i].Triggered != scr[j].Triggered {
			return scr[i].Triggered > scr[j].Triggered
		}
		return scr[i].Strategy < scr[j].Strategy
	})
	for _, s := range scr {
		out.Screens = append(out.Screens, JournalScreenView{
			Strategy: s.Strategy, Triggered: s.Triggered, Picked: s.Picked, TopSymbols: s.TopSymbols,
		})
	}
	return out, nil
}

func summariseTrades(trades []port.JournalTrade) JournalTradeStats {
	st := JournalTradeStats{ByReason: map[string]int{}, ByStrategy: []JournalStrategyStat{}, Trades: []JournalClosedTrade{}}
	perStrategy := map[string]*JournalStrategyStat{}
	for _, t := range trades {
		net := t.NetJPY()
		// 人間が建てた external 建玉には戦略が無い。**空文字のまま出さない** —
		// 段2 が `"strategy": ""` を「戦略名の欠落」と読む。forward-report と同じ
		// unknown に寄せて、同じ表の 1 行として見せる。
		strat := t.Strategy
		if strat == "" {
			strat = "unknown"
		}
		view := JournalClosedTrade{
			Symbol: t.Symbol, Strategy: strat, Quantity: t.Quantity,
			EntryPrice: t.EntryPrice, ClosePrice: t.ClosePrice, NetJPY: net,
			CloseReason: t.CloseReason, ClosedAt: t.ClosedAt.In(clock.JST).Format(time.RFC3339),
		}
		st.N++
		switch {
		case net > 0:
			st.Wins++
		case net < 0:
			st.Losses++
		default:
			st.Flat++ // 0 を勝ちにも負けにも数えない(勝率の分母を静かに歪めない)
		}
		st.GrossJPY += t.GrossJPY
		st.FeeJPY += t.FeeJPY
		st.CarryJPY += t.CarryJPY
		st.ByReason[t.CloseReason]++
		s, ok := perStrategy[strat]
		if !ok {
			s = &JournalStrategyStat{Strategy: strat}
			perStrategy[strat] = s
		}
		s.N++
		s.NetJPY += net
		if net > 0 {
			s.Wins++
		}
		st.Trades = append(st.Trades, view)
	}
	st.NetJPY = st.GrossJPY - st.FeeJPY + st.CarryJPY
	for _, s := range perStrategy {
		st.ByStrategy = append(st.ByStrategy, *s)
	}
	sort.SliceStable(st.ByStrategy, func(i, j int) bool {
		if st.ByStrategy[i].N != st.ByStrategy[j].N {
			return st.ByStrategy[i].N > st.ByStrategy[j].N
		}
		return st.ByStrategy[i].Strategy < st.ByStrategy[j].Strategy
	})
	return st
}

func jstDayStart(t time.Time) time.Time {
	j := t.In(clock.JST)
	return time.Date(j.Year(), j.Month(), j.Day(), 0, 0, 0, 0, clock.JST)
}
