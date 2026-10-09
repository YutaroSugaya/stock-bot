package app

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/risk"
	"stockbot/backend/internal/domain/strategy"
	"stockbot/backend/internal/port"
)

// selectorCandleLookback は**スクリーニング/ランキングに供給する日足の本数**。
//
// 🚨 事前登録は「250 を 2 箇所に散らしたことが原因だから、**数字を上げるだけでなく
// 1 箇所に寄せろ**」と書いた。`app/bundle.go`(Evaluate 側の供給)は
// `strategy.DailyBarsRequired` へ寄せたが、**こちら(Screen 側の供給)は裸の 300 が
// 残っていた** — しかもコメントは「最長のゲート(100日移動平均)」と、実際の要求
// (`high_52w_momentum.Screen` = 253 本)より小さい値を根拠に書いてあった。
// 52週高値が 4 週間 建玉ゼロだったのと同じ失敗の形。
//
// 余裕を足すのは、Screen が窓の**手前**の値も参照する戦略のため。
// 定数に紐づけたので、要求本数の大きい戦略を足せば自動で追随する。
const selectorLookbackHeadroom = 47

const selectorCandleLookback = strategy.DailyBarsRequired + selectorLookbackHeadroom

// 「広く見て狭く arm する」ユニバース選択層。発注はしない(arm された銘柄の決定論
// price loop が risk gate + broker OCO を通して入る)。建玉中/決済中の銘柄は
// 決して disarm しない(config 凍結)。
type Selector struct {
	candles    port.CandleRepository
	posRepo    port.PositionRepository
	holders    map[string]*ActiveConfigHolder // symbol -> its bundle's live config holder
	tiers      []SelectorTier                 // **優先順位つき**の arm 段(先頭が最優先)
	accountMax int                            // 口座全体の建玉上限
	// maxRiskPerTradeJPY は arm 時の**計画損失**の上限(計画SL幅×qty)。0 = 無効。
	// 発注前ゲート(risk の `risk_per_trade`)の**先出し**で、判定の基準は同一。
	// 詳細は WithMaxRiskPerTradeJPY。**ティア共通**(口座に効く額なので戦略ごとに割らない)。
	maxRiskPerTradeJPY int
	// margin / leverageRatio は **口座の資金枠**を arm の前提条件にするための入力。
	// 0 / nil = フィルタ無効(research はレバ上限を持たない)。詳細は WithLeverageHeadroom。
	margin        AccountMarginReader
	leverageRatio float64
	noTradeCfg    func(symbol string) *config.StrategyConfig
	persist       func(*config.StrategyConfig) error // nil-safe (Postgres config FK)
	logger        *slog.Logger

	// 直近 Tick 時点で「1単元が maxNotionalJPY 以内」だった銘柄数。-1 = 未計測。
	// 画面に出す内数なので、ランキングと同じ Tick の中で同じ日足から数える。
	affordableN atomic.Int64
	// 同じ Tick で「上限を超えていた」候補の集合(map[UnaffordableKey]bool・読み取り専用)。
	// 未計測なら nil。**発火しても arm されない銘柄**を画面が名指しするための材料。
	// 🛑 キーは **(銘柄, 戦略)**。ティアごとに建玉金額上限が違う(bnf 550,000 /
	// donchian 300,000)ので、銘柄だけのキーだと「donchian の上限で落ちた」ことが
	// bnf の行にも「上限超」の印を付けてしまう。
	unaffordable atomic.Value

	// 直近 Tick の資金枠の状態と余力(円)。画面が「なぜ 1 本も arm されていないのか」を
	// 名指しで読むための出口。levState は LeverageState、levHeadroom は cap − 建玉合計
	// (**負の値も出す** — 枠待ちの深さが読めなくなる)。
	levState    atomic.Value
	levHeadroom atomic.Int64

	// blocks は live の銘柄ごとの新規停止(人間のボタン)。nil = 無効(research)。
	blocks port.SymbolBlockReader

	// rejections は「発火した候補を arm しなかった理由」の監査証跡(signal_rejections)。
	// nil = 書かない(research)。now は行の時刻。notArmedSeen は (銘柄 → 理由) のエッジ記録で、
	// 連続する同じ理由は書かない(発注前ゲートの recordOn と同じ)。
	rejections   port.SignalRejectionRepository
	now          func() time.Time
	notArmedSeen map[string]string
}

