// Package broker holds the brokerage adapters. 立花 e支店 v4r10 の wire 仕様は docs/runtime/TACHIBANA_API_NOTES.md(v4r9 は 2026-09-27 廃止)。
package broker

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

// 守りは ocoVerified で fail-close: 逆指値の発動方向・常駐・bot 停止下の約定を人間がデモ環境で
// 実証するまで PlaceSettleOCO が error を返し、entry saga が巻き戻る(裸の実弾建玉を作らない)。
type Tachibana struct {
	env      string // "production" | "demo"
	ver      string // API 版。authBase も秘匿パターンも起動ログも**ここから引く**
	host     string
	authID   string          // sAuthId。デモ/本番で別セット
	privKey  *rsa.PrivateKey // login 応答の暗号化仮想URL(=セッション)を復号する
	secondPW string          // 第2パスワード。発注 / 取消で必須
	taxKubun string          // 譲渡益課税区分 (既定 特定 "1"; login 応答で更新)

	ocoVerified   bool
	marginEnabled bool // 一般信用: 信用建玉 / 維持率も引く
	clock         clock.Clock
	httpClient    *http.Client
	authBase      string

	// 立花の 1 日の利用回数上限を守るため、この天井を外して起動する経路を作らない。
	limiter *rateLimiter

	// 相手のサーバから見た送信回数。見積りは黙って古くなる。詳細: docs/runtime/TACHIBANA_API_NOTES.md §1.5-6
	apiRequests atomic.Int64
	// プロセスを跨いで残る数え口(立花と同じ集計単位)。nil 可 — paper / テストでは挿さない。
	usage atomic.Pointer[UsageRecorder]

	// ログ秘匿 literal(仮想URL 5 本 + そのパス + 第2パスワード)。新しい世代が先頭。
	// 🛑 session と**別に** atomic で持つ: redactBody に至る経路で t.mu の保持状況が
	// 揃わないため(理由は tachibana_transport.go の redactBody を読むこと)。
	// スライスは毎回作り直して Store する = 読み手はロック無しで読める。
	redactLits atomic.Pointer[[]string]
	// 構築時の秘密(第 2 パスワード)。世代の FIFO(上限 32 本)から**押し出さない**
	// (1 回しか登録しないと常に最古 = 4 回目の login で落ちる)。
	// 構築後は読むだけ。
	pinnedLits []string

	// refreshMu はセッションの張り直し(logout+login)を **1 本に絞る**。
	// 🚨 トークン更新ループと p_errno=2 を見たリクエストが
	// 同時に logout+login を撃つと、**互いの新しいセッションを logout して殺す**。
	// 🛑 t.mu とは別のロック。t.mu は 1 リクエストの送信中ずっと握られるので、
	// そこに張り直しを相乗りさせると照会 1 本ごとに更新が直列化して詰まる。
	refreshMu sync.Mutex
	// p_errno=2 を見たリクエストによる張り直しの間隔(refreshMu で守る)。
	// 🚨 立花はログインを 1 日 1 回に留めるよう求めている。張り直しても死んだままのとき
	// 次のリクエストがまた張り直すと、照会の本数ぶんログインを撃つ。
	reloginNotBefore time.Time
	reloginBackoff   time.Duration
	lastRelogin      time.Time

	mu        sync.Mutex
	batchSize int // 時価一括の1リクエスト銘柄数 (0 = defaultQuoteBatchSize)
	session   *tachiSession
	// sessionGeneration は login 成功のたびに前へ進む番号。「自分が使ったセッションは
	// まだ現役か」を判定する唯一の手段(仮想URL は秘匿値なので比較に使わない)。
	sessionGeneration uint64
	lastLoginAt       time.Time         // 直近の**成功した** login の時刻(日次の張り直しが読む)
	updateNotice      APIUpdateNotice   // 直近 login 応答の予定日告知(tachibana_notice.go)
	eigyouDay         map[string]string // orderID -> 営業日 (cancel/detail が要るが port に引数が無い)
	settleLegs        map[string]string // brokerPositionID -> 逆指値の注文番号 (leg は分割されない)
}

// 論理呼び出しではなく分割後の本数・login・再送を全部含む(doGET で加算)。
func (t *Tachibana) APIRequests() int64 { return t.apiRequests.Load() }

// UsageRecorder は wire 1 本ぶんの記録先。**adapter は具体実装を知らない**
// (permanent なカウンタは cmd 側で組み立てて挿す)。
type UsageRecorder interface{ Record(clmid string) }

