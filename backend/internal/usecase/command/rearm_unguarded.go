package command

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
)

// RearmUnguarded は **板から消えた守りを台帳の凍結値で置き直す**。
//
// 🚨 なぜ必要になったか:
// 多日保有の守りは、置いた翌日以降に **broker 側で勝手に消える**。例 ——
// 前日終値が 2,908 → 2,803 に下がって値幅制限の帯がずれ、凍結 TP 3,345 が帯の外に出た
// 結果、立花が**繰越で注文を無効化**する(該当注文の状態は「無効」/「繰越失効」)。
// **帯の内側だった SL も道連れ**で、live の建玉が丸 1 日ぶん裸になった。
//
// 🛑 **既存のどの経路もこれを直せなかった。**
//   - `ReplaceProtectiveOrder`(寄り前の置き直し)は **板に注文が在ること**が前提。
//     消えた守りは対象にすらならない(期日を見に行く相手がいない)
//   - `ArmProtectiveOrder` は **人間が叩く手動経路**
//
// つまり毎朝の裸検知は「見つける」だけで、**直すのは人間の手作業**だった。
// 寄りで消えて人間が翌朝に直すまで、丸 1 日ぶん裸になる。
//
// 🛑 **この経路は守りを置くだけ。** 建玉を作らない・増やさない・決済しない。
// リスクは単調に減る方向にしか動かない —— だから `ArmProtectiveOrder` と同じく
// **emergency 中でも場中でも走ってよい**(置き直しと違い、守りが消える窓が開かない)。
//
// 🛑 **値段は台帳の凍結値から取る。** 人間の入力を待たない —— 待っている間ずっと裸。
// 帯の検問(TP が外なら落とす / SL が外なら置かない)は `ArmProtectiveOrder` が持つ。
type RearmUnguarded struct {
	positions port.PositionRepository
	broker    rearmBroker
	arm       *ArmProtectiveOrder
	log       func(msg string, kv ...any)
}

// rearmBroker は守りを置く面 + **建玉照会**。
//
// 🛑 建玉照会を型に含めるのは、**「守りが無い」と「建玉が無い」を取り違えない**ため。
// 板の逆指値が約定して建玉が消えた後、この経路が 15 分ごとに
// 決済済みの建玉へ守りを置こうとして拒否され、ログに「この建玉はまだ裸」が 6 回
// 並んだ。裸の警報は本物のときだけ鳴らないと、鳴っても誰も見なくなる。
// optional interface(型アサーション)にしないのは、配線を忘れても緑のまま
// 昔の挙動に落ちるから。
type rearmBroker interface {
	protectiveArmer
	brokerPositionLister
}

// RearmResult は 1 周ぶん。
//
// 🛑 **Armed と Skipped を割る。**「守りが板にあって何もしなかった」(正常運転)と
// 「置きに行って置けた」(直前まで裸だった = 異常)は運用上まったく別の状態で、
// 前者を後者と読むと毎周が異常に見え、後者を前者と読むと**裸だった事実が消える**。
type RearmResult struct {
	Armed   int // 板が空だったので置き直した本数
	Skipped int // 既に守りがあって触らなかった本数
	// Settled は **broker がその建玉をもう持っていない**本数。守りが無いのは
	// 当然で、裸ではない(板の守りが約定した / 人間が締めた)。台帳を締めるのは
	// reconcile の仕事なので、ここでは触らずに数えるだけ。
	Settled int
}

func NewRearmUnguarded(pr port.PositionRepository, brk rearmBroker, arm *ArmProtectiveOrder) *RearmUnguarded {
	return &RearmUnguarded{positions: pr, broker: brk, arm: arm}
}

func (r *RearmUnguarded) WithLogger(fn func(msg string, kv ...any)) *RearmUnguarded {
	r.log = fn
	r.arm = r.arm.WithLogger(fn)
	return r
}

