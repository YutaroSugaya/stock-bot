package strategy

import "stockbot/backend/internal/config"

// Spec は戦略カタログの 1 行。
//
// 🛑 **戦略の登録はここ 1 か所**。engine の登録(MenuStrategies)・screener(DefaultScreeners)・
// advisor の候補(MenuNames)・live の selector(LiveArmScreeners)・backtest の名前解決(ByName)・
// MaxHold の窓・向きは全部この表から派生する。以前は 7 か所に写していて、v2 の 3 本が
// engine に無いまま枠を消費した/ backtest が day2 と stabilized を引けなかった。
// 戦略を 1 本足すときに触るのは、この表と catalog_test.go だけ。
//
// live の allowlist(`hard_limits.live_allowed_strategies` と `catastrophe_guards_test`)は
// 「実弾で起動してよいか」の別の栓で、ここからは派生させない。
type Spec struct {
	Strategy Strategy
	// Menu はコードのメニュー(engine の登録・screener・advisor の候補)に載せるか。
	// false = cmd/backtest の再現用だけ(棄却済み・v1・ban 維持)。paper で実際に回る入口は
	// さらに `advisor_v2.entries` が絞る(ScreenersForEntries)。
	Menu bool
	// LiveArmable は live の selector が arm してよいか。条件は**出口を自前計算すること**
	// (config の exit 欄が 0 のまま hard_limits を通るので、テンプレート無しで arm しても
	// TP/SL の無い建玉にならない)。GKM / PEAD / 52週高値 は config の exit を読むので立てない —
	// 立てるなら戦略ごとの出口テンプレートを同時に commit すること。広げるのはリスクを増やす変更。
	//
	// 🚨 live で trail を回すと利確が板に乗らない(TP 脚が無いので PlaceSettleOCO は stop-only)。
	// SL は板に常駐するが、ratchet 利確は bot の OnTick だけが持つ。
	LiveArmable bool
	// Direction は入口の**事前登録された向き**。空 = buy_only。
	// 入口の行にだけ書く — `_trail` 兄弟は基の値を引く(EntryDirection)。
	Direction config.Direction
	// LookbackDays は入口シグナルのルックバック窓(営業日)。MaxHold = 窓 × 0.5(MaxHoldBusinessDays)。
	// 0 = この表から期限を与えない(BNF 家族は自前の MaxHold を持つ)。入口の行にだけ書く。
	LookbackDays int
}

// catalog の順序はメニューの並び(画面と snapshot の並びを安定させる。ランキングの
// tiebreak は名前なので順序は結果に効かない)。
var catalog = []Spec{
	{Strategy: BNFReversion{}, Menu: true, LiveArmable: true},
	// bnf の兄弟。多日建玉の板は bnf も stop-only なので、
	// trail にしても板の守りの形は変わらない。
	{Strategy: BNFReversionTrail{}, Menu: true, LiveArmable: true},
	{Strategy: HighVolumePremium{}, Menu: true, LookbackDays: hvpLookback},                              // 50日出来高最大
	{Strategy: PostJumpDrift{}, Menu: true, LookbackDays: peadVolWindow},                                // 60日ボラ窓
	{Strategy: High52wMomentum{}, Menu: true, Direction: config.DirectionBoth, LookbackDays: h52Window}, // 52週
	// v2 が v1 を置き換える(v1 は Menu=false でコードは残す)。
	// v1+v2 を並走させない理由: v2 の成立集合は v1 の真部分集合で、1銘柄1 config + ナンピン禁止の下では
	// 同じ場面を取ることが構造的に不可能。同じ母集団を score 軸で割り合うだけになり A/B にならない。
	{Strategy: AbsMomentumV2{}, Menu: true, LiveArmable: true, Direction: config.DirectionBoth, LookbackDays: absLookback},   // 約6ヶ月
	{Strategy: ATRBreakoutV2{}, Menu: true, LiveArmable: true, Direction: config.DirectionBoth, LookbackDays: atrN},          // ATR14 の収縮→拡大
	{Strategy: DonchianBreakoutV2{}, Menu: true, LiveArmable: true, Direction: config.DirectionBoth, LookbackDays: dbWindow}, // 20日高値
	// **入口が完全に同一で出口だけが違う兄弟アーム**(v3 ではない)。
	// 置き換えると「固定 TP と トレール のどちらが良いか」という問い自体が測れない。
	// 🛑 標本は薄まらない — (銘柄, 戦略) キーで両アームが同じトリガーを取れるので、
	// 各アームがフルの標本を得る。
	// 🛑 `bot_config` の `top_n` は per_strategy_n × アーム数を賄える値でなければならない
	// (足りないとペアの片方が次ラウンドに回り、建値がずれて「同一建値」の前提が壊れる)。
	{Strategy: AbsMomentumV2Trail{}, Menu: true},
	{Strategy: ATRBreakoutV2Trail{}, Menu: true},
	// donchian の兄弟だけ live へ出せる。
	{Strategy: DonchianBreakoutV2Trail{}, Menu: true, LiveArmable: true},
	{Strategy: High52wMomentumTrail{}, Menu: true},
	// bnf ファミリーの新入口 2 つ + 兄弟。
	{Strategy: BNFDay2Reversion{}, Menu: true},
	// live へ出せる(物差しは資金×日あたりの損益・エッジ判定ではない)。
	// 場中に建つ(前日の確定足の前兆 + 当日の現在値が −12% を割った tick)。
	{Strategy: BNFDay2ReversionTrail{}, Menu: true, LiveArmable: true},
	{Strategy: BNFStabilizedReversion{}, Menu: true},
	{Strategy: BNFStabilizedReversionTrail{}, Menu: true},
	// 日中版とその兄弟(forward paper だけで測る・バックテストは使わない)。
	{Strategy: BNFIntradayReversion{}, Menu: true, LiveArmable: true},
	{Strategy: BNFIntradayReversionTrail{}, Menu: true},

	// ---- ここから下は Menu=false: cmd/backtest の再現用だけ ----
	// 台帳準拠 — reject 済み #7 rsi2 / #8 gap / #9 bollinger、棄却済み #1 TSM と euphoria-short、
	// 灰色だが ban 維持の #2 ma_cross、v2 に置き換えた v1 の 3 本。緩和は人間の明示的な commit のみ。
	{Strategy: NoTrade{}},
	{Strategy: TimeSeriesMomentum{}},
	{Strategy: MACross{}},
	{Strategy: AbsMomentum{}},
	{Strategy: DonchianBreakout{}},
	{Strategy: ATRBreakout{}},
	{Strategy: RSI2Reversion{}},
	{Strategy: BollingerReversion{}},
	{Strategy: GapReversion{}},
	{Strategy: BNFEuphoriaShort{}},
}

