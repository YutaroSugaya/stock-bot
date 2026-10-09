package query

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
)

type ForwardTradeView struct {
	Symbol      string  `json:"symbol"`
	Strategy    string  `json:"strategy,omitempty"` // resolver がある場合のみ
	Side        string  `json:"side"`
	Quantity    int     `json:"quantity"`
	EntryPrice  float64 `json:"entry_price"`
	ClosePrice  float64 `json:"close_price"`
	NetJPY      float64 `json:"net_jpy"`
	CloseReason string  `json:"close_reason"`
	ClosedAt    string  `json:"closed_at"` // JST "2006-01-02 15:04"
	// エントリー時に screener が出していた score の復元値。nil = 復元不能
	// (黙って 0 にしない)。
	Score *float64 `json:"score,omitempty"`
}

// ForwardStrategyScoreView は「強いシグナルほど稼いでいるか」の一次チェック。
// **score は戦略ごとに定義が違い横断比較してはいけない**(abs_momentum は上限
// なし、donchian は構造上 1.0 付近が上限)ので、分位は必ず戦略内で切る。
type ForwardStrategyScoreView struct {
	Restored        int     `json:"restored"` // score を復元できた trade 数
	Missing         int     `json:"missing"`  // 復元不能(60秒窓に run 無し等)
	Median          float64 `json:"median"`
	LowN            int     `json:"low_n"`
	LowAvgPer1MJPY  float64 `json:"low_avg_per_1m_jpy"`
	HighN           int     `json:"high_n"`
	HighAvgPer1MJPY float64 `json:"high_avg_per_1m_jpy"`
}

type ForwardStrategyView struct {
	Strategy    string                    `json:"strategy"`
	N           int                       `json:"n"`
	Wins        int                       `json:"wins"`
	NetJPY      float64                   `json:"net_jpy"`
	NetPer1MJPY float64                   `json:"net_per_1m_jpy"`
	Score       *ForwardStrategyScoreView `json:"score,omitempty"` // score resolver がある場合のみ
	// 🛑 **買いと売りの内訳**。`direction: both` の 8 アームは
	// 売りも建てるので、合算 net だけだと「買いで勝って売りで負けている」が平らに
	// 見え、「エッジが下落側の流動性供給に固有か」という問いが測れない。
	// `-side` で 2 回打てば分けられるが、**既定の 1 画面に出ないと採点者は合算だけ
	// 見て verdict を出す**。BuyN + SellN == N(向き不明 = 旧建玉 / external は Buy 側)。
	BuyN       int     `json:"buy_n"`
	BuyNetJPY  float64 `json:"buy_net_jpy"`
	SellN      int     `json:"sell_n"`
	SellNetJPY float64 `json:"sell_net_jpy"`
	// 🛑 **決済理由ごとの本数と net**。
	// 事前登録は 3 つの材料を挙げ、1 は `cmd/holding-period`、3 は `cmd/counterfactual`
	// が担うのに、**2 だけ集計する場所がどこにも無かった**。
	// トップレベルの `by_reason` は全戦略合算の**件数だけ**で、
	// 「max_hold で閉じた分の net が戦略ごとにいくらか」= MaxHold 短縮が
	// 効いたか効きすぎたかを、まさに答えられない形になっていた。
	ByReason []ForwardReasonView `json:"by_reason,omitempty"`
}

// ForwardReasonView は 1 戦略の中の決済理由 1 種類ぶん。
type ForwardReasonView struct {
	Reason      string  `json:"reason"`
	N           int     `json:"n"`
	Wins        int     `json:"wins"`
	NetJPY      float64 `json:"net_jpy"`
	NetPer1MJPY float64 `json:"net_per_1m_jpy"`
}

type ForwardMonthView struct {
	Month  string  `json:"month"` // JST "2006-01"
	N      int     `json:"n"`
	NetJPY float64 `json:"net_jpy"`
}

type ForwardDayView struct {
	Day         string  `json:"day"` // JST "2006-01-02"
	N           int     `json:"n"`
	Wins        int     `json:"wins"`
	NetJPY      float64 `json:"net_jpy"`
	NetPer1MJPY float64 `json:"net_per_1m_jpy"` // ¥1M notional 正規化
}

type ForwardSymbolView struct {
	Symbol      string  `json:"symbol"`
	N           int     `json:"n"`
	Wins        int     `json:"wins"`
	NetJPY      float64 `json:"net_jpy"`
	NetPer1MJPY float64 `json:"net_per_1m_jpy"`
}

