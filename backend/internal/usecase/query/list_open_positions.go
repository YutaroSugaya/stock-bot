// Package query holds the read-only usecases (CQRS read side). They return View
// DTOs and never expose domain entities or mutate state.
package query

import (
	"context"
	"time"

	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
)

type OpenPositionView struct {
	ID          int64   `json:"id"`
	Symbol      string  `json:"symbol"`
	Side        string  `json:"side"`
	Quantity    int     `json:"quantity"`
	EntryPrice  float64 `json:"entry_price"`
	HoldingMode string  `json:"holding_mode"`
	Source      string  `json:"source"`
	Status      string  `json:"status"`
	ConfigID    string  `json:"config_id"`
	// config_id(`20260728-084554-4063`)は生成時刻と銘柄でしかなく「どの戦略で
	// 建てたか」が読めないので画面にはこちらを出す。空 = 解決不能(resolver 無し)。
	Strategy string `json:"strategy,omitempty"`

	// 建玉時に凍結した出口幾何。幅は円/株、Price は確定した絶対価格(broker の
	// OCO に入っているのと同じ値段)。
	TakeProfitJPY   float64 `json:"take_profit_jpy"`
	StopLossJPY     float64 `json:"stop_loss_jpy"`
	TakeProfitPrice float64 `json:"take_profit_price"`
	StopLossPrice   float64 `json:"stop_loss_price"`
	TickSizeAtEntry float64 `json:"tick_size_at_entry"`

	// トレールアーム(bnf_reversion_trail 等)は TP を持たず出口は ratchet なので、
	// これを出さないと TP=0 の建玉が画面上「出口なし」に見える。幅は **円/株**
	// (EvaluateExit が UnrealizedJPY = 円/株 と比べる単位)。peak は建値からの
	// 最大含み、armed は arm 到達済みか。
	RatchetArmJPY      float64 `json:"ratchet_arm_jpy"`
	RatchetGivebackJPY float64 `json:"ratchet_giveback_jpy"`
	PeakUnrealizedJPY  float64 `json:"peak_unrealized_jpy"`
	RatchetArmed       bool    `json:"ratchet_armed"`

	// 損失側の守りが**実際に効く**値段と理由。作動済みトレールは giveback 線と SL
	// の2本を持ち、遠い方には価格が先に届かない。画面がこの判定を JS で書き直すと
	// engine から静かにずれるので、domain.ProtectiveExit の答えを渡す。
	ProtectiveExitPrice  float64 `json:"protective_exit_price"`
	ProtectiveExitReason string  `json:"protective_exit_reason"`

	// 🛑 **トレールの床が効く建玉か**(migration 0013・新規建玉のみ true)。
	// 画面はこれを見ないと **旧規則の建玉と床つきの建玉を区別できない** —
	// giveback 線を旧式 `peak − giveback` で描くか、床 `max(arm, peak − giveback)` で
	// 描くかが変わる。
	RatchetFloorAtArm bool `json:"ratchet_floor_at_arm"`

	// 建玉時刻(JST)。**旧台帳を閉じる判断材料は「残玉の本数と経過営業日」**
	// なので、画面がそれを出すにはこれが要る。
	OpenedAt time.Time `json:"opened_at"`

	// 時間切れ(max_hold)の期限。凍結値 `MaxHoldMinutes` から domain(`MaxHoldUntil`)が
	// 決めた時刻をそのまま渡す — 画面が opened_at + 分 を計算し直すと engine からずれる。
	// nil = 無期限。HardUntil は延長(建値近辺なら持ち越す)の上限で、延長なしなら nil。
	// 期限が場外に来た建玉は、次の立会の最初の値で成行決済される。
	MaxHoldMinutes   int        `json:"max_hold_minutes"`
	MaxHoldUntil     *time.Time `json:"max_hold_until"`
	MaxHoldHardUntil *time.Time `json:"max_hold_hard_until"`

	// 株式分割(併合)で言い直した建玉の累積の株数倍率と、最後に言い直した権利落ち日
	// (migration 0022)。1 / "" = 言い直していない。株数が 100 の倍数でない・建値が
	// 格子外になっている建玉の理由がここで分かる。
	SplitFactor     float64 `json:"split_factor"`
	SplitAdjustedOn string  `json:"split_adjusted_on,omitempty"`
}

type ListOpenPositions struct {
	posRepo port.PositionRepository
}

func NewListOpenPositions(pr port.PositionRepository) *ListOpenPositions {
	return &ListOpenPositions{posRepo: pr}
}

func (q *ListOpenPositions) Execute(ctx context.Context, symbol string) ([]OpenPositionView, error) {
	positions, err := q.posRepo.ListOpenOrClosing(ctx, symbol)
	if err != nil {
		return nil, err
	}
	out := make([]OpenPositionView, 0, len(positions))
	for _, p := range positions {
		out = append(out, toView(p))
	}
	return out, nil
}

func toView(p position.Position) OpenPositionView {
	protPx, protReason := position.ProtectiveExit(p)
	var until, hard *time.Time
	if soft := p.MaxHoldUntil(); !soft.IsZero() {
		until = &soft
		if p.ExtensionMaxMinutes > 0 {
			h := soft.Add(time.Duration(p.ExtensionMaxMinutes) * time.Minute)
			hard = &h
		}
	}
	return OpenPositionView{
		ID: p.ID, Symbol: p.Symbol, Side: string(p.Side), Quantity: p.Quantity,
		EntryPrice: p.EntryPrice, HoldingMode: string(p.HoldingMode),
		Source: string(p.Source), Status: string(p.Status), ConfigID: p.StrategyConfigID,
		TakeProfitJPY: p.TakeProfitJPY, StopLossJPY: p.StopLossJPY,
		TakeProfitPrice: p.TakeProfitPrice, StopLossPrice: p.StopLossPrice, TickSizeAtEntry: p.TickSizeAtEntry,
		RatchetArmJPY: p.RatchetArmJPY, RatchetGivebackJPY: p.RatchetGivebackJPY,
		PeakUnrealizedJPY: p.PeakUnrealizedJPY, RatchetArmed: p.RatchetArmed,
		ProtectiveExitPrice: protPx, ProtectiveExitReason: protReason,
		RatchetFloorAtArm: p.RatchetFloorAtArm, OpenedAt: p.OpenedAt,
		MaxHoldMinutes: p.MaxHoldMinutes, MaxHoldUntil: until, MaxHoldHardUntil: hard,
		SplitFactor: splitFactorOf(p), SplitAdjustedOn: splitDayOf(p),
	}
}

func splitFactorOf(p position.Position) float64 {
	if p.SplitFactor <= 0 {
		return 1
	}
	return p.SplitFactor
}

func splitDayOf(p position.Position) string {
	if p.SplitAdjustedOn.IsZero() {
		return ""
	}
	return p.SplitAdjustedOn.Format(time.DateOnly)
}
