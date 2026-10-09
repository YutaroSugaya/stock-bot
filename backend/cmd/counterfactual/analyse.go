package main

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"stockbot/backend/internal/backtest"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

// 集計は **main から切り出した純関数**にしてある。ここが main() の中にあった頃は
// 「ambiguous に 0 を足す」「未決着の含みを決着損益と同じ列に足す」といった非対称が
// テストから到達できず、実際に両方入っていた。

type row struct {
	PositionID  int64   `json:"position_id"`
	Symbol      string  `json:"symbol"`
	Strategy    string  `json:"strategy"`
	CloseReason string  `json:"close_reason"`
	ClosedAt    string  `json:"closed_at"`
	RealisedNet float64 `json:"realised_net_jpy"`
	Outcome     string  `json:"cf_outcome"`
	CFDays      int     `json:"cf_days"`
	// SettledJPY は **バリアに到達した場合だけ**の推定損益(手数料・金利は含まない)。
	SettledJPY float64 `json:"cf_settled_gross_jpy,omitempty"`
	// UnrealisedJPY は未決着の含み(最終終値での mark-to-market)。**決着損益と足さない**。
	UnrealisedJPY float64 `json:"cf_unrealised_gross_jpy,omitempty"`
	Note          string  `json:"note,omitempty"`
}

type summary struct {
	Strategy   string `json:"strategy"`
	N          int    `json:"n"`
	TakeProfit int    `json:"cf_take_profit"`
	StopLoss   int    `json:"cf_stop_loss"`
	Ambiguous  int    `json:"cf_ambiguous"`
	Unresolved int    `json:"cf_unresolved"`
	// MaxHoldHit = 凍結された期限に達していた本数。**「手仕舞いしなければ伸びていた」
	// と読めない標本**がどれだけあるかを示す(反実仮想③の分母)。
	MaxHoldHit  int     `json:"cf_max_hold"`
	NoData      int     `json:"cf_no_data"`
	Skipped     int     `json:"skipped"`
	RealisedJPY float64 `json:"realised_net_jpy"`
	// 決着した本数ぶんだけの推定損益。**含み(未決着)を混ぜない**。
	SettledJPY    float64 `json:"cf_settled_gross_jpy"`
	SettledN      int     `json:"cf_settled_n"`
	UnrealisedJPY float64 `json:"cf_unrealised_gross_jpy"`
	UnrealisedN   int     `json:"cf_unrealised_n"`
}

type report struct {
	Rows       []row     `json:"rows"`
	ByStrategy []summary `json:"by_strategy"`
	FirstClose string    `json:"first_close"`
	LastClose  string    `json:"last_close"`
	AsOfBar    string    `json:"mtm_as_of_bar"` // 含みを測った日足の最終日(全銘柄で最新のもの)
	Warnings   []string  `json:"warnings,omitempty"`
}

// closeReasonMaxHold だけは「キャップが無ければどうだったか」を問う決済理由なので、
// 反実仮想の期限を**外して**歩く。他の理由は凍結された期限までしか歩けない。
const closeReasonMaxHold = "max_hold"

// barsFn は銘柄の日足(古い順)を返す。err はデータ欠測(推定しない)。
type barsFn func(symbol string) ([]market.Candle, error)

// cycle2Start は出口の規則が変わった境界。ここを跨いだ標本は出口幾何が別物(円建て → ATR)なので
// 同じ表に並べてはいけない(事前登録で「合算しない」と決めてある)。
var cycle2Start = cycleBoundary()

// cycleBoundary は現サイクルの開始日。**`STOCKBOT_CYCLE_SINCE` が唯一の定義**で
// (`stockbot-routine.sh` の自動集計と同じ変数)、未設定なら既定へ落ちる。
// Go 側に日付を焼くと、次のサイクルが始まった日に**警告が黙って出なくなる**。
func cycleBoundary() time.Time {
	if v := os.Getenv("STOCKBOT_CYCLE_SINCE"); v != "" {
		if t, err := time.ParseInLocation("2006-01-02", v, clock.JST); err == nil {
			return t
		}
	}
	return time.Date(2026, 8, 10, 0, 0, 0, 0, clock.JST)
}