// 数え方の名乗り。**同じ形の JSON が 2 つの意味を持つ**ので、出力自身に書かせる。
//   - CountingEdgeSample … 戦略の出口だけ。cmd/forward-report → cmd/edge-judge の既定。
//   - CountingAccount    … 口座に効いた往復すべて(entry_compensated / external_close 込み)。
//     ダッシュボードの戦績。**判定に流し込んではいけない**(cmd/edge-judge が弾く)。
const (
	CountingEdgeSample = "edge_sample"
	CountingAccount    = "account"
)

// ForwardReportView は forward 台帳の読み出し面。net は daily loss cap と同じ
// 規約 gross − fee + carry。NetPnLJPY は closed_at 昇順で、そのまま
// cmd/edge-judge の net_pnl_jpy 入力になる。
type ForwardReportView struct {
	// Counting は上の 2 値。**omitempty を付けない** — 空欄は「古い出力」を意味し、
	// 「口座ベース」と読み違えられてはならない。
	Counting string `json:"counting"`
	Since    string `json:"since,omitempty"` // "" = 全期間
	// Until は集計の終了日(その日の 0 時 JST・排他)。"" = 上限なし。複数の期間が
	// 同じ DB に入っていると、開始日だけでは前の期間を切り出せない。
	Until         string  `json:"until,omitempty"`
	N             int     `json:"n"`
	Wins          int     `json:"wins"`
	NetTotalJPY   float64 `json:"net_total_jpy"`
	GrossTotalJPY float64 `json:"gross_total_jpy"`
	FeeTotalJPY   float64 `json:"fee_total_jpy"`
	CarryTotalJPY float64 `json:"carry_total_jpy"`
	SymbolN       int     `json:"symbol_n"`
	// UniverseN は SymbolN の写し。edge-judge の銘柄集中ゲート(嘘発見器)は
	// `universe_n` というキーを読むので冗長でも名前を合わせる — symbol_n しか
	// 出していなかったため、判定が universe_n=0 で**黙ってそのゲートをスキップ**
	// していた。
	UniverseN      int                   `json:"universe_n"`
	ByMonth        []ForwardMonthView    `json:"by_month"`
	ByDay          []ForwardDayView      `json:"by_day"`                // 新しい日が先
	BySymbol       []ForwardSymbolView   `json:"by_symbol"`             // net 降順
	ByStrategy     []ForwardStrategyView `json:"by_strategy,omitempty"` // resolver がある場合のみ・net 降順
	ByReason       map[string]int        `json:"by_reason"`
	StrategyFilter string                `json:"strategy_filter,omitempty"` // フィルタ適用時にその名前
	SideFilter     string                `json:"side_filter,omitempty"`     // "BUY" / "SELL"(売り側 judge)
	// ¥1M notional 正規化。建玉金額が銘柄で 20 倍以上ばらつくので、生の円で判定
	// すると「値がさ株を掴んだ戦略」が強く見える。**edge-judge にはこちらを渡す**。
	NetPer1MTotalJPY float64   `json:"net_per_1m_total_jpy"`
	NetPnLJPY        []float64 `json:"net_pnl_jpy"`
	NetPer1MJPY      []float64 `json:"net_per_1m_jpy"`
	// Days は net 系列と**同順同長**のトレード1本ごとの JST 日ラベル(ByDay の
	// 集計とは別物)。edge-judge の day-block bootstrap の入力 — 200銘柄を同時に
	// 見る forward 標本はトレードが日単位でクラスタするので、トレード単位の復元
	// 抽出だと CI が楽観側に出る。
	Days   []string           `json:"days"`
	Trades []ForwardTradeView `json:"trades"`
	// 復元できなかった score は黙って落とさず必ず数える。
	ScoreMissingN int `json:"score_missing_n,omitempty"`
	// 「約定後に巻き戻した往復」(close_reason=entry_compensated)の件数。
	// **Counting で意味が変わる**: edge_sample では上の N から除外した件数、
	// account では **N の内数**。どちらでも**黙って落とさない** — 台帳から消えたのか、
	// そもそも起きなかったのかを区別できるようにする。
	CompensatedN int `json:"compensated_n,omitempty"`
	// external(人間が建てた)建玉の決済。CompensatedN と同じ扱い。
	ExternalN int `json:"external_n,omitempty"`
	// その net(= gross − fee + carry)。**件数だけでは足りない** — 画面が
	// 「何も起きなかった」と読めてしまう
	// (n:0 / compensated_n:5 だと戦績が消えたように見える)。口座には実額として
	// 効いているので額も出す。`omitempty` を付けない: 0円と「集計していない」は違う。
	CompensatedNetJPY float64 `json:"compensated_net_jpy"`
	ExternalNetJPY    float64 `json:"external_net_jpy"`
	// 絞った閾値(0 = 絞っていない)を出力に残す — 部分集合の数字が全体の数字と
	// 読み違えられないように。
	MaxNotionalJPY float64 `json:"max_notional_jpy,omitempty"`
}

