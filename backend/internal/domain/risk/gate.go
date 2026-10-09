// Package risk contains the pure pre-trade risk gate. usecase
// assembles the inputs and calls EvaluateSignal / EvaluateHardSafety; the gate
// performs no I/O. Order matters: hard blow-up brakes (daily loss, emergency,
// session) are checked before soft, operator-overridable guards.
package risk

import (
	"fmt"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/strategy"
)

// Decision is the outcome of running the gate on a Signal. QtyMultiplier
// scales the entry quantity (0/≤0 → treat as 1.0); set to 0.5 by the
// consecutive-loss halving rule. Only meaningful when Allowed is true.
type Decision struct {
	Allowed       bool
	Reason        string
	QtyMultiplier float64
}

// AccountSnapshot is the live trading state passed to the gate. It mirrors an
// earlier project's snapshot plus the stock-specific fields: session, EOD,
// margin and the tick-alignment inputs.
type AccountSnapshot struct {
	EmergencyStop bool

	// Per-symbol aggregates.
	// 🛑 OpenPositions は **銘柄**単位(external を除く bot 建玉)。live の建玉枠はこちら。
	OpenPositions int
	// OpenPositionsSameStrategy は同上を **同一戦略に限った**もの。
	// paper の建玉枠はこちらを見る。
	//
	// 🚨 なぜ要るか: 建玉の一意性キーを (銘柄, 戦略) にしたのに、枠だけが銘柄キーの
	// ままだと、決定論テンプレートが凍結する `max_open_positions: 1` が**兄弟アームの
	// 2 本目を必ず落とす**。兄弟アームのペアが 0 本になる壊れ方そのもの。
	// 🛑 戦略が分からない bot 建玉(strategy_name を持たない旧建玉)は**全戦略に数える**(fail-close)。
	OpenPositionsSameStrategy int
	// EntriesTodaySameStrategy は **その営業日に** (銘柄, 戦略) で建てた bot 建玉の数(決済済みも含む)。
	// 日中保有の「同日 1 回転」を見る。日中の建てでだけ埋める。
	EntriesTodaySameStrategy int
	TradesInWindow           int
	LossInWindowJPY          int
	DailyLossJPY             int
	MaxDailyLossJPY          int
	ConsecutiveLosses        int
	MaxConsecutiveLosses     int

	// Account-wide aggregates (0 disables that gate).
	AccountOpenPositions    int
	AccountDailyLossJPY     int
	AccountMaxOpenPositions int
	AccountMaxDailyLossJPY  int

	// AccountEntriesToday は **その営業日に**口座全体で建てた bot 建玉の数(銘柄・戦略を問わない・
	// 決済済みも含む・external は数えない)。台帳から数えるので再起動しても同じ数になる。
	// AccountMaxEntriesPerDay はその上限。0 = 無効(research はサイクルの事前登録を動かさない)。
	//
	// 🚨 なぜ要るか: 暴落の初日に出た発火で枠を埋めると、その玉が
	// 続けて損切りされ、底の発火は見送りになる(2008-10 は 21 本中 16 本、2020-03 は 18 本中 15 本が SL)。
	// 上限で 2〜3 日目に資金を残す。日足の再現では両データ・全組み合わせで総額は同じか増えた。
	AccountEntriesToday     int
	AccountMaxEntriesPerDay int

	// --- 監視銘柄の予算 ---
	//
	// 🚨 なぜ「本数」ではなく「銘柄数」か: 時価は 1 リクエストに 120 銘柄まで積め、
	// `BatchQuoteFeed` は 121 銘柄目で**間隔をチャンク数だけ伸ばして通信量を一定に保つ**。
	// つまり監視銘柄が増えても **API 回数は 1 回も増えず**、代わりに時価の実効間隔が
	// 3秒 → 6秒 → 9秒 と落ちる(実測で 6秒へ落ちた)。監視集合は
	// **トラック間で共有**なので、paper の建玉が live の OnTick 決済判定と、バックテストの
	// 入力である分足の密度まで道連れにする。
	//
	// 建玉の**本数**は監視集合に効かない(同じ銘柄に 12 アーム乗っても 1 銘柄)。
	// 効くのは**銘柄数**だけなので、枠も銘柄数で持つ。

	// AccountOpenSymbols は口座全体の **保有銘柄数**(重複排除・external 含む)。
	AccountOpenSymbols int
	// AccountMaxOpenSymbols はその上限。0 = 無効(live / backtest はここを使わない)。
	AccountMaxOpenSymbols int
	// SymbolAlreadyHeld はこの銘柄に既に OPEN/CLOSING があるか。
	// 🛑 **true なら枠を消費しない** — 監視銘柄が増えないので API も解像度もコストゼロ。
	// この 1 行が「少ない銘柄に多くのアームを重ねて N を稼ぐ」方向のバイアスになる。
	SymbolAlreadyHeld bool

	// EntryArmOpenSymbols は **同じ入口**(兄弟アーム込み)が保有している銘柄数。
	// 全体の枠を先着順で配ると発火の多い入口が食い尽くすので、入口ごとに予約する
	// (実測: donchian + atr が 457 建玉中 322 本 = 70%、bnf は 3 本)。
	EntryArmOpenSymbols int
	// EntryArmMaxOpenSymbols はその上限。0 = 無効。
	EntryArmMaxOpenSymbols int
	// EntryArmHoldsSymbol はこの銘柄を**同じ入口**が既に保有しているか。
	// 🛑 兄弟アーム(`X` と `X_trail`)は入口が同一で必ず同じ銘柄に乗るので、これが
	// 無いと **ペアの 2 本目だけが枠の境界で弾かれ、ペア差が測れなくなる**。
	EntryArmHoldsSymbol bool

	// MaxRiskPerTradeJPY は **1本あたりの計画損失**の上限(建値から SL までの距離 ×
	// 株数)。0 = 無効(research / harvest は資本リスクがゼロなので、全トリガー採用の
	// 標本をこの理由で censoring しない — MaxGrossNotionalRatio と同じ扱い)。
	//
	// 🛑 **SL を狭めるものではない。** 出口の幾何は戦略が決めたまま動かさず、
	// 「その SL だと 1 本で上限を超える」候補を**建てない**だけ。狭める案を採らなかったのは、
	// 平均回帰で stop を建値に寄せると戻る前に振り落とされる本が増え、その当否を測る
	// MFE/MAE がまだ十分に無いため。
	//
	// 🚨 **損失の上限ではない。** ギャップとストップ安は逆指値をすり抜けるので、実損は
	// これを超えうる(6841 = 建値 4,727 / SL 4,402 で計画 32,500 だが、4,200 で寄れば
	// 52,700)。保証するのは「**建てる前に分かる計画損失**がここまで」だけ。
	//
	// 動機(paper の bnf 実測): 2.0×ATR の SL は銘柄ごとに 7.0〜17.0% と
	// ばらけ、1本の計画損失が 12,636〜61,529 円に散る。上端は
	// AccountMaxDailyLossJPY(60,000)を**1本で使い切る** = フルストップ 1 回で
	// その日の新規が止まる。上限はその尾だけを落とす。
	MaxRiskPerTradeJPY int

	InCooldown    bool
	CooldownUntil time.Time
	CooldownKind  string

	Now time.Time

	ConsecutiveLossGuardsDisabled bool

	// Same-side open counts INCLUDING external (証券アプリ) positions — the
	// nanpin block reads these. **live はこちらだけを見る。**
	OpenBuyInclExternal  int
	OpenSellInclExternal int

	// 同上を **同一戦略に限った**もの。paper のナンピン判定はこちらを見る。
	// 🛑 **external 建玉は戦略が分からないので、どの戦略に対しても数に入れる** —
	// 「同 symbol 同 side の OPEN(external 含む)があれば新規 reject」(CLAUDE.md)を
	// 緩めるのは **bot 同士で戦略が違うとき** の 1 点だけ。
	OpenBuySameStrategyInclExternal  int
	OpenSellSameStrategyInclExternal int

	// Stock-specific.
	OutsideSessionHours   bool
	EndOfDayCloseRequired bool // at/after entry cutoff (14:55)
	MarginRatio           float64
	CollateralRequiredJPY int
	AvailableToTradeJPY   int

	// レバレッジ上限(建玉合計 ≤ 保証金 × MaxGrossNotionalRatio)。
	//
	// 🛑 **委託保証金率(required_rate 0.33)は上限ではない** — あれは「レバ 3.0 倍まで
	// 建てられる」という意味で、事前にコミットした基準(実弾は保証金の
	// 1.0 倍以内から開始)を機械強制するものが今まで**何も無かった**。満額 909,090円 で
	// 建てると bot の 30% ブレーカーは建値比 −3.0% で発火する = 構造的に運用不能。
	//
	// **MaxGrossNotionalRatio が 0 のときは無効**。research(紙・200銘柄・全トリガー
	// 採用)はレバ規律の対象外で、ここを既定 1.0 にするとサンプルを censoring する。
	CollateralJPY         int
	OpenGrossNotionalJPY  int
	MaxGrossNotionalRatio float64
	// MinCollateralJPY は最低委託保証金(hard_limits `margin.min_collateral_jpy`)。
	// CollateralJPY がこれを割ったら新規 entry を断る。0 = 無効。
	MinCollateralJPY    int
	MarginStatusUnknown bool // broker could not report margin → fail-close entries

	// RepoStatusUnknown is set when the snapshot could not read the position /
	// trade repositories (e.g. a transient Postgres error). The daily-loss caps,
	// nanpin block and count caps are all fed by those reads, so a read failure
	// must FAIL CLOSE (reject the entry) — never silently evaluate the
	// never-overridable gates against zero (review: snapshot fail-open).
	RepoStatusUnknown bool

	// ManualSymbolBlocked はこの銘柄の新規が人間に止められているか(live の銘柄ごとの停止ボタン)。
	// SymbolBlocksUnreadable は停止のファイルが読めない・壊れているか(→ その track の新規を全部止める)。
	// 🛑 どちらも停止の store を配線した track(live)でだけ立つ。research は常に偽。
	// 止めるのは新規だけ — 決済・守り・引け前フラット化は entry ゲートを通らない。
	ManualSymbolBlocked    bool
	SymbolBlocksUnreadable bool
}

