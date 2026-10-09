package command

import (
	"context"
	"errors"
	"fmt"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/domain/risk"
	"stockbot/backend/internal/domain/session"
	"stockbot/backend/internal/port"
)

// ArmProtectiveOrder は **守りが 1 本も無い多日建玉に守りを置く**。
//
// 🚨 なぜ必要になったか:
// この bot が broker 側の守りを置く経路は **新規建て時の ExecuteOrder だけ**だった。
// 置き直し(ReplaceProtectiveOrder)は「板に注文が
// 在ること」が前提で、**板が空の状態から復旧する手段が無い**。置き直しの
// 再発注が口座区分の取り違えで拒否され、取消済みの live の建玉が寄り前に裸で残った
// とき、コードにできることが何も無かった。
//
// 🛑 **この経路は守りを置くだけ。**建玉を作らない・増やさない・決済しない。
// リスクは単調に減る方向にしか動かない。だから:
//   - **emergency 中でも撃てる**(emergency は新規建てを止めるもので、守りを
//     置くのを止めるものではない。裸の建玉を放置するほうが危険)
//   - **場中でも撃てる**。置き直しと違い守りが消える窓が開かないので、時間帯で
//     縛る理由が無い。裸に気づいたのが 10:00 なら 10:00 に置くのが正しい
//
// 🛑 **人間が渡すのは値段(TP/SL)だけ。**数量・side・口座区分・建玉 ID は台帳から
// 取る —— 人間に数量を打たせると打ち間違いがそのまま「建玉より多い返済注文」になる。
// 値段だけ人間なのは、板にあった値段を bot が知らないため(人間が手で締めている
// ことがあり、凍結値で置くとその調整を勝手に元の広い幅へ戻す)。
type ArmProtectiveOrder struct {
	positions port.PositionRepository
	broker    protectiveArmer
	hours     session.TradingHours
	clock     clock.Clock
	log       func(msg string, kv ...any)
	// refPrice は値幅制限の基準値段(前日終値)。nil / 0 は「判定できない」。
	// 🚨 これが無いと復旧が落ちる(下の Execute 参照)。
	refPrice func(ctx context.Context, symbol string) float64
}

// protectiveArmer は「今ある守りを確かめて、無ければ置く」のに要る面だけ。
type protectiveArmer interface {
	ListProtectiveOrders(ctx context.Context, symbol string) ([]port.ProtectiveOrderInfo, error)
	PlaceSettleOCO(ctx context.Context, in port.OCOCloseOrderInput) (string, error)
}

// ErrProtectiveAlreadyOnBoard は「守りは既に板にある」。**異常ではない** ——
// 自動復旧(RearmUnguarded)が毎周これを受け取るので、失敗と区別できる形が要る。
var ErrProtectiveAlreadyOnBoard = errors.New("守りは既に板にある")

// ArmProtectiveInput は人間が指定する部分。**値段だけ**。
type ArmProtectiveInput struct {
	Symbol     string
	TakeProfit float64 // 0 = 利確脚なし(SL のみの守り)
	StopLoss   float64 // 必須
}

func NewArmProtectiveOrder(pr port.PositionRepository, brk protectiveArmer,
	hours session.TradingHours, clk clock.Clock) *ArmProtectiveOrder {
	return &ArmProtectiveOrder{positions: pr, broker: brk, hours: hours, clock: clk}
}

func (a *ArmProtectiveOrder) WithLogger(fn func(msg string, kv ...any)) *ArmProtectiveOrder {
	a.log = fn
	return a
}

// WithPriceLimitRef は値幅制限の基準値段(前日終値)の引き口を挿す。
// 🛑 **挿さないと帯の判定を一切しない**(挿し忘れが「判定した気になる」形にならないよう、
// 既定は「判定しない」= 値段をそのまま送る)。production は必ず挿すこと。
func (a *ArmProtectiveOrder) WithPriceLimitRef(fn func(ctx context.Context, symbol string) float64) *ArmProtectiveOrder {
	a.refPrice = fn
	return a
}