func analyse(positions []port.ClosedPositionSnapshot, bars barsFn) report {
	rep := report{Rows: []row{}, ByStrategy: []summary{}}
	perStrategy := map[string]*summary{}
	get := func(name string) *summary {
		s, ok := perStrategy[name]
		if !ok {
			s = &summary{Strategy: name}
			perStrategy[name] = s
		}
		return s
	}
	var first, last, asOf time.Time
	for _, p := range positions {
		if first.IsZero() || p.ClosedAt.Before(first) {
			first = p.ClosedAt
		}
		if p.ClosedAt.After(last) {
			last = p.ClosedAt
		}
		s := get(p.Strategy)
		s.N++
		s.RealisedJPY += p.NetJPY

		candles, err := bars(p.Symbol)
		if err != nil {
			s.Skipped++ // 日足が無い銘柄は推定しない(欠測を 0 と混ぜない)
			continue
		}
		if n := len(candles); n > 0 && candles[n-1].OpenTime.After(asOf) {
			asOf = candles[n-1].OpenTime
		}
		// 🛑 分割の取り違えガード: CSV は連鎖調整で**過去バーが新スケールに書き換わる**が、
		// 建玉の凍結価格は旧スケールのまま。決済当日のバーと実約定値が桁違いなら、
		// その建玉は分割を跨いでいるので推定しない(買い建玉が初日に即 SL と誤判定する)。
		if !closePriceConsistent(candles, p) {
			s.Skipped++
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: 決済値 %.1f が決済日の日足と桁違い(分割の可能性)— 推定から除外",
				p.Symbol, p.ClosePrice))
			continue
		}

		side := order.SideBuy
		if strings.EqualFold(p.Side, string(order.SideSell)) {
			side = order.SideSell
		}
		// 🚨 **凍結された MaxHold より先は歩かない**。
		//
		// 問いは決済理由で変わる:
		//   - `max_hold` … 「**キャップが無ければ**どうだったか」= Until を渡さない
		//   - それ以外(manual / forced_flat / …)… 「**手仕舞いしなければ**どうだったか」
		//     = その建玉に凍結されていた期限までしか歩けない。渡さないと期限の後に
		//     来た TP/SL を拾い、**存在しない出口**を数える。
		//
		// config 系の戦略にも期限が付いているので、実質全件に効く。
		var until time.Time
		if p.CloseReason != closeReasonMaxHold && p.MaxHoldMinutes > 0 {
			until = p.OpenedAt.Add(time.Duration(p.MaxHoldMinutes) * time.Minute)
		}
		// 🚨 **トレール建玉は床で歩く**(`-floor`)。実際の出口は
		// `max(arm, peak − giveback)` の線であって固定 TP/SL ではないので、固定脚で
		// 歩いた結果は**別戦略の推定**。格子と実アームを突き合わせたときの
		// 不一致を「反実仮想の推定誤差」と誤読する。
		// 🛑 床の版スタンプ(`ratchet_floor_at_arm`)は**凍結値をそのまま渡す** —
		// false の建玉(旧台帳の旧建玉)に床を当て直すと測定対象が入れ替わる。
		if floorMode && p.RatchetArmJPY > 0 {
			tr := backtest.TrailCounterfactual(backtest.TrailCFInput{
				Side: side, ArmJPY: p.RatchetArmJPY, GivebackJPY: p.RatchetGivebackJPY,
				StopLossJPY: p.StopLossPrice, FloorAtArm: p.RatchetFloorAtArm,
				EntryPrice: p.EntryPrice, PeakAtClose: p.PeakUnrealizedJPY,
				Armed: p.PeakUnrealizedJPY >= p.RatchetArmJPY,
				After: p.ClosedAt, Until: until, Bars: candles,
			})
			rep.Rows = append(rep.Rows, applyTrail(s, p, tr))
			continue
		}
		res := backtest.Counterfactual(backtest.CFInput{
			Side: side, TakeProfitPrice: p.TakeProfitPrice, StopLossPrice: p.StopLossPrice,
			After: p.ClosedAt, Until: until, Bars: candles,
		})
		r := row{
			PositionID: p.PositionID, Symbol: p.Symbol, Strategy: p.Strategy, CloseReason: p.CloseReason,
			ClosedAt: p.ClosedAt.In(clock.JST).Format("2006-01-02"), RealisedNet: p.NetJPY,
			Outcome: string(res.Outcome), CFDays: res.Days,
		}
		if p.RatchetArmJPY > 0 {
			// トレール建玉の実際の出口は peak − giveback の線であって固定 TP/SL ではない。
			// 固定脚で歩いた結果は**別戦略の推定**なので、そう明記する。
			r.Note = "トレール建玉(実際の出口は ratchet 線)— 固定 TP/SL で歩いた推定なので他戦略と同列に読まない"
		}
		switch res.Outcome {
		case backtest.CFTakeProfit, backtest.CFStopLoss:
			exit := barrierFill(side, res, p)
			r.SettledJPY = grossJPY(side, p.EntryPrice, exit, p.Quantity)
			s.SettledJPY += r.SettledJPY
			s.SettledN++
			if res.Outcome == backtest.CFTakeProfit {
				s.TakeProfit++
			} else {
				s.StopLoss++
			}
		case backtest.CFAmbiguous:
			r.Note = joinNote(r.Note, "同一日足で TP/SL の両方に触れた(順序不明・推定損益を出さない)")
			s.Ambiguous++
		case backtest.CFMaxHold:
			// 期限で成行返済されていた。**決着した出口**なので含み(未決着)と混ぜない。
			r.SettledJPY = grossJPY(side, p.EntryPrice, res.LastClose, p.Quantity)
			s.SettledJPY += r.SettledJPY
			s.SettledN++
			s.MaxHoldHit++
			r.Note = joinNote(r.Note, "凍結された MaxHold の期限に達していた(手仕舞いしなくても期限で閉じていた)")
		case backtest.CFUnresolved:
			r.UnrealisedJPY = grossJPY(side, p.EntryPrice, res.LastClose, p.Quantity)
			r.Note = joinNote(r.Note, "未決着 — 最終終値での含み(決着損益ではない)")
			s.UnrealisedJPY += r.UnrealisedJPY
			s.UnrealisedN++
			s.Unresolved++
		default: // CFNoData
			r.Note = joinNote(r.Note, "決済後の日足がまだ無い(当日の日足は翌朝の fetch-daily 待ち)")
			s.NoData++
		}
		rep.Rows = append(rep.Rows, r)
	}

	for _, s := range perStrategy {
		rep.ByStrategy = append(rep.ByStrategy, *s)
	}
	sort.Slice(rep.ByStrategy, func(i, j int) bool {
		if rep.ByStrategy[i].N != rep.ByStrategy[j].N {
			return rep.ByStrategy[i].N > rep.ByStrategy[j].N
		}
		return rep.ByStrategy[i].Strategy < rep.ByStrategy[j].Strategy
	})
	if !first.IsZero() {
		rep.FirstClose = first.In(clock.JST).Format("2006-01-02")
		rep.LastClose = last.In(clock.JST).Format("2006-01-02")
	}
	if !asOf.IsZero() {
		rep.AsOfBar = asOf.In(clock.JST).Format("2006-01-02")
	}
	// 🛑 サイクル境界を跨いだ標本は出口幾何が別物。黙って 1 つの表に並べない。
	if !first.IsZero() && first.Before(cycle2Start) && !last.Before(cycle2Start) {
		rep.Warnings = append(rep.Warnings,
			"対象がサイクル境界(2026-08-10)を跨いでいる。出口幾何が別物(円建て → ATR)なので合算して読まない — -since で切ること")
	}
	return rep
}

