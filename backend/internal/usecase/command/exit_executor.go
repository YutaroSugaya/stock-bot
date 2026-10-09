package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
	"stockbot/backend/internal/port"
)

// EmergencyController is a local interface so the usecase layer never imports
// the safety package. *safety.EmergencyStop satisfies it.
type EmergencyController interface {
	Active() bool
	Trip(reason string, now time.Time) error
}

// closeExecutor is the single place that flips a position out of OPEN, honouring
// the ClaimForClose CAS so a position is never double-closed.
type closeExecutor struct {
	broker  port.Broker
	posRepo port.PositionRepository
	closer  port.PositionCloser
	// emergency は **close 失敗のすべて**(拒否 / 裸)で trip する警報。自動の出口
	// (ForceFlatten / ManageOpenPositions)はこれを 1 つ持つだけで両方の枝に効く。
	emergency EmergencyController
	// unprotected は **「裸の建玉」だけ**に効く警報。決済が受理されたのに約定も
	// 板への常駐もしない = 逆指値も決済注文も無い、という枝でしか trip しない。
	//
	// 🛑 emergency と分けてあるのは帳簿締め(CloseAllOpen)のため。あちらの
	// 「close 拒否で bot を止めない」は **拒否**についての事前コミットで、**裸**は
	// 別問題。emergency を丸ごと渡すと拒否枝まで trip し、1 件の拒否で live の
	// 新規建てが人間の resume 待ちになる。
	unprotected EmergencyController
	// carry prices the信用建玉の資金コスト(買方金利 / 貸株料)。ゼロ値なら carry は
	// 0 — 料率が config に入っていない構成でレートを捏造しないため。
	carry position.CarryCalc
}