// nonStrategyTally は「戦略の出口ではない決済」の件数と net。件数と額を1つの型に
// まとめてあるのは、片方だけ足して片方を忘れる差分を作らせないため。**数えるのは
// 常に行い**、集計から外すかどうかだけが countNonStrategy で切り替わる。
type nonStrategyTally struct {
	compensatedN   int
	externalN      int
	compensatedNet float64
	externalNet    float64
}

type BuildForwardReport struct {
	trades      port.ClosedTradeReader
	strategies  port.TradeStrategyResolver // optional: 戦略別分計(Postgres のみ)
	scores      port.TradeScoreResolver    // optional: エントリー時 score の復元(Postgres のみ)
	filter      string                     // optional: この戦略の trade だけに絞る
	sideFilter  string                     // optional: "BUY"/"SELL" だけに絞る
	maxNotional float64                    // optional: 建玉金額の上限(0 = 無制限)
	// countNonStrategy: 戦略の出口ではない決済も普通のトレードとして集計に入れる。
	// **既定 false = エッジ標本**。true にするのは画面(/api/performance)だけ。
	countNonStrategy bool
}

type ForwardReportOption func(*BuildForwardReport)

func WithStrategyResolver(sr port.TradeStrategyResolver) ForwardReportOption {
	return func(q *BuildForwardReport) { q.strategies = sr }
}

// WithStrategyFilter narrows every aggregate to one strategy — the unit you feed
// to cmd/edge-judge. Requires a resolver.
func WithStrategyFilter(name string) ForwardReportOption {
	return func(q *BuildForwardReport) { q.filter = name }
}

// WithMaxNotionalJPY は資金キャパシティ分計(建玉金額 entry×qty ≤ max)。¥1M
// 正規化は「単位あたりの強さ」しか測らず「その株を買えるか」は測らないので別に見る。
// **診断であって昇格ゲートではない** — 部分集合の切り出しは多重性を増やすので、
// 事後の勝ち探しには使わない。max <= 0 は無制限。notional が取れない行
// (entry<=0 / qty=0)は**残す** — 落とすと「小資金でも成立」を過大に見せる。
func WithMaxNotionalJPY(max float64) ForwardReportOption {
	return func(q *BuildForwardReport) { q.maxNotional = max }
}

// WithSideFilter narrows every aggregate to one side ("BUY" / "SELL").
//
// 🚨 事前登録した「**売り側だけ**の edge-judge」を回すのに要る。これが無いと
// 売り側の net 系列を切り出せず、「向きを開けたのが良かったか」を判定する道具が
// 存在しないまま締めを迎える。
//
// 🛑 **診断であって昇格ゲートではない**(-strategy / -max-notional-jpy と同じ扱い)。
// 部分集合の切り出しは多重性を増やす。事前登録に無い切り方で勝ち探しをしない。
func WithSideFilter(side string) ForwardReportOption {
	return func(q *BuildForwardReport) { q.sideFilter = strings.ToUpper(strings.TrimSpace(side)) }
}

// WithNonStrategyClosesCounted は `entry_compensated`(約定後に守りを置けず巻き戻した
// 往復)と `external_close`(人間が建てた建玉の決済)を**普通のトレードとして戦略に
// 計上する**(= 口座ベース)。ダッシュボードの戦績はこちら。
//
// 🛑 **エッジ判定にはこれを付けない**。cmd/forward-report は既定のまま = 戦略の出口
// だけを標本にする(事前コミット)。取り違えは出力の `counting`
// を見て cmd/edge-judge が fail-close で弾く。
func WithNonStrategyClosesCounted() ForwardReportOption {
	return func(q *BuildForwardReport) { q.countNonStrategy = true }
}