// 銘柄ごとの新規停止の reject 理由(signal_rejections に残り、止めた判断の答え合わせに使う)。
const (
	ReasonManualSymbolBlock      = "manual_symbol_block"
	ReasonSymbolBlocksUnreadable = "symbol_blocks_unreadable"
)

const (
	consecutiveLossHalveThreshold  = 2
	consecutiveLossHalveMultiplier = 0.5
)

// EvaluateSignal returns Allowed=true only if the proposed entry passes every
// guard. Non-entries are trivially allowed. The caller records rejections to
// signal_rejections.
//
// **2相構成**: 担保(broker 由来)を見るゲートだけを EvaluateCollateral に
// 分けてある。呼び出し側は EvaluateStructural を先に回し、通ったものだけ口座照会を
// 払ってから EvaluateCollateral を回せる — 建玉枠やナンピン禁止で捨てるエントリーに
// 立花への 3 リクエストを払わないため(実測 1,680回/時。TACHIBANA_API_NOTES.md)。
// **ここ(合成版)の振る舞いは分解前と同じでなければならない** = gate_test.go が回帰網。
func EvaluateSignal(sig strategy.Signal, cfg *config.StrategyConfig, snap AccountSnapshot, summary *market.MarketSummary) Decision {
	if !sig.IsEntry() {
		return Decision{Allowed: true}
	}
	if d := EvaluateStructural(sig, cfg, snap, summary); !d.Allowed {
		return d
	}
	if d := EvaluateCollateral(sig, snap); !d.Allowed {
		return d
	}
	mul := 1.0
	if !snap.ConsecutiveLossGuardsDisabled && snap.ConsecutiveLosses >= consecutiveLossHalveThreshold {
		mul = consecutiveLossHalveMultiplier
	}
	return Decision{Allowed: true, QtyMultiplier: mul}
}