// closeOne claims and closes one position, returning true when it actually
// closed. It CANCELS the resident broker-side protective legs (逆指値/OCO) BEFORE
// the settle order: a working protective order 拘束 the 建玉 quantity, so a second
// settle order for the same 建玉 is rejected while it rests — dropping this step
// bricked every bot-initiated exit (TP / max-hold / ratchet / 14:50 引け前
// フラット化).
//
// A trade is booked ONLY against a confirmed fill (resolveSettleFill); an
// accepted-but-unfilled settle leaves the position CLOSING for reconcile.
func (x closeExecutor) closeOne(ctx context.Context, p position.Position, observedPrice float64, reason string, now time.Time) (bool, error) {
	// 🛑 **板の照会は claim より前**。ここで失敗したら claim せずに返す —— 建玉は
	// OPEN のままなので次ティックが素直に再試行する。claim してから諦めると建玉は
	// CLOSING に落ち、そこから先は誰も拾わない: ForceFlatten と ManageOpenPositions は
	// StatusOpen 以外を skip し、reconcile の resolveStuckClosing は板に残った守りを
	// 「決済注文が働いている」と読んで再送も escalate もしない。一過性の照会エラーで
	// 14:50 の強制フラット化が丸ごと止まり、建玉が裸で置き去りになる。
	legs, err := x.protectiveLegsToCancel(ctx, p)
	if err != nil {
		return false, err // 何も claim していない = 次ティックで再試行できる
	}
	ok, err := x.posRepo.ClaimForClose(ctx, p.ID, now)
	if err != nil || !ok {
		return false, err // benign skip: already CLOSING/CLOSED
	}
	// 🛑 取消の結果を**捨てない**。ただしここでは trip しない —— 取消が通らなければ
	// 守りは板に残っており、続く ClosePosition が拘束で拒否されて下の枝が 1 回だけ
	// 鳴る。ここでも鳴らすと同じ 1 事象で 2 回鳴り、emergency は先勝ちなので
	// **後から来る本当の理由が消える**。
	cancelErr := x.cancelLegs(ctx, legs)
	res, err := x.broker.ClosePosition(ctx, port.CloseRequest{
		Symbol: p.Symbol, BrokerPositionID: p.BrokerPositionID, Side: p.Side.Opposite(),
		Quantity: p.Quantity, ExecKind: p.ExecKind,
	})
	if err != nil || res == nil || !res.Accepted {
		// 🚨 **拒否には 3 つ目の状態がある: broker にその建玉がもう無い。**
		// 板の逆指値が先に約定して建玉が消えていると、返済は
		// 「信用建玉明細にデータがありません」で拒否される。それを
		// **決済済みの建玉を「本当に裸」と読んで** live を緊急停止してはいけない。
		// **裸と決済済みは正反対**で、守るものが無い建玉に守りは置けない。
		//
		// 🛑 判定は **建玉照会(唯一の権威)**で行い、拒否の文面では行わない ——
		// 同じ文面は建玉の指定違いでも出るので、生きている建玉を決済済みと誤読すると
		// 守りを置き直す機会ごと失う。照会が落ちた回は確かめられていないので
		// 従来どおり trip する(fail-close)。
		if gone, checked := positionGoneAtBroker(ctx, x.broker, p); checked && gone {
			// 台帳は **実約定でしか締めない**(幽霊決済の禁止)。締められなければ
			// CLOSING のまま reconcile が引き継ぐ。どちらの枝でも trip しない ——
			// 建玉が無い以上、守りの有無を人間に問う意味が無い。
			if price, fee, reason, ok := boardSettleFill(ctx, x.broker, p); ok {
				return x.bookSettled(ctx, p, price, fee, reason, now)
			}
			return false, fmt.Errorf("返済 %s: broker にこの建玉はもう無い(板の守りが約定した公算)— "+
				"実約定を確認できないので CLOSING のまま reconcile に渡す", p.Symbol)
		}
		// 🚨 **理由を取消の成否で分ける。運用の次の一手が正反対になるため。**
		//   取消できた   → 守りは落ちている = 本当に裸。人間は守りを置き直す。
		//   取消できない → 守りが板に残っている公算が高い。そこで置き直すと守りが
		//                  二重になり、返済可能数量を超えて**両方**弾かれうる。
		//                  人間がまずやるべきは板を見ること。
		if x.emergency != nil {
			_ = x.emergency.Trip(closeRejectReason(cancelErr)+":"+p.BrokerPositionID, now)
		}
		// 🛑 文言に「left CLOSING for reconcile」を残す — 呼び手(CloseAllOpen.CloseOne)は
		// これを HTTP 500 の本文に出す。ここを落とすと運用者が「建玉が CLOSING で
		// 残っている」を読めなくなる。
		if cancelErr != nil {
			return false, fmt.Errorf("守りの取消が確認できないまま返済も通らなかった %s (left CLOSING for reconcile): %w",
				p.Symbol, errors.Join(cancelErr, err))
		}
		return false, err // leave CLOSING; reconcile / forced-flatten handles it
	}
	// 🛑 返済が**通った**枝では cancelErr を握り潰す。ここで error を返すと
	// CloseAllOpen.CloseOne が「閉じたのに失敗」として 500 を返し、建玉は閉じている
	// のに人間が失敗と読む。取消の失敗は返済が通った時点で実害が無い(拘束が
	// 残っていれば通らない)。
	// 🚨 An ACCEPTED settle order is not a fill. Live brokers (立花) answer
	// ClosePosition with no price because the fill must be polled, and filling
	// that gap with the observed 時価 books a GHOST CLOSE: the ledger says flat
	// while the 建玉 is still at the broker, where reconcile re-adopts it as
	// external = permanently unmanaged (典型は寄らずのストップ安).
	// Resolve it the way the entry saga does; book nothing unless it filled.
	closePrice, settleFee, confirmed := resolveSettleFill(ctx, x.broker, res, p.Quantity)
	if !confirmed {
		// 🚨 受理は約定ではない — が、**受理は「板に載っている」ことでもない**。
		// 守りの脚は既に cancel してあるので、決済注文が板に無ければこの建玉は
		// 逆指値も決済注文も持たない = 完全に裸。人間が証券アプリで TP を置き直すまで
		// 守りがゼロになる(live の reconcile は保有中でも 1 時間おきなので、
		// bot 側にこの窓を縮める手段が無い)。
		//
		// 板に残っているなら trip しない: 未約定の決済を残すのは意図的な設計で
		// (ストップ安の引けは比例配分 = 板の売り注文にしか配分されない)、
		// そこで trip すると正常な待ちのたびに実弾が緊急停止する。
		tripIfSettleLeavesPositionNaked(ctx, x.broker, x.unprotectedAlarm(),
			p.Symbol, p.Side.Opposite(), p.BrokerPositionID, res, now)
		return false, nil // leave CLOSING — reconcile owns it from here
	}
	if closePrice <= 0 {
		closePrice = observedPrice
	}
	if closePrice <= 0 { // last resort: pull a fresh quote
		if t, terr := x.broker.GetTicker(ctx, p.Symbol); terr == nil && t != nil {
			closePrice = t.Mid()
		}
	}
	if closePrice <= 0 {
		// Cannot value the close honestly — never book a 0-price trade. Leave the
		// position CLOSING so reconcile resolves it.
		return false, nil
	}
	return x.bookSettled(ctx, p, closePrice, settleFee, reason, now)
}