func WithScoreResolver(sr port.TradeScoreResolver) ForwardReportOption {
	return func(q *BuildForwardReport) { q.scores = sr }
}

func NewBuildForwardReport(tr port.ClosedTradeReader, opts ...ForwardReportOption) *BuildForwardReport {
	q := &BuildForwardReport{trades: tr}
	for _, o := range opts {
		o(q)
	}
	return q
}

// Execute reads closed trades at/after since (zero = all).
func (q *BuildForwardReport) Execute(ctx context.Context, since time.Time) (ForwardReportView, error) {
	return q.ExecuteRange(ctx, since, time.Time{})
}

// ExecuteRange は since 以降・until より前(zero = 上限なし)の決済だけを集計する。
func (q *BuildForwardReport) ExecuteRange(ctx context.Context, since, until time.Time) (ForwardReportView, error) {
	records, err := q.trades.ListClosedSince(ctx, since)
	if err != nil {
		return ForwardReportView{}, err
	}
	if !until.IsZero() {
		kept := records[:0]
		for _, r := range records {
			if r.ClosedAt.Before(until) {
				kept = append(kept, r)
			}
		}
		records = kept
	}
	// repo は closed_at 昇順を約束するが、系列順は判定入力そのものなので防御で並べ直す。
	sort.SliceStable(records, func(i, j int) bool { return records[i].ClosedAt.Before(records[j].ClosedAt) })

	names, err := q.resolveStrategyNames(ctx, records)
	if err != nil {
		return ForwardReportView{}, err
	}
	strategyOf := q.strategyResolver(names)
	if records, err = q.applyFilters(records, strategyOf); err != nil {
		return ForwardReportView{}, err
	}
	records, nonStrat := q.splitNonStrategy(records)
	scores, err := q.resolveScores(ctx, records)
	if err != nil {
		return ForwardReportView{}, err
	}

	view := q.newView(since, nonStrat)
	if !until.IsZero() {
		view.Until = until.In(clock.JST).Format("2006-01-02")
	}
	acc := newForwardAccumulator(&view, q.strategies != nil, q.scores != nil)
	for _, t := range records {
		acc.add(t, strategyOf(t), scores)
	}
	view.SymbolN = len(acc.symbols)
	view.UniverseN = view.SymbolN
	sortForwardViews(&view)
	if q.scores != nil {
		for j := range view.ByStrategy {
			st := view.ByStrategy[j].Strategy
			view.ByStrategy[j].Score = buildScoreView(acc.scoredByStrategy[st], acc.scoreMissingByStrategy[st])
		}
	}
	return view, nil
}

// positionIDs returns the distinct position ids in first-seen order.
func positionIDs(records []port.TradeRecord) []int64 {
	ids := make([]int64, 0, len(records))
	seen := map[int64]bool{}
	for _, t := range records {
		if !seen[t.PositionID] {
			seen[t.PositionID] = true
			ids = append(ids, t.PositionID)
		}
	}
	return ids
}

func (q *BuildForwardReport) resolveStrategyNames(ctx context.Context, records []port.TradeRecord) (map[int64]string, error) {
	if q.strategies == nil {
		return map[int64]string{}, nil
	}
	return q.strategies.StrategyByPositionID(ctx, positionIDs(records))
}

// strategyResolver maps a trade to its strategy name. Ids the resolver does not
// know become "unknown" — visible beats hidden for an audit.
func (q *BuildForwardReport) strategyResolver(names map[int64]string) func(port.TradeRecord) string {
	return func(t port.TradeRecord) string {
		if q.strategies == nil {
			return ""
		}
		if n := names[t.PositionID]; n != "" {
			return n
		}
		return "unknown"
	}
}

// applyFilters narrows by -strategy / -side / -max-notional, in that order.
func (q *BuildForwardReport) applyFilters(records []port.TradeRecord, strategyOf func(port.TradeRecord) string) ([]port.TradeRecord, error) {
	if q.filter != "" {
		if q.strategies == nil {
			return nil, fmt.Errorf("strategy filter %q requires a strategy resolver (Postgres)", q.filter)
		}
		records = keepTrades(records, func(t port.TradeRecord) bool { return strategyOf(t) == q.filter })
	}
	if q.sideFilter != "" {
		records = keepTrades(records, func(t port.TradeRecord) bool { return strings.EqualFold(string(t.Side), q.sideFilter) })
	}
	// notional 不明は残す(落とすと「小資金でも成立」を過大に見せる)。
	if q.maxNotional > 0 {
		records = keepTrades(records, func(t port.TradeRecord) bool {
			n := notionalJPY(t.EntryPrice, t.Quantity)
			return n == 0 || n <= q.maxNotional
		})
	}
	return records, nil
}

