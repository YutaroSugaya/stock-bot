package backtest

import (
	"encoding/json"
	"math"
	"sort"
)

// MarshalJSON emits non-finite PF/RR (+Inf on a no-loss series, NaN on empty)
// as null: encoding/json errors on them, which would drop the whole report.
func (m Metrics) MarshalJSON() ([]byte, error) {
	type alias Metrics
	return json.Marshal(struct {
		alias
		GrossPF *float64 `json:"gross_pf"`
		NetPF   *float64 `json:"net_pf"`
		RR      *float64 `json:"rr"`
	}{alias(m), finite(m.GrossPF), finite(m.NetPF), finite(m.RR)})
}

func finite(v float64) *float64 {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return nil
	}
	return &v
}

func computeMetrics(trades []Trade) Metrics {
	m := Metrics{N: len(trades)}
	if len(trades) == 0 {
		return m
	}
	var grossWin, grossLoss, netWin, netLoss float64
	var wins, losses int
	var sumNet, sumGross float64
	consec, maxConsec := 0, 0
	var equity, peak, maxDD float64

	for _, t := range trades {
		sumNet += t.NetJPY
		sumGross += t.GrossJPY

		if t.GrossJPY > 0 {
			grossWin += t.GrossJPY
		} else {
			grossLoss += -t.GrossJPY
		}

		if t.NetJPY > 0 {
			netWin += t.NetJPY
			wins++
			consec = 0
		} else {
			netLoss += -t.NetJPY
			losses++
			consec++
			if consec > maxConsec {
				maxConsec = consec
			}
		}

		equity += t.NetJPY
		if equity > peak {
			peak = equity
		}
		if dd := peak - equity; dd > maxDD {
			maxDD = dd
		}
	}

	m.GrossPF = pf(grossWin, grossLoss)
	m.NetPF = pf(netWin, netLoss)
	m.GrossExpectancy = sumGross / float64(m.N)
	m.NetExpectancy = sumNet / float64(m.N)
	m.WinRate = float64(wins) / float64(m.N)
	if wins > 0 {
		m.AvgWinJPY = netWin / float64(wins)
	}
	if losses > 0 {
		m.AvgLossJPY = netLoss / float64(losses)
	}
	if m.AvgLossJPY > 0 {
		m.RR = m.AvgWinJPY / m.AvgLossJPY
	} else if m.AvgWinJPY > 0 {
		m.RR = math.Inf(1)
	}
	m.MaxConsecLoss = maxConsec
	m.MaxDrawdownJPY = maxDD
	return m
}

// pf: +Inf when there are wins but no losses, 0 when there are no wins.
func pf(win, loss float64) float64 {
	switch {
	case loss == 0 && win == 0:
		return 0
	case loss == 0:
		return math.Inf(1)
	default:
		return win / loss
	}
}

func equityCurve(trades []Trade) []EquityPoint {
	sorted := append([]Trade(nil), trades...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ClosedAt.Before(sorted[j].ClosedAt) })
	out := make([]EquityPoint, 0, len(sorted))
	cum := 0.0
	for _, t := range sorted {
		cum += t.NetJPY
		out = append(out, EquityPoint{Time: t.ClosedAt, Equity: cum})
	}
	return out
}