// bookSettled は **約定が確認できた決済**を台帳に書く唯一の口。
//
// 🛑 自分が出した返済(closeOne の本流)と、broker 側の守りが約定して建玉が消えて
// いた枝の**両方**がここを通る。台帳の規約(net = gross − fee + carry・トレールの
// ラベル分け)を 2 箇所に書くと、片方だけ直して緑のまま台帳がずれる。
func (x closeExecutor) bookSettled(ctx context.Context, p position.Position,
	closePrice, settleFee float64, reason string, now time.Time) (bool, error) {
	gross := grossPnL(p, closePrice)
	// 台帳の規約は net = gross − fee + carry。fee は建玉時に凍結した entry 手数料 +
	// この決済脚(両方 0 のままだと daily loss cap の "net" が黙って gross に劣化する)。
	// carry は信用建玉の資金コスト(立花 e支店 一般信用: 買方金利 年2.50% / 貸株料
	// 年1.15%、約定金額 × 受渡日の両端入れ日数)で負値。料率が config に無ければ 0 —
	// レートを捏造すると台帳の net が嘘になる。
	fee := p.EntryFeeJPY + settleFee
	carry := x.carry.JPY(p, now)
	// トレール決済のラベルは **realized gross の符号**で分ける。
	// `ratchet_takeprofit` が損失で出ていたので、段2 も台帳も「利確が発火した」と読んで
	// いた。判定時の時価ではなく約定値から分類する — 新規則で giveback_loss が出るのは
	// ギャップで床を飛ばした場合だけで、その差がそのままラベルの差になる。
	_, err := x.closer.CloseAndRecord(ctx, p.ID, now, port.TradeRecord{
		PositionID: p.ID, Symbol: p.Symbol, Side: p.Side, Quantity: p.Quantity,
		EntryPrice: p.EntryPrice, ClosePrice: closePrice, ProfitLossJPY: gross,
		FeeJPY: fee, CarryJPY: carry,
		CloseReason: port.RatchetCloseReason(reason, gross), ClosedAt: now,
	})
	return err == nil, err
}