// WithRejectionSink は、発火した候補を arm しなかったときに理由を signal_rejections へ残す。
//
// 🚨 paper が建てた銘柄を live の selector が arm しないとき、原因(計画損失の
// 上限超など)がログにも DB にも無いと追えない。篩が黙って continue するのが問題。
// 発注前ゲートは reject を signal_rejections に残すのに、その**先出し**である selector が
// 残さないと「arm されなかった = 何も起きなかった」に見える。
func (s *Selector) WithRejectionSink(r port.SignalRejectionRepository, now func() time.Time) *Selector {
	s.rejections = r
	s.now = now
	return s
}

// notArmed は発火した候補を arm しなかった理由を 1 行のログと(挿してあれば)signal_rejections に残す。
// 同じ (銘柄, 理由) の連続は書かない。armed で消すので、間に arm を挟めば新しいエッジになる。
func (s *Selector) notArmed(ctx context.Context, c strategy.Candidate, cfg *config.StrategyConfig, reason, detail string) {
	if s.notArmedSeen == nil {
		s.notArmedSeen = map[string]string{}
	}
	if s.notArmedSeen[c.Symbol] == reason {
		return
	}
	s.logf("selector: 発火した候補を arm しなかった", "symbol", c.Symbol, "strategy", string(c.Strategy),
		"reason", reason, "detail", detail, "score", c.Score)
	if s.rejections == nil {
		s.notArmedSeen[c.Symbol] = reason
		return
	}
	var configID string
	if cfg != nil {
		configID = cfg.ConfigID
	}
	var at time.Time
	if s.now != nil {
		at = s.now()
	}
	// insert が通ったときだけエッジを進める(一過性の DB 障害で唯一の 1 行を落とさない)。
	if err := s.rejections.InsertRejection(ctx, port.SignalRejection{
		Symbol: c.Symbol, ConfigID: configID, Reason: "selector_" + reason, Detail: detail, CreatedAt: at,
	}); err == nil {
		s.notArmedSeen[c.Symbol] = reason
	} else {
		s.logf("selector: signal_rejections への書込に失敗", "symbol", c.Symbol, "err", err)
	}
}

// WithSymbolBlocks は人間が止めた銘柄を arm しない(arm 済みなら次の Tick で外す)。
//
// 🛑 発注前ゲート(`manual_symbol_block`)の**先出し**。ゲートだけだと止めた銘柄が
// 枠を占有し続ける(CLAUDE.md §3「枠を配る前にも落とす」と同じ理由)。
// 読めない・壊れているときは 1 銘柄も arm しない(ゲートも全部 reject する)。
// 建玉中 / 決済中の銘柄は今までどおり disarm しない(決済と守りは止めない)。
func (s *Selector) WithSymbolBlocks(r port.SymbolBlockReader) *Selector {
	s.blocks = r
	return s
}

// blockedSymbols は止めた銘柄の集合。第 2 返り値が false なら読めない(arm しない)。
func (s *Selector) blockedSymbols(ctx context.Context) (map[string]bool, bool) {
	if s.blocks == nil {
		return nil, true
	}
	list, err := s.blocks.List(ctx)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("selector: 銘柄の停止を読めない — live の新規を全部止める(fail-close)", "err", err)
		}
		return nil, false
	}
	out := make(map[string]bool, len(list))
	for _, b := range list {
		out[b.Symbol] = true
	}
	return out, true
}

// LeverageState は selector が見た**口座の資金枠**の状態。
type LeverageState string

const (
	// LeverageOff はレバ上限を持たない構成(research)。従来どおり資金枠を見ない。
	LeverageOff LeverageState = ""
	// LeverageOK は 1 円以上の余力がある状態(候補ごとの判定はそのあと)。
	LeverageOK LeverageState = "ok"
	// LeverageFundless は **資金枠なし(枠待ち)**。建玉合計が上限に達していて、
	// どの候補も構造的に建たない。
	LeverageFundless LeverageState = "fundless"
	// LeverageUnknown は口座照会が読めない状態。**arm しない**(fail-close)。
	LeverageUnknown LeverageState = "unknown"
)