// EvaluateStructural runs every guard that needs NO broker query — 口座の状態では
// なく「我々の帳簿と時計」だけで決まるもの(emergency / セッション / 日次損失 /
// 建玉枠 / ナンピン禁止 / 窓 / 方向 / スプレッド)。
//
// 🛑 **broker 由来のフィールドを読まないこと**(MarginStatusUnknown /
// AvailableToTradeJPY / CollateralJPY)。読んだ瞬間、口座照会前に呼べるという
// この関数の唯一の存在理由が消える。
func EvaluateStructural(sig strategy.Signal, cfg *config.StrategyConfig, snap AccountSnapshot, summary *market.MarketSummary) Decision {
	if !sig.IsEntry() {
		return Decision{Allowed: true}
	}
	if cfg == nil {
		return Decision{Reason: "no_active_config"}
	}
	for _, g := range structuralGates {
		if reason := g.check(sig, cfg, snap, summary); reason != "" {
			return Decision{Reason: reason}
		}
	}
	// QtyMultiplier(連敗時の半分張り)は合成側 EvaluateSignal が付ける。構造ゲート
	// 単体の戻り値でサイズを決めさせない — 担保チェックを飛ばした発注が生まれる。
	return Decision{Allowed: true}
}

// structuralGate returns the reject reason, "" to pass.
type structuralGate func(sig strategy.Signal, cfg *config.StrategyConfig, snap AccountSnapshot, summary *market.MarketSummary) string