// barrierFill は到達バーの約定価格。**窓を開けて飛び越えた日は寄り値**が実際の約定に
// なる(逆指値は不利側・TP 指値は有利側)。バリア価格をそのまま使うと、ギャップの
// ぶんだけ現実と系統的にずれる。
func barrierFill(side order.Side, res backtest.CFResult, p port.ClosedPositionSnapshot) float64 {
	barrier := p.StopLossPrice
	if res.Outcome == backtest.CFTakeProfit {
		barrier = p.TakeProfitPrice
	}
	if res.HitOpen <= 0 {
		return barrier
	}
	beyond := res.HitOpen >= barrier
	if side == order.SideSell {
		beyond = res.HitOpen <= barrier
	}
	if res.Outcome == backtest.CFTakeProfit {
		// 利確側: 寄りが既にバリアを越えていればそこで約定(有利)。
		if beyond {
			return res.HitOpen
		}
		return barrier
	}
	// 損切側: 寄りが既にバリアを割っていればそこで約定(不利)。
	if !beyond {
		return res.HitOpen
	}
	return barrier
}

// closePriceConsistent は決済当日の日足と実約定値の桁が合うかを見る。分割の連鎖調整で
// 過去バーだけが新スケールになると、凍結価格(旧スケール)との比較が壊れる。
func closePriceConsistent(bars []market.Candle, p port.ClosedPositionSnapshot) bool {
	if p.ClosePrice <= 0 {
		return true // 比較材料が無い(判定しない)
	}
	want := p.ClosedAt.In(clock.JST).Format("2006-01-02")
	for _, b := range bars {
		if b.OpenTime.In(clock.JST).Format("2006-01-02") != want || b.Close <= 0 {
			continue
		}
		// 1 日の値幅は制限値幅の内側なので、2 倍以上ずれていれば同じスケールではない。
		ratio := p.ClosePrice / b.Close
		return ratio > 0.5 && ratio < 2.0
	}
	return true // 決済日のバーが無い(判定材料なし)
}

