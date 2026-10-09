package command

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/port"
)

// SplitAuthority は「分割を何で断定して台帳を言い直すか」。トラックごとに**明示**する
// (配線の有無で黙って切り替わると、忘れたときに緑のまま別の挙動に落ちる)。
type SplitAuthority int

const (
	// SplitAuthorityPrice は paper / paper_live_feed。建玉の正本が台帳(と紙の帳簿)なので、
	// 権利落ちの値段の証拠(`market.ExDateSplitRatio`)だけで言い直す。
	SplitAuthorityPrice SplitAuthority = iota + 1
	// SplitAuthorityBroker は live(立花)。**建玉の正本は broker**なので、値段の証拠は
	// 「分割らしい」の引き金にとどめ、broker の建玉照会が分割後の株数を示したときだけ
	// 言い直す。確かめられなければその建玉の自動決済を止めて緊急停止する。
	SplitAuthorityBroker
)

// splitHoldReason は live で分割を確かめられなかったときの緊急停止の理由(接頭辞)。
const splitHoldReason = "split_unconfirmed"

// SplitGuard は**株式分割(併合)の権利落ち日**に、分割前の円で凍結した建玉を守る。
//
// 🚨 なぜ要るか(paper で踏んだ 1:5 分割の例): 建値 11,570 / SL 10,5xx 円の建玉が、
// 権利落ちの寄り 2,495 円で SL を「割った」と判定され、実際は 500 株・建値 2,314 円相当の
// **含み益**なのに見かけ上の大きな損失として損切りされた。live で同じことが起きると、分割前の SL
// で 100 株だけ売って残りが守りの無い建玉になる。
//
// OnTick(ManageOpenPositions)の決済判定の**前**に通す。返すのは言い直した建玉一覧と、
// その日は自動決済しない建玉(live で確かめられなかったもの)。
//
// 🛑 判定の材料は **前営業日の分割未調整の終値**(運用側の日足の最終バー)と時価だけ。
// 最終バーが前営業日でなければ判定しない — 2 日ぶんの値動きを 1 日の値幅制限で測ると、
// 実相場の連続ストップ安を分割と読みうる(読むと SL を割り引いて守りが効かなくなる)。
type SplitGuard struct {
	positions port.PositionRepository
	candles   port.CandleRepository
	broker    port.Broker
	book      port.SplitAdjustableBook
	hours     session.TradingHours
	emergency EmergencyController
	authority SplitAuthority
	log       func(msg string, kv ...any)

	mu      sync.Mutex
	prev    map[string]prevCloseEntry // symbol -> その日の前営業日終値
	held    map[int64]string          // position id -> 保留した日(live で確かめられなかった)
	tripped map[string]bool           // "symbol|day" -> 緊急停止を撃った
	checked map[string]bool           // "symbol|day" -> live でその日の建玉照会を済ませた
}

type prevCloseEntry struct {
	day   string
	close float64
	ok    bool
	at    time.Time // 取得した時刻。**見つからなかった結果は prevCloseRetry だけ**覚える
}

// prevCloseRetry は「前営業日のバーが無い」を覚えておく長さ。1 日ぶん覚えると、日足の更新
// (朝の fetch-daily)より先に起動した日はその日ずっと分割を判定できない。
const prevCloseRetry = 5 * time.Minute

func NewSplitGuard(pr port.PositionRepository, candles port.CandleRepository, brk port.Broker,
	hours session.TradingHours, em EmergencyController, authority SplitAuthority) *SplitGuard {
	return &SplitGuard{
		positions: pr, candles: candles, broker: brk, hours: hours, emergency: em, authority: authority,
		prev: map[string]prevCloseEntry{}, held: map[int64]string{}, tripped: map[string]bool{}, checked: map[string]bool{},
	}
}

// WithPaperBook は紙の帳簿(paper)を挿す。台帳を言い直したら帳簿の株数・建値も揃える。
func (g *SplitGuard) WithPaperBook(b port.SplitAdjustableBook) *SplitGuard {
	g.book = b
	return g
}

// Authority はこのガードが分割を何で断定するか(配線の検査用)。
func (g *SplitGuard) Authority() SplitAuthority { return g.authority }

func (g *SplitGuard) WithLogger(fn func(msg string, kv ...any)) *SplitGuard {
	g.log = fn
	return g
}

func (g *SplitGuard) logf(msg string, kv ...any) {
	if g.log != nil {
		g.log(msg, kv...)
	}
}

