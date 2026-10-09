// Package handler は薄い HTTP 層: parse → usecase → encode。
// 業務ロジックを持たず、repository を直接触らない。
package handler

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/usecase/command"
	"stockbot/backend/internal/usecase/query"
)

type EmergencyController interface {
	Active() bool
	Reason() string
	Trip(reason string, now time.Time) error
	Resume() error
}

// nil 結果 = 該当する OPEN 建玉なし(→ 404)。
type MaxHoldExtender interface {
	Execute(ctx context.Context, positionID int64, addMinutes int) (*port.MaxHoldExtended, error)
}

// CloseOne は 404/409 の写像用に ErrPositionNotFound / ErrPositionNotOpen /
// ErrPositionExternal を返す。
type ManualCloser interface {
	Execute(ctx context.Context) (command.CloseAllResult, error)
	CloseOne(ctx context.Context, positionID int64) (*command.CloseOneResult, error)
}

type Handler struct {
	status      *query.GetBotStatus
	listOpen    *query.ListOpenPositions
	emergency   EmergencyController
	extend      MaxHoldExtender
	activeBySym func() map[string]string
	// UI 専用の集計。シリアライズ済みの形で受けることで handler は app に依存しない。
	dashExtra func() map[string]any

	// token 未設定 × loopback 外 bind は fail-close。人間専用の emergency ゲートが
	// ネットワーク越しに無認証で叩ける状態を作らない。
	apiToken        string
	bindNonLoopback bool

	// LLM 判断ジャーナル(観測専用)。nil → 空配列を返す(パネルを壊さない)。
	advisorRuns *query.ListAdvisorRuns

	// nil = trade repo 無し → 503。空の戦績を「損益ゼロ」と誤読させないため。
	performance *query.BuildForwardReport
	// 戦績のサイクル切り替え(nil = `?cycle=` は常に 400・一覧は空)。
	perfCycles       []PerformanceCycle
	perfCycleDefault string

	// 非同期起動して即 return する契約(実行中なら error)。nil = advisor OFF → 503。
	advisorTrigger func() error

	// 画面の「未判定を判定」ボタン。arm 済みで判定が無い銘柄の go/no-go を別プロセスで起動して即 return し、
	// 起動した銘柄を返す(実行中なら port.ErrGoNoGoRunning)。nil = 未配線 → 503。表示と記録だけ。
	gonogoRun func() ([]string, error)

	// nil = 未配線 → 503。mode は稼働モード文字列(層規約 R1: handler は config を
	// import せず string で受け取る)。live_config では全清算を 409 で拒否する。
	manualClose ManualCloser
	mode        string

	// hybrid の live トラック(nil = live 無し → /api/live/* は 503)。
	// **research の束と混ぜない** — 損益の混読が実弾の判断を誤らせる。
	live *LiveViews
}

func New(status *query.GetBotStatus, listOpen *query.ListOpenPositions, em EmergencyController, extend MaxHoldExtender, activeBySym func() map[string]string, dashExtra func() map[string]any) *Handler {
	return &Handler{status: status, listOpen: listOpen, emergency: em, extend: extend, activeBySym: activeBySym, dashExtra: dashExtra}
}

// token = STOCKBOT_API_TOKEN。bindNonLoopback かつ token 空なら変更系は 403 —
// リモート公開が黙って無認証になることを防ぐ。読み取り系はこの制限を受けない。
func (h *Handler) WithAuth(token string, bindNonLoopback bool) *Handler {
	h.apiToken = token
	h.bindNonLoopback = bindNonLoopback
	return h
}

func (h *Handler) WithAdvisorRuns(q *query.ListAdvisorRuns) *Handler {
	h.advisorRuns = q
	return h
}

func (h *Handler) WithPerformance(q *query.BuildForwardReport) *Handler {
	h.performance = q
	return h
}

// cmd が closure で注入する(handler は app を import しない)。
func (h *Handler) WithAdvisorTrigger(f func() error) *Handler {
	h.advisorTrigger = f
	return h
}