// structuralGates is the ordered table EvaluateStructural walks. The order is a
// contract (TestStructuralGateOrderIsPinned): never-overridable brakes first so
// their reason wins, the quote-dependent spread guard last.
var structuralGates = []struct {
	name  string
	check structuralGate
}{
	{"hard_brakes", gateHardBrakes},
	{"exec_kind_coherence", gateExecKindCoherence},
	{"tick_alignment", gateTickAlignment},
	{"risk_per_trade", gateRiskPerTrade},
	{"cooldown", gateCooldown},
	{"consecutive_losses", gateConsecutiveLosses},
	{"position_count_caps", gatePositionCountCaps},
	{"account_entries_per_day", gateAccountEntriesPerDay},
	{"symbol_budget", gateSymbolBudget},
	{"nanpin_block", gateNanpinBlock},
	{"intraday_once_per_day", gateIntradayOncePerDay},
	{"window_throttles", gateWindowThrottles},
	{"direction", gateDirection},
	{"spread", gateSpread},
}

func structuralGateNames() []string {
	names := make([]string, 0, len(structuralGates))
	for _, g := range structuralGates {
		names = append(names, g.name)
	}
	return names
}

// gateHardBrakes: the blow-up brakes nothing may override.
func gateHardBrakes(_ strategy.Signal, _ *config.StrategyConfig, snap AccountSnapshot, _ *market.MarketSummary) string {
	switch {
	case snap.EmergencyStop:
		return "emergency_stop"
	case snap.RepoStatusUnknown:
		// Risk state (open positions, daily loss, streak, window counts) could not
		// be read — the caps below would evaluate against zero. Fail closed.
		return "risk_state_unavailable"
	case snap.OutsideSessionHours:
		return "outside_session_hours"
	case snap.EndOfDayCloseRequired:
		return "end_of_day_flat_required"
	case snap.MaxDailyLossJPY > 0 && snap.DailyLossJPY >= snap.MaxDailyLossJPY:
		return fmt.Sprintf("daily_loss %d >= cap %d", snap.DailyLossJPY, snap.MaxDailyLossJPY)
	case snap.AccountMaxDailyLossJPY > 0 && snap.AccountDailyLossJPY >= snap.AccountMaxDailyLossJPY:
		return fmt.Sprintf("account_daily_loss %d >= cap %d", snap.AccountDailyLossJPY, snap.AccountMaxDailyLossJPY)
	}
	return symbolBlockReason(snap)
}

// symbolBlockReason は人間の銘柄停止(override で越えられない)。読めないときは全部止める。
func symbolBlockReason(snap AccountSnapshot) string {
	switch {
	case snap.SymbolBlocksUnreadable:
		return ReasonSymbolBlocksUnreadable
	case snap.ManualSymbolBlocked:
		return ReasonManualSymbolBlock
	}
	return ""
}