// SetUsageRecorder は永続カウンタを挿す。プロセス内 atomic は make stop/start で
// ゼロに戻るので、1日の実数を言うにはプロセスを跨いで残る記録先が要る。
func (t *Tachibana) SetUsageRecorder(r UsageRecorder) {
	if r == nil {
		t.usage.Store(nil)
		return
	}
	t.usage.Store(&r)
}

func (t *Tachibana) recordUsage(clmid string) {
	if p := t.usage.Load(); p != nil {
		(*p).Record(clmid)
	}
}

// defaultTachibanaAPIVersion は叩く API 版の**唯一の真実源**(v4r9 は 2026-09-27 廃止)。
// URL も起動ログもログ秘匿の版パターンも、この定数(または t.ver / APIVersion())
// から引く — 版の文字列を 2 箇所に書ける状態を作らない。
//
// 🛑 **この行の値を変えるのが本番切替**。
// 切戻しは同じ 1 行を旧版に戻すだけ(旧版は 2026-09-27 まで生きている)。
const defaultTachibanaAPIVersion = "v4r10"

// tachibanaAuthBase は認証専用 URL を版から組む唯一の関数。
// 本番 https://kabuka.e-shiten.jp/e_api_<ver>/auth/
// デモ https://demo-kabuka.e-shiten.jp/e_api_<ver>/auth/
// (受入 A3 が prod Go の版付き URL literal をコメント込みで grep するので、綴りを焼かない)
func tachibanaAuthBase(host, ver string) string {
	return fmt.Sprintf("https://%s/e_api_%s/auth/", host, ver)
}

// APIVersion は**いま叩いている** API 版。起動ログとログ秘匿の版パターンはここから
// 引く(定数を各所へ写すと、版を上げたときに写した先だけ取り残される)。
func (t *Tachibana) APIVersion() string { return t.ver }

// APIEnv は NewTachibana に渡された env を**そのまま**返す("prod" のような typo も
// 丸めない)。fail-close で実際にどちらへ倒れたかは APIHost が持つ。
func (t *Tachibana) APIEnv() string { return t.env }

// APIHost は fail-close 解決**後**の接続先ホスト。APIEnv と対で 1 行に出すと
// 「env に prod と書いたのにデモへ倒れていた」がログだけで読める。
func (t *Tachibana) APIHost() string { return t.host }

var tachibanaAPIVersionRe = regexp.MustCompile(`^v4r[0-9]{1,2}$`)

// UseAPIVersion は API 版を差し替える。🛑 **平行リリース期間に v4r10 を実測するための
// probe 専用の口**で、bot の配線(cmd/stockbot)からは呼ばない(guard test が機械照合)。
// ログイン後の差し替えは拒否する — 仮想URL は版ごとに別セッションなので、
// 差し替えると「新しい版の authBase に古い版のセッション」という状態が作れてしまう。
func (t *Tachibana) UseAPIVersion(ver string) error {
	if !tachibanaAPIVersionRe.MatchString(ver) {
		return fmt.Errorf("tachibana: 版の形式が不正: %q(v4rNN)", ver)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.session != nil {
		return fmt.Errorf("tachibana: ログイン後に API 版は差し替えられない")
	}
	t.ver = ver
	t.authBase = tachibanaAuthBase(t.host, ver)
	return nil
}

// 起動時に RefreshToken を1回呼んで login する。env "demo" はデモホスト、それ以外は本番。
// authID / privKey はデモと本番で別セット。ocoVerified は STOCKBOT_TACHIBANA_OCO_VERIFIED=1 から
// 配線する人間ゲート。marginEnabled は GetPositions に 信用建玉 を、GetAccountMargin に実 維持率 を足す。
func NewTachibana(env, authID string, privKey *rsa.PrivateKey, secondPW string, ocoVerified, marginEnabled bool, c clock.Clock) *Tachibana {
	if c == nil {
		c = clock.System()
	}
	// fail-close: env=="production" 完全一致のときだけ本番ホスト(未設定・typo は必ずデモ側へ)。
	host := "demo-kabuka.e-shiten.jp"
	if env == "production" {
		host = "kabuka.e-shiten.jp"
	}
	t := &Tachibana{
		env: env, ver: defaultTachibanaAPIVersion, host: host,
		authID: authID, privKey: privKey, secondPW: secondPW, taxKubun: tachiTaxSpecific,
		ocoVerified: ocoVerified, marginEnabled: marginEnabled,
		clock: c, httpClient: &http.Client{Timeout: 30 * time.Second},
		eigyouDay: map[string]string{}, settleLegs: map[string]string{},
		// 既定で必ず制限(fail-safe): 設定漏れは過小呼び出しへ倒す。
		limiter: newRateLimiter(DefaultTachibanaRPS, DefaultTachibanaBurst, c),
	}
	// 🛑 authBase は**組み立て済みの t.ver / t.host から組む**。版リテラルを 2 箇所に
	// 置かないので、「ver だけ直して authBase が旧版のまま」が構造的に起きない。
	t.authBase = tachibanaAuthBase(t.host, t.ver)
	// 第2パスワードは login を待たずに秘匿対象へ入れる。非200 のゲートウェイは要求クエリ
	// ごと本文に echo し、クエリには発注時 sSecondPassword が**平文で**載る(JSON を
	// Shift-JIS + percent-encode しても ASCII 英数字はそのまま残る)。相対パスで echo
	// されると absURLInBody は当たらない = 正規表現では原理的に塞げない。
	t.pinnedLits = []string{secondPW}
	t.rememberRedactLiterals(secondPW)
	return t
}

// 銘柄数に依らない天井。根拠と実績は docs/runtime/TACHIBANA_API_NOTES.md §1.5。
const (
	DefaultTachibanaRPS   = 2.0
	DefaultTachibanaBurst = 10
)

// rps<=0 は無制限。数回しか叩かない probe/test 専用で、ポーリングする bot では使わない。
func (t *Tachibana) SetRateLimit(rps float64, burst int) {
	t.limiter = newRateLimiter(rps, burst, t.clock)
}

// 壊れた鍵を初回 login まで遅らせず起動時に落とすため、配線から呼ぶ。
func ParseTachibanaPrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("tachibana: no PEM block found in private key")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	ki, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("tachibana: parse RSA private key (tried PKCS#1 and PKCS#8): %w", err)
	}
	rk, ok := ki.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("tachibana: private key is %T, not RSA", ki)
	}
	return rk, nil
}

