package backtest

import (
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
)

func tbars(start time.Time, ohlc ...[4]float64) []market.Candle {
	out := make([]market.Candle, 0, len(ohlc))
	for i, c := range ohlc {
		out = append(out, market.Candle{
			OpenTime: start.AddDate(0, 0, i+1),
			Open:     c[0], High: c[1], Low: c[2], Close: c[3],
		})
	}
	return out
}

// 🚨 「床あり再計算」。固定 TP/SL で歩いた結果はトレール建玉にとって
// **別戦略の推定**なので、08-13 の格子と実アームの不一致を「推定誤差」と誤読する。
func TestTrailCounterfactual(t *testing.T) {
	closed := time.Date(2026, 8, 21, 15, 0, 0, 0, clock.JST)
	base := TrailCFInput{
		Side: order.SideBuy, ArmJPY: 100, GivebackJPY: 150, StopLossJPY: 800,
		FloorAtArm: true, EntryPrice: 1000, After: closed,
	}

	// arm(+100)に届いてから床(= max(100, peak-150))に触れて利確。
	t.Run("床で利確する", func(t *testing.T) {
		in := base
		in.Bars = tbars(closed,
			[4]float64{1000, 1120, 1000, 1110}, // peak +120 → armed(このバーでは出さない)
			[4]float64{1110, 1115, 1090, 1095}, // 床 = max(100, 120-150)=100 → 安値 +90 で触れる
		)
		got := TrailCounterfactual(in)
		if got.Outcome != TrailCFFloor {
			t.Fatalf("outcome = %s, want ratchet_floor", got.Outcome)
		}
		// 🛑 床は **arm(+100)** で止まる(旧規則なら peak-giveback = −30 で建値割れ)。
		if got.ExitUnrealJPY != 100 {
			t.Fatalf("exit = %v, want 100(床 = arm。建値割れで出ない)", got.ExitUnrealJPY)
		}
	})

	// 🛑 armed になったバーでは床で出さない(日中の順序が決められないので悲観側)。
	t.Run("armed になったバーでは床で出さない", func(t *testing.T) {
		in := base
		in.Bars = tbars(closed, [4]float64{1000, 1120, 1000, 1000}) // 同じバーで armed かつ +0 まで戻る
		got := TrailCounterfactual(in)
		if got.Outcome == TrailCFFloor {
			t.Fatal("armed と同じバーで床決済した — 最も有利な日中順序を勝手に採っている")
		}
		if !got.Armed {
			t.Fatal("armed に到達していない")
		}
	})

	// 床の版スタンプが false の建玉(harvest)は**旧規則**で歩く。
	t.Run("FloorAtArm=false は旧規則(建値割れで出る)", func(t *testing.T) {
		in := base
		in.FloorAtArm = false
		in.Bars = tbars(closed,
			[4]float64{1000, 1120, 1000, 1110},
			[4]float64{1110, 1115, 950, 960}, // 床 = 120-150 = −30 → 安値 −50 で触れる
		)
		got := TrailCounterfactual(in)
		if got.Outcome != TrailCFFloor || got.ExitUnrealJPY != -30 {
			t.Fatalf("got %+v, want 床 −30(旧規則は建値割れで出る)", got)
		}
	})

	// SL は床より先に見る(守りは broker 側)。
	t.Run("SL が先", func(t *testing.T) {
		in := base
		in.Bars = tbars(closed, [4]float64{1000, 1010, 790, 800})
		if got := TrailCounterfactual(in); got.Outcome != TrailCFStopLoss {
			t.Fatalf("outcome = %s, want stop_loss", got.Outcome)
		}
	})

	// 決済時点で既に peak があるなら、そこから続きを歩く(ゼロから数え直さない)。
	// peak 400 → 床 = max(arm 100, 400−150) = **250**。ゼロから数え直していたら
	// 床は arm(100)に落ち、この安値(+200)では出ない。
	t.Run("決済時点の peak を引き継ぐ", func(t *testing.T) {
		in := base
		in.PeakAtClose = 400
		in.Armed = true
		in.Bars = tbars(closed, [4]float64{1300, 1310, 1200, 1210}) // 安値 +200 <= 床 250
		got := TrailCounterfactual(in)
		if got.Outcome != TrailCFFloor || got.ExitUnrealJPY != 250 {
			t.Fatalf("got %+v, want 床 250(= max(arm100, peak400−giveback150))— "+
				"peak をゼロから数え直すと床が arm まで落ちてこの安値では出ない", got)
		}
	})

	// 🛑 床は **arm を下回らない**(FloorAtArm=true)。peak−giveback が arm より
	// 小さいときは arm で止まる —— これが床の規則「armed 後は建値割れで出ない」。
	t.Run("床は arm を下回らない", func(t *testing.T) {
		in := base
		in.PeakAtClose = 200 // peak−giveback = 50 < arm 100
		in.Armed = true
		in.Bars = tbars(closed, [4]float64{1100, 1105, 1040, 1045}) // 安値 +40
		got := TrailCounterfactual(in)
		if got.Outcome != TrailCFFloor || got.ExitUnrealJPY != 100 {
			t.Fatalf("got %+v, want 床 100(= arm。peak−giveback=50 では止めない)", got)
		}
	})

	t.Run("材料が無ければ no_data", func(t *testing.T) {
		if got := TrailCounterfactual(base); got.Outcome != TrailCFNoData {
			t.Fatalf("outcome = %s, want no_data", got.Outcome)
		}
	})
}