func keepTrades(records []port.TradeRecord, keep func(port.TradeRecord) bool) []port.TradeRecord {
	out := make([]port.TradeRecord, 0, len(records))
	for _, t := range records {
		if keep(t) {
			out = append(out, t)
		}
	}
	return out
}

// splitNonStrategy tallies entry_compensated / external_close and drops them from
// the edge sample unless the report counts on an account basis.
//
// 🛑 **エッジ標本からだけ** entry_compensated / external_close を外す。
// 約定した後に守りを board に置けず entry saga が巻き戻した往復は、**戦略の出口が
// 一度も発火していない**。混ぜると「戦略が n 回試して負けた」に見え、N(day-block
// bootstrap の分母)も銘柄集中ゲートの universe_n も歪む。external(人間が建てた
// 建玉)の決済も同じで、口座には実額として効いているが bot の戦略の出口ではない。
//
// 🛑 **画面(countNonStrategy)では外さない**。全件が
// 巻き戻しだった日に戦績が空に見え、口座で動いた実額が画面から消えるため。
// どちらで数えたかは view.Counting が名乗る。
//
// 🛑 **数えるのは -strategy / -max-notional で絞った後**。先に数えると「その部分集合で
// 何件除外したか」ではなく全体の件数が出て、部分集合の報告に全体の数字が混ざる。
func (q *BuildForwardReport) splitNonStrategy(recs []port.TradeRecord) (kept []port.TradeRecord, ns nonStrategyTally) {
	kept = make([]port.TradeRecord, 0, len(recs))
	for _, t := range recs {
		switch t.CloseReason {
		case port.CloseReasonEntryCompensated:
			ns.compensatedN++
			ns.compensatedNet += t.ProfitLossJPY - t.FeeJPY + t.CarryJPY
		case port.CloseReasonExternalClose:
			ns.externalN++
			ns.externalNet += t.ProfitLossJPY - t.FeeJPY + t.CarryJPY
		}
		// 件数・net は常に数え、**集計に残すかどうか**だけを切り替える。
		if q.countNonStrategy || !port.IsNonStrategyClose(t.CloseReason) {
			kept = append(kept, t)
		}
	}
	return kept, ns
}

// resolveScores loads screen scores; ids missing from the map are counted as
// ScoreMissingN by the accumulator rather than silently dropped.
func (q *BuildForwardReport) resolveScores(ctx context.Context, records []port.TradeRecord) (map[int64]port.TradeScore, error) {
	if q.scores == nil {
		return map[int64]port.TradeScore{}, nil
	}
	return q.scores.ScoreByPositionID(ctx, positionIDs(records))
}

func (q *BuildForwardReport) newView(since time.Time, ns nonStrategyTally) ForwardReportView {
	view := ForwardReportView{ByReason: map[string]int{}, Counting: CountingEdgeSample}
	if q.countNonStrategy {
		view.Counting = CountingAccount
	}
	if !since.IsZero() {
		view.Since = since.In(clock.JST).Format("2006-01-02")
	}
	view.StrategyFilter = q.filter
	view.SideFilter = q.sideFilter
	view.MaxNotionalJPY = q.maxNotional
	view.CompensatedN = ns.compensatedN
	view.ExternalN = ns.externalN
	view.CompensatedNetJPY = ns.compensatedNet
	view.ExternalNetJPY = ns.externalNet
	return view
}

// forwardAccumulator folds one trade at a time into the per-month / day / symbol /
// strategy views while keeping the net series and Days aligned index-for-index.
type forwardAccumulator struct {
	view       *ForwardReportView
	byStrategy bool
	withScores bool
	symbols    map[string]bool
	monthIdx   map[string]int
	dayIdx     map[string]int
	symIdx     map[string]int
	stratIdx   map[string]int

	scoredByStrategy       map[string][]scoredTrade
	scoreMissingByStrategy map[string]int
}

