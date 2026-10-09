package broker

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

// 発注 / 取消 / 守り(逆指値・OCO は同一 CLM)。守りは ocoVerified で fail-close — 人間がデモ環境で
// 逆指値の発動方向・常駐・bot 停止下の約定を実証するまで PlaceSettleOCO / ResolveSettleLegs は error を返し、
// entry saga が巻き戻る(守り無しで建てるより入らない方を選ぶ)。

type newOrderResp struct {
	commonResp
	ResultCode  string `json:"sResultCode"`
	ResultText  string `json:"sResultText"`
	WarningCode string `json:"sWarningCode"`
	WarningText string `json:"sWarningText"`
	OrderNumber string `json:"sOrderNumber"`
	EigyouDay   string `json:"sEigyouDay"`
}

func (r newOrderResp) accepted() bool { return r.PErrNo == tachiErrOK && r.ResultCode == tachiErrOK }
func (r newOrderResp) message() string {
	if r.ResultText != "" {
		return r.ResultText
	}
	return r.WarningText
}

// 公式 v4r9 仕様の現金信用区分:
//
//	0：現物 / 2：新規(制度信用6ヶ月) / 4：返済(制度信用6ヶ月)
//	6：新規(一般信用6ヶ月) / 8：返済(一般信用6ヶ月)
//
// 🛑 **立花の口座で通るのは制度信用**。一般信用 "6" は
// 仕様に存在するのに「現金信用区分に誤りがあります」で拒否された(4751)。公式
// サンプルの信用注文6例も全て制度信用。一般信用の綴りは残してあるが、
// config.SupportsExecKind が立花構成では起動時に弾く。
//
// 一日信用は API に区分そのものが無い。
// 返済はナンピン禁止で1建玉=一意なので建日順(FIFO)で確定できる。
func genkinShinyouKubun(ek order.ExecKind, isClose bool) string {
	switch ek {
	case order.ExecMarginSystem:
		if isClose {
			return tachiGenkinSystemExit // "4" 制度信用返済
		}
		return tachiGenkinSystemNew // "2" 制度信用新規
	case order.ExecMarginGeneral:
		if isClose {
			return tachiGenkinMarginExit // "8" 一般信用返済
		}
		return tachiGenkinMarginNew // "6" 一般信用新規
	default:
		return tachiGenbutu // "0" 現物 (cash / default)
	}
}

// priceField は "0"(成行)か指値。未確定項目は docs/runtime/TACHIBANA_API_NOTES.md §7。
func (t *Tachibana) newOrderFields(symbol string, side order.Side, qty int, priceField, gyakuType, gyakuTrigger, gyakuPrice string, execKind order.ExecKind, isClose bool) map[string]string {
	genkin := genkinShinyouKubun(execKind, isClose)
	tatebi := tachiUnspecified // 現物/新規: 指定なし
	// 🛑 **信用返済なら区分を問わず**建日順。一般信用だけ見ていると、制度信用の
	// 返済が「指定なし」で飛んで拒否される(区分が増えるたびに漏れる形は使わない)。
	if genkin == tachiGenkinMarginExit || genkin == tachiGenkinSystemExit {
		tatebi = tachiTatebiByDate // 信用返済: 建日順(FIFO)
	}
	return map[string]string{
		"sZyoutoekiKazeiC":    t.taxKubun,
		"sIssueCode":          symbol,
		"sSizyouC":            tachiSizyouToushou,
		"sBaibaiKubun":        sideKubun(side),
		"sCondition":          tachiConditionNone,
		"sOrderPrice":         priceField,
		"sOrderSuryou":        strconv.Itoa(qty),
		"sGenkinShinyouKubun": genkin,
		// 当日限り。**守りの脚だけ** PlaceSettleOCO が期日で上書きする(下)。
		// 新規建てと成行返済をここで延ばしてはいけない — 建て注文が数日生き残ると、
		// 寄らなかった翌日に突然約定する。
		"sOrderExpireDay":     tachiExpireToday,
		"sGyakusasiOrderType": gyakuType,
		// 🛑 ここだけ "0"。他の「指定なし」項目(sGyakusasiPrice / sTatebiType /
		// sTategyokuZyoutoekiKazeiC)は "*" だが、逆指値条件は仕様が "0：指定なし"。
		"sGyakusasiZyouken":         orDefault(gyakuTrigger, tachiGyakusasiZyoukenNone),
		"sGyakusasiPrice":           orDefault(gyakuPrice, tachiUnspecified),
		"sTatebiType":               tatebi,
		"sTategyokuZyoutoekiKazeiC": tachiUnspecified,
		"sSecondPassword":           t.secondPW,
	}
}

