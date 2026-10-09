package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"stockbot/backend/internal/port"
	"stockbot/backend/internal/usecase/command"
	"stockbot/backend/internal/usecase/query"
)

// LiveViews は hybrid の live トラックの読み書き口。**research 側の束とは別インスタンス**
// で、handler はどちらがどちらかを混ぜない(損益の混読が実弾の判断を誤らせる)。
//
// nil = live track 無し → /api/live/* はすべて 503。「取引ゼロ」を返さないのは、
// 空の応答を「live は動いているが建玉が無い」と読めてしまうため。
type LiveViews struct {
	Status      *query.GetBotStatus
	ListOpen    *query.ListOpenPositions
	Performance *query.BuildForwardReport
	// Cycles は戦績のサイクル切り替え。live は同じ live DB を期間で切るので Report は nil。
	Cycles       []PerformanceCycle
	CycleDefault string
	Emergency    EmergencyController
	// Close は **個別成行決済のみ**。live の flatten-all は提供しない
	// (全清算を人間がボタン 1 つでやる操作にしない — HYBRID Step 5)。
	Close       ManualCloser
	ActiveBySym func() map[string]string
	// Extend は live の max_hold 延長。**broker 側の守り(逆指値/OCO)には触らない** —
	// 伸ばすのは bot 側の時間切れ決済だけ。守りの期日は別経路(注文訂正)が延ばす。
	Extend MaxHoldExtender
	// ExtendOptions は延長先の候補(各営業日の引け前・1 回の上限内)。画面のセレクトが
	// 読み、選ばれた add_minutes を Extend へ渡す。nil = 未配線 → 503。
	ExtendOptions MaxHoldExtendOptioner
	// ReplaceProtective は守りの**取消 → 再発注**(期日の延長)。
	// 🛑 訂正では発注日+10営業日の天井を超えられないので、これが唯一の延長手段。
	// nil = 未配線 → 503。
	// 返り値は (置き直した本数, 期日に余裕があって見送った本数, エラーの文言)。
	// **エラーを握り潰さない** — 置き直しの失敗は「建玉が裸」を意味しうる。
	// 🛑 skipped を返すのは `replaced: 0` の意味を割るため。「期日が近いものが
	// 無かった」と「対象はあったが全部失敗した」は運用上まったく別の状態。
	ReplaceProtective func(context.Context) (replaced, skipped int, errs []string)
	// ArmProtective は**守りが 1 本も無い多日建玉に守りを置く**。置き直しとは別物:
	// 置き直しは板に注文が在ることが前提で、板が空だと何もできない(実弾の建玉を
	// 裸にしたとき、置き直しにできることは無い)。
	// 人間が渡すのは値段だけ — 数量・区分・建玉 ID は台帳から取る。
	// nil = 未配線 → 503。
	ArmProtective func(ctx context.Context, symbol string, tp, sl float64) (string, error)
	// RepriceProtective は守りの**値段**を変える(取消 → 指定値段で再発注)。
	// 🛑 **場中でも走る**。取消と再発注の間は
	// 守りが板から消えるが、その窓を承知の上で選んだ経路。usecase 側が取消の前に
	// 建玉・区分・値段の向きを全部検証する。nil = 未配線 → 503。
	RepriceProtective func(ctx context.Context, symbol string, tp, sl float64) (string, error)
	// Extra は research 側 dashExtra と同じ形の UI 集計(counters / summaries /
	// selector.ranking)。live 用に**別インスタンス**を作る — 混ぜると画面上で
	// どちらの数字か区別できなくなる。
	Extra func() map[string]any
	// 銘柄ごとの新規停止(人間のボタン)。止めるのは live のその銘柄の新規だけ。
	// nil = 未配線 → 503。SymbolBlocks は live dashboard の `symbol_blocks` に載る。
	BlockSymbol   SymbolBlocker
	ReleaseSymbol SymbolReleaser
	SymbolBlocks  *query.ListSymbolBlocks
}

// SymbolBlocker は銘柄の新規を止める(command.BlockLiveSymbol)。
type SymbolBlocker interface {
	Execute(ctx context.Context, symbol, note string) error
}

// SymbolReleaser は停止を解く(command.ReleaseLiveSymbol)。
type SymbolReleaser interface {
	Execute(ctx context.Context, symbol string) error
}

// WithLiveTrack wires the live track's read/write surface. 未呼び出し = live 無し。
func (h *Handler) WithLiveTrack(v *LiveViews) *Handler {
	h.live = v
	return h
}

// liveOr503 は live track 未配線を 503 に落とす共通の入口。
func (h *Handler) liveOr503(fn func(*LiveViews, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.live == nil {
			// 🛑 空の応答を返さない。「live は動いているが建玉が無い」と読めてしまう。
			http.Error(w, "live track is not running (STOCKBOT_LIVE_BOT_CONFIG が未設定)",
				http.StatusServiceUnavailable)
			return
		}
		fn(h.live, w, r)
	}
}

func (h *Handler) getLiveStatus(v *LiveViews, w http.ResponseWriter, r *http.Request) {
	view, err := v.Status.Execute(r.Context(), v.ActiveBySym())
	writeJSON(w, view, err)
}