func newForwardAccumulator(view *ForwardReportView, byStrategy, withScores bool) *forwardAccumulator {
	return &forwardAccumulator{
		view: view, byStrategy: byStrategy, withScores: withScores,
		symbols: map[string]bool{}, monthIdx: map[string]int{}, dayIdx: map[string]int{},
		symIdx: map[string]int{}, stratIdx: map[string]int{},
		scoredByStrategy: map[string][]scoredTrade{}, scoreMissingByStrategy: map[string]int{},
	}
}

func (a *forwardAccumulator) add(t port.TradeRecord, st string, scores map[int64]port.TradeScore) {
	view := a.view
	net := t.ProfitLossJPY - t.FeeJPY + t.CarryJPY
	n1m := position.Per1MNotional(net, t.EntryPrice, t.Quantity) // 出せない行は 0 で系列長を保つ(落とすと判定の N が実際と食い違う)
	closedAt := t.ClosedAt.In(clock.JST)

	view.N++
	if net > 0 {
		view.Wins++
	}
	view.NetTotalJPY += net
	view.GrossTotalJPY += t.ProfitLossJPY
	view.FeeTotalJPY += t.FeeJPY
	view.CarryTotalJPY += t.CarryJPY
	a.symbols[t.Symbol] = true
	view.ByReason[t.CloseReason]++
	view.NetPnLJPY = append(view.NetPnLJPY, net)
	view.NetPer1MJPY = append(view.NetPer1MJPY, n1m)
	view.NetPer1MTotalJPY += n1m

	a.addMonth(closedAt.Format("2006-01"), net)
	day := closedAt.Format("2006-01-02")
	// net 系列と同順同長を保つため、集計の分岐より前に無条件で append する。
	view.Days = append(view.Days, day)
	a.addDay(day, net, n1m)
	a.addSymbol(t.Symbol, net, n1m)
	if a.byStrategy {
		a.addStrategy(st, t, net, n1m)
	}

	var scorePtr *float64
	if a.withScores {
		if s, ok := scores[t.PositionID]; ok {
			v := s.Score
			scorePtr = &v
			a.scoredByStrategy[st] = append(a.scoredByStrategy[st], scoredTrade{score: s.Score, per1M: n1m})
		} else {
			view.ScoreMissingN++
			a.scoreMissingByStrategy[st]++
		}
	}
	view.Trades = append(view.Trades, ForwardTradeView{
		Symbol: t.Symbol, Strategy: st, Side: string(t.Side), Quantity: t.Quantity,
		EntryPrice: t.EntryPrice, ClosePrice: t.ClosePrice, NetJPY: net,
		CloseReason: t.CloseReason, ClosedAt: closedAt.Format("2006-01-02 15:04"),
		Score: scorePtr,
	})
}

func (a *forwardAccumulator) addMonth(month string, net float64) {
	i, ok := a.monthIdx[month]
	if !ok {
		i = len(a.view.ByMonth)
		a.monthIdx[month] = i
		a.view.ByMonth = append(a.view.ByMonth, ForwardMonthView{Month: month})
	}
	a.view.ByMonth[i].N++
	a.view.ByMonth[i].NetJPY += net
}

func (a *forwardAccumulator) addDay(day string, net, n1m float64) {
	i, ok := a.dayIdx[day]
	if !ok {
		i = len(a.view.ByDay)
		a.dayIdx[day] = i
		a.view.ByDay = append(a.view.ByDay, ForwardDayView{Day: day})
	}
	v := &a.view.ByDay[i]
	v.N++
	v.NetJPY += net
	v.NetPer1MJPY += n1m
	if net > 0 {
		v.Wins++
	}
}

func (a *forwardAccumulator) addSymbol(symbol string, net, n1m float64) {
	i, ok := a.symIdx[symbol]
	if !ok {
		i = len(a.view.BySymbol)
		a.symIdx[symbol] = i
		a.view.BySymbol = append(a.view.BySymbol, ForwardSymbolView{Symbol: symbol})
	}
	v := &a.view.BySymbol[i]
	v.N++
	v.NetJPY += net
	v.NetPer1MJPY += n1m
	if net > 0 {
		v.Wins++
	}
}

func (a *forwardAccumulator) addStrategy(st string, t port.TradeRecord, net, n1m float64) {
	j, ok := a.stratIdx[st]
	if !ok {
		j = len(a.view.ByStrategy)
		a.stratIdx[st] = j
		a.view.ByStrategy = append(a.view.ByStrategy, ForwardStrategyView{Strategy: st})
	}
	v := &a.view.ByStrategy[j]
	v.N++
	if net > 0 {
		v.Wins++
	}
	v.NetJPY += net
	v.NetPer1MJPY += n1m
	if strings.EqualFold(string(t.Side), "SELL") {
		v.SellN++
		v.SellNetJPY += net
	} else {
		v.BuyN++ // 向き不明(旧建玉 / external)は買い側
		v.BuyNetJPY += net
	}
	addReason(v, t.CloseReason, net, n1m)
}