// Execute は多日建玉を 1 銘柄ずつ見て、守りが板に無ければ凍結値で置き直す。
//
// 🛑 **1 銘柄の失敗で他を止めない。** 1 本置けなかったからといって、他の裸を
// 放置する理由にはならない。errs に積んで最後まで回る。
func (r *RearmUnguarded) Execute(ctx context.Context) (RearmResult, []error) {
	var res RearmResult
	// CLOSING も含める —— 守りを cancel した後に決済が板へ載らないと、建玉は
	// CLOSING かつ裸になる。ArmProtectiveOrder 側が「決済注文が残っているなら置かない」
	// を見るので、ここで削ると復旧経路が塞がるだけ。
	guarded, err := multidayPositions(ctx, r.positions, true)
	if err != nil {
		return res, []error{fmt.Errorf("守りの置き直し(復旧): 建玉を読めない: %w", err)}
	}
	syms := make([]string, 0, len(guarded))
	for sym := range guarded {
		syms = append(syms, sym)
	}
	sort.Strings(syms) // 決定論。ログの並びが実行ごとに変わると差分が読めない

	// 🛑 **建玉照会は 1 周に 1 回**(銘柄ごとに払わない。口座単位の量なので銘柄数で
	// 割り増しても情報は増えない)。落ちた回は nil = 「確かめられなかった」で、
	// 従来どおり置きに行く —— この経路はリスクを単調に減らすので、確かめられない
	// ときに黙るほうが危ない。
	atBroker := r.symbolsHeldAtBroker(ctx)

	var errs []error
	for _, sym := range syms {
		held := guarded[sym]
		if len(held) == 0 {
			continue
		}
		// 🚨 **broker に建玉が無いなら、守りが無いのは当然で裸ではない。**
		// 守るものが無い建玉に守りは置けないし、置こうとすれば拒否される
		// (15 分ごとにそれを繰り返す)。台帳を
		// 締めるのは reconcile の仕事なので、ここでは触らない。
		if atBroker != nil && len(atBroker[sym]) == 0 {
			res.Settled++
			if r.log != nil {
				r.log("broker にこの建玉はもう無い — 守りの置き直しを飛ばす(裸ではない)",
					"symbol", sym, "次", "reconcile が実約定で台帳を締める")
			}
			continue
		}
		// 🚨 **broker の株数が台帳と違う建玉に、凍結値で守りを置かない**。
		// 株式分割の権利落ちで broker だけが建玉を言い直していると、分割前の SL・株数の
		// 逆指値は時価の数倍 = 寄りで即発動し、一部だけ売れて残りが裸になる。
		// 株数と建単価の**両方**が同じ分割比(株数 ×r・建単価 ÷r)を示すときだけ台帳を言い直してから置く。
		restated, err := r.restateForBrokerSplit(ctx, sym, held, atBroker[sym])
		if err != nil {
			errs = append(errs, err)
			continue
		}
		held = restated
		// 🛑 **凍結値が無い建玉は触らない。** 0 を送ると「SL の無い守り」= 守りではない。
		// ArmProtectiveOrder も SL<=0 を弾くが、ここで先に落として理由を明示する。
		tp, sl := frozenLegs(held)
		if sl <= 0 {
			errs = append(errs, fmt.Errorf("🚨 守りの置き直し(復旧) %s: 台帳に凍結 SL が無い(%v)— "+
				"**この建玉は裸のまま**。人間が値段を決めて arm すること", sym, sl))
			continue
		}

		id, err := r.arm.Execute(ctx, ArmProtectiveInput{Symbol: sym, TakeProfit: tp, StopLoss: sl})
		switch {
		case err == nil:
			res.Armed++
			if r.log != nil {
				r.log("🚨 板から消えていた守りを置き直した(自動復旧)", "symbol", sym, "order", id,
					"tp", tp, "sl", sl,
					"理由", "多日の守りは翌日以降 broker 側で失効しうる")
			}
		case errors.Is(err, ErrProtectiveAlreadyOnBoard):
			// 正常運転。守りは板にある。
			res.Skipped++
		default:
			// 🚨 **飲み込まない。**ここで黙ると「直したつもりで裸」になり、
			// 毎周の ERROR だけが延々と出続ける。
			errs = append(errs, err)
		}
	}
	return res, errs
}

// symbolsHeldAtBroker は broker がいま持っている建玉を銘柄ごとに返す。
// **照会できなかったら nil**(空集合ではない)—— 空集合と区別できないと、障害の
// 1 回で全建玉を「決済済み」と読んで守りの復旧を丸ごと止める。
func (r *RearmUnguarded) symbolsHeldAtBroker(ctx context.Context) map[string][]port.BrokerPosition {
	if r.broker == nil {
		return nil
	}
	held, err := r.broker.GetPositions(ctx)
	if err != nil {
		return nil
	}
	out := make(map[string][]port.BrokerPosition, len(held))
	for _, bp := range held {
		out[bp.Symbol] = append(out[bp.Symbol], bp)
	}
	return out
}

// splitEntryTolerance は「broker の建単価が台帳の建値 ÷ 分割比に一致する」の許容(相対)。
// broker は建単価を丸めて持つので完全一致は要求しないが、部分返済(建単価は不変)とは
// 桁で離れる。
const splitEntryTolerance = 0.02