// gateExecKindCoherence: intraday margin cannot hold overnight.
func gateExecKindCoherence(sig strategy.Signal, cfg *config.StrategyConfig, _ AccountSnapshot, _ *market.MarketSummary) string {
	if cfg.ExecKind == config.ExecMarginOneday && sig.HoldingMode == order.HoldingMultiday {
		return "oneday_margin_cannot_hold_multiday"
	}
	return ""
}

// gateTickAlignment: broker-side TP/SL must sit on the 呼値 grid.
func gateTickAlignment(sig strategy.Signal, _ *config.StrategyConfig, _ AccountSnapshot, _ *market.MarketSummary) string {
	if sig.EntryPrice <= 0 {
		return ""
	}
	tp, sl := position.TPSLPricesFromJPY(sig.Symbol, sig.Side, sig.EntryPrice, sig.TakeProfitJPY, sig.StopLossJPY)
	if tp > 0 && !market.IsTickAlignedOf(sig.Symbol, tp) {
		return "tp_not_tick_aligned"
	}
	if sl > 0 && !market.IsTickAlignedOf(sig.Symbol, sl) {
		return "sl_not_tick_aligned"
	}
	return ""
}

// gateRiskPerTrade: 1本あたりの計画損失の上限。
//
// 🛑 **構造ゲート側に置く**。判定材料は Signal だけ(建値から SL までの距離 × 株数)で
// broker を 1 度も触らないので、collateral 段に置くと「建てないと分かっている候補」の
// ために立花へ wire 3 リクエストを払うことになる(API 予算を焼く形)。
//
// 🛑 **SL の無いシグナルはここで拾わない**。落とすなら別の理由で落とす —
// 混ぜると signal_rejections で「守りが無い」と「1本が重すぎる」が区別できなくなる。
func gateRiskPerTrade(sig strategy.Signal, _ *config.StrategyConfig, snap AccountSnapshot, _ *market.MarketSummary) string {
	if snap.MaxRiskPerTradeJPY > 0 && sig.StopLossJPY > 0 && sig.Quantity > 0 {
		if risk := int(sig.StopLossJPY * float64(sig.Quantity)); risk > snap.MaxRiskPerTradeJPY {
			return fmt.Sprintf("risk_per_trade %d > cap %d", risk, snap.MaxRiskPerTradeJPY)
		}
	}
	return ""
}

func gateCooldown(_ strategy.Signal, _ *config.StrategyConfig, snap AccountSnapshot, _ *market.MarketSummary) string {
	if snap.InCooldown {
		return fmt.Sprintf("cooldown %s until %s", snap.CooldownKind, snap.CooldownUntil.Format("15:04:05"))
	}
	return ""
}

// gateConsecutiveLosses: operator-overridable; can be disabled.
func gateConsecutiveLosses(_ strategy.Signal, _ *config.StrategyConfig, snap AccountSnapshot, _ *market.MarketSummary) string {
	if snap.ConsecutiveLossGuardsDisabled {
		return ""
	}
	if snap.MaxConsecutiveLosses > 0 && snap.ConsecutiveLosses >= snap.MaxConsecutiveLosses {
		return fmt.Sprintf("consecutive_losses %d >= cap %d", snap.ConsecutiveLosses, snap.MaxConsecutiveLosses)
	}
	return ""
}

// gateAccountEntriesPerDay は口座全体の「1 営業日の新規本数」の上限(0 = 無効)。
// 先着順で数える(どの銘柄を優先するかは selector の順位と時価の到着順が決める)。
func gateAccountEntriesPerDay(_ strategy.Signal, _ *config.StrategyConfig, snap AccountSnapshot, _ *market.MarketSummary) string {
	if snap.AccountMaxEntriesPerDay > 0 && snap.AccountEntriesToday >= snap.AccountMaxEntriesPerDay {
		return fmt.Sprintf("account_entries_per_day (%d >= cap %d)", snap.AccountEntriesToday, snap.AccountMaxEntriesPerDay)
	}
	return ""
}

