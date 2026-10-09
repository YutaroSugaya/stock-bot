package command

import (
	"context"
	"errors"
	"fmt"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
)

// Errors returned by CloseOne (the handler maps them to 404 / 409).
var (
	ErrPositionNotFound = errors.New("no position with that id")
	ErrPositionNotOpen  = errors.New("position is not OPEN")
	// 他人/手動の建玉。bot は表示するだけで触らない。
	ErrPositionExternal = errors.New("external position is display-only")
)

// QuoteLookup returns a symbol's most recent TRADEABLE and INDICATIVE summaries
// (either may be nil). Injected as a closure because the usecase layer cannot
// import app.
type QuoteLookup func(symbol string) (tradeable, indicative *market.MarketSummary)

// CloseAllOpen は運用ツール(サイクル境界の帳簿締め・異常時の退避)であって戦略の
// 出口ではない。ForceFlatten との違い:
//   - 対象は StatusOpen かつ Source != external の**全**建玉(HoldingMode 不問)。
//   - close_reason は "manual" — 自然な出口 = 戦略の成績、manual = 裁量の打ち切りで、
//     台帳はこの列で censoring を機械的に区別する。
//   - close が拒否されても emergency を trip しない(帳簿締めで bot を止めない)。
//     拒否された建玉は CLOSING のまま残り reconcile が回収する。
//
// 価格は tradeable が第一で、無ければ indicative(前日終値)に落として**どちらを
// 使ったかログに残す**。indicative での評価は普段は禁止だが、戦略評価ではなく
// 帳簿締めなのでここだけ例外。どちらも無ければ closeOne の GetTicker に任せる。
type CloseAllOpen struct {
	exec   closeExecutor
	repo   port.PositionRepository
	quotes QuoteLookup
	clock  clock.Clock
	log    func(msg string, args ...any)
}

// NewCloseAllOpen: quotes may be nil, then every close falls back to a fresh
// broker quote.
//
// unprotected は **裸の建玉だけ**を報せる警報(nil = 報せない)。
//
// 🛑 **必須引数にしてある**(WithXxx の任意ビルダーにしない)。この関数が
// `emergency: nil` をリテラルで固定していたせいで、763782c が足した
// close_unfilled_unprotected trip が **本番の配線では一度も発火しない**まま
// 緑になっていた。既定 nil を作らず、新しいトラックが
// 増えたらコンパイラが「どちらか」を必ず選ばせる。
func NewCloseAllOpen(b port.Broker, pr port.PositionRepository, closer port.PositionCloser,
	quotes QuoteLookup, c clock.Clock, unprotected EmergencyController) *CloseAllOpen {
	if c == nil {
		c = clock.System()
	}
	return &CloseAllOpen{
		// 🛑 emergency は意図的に nil のまま: 帳簿締めの close **拒否**で bot を
		// 止めない。渡すのは unprotected だけ = 裸の枝にしか効かない。
		exec:   closeExecutor{broker: b, posRepo: pr, closer: closer, emergency: nil, unprotected: unprotected},
		repo:   pr,
		quotes: quotes,
		clock:  c,
	}
}

// WithCarry: 未設定なら CarryJPY は 0 のまま — 料率を捏造しない。
func (c *CloseAllOpen) WithCarry(cc position.CarryCalc) *CloseAllOpen {
	c.exec.carry = cc
	return c
}

// WithLogger takes slog.Logger.Info as-is — usecase 層は slog を import しない。
func (c *CloseAllOpen) WithLogger(f func(msg string, args ...any)) *CloseAllOpen {
	c.log = f
	return c
}

type CloseAllResult struct {
	Closed          int `json:"closed"`
	Failed          int `json:"failed"` // CLOSING のまま reconcile 待ちになっている数
	SkippedExternal int `json:"skipped_external"`
}

type CloseOneResult struct {
	PositionID int64  `json:"position_id"`
	Symbol     string `json:"symbol"`
	Quantity   int    `json:"quantity"`
	Closed     bool   `json:"closed"`
}

// Execute は失敗しても止まらない — 全件試して結果を数える。
func (c *CloseAllOpen) Execute(ctx context.Context) (CloseAllResult, error) {
	var res CloseAllResult
	now := c.clock()
	positions, err := c.repo.ListOpenAllSymbols(ctx)
	if err != nil {
		return res, err
	}
	for _, p := range positions {
		if p.Status != position.StatusOpen {
			continue // 既に CLOSING(reconcile 待ち)
		}
		if p.Source == position.SourceExternal {
			res.SkippedExternal++
			continue
		}
		price, source := c.priceFor(p.Symbol)
		ok, err := c.exec.closeOne(ctx, p, price, "manual", now)
		if err != nil || !ok {
			res.Failed++
			c.logf("manual close failed (left CLOSING for reconcile)",
				"position_id", p.ID, "symbol", p.Symbol, "price", price, "price_source", source, "err", err)
			continue
		}
		res.Closed++
		c.logf("manual close", "position_id", p.ID, "symbol", p.Symbol, "price", price, "price_source", source)
	}
	c.logf("flatten-all done", "closed", res.Closed, "failed", res.Failed, "skipped_external", res.SkippedExternal)
	return res, nil
}

// CloseOne は emergency 中でも通る: 決済はリスクを減らす方向なので、
// 「手動 override は hard gate をバイパスしない」(entry の規律)とは別の話。
func (c *CloseAllOpen) CloseOne(ctx context.Context, positionID int64) (*CloseOneResult, error) {
	now := c.clock()
	p, err := c.repo.GetByID(ctx, positionID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, ErrPositionNotFound
	}
	if p.Source == position.SourceExternal {
		return nil, ErrPositionExternal
	}
	if p.Status != position.StatusOpen {
		return nil, fmt.Errorf("%w (status=%s)", ErrPositionNotOpen, p.Status)
	}
	price, source := c.priceFor(p.Symbol)
	ok, err := c.exec.closeOne(ctx, *p, price, "manual", now)
	if err != nil {
		c.logf("manual close failed", "position_id", p.ID, "symbol", p.Symbol, "err", err)
		return nil, err
	}
	if !ok {
		// 🛑 文言を「拒否」に寄せない。ここには **拒否**(Accepted=false)と
		// **受理されたが約定を確認できない**(裸かもしれない)の両方が来る。
		// 後者なのに「rejected」と出ると、人間が状況を読み違える。
		c.logf("manual close did not book (left CLOSING for reconcile)",
			"position_id", p.ID, "symbol", p.Symbol, "price", price, "price_source", source)
		return nil, fmt.Errorf("broker rejected the settle order for position %d (left CLOSING for reconcile)", p.ID)
	}
	c.logf("manual close", "position_id", p.ID, "symbol", p.Symbol, "price", price, "price_source", source)
	return &CloseOneResult{PositionID: p.ID, Symbol: p.Symbol, Quantity: p.Quantity, Closed: true}, nil
}

func (c *CloseAllOpen) priceFor(symbol string) (float64, string) {
	if c.quotes == nil {
		return 0, "none"
	}
	tradeable, indicative := c.quotes(symbol)
	if px := observedPrice(tradeable); px > 0 {
		return px, "tradeable"
	}
	if px := observedPrice(indicative); px > 0 {
		return px, "indicative"
	}
	return 0, "none"
}

func (c *CloseAllOpen) logf(msg string, args ...any) {
	if c.log != nil {
		c.log(msg, args...)
	}
}