// AccountMarginReader は口座の保証金を**キャッシュ経由で**読む口。
// 🛑 生の broker を渡さない —— selector は毎 Tick 呼ぶので、直に叩くと
// 銘柄ごとに照会する通信量が別の場所から復活する(command.AccountMarginCache が満たす)。
type AccountMarginReader interface {
	Get(ctx context.Context) (*order.AccountMargin, bool)
}

// leverageBudget は 1 Tick ぶんの資金枠。arm 済みぶんを足しながら使う。
type leverageBudget struct {
	on         bool
	ratio      float64
	collateral int
	openGross  int // 既存建玉 + **この Tick で既に arm したぶん**
}

// Selector のランキング(RankCandidates の score 降順)は **単一戦略でしか正しくない**。
// score の定義は戦略ごとに違って比較できない(abs_momentum は 現在値/200日線 で上限なし
// = 1.3〜1.8、donchian は 現在値/20日高値 で構造上 1.0 付近が上限)ため、複数戦略の行を
// 降順に並べると上限の無い戦略が全枠を独占する。実測: 選択 80 件中 73 件が
// abs_momentum、19 銘柄トリガーしたドンチャンは 0 件 = audition 不成立。
// advisor 経路はこれを selectAdvisePicks の戦略別ラウンドロビンで解いたが、**Selector は
// 解いていない**。現行配線(`strategy.LiveArmScreeners`)は必ず 1 個以下しか渡さないので
// 発火しないが、それは配線側の暗黙の前提なので、ここで機械的に固定する。
func ValidateSelectorScreeners(screeners []strategy.Screener) error {
	if len(screeners) <= 1 {
		return nil
	}
	names := make([]string, 0, len(screeners))
	for _, s := range screeners {
		names = append(names, string(s.Name()))
	}
	return fmt.Errorf("selector: 複数戦略のスクリーナー %v は渡せない — score は戦略間で比較不能で、"+
		"降順ランキングでは上限の無い戦略が全枠を独占する。"+
		"複数戦略を arm したいなら selectAdvisePicks の戦略別ラウンドロビンを使う", names)
}

// SelectorTier は **優先順位つきの arm 段**。先頭が最優先で、前の段が空き枠を
// 取り切ったら次の段は 1 本も arm されない。
//
// 🚨 なぜラウンドロビンでも合成ランキングでもないか: `ValidateSelectorScreeners` が
// 禁じているとおり score は戦略間で比較不能で、降順に混ぜると上限の無い戦略が全枠を
// 独占する(実測: 80枠中 73 が abs_momentum、19銘柄トリガーした donchian は
// 0 件 = audition 不成立)。**優先ティアは score を一度も跨いで比較しない** —
// ランキングは常に 1 ティアの中だけで起きるので、この問題が構造的に発生しない。
//
// 🛑 1 ティアには **1 戦略しか入れない**(`ValidateSelectorScreeners` が機械強制)。
// ティアを分けるのは戦略に優先順位を付けるためであって、束ねるためではない。
type SelectorTier struct {
	Screeners []strategy.Screener
	// ArmCfg は arm する config を組む(戦略テンプレートを銘柄に clone する)。
	ArmCfg func(symbol string) *config.StrategyConfig
	// MaxNotionalJPY は **このティアの** 1単元の建玉金額上限(close×qty)。0 = 無効。
	// 🛑 ティアごとに違ってよい(live: bnf 550,000 / donchian_v2_trail 300,000)。
	MaxNotionalJPY int
}

// UnaffordableKey は「上限超」報告のキー。**(銘柄, 戦略)** で持つ —— ティアごとに
// 建玉金額上限が違うので、銘柄だけのキーだと「donchian の上限で落ちた」ことが
// bnf の行にも「上限超」の印を付けてしまう。
func UnaffordableKey(symbol string, name config.StrategyName) string {
	return symbol + "|" + string(name)
}

// NewSelector は単一戦略の Selector(既存の配線)。内部的には 1 ティアの
// NewSelectorTiered と同じ。
func NewSelector(candles port.CandleRepository, posRepo port.PositionRepository,
	holders map[string]*ActiveConfigHolder, screeners []strategy.Screener, accountMax, maxNotionalJPY int,
	armCfg, noTradeCfg func(string) *config.StrategyConfig,
	persist func(*config.StrategyConfig) error, logger *slog.Logger) *Selector {
	return NewSelectorTiered(candles, posRepo, holders,
		[]SelectorTier{{Screeners: screeners, ArmCfg: armCfg, MaxNotionalJPY: maxNotionalJPY}},
		accountMax, noTradeCfg, persist, logger)
}