// Execute は 1 銘柄ぶん。返り値は (broker の注文 ID, エラー)。
func (a *ArmProtectiveOrder) Execute(ctx context.Context, in ArmProtectiveInput) (string, error) {
	// 🛑 SL の無い「守り」は守りではない。TP だけ置くと下方向は裸のまま。
	if in.StopLoss <= 0 {
		return "", fmt.Errorf("守りの設置 %s: 逆指値(SL)の価格が要る — TP だけでは下方向が裸のまま", in.Symbol)
	}

	pos, err := a.armablePosition(ctx, in.Symbol)
	if err != nil {
		return "", err
	}

	// 🛑 **二重に置かない。**返済注文が建玉数量を超えると broker は拘束数量の超過で
	// 両方弾きうる。「もう1本置いておけば安心」は守りの経路では成立しない。
	orders, err := a.broker.ListProtectiveOrders(ctx, in.Symbol)
	if err != nil {
		return "", fmt.Errorf("守りの設置 %s: 既存注文の照会に失敗 — **置かない**"+
			"(既に守りがあるか分からないまま置くと二重になる): %w", in.Symbol, err)
	}
	for _, o := range orders {
		if o.HasStopLeg && o.Side == pos.Side.Opposite() {
			return "", fmt.Errorf("%w: 守りの設置 %s: 既に逆指値の守りが板にある(注文 %s・%d株・期日 %s)— "+
				"**置かない**(二重の返済注文は拘束数量の超過で両方弾かれうる)。"+
				"期日を延ばしたいなら置き直し(取消 → 再発注)を使うこと",
				ErrProtectiveAlreadyOnBoard, in.Symbol, o.OrderID, o.Quantity, expiryLabel(o.ExpireOn))
		}
		// 🛑 決済中の建玉に **決済注文が残っている**なら、守りではなく決済が進行中。
		// そこへ返済注文を重ねると実質の新規売りになりうる(CLOSING を arm の対象へ
		// 広げた際に残した唯一の禁止条件)。逆指値脚の有無は問わない —— 成行の
		// 返済注文は脚を持たないので、HasStopLeg で絞ると必ず取りこぼす。
		if pos.Status == position.StatusClosing && o.Side == pos.Side.Opposite() {
			return "", fmt.Errorf("守りの設置 %s: 決済中で、板に決済側の注文が残っている(注文 %s・%d株)— "+
				"**置かない**(返済注文を重ねると実質の新規売りになりうる)。決済の決着を待つこと",
				in.Symbol, o.OrderID, o.Quantity)
		}
	}

	// 🛑 期日は営業日で数える。当日限りにすると**翌日から裸に戻る**。カレンダーが
	// 尽きたら日付を捏造せず error —— 捏造した休場日を指定すると注文ごと拒否され、
	// 「置けたつもりで裸」になる。
	expireOn := a.hours.NthTradingDayFrom(a.clock(), settleMaxTradingDays)
	if expireOn.IsZero() {
		return "", fmt.Errorf("🚨 守りの設置 %s: 休場カレンダーが尽きていて注文期日を出せない — "+
			"hard_limits の holidays / calendar_through を更新すること", in.Symbol)
	}

	// 🚨 **値幅制限の帯で脚を検問する**。
	// 立花は**脚が 1 本でも帯の外だと注文ごと拒否**する。この日、前日終値が 2,908 → 2,803 に
	// 下がって帯が 2,408〜3,408 から 2,303〜3,303 へずれ、凍結 TP 3,345 が 42 円はみ出した。
	// その結果 **SL まで一緒に置けず、裸の建玉を直す唯一の経路が 2 回とも落ちた**。
	// CLAUDE.md の「TP が帯の外なら TP 脚だけ落として SL を置く」は ExecuteOrder(新規建て)
	// にしか入っておらず、**復旧経路には入っていなかった**。
	ocoTP := in.TakeProfit
	protectiveRef := 0.0
	if a.refPrice != nil {
		if ref := a.refPrice(ctx, in.Symbol); ref > 0 {
			protectiveRef = ref
			// 🛑 SL が帯の外なら**置きに行かない**。行っても拒否されるので結果は同じ
			// 「裸のまま」だが、broker の文言(`値幅制限内の単価を入力してください`)は
			// **どちらの脚が外なのかも帯の数字も言わない**。人間に数字を返す。
			if inside, ok := risk.PriceInsideLimitBand(in.StopLoss, ref); ok && !inside {
				up, down, _ := risk.LimitBandFor(ref)
				return "", fmt.Errorf("🚨 守りの設置 %s: 逆指値(SL) %g が今日の値幅制限の外 "+
					"(基準値段 %g / 帯 %g〜%g)— **置きに行かない**(立花は帯の外の脚があると "+
					"注文ごと拒否するので、SL も TP も板に乗らず建玉は裸のまま)。"+
					"帯の内側の SL を指定し直すこと",
					in.Symbol, in.StopLoss, ref, down, up)
			}
		}
	}
	// 🛑 TP は**落とすだけ**。SL と違って欠けても致命的でない(TP は OnTick が引き継ぐ)。
	// 多日建玉は帯の内外に依らず **stop-only**(risk.ProtectiveTakeProfitOnBoard)。
	// この経路が置く建玉は多日だけなので、実質つねに TP 0 で置く。
	if place, reason := risk.ProtectiveTakeProfitOnBoard(pos.HoldingMode, pos.Side.Opposite(), ocoTP, protectiveRef); !place && ocoTP > 0 {
		if a.log != nil {
			a.log("守りの TP 脚を板に載せない", "symbol", in.Symbol, "tp", ocoTP, "reason", reason,
				"理由", "多日の守りは stop-only(繰越時に TP が帯の外だと SL ごと失効する)。TP は OnTick が持つ")
		}
		ocoTP = 0
	}

	id, err := a.broker.PlaceSettleOCO(ctx, port.OCOCloseOrderInput{
		Symbol:           in.Symbol,
		BrokerPositionID: pos.BrokerPositionID,
		Side:             pos.Side.Opposite(),
		Quantity:         pos.Quantity,
		TakeProfit:       ocoTP,
		StopLoss:         in.StopLoss,
		// 🚨 台帳の凍結区分。ゼロ値だと adapter が現物 "0" を送り、制度信用の建玉には
		// 「口座区分がお預かり銘柄と不一致」で拒否される。
		ExecKind: pos.ExecKind,
		ExpireOn: expireOn,
	})
	if err != nil {
		return "", fmt.Errorf("🚨 守りの設置 %s に失敗 — **この建玉はまだ裸**: %w", in.Symbol, err)
	}
	if a.log != nil {
		a.log("守りを設置した", "symbol", in.Symbol, "order", id, "qty", pos.Quantity,
			"tp", ocoTP, "sl", in.StopLoss, "exec_kind", string(pos.ExecKind),
			"expire_on", expireOn.Format("2006-01-02"))
	}
	return id, nil
}