// 立花の数値は文字列。'*' / '-' / 空 / 解釈不能は欠損として 0 に倒す。
type sfloat float64

func (s *sfloat) UnmarshalJSON(b []byte) error {
	str := strings.TrimSpace(strings.Trim(string(b), `"`))
	if str == "" || str == "*" || str == "-" {
		*s = 0
		return nil
	}
	f, err := strconv.ParseFloat(str, 64)
	if err != nil {
		*s = 0
		return nil
	}
	*s = sfloat(f)
	return nil
}

func (s sfloat) f() float64 { return float64(s) }

// 0件のとき配列でなく文字列で返る wire 仕様。詳細: docs/runtime/TACHIBANA_API_NOTES.md §4.5
type slist[T any] []T

func (l *slist[T]) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" || trimmed == "null" {
		*l = nil
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal([]byte(trimmed), &s); err != nil {
			return err
		}
		switch strings.TrimSpace(s) {
		case "", "*", "-":
			*l = nil
			return nil
		}
		return fmt.Errorf("tachibana: list field encoded as non-empty string %q", s)
	}
	var v []T
	if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
		return err
	}
	*l = v
	return nil
}

type zanKaiKanougakuResp struct {
	commonResp
	ResultCode            string `json:"sResultCode"`
	SummaryGenkabuKaituke sfloat `json:"sSummaryGenkabuKaituke"` // 株式現物買付可能額
	HusokukinHasseiFlg    string `json:"sHusokukinHasseiFlg"`    // 不足金発生フラグ
}

// ⚠ CLM 名・フィールド名とも未裏取り。marginEnabled=true で使う前にデモ確認。詳細: docs/runtime/TACHIBANA_API_NOTES.md §7
type hosyoukinRituResp struct {
	commonResp
	ResultCode     string `json:"sResultCode"`
	ItakuHosyoukin sfloat `json:"sItakuHosyoukinRitu"` // 委託保証金維持率(%)
}