// Screen は symbol の建玉 ps を price(時価)で見て、権利落ちなら言い直す。
// hold に入った建玉は**その日は自動決済しない**(呼び手は決済判定を飛ばすこと)。
func (g *SplitGuard) Screen(ctx context.Context, symbol string, price float64, now time.Time,
	ps []position.Position) (out []position.Position, hold map[int64]bool) {
	out = ps
	day := g.hours.DayStart(now)
	dayKey := day.Format(time.DateOnly)

	g.mu.Lock()
	for id, d := range g.held {
		if d != dayKey {
			delete(g.held, id) // 前日の保留は持ち越さない(翌日は前日終値が分割後になる)
		}
	}
	g.mu.Unlock()

	var targets []int
	for i, p := range ps {
		if g.isHeld(p.ID, dayKey) {
			if hold == nil {
				hold = map[int64]bool{}
			}
			hold[p.ID] = true
			continue
		}
		if p.Status != position.StatusOpen || p.Source == position.SourceExternal {
			continue
		}
		if !g.hours.DayStart(p.OpenedAt).Before(day) {
			continue // 権利落ち日に建てた建玉は最初から分割後の値段
		}
		if !p.SplitAdjustedOn.IsZero() && p.SplitAdjustedOn.Format(time.DateOnly) == dayKey {
			continue // 今日はもう言い直した
		}
		targets = append(targets, i)
	}
	if len(targets) == 0 {
		return out, hold
	}
	// 🚨 live は**値段の証拠が無くても**その日の最初のティックで 1 度だけ建玉照会する。
	// 1:1.2 のような小さい分割は値幅制限の内側(−17%)に収まり、値段からは断定できない。
	// broker の株数 ×r・建単価 ÷r がそろっていれば、それ自体が分割の証拠(唯一の権威)。
	if g.authority == SplitAuthorityBroker && g.firstCheckToday(symbol, dayKey) {
		out, hold = g.screenFromBroker(ctx, symbol, day, out, hold, targets)
		targets = g.remaining(out, targets, dayKey)
		if len(targets) == 0 {
			return out, hold
		}
	}
	prevClose, ok := g.prevClose(ctx, symbol, now)
	if !ok {
		return out, hold
	}
	r, isSplit := market.ExDateSplitRatio(prevClose, price)
	if !isSplit {
		return out, hold
	}

	out = append([]position.Position(nil), ps...)
	var atBroker map[string]port.BrokerPosition
	var brokerErr error
	if g.authority == SplitAuthorityBroker {
		atBroker, brokerErr = g.brokerPositions(ctx)
	}
	for _, i := range targets {
		p := out[i]
		adj, err := position.SplitAdjusted(p, r, day)
		if err != nil {
			hold = g.hold(hold, p, dayKey, now, fmt.Sprintf("株数が整数にならない: %v", err))
			continue
		}
		if g.authority == SplitAuthorityBroker {
			if why := confirmSplitAtBroker(p, adj, atBroker, brokerErr); why != "" {
				hold = g.hold(hold, p, dayKey, now, why)
				continue
			}
		}
		applied, err := g.positions.ApplySplit(ctx, adj)
		if err != nil || !applied {
			// 書けなかった / 別経路が先に言い直した。**このティックは決済判定しない**
			// (分割前の凍結値で判定すると誤決済する)。次のティックで読み直す。
			if hold == nil {
				hold = map[int64]bool{}
			}
			hold[p.ID] = true
			g.logf("🚨 分割調整を台帳に書けなかった — このティックは決済判定を飛ばす",
				"symbol", symbol, "position_id", p.ID, "err", err, "applied", applied)
			continue
		}
		if g.book != nil && p.BrokerPositionID != "" {
			if !g.book.AdjustPositionForSplit(p.BrokerPositionID, adj.Quantity, adj.EntryPrice) {
				g.logf("紙の帳簿に建玉が無い — 台帳だけ言い直した", "symbol", symbol, "position_id", p.ID)
			}
		}
		out[i] = adj
		g.logf("⚠ 株式分割の権利落ち — 建玉を分割後の単位へ言い直した(損益は不変)",
			"symbol", symbol, "position_id", p.ID, "株数倍率", r, "前日終値", prevClose, "時価", price,
			"株数", fmt.Sprintf("%d → %d", p.Quantity, adj.Quantity),
			"建値", fmt.Sprintf("%.1f → %.1f", p.EntryPrice, adj.EntryPrice),
			"SL", fmt.Sprintf("%.1f → %.1f", p.StopLossPrice, adj.StopLossPrice))
	}
	return out, hold
}

// firstCheckToday は (銘柄, 日) の最初の呼び出しだけ true。
func (g *SplitGuard) firstCheckToday(symbol, dayKey string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	k := symbol + "|" + dayKey
	if g.checked[k] {
		return false
	}
	g.checked[k] = true
	return true
}