// tripIfSettleLeavesPositionNaked は「決済を出したのに約定も板への常駐も確認できない」
// ときに、**建玉が本当に裸か**を確かめてから警報を鳴らす。
//
// 🚨 受理は約定ではない — が、**受理は「板に載っている」ことでもない**。守りの脚は
// 既に cancel してあるので、決済注文が板に無ければこの建玉は逆指値も決済注文も
// 持たない = 完全に裸。人間が証券アプリで TP を置き直すまで守りがゼロになる
// (live の reconcile は保有中でも 1 時間おきなので、bot 側にこの窓を縮める手段が無い)。
//
// 🛑 **決済経路(closeOne)と補償経路(bookCompensatedRoundTrip)で共有する。**
// 同じロジックを 2 箇所に書くと、片方だけ直して緑のまま穴が残る。
//
// 板に残っているなら鳴らさない: 未約定の決済を残すのは意図的な設計で(ストップ安の
// 引けは比例配分 = 板の売り注文にしか配分されない)、そこで鳴らすと正常な待ちの
// たびに実弾の新規建てが止まる。
func tripIfSettleLeavesPositionNaked(ctx context.Context, brk port.Broker, alarm EmergencyController,
	symbol string, closeSide order.Side, brokerPositionID string, res *port.CloseResult, now time.Time) {
	// 🛑 nil 判定を **先に** 置く。鳴らせない構成(research / harvest)で板照会を
	// 1 本余計に投げない — 立花の API 予算は 10,000 回/日 で現在 約7,700 回使用中。
	if alarm == nil {
		return
	}
	if hasWorkingCloseOrderOn(ctx, brk, symbol, closeSide) {
		return // 決済注文が板に残っている = 正常な待ち
	}
	// 🚨 **板が空でも即断しない。約定して板から消えた場合と区別が付かない。**
	//   経路A(レース): ResolveExecution の時点では未約定 → 数百 ms 後に成行返済が
	//     約定 → 板から消える。寄り付きの成行返済では普通に起きる。
	//   経路B(障害の増幅): 立花の一過性エラーで ResolveExecution が失敗 → 続く板
	//     照会も同じ理由で失敗 → 裸扱い。**API の瞬断が口座全体の緊急停止に化ける**。
	// 追加コストは鳴らす直前の 1 回だけなので、ここで約定を引き直す。
	if res != nil && res.OrderID != "" {
		if _, _, confirmed := resolveSettleFill(ctx, brk, res, 0); confirmed {
			return // 約定していた = 裸ではない(記帳は reconcile が引き継ぐ)
		}
	}
	_ = alarm.Trip("close_unfilled_unprotected:"+brokerPositionID, now)
}

// unprotectedAlarm は「守りも決済注文も板に無い = 裸」を報せる先を返す。
//
// 自動の出口は emergency を 1 つ持ち、拒否も裸も同じ警報で trip する(従来どおり
// — 呼び出し側は 1 行も変わらない)。帳簿締め(CloseAllOpen)だけが emergency を
// 持たず unprotected を持つので、「拒否では止めない・裸では止める」が両立する。
//
// 🛑 emergency 側を優先するのは fail-close 方向。両方 nil のときだけ黙る。
func (x closeExecutor) unprotectedAlarm() EmergencyController {
	if x.emergency != nil {
		return x.emergency
	}
	return x.unprotected
}

// hasWorkingCloseOrderOn は決済側に **何らかの** 注文が板に載っているかを見る
// (closeOne と補償経路の両方から使う)。
//
// 🛑 reconcile の同名ヘルパと違い `HasStopLeg` を要求しない。あちらの問いは
// 「守りがあるか」(利確指値だけ残った建玉は下方向に裸)だが、ここの問いは
// 「いま出した決済注文が板に載ったか」で、**成行の返済注文は逆指値脚を持たない**。
// 同じ述語を使い回すと、正常に板へ載った成行を毎回「無い」と読んで trip する。
//
// 照会できないときは false = 裸として扱う(fail-close)。守りを cancel 済みの
// 状態で「確かめられなかった」を「大丈夫」と読む理由は無い。
func hasWorkingCloseOrderOn(ctx context.Context, brk port.Broker, symbol string, closeSide order.Side) bool {
	orders, err := brk.GetActiveOrders(ctx, symbol)
	if err != nil {
		return false
	}
	for _, o := range orders {
		if o.Side == closeSide {
			return true
		}
	}
	return false
}

// asyncSettleBroker is a broker whose settle fills are ASYNCHRONOUS: ClosePosition
// answers with acceptance only and the fill has to be polled (立花). The declaration
// is what keeps this path **live-only** — the alternative (inferring it from a
// missing FilledPrice) also catches paper when it happens to hold no price for a
// symbol, which silently changes the paper measurement (再起動後に当日未約定の
// 銘柄は `p.prices` が空)。
//
// 🛑 両方とも port の interface で組む。ここで自前のメソッド集合を書くと、
// ラッパ(LiveQuoteShared)が転送を忘れたときにアサーションが**黙って外れる** —
// port.LiveBroker の一員なら転送漏れはコンパイルエラーになる(外れると live の
// 決済が丸ごと旧経路へ落ちる)。
type asyncSettleBroker interface {
	port.SettleFillReporter
	port.ExecutionResolver
}