func (t *Tachibana) PlaceOrder(ctx context.Context, req order.PlaceOrderRequest) (*port.PlaceOrderResult, error) {
	if req.Quantity <= 0 {
		return nil, fmt.Errorf("tachibana: non-positive quantity %d", req.Quantity)
	}
	priceField := tachiOrderPriceMarket
	if req.Type == order.OrderTypeLimit && req.Price > 0 {
		priceField = ftoa(req.Price)
	}
	var r newOrderResp
	fields := t.newOrderFields(req.Symbol, req.Side, req.Quantity, priceField, tachiGyakusasiNone, "", "", req.ExecKind, false)
	if err := t.request(ctx, urlRequest, tachiCLMNewOrder, fields, &r); err != nil {
		return nil, err
	}
	if r.accepted() && r.OrderNumber != "" {
		t.rememberEigyou(r.OrderNumber, r.EigyouDay)
	}
	res := &port.PlaceOrderResult{OrderID: r.OrderNumber, Accepted: r.accepted(), Message: r.message()}
	// 🛑 **信用なら区分を問わず** 建玉一覧と同じ合成キーを返す。ここが一般信用だけを
	// 見ていたため、制度信用の注文は id が空で返り、台帳には現物形式の
	// genbutu:<code>:<課税区分> が入った。reconcile はこの id で broker 側と突き合わせる
	// ので一致せず、bot は**自分が建てた玉を「外部の裸玉」として二重採用**した。
	if req.ExecKind.IsMargin() {
		res.BrokerPositionID = shinyoPositionID(req.Symbol)
	}
	return res, nil
}

// SettleFillsAsync は「決済の約定値は同期に返らない」ことの宣言。呼び手はこれを見て
// ClosePosition の受理を約定と読まず、ResolveExecution で確かめる(usecase の
// asyncSettleBroker)。**paper は実装しない** — 紙は即約定で、価格を持たない銘柄でも
// 同期に返す契約なので、同じ経路に入れると紙の測定が黙って変わる。
func (t *Tachibana) SettleFillsAsync() bool { return true }

func (t *Tachibana) ClosePosition(ctx context.Context, req port.CloseRequest) (*port.CloseResult, error) {
	if req.Quantity <= 0 {
		return nil, fmt.Errorf("tachibana: non-positive close quantity %d", req.Quantity)
	}
	var r newOrderResp
	fields := t.newOrderFields(req.Symbol, req.Side, req.Quantity, tachiOrderPriceMarket, tachiGyakusasiNone, "", "", req.ExecKind, true)
	if err := t.request(ctx, urlRequest, tachiCLMNewOrder, fields, &r); err != nil {
		return nil, err
	}
	if r.accepted() && r.OrderNumber != "" {
		t.rememberEigyou(r.OrderNumber, r.EigyouDay)
	}
	// FilledPrice は同期には返らない。呼び出し側が解決する(port 契約)。
	return &port.CloseResult{OrderID: r.OrderNumber, Accepted: r.accepted(), FilledPrice: 0, Message: r.message()}, nil
}

// 立花は取消に営業日が要るが port が渡さないので、発注時に控えた map から引く。
func (t *Tachibana) CancelOrder(ctx context.Context, orderID string) (*port.CancelResult, error) {
	var r newOrderResp
	fields := map[string]string{
		"sOrderNumber":    orderID,
		"sEigyouDay":      orDefault(t.recallEigyou(orderID), "0"),
		"sSecondPassword": t.secondPW,
	}
	if err := t.request(ctx, urlRequest, tachiCLMCancel, fields, &r); err != nil {
		return nil, err
	}
	return &port.CancelResult{OrderID: orderID, Cancelled: r.accepted()}, nil
}