// NewSelectorTiered は **優先順位つき**の Selector。tiers は先頭が最優先。
func NewSelectorTiered(candles port.CandleRepository, posRepo port.PositionRepository,
	holders map[string]*ActiveConfigHolder, tiers []SelectorTier, accountMax int,
	noTradeCfg func(string) *config.StrategyConfig,
	persist func(*config.StrategyConfig) error, logger *slog.Logger) *Selector {
	if accountMax < 1 {
		accountMax = 1
	}
	s := &Selector{
		candles: candles, posRepo: posRepo, holders: holders, tiers: tiers,
		accountMax: accountMax, noTradeCfg: noTradeCfg, persist: persist, logger: logger,
	}
	s.affordableN.Store(-1) // 未計測。0(= 全銘柄が上限超)と区別する
	s.levState.Store(LeverageOff)
	return s
}

// WithMaxRiskPerTradeJPY は **1本あたりの計画損失**(計画SL幅 × 株数)の上限を挿す。
// 0 / 未挿入 = 無効。
//
// 🚨 これは発注前ゲート(`risk` の `risk_per_trade`)の**先出し**であって、二重化では
// ない。ゲートだけだと **建玉にならない候補が口座の建玉枠を占有し続ける** —— selector は
// 毎 Tick 同じ日足から決定論的に同じ首位を arm するので、「arm 済み・発注は毎回 reject」
// のまま枠が永久に空かない(分割未調整の銘柄が起こす「発注待ちのまま
// 動かない沈黙のデッドロック」と同じ形)。事前登録が貸借の売り候補について
// 「枠を配る前にも落とす」と定めているのと同じ理由。
//
// 🛑 **判定の基準はゲートと同一に保つ**(どちらも `> cap` で切る)。片方だけ `>=` に
// すると「arm されたのに必ず reject」または「arm されないのにゲートは通す」がズレて出て、
// signal_rejections と画面の armed が食い違う。
//
// 🛑 **これは SL を狭めるものではない**(出口の幾何は戦略が決めたまま)。詳細は
// `risk.AccountSnapshot.MaxRiskPerTradeJPY`。
func (s *Selector) WithMaxRiskPerTradeJPY(cap int) *Selector {
	s.maxRiskPerTradeJPY = cap
	return s
}

// WithLeverageHeadroom は **口座の資金枠**を arm の前提条件にする。ratio<=0 / margin nil
// で無効(research はレバ上限を持たない)。
//
// 🚨 この配線の理由: 余力が候補 1 本の金額を下回る口座で selector が
// その候補(8001)を arm した。**arm した時点で成立しえない**ので、場中ずっと
// 「毎ティック entry シグナル → 構造ゲート通過 → 口座照会 → gross_notional_cap で
// reject」を繰り返し、後場だけで wire を数千回焼いた。
//
// 🛑 判定は `risk.WithinGrossNotionalCap` を**そのまま**呼ぶ(発注前ゲートと同一の式)。
// ここで算術を書き写すと、片方だけ直したときに「arm されたのに必ず reject」または
// 「arm されないのにゲートは通す」がズレて出る。
//
// 🛑 **順番は「口座 → 候補」**: 先に「そもそも 1 本でも
// 建てられるのか」を見て、駄目なら **枠待ち(資金枠なし)** として 1 銘柄も arm しない。
// 建てられるなら、そこから候補ごとに 1単元がティア上限以内か / 余力に収まるかを見る。
func (s *Selector) WithLeverageHeadroom(m AccountMarginReader, ratio float64) *Selector {
	s.margin, s.leverageRatio = m, ratio
	return s
}

// LeverageState は直近 Tick 時点の資金枠の状態(画面/診断用)。
func (s *Selector) LeverageState() LeverageState {
	v, _ := s.levState.Load().(LeverageState)
	return v
}

// LeverageHeadroomJPY は直近 Tick 時点の余力(上限 − 建玉合計)。
// 🛑 **負の値もそのまま返す** —— 0 に丸めると「あと少しで空く」と「大幅に超過」が
// 同じ表示になり、枠待ちがいつ明けるか読めなくなる。
func (s *Selector) LeverageHeadroomJPY() int { return int(s.levHeadroom.Load()) }