// remaining は targets のうち、まだ今日言い直していない建玉の添字。
func (g *SplitGuard) remaining(ps []position.Position, targets []int, dayKey string) []int {
	var rest []int
	for _, i := range targets {
		if !ps[i].SplitAdjustedOn.IsZero() && ps[i].SplitAdjustedOn.Format(time.DateOnly) == dayKey {
			continue
		}
		rest = append(rest, i)
	}
	return rest
}

// screenFromBroker は live で、broker の建玉が**分割後の単位**(株数 ×r・建単価 ÷r)になっている
// 建玉を言い直す。照会に失敗した回は何もしない(値段の証拠の経路がそのあと判断する)。
func (g *SplitGuard) screenFromBroker(ctx context.Context, symbol string, day time.Time,
	ps []position.Position, hold map[int64]bool, targets []int) ([]position.Position, map[int64]bool) {
	atBroker, err := g.brokerPositions(ctx)
	if err != nil {
		return ps, hold
	}
	out := ps
	copied := false
	for _, i := range targets {
		p := ps[i]
		bp, ok := atBroker[p.BrokerPositionID]
		if p.BrokerPositionID == "" || !ok {
			continue
		}
		if bp.Quantity == p.Quantity {
			if brokerRightsProcessed(p, bp) {
				hold = g.hold(hold, p, dayKeyOf(day), day, fmt.Sprintf(
					"broker の建単価が %g → %g に変わり株数は %d のまま — 整数倍以外の株式分割の権利処理"+
						"(立花は株数を増やさず建単価から権利処理価格を差し引く)。比を確定できないので言い直さない",
					p.EntryPrice, bp.EntryPrice, p.Quantity))
			}
			continue
		}
		r, isSplit := brokerSplitFactor(p, bp)
		if !isSplit {
			g.logf("⚠ broker の株数が台帳と違うが、建単価が分割後の単位になっていない — 分割とは読まない",
				"symbol", symbol, "position_id", p.ID, "台帳", fmt.Sprintf("%d 株 @ %g", p.Quantity, p.EntryPrice),
				"broker", fmt.Sprintf("%d 株 @ %g", bp.Quantity, bp.EntryPrice))
			continue
		}
		adj, err := position.SplitAdjusted(p, r, day)
		if err != nil {
			continue
		}
		applied, err := g.positions.ApplySplit(ctx, adj)
		if err != nil || !applied {
			if hold == nil {
				hold = map[int64]bool{}
			}
			hold[p.ID] = true
			continue
		}
		if !copied {
			out = append([]position.Position(nil), ps...)
			copied = true
		}
		out[i] = adj
		g.logf("⚠ broker の建玉が株式分割で言い直されていた — 台帳を揃えた(損益は不変)",
			"symbol", symbol, "position_id", p.ID, "株数倍率", r,
			"株数", fmt.Sprintf("%d → %d", p.Quantity, adj.Quantity),
			"SL", fmt.Sprintf("%g → %g", p.StopLossPrice, adj.StopLossPrice))
	}
	return out, hold
}

// brokerRightsProcessed は、broker が同じ建玉を**同じ株数のまま建単価だけ変えて**持っているか。
// 立花は整数倍以外の株式分割(1:1.2 等)で株数を増やさず、建単価から権利処理価格を差し引く
// (公式「株式分割等の場合における信用建玉の取扱い」)。これを分割前の凍結値のまま扱うと、
// 権利落ちの値下がりで SL を「割った」と読む。
func brokerRightsProcessed(p position.Position, bp port.BrokerPosition) bool {
	if bp.Quantity != p.Quantity || p.EntryPrice <= 0 || bp.EntryPrice <= 0 {
		return false
	}
	return math.Abs(bp.EntryPrice/p.EntryPrice-1) > splitEntryTolerance
}

func dayKeyOf(day time.Time) string { return day.Format(time.DateOnly) }

// brokerSplitFactor は、broker が同じ建玉を**株数 ×r・建単価 ÷r** で持っているなら (r, true)。
// 株数だけが違って建単価が変わっていない(人間の部分返済など)なら false — 分割とは読まない。
// 比は一覧で縛らない(1:1.1 / 1:1.2 もある)。株数と建単価の**独立な 2 つ**がそろうことが証拠。
func brokerSplitFactor(p position.Position, bp port.BrokerPosition) (float64, bool) {
	if p.Quantity <= 0 || bp.Quantity <= 0 || bp.Quantity == p.Quantity || p.EntryPrice <= 0 || bp.EntryPrice <= 0 {
		return 0, false
	}
	r := float64(bp.Quantity) / float64(p.Quantity)
	if r < 0.01 || r > 100 {
		return 0, false
	}
	if math.Abs(bp.EntryPrice/(p.EntryPrice/r)-1) > splitEntryTolerance {
		return 0, false
	}
	return r, true
}