func sortForwardViews(view *ForwardReportView) {
	// 日別は「新しい日が先」— 直近の戦績を上から読むための並び。
	sort.SliceStable(view.ByDay, func(i, j int) bool { return view.ByDay[i].Day > view.ByDay[j].Day })
	// 銘柄別は net 降順(同額は銘柄コード昇順)。
	sort.SliceStable(view.BySymbol, func(i, j int) bool {
		if view.BySymbol[i].NetJPY != view.BySymbol[j].NetJPY {
			return view.BySymbol[i].NetJPY > view.BySymbol[j].NetJPY
		}
		return view.BySymbol[i].Symbol < view.BySymbol[j].Symbol
	})
	// 戦略別は net 降順(同額は名前昇順)。
	sort.SliceStable(view.ByStrategy, func(i, j int) bool {
		if view.ByStrategy[i].NetJPY != view.ByStrategy[j].NetJPY {
			return view.ByStrategy[i].NetJPY > view.ByStrategy[j].NetJPY
		}
		return view.ByStrategy[i].Strategy < view.ByStrategy[j].Strategy
	})
}

type scoredTrade struct{ score, per1M float64 }

// buildScoreView の分位は**戦略内**限定 — score の定義が戦略ごとに違うので、
// 横断で切ると交絡する。復元ゼロ・欠落ゼロなら nil(列を出さない)。
func buildScoreView(xs []scoredTrade, missing int) *ForwardStrategyScoreView {
	if len(xs) == 0 && missing == 0 {
		return nil
	}
	v := &ForwardStrategyScoreView{Restored: len(xs), Missing: missing}
	if len(xs) == 0 {
		return v
	}
	// stable ソートで入力順(closed_at 昇順)を同点のタイブレークにする — 同点
	// score が中央値境界を跨ぐとき、割り付けが実行ごとに揺れない。
	sort.SliceStable(xs, func(i, j int) bool { return xs[i].score < xs[j].score })
	n := len(xs)
	if n%2 == 1 {
		v.Median = xs[n/2].score
	} else {
		v.Median = (xs[n/2-1].score + xs[n/2].score) / 2
	}
	// 奇数なら中央の1本は **Low 側**(参照実測と同じ規約 — 答え
	// 合わせが割り付け規約の差でズレないよう固定する)。
	low, high := xs[:(n+1)/2], xs[(n+1)/2:]
	v.LowN, v.HighN = len(low), len(high)
	for _, x := range low {
		v.LowAvgPer1MJPY += x.per1M
	}
	if len(low) > 0 {
		v.LowAvgPer1MJPY /= float64(len(low))
	}
	for _, x := range high {
		v.HighAvgPer1MJPY += x.per1M
	}
	if len(high) > 0 {
		v.HighAvgPer1MJPY /= float64(len(high))
	}
	return v
}

// addReason は戦略ビューへ決済理由別の内訳を足す。理由の種類は 10 程度なので
// 線形探索で十分(index を別に持つとビューの並びと二重管理になる)。
func addReason(v *ForwardStrategyView, reason string, net, per1m float64) {
	for i := range v.ByReason {
		if v.ByReason[i].Reason == reason {
			v.ByReason[i].N++
			if net > 0 {
				v.ByReason[i].Wins++
			}
			v.ByReason[i].NetJPY += net
			v.ByReason[i].NetPer1MJPY += per1m
			return
		}
	}
	w := 0
	if net > 0 {
		w = 1
	}
	v.ByReason = append(v.ByReason, ForwardReasonView{
		Reason: reason, N: 1, Wins: w, NetJPY: net, NetPer1MJPY: per1m,
	})
}

// notionalJPY は取れないとき 0 を返し、呼び手はそれを「不明」として扱う
// (0円の建玉は存在しない)。売り建ての負数量も金額としては正で扱う。
func notionalJPY(entry float64, qty int) float64 {
	if entry <= 0 || qty == 0 {
		return 0
	}
	if qty < 0 {
		qty = -qty
	}
	return entry * float64(qty)
}