// evaluateLeverage は Tick 冒頭の**口座側**の判定。第 2 返り値が false なら
// この Tick では 1 銘柄も arm しない。
func (s *Selector) evaluateLeverage(ctx context.Context) (leverageBudget, bool) {
	if s.leverageRatio <= 0 || s.margin == nil {
		s.levState.Store(LeverageOff)
		s.levHeadroom.Store(0)
		return leverageBudget{}, true
	}
	am, ok := s.margin.Get(ctx)
	if !ok || am == nil || am.Equity <= 0 {
		// 🛑 fail-close。通しても発注前ゲートが margin_status_unavailable で必ず落とすので、
		// arm しても「毎ティック照会して毎ティック reject」が増えるだけ。
		s.levState.Store(LeverageUnknown)
		s.levHeadroom.Store(0)
		return leverageBudget{}, false
	}
	open, err := s.posRepo.ListOpenAllSymbols(ctx)
	if err != nil {
		s.logf("selector: 建玉合計が読めない — 資金枠を判定できないので arm しない", "err", err)
		s.levState.Store(LeverageUnknown)
		s.levHeadroom.Store(0)
		return leverageBudget{}, false
	}
	gross := 0
	for _, p := range open {
		gross += int(p.EntryPrice * float64(p.Quantity))
	}
	b := leverageBudget{on: true, ratio: s.leverageRatio, collateral: int(am.Equity), openGross: gross}
	headroom := int(float64(b.collateral)*b.ratio) - gross
	s.levHeadroom.Store(int64(headroom))
	if headroom <= 0 {
		s.levState.Store(LeverageFundless)
		return b, false
	}
	s.levState.Store(LeverageOK)
	return b, true
}

// withinLeverageHeadroom は候補 1 本ぶんが資金枠に収まるか。**この Tick で既に arm した
// ぶんを足して**判定する —— 個別には収まる候補を並べて arm すると、実際に建つのは
// 1 本目だけで、残りは「毎ティック reject」の空回りになる。
func (b *leverageBudget) admits(cfg *config.StrategyConfig, daily []market.Candle) bool {
	if !b.on {
		return true
	}
	if cfg == nil || cfg.Risk.Quantity <= 0 || len(daily) == 0 {
		return false // 値段が無ければ判定できない = 通さない(fail-close)
	}
	newGross := int(daily[len(daily)-1].Close * float64(cfg.Risk.Quantity))
	if !risk.WithinGrossNotionalCap(b.collateral, b.openGross, newGross, b.ratio) {
		return false
	}
	b.openGross += newGross // 配った枠は消費する
	return true
}

// headroomJPY は、この Tick で既に配ったぶんを引いた残りの余力(監査の detail 用)。
func (b *leverageBudget) headroomJPY() int {
	if !b.on {
		return 0
	}
	return int(float64(b.collateral)*b.ratio) - b.openGross
}