// research の /api/dashboard と同じ形。UI は同じ描画関数を track 切替で使い回す。
func (h *Handler) getLiveDashboard(v *LiveViews, w http.ResponseWriter, r *http.Request) {
	view, err := v.Status.Execute(r.Context(), v.ActiveBySym())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := map[string]any{"status": view}
	if v.Extra != nil {
		for k, val := range v.Extra() {
			out[k] = val
		}
	}
	if v.SymbolBlocks != nil {
		out["symbol_blocks"] = v.SymbolBlocks.Execute(r.Context())
	}
	writeJSON(w, out, nil)
}

// 🛑 live の銘柄ごとの新規停止。**人間の操作**(AI は実機の bot に POST しない)。
// 止めるのは新規だけ — 決済・守り・引け前フラット化・max_hold 延長・既存の建玉には効かない。
// 応答は停止の一覧(画面がそのまま描き直す)。
func (h *Handler) postLiveSymbolBlock(v *LiveViews, w http.ResponseWriter, r *http.Request) {
	if v.BlockSymbol == nil || v.SymbolBlocks == nil {
		http.Error(w, "live symbol block not available", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Symbol string `json:"symbol"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := v.BlockSymbol.Execute(r.Context(), req.Symbol, req.Note); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, command.ErrSymbolNotAllowed) {
			status = http.StatusBadRequest
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, v.SymbolBlocks.Execute(r.Context()), nil)
}

func (h *Handler) postLiveSymbolBlockRelease(v *LiveViews, w http.ResponseWriter, r *http.Request) {
	if v.ReleaseSymbol == nil || v.SymbolBlocks == nil {
		http.Error(w, "live symbol block not available", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Symbol string `json:"symbol"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := v.ReleaseSymbol.Execute(r.Context(), req.Symbol); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, port.ErrSymbolNotBlocked) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, v.SymbolBlocks.Execute(r.Context()), nil)
}

func (h *Handler) getLivePositions(v *LiveViews, w http.ResponseWriter, r *http.Request) {
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		http.Error(w, "missing symbol", http.StatusBadRequest)
		return
	}
	views, err := v.ListOpen.Execute(r.Context(), symbol)
	writeJSON(w, views, err)
}

func (h *Handler) getLivePerformance(v *LiveViews, w http.ResponseWriter, r *http.Request) {
	if v.Performance == nil {
		http.Error(w, "live performance not available (no trade repository)", http.StatusServiceUnavailable)
		return
	}
	servePerformance(w, r, v.Performance, v.Cycles)
}