// gatePositionCountCaps honours MaxConcurrent so pyramids aren't blocked here.
//
// キーは **paper では (銘柄, 戦略)**、**live では従来どおり銘柄**。
// ナンピン禁止と同じ非対称で、緩めるのは「bot 同士で戦略が違うか」の 1 点だけ。
//
// 🛑 この枠は**側を見ない**のが要点: ナンピン禁止は (銘柄, 側, 戦略) キーなので、
// `direction: both` の戦略が同一銘柄で買いと売りを**同時に**持つのはあちらでは
// 止まらない。同一戦略の自己両建ては測定として無意味なので、ここで止める。
func gatePositionCountCaps(sig strategy.Signal, cfg *config.StrategyConfig, snap AccountSnapshot, _ *market.MarketSummary) string {
	effMaxOpen := cfg.Risk.MaxOpenPositions
	if sig.MaxConcurrent > effMaxOpen {
		effMaxOpen = sig.MaxConcurrent
	}
	openHere := snap.OpenPositions
	if cfg.Mode != config.ModeLive {
		openHere = snap.OpenPositionsSameStrategy
	}
	if effMaxOpen > 0 && openHere >= effMaxOpen {
		return fmt.Sprintf("open_positions %d >= cap %d", openHere, effMaxOpen)
	}
	if snap.AccountMaxOpenPositions > 0 && snap.AccountOpenPositions >= snap.AccountMaxOpenPositions {
		return fmt.Sprintf("account_open_positions %d >= cap %d", snap.AccountOpenPositions, snap.AccountMaxOpenPositions)
	}
	return ""
}

// gateSymbolBudget: 監視銘柄の予算。
//
// 🛑 **どちらも「新しい銘柄を開くとき」だけ効く**。既に監視している銘柄への建ては
// 監視集合を 1 つも増やさないので、通信量にも時価の解像度にも効かない = 止める理由が無い。
// 入口の枠を先に見るのは、全体の枠を先着順で配ると発火の多い入口が食い尽くして
// たまにしか発火しない入口が新しい銘柄を開けなくなるため(枠の予約)。
func gateSymbolBudget(_ strategy.Signal, _ *config.StrategyConfig, snap AccountSnapshot, _ *market.MarketSummary) string {
	if snap.EntryArmMaxOpenSymbols > 0 && !snap.EntryArmHoldsSymbol && snap.EntryArmOpenSymbols >= snap.EntryArmMaxOpenSymbols {
		return fmt.Sprintf("entry_arm_open_symbols %d >= cap %d", snap.EntryArmOpenSymbols, snap.EntryArmMaxOpenSymbols)
	}
	if snap.AccountMaxOpenSymbols > 0 && !snap.SymbolAlreadyHeld && snap.AccountOpenSymbols >= snap.AccountMaxOpenSymbols {
		return fmt.Sprintf("account_open_symbols %d >= cap %d", snap.AccountOpenSymbols, snap.AccountMaxOpenSymbols)
	}
	return ""
}

// gateNanpinBlock: same-symbol same-side, including external positions. Hard
// gate, independent of the count caps, NOT overridable.
//
// キーは **paper では (銘柄, 側, 戦略)**、**live では従来どおり (銘柄, 側)**。
// 緩めるのは「bot 同士で戦略が違うか」の 1 点だけで、
// 同一戦略の積み増しも external 建玉との重複も従来どおり hard block。
// live を分けるのは、同一銘柄のエクスポージャが最大で戦略数ぶん(メニューのアーム数ぶん)に
// なるため — 研究モードの設定を実弾に持ち込まない前例(全トリガー採用 /
// account_max_open_positions)に揃える。
func gateNanpinBlock(sig strategy.Signal, cfg *config.StrategyConfig, snap AccountSnapshot, _ *market.MarketSummary) string {
	maxSameSide := sig.MaxConcurrent
	if maxSameSide < 1 {
		maxSameSide = 1
	}
	openBuy, openSell := snap.OpenBuyInclExternal, snap.OpenSellInclExternal
	if cfg.Mode != config.ModeLive {
		openBuy, openSell = snap.OpenBuySameStrategyInclExternal, snap.OpenSellSameStrategyInclExternal
	}
	if sig.Side == order.SideBuy && openBuy >= maxSameSide {
		return fmt.Sprintf("pyramiding_blocked_same_side_buy (%d open incl external, cap %d)", openBuy, maxSameSide)
	}
	if sig.Side == order.SideSell && openSell >= maxSameSide {
		return fmt.Sprintf("pyramiding_blocked_same_side_sell (%d open incl external, cap %d)", openSell, maxSameSide)
	}
	return ""
}