// confirmSplitAtBroker は live で「broker がこの建玉を分割後の株数で持っているか」。
// "" = 確認できた。それ以外は保留の理由。
func confirmSplitAtBroker(p, adj position.Position, atBroker map[string]port.BrokerPosition, err error) string {
	if err != nil {
		return fmt.Sprintf("建玉照会に失敗(確かめられない): %v", err)
	}
	if p.BrokerPositionID == "" {
		return "台帳に broker の建玉 id が無い(照合できない)"
	}
	bp, ok := atBroker[p.BrokerPositionID]
	if !ok {
		return "broker にこの建玉 id が見当たらない"
	}
	if bp.Quantity != adj.Quantity {
		return fmt.Sprintf("broker の株数 %d が分割後の %d と一致しない(台帳は %d)", bp.Quantity, adj.Quantity, p.Quantity)
	}
	// 建単価が読めるなら、それも分割後の単位になっていること(読めない 0 は株数だけで判断 —
	// こちらは値段の証拠 ExDateSplitRatio を別に持っている)。
	if bp.EntryPrice > 0 && adj.EntryPrice > 0 && math.Abs(bp.EntryPrice/adj.EntryPrice-1) > splitEntryTolerance {
		return fmt.Sprintf("broker の建単価 %g が分割後の %g と一致しない", bp.EntryPrice, adj.EntryPrice)
	}
	return ""
}

func (g *SplitGuard) brokerPositions(ctx context.Context) (map[string]port.BrokerPosition, error) {
	bps, err := g.broker.GetPositions(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[string]port.BrokerPosition, len(bps))
	for _, bp := range bps {
		m[bp.BrokerPositionID] = bp
	}
	return m, nil
}

func (g *SplitGuard) isHeld(id int64, dayKey string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.held[id] == dayKey
}

// hold は言い直せなかった建玉を**その日のあいだ**保留し、live なら銘柄ごとに 1 度だけ緊急停止する。
// 🛑 台帳は書き換えない(推測で株数を作ると broker とずれる)。緊急停止は新規を止めるだけで
// 既存の建玉には触らない — 板の守りの扱いは人間が broker の画面で確かめてから決める。
func (g *SplitGuard) hold(hold map[int64]bool, p position.Position, dayKey string, now time.Time, why string) map[int64]bool {
	if hold == nil {
		hold = map[int64]bool{}
	}
	hold[p.ID] = true
	g.mu.Lock()
	g.held[p.ID] = dayKey
	key := p.Symbol + "|" + dayKey
	first := !g.tripped[key]
	g.tripped[key] = true
	g.mu.Unlock()
	if !first {
		return hold
	}
	g.logf("🚨 株式分割の権利落ちらしいが、建玉を言い直せない — この建玉の自動決済をその日のあいだ止め、緊急停止する",
		"symbol", p.Symbol, "position_id", p.ID, "理由", why,
		"人間がやること", "broker の画面で建玉の株数・建単価と板の守り(逆指値)の状態を確かめ、必要なら守りを置き直す")
	// 🛑 緊急停止は **live だけ**。paper で撃つと研究トラックの新規が丸ごと止まる(建玉の正本が
	// 台帳なので、broker の画面で確かめる人間の作業も発生しない)。
	if g.emergency != nil && g.authority == SplitAuthorityBroker {
		_ = g.emergency.Trip(splitHoldReason+":"+p.Symbol, now)
	}
	return hold
}

// prevClose は前営業日の終値(分割未調整)。最終バーが前営業日でなければ ok=false。
func (g *SplitGuard) prevClose(ctx context.Context, symbol string, now time.Time) (float64, bool) {
	dayKey := g.hours.DayStart(now).Format(time.DateOnly)
	g.mu.Lock()
	if e, ok := g.prev[symbol]; ok && e.day == dayKey && (e.ok || now.Sub(e.at) < prevCloseRetry) {
		g.mu.Unlock()
		return e.close, e.ok
	}
	g.mu.Unlock()

	e := prevCloseEntry{day: dayKey, at: now}
	want, found := g.hours.PrevTradingDay(g.hours.DayStart(now), 10)
	if found && g.candles != nil {
		if cs, err := g.candles.List(ctx, symbol, port.PeriodDaily, 5); err == nil {
			today := g.hours.DayStart(now)
			for i := len(cs) - 1; i >= 0; i-- {
				d := g.hours.DayStart(cs[i].OpenTime)
				if !d.Before(today) {
					continue // 今日の形成中のバーは使わない
				}
				if d.Equal(want) && cs[i].Close > 0 {
					e.close, e.ok = cs[i].Close, true
				}
				break
			}
		} else {
			// 読めなかった回はキャッシュしない(次のティックで読み直す)
			return 0, false
		}
	}
	g.mu.Lock()
	g.prev[symbol] = e
	g.mu.Unlock()
	return e.close, e.ok
}
