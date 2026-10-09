package command

import (
	"context"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
)

// costFloorStrategy は `applyCostFloor` が返すのと同じ形の見送りシグナルを出す。
// 実物(スプレッドを広げて 12 スクリーナーのどれかを床に落とす)を usecase 層で
// 組み立てるより、**契約(Decision=NoTrade + Reason)だけ**を再現するほうが
// この配線の回帰を正確に射抜ける。
type costFloorStrategy struct{ reason string }

func (s costFloorStrategy) Name() config.StrategyName { return config.StrategyTimeSeriesMomentum }
func (s costFloorStrategy) Evaluate(in strategy.EvalInput) strategy.Signal {
	// 🛑 実物の `noTradeSignal` と**同じ形**を返す(Symbol を含む)。ここを省くと
	// 「銘柄が空でも通るテスト」になり、アーム別に数えられない行を見逃す。
	// 形そのものは domain 側の TestCostFloorNoTradeCarriesTheSymbol が縛る。
	return strategy.Signal{
		Decision: strategy.DecisionNoTrade, Reason: s.reason,
		Symbol:       in.Config.Symbol,
		StrategyName: config.StrategyTimeSeriesMomentum,
		ConfigID:     in.Config.ConfigID, CreatedAt: in.Now,
	}
}

func newCostFloorHarness(t *testing.T, now time.Time, reason string) (*harness, *repository.InMemoryRejectionRepo) {
	t.Helper()
	h := newHarness(t, now)
	h.cycle.engine = strategy.NewEngine(func() string { return "sig-1" }, costFloorStrategy{reason: reason})
	rej := repository.NewInMemoryRejectionRepo()
	h.cycle.Rejections = rej
	return h, rej
}