// gateIntradayOncePerDay: 日中保有は (銘柄, 戦略) ごとに **同日 1 回転**。
// 利益で時間切れ決済した直後に同じ戦略で買い直す経路がある — 利益の後は cooldown が
// 効かないので回数で止める。兄弟 `_trail` は別戦略なのでペアは成立する。
func gateIntradayOncePerDay(sig strategy.Signal, _ *config.StrategyConfig, snap AccountSnapshot, _ *market.MarketSummary) string {
	if sig.HoldingMode == order.HoldingIntraday && snap.EntriesTodaySameStrategy >= 1 {
		return fmt.Sprintf("intraday_once_per_day (%d entries today)", snap.EntriesTodaySameStrategy)
	}
	return ""
}

func gateWindowThrottles(_ strategy.Signal, cfg *config.StrategyConfig, snap AccountSnapshot, _ *market.MarketSummary) string {
	if cfg.Risk.MaxTradesInThisWindow > 0 && snap.TradesInWindow >= cfg.Risk.MaxTradesInThisWindow {
		return fmt.Sprintf("trades_in_window %d >= cap %d", snap.TradesInWindow, cfg.Risk.MaxTradesInThisWindow)
	}
	if cfg.Risk.MaxLossInThisWindowJPY > 0 && snap.LossInWindowJPY >= cfg.Risk.MaxLossInThisWindowJPY {
		return fmt.Sprintf("loss_in_window %d >= cap %d", snap.LossInWindowJPY, cfg.Risk.MaxLossInThisWindowJPY)
	}
	return ""
}

func gateDirection(sig strategy.Signal, cfg *config.StrategyConfig, _ AccountSnapshot, _ *market.MarketSummary) string {
	switch cfg.Entry.Direction {
	case config.DirectionNone:
		return "direction_none"
	case config.DirectionBuyOnly:
		if sig.Side != order.SideBuy {
			return "direction_buy_only_blocks_short"
		}
	case config.DirectionSellOnly:
		if sig.Side != order.SideSell {
			return "direction_sell_only_blocks_long"
		}
	}
	return ""
}

// gateSpread: 薄商い guard — the only gate that reads the quote.
func gateSpread(_ strategy.Signal, cfg *config.StrategyConfig, _ AccountSnapshot, summary *market.MarketSummary) string {
	if summary != nil && cfg.Entry.MaxSpreadTicks > 0 && summary.CurrentRate.SpreadTicks > cfg.Entry.MaxSpreadTicks {
		return fmt.Sprintf("spread %.2f > cap %.2f", summary.CurrentRate.SpreadTicks, cfg.Entry.MaxSpreadTicks)
	}
	return ""
}

// ReasonCollateralBelowMinimum は保証金が最低委託保証金を割って新規 entry を断った理由。
//
// 🚨 `margin.min_collateral_jpy` は yaml と「値が 30 万以上か」のテストにしか
// 無いと、本番コードからの参照が 0 件 = 効いている安全装置に見えて何もしていない。
const ReasonCollateralBelowMinimum = "collateral_below_minimum"