// CancelProtectiveOrder は守りの注文を取り消す。**営業日は注文そのものから取る。**
//
// 🚨 `CancelOrder(id)` を使わない理由: あちらは `recallEigyou` = プロセス内 map で、
// 空なら "0" に落ちる。守りは前のプロセスが出した注文なので、**再起動後の取消は
// 必ず外れる**。
//
// 🛑 BrokerRef が空なら **API を叩かずに error**。空のまま送ると "0"(当日)になり、
// **別の注文を取り消しうる** —— 守りを消したうえで違う注文まで消す最悪の形。
func (t *Tachibana) CancelProtectiveOrder(ctx context.Context, o port.ProtectiveOrderInfo) error {
	if o.BrokerRef == "" {
		return fmt.Errorf("tachibana: 守りの取消に営業日が無い(order %s) — 照会が sOrderSikkouDay を返していない。"+
			"空のまま送ると当日扱いになり別の注文を取り消しうるので撃たない", o.OrderID)
	}
	var r newOrderResp
	fields := map[string]string{
		"sOrderNumber":    o.OrderID,
		"sEigyouDay":      o.BrokerRef,
		"sSecondPassword": t.secondPW,
	}
	if err := t.request(ctx, urlRequest, tachiCLMCancel, fields, &r); err != nil {
		return err
	}
	if !r.accepted() {
		return fmt.Errorf("tachibana: 守りの取消が拒否された(order %s): %s", o.OrderID, r.message())
	}
	return nil
}

// ListProtectiveOrders は板に残っている決済注文を**期日つき**で返す。
// ExpireOn が zero = 当日限り。🛑 zero を「無期限」と読まない。
func (t *Tachibana) ListProtectiveOrders(ctx context.Context, symbol string) ([]port.ProtectiveOrderInfo, error) {
	rows, err := t.orderList(ctx, symbol)
	if err != nil {
		return nil, err
	}
	var out []port.ProtectiveOrderInfo
	for _, r := range rows {
		if symbol != "" && r.IssueCode != symbol {
			continue
		}
		if !orderIsActive(r) {
			continue
		}
		out = append(out, port.ProtectiveOrderInfo{
			OrderID: r.OrderNumber, Symbol: r.IssueCode,
			ExpireOn:   parseExpireDay(r.OrderExpireDay),
			HasStopLeg: orderHasStopLeg(r.GyakusasiOrderType),
			Side:       baibaiToSide(r.BaibaiKubun),
			Quantity:   int(r.OrderSuryou.f()),
			// 営業日。訂正は (注文番号, 営業日) を要求するので**照会から運ぶ**
			// (プロセス内 map は再起動で空になり、前プロセスが出した守りを訂正できない)。
			//
			// ⚠ 一次資料では `aOrderList` の行に `sEigyouDay` は**無く**、あるのは
			// `sOrderSikkouDay`(注文執行日)だけ。この 2 つを同じ値として扱うのは
			// **既に production で通っている前提**である: `GetExecutions` /
			// `tryResolveTachi` は同じ `sOrderSikkouDay` を `CLMOrderListDetail` の
			// `sEigyouDay` として渡しており(orderDetail の引数)、その経路で実弾の
			// 約定解決が成功している。tryResolveTachi は `rememberEigyou` にも
			// その値を入れている。
			// それでも外れたときは broker が拒否を error で返すので、
			// **黙って守りが切れることはない**(ログで分かる)。
			BrokerRef: r.EigyouDay,
			// 再発注で引き継ぐ値段(上のコメント参照)。
			LimitPrice:  r.Price.f(),
			StopTrigger: r.GyakusasiZyouken.f(),
			StopPrice:   r.GyakusasiPrice.f(),
		})
	}
	return out, nil
}