func grossJPY(side order.Side, entry, exit float64, qty int) float64 {
	if exit <= 0 {
		return 0
	}
	diff := exit - entry
	if side == order.SideSell {
		diff = -diff
	}
	return math.Round(diff * float64(qty))
}

func joinNote(a, b string) string {
	if a == "" {
		return b
	}
	return a + " / " + b
}

// floorMode は `-floor`。パッケージ変数なのは analyse が純関数のまま
// テストから切り替えられるようにするため。
var floorMode bool

// applyTrail はトレール反実仮想の結果を 1 行 + 集計へ。
func applyTrail(s *summary, p port.ClosedPositionSnapshot, tr backtest.TrailCFResult) row {
	r := row{
		PositionID: p.PositionID, Symbol: p.Symbol, Strategy: p.Strategy, CloseReason: p.CloseReason,
		ClosedAt: p.ClosedAt.In(clock.JST).Format("2006-01-02"), RealisedNet: p.NetJPY,
		Outcome: string(tr.Outcome), CFDays: tr.Days,
		Note: "トレール反実仮想(床あり)— 固定 TP/SL で歩いた行と同じ表に並べない",
	}
	gross := math.Round(tr.ExitUnrealJPY * float64(p.Quantity))
	switch tr.Outcome {
	case backtest.TrailCFFloor, backtest.TrailCFStopLoss, backtest.TrailCFMaxHold:
		r.SettledJPY = gross
		s.SettledJPY += gross
		s.SettledN++
		switch tr.Outcome {
		case backtest.TrailCFFloor:
			s.TakeProfit++
		case backtest.TrailCFStopLoss:
			s.StopLoss++
		default:
			s.MaxHoldHit++
		}
	case backtest.TrailCFUnresolved:
		r.UnrealisedJPY = gross
		s.UnrealisedJPY += gross
		s.UnrealisedN++
		s.Unresolved++
	default:
		s.NoData++
	}
	return r
}