// 冪等(同じ日足 + 同じ建玉なら同じ銘柄が arm される)なので任意の周期で呼べる。
func (s *Selector) Tick(ctx context.Context) {
	openCount, err := s.posRepo.CountOpenAllSymbols(ctx)
	if err != nil {
		s.logf("selector: count open positions failed", "err", err)
		return
	}

	// 建玉中/決済中の銘柄: disarm もしない(config 凍結)し再 arm もしない(枠は既に
	// その銘柄のもの)。
	held := make(map[string]bool, len(s.holders))
	for sym := range s.holders {
		if s.holdsPosition(ctx, sym) {
			held[sym] = true
		}
	}

	// 🛑 ユニバースと arm config は**空き枠の有無に関わらず**組む。「スキャン対象 N 銘柄
	// のうち、資金上限で実際に建てられるのは何銘柄か」は画面に出す内数で、free > 0 の
	// ときだけ数えると枠が埋まっている間ずっと古い値が残る — その内数を一番知りたいのは
	// **枠が埋まっている時**(arm が走らないので画面から実像を確かめる手段が他に無い)。
	// 代償は armCfg(clone + hard-limit 検証)を毎 Tick 全銘柄ぶん回すこと。どちらも
	// 純粋な関数で、rearm 間隔(既定 30 分)に対して無視できる。
	// 🛑 **口座の資金枠を先に見る**。余力が無い / 読めない
	// なら、候補を 1 本も arm しない = 枠待ち。理由は LeverageState として画面へ出す。
	budget, mayArm := s.evaluateLeverage(ctx)
	blocked, blocksOK := s.blockedSymbols(ctx)
	mayArm = mayArm && blocksOK

	universe := s.loadUniverse(ctx)
	// ティアごとに arm config と資金内数を組む。**空き枠の有無に関わらず**組むのは
	// 上のコメントのとおり(内数を一番知りたいのは枠が埋まっている時)。
	cfgs := make([]map[string]*config.StrategyConfig, len(s.tiers))
	affordable := 0
	tooBig := map[string]bool{}
	for ti, tier := range s.tiers {
		cfgs[ti] = make(map[string]*config.StrategyConfig, len(universe))
		for sym, cs := range universe {
			cfg := tier.ArmCfg(sym)
			if cfg == nil {
				continue // hard limits が arm を拒否 = そもそも建たない。内数にも入れない
			}
			cfgs[ti][sym] = cfg
			if affordableIn(tier.MaxNotionalJPY, cfg, cs) {
				// 🛑 内数は **最優先ティア**のぶんだけ数える。画面の「スキャン対象 N のうち
				// 建てられるのは M」は 1 つの数字なので、ティアを跨いで足すと同じ銘柄を
				// 二重に数えて母数と合わなくなる。下位ティアの内数は ranking の行が持つ。
				if ti == 0 {
					affordable++
				}
			} else {
				tooBig[UnaffordableKey(sym, cfg.StrategyName)] = true
			}
		}
	}
	s.affordableN.Store(int64(affordable))
	s.unaffordable.Store(tooBig)

	// 空き枠(accountMax − 建玉数)を、建玉していない上位候補で埋める。首位が建玉中でも
	// 残りの枠は止まらない(走査中に飛ばすだけ)。config は上で組んだものを使い回す
	// (armCfg の clone + hard-limit 検証を二度走らせない)。
	//
	// 🛑 **ティアは順番に処理し、score を跨いで比較しない。** 前のティアが枠を取り切ったら
	// 後ろのティアは 1 本も arm されない(bnf 最優先、余った枠に限り donchian_v2_trail)。
	want := make(map[string]*config.StrategyConfig)
	free := s.accountMax - openCount
	if free <= 0 || !mayArm {
		// 口座側の理由で 1 本も arm しない Tick。銘柄ごとではなく 1 行で残す(30 分おき)。
		reason := "account_full"
		switch {
		case !blocksOK:
			reason = "symbol_blocks_unreadable"
		case !mayArm:
			reason = "leverage_" + string(s.LeverageState())
		}
		s.logf("selector: この Tick は arm を見送った(口座側)", "reason", reason,
			"open_count", openCount, "account_max", s.accountMax,
			"lev_state", string(s.LeverageState()), "headroom_jpy", s.LeverageHeadroomJPY())
	} else {
		for ti, tier := range s.tiers {
			if len(want) >= free {
				break // 上位ティアが枠を取り切った
			}
			for _, c := range strategy.RankCandidates(universe, tier.Screeners) {
				if len(want) >= free {
					break
				}
				// 🚨 **上位ティアが未トリガーの候補で枠を埋めない。**
				// `RankCandidates` は未トリガーの候補も返す(トリガー済みが先頭に並ぶ)。
				// 従来の単一ティアではそれを arm するのが正しかった —— arm は config を
				// 差すだけで建玉は entry ゲートが決めるので、日足では未成立でも場中に
				// 成立した瞬間に入れる。ところが**ティアを重ねるとこれが牙を剥く**:
				// 最優先ティアが毎 Tick 未トリガーの首位で枠を埋め切り、下位ティアが
				// **永久に 1 本も arm されない**(= 指示が黙って無効になる)。
				// だから **最下位ティア以外はトリガー済みだけ**が枠を取る。最下位は
				// 従来どおり未トリガーでも arm する(枠を遊ばせない)。
				// 並びはトリガー済みが先頭なので、最初の未トリガーで打ち切ってよい。
				if ti < len(s.tiers)-1 && !c.Triggered {
					break
				}
				if blocked[c.Symbol] {
					// 人間が新規を止めた銘柄。**枠は消費しない**(下位が繰り上がる)
					if c.Triggered {
						s.notArmed(ctx, c, cfgs[ti][c.Symbol], "manual_symbol_block", "")
					}
					continue
				}
				if held[c.Symbol] || want[c.Symbol] != nil {
					// 建玉中、または**上位ティアが既に取った銘柄**。live のナンピン禁止キーは
					// (銘柄, 側)なので 1 銘柄に 2 戦略は載せられない — 上位が勝つ。
					continue
				}
				cfg := cfgs[ti][c.Symbol]
				// arm config が無い / 1 単元が上限超 / 計画損失が 1 本で上限超 は飛ばす。
				// **枠は消費しない**(下位が繰り上がる)。篩は LiveArmCandidates と共有する。
				// 🚨 発火した候補を落とすときは理由を残す。
				if reason, detail := tier.rejectReason(cfg, universe[c.Symbol], c, s.maxRiskPerTradeJPY); reason != "" {
					if c.Triggered {
						s.notArmed(ctx, c, cfg, reason, detail)
					}
					continue
				}
				if !budget.admits(cfg, universe[c.Symbol]) {
					// 口座の資金枠に収まらない。建たない候補に枠を配らない
					if c.Triggered {
						s.notArmed(ctx, c, cfg, "gross_notional_cap", fmt.Sprintf("1 単元 %d > 余力 %d",
							lotNotionalJPY(cfg, universe[c.Symbol]), budget.headroomJPY()))
					}
					continue
				}
				want[c.Symbol] = cfg
				delete(s.notArmedSeen, c.Symbol) // arm したらエッジを閉じる(次に落ちたら新しい 1 行)
			}
		}
	}

	for sym, h := range s.holders {
		if held[sym] {
			continue // never disarm a symbol holding/closing a position (config freeze)
		}
		if cfg := want[sym]; cfg != nil {
			if s.persist != nil {
				_ = s.persist(cfg)
			}
			if h.ConfigID() != cfg.ConfigID {
				s.logf("selector: armed", "symbol", sym, "config", cfg.ConfigID, "strategy", string(cfg.StrategyName))
			}
			h.Set(cfg)
		} else if cur := h.Get(); cur == nil || cur.StrategyName != config.StrategyNoTrade {
			h.Set(s.noTradeCfg(sym))
		}
	}
}