// parseExpireDay は "YYYYMMDD" を JST の日付へ。"0" / 空 / 解釈不能は **zero**
// (= 当日限り扱い)。読めなかったものを「先の日付」と読むと、切れる守りを
// 「まだ大丈夫」と誤判定する(fail-close)。
func parseExpireDay(s string) time.Time {
	if s == "" || s == tachiExpireToday {
		return time.Time{}
	}
	d, err := time.ParseInLocation(tachiExpireDayLayout, s, clock.JST)
	if err != nil {
		return time.Time{}
	}
	return d
}

var errOCOFailClose = fmt.Errorf("tachibana: protective exit FAIL-CLOSE — set STOCKBOT_TACHIBANA_OCO_VERIFIED=1 only after demo-verifying the 逆指値 (trigger direction / residency / bot-independent fill); docs §0,§5")

// bot が死んでも守りが残るよう broker 側に置く。ocoVerified まで fail-close。
func (t *Tachibana) PlaceSettleOCO(ctx context.Context, in port.OCOCloseOrderInput) (string, error) {
	if !t.ocoVerified {
		return "", errOCOFailClose
	}
	if in.StopLoss <= 0 {
		return "", fmt.Errorf("tachibana: PlaceSettleOCO requires a stop-loss price (no naked entry)")
	}
	gyakuType := tachiGyakusasiStop // '1' 逆指値のみ
	orderPrice := tachiUnspecified  // no 通常 leg
	if in.TakeProfit > 0 {
		gyakuType = tachiGyakusasiDouble // '2' 通常+逆指値 (OCO)
		orderPrice = ftoa(in.TakeProfit) // 通常 leg = 利確指値
	}
	var r newOrderResp
	// トリガーは StopLoss、発動後は成行('0')。守りの leg は返済(信用)か現物売り。
	fields := t.newOrderFields(in.Symbol, in.Side, in.Quantity, orderPrice, gyakuType, ftoa(in.StopLoss), tachiOrderPriceMarket, in.ExecKind, true)
	// 🛑 守りだけ期日を延ばす。当日期限のままだと多日保有の建玉は**建てた日の引けで
	// 守りが消え、2日目から裸**になる。zero のときは当日のまま — 呼び出し側が
	// 営業日を出せなかった(休場カレンダー切れ)ときに、こちらで勝手に日付を捏造しない。
	if !in.ExpireOn.IsZero() {
		fields["sOrderExpireDay"] = in.ExpireOn.Format(tachiExpireDayLayout)
	}
	if err := t.request(ctx, urlRequest, tachiCLMNewOrder, fields, &r); err != nil {
		return "", err
	}
	if !r.accepted() || r.OrderNumber == "" {
		return "", fmt.Errorf("tachibana: protective exit rejected: %s", r.message())
	}
	t.rememberEigyou(r.OrderNumber, r.EigyouDay)
	t.mu.Lock()
	t.settleLegs[in.BrokerPositionID] = r.OrderNumber
	t.mu.Unlock()
	return r.OrderNumber, nil
}

// 立花の通常+逆指値は注文番号1本(leg は分割されない)ので tp/sl とも同じ id を返す。
func (t *Tachibana) ResolveSettleLegs(_ context.Context, brokerPositionID, _ string) (string, string, error) {
	if !t.ocoVerified {
		return "", "", errOCOFailClose
	}
	t.mu.Lock()
	id := t.settleLegs[brokerPositionID]
	t.mu.Unlock()
	if id == "" {
		return "", "", fmt.Errorf("tachibana: no protective order recorded for %q", brokerPositionID)
	}
	return id, id, nil
}

func (t *Tachibana) rememberEigyou(orderID, eigyouDay string) {
	if orderID == "" {
		return
	}
	t.mu.Lock()
	t.eigyouDay[orderID] = eigyouDay
	t.mu.Unlock()
}

func (t *Tachibana) recallEigyou(orderID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.eigyouDay[orderID]
}