// Catalog はカタログの写し(呼び手が書き換えても表は変わらない)。
func Catalog() []Spec {
	return append([]Spec(nil), catalog...)
}

func lookup(n config.StrategyName) (Spec, bool) {
	for _, s := range catalog {
		if s.Strategy.Name() == n {
			return s, true
		}
	}
	return Spec{}, false
}

// ByName は名前からカタログの実体を引く(メニュー外の再現用も含む)。未知 = nil。
func ByName(n config.StrategyName) Strategy {
	s, ok := lookup(n)
	if !ok {
		return nil
	}
	return s.Strategy
}

// MenuStrategies はメニューの実体(engine に登録する集合)。
func MenuStrategies() []Strategy {
	var out []Strategy
	for _, s := range catalog {
		if s.Menu {
			out = append(out, s.Strategy)
		}
	}
	return out
}

// MenuNames はメニューの名前(advisor が提案してよい集合・Promoter の menu ゲート)。
// hard_limits.live_allowed_strategies(live の栓)とは別物で、ここを広げても live の経路は開かない。
func MenuNames() []config.StrategyName {
	var out []config.StrategyName
	for _, s := range MenuStrategies() {
		out = append(out, s.Name())
	}
	return out
}

// DefaultScreeners はコードのメニューの screener。menu 外(reject / 棄却 / ban 維持)は screen しない。
// 🛑 paper で実際に回る入口は `advisor_v2.entries` が絞る(ScreenersForEntries)。
// トレンド系はここから**消さない**(停止であって棄却ではない)。
func DefaultScreeners() []Screener {
	var out []Screener
	for _, s := range MenuStrategies() {
		if sc, ok := s.(Screener); ok {
			out = append(out, sc)
		}
	}
	return out
}

// LiveArmScreeners は live の selector がその戦略でランキングするための screener。
// arm してよくない戦略は nil(= arm しない)。arm 可能な集合と screener を**同じ 1 行**から
// 出す — 2 つの綴りに割ると、screener はあるのに arm されない戦略が静かに生まれる。
func LiveArmScreeners(n config.StrategyName) []Screener {
	s, ok := lookup(n)
	if !ok || !s.LiveArmable {
		return nil
	}
	sc, ok := s.Strategy.(Screener)
	if !ok {
		return nil
	}
	return []Screener{sc}
}

// LiveArmable は live の selector がその戦略を arm してよいか(LiveArmScreeners と同じ 1 行から出す)。
func LiveArmable(n config.StrategyName) bool {
	return len(LiveArmScreeners(n)) > 0
}

// EntryDirection は戦略の事前登録された向き。`_trail` 兄弟は基の値を引く(片方だけ
// `both` にすると入口の成立集合がずれ、ペア設計が壊れる)。表に無い = buy_only。
func EntryDirection(n config.StrategyName) config.Direction {
	if s, ok := lookup(EntryArmOf(n)); ok && s.Direction != "" {
		return s.Direction
	}
	return config.DirectionBuyOnly
}