// 🚨 **事前登録した検出器が、実は何も検出していなかった**。
//
// 仕様:
//
//	「壊れたペアの件数を必ず出す。signal_rejections の tp_below_cost_floor を
//	 アーム別に数え、『ペア成立 N / 片側のみ N』を記録する。
//	 **黙って落とさない。**」
//
// ところが `applyCostFloor` はこれを **entry ではなく見送り**として返し、
// `TradingCycle.Execute` は `!sig.IsEntry()` で**記録の手前で return** していた。
// `recordRejection` の唯一の呼出経路は entry 提案の後ろにしか無い。
//
// これは締めのときに再構成できない —— 起きた瞬間に書かなければ消える種類のデータで、
// しかも **trail 側だけが選択的に落ちる**(床の対象が 1.0×ATR = v2 の 3 倍厳しい)。
// 「ペア差」を本命の統計量に据えた設計そのものが、この 1 本の穴で測れなくなる。
func TestCostFloorRefusalIsRecordedAsARejection(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h, rej := newCostFloorHarness(t, now, strategy.ReasonTPBelowCostFloor)
	in := evalInput(now, multidayConfig(), dailyUptrend(250))
	h.broker.SetPrice("7203", in.Summary.CurrentRate.Last)

	res, err := h.cycle.Execute(ctx, in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Entered {
		t.Fatal("床に落ちたのに建った")
	}
	if res.RejectReason != strategy.ReasonTPBelowCostFloor {
		t.Errorf("RejectReason = %q, want %q — 画面の reject 件数からも漏れる",
			res.RejectReason, strategy.ReasonTPBelowCostFloor)
	}

	rows := rej.All()
	if len(rows) != 1 {
		t.Fatalf("signal_rejections の行数 = %d, want 1 — 検出器が盲目のまま", len(rows))
	}
	if rows[0].Reason != strategy.ReasonTPBelowCostFloor || rows[0].Symbol != "7203" || rows[0].ConfigID == "" {
		t.Fatalf("行の中身が足りない(アーム別に数えられない): %+v", rows[0])
	}
}

// 🛑 **「セットアップが無い」は記録しない**。200銘柄 × 13アーム × 3秒で回るので、
// 全ての見送り理由を書いたら台帳が使い物にならなくなる(233,193 行事故の再来)。
// 記録するのは「入口は成立したのに我々が断った」ものだけ。
func TestOrdinaryNoTradeReasonsAreNotRecorded(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	for _, reason := range []string{"no_setup", "insufficient_daily_history", "below_threshold"} {
		h, rej := newCostFloorHarness(t, now, reason)
		in := evalInput(now, multidayConfig(), dailyUptrend(250))
		h.broker.SetPrice("7203", in.Summary.CurrentRate.Last)
		res, err := h.cycle.Execute(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		if res.RejectReason != "" {
			t.Errorf("%s: RejectReason = %q, want 空(entry の提案自体が無い)", reason, res.RejectReason)
		}
		if rows := rej.All(); len(rows) != 0 {
			t.Errorf("%s: %d 行書かれた — 見送り理由を全部書くと台帳が潰れる", reason, len(rows))
		}
	}
}

// 記録対象の集合が**ドメイン側の定数**と結びついていること。文字列を 2 箇所に
// 書くと、片方を変えた瞬間に静かに記録が止まる(この穴が生まれた経緯そのもの)。
func TestRecordableNoTradeReasonsUseDomainConstants(t *testing.T) {
	if !recordableNoTradeReasons[strategy.ReasonTPBelowCostFloor] {
		t.Fatal("tp_below_cost_floor が記録対象から外れている")
	}
	if strategy.ReasonTPBelowCostFloor != "tp_below_cost_floor" {
		t.Fatalf("定数の値が変わった: %q — 既存の signal_rejections と GROUP BY が繋がらなくなる",
			strategy.ReasonTPBelowCostFloor)
	}
}

// 🛑 **現物では売れない**。exec_kind は holding_mode からしか決まらないので、
// `multiday.exec_kind` を cash に戻すと空売りが「現物売り」として通り、carry(貸株料)が
// **0 で記帳**される(position/carry.go は ExecCash に 0 を返す)。設計は売りのコストを
// 貸株料込みで測ると事前登録しているので、これが起きると売り標本の net が静かに甘く出る。
func TestCashExecKindCannotOpenAShort(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	// 出荷 config と違って multiday を **現物**に落とした構成(= 設定ミス)。
	h.cycle.execKindFor = func(order.HoldingMode) order.ExecKind { return order.ExecCash }
	rej := repository.NewInMemoryRejectionRepo()
	h.cycle.Rejections = rej
	h.cycle.engine = strategy.NewEngine(func() string { return "sig-1" }, sellStrategy{})

	in := evalInput(now, multidayConfig(), dailyUptrend(250))
	h.broker.SetPrice("7203", in.Summary.CurrentRate.Last)
	res, err := h.cycle.Execute(ctx, in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Entered {
		t.Fatal("現物で空売りが通った — 貸株料 0 の売り標本が台帳に入る")
	}
	if res.RejectReason != "cash_cannot_short" {
		t.Fatalf("RejectReason = %q, want cash_cannot_short", res.RejectReason)
	}
	if rows := rej.All(); len(rows) != 1 || rows[0].Reason != "cash_cannot_short" {
		t.Fatalf("理由が台帳に残らない(設定ミスが無音になる): %+v", rows)
	}
}

// 信用(制度)なら売れる — 門を足したせいで売りが全部落ちる、を防ぐ。
func TestMarginSystemCanOpenAShort(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	h.cycle.execKindFor = func(order.HoldingMode) order.ExecKind { return order.ExecMarginSystem }
	h.cycle.engine = strategy.NewEngine(func() string { return "sig-1" }, sellStrategy{})

	in := evalInput(now, multidayConfig(), dailyUptrend(250))
	h.broker.SetPrice("7203", in.Summary.CurrentRate.Last)
	res, err := h.cycle.Execute(ctx, in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.RejectReason == "cash_cannot_short" {
		t.Fatal("制度信用の売りまで落とした(売り標本がゼロになる)")
	}
}

type sellStrategy struct{}

func (sellStrategy) Name() config.StrategyName { return config.StrategyTimeSeriesMomentum }
func (sellStrategy) Evaluate(in strategy.EvalInput) strategy.Signal {
	return strategy.Signal{
		Decision: strategy.DecisionEnter, Symbol: in.Config.Symbol, Side: order.SideSell,
		Quantity: 100, EntryPrice: in.Summary.CurrentRate.Last,
		TakeProfitJPY: 100, StopLossJPY: 50, MaxHoldMinutes: 240,
		HoldingMode:  in.Config.HoldingMode,
		StrategyName: config.StrategyTimeSeriesMomentum,
		ConfigID:     in.Config.ConfigID, CreatedAt: in.Now,
	}
}

// 🚨 **理由が交互に出ると dedup をすり抜けて毎ティック書かれる**
// (前のコミットが自分で作った回帰)。
//
// `recordRejection` は (銘柄, 戦略) につき **エッジを 1 つ**しか覚えない。
// 同じ種別の連続は潰せるが、**2 種類が交互に来ると毎回「変化した」と判定**される。
//
// そして交互は仮定ではなく構造的に起きる:
//   - コスト床の `SpreadTicks` は**その瞬間の気配**(`CurrentRate.SpreadTicks`)
//   - 比較先(TP / ratchet arm)は **ATR 由来で日中不変**
//     → スプレッドが 2↔3 ティック揺れるだけで Enter と tp_below_cost_floor が交互になる。
//
// 建玉済みなら Enter 側は gate が `open_positions` で落とすので、台帳には
//
//	open_positions → tp_below_cost_floor → open_positions → …
//
// が 3 秒ごとに入る。**この経路を足す前は 1 日 1 行だった。**
//
// 対策: 見送り理由の記録は**別のエッジ**で覚える。エントリー拒否の系列と干渉しない。
func TestAlternatingReasonsDoNotDefeatTheRejectionDedup(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 17, 10, 0, 0, 0, clock.JST)
	h := newHarness(t, now)
	rej := repository.NewInMemoryRejectionRepo()
	h.cycle.Rejections = rej

	floor := strategy.NewEngine(func() string { return "sig-1" },
		costFloorStrategy{reason: strategy.ReasonTPBelowCostFloor})
	// emergency を trip させて、entry 提案側が必ず同じ理由で落ちるようにする
	// (実運用では open_positions。ここでは安定した「entry 拒否の理由」なら何でもよい)。
	_ = h.emergency.Trip("test", now)
	enter := strategy.NewEngine(func() string { return "sig-1" }, strategy.TimeSeriesMomentum{})

	in := evalInput(now, multidayConfig(), dailyUptrend(250))
	h.broker.SetPrice("7203", in.Summary.CurrentRate.Last)

	// 交互に 6 ティック回す(= スプレッドが 2↔3 で揺れている 18 秒ぶん)。
	for i := 0; i < 6; i++ {
		if i%2 == 0 {
			h.cycle.engine = enter
		} else {
			h.cycle.engine = floor
		}
		if _, err := h.cycle.Execute(ctx, in); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}

	rows := rej.All()
	// 期待: 種別ごとに 1 行ずつ(= 2 行)。毎ティック書かれるなら 6 行になる。
	if len(rows) > 2 {
		t.Fatalf("%d 行書かれた(want ≤2)— 理由が交互に出るだけで dedup が無効化され、"+
			"3秒ごとに 1 行 = 233,193 行事故の再来", len(rows))
	}
	kinds := map[string]int{}
	for _, r := range rows {
		kinds[r.Reason]++
	}
	if kinds[strategy.ReasonTPBelowCostFloor] != 1 {
		t.Errorf("tp_below_cost_floor = %d 行, want 1(アーム別に数える単位は「その日に起きたか」)",
			kinds[strategy.ReasonTPBelowCostFloor])
	}
	if kinds["emergency_stop"] != 1 {
		t.Errorf("entry 拒否側 = %d 行, want 1(見送りの記録が既存の系列を壊していない)",
			kinds["emergency_stop"])
	}
}