// resolveSettleFill answers "did the settle order actually fill, and at what?".
// confirmed=false means the fill could not be established — the caller must NOT
// book a trade. The unfilled order is deliberately LEFT RESTING, the opposite of
// the entry saga (which cancels and aborts): ストップ安の引けは比例配分で、板に
// 出ている売り注文にしか配分されない — cancelling throws away the only way out.
//
// Shared by closeOne and Reconcile's re-issue: the ghost-close hole exists at
// **every** place a settle order is booked, and fixing only one moves it by a day.
func resolveSettleFill(ctx context.Context, brk port.Broker, res *port.CloseResult, quantity int) (price, fee float64, confirmed bool) {
	if res.FilledPrice > 0 {
		return res.FilledPrice, res.FeeJPY, true // synchronous fill (paper)
	}
	async, ok := brk.(asyncSettleBroker)
	if !ok || !async.SettleFillsAsync() {
		// Fills are synchronous for this adapter: a missing price is a missing
		// price, not an unresolved fill. Keep the legacy observed-price fallback.
		return 0, res.FeeJPY, true
	}
	if res.OrderID == "" {
		// Accepted WITHOUT an order number: 立花's accepted() does not look at
		// sOrderNumber, so this is reachable — and unresolvable. Never book it.
		return 0, 0, false
	}
	rex, err := async.ResolveExecution(ctx, res.OrderID)
	if err != nil {
		// ErrOrderNotFilled = CONFIRMED zero fill (張り付き・売買停止). Anything else
		// = unconfirmable. Both must book nothing: one has no shares to book, the
		// other cannot be valued honestly.
		return 0, 0, false
	}
	// 約定数量が正, on the settle side too. A quantity of 0 means the adapter did
	// not report one (entry saga reads it the same way), but a reported partial
	// would book a flat position while shares are still held. 🛑 部分約定はここで
	// 検知するだけで、残数量での再送はまだ無い(reconcile は元の数量で再送するので
	// 拒否 → hardPeriod で trip し人間へ)。既知の穴。
	if rex.FilledQuantity > 0 && rex.FilledQuantity < quantity {
		return 0, 0, false
	}
	return rex.FilledPrice, rex.FeeJPY, true
}

// protectiveLegToCancel は取り消す 1 本。営業日つきの経路が使えるかを持ち回る。
type protectiveLegToCancel struct {
	info      port.ProtectiveOrderInfo
	withDay   bool   // true = CancelProtectiveOrder(営業日つき)で落とせる
	fallbackI string // withDay=false のときに CancelOrder へ渡す id
}