// 応答フィールド名は T2 公式サンプルで裏取り済み。誤ると全エントリーが約定確認不能になる。
// ⚠ 要求側の項目名(sIssueCode フィルタ / detail の sOrderNumber+sEigyouDay)は参照実装由来で要デモ確認。
type orderListRow struct {
	OrderNumber    string `json:"sOrderOrderNumber"`
	IssueCode      string `json:"sOrderIssueCode"`
	BaibaiKubun    string `json:"sOrderBaibaiKubun"`
	OrderSuryou    sfloat `json:"sOrderOrderSuryou"`
	CurrentSuryou  sfloat `json:"sOrderCurrentSuryou"` // 残数量 (0 = 全約定)
	Status         string `json:"sOrderStatusCode"`
	YakuzyouStatus string `json:"sOrderYakuzyouStatus"` // 約定状態 0未/1一部/2全部/3約定中
	Price          sfloat `json:"sOrderOrderPrice"`
	EigyouDay      string `json:"sOrderSikkouDay"`
	// 逆指値注文種別 0通常 / 1逆指値 / 2通常+逆指値。守り(SL)を持つ注文の判別子。
	GyakusasiOrderType string `json:"sOrderGyakusasiOrderType"`
	// 注文期日。"0" / 空 = 当日限り。多日保有の守りがいつ切れるかはここでしか読めない。
	OrderExpireDay string `json:"sOrderOrderExpireDay"`
	// 逆指値条件(トリガー価格)と逆指値の執行値段。**取消 → 再発注で値段を引き継ぐ**
	// のに要る。訂正では期日の天井を超えられないので、期日を延ばすには
	// 出し直すしかなく、そのとき板の値段を再現できないと人間が手で締めた幅が消える。
	GyakusasiZyouken sfloat `json:"sOrderGyakusasiZyouken"`
	GyakusasiPrice   sfloat `json:"sOrderGyakusasiPrice"`
}

type orderListResp struct {
	commonResp
	ResultCode string              `json:"sResultCode"`
	Orders     slist[orderListRow] `json:"aOrderList"`
}

type yakuzyouRow struct {
	Suryou sfloat `json:"sYakuzyouSuryou"`
	Price  sfloat `json:"sYakuzyouPrice"`
}

type orderDetailResp struct {
	commonResp
	ResultCode    string             `json:"sResultCode"`
	OrderNumber   string             `json:"sOrderNumber"`
	BaibaiTesuryo sfloat             `json:"sBaiBaiTesuryo"` // 売買手数料
	Shouhizei     sfloat             `json:"sShouhizei"`     // 消費税
	Fills         slist[yakuzyouRow] `json:"aYakuzyouSikkouList"`
}

// p_errno=1(データ無)は「注文ゼロ」= 正常な空であってエラーではない。
func (t *Tachibana) orderList(ctx context.Context, symbol string) ([]orderListRow, error) {
	var r orderListResp
	fields := map[string]string{}
	if symbol != "" {
		fields["sIssueCode"] = symbol
	}
	if err := t.request(ctx, urlRequest, tachiCLMOrderList, fields, &r); err != nil {
		return nil, err
	}
	if r.PErrNo == tachiErrNoData {
		return nil, nil
	}
	if err := businessErr("OrderList", r.PErrNo, r.ResultCode); err != nil {
		return nil, err
	}
	return r.Orders, nil
}

// ProbeOrderRows は注文一覧の**生レコード**をそのまま返す read-only プローブ。
// 注文状態コード(sOrderStatusCode)の実値が repo に無く、docs も「Status 50 常駐」と
// 書くだけなので、締める前に本番の実注文で確かめるために要る。加工しないのが要点 —
// 何が来ているかを見る道具なので、こちらの解釈を混ぜたら用を成さない。発注はしない。
func (t *Tachibana) ProbeOrderRows(ctx context.Context, symbol string) ([]map[string]string, error) {
	var r struct {
		commonResp
		ResultCode string                   `json:"sResultCode"`
		Orders     slist[map[string]string] `json:"aOrderList"`
	}
	fields := map[string]string{}
	if symbol != "" {
		fields["sIssueCode"] = symbol
	}
	if err := t.request(ctx, urlRequest, tachiCLMOrderList, fields, &r); err != nil {
		return nil, err
	}
	if r.PErrNo == tachiErrNoData {
		return nil, nil
	}
	if err := businessErr("OrderList", r.PErrNo, r.ResultCode); err != nil {
		return nil, err
	}
	return r.Orders, nil
}