// 🛑 live の**個別**成行決済。実弾 1 ポジを人間が手で返済する唯一の手段。
//
// research 側の「警報なし」規約は**流用しない** — あちらは「paper は資本リスク
// ゼロ」という理由に依存していて live に移植できない。
//
// live が活かすのは `closeOne` の **`close_unfilled_unprotected`** trip: 守りの脚を
// cancel した後に決済が「受理されたのに約定も板への常駐もしない」と、その建玉は
// 逆指値も決済注文も持たない。宛先は `NewCloseAllOpen` の
// `unprotected` 引数で、live_track.go が `lt.emergency` を渡している。
// 🛑 **`close_rejected_unprotected` はこの経路では発火しない** — 「帳簿締めの close
// 拒否で bot を止めない」という CloseAllOpen の事前コミットを維持しているため
// (拒否は CLOSING のまま reconcile が回収)。
func (h *Handler) postLiveClosePosition(v *LiveViews, w http.ResponseWriter, r *http.Request) {
	if v.Close == nil {
		http.Error(w, "live manual close not available", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		PositionID int64 `json:"position_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if req.PositionID <= 0 {
		http.Error(w, "position_id must be a positive integer", http.StatusBadRequest)
		return
	}
	res, err := v.Close.CloseOne(r.Context(), req.PositionID)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, command.ErrPositionNotFound):
			status = http.StatusNotFound
		case errors.Is(err, command.ErrPositionNotOpen), errors.Is(err, command.ErrPositionExternal):
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, res, nil)
}

// live の emergency は **research を止めない**(別フラグファイル)。紙は資本リスクが
// ゼロでデータ収集を続ける価値があるので、片側の trip を他側に伝播させない。
func (h *Handler) postLiveEmergencyStop(v *LiveViews, w http.ResponseWriter, r *http.Request) {
	reason := r.URL.Query().Get("reason")
	if reason == "" {
		reason = "manual_api_live"
	}
	if err := v.Emergency.Trip(reason, time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"emergency_stop": true, "track": "live", "reason": v.Emergency.Reason()}, nil)
}

func (h *Handler) postLiveEmergencyResume(v *LiveViews, w http.ResponseWriter, r *http.Request) {
	if err := v.Emergency.Resume(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"emergency_stop": false, "track": "live"}, nil)
}

// 🛑 live の max_hold 延長。research と同じ本体を使うが**別の usecase インスタンス**
// (live の台帳を見る)を渡す — 混ぜると research の id で実弾建玉を伸ばせてしまう。
//
// 伸ばすのは bot 側の時間切れ決済だけで、broker 側の守りは動かない。多日保有を
// 守りの期日より先まで伸ばすと裸になるので、**守りの期日訂正とセットで使う**
func (h *Handler) postLiveExtendMaxHold(v *LiveViews, w http.ResponseWriter, r *http.Request) {
	extendMaxHold(v.Extend, w, r)
}

// MaxHoldExtendOptioner は延長先の候補を列挙する(query.ListExtendOptions)。
type MaxHoldExtendOptioner interface {
	Execute(ctx context.Context, positionID int64) (*query.ExtendOptionsView, error)
}

// 延長先の候補。**読むだけ**(延長そのものは POST …/positions/extend)。
// 🛑 候補の日付も 1 回の上限も handler は持たない — カレンダーと保有区分を読める
// usecase が決める(extendMaxHold と同じ理由)。
func (h *Handler) getLiveExtendOptions(v *LiveViews, w http.ResponseWriter, r *http.Request) {
	if v.ExtendOptions == nil {
		http.Error(w, "extend options not available", http.StatusServiceUnavailable)
		return
	}
	id, err := strconv.ParseInt(r.URL.Query().Get("position_id"), 10, 64)
	if err != nil {
		http.Error(w, "position_id must be an integer", http.StatusBadRequest)
		return
	}
	view, err := v.ExtendOptions.Execute(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if view == nil {
		http.Error(w, "no open position with that id", http.StatusNotFound)
		return
	}
	writeJSON(w, view, nil)
}

// postLiveReplaceProtective は守りを取消 → 再発注して期日を延ばす。
//
// 🛑 **叩いたときだけ走る。** 自動化(場外の定期実行)を入れる前に、人間が見ている
// 前で 1 回試せる面を用意する — 初回の実弾操作を無人で走らせない。
func (h *Handler) postLiveReplaceProtective(v *LiveViews, w http.ResponseWriter, r *http.Request) {
	if v.ReplaceProtective == nil {
		http.Error(w, "protective replace not available", http.StatusServiceUnavailable)
		return
	}
	n, skipped, errs := v.ReplaceProtective(r.Context())
	if errs == nil {
		errs = []string{}
	}
	writeJSON(w, map[string]any{"replaced": n, "skipped": skipped, "errors": errs}, nil)
}

// postLiveArmProtective は守りの**新規設置**。板に守りが無い建玉を救う唯一の経路。
//
// 🛑 **emergency 中でも通す。**emergency は新規建てを止めるものであって、
// 裸の建玉に守りを置くのを止めるものではない。ここを塞ぐと「trip した後は
// 二度と守れない」になり、trip するほど危険になるという逆立ちが起きる。
//
// 🛑 **数量は受け取らない。**人間が渡すのは値段(tp/sl)だけで、数量・side・
// 口座区分・建玉 ID は台帳から引く。数量を打たせると打ち間違いがそのまま
// 「建玉より多い返済注文」になる。
func (h *Handler) postLiveArmProtective(v *LiveViews, w http.ResponseWriter, r *http.Request) {
	if v.ArmProtective == nil {
		http.Error(w, "protective arm not available", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Symbol     string  `json:"symbol"`
		TakeProfit float64 `json:"take_profit"`
		StopLoss   float64 `json:"stop_loss"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "body must be {\"symbol\":..,\"take_profit\":..,\"stop_loss\":..}: "+err.Error(),
			http.StatusBadRequest)
		return
	}
	id, err := v.ArmProtective(r.Context(), req.Symbol, req.TakeProfit, req.StopLoss)
	if err != nil {
		// 🛑 400 で返す。ここでの失敗は**建玉が裸のまま**を意味するので、
		// 呼んだ人間が読める文言をそのまま出す(握り潰さない)。
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"symbol": req.Symbol, "order_id": id,
		"take_profit": req.TakeProfit, "stop_loss": req.StopLoss}, nil)
}

// postLiveRepriceProtective は守りの**値段**を変える(取消 → 再発注)。
//
// 🚨 **実弾の守りを一度板から降ろす。**再発注が失敗すればその建玉は裸になり、
// usecase 側が emergency を trip する。arm(置くだけ)と違い、これは
// **リスクが一時的に増える**操作なので、失敗の文言をそのまま人間に返す。
func (h *Handler) postLiveRepriceProtective(v *LiveViews, w http.ResponseWriter, r *http.Request) {
	if v.RepriceProtective == nil {
		http.Error(w, "protective reprice not available", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Symbol     string  `json:"symbol"`
		TakeProfit float64 `json:"take_profit"`
		StopLoss   float64 `json:"stop_loss"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "body must be {\"symbol\":..,\"take_profit\":..,\"stop_loss\":..}: "+err.Error(),
			http.StatusBadRequest)
		return
	}
	id, err := v.RepriceProtective(r.Context(), req.Symbol, req.TakeProfit, req.StopLoss)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"symbol": req.Symbol, "order_id": id,
		"take_profit": req.TakeProfit, "stop_loss": req.StopLoss}, nil)
}