// mode は cmd から string で注入する(handler は config を知らない)。
func (h *Handler) WithManualClose(c ManualCloser, mode string) *Handler {
	h.manualClose = c
	h.mode = mode
	return h
}

// token があれば一致必須。token 無しなら loopback bind のみ許可(非 loopback は
// fail-close)。loopback bind は他プロセスは締め出すが**同じマシンのブラウザ**は
// 締め出さない: 悪意あるページは preflight 無しでここへ cross-origin POST できる
// (カスタムヘッダ不要・body の content-type も見ていない)ので、既定の dev 構成では
// 開いているタブから emergency-resume / extend が叩けてしまう。以下の fetch
// metadata 検査がそれを塞ぐ。curl や運用スクリプトは metadata を送らないので無影響。
func (h *Handler) authorizeMutation(w http.ResponseWriter, r *http.Request) bool {
	// cross-site のブラウザ POST = CSRF。"none" は手入力 URL / ブックマーク。
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" && s != "same-origin" && s != "none" {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return false
	}
	if h.apiToken != "" {
		if !tokenMatches(r, h.apiToken) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return false
		}
		return true
	}
	if h.bindNonLoopback {
		http.Error(w, "control API disabled on a non-loopback bind: set STOCKBOT_API_TOKEN to enable", http.StatusForbidden)
		return false
	}
	// DNS rebinding: 攻撃者ドメインが 127.0.0.1 に再解決されるとブラウザには
	// same-origin に見えて Sec-Fetch-Site では捕まらない。Host ヘッダには攻撃者の
	// 名前が残るので、loopback bind では Host も loopback であることを要求する。
	if isBrowserRequest(r) && !isLoopbackHost(r.Host) {
		http.Error(w, "unexpected Host for a loopback bind", http.StatusForbidden)
		return false
	}
	// fetch metadata 以前のエンジン(古い Electron/WebView の Chromium <76)は
	// Sec-Fetch-Site を送らず上の検査を素通りする。POST には必ず Origin が付くので
	// そちらも同じ基準で見る。curl や運用スクリプトは Origin を送らない。
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || !isLoopbackHost(u.Host) || !strings.EqualFold(u.Host, r.Host) {
			http.Error(w, "unexpected Origin for a loopback bind", http.StatusForbidden)
			return false
		}
	}
	return true
}

// mutating は全部これを通す。GET 側の guardRead と対にして、ガードを**配線の 1 か所**に
// 集める — 各ハンドラの先頭で呼ぶ形だと、新しい POST を足したときに 1 本だけ書き忘れても
// コンパイルは通り、テストが網羅していなければ緑のまま無防備になる。
func (h *Handler) guardWrite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.authorizeMutation(w, r) {
			return
		}
		next(w, r)
	}
}

// loopback bind のときだけ効かせる: 非 loopback 配備は LAN 名で来るので、そこで
// loopback Host を要求すると読み取りアクセスが壊れる。
func (h *Handler) guardRead(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// loopback の外に bind したら参照系も token で守る。変更系だけ守ると、同じネットワークの
		// 誰でも建玉と損益を読める。token 無しの非 loopback は fail-close(変更系と同じ)。
		if h.bindNonLoopback {
			if h.apiToken == "" {
				http.Error(w, "read API disabled: non-loopback bind requires STOCKBOT_API_TOKEN", http.StatusForbidden)
				return
			}
			if !tokenMatches(r, h.apiToken) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		if !h.bindNonLoopback && isBrowserRequest(r) {
			if !isLoopbackHost(r.Host) {
				http.Error(w, "unexpected Host for a loopback bind", http.StatusForbidden)
				return
			}
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || !isLoopbackHost(u.Host) || !strings.EqualFold(u.Host, r.Host) {
					http.Error(w, "unexpected Origin for a loopback bind", http.StatusForbidden)
					return
				}
			}
		}
		next(w, r)
	}
}