// armablePosition は守りを置いてよい建玉を **1 つに確定**する。曖昧なら置かない ——
// 数量や区分を取り違えた返済注文は、建玉を守るどころか「持っていない建玉の返済」
// = 実質の新規売りになりうる。
//
// 🚨 **CLOSING の建玉も対象に含める**。守りを cancel した後に決済が
// 板へ載らないと建玉は CLOSING かつ裸になる。そこを除外していたため、
// **bot が自力で裸にした状態を bot は直せなかった** —— 人間が証券アプリで
// 直すしかなくなる構造的な理由がこれ。板のスイープが
// 「arm で復旧せよ」と案内する状態を arm が拒否する、という自己矛盾でもあった。
//
// 🛑 ただし **板に決済側の注文が残っていれば置かない**。決済中の建玉に返済注文を
// 重ねると実質の新規売りになりうる、という元の懸念はそこでは正しい。
func (a *ArmProtectiveOrder) armablePosition(ctx context.Context, sym string) (position.Position, error) {
	guarded, err := multidayPositions(ctx, a.positions, true)
	if err != nil {
		return position.Position{}, fmt.Errorf("守りの設置 %s: 建玉を読めない: %w", sym, err)
	}
	held := guarded[sym]
	// 🛑 OPEN と CLOSING が混在したら OPEN を優先して 1 本に確定する。
	// 「候補が 2 本あるから置かない」にすると、裸の CLOSING を道連れにして
	// 復旧経路が塞がる(= 広げた意味が無くなる)。
	if len(held) > 1 {
		var open []position.Position
		for _, p := range held {
			if p.Status == position.StatusOpen {
				open = append(open, p)
			}
		}
		if len(open) == 1 {
			held = open
		}
	}
	switch len(held) {
	case 1:
		// 🛑 CLOSING のときに「板に決済注文が残っていないか」を確かめるのは Execute 側。
		// あちらが既に ListProtectiveOrders を呼んでいるので、ここで照会すると
		// 立花の API 予算を 1 回余計に使う(10,000 回/日・現在 約7,700 回)。
		return held[0], nil
	case 0:
		// multidayPositions は external / intraday を落としている。
		// どれで落ちたかは呼び手に区別できないが、**置かない**判断は同じ。
		return position.Position{}, fmt.Errorf("守りの設置 %s: 守りを置ける多日建玉が台帳に無い — "+
			"**置かない**(bot の建玉でない / intraday のいずれか。external の建玉に"+
			"返済注文を出すと実質の新規売りになりうる)", sym)
	default:
		return position.Position{}, fmt.Errorf("守りの設置 %s: 多日建玉が %d 本あってどれの守りか確定できない — "+
			"**置かない**(数量を取り違えた返済注文は建玉を守らない)", sym, len(held))
	}
}