// loadUniverse reads the daily series ランキングに使える銘柄だけ返す。
func (s *Selector) loadUniverse(ctx context.Context) map[string][]market.Candle {
	universe := make(map[string][]market.Candle, len(s.holders))
	for sym := range s.holders {
		cs, lerr := s.candles.List(ctx, sym, port.PeriodDaily, selectorCandleLookback)
		if lerr != nil {
			continue
		}
		// 🛑 分割未調整の系列をランキングに入れない。SymbolBundle 側の段差ガードは
		// **評価を止める**だけなので発注はされないが、それだけでは足りない —
		// 25日線が実勢の数倍に居座った偽のパニックは逆張り(BNF)の首位に立ち、
		// 口座枠(実弾は 1 枠)を占めたまま永久に発注しない沈黙のデッドロックになる。
		// 分割未調整の銘柄が実際にこの状態になる(「🎯 発注待ち」のまま動かない)。
		if ok, d := ScreenableDaily(cs); !ok {
			if d != "insufficient_history" {
				s.logf("selector: 分割未調整の日足 — ランキングから除外(`make fetch-daily` で連鎖調整する)",
					"symbol", sym, "detail", d)
			}
			continue
		}
		universe[sym] = cs
	}
	return universe
}

// AffordableCount は直近 Tick 時点で「1単元の建玉金額が maxNotionalJPY 以内」だった
// 銘柄数(**-1 は未計測** — 0 = 全銘柄が上限超 と区別する)。
//
// 画面が「スキャン対象 200」しか出さないと、その 200 を全部建てうると読めてしまう。
// live は 1単元の金額上限が別に立っていて、ユニバースの半分しか
// 上限内に無い日もある(= 発火しても半分は最初から対象外)。その内数。
func (s *Selector) AffordableCount() int { return int(s.affordableN.Load()) }