// 現行ブラウザは必ず付け、非ブラウザのクライアントは送らないヘッダで判定する。
func isBrowserRequest(r *http.Request) bool {
	return r.Header.Get("Sec-Fetch-Site") != "" || r.Header.Get("Origin") != ""
}

// Host は大小無視・末尾ドット(root label)ありうる。ここで false negative を出すと
// **運用者が自分のキルスイッチから 403 で締め出される**ので、正規化してから比べる。
func isLoopbackHost(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host // no port present
	}
	h = strings.TrimSuffix(strings.ToLower(strings.Trim(h, "[]")), ".")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// 比較は constant time。
func tokenMatches(r *http.Request, want string) bool {
	got := r.Header.Get("X-Api-Token")
	if got == "" {
		if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
			got = strings.TrimPrefix(a, "Bearer ")
		}
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.indexPage) // dashboard SPA at /
	// 読み取りは意図的に無認証だが、rebinding したページに吸い出されてはいけないので
	// 変更系と同じブラウザ出自チェックを通す。
	mux.HandleFunc("GET /api/status", h.guardRead(h.getStatus))
	mux.HandleFunc("GET /api/dashboard", h.guardRead(h.getDashboard))
	mux.HandleFunc("GET /api/positions", h.guardRead(h.getPositions))
	mux.HandleFunc("GET /api/performance", h.guardRead(h.getPerformance))
	mux.HandleFunc("GET /api/performance/cycles", h.guardRead(h.getPerformanceCycles))
	mux.HandleFunc("GET /api/advisor-runs", h.guardRead(h.getAdvisorRuns))
	mux.HandleFunc("POST /api/advisor-trigger", h.guardWrite(h.postAdvisorTrigger))
	// 寄り前の go/no-go の「未判定を判定」(表示と記録だけ・research と live の両方の未判定を判定する)。
	// live タブは api() が /api/live を前置するので同じ処理を live 側にも置く(live track の有無に依らない)。
	mux.HandleFunc("POST /api/gonogo/run", h.guardWrite(h.postGoNoGoRun))
	mux.HandleFunc("POST /api/live/gonogo/run", h.guardWrite(h.postGoNoGoRun))
	mux.HandleFunc("POST /api/positions/extend", h.guardWrite(h.postExtendMaxHold))
	mux.HandleFunc("POST /api/positions/close", h.guardWrite(h.postClosePosition))
	mux.HandleFunc("POST /api/flatten-all", h.guardWrite(h.postFlattenAll))
	mux.HandleFunc("POST /api/emergency-stop", h.guardWrite(h.postEmergencyStop))
	mux.HandleFunc("POST /api/emergency-resume", h.guardWrite(h.postEmergencyResume))
	// hybrid: live トラック。既存ルートは research のまま**不変**で、live は別 prefix。
	// 🛑 live の flatten-all は提供しない(全清算を人間がボタン 1 つでやる操作にしない)。
	mux.HandleFunc("GET /api/live/dashboard", h.guardRead(h.liveOr503(h.getLiveDashboard)))
	mux.HandleFunc("GET /api/live/status", h.guardRead(h.liveOr503(h.getLiveStatus)))
	mux.HandleFunc("GET /api/live/positions", h.guardRead(h.liveOr503(h.getLivePositions)))
	mux.HandleFunc("GET /api/live/performance", h.guardRead(h.liveOr503(h.getLivePerformance)))
	mux.HandleFunc("GET /api/live/performance/cycles", h.guardRead(h.liveOr503(h.getLivePerformanceCycles)))
	mux.HandleFunc("POST /api/live/positions/close", h.guardWrite(h.liveOr503(h.postLiveClosePosition)))
	mux.HandleFunc("POST /api/live/positions/extend", h.guardWrite(h.liveOr503(h.postLiveExtendMaxHold)))
	mux.HandleFunc("GET /api/live/positions/extend-options", h.guardRead(h.liveOr503(h.getLiveExtendOptions)))
	// 守りの取消 → 再発注(期日の延長)。訂正では天井を超えられないので唯一の手段。
	mux.HandleFunc("POST /api/live/protective/replace", h.guardWrite(h.liveOr503(h.postLiveReplaceProtective)))
	mux.HandleFunc("POST /api/live/protective/arm", h.guardWrite(h.liveOr503(h.postLiveArmProtective)))
	mux.HandleFunc("POST /api/live/protective/reprice", h.guardWrite(h.liveOr503(h.postLiveRepriceProtective)))
	mux.HandleFunc("POST /api/live/emergency-stop", h.guardWrite(h.liveOr503(h.postLiveEmergencyStop)))
	mux.HandleFunc("POST /api/live/emergency-resume", h.guardWrite(h.liveOr503(h.postLiveEmergencyResume)))
	mux.HandleFunc("POST /api/live/symbol-blocks", h.guardWrite(h.liveOr503(h.postLiveSymbolBlock)))
	mux.HandleFunc("POST /api/live/symbol-blocks/release", h.guardWrite(h.liveOr503(h.postLiveSymbolBlockRelease)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

func (h *Handler) getStatus(w http.ResponseWriter, r *http.Request) {
	view, err := h.status.Execute(r.Context(), h.activeBySym())
	writeJSON(w, view, err)
}

// advisor 不在でも error ではなく空配列(既定 OFF でパネルが dashboard を壊さない)。
func (h *Handler) getAdvisorRuns(w http.ResponseWriter, r *http.Request) {
	limit := 0 // 0 = query 側の既定(20)
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	views, err := h.advisorRuns.Execute(r.Context(), r.URL.Query().Get("symbol"), limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if views == nil {
		views = []query.AdvisorRunView{}
	}
	writeJSON(w, views, nil)
}

// ?since=YYYY-MM-DD は JST の 0 時(日付境界を UTC で切らない)。解釈できない since は
// 黙って全期間に落とさず 400 — 別物の集計を戦績として見せない。
func (h *Handler) getPerformance(w http.ResponseWriter, r *http.Request) {
	if h.performance == nil {
		http.Error(w, "performance not available (no trade repository)", http.StatusServiceUnavailable)
		return
	}
	servePerformance(w, r, h.performance, h.perfCycles)
}

// 取引日は JST で切る。
var jst = clock.JST

// SPA が1リフレッシュ1 fetch で済むよう status + counters + 銘柄別を1本にまとめる。
func (h *Handler) getDashboard(w http.ResponseWriter, r *http.Request) {
	view, err := h.status.Execute(r.Context(), h.activeBySym())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := map[string]any{"status": view}
	if h.dashExtra != nil {
		for k, v := range h.dashExtra() {
			out[k] = v
		}
	}
	writeJSON(w, out, nil)
}

func (h *Handler) getPositions(w http.ResponseWriter, r *http.Request) {
	symbol := r.URL.Query().Get("symbol")
	if symbol == "" {
		http.Error(w, "missing symbol", http.StatusBadRequest)
		return
	}
	views, err := h.listOpen.Execute(r.Context(), symbol)
	writeJSON(w, views, err)
}

// broker 側の TP/SL OCO には触らない — bot 側の時間切れ決済だけを後ろへずらす。
func (h *Handler) postExtendMaxHold(w http.ResponseWriter, r *http.Request) {
	extendMaxHold(h.extend, w, r)
}

// extendMaxHold は research / live で共通の本体。
//
// 🛑 **上限の数字を handler が持たない。** 1 回あたりの上限は保有区分で変わる
// (intraday 12時間 / multiday 30日)ので、判定できるのは建玉を読める usecase だけ。
// ここに 720 を焼くと multiday の延長が handler で先に落ち、usecase 側の規則が
// 死んだままになる。
func extendMaxHold(ext MaxHoldExtender, w http.ResponseWriter, r *http.Request) {
	if ext == nil {
		http.Error(w, "extend not available", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		PositionID int64 `json:"position_id"`
		AddMinutes int   `json:"add_minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if req.AddMinutes < 1 {
		http.Error(w, "add_minutes must be a positive integer", http.StatusBadRequest)
		return
	}
	res, err := ext.Execute(r.Context(), req.PositionID, req.AddMinutes)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, command.ErrEmergencyActive):
			status = http.StatusConflict // 手動 override は emergency を絶対にバイパスしない
		case errors.Is(err, command.ErrInvalidExtendMinutes):
			status = http.StatusBadRequest
		}
		http.Error(w, err.Error(), status)
		return
	}
	if res == nil {
		http.Error(w, "no open position with that id", http.StatusNotFound)
		return
	}
	writeJSON(w, res, nil)
}

// research / paper 専用: live_config では 409 で拒否する — 実弾の全清算は人間が
// 証券会社の画面でやる判断で、HTTP 一発で起こしてよい操作ではない。
func (h *Handler) postFlattenAll(w http.ResponseWriter, r *http.Request) {
	if h.manualClose == nil {
		http.Error(w, "flatten-all not available", http.StatusServiceUnavailable)
		return
	}
	if h.mode == "live_config" {
		http.Error(w, "flatten-all is disabled in live_config (実弾の全清算は人間の判断)", http.StatusConflict)
		return
	}
	res, err := h.manualClose.Execute(r.Context())
	writeJSON(w, res, err)
}

// emergency 中でも通る(決済はリスクを減らす方向 — entry/extend の hard gate とは別)。
func (h *Handler) postClosePosition(w http.ResponseWriter, r *http.Request) {
	if h.manualClose == nil {
		http.Error(w, "manual close not available", http.StatusServiceUnavailable)
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
	res, err := h.manualClose.CloseOne(r.Context(), req.PositionID)
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

func (h *Handler) postEmergencyStop(w http.ResponseWriter, r *http.Request) {
	reason := r.URL.Query().Get("reason")
	if reason == "" {
		reason = "manual_api"
	}
	if err := h.emergency.Trip(reason, time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"emergency_stop": true, "reason": h.emergency.Reason()}, nil)
}

func (h *Handler) postEmergencyResume(w http.ResponseWriter, r *http.Request) {
	if err := h.emergency.Resume(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"emergency_stop": false}, nil)
}

// 202 = 非同期受付(LLM は最長 timeout_seconds 走るので待たせない)。config の arm に
// 繋がる = 状態を変える操作なので authorizeMutation を通す。
// 契約: 注入 closure は「実行中(busy)」以外の error を返さない — handler は app を
// import できず error を一律 409 に写像するため。破るなら cmd 側で分類してから渡す。
// WithGoNoGoRun は cmd が closure で注入する(handler は app / adapter を import しない)。
func (h *Handler) WithGoNoGoRun(f func() ([]string, error)) *Handler {
	h.gonogoRun = f
	return h
}

func (h *Handler) postGoNoGoRun(w http.ResponseWriter, r *http.Request) {
	if h.gonogoRun == nil {
		http.Error(w, "gonogo run is not wired", http.StatusServiceUnavailable)
		return
	}
	syms, err := h.gonogoRun()
	switch {
	case errors.Is(err, port.ErrGoNoGoRunning):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if syms == nil {
		syms = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	if len(syms) == 0 {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusAccepted)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"started": len(syms) > 0, "symbols": syms})
}

func (h *Handler) postAdvisorTrigger(w http.ResponseWriter, r *http.Request) {
	if h.advisorTrigger == nil {
		http.Error(w, "advisor is disabled (bot_config advisor_v2.enabled)", http.StatusServiceUnavailable)
		return
	}
	if err := h.advisorTrigger(); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"started": true})
}

func writeJSON(w http.ResponseWriter, v any, err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