func (t *Tachibana) orderDetail(ctx context.Context, orderID, eigyouDay string) (orderDetailResp, error) {
	var r orderDetailResp
	fields := map[string]string{
		"sOrderNumber": orderID,
		"sEigyouDay":   orDefault(eigyouDay, t.recallEigyou(orderID)),
	}
	if err := t.request(ctx, urlRequest, tachiCLMOrderListDetail, fields, &r); err != nil {
		return r, err
	}
	if r.PErrNo == tachiErrNoData {
		return r, nil
	}
	if err := businessErr("OrderListDetail", r.PErrNo, r.ResultCode); err != nil {
		return r, err
	}
	return r, nil
}

func (t *Tachibana) GetActiveOrders(ctx context.Context, symbol string) ([]order.Order, error) {
	rows, err := t.orderList(ctx, symbol)
	if err != nil {
		return nil, err
	}
	var out []order.Order
	for _, r := range rows {
		if symbol != "" && r.IssueCode != symbol {
			continue
		}
		if !orderIsActive(r) {
			continue
		}
		out = append(out, order.Order{
			OrderID: r.OrderNumber, Symbol: r.IssueCode, Side: baibaiToSide(r.BaibaiKubun),
			Quantity: int(r.OrderSuryou.f()), Price: r.Price.f(), Status: orDefault(r.YakuzyouStatus, r.Status),
			HasStopLeg: orderHasStopLeg(r.GyakusasiOrderType),
		})
	}
	return out, nil
}

// orderHasStopLeg は注文が逆指値(SL)脚を持つかを逆指値注文種別から判定する。
// 🛑 空("" = 応答に項目が無い)は **false**。「確かめられなかった」を「守られている」と
// 読むと、判別子が取れない構成で守り検査が丸ごと無効化される(fail-close)。
func orderHasStopLeg(gyakusasiOrderType string) bool {
	return gyakusasiOrderType == tachiGyakusasiStop || gyakusasiOrderType == tachiGyakusasiDouble
}

// orderIsActive は「その注文がいま板に残っているか」。
//
// 🛑 **残数量(sOrderCurrentSuryou)を無条件で要求する**。以前は約定状態が空のときだけ
// 残数量を見ていたので、**取消済の注文が有効として返っていた** — 取消済でも約定状態は
// "0"(未約定)のままだからで、約定状態だけでは有効と区別がつかない。死んだ逆指値を
// 守りと読むと、裸の建玉が protectiveOrderIsResting も external_adopt_unprotected も
// 素通りする。
//
// 実測(本番 4704。docs の「Status 50 常駐」は**誤り**だった):
//
//	有効   sOrderStatus=未約定    sOrderStatusCode=1  残数量=100
//	取消済 sOrderStatus=取消完了  sOrderStatusCode=7  残数量=0
//
// 状態コードの全体表は一次資料で確認できていないので、コードの whitelist ではなく
// **残数量**で判定する — 数量はコード表に依存しない(残っていない注文は板にも無い)。
func orderIsActive(r orderListRow) bool {
	if r.YakuzyouStatus == tachiYakuzyouAll { // "2" 全部約定
		return false
	}
	return r.CurrentSuryou.f() > 0
}