// protectiveLegsToCancel は決済前に落とすべき注文を **板から** 集める。
//
// 🛑 **板が正、台帳は記憶にすぎない。** 記録した leg id だけを cancel すると、
// それ以外の決済側注文が板に残っていた瞬間に出口が bricked する: 拘束は解けず
// ClosePosition は拒否され、TP / max-hold / ratchet / 14:50 フラット化の**全部**が
// 同じ穴に落ちる。ズレは実口座で起きうる — 板の注文番号と positions_live が指す
// 注文番号が食い違う、100株の建玉に決済注文が 200株載っている、など。
//
// 🚨 **台帳の leg id では取り消さない**。記録した id が板に
// 無ければ、それは既に死んだ注文か板とズレた記憶のどちらかで、しかも**営業日が
// 分からない**。立花の取消は (注文番号, 営業日) で引き、営業日が空なら "0"(当日)に
// 落ちる。注文番号は日付スコープなので、**古い番号 + 当日 が今日の別の注文に当たりうる**
// —— 守りを残したまま無関係な注文を消す最悪の形。板に出ているものだけを、板が
// 教えてくれた鍵で落とす。
//
// 営業日を運べる broker(立花 = port.ProtectiveOrderBoard)では ListProtectiveOrders を
// 使う。その応答だけが BrokerRef に営業日を載せており、CancelProtectiveOrder は
// それが空なら**撃たずに error** を返す(port.ProtectiveOrderBoard の規約)。
// 運べない broker(paper)では GetActiveOrders + CancelOrder のまま — 紙の板は
// プロセス内の帳簿で、営業日という概念自体が無い。
func (x closeExecutor) protectiveLegsToCancel(ctx context.Context, p position.Position) ([]protectiveLegToCancel, error) {
	closeSide := p.Side.Opposite()
	var legs []protectiveLegToCancel
	seen := map[string]bool{}

	if board, ok := x.broker.(port.ProtectiveOrderBoard); ok {
		orders, err := board.ListProtectiveOrders(ctx, p.Symbol)
		if err != nil {
			// 照会できない = 営業日が分からない。**撃たない**(当日扱いで別注文を掴む)。
			// 呼び手は claim 前なので、建玉は OPEN のまま次ティックで再試行される。
			return nil, fmt.Errorf("守りの取消 %s: 板の照会に失敗 — 営業日が分からないので撃たない: %w", p.Symbol, err)
		}
		for _, o := range orders {
			if o.Side != closeSide || o.OrderID == "" || seen[o.OrderID] {
				continue
			}
			seen[o.OrderID] = true
			legs = append(legs, protectiveLegToCancel{info: o, withDay: true})
		}
	}

	// 板の一覧に載らなかった決済側注文(paper の全部 / 営業日つき経路が拾えなかった分)。
	// 🛑 ここで諦めない — 落とし損ねると拘束が残り、返済そのものが通らない。
	orders, err := x.broker.GetActiveOrders(ctx, p.Symbol)
	if err != nil {
		if len(legs) > 0 {
			return legs, nil // 営業日つきで拾えた分だけで進む(そちらが本命)
		}
		return nil, fmt.Errorf("守りの取消 %s: 板の照会に失敗: %w", p.Symbol, err)
	}
	for _, o := range orders {
		if o.Side != closeSide || o.OrderID == "" || seen[o.OrderID] {
			continue
		}
		seen[o.OrderID] = true
		legs = append(legs, protectiveLegToCancel{
			info:      port.ProtectiveOrderInfo{OrderID: o.OrderID, Symbol: o.Symbol, Side: o.Side, Quantity: o.Quantity},
			fallbackI: o.OrderID,
		})
	}
	return legs, nil
}

// cancelLegs は集めた注文を落とし、**落とせなかったものを畳んで返す**。
// ここでは trip しない(呼び手が 1 回だけ鳴らす)。
func (x closeExecutor) cancelLegs(ctx context.Context, legs []protectiveLegToCancel) error {
	var errs []error
	for _, l := range legs {
		if l.withDay {
			board, ok := x.broker.(port.ProtectiveOrderBoard)
			if !ok { // 集めた時点で成立していたので起きないが、型の穴を残さない
				errs = append(errs, fmt.Errorf("守りの取消: broker %T が営業日つきの取消を持たない", x.broker))
				continue
			}
			if err := board.CancelProtectiveOrder(ctx, l.info); err != nil {
				errs = append(errs, fmt.Errorf("守りの取消(注文 %s・営業日 %q): %w",
					l.info.OrderID, l.info.BrokerRef, err))
			}
			continue
		}
		if _, err := x.broker.CancelOrder(ctx, l.fallbackI); err != nil {
			errs = append(errs, fmt.Errorf("守りの取消(注文 %s): %w", l.fallbackI, err))
		}
	}
	return errors.Join(errs...)
}

// closeRejectReason は返済が拒否されたときの trip 理由。**守りを落とせたかで分ける。**
//
//	close_rejected_unprotected        取消は全部通った = 建玉は本当に裸
//	close_rejected_cancel_unconfirmed 取消が 1 本でも確認できていない = 板に守りが
//	                                  残っている可能性がある。裸と読んで置き直すと
//	                                  守りが二重になり両方弾かれうるので、人間は
//	                                  まず板を見る。
func closeRejectReason(cancelErr error) string {
	if cancelErr != nil {
		return "close_rejected_cancel_unconfirmed"
	}
	return "close_rejected_unprotected"
}

// grossPnL is the realized GROSS P&L in JPY; net = gross − fee + carry downstream.
func grossPnL(p position.Position, closePrice float64) float64 {
	diff := closePrice - p.EntryPrice
	if p.Side == order.SideSell {
		diff = -diff
	}
	return diff * float64(p.Quantity)
}