// MaxNotionalJPY は **最優先ティアの** 1銘柄あたり建玉金額上限(0 = フィルタ無効)。
// 画面の脚注用。🛑 ティアごとに違う値を持つので、これは「代表値」であって全体の上限ではない
// (下位ティアの上限は ranking の行が `BlockedNotional` で表す)。
func (s *Selector) MaxNotionalJPY() int {
	if len(s.tiers) == 0 {
		return 0
	}
	return s.tiers[0].MaxNotionalJPY
}

// UnaffordableSymbols は直近 Tick 時点で 1単元が上限を超えていた銘柄(未計測なら空)。
//
// 🛑 スクリーナー(発火判定)は資金を見ない。値がさ株がパニック急落すれば当然発火するが、
// live ではその銘柄は**構造的に arm されない**ので、画面が素の発火数だけを出すと
// 「発注直前の候補」に見える。発火した候補が全部上限超だと、
// 画面は「発火中の候補 N / 発注待ち 0」のまま一日動かない。
// 返す map は Tick が作った使い捨てで、呼び手は**読むだけ**(書き換えると次の Tick まで壊れる)。
func (s *Selector) UnaffordableSymbols() map[string]bool {
	m, _ := s.unaffordable.Load().(map[string]bool)
	return m
}

func (s *Selector) holdsPosition(ctx context.Context, sym string) bool {
	open, err := s.posRepo.ListOpenOrClosing(ctx, sym)
	return err == nil && len(open) > 0
}

// 口座に対して高すぎる銘柄を armed 集合から外す。cap 0 / 株数 0 は無効(サイズを
// 当てる対象が無い — entry 時の委託保証金チェックが正)。価格が無ければ fail-close。
// 🛑 cap は **ティアごと**に渡す(live: bnf 550,000 / donchian_v2_trail 300,000)。
func affordableIn(maxNotionalJPY int, cfg *config.StrategyConfig, daily []market.Candle) bool {
	if maxNotionalJPY <= 0 {
		return true
	}
	if cfg == nil || cfg.Risk.Quantity <= 0 {
		return true
	}
	if len(daily) == 0 {
		return false
	}
	return daily[len(daily)-1].Close*float64(cfg.Risk.Quantity) <= float64(maxNotionalJPY)
}

// withinRiskPerTrade は「1本の計画損失が上限以内か」。
//
// 🛑 **計画 SL を報告しない候補(0)は通す。** 0 は「算出できなかった」であって
// 「損失ゼロ」ではないが、ここで fail-close にすると出口を config から読む戦略が
// 黙って永久に arm されなくなる(= 標本が理由の分からないまま消える)。重さの最終判定は
// 発注前ゲートが持つので、未算出はそちらへ委ねるのが正しい縮退。
func (s *Selector) withinRiskPerTrade(cfg *config.StrategyConfig, c strategy.Candidate) bool {
	return withinRiskPerTrade(s.maxRiskPerTradeJPY, cfg, c)
}

func withinRiskPerTrade(maxRiskPerTradeJPY int, cfg *config.StrategyConfig, c strategy.Candidate) bool {
	if maxRiskPerTradeJPY <= 0 {
		return true
	}
	if cfg == nil || cfg.Risk.Quantity <= 0 || c.StopLossJPY <= 0 {
		return true
	}
	return int(c.StopLossJPY*float64(cfg.Risk.Quantity)) <= maxRiskPerTradeJPY
}

// dashboard/診断の表示を決定論にするため名前順で返す。
func (s *Selector) ArmedSymbols() []string {
	var out []string
	for sym, h := range s.holders {
		if c := h.Get(); c != nil && c.StrategyName != config.StrategyNoTrade {
			out = append(out, sym)
		}
	}
	sort.Strings(out)
	return out
}

// dashboard の `armed_symbol` の定義。呼び出し側(cmd)に inline しないのは、
// backend/cmd/** がテスト存在チェックの対象外で、inline した写しが唯一の
// 検証されない契約定義になってしまうため。
func (s *Selector) ArmedSymbol() string {
	if a := s.ArmedSymbols(); len(a) > 0 {
		return a[0]
	}
	return ""
}

func (s *Selector) logf(msg string, kv ...any) {
	if s.logger != nil {
		s.logger.Info(msg, kv...)
	}
}