// restateForBrokerSplit は、broker が同じ建玉 id を**台帳と違う株数**で持っているとき、
// それが株式分割(併合)なら台帳を言い直して返す。分割と断定できなければ error(置かない)。
// broker 側に同じ id が無い・株数が同じなら何もしない(従来どおり)。
func (r *RearmUnguarded) restateForBrokerSplit(ctx context.Context, sym string, held []position.Position,
	atBroker []port.BrokerPosition) ([]position.Position, error) {
	out := append([]position.Position(nil), held...)
	for i, p := range out {
		if p.BrokerPositionID == "" || p.Status != position.StatusOpen {
			continue
		}
		var bp *port.BrokerPosition
		for j := range atBroker {
			if atBroker[j].BrokerPositionID == p.BrokerPositionID {
				bp = &atBroker[j]
				break
			}
		}
		if bp == nil {
			continue
		}
		if bp.Quantity == p.Quantity {
			// 🚨 整数倍以外の株式分割: 立花は株数を増やさず建単価だけ下げる。分割前の凍結 SL は
			// 権利落ち後の時価より上に来うる = 置いた瞬間に発動する。比を確定できないので置かない。
			if brokerRightsProcessed(p, *bp) {
				return nil, fmt.Errorf("🚨 守りの置き直し(復旧) %s: broker の建単価が %g → %g に変わり株数は %d のまま"+
					"(整数倍以外の株式分割の権利処理とみられる)— 分割前の凍結値では**置かない**。"+
					"broker の画面で確かめ、人間が守りを置くこと", sym, p.EntryPrice, bp.EntryPrice, p.Quantity)
			}
			continue
		}
		ratio, isSplit := brokerSplitFactor(p, *bp)
		if !isSplit {
			return nil, fmt.Errorf("🚨 守りの置き直し(復旧) %s: broker の株数 %d(建単価 %g)が台帳の %d(建値 %g)と"+
				"一致せず、株式分割とも断定できない — **置かない**(凍結値で置くと数量と値段がずれた返済注文になる)。"+
				"broker の画面で建玉を確かめ、人間が守りを置くこと",
				sym, bp.Quantity, bp.EntryPrice, p.Quantity, p.EntryPrice)
		}
		day := r.arm.hours.DayStart(r.arm.clock())
		adj, err := position.SplitAdjusted(p, ratio, day)
		if err != nil {
			return nil, fmt.Errorf("🚨 守りの置き直し(復旧) %s: 株式分割の言い直しに失敗 — 置かない: %w", sym, err)
		}
		ok, err := r.positions.ApplySplit(ctx, adj)
		if err != nil {
			return nil, fmt.Errorf("🚨 守りの置き直し(復旧) %s: 株式分割の言い直しを台帳に書けない — 置かない: %w", sym, err)
		}
		if !ok {
			return nil, fmt.Errorf("🚨 守りの置き直し(復旧) %s: 台帳の言い直しが競合した — 次の周で読み直す", sym)
		}
		out[i] = adj
		if r.log != nil {
			r.log("⚠ broker の建玉が株式分割で言い直されていた — 台帳を揃えてから守りを置く",
				"symbol", sym, "position_id", p.ID, "株数倍率", ratio,
				"株数", fmt.Sprintf("%d → %d", p.Quantity, adj.Quantity),
				"SL", fmt.Sprintf("%g → %g", p.StopLossPrice, adj.StopLossPrice))
		}
	}
	return out, nil
}

// frozenLegs は台帳の凍結 TP/SL を 1 組に確定する。
//
// 🛑 **ArmProtectiveOrder.armablePosition と同じ絞り方をする。**あちらは OPEN を優先して
// 1 本に確定し、確定できなければ error にする。ここで別の建玉の値段を拾うと
// 「A の数量に B の値段」で置くことになる —— 数量と値段がずれた返済注文は、
// 守るどころか台帳と board の対応を壊す。
func frozenLegs(held []position.Position) (tp, sl float64) {
	if len(held) == 0 {
		return 0, 0
	}
	pick := held
	if len(pick) > 1 {
		var open []position.Position
		for _, p := range pick {
			if p.Status == position.StatusOpen {
				open = append(open, p)
			}
		}
		if len(open) == 1 {
			pick = open
		}
	}
	if len(pick) != 1 {
		// 曖昧。arm 側も同じ条件で error にするので、0 を返して呼び手に落とさせる。
		return 0, 0
	}
	return pick[0].TakeProfitPrice, pick[0].StopLossPrice
}