// EvaluateCollateral runs ONLY the guards that need the broker's 口座照会:
// 余力(委託保証金)とレバレッジ上限。呼び出し側は EvaluateStructural を通った
// エントリーについてだけ、口座照会を払ってからこれを呼ぶ。
//
// 🛑 **未照会は fail-close**。BuildStructural は broker を触らないので
// MarginStatusUnknown=true のまま返す = この関数を通らない。照会し忘れた経路が
// 担保チェックを素通りするのではなく必ず reject されるのは、その不変条件による。
func EvaluateCollateral(sig strategy.Signal, snap AccountSnapshot) Decision {
	if !sig.IsEntry() {
		return Decision{Allowed: true}
	}

	// --- margin sufficiency ---
	// Unknown margin status is FAIL-CLOSE: a broker margin-endpoint outage must
	// reject entries, not silently disable the collateral gate (review ⑥a).
	if snap.MarginStatusUnknown {
		return Decision{Reason: "margin_status_unavailable"}
	}
	// Available funds are compared directly (no `avail > 0` guard): margin is
	// KNOWN here (unknown fails closed above), so avail 0/negative is the 不足金
	// signal and MUST reject — gating it on `avail > 0` silently self-disabled
	// the collateral check exactly when collateral was exhausted (review ⑥a-2).
	if snap.CollateralRequiredJPY > 0 && snap.CollateralRequiredJPY > snap.AvailableToTradeJPY {
		return Decision{Reason: fmt.Sprintf("insufficient_margin need %d > avail %d", snap.CollateralRequiredJPY, snap.AvailableToTradeJPY)}
	}

	// --- 最低委託保証金 ---
	// 保証金はレバレッジ上限と同じ CollateralJPY(broker の Equity)で測る。2 つのゲートが
	// 別の「保証金」を見ると、片方だけ通る状態の説明がつかなくなる。
	if snap.MinCollateralJPY > 0 && snap.CollateralJPY < snap.MinCollateralJPY {
		return Decision{Reason: fmt.Sprintf("%s have %d < min %d", ReasonCollateralBelowMinimum, snap.CollateralJPY, snap.MinCollateralJPY)}
	}

	// --- leverage cap (事前にコミットした基準の機械強制) ---
	// collateral gate(余力)の**後**に置く: 余力は broker が決める上限、こちらは
	// 我々が自分に課す上限で、後者の方が常に厳しい前提。
	if snap.MaxGrossNotionalRatio > 0 {
		newGross := int(sig.EntryPrice * float64(sig.Quantity))
		if !WithinGrossNotionalCap(snap.CollateralJPY, snap.OpenGrossNotionalJPY, newGross, snap.MaxGrossNotionalRatio) {
			return Decision{Reason: fmt.Sprintf("gross_notional_cap open %d + new %d > 保証金 %d × %.2f倍",
				snap.OpenGrossNotionalJPY, newGross, snap.CollateralJPY, snap.MaxGrossNotionalRatio)}
		}
	}
	return Decision{Allowed: true}
}

// EvaluateHardSafety re-checks ONLY the never-overridable gates, used by the
// manual-override path so an operator can never bypass daily_loss /
// emergency_stop / session / margin.
func EvaluateHardSafety(sig strategy.Signal, cfg *config.StrategyConfig, snap AccountSnapshot, summary *market.MarketSummary) Decision {
	if !sig.IsEntry() {
		return Decision{Allowed: true}
	}
	if cfg == nil {
		return Decision{Reason: "no_active_config"}
	}
	if snap.EmergencyStop {
		return Decision{Reason: "emergency_stop"}
	}
	if snap.RepoStatusUnknown {
		return Decision{Reason: "risk_state_unavailable"}
	}
	if snap.OutsideSessionHours {
		return Decision{Reason: "outside_session_hours"}
	}
	if snap.EndOfDayCloseRequired {
		return Decision{Reason: "end_of_day_flat_required"}
	}
	if snap.MaxDailyLossJPY > 0 && snap.DailyLossJPY >= snap.MaxDailyLossJPY {
		return Decision{Reason: fmt.Sprintf("daily_loss %d >= cap %d", snap.DailyLossJPY, snap.MaxDailyLossJPY)}
	}
	if snap.AccountMaxDailyLossJPY > 0 && snap.AccountDailyLossJPY >= snap.AccountMaxDailyLossJPY {
		return Decision{Reason: fmt.Sprintf("account_daily_loss %d >= cap %d", snap.AccountDailyLossJPY, snap.AccountMaxDailyLossJPY)}
	}
	if reason := symbolBlockReason(snap); reason != "" {
		return Decision{Reason: reason}
	}
	if snap.MarginStatusUnknown {
		return Decision{Reason: "margin_status_unavailable"}
	}
	if snap.CollateralRequiredJPY > 0 && snap.CollateralRequiredJPY > snap.AvailableToTradeJPY {
		return Decision{Reason: fmt.Sprintf("insufficient_margin need %d > avail %d", snap.CollateralRequiredJPY, snap.AvailableToTradeJPY)}
	}
	return Decision{Allowed: true}
}