// 注文ごと best-effort(1件失敗しても止めない)。live のホットパスではない。
func (t *Tachibana) GetExecutions(ctx context.Context, limit int) ([]order.Execution, error) {
	rows, err := t.orderList(ctx, "")
	if err != nil {
		return nil, err
	}
	var out []order.Execution
	for _, r := range rows {
		det, derr := t.orderDetail(ctx, r.OrderNumber, r.EigyouDay)
		if derr != nil {
			continue
		}
		fee := det.BaibaiTesuryo.f() + det.Shouhizei.f()
		feeAssigned := false
		for i, f := range det.Fills {
			if f.Suryou.f() <= 0 {
				continue
			}
			perFill := 0.0
			if !feeAssigned { // 手数料は最初に**出力した**約定に載せる(先頭の空行を飛ばす)
				perFill = fee
				feeAssigned = true
			}
			out = append(out, order.Execution{
				ExecutionID: r.OrderNumber + ":" + strconv.Itoa(i), OrderID: r.OrderNumber,
				Symbol: r.IssueCode, Side: baibaiToSide(r.BaibaiKubun),
				Quantity: int(f.Suryou.f()), Price: f.Price.f(), FeeJPY: perFill, Timestamp: t.clock(),
			})
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// 期限までに一部でも約定していれば partial を返し、ゼロなら error(約定数量が正)。
func (t *Tachibana) ResolveExecution(ctx context.Context, orderID string) (port.ResolvedExecution, error) {
	deadline := time.After(15 * time.Second)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	everPresent := false // 一度でも板に working として見えたか
	for {
		rex, done, present, err := t.tryResolveTachi(ctx, orderID)
		if err != nil {
			return port.ResolvedExecution{}, err
		}
		everPresent = everPresent || present
		if done {
			return rex, nil
		}
		select {
		case <-ctx.Done():
			return port.ResolvedExecution{}, ctx.Err()
		case <-deadline:
			if rex.FilledQuantity > 0 {
				return rex, nil // partial fill at deadline
			}
			if everPresent {
				// 板に見えていて未約定と確認できたときだけ typed sentinel(saga は cancel で綺麗に中断)。
				return port.ResolvedExecution{}, fmt.Errorf("tachibana: order %q: %w", orderID, port.ErrOrderNotFilled)
			}
			// 一度も見えなかった = 約定して消えた可能性があり確認不能。saga は補償に倒す(裸玉を残さない)。
			return port.ResolvedExecution{}, fmt.Errorf("tachibana: order %q not resolvable before deadline", orderID)
		case <-tick.C:
		}
	}
}

// 戻りは (結果, done=残数量0かつ約定あり, present=板に見えた, err)。
func (t *Tachibana) tryResolveTachi(ctx context.Context, orderID string) (port.ResolvedExecution, bool, bool, error) {
	rows, err := t.orderList(ctx, "")
	if err != nil {
		return port.ResolvedExecution{}, false, false, err
	}
	var row *orderListRow
	for i := range rows {
		if rows[i].OrderNumber == orderID {
			row = &rows[i]
			break
		}
	}
	if row == nil { // まだ板に出ていない
		return port.ResolvedExecution{OrderID: orderID, FilledAt: t.clock()}, false, false, nil
	}
	t.rememberEigyou(orderID, row.EigyouDay)
	det, err := t.orderDetail(ctx, orderID, row.EigyouDay)
	if err != nil {
		return port.ResolvedExecution{}, false, true, err
	}
	var qty, pxQty float64
	for _, f := range det.Fills {
		qty += f.Suryou.f()
		pxQty += f.Suryou.f() * f.Price.f()
	}
	rex := port.ResolvedExecution{
		OrderID:          orderID,
		FilledQuantity:   int(qty),
		FeeJPY:           det.BaibaiTesuryo.f() + det.Shouhizei.f(),
		BrokerPositionID: "genbutu:" + row.IssueCode + ":" + t.taxKubun,
		FilledAt:         t.clock(),
	}
	if qty > 0 {
		rex.FilledPrice = pxQty / qty // 分割約定の VWAP
	}
	fullyFilled := row.OrderSuryou.f() > 0 && row.CurrentSuryou.f() == 0
	return rex, fullyFilled && qty > 0, true, nil
}

// 建玉番号は新規時に即返らないので、ナンピン禁止(1銘柄1建玉)を前提に code から合成する。
func shinyoPositionID(symbol string) string { return "shinyo:" + symbol }

func baibaiToSide(s string) order.Side {
	if s == tachiBaibaiSell {
		return order.SideSell
	}
	return order.SideBuy
}

func ftoa(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