// 現物買付可能額を available funds として返す。MarginRatio は現物なら 1.0、一般信用有効時は実 委託保証金維持率
// (維持率ブレーカーが割れで落ちる)。⚠ Equity は買付可能額での近似 — 正確な純資産には評価額合計が要る。
func (t *Tachibana) GetAccountMargin(ctx context.Context) (*order.AccountMargin, error) {
	var r zanKaiKanougakuResp
	if err := t.request(ctx, urlRequest, tachiCLMKanougaku, map[string]string{}, &r); err != nil {
		return nil, err
	}
	if err := businessErr("ZanKaiKanougaku", r.PErrNo, r.ResultCode); err != nil {
		return nil, err
	}
	avail := r.SummaryGenkabuKaituke.f()
	// 不足金発生中は買付余力ゼロとして報告し、発注前 collateral gate で新規を止める。
	if r.HusokukinHasseiFlg == "1" {
		avail = 0
	}
	out := &order.AccountMargin{AvailableJPY: avail, Equity: avail}
	ratio := 1.0 // 現物 / flat は健全。ブレーカーは 0 < ratio < threshold でのみ落ちる
	if t.marginEnabled {
		// 🛑 **信用の可否は現物余力では判定できない**。CLMZanKaiKanougaku が返すのは
		// 現株買付可能額だけなので、信用新規建可能額はこちらで引く。
		var kr shinkiKanoIjirituResp
		if err := t.request(ctx, urlRequest, tachiCLMShinkiKanoIjiritu, map[string]string{}, &kr); err != nil {
			return nil, err // fail-close: 建余力が見えないまま信用を回さない
		}
		if kr.PErrNo != tachiErrNoData {
			if err := businessErr("ZanShinkiKanoIjiritu", kr.PErrNo, kr.ResultCode); err != nil {
				return nil, err
			}
			out.MarginNewJPY = kr.SinyouSinkidate.f()
			out.MarginCall = kr.OisyouKakuteiFlg == "1"
			if out.MarginCall {
				// 追証中は建て増さない。維持率 trip とは別軸の停止条件。
				out.MarginNewJPY = 0
			}
		}
		var hr hosyoukinRituResp
		if err := t.request(ctx, urlRequest, tachiCLMHosyoukinRitu, map[string]string{}, &hr); err != nil {
			return nil, err // fail-close: 維持率が見えないまま信用を回さない
		}
		if hr.PErrNo != tachiErrNoData {
			if err := businessErr("ZanRealHosyoukinRitu", hr.PErrNo, hr.ResultCode); err != nil {
				return nil, err
			}
			if v := hr.ItakuHosyoukin.f(); v > 0 { // 0/空 = 建玉なし
				ratio = v / 100.0
			}
		}
	}
	out.MarginRatio = ratio
	return out, nil
}

// 建余力＆本日維持率(CLMZanShinkiKanoIjiritu)。
type shinkiKanoIjirituResp struct {
	commonResp
	ResultCode       string `json:"sResultCode"`
	SinyouSinkidate  sfloat `json:"sSummarySinyouSinkidate"` // 信用新規建可能額
	Itakuhosyoukin   sfloat `json:"sItakuhosyoukin"`         // 委託保証金率(%)
	OisyouKakuteiFlg string `json:"sOisyouKakuteiFlg"`       // 追証 0:未確定 1:確定
}

type genbutuKabuListResp struct {
	commonResp
	ResultCode string `json:"sResultCode"`
	List       slist[struct {
		IssueCode          string `json:"sUriOrderIssueCode"`
		ZyoutoekiKazeiC    string `json:"sUriOrderZyoutoekiKazeiC"`
		ZanKabuSuryou      sfloat `json:"sUriOrderZanKabuSuryou"`
		UritukeKanouSuryou sfloat `json:"sUriOrderUritukeKanouSuryou"`
		GaisanBokaTanka    sfloat `json:"sUriOrderGaisanBokaTanka"`
	}] `json:"aGenbutuKabuList"`
}

type shinyouTateResp struct {
	commonResp
	ResultCode string `json:"sResultCode"`
	List       slist[struct {
		TategyokuNumber string `json:"sOrderTategyokuNumber"`
		IssueCode       string `json:"sOrderIssueCode"`
		BaibaiKubun     string `json:"sOrderBaibaiKubun"`
		TategyokuSuryou sfloat `json:"sOrderTategyokuSuryou"`
		TategyokuTanka  sfloat `json:"sOrderTategyokuTanka"`
	}] `json:"aShinyouTategyokuList"`
}

// 現物は常に、一般信用有効時は 信用建玉 も返す(Reconcile が信用建玉を見落として誤 trip しないように)。
// 現物には建玉番号が無いので id = genbutu:code:課税区分、信用は shinyo:code
// (ナンピン禁止で1建玉=一意。PlaceOrder が合成する id と同じキー)。
func (t *Tachibana) GetPositions(ctx context.Context) ([]port.BrokerPosition, error) {
	var out []port.BrokerPosition

	var r genbutuKabuListResp
	if err := t.request(ctx, urlRequest, tachiCLMGenbutu, map[string]string{}, &r); err != nil {
		return nil, err
	}
	if r.PErrNo != tachiErrNoData { // noData は「建玉なし」= 正常な空
		if err := businessErr("GenbutuKabuList", r.PErrNo, r.ResultCode); err != nil {
			return nil, err
		}
		for _, p := range r.List {
			if p.ZanKabuSuryou.f() <= 0 {
				continue
			}
			out = append(out, port.BrokerPosition{
				BrokerPositionID: "genbutu:" + p.IssueCode + ":" + p.ZyoutoekiKazeiC,
				Symbol:           p.IssueCode,
				Side:             order.SideBuy,
				Quantity:         int(p.ZanKabuSuryou.f()),
				EntryPrice:       p.GaisanBokaTanka.f(),
				ExecKind:         order.ExecCash,
			})
		}
	}

	if t.marginEnabled {
		mp, err := t.shinyoPositions(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, mp...)
	}
	return out, nil
}

func (t *Tachibana) shinyoPositions(ctx context.Context) ([]port.BrokerPosition, error) {
	var r shinyouTateResp
	if err := t.request(ctx, urlRequest, tachiCLMShinyouTate, map[string]string{}, &r); err != nil {
		return nil, err
	}
	if r.PErrNo == tachiErrNoData {
		return nil, nil
	}
	if err := businessErr("ShinyouTategyokuList", r.PErrNo, r.ResultCode); err != nil {
		return nil, err
	}
	out := make([]port.BrokerPosition, 0, len(r.List))
	for _, p := range r.List {
		if p.TategyokuSuryou.f() <= 0 {
			continue
		}
		out = append(out, port.BrokerPosition{
			BrokerPositionID: shinyoPositionID(p.IssueCode),
			Symbol:           p.IssueCode,
			Side:             baibaiToSide(p.BaibaiKubun),
			Quantity:         int(p.TategyokuSuryou.f()),
			EntryPrice:       p.TategyokuTanka.f(),
			// 立花の信用建玉一覧は制度/一般を区別して返さない。口座で通るのは制度信用
			// なのでそちらに寄せる。返済時の区分もこれで決まる。
			ExecKind: order.ExecMarginSystem,
		})
	}
	// 🚨 **同一 ID を合算してから返す。** 立花は建玉ごとに 1 行返すのに、こちらの ID は
	// shinyo:<コード> で銘柄単位に潰れる。ナンピン禁止の bot 単体なら 1 銘柄 1 建玉だが、
	// **人間が同じ銘柄に複数建てると前提が崩れる**。
	// 生のまま返すと呼び手が map[ID] に入れた時点で最後の 1 行以外が消え、数量も建値も
	// 実態とずれる — reconcile はその値で PnL を確定し、entry saga は孤児判定に使う。
	return aggregateByPositionID(out), nil
}

// aggregateByPositionID は同じ (BrokerPositionID, Side) の建玉を 1 件に畳む。
// 数量は合計、建値は**数量加重平均**(合算した建玉を 1 本として扱う以上、平均建値でしか
// 損益を表現できない)。売建と買建は畳まない — 畳むと反対売買になる。
// 出力は ID + Side の昇順で決定論。
func aggregateByPositionID(in []port.BrokerPosition) []port.BrokerPosition {
	type key struct {
		id   string
		side order.Side
	}
	agg := make(map[key]*port.BrokerPosition, len(in))
	keys := make([]key, 0, len(in))
	for _, p := range in {
		k := key{id: p.BrokerPositionID, side: p.Side}
		cur, ok := agg[k]
		if !ok {
			cp := p
			agg[k] = &cp
			keys = append(keys, k)
			continue
		}
		total := cur.Quantity + p.Quantity
		if total > 0 {
			cur.EntryPrice = (cur.EntryPrice*float64(cur.Quantity) + p.EntryPrice*float64(p.Quantity)) / float64(total)
		}
		cur.Quantity = total
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].id != keys[j].id {
			return keys[i].id < keys[j].id
		}
		return keys[i].side < keys[j].side
	})
	out := make([]port.BrokerPosition, 0, len(keys))
	for _, k := range keys {
		out = append(out, *agg[k])
	}
	return out
}

func businessErr(clm, pErrno, resultCode string) error {
	if pErrno != "" && pErrno != tachiErrOK {
		return fmt.Errorf("tachibana %s: p_errno=%s", clm, pErrno)
	}
	if resultCode != "" && resultCode != tachiErrOK {
		return fmt.Errorf("tachibana %s: result=%s", clm, resultCode)
	}
	return nil
}

func sideKubun(s order.Side) string {
	if s == order.SideSell {
		return tachiBaibaiSell
	}
	return tachiBaibaiBuy
}

var (
	_ port.Broker     = (*Tachibana)(nil)
	_ port.LiveBroker = (*Tachibana)(nil)
)
