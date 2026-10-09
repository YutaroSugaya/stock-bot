package broker

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/transform"

	"stockbot/backend/internal/domain/clock"
)

// Shift-JIS GET エンベロープ / セッション(仮想URL + 単調 p_no)/ login。詳細: docs/runtime/TACHIBANA_API_NOTES.md §1-2

// CLM id と enum の出典は docs/runtime/TACHIBANA_API_NOTES.md §3(立花 enum.go)。
const (
	tachiCLMLogin              = "CLMAuthLoginRequest"
	tachiCLMLoginAck           = "CLMAuthLoginAck"
	tachiCLMLogout             = "CLMAuthLogoutRequest"
	tachiCLMNewOrder           = "CLMKabuNewOrder"
	tachiCLMCancel             = "CLMKabuCancelOrder"
	tachiCLMGenbutu            = "CLMGenbutuKabuList"
	tachiCLMKanougaku          = "CLMZanKaiKanougaku"
	tachiCLMOrderList          = "CLMOrderList"
	tachiCLMOrderListDetail    = "CLMOrderListDetail"
	tachiCLMMarketPrice        = "CLMMfdsGetMarketPrice"
	tachiCLMMarketPriceHistory = "CLMMfdsGetMarketPriceHistory"
	tachiCLMSystemStatus       = "CLMSystemStatus"
	tachiCLMShinyouTate        = "CLMShinyouTategyokuList" // 信用建玉一覧
	tachiCLMHosyoukinRitu      = "CLMZanRealHosyoukinRitu" // 実質保証金率(維持率)
	// 建余力＆本日維持率。信用新規建可能額 + 委託保証金率 + **追証フラグ**が 1 本で取れる。
	tachiCLMShinkiKanoIjiritu = "CLMZanShinkiKanoIjiritu"

	// sOrderYakuzyouStatus (T2 裏取り): 0=未約定 / 1=一部約定 / 2=全部約定 / 3=約定中。working は 2 以外。
	tachiYakuzyouUnfilled  = "0"
	tachiYakuzyouPartial   = "1"
	tachiYakuzyouAll       = "2"
	tachiYakuzyouExecuting = "3"

	tachiJsonOfmt = "6" // Wrapped(2) | WordKey(4): field-name keys required

	// enum values (docs §3)
	tachiSizyouToushou = "00" // 市場: 東証
	tachiBaibaiSell    = "1"  // 売買区分: 売
	tachiBaibaiBuy     = "3"  // 売買区分: 買
	tachiConditionNone = "0"  // 執行条件: 指定なし
	// 🛑 逆指値の「指定なし」センチネルは**項目ごとに違う**。
	// sGyakusasiZyouken(逆指値条件)は **"0"**、sGyakusasiPrice(逆指値値段)は "*"。
	// 両方 "*" を送って「逆指値条件に誤りがあります」で拒否された(4751)。
	tachiGyakusasiZyoukenNone = "0" // 逆指値条件: 指定なし

	// 注文期日。仕様は「0:当日 / 上記以外は YYYYMMDD [10営業日迄]」。
	// 公式サンプルは信用注文を含め**全例が "0"** なので、日付指定は実機未検証。
	tachiExpireToday = "0"
	// 訂正注文で「変更なし」を表すセンチネル(一次資料: e-shiten API リファレンス
	// CLMKabuCorrectOrder。全項目必須で、変えない項目は "*")。
	tachiCorrectNoChange  = "*"
	tachiExpireDayLayout  = "20060102"
	tachiGenbutu          = "0" // 現金信用区分: 現物
	tachiGenkinMarginNew  = "6" // 現金信用区分: 一般信用新規(6ヶ月) — **口座で拒否された**
	tachiGenkinSystemNew  = "2" // 現金信用区分: 制度信用新規(6ヶ月) — 立花で実際に通る区分
	tachiGenkinSystemExit = "4" // 現金信用区分: 制度信用返済(6ヶ月)
	tachiGenkinMarginExit = "8" // 現金信用区分: 一般信用返済(6ヶ月)
	tachiTatebiByDate     = "2" // 建日種類: 建日順(FIFO・ナンピン禁止で1建玉=一意)
	tachiGyakusasiNone    = "0" // 逆指値注文種別: 通常
	tachiGyakusasiStop    = "1" // 逆指値注文種別: 逆指値
	tachiGyakusasiDouble  = "2" // 逆指値注文種別: 通常+逆指値(OCO)
	tachiTaxSpecific      = "1" // 譲渡益課税区分: 特定
	tachiOrderPriceMarket = "0" // 注文値段: 成行
	tachiUnspecified      = "*" // 各種「指定なし」

	tachiErrOK              = "0" // p_errno: 問題なし
	tachiErrNoData          = "1" // p_errno: データ無し (空リストは正常)
	tachiErrSessionInactive = "2" // p_errno: 無効なセッション → 再ログイン
)

// 仮想URL は 1 本ごとがセッショントークンそのもの(ログに出さない)。
type tachiSession struct {
	requestURL string
	masterURL  string
	priceURL   string
	eventURL   string
	eventWSURL string // 叩いていない leg。**秘匿対象の網羅**のために持つ(urlFor に case は足さない)
	lastNo     int64
}

type urlKind int

const (
	urlRequest urlKind = iota
	urlPrice
	urlMaster
)

func (s *tachiSession) urlFor(k urlKind) string {
	switch k {
	case urlPrice:
		return s.priceURL
	case urlMaster:
		return s.masterURL
	default:
		return s.requestURL
	}
}

type commonResp struct {
	PNo    string `json:"p_no"`
	PErrNo string `json:"p_errno"`
	PErr   string `json:"p_err"`
	CLMID  string `json:"sCLMID"`
}

func (c commonResp) errno() string { return c.PErrNo }

type errNoCarrier interface{ errno() string }

var (
	errTachiNoSession = fmt.Errorf("tachibana: not logged in")
	errTachiLogin     = fmt.Errorf("tachibana: login failed")
	// p_errno=2 の張り直しが間隔の中で見送られた(ログインを撃たずに照会を失敗させる)。
	errTachiReloginThrottled = fmt.Errorf("tachibana: re-login throttled (立花への高負荷を避けるため張り直しの間隔を空けている)")
)

// サーバ時刻と ±30秒ずれると p_errno=8。ホスト時計は NTP 同期必須。
func (t *Tachibana) sdDate() string {
	return t.clock().Format("2006.01.02-15:04:05.000")
}

func (t *Tachibana) authURL() string { return t.authBase }

// API は全フィールド文字列型で、行きも帰りも Shift-JIS。
func (t *Tachibana) doGET(ctx context.Context, uri string, fields map[string]string) ([]byte, error) {
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	sjis, _, err := transform.Bytes(japanese.ShiftJIS.NewEncoder(), raw)
	if err != nil {
		return nil, fmt.Errorf("tachibana: shift-jis encode: %w", err)
	}
	u, err := url.Parse(uri)
	if err != nil {
		return nil, redactTransportErr(fields["sCLMID"], err)
	}
	u.RawQuery = url.QueryEscape(string(sjis))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, redactTransportErr(fields["sCLMID"], err)
	}
	// 送る直前に数える(Do 成功後だと届いたのに手元で失敗した送信が落ちて過少申告になる)。詳細: docs/runtime/TACHIBANA_API_NOTES.md §1.5-6
	// 2 つのカウンタは同じ 1 行で動かす: 数え口が分かれると定義がずれて、また
	// 「bot 側の積み上げが broker 側の実績を説明できない」に戻る(説明できない数千回の差)。
	t.apiRequests.Add(1)
	t.recordUsage(fields["sCLMID"])
	res, err := t.httpClient.Do(req)
	if err != nil {
		return nil, redactTransportErr(fields["sCLMID"], err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, redactTransportErr(fields["sCLMID"], err)
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tachibana %s: http %d: %s", fields["sCLMID"], res.StatusCode, t.redactBody(body))
	}
	// 復号は失敗しない
	utf8, _, _ := transform.Bytes(japanese.ShiftJIS.NewDecoder(), body)
	return utf8, nil
}

// gateway/proxy は要求パスを本文にそのまま echo する。そのパスがセッショントークン。
//
// 🛑 版番号を正規表現に焼かない。v4r10 マニュアルは仮想URL のフォーマットを保証しない
// (「意味のない文字列」)ので、prefix を前提にした秘匿は**それ単体では原理的に弱い**。
// 版非依存の正規表現は安い保険にすぎず、確実な手段は下の literal 秘匿。
var (
	absURLInBody  = regexp.MustCompile(`https?://[^\s"'<>]+`)
	apiPathInBody = regexp.MustCompile(`(?i)/e_api_v[0-9a-z]+/[^\s"'<>]*`)
)

// 秘匿 literal の下限長。空文字を ReplaceAll すると全文字の間に置換文字列が挟まって
// 本文が壊れる(login は sUrlEvent に "" を返すことがある)。短すぎる literal も無関係な
// 場所を潰すだけで益がない。🛑 過剰秘匿はログが読みにくくなるだけ / 過少秘匿は秘密が漏れる。
// 迷ったら潰す側へ倒す。
const minRedactLiteralLen = 4

// 何本まで literal を覚えるか。再ログイン直後は**古い**仮想URL を載せた要求がまだ飛んで
// いることがあるので、入れ替えた瞬間に前世代を捨てない(fail-close)。1 世代 = 最大 11 本
// (仮想URL 5 本 + そのパス 5 本 + 第2パスワード)。
const maxRedactLiterals = 32

// rememberRedactLiterals は秘匿対象の実文字列を登録する。新しい世代を先頭に置き、重複を
// 落とし、上限を超えたぶんを**古い側から**捨てる(= 保存順は世代順のまま。長さ順に並べ替え
// るのは redactBodyWith の中で、そこでコピーしてから)。絶対URL は**パス単体**でも登録する
// (scheme と host を落としてパスだけ echo する 404 では、絶対URL の literal は 1 文字も
// 当たらない)。
//
// 🛑 スライスは毎回作り直して Store するだけで、**公開済みのスライスを書き換えない**。
// これで読み手(redactBody)は t.mu もロックも無しで安全に読める。
func (t *Tachibana) rememberRedactLiterals(vals ...string) {
	var old []string
	if p := t.redactLits.Load(); p != nil {
		old = *p
	}
	next := make([]string, 0, 2*len(vals)+len(old))
	seen := make(map[string]bool, 2*len(vals)+len(old))
	add := func(s string) {
		if len([]rune(s)) < minRedactLiteralLen || seen[s] {
			return
		}
		seen[s] = true
		next = append(next, s)
	}
	for _, v := range vals {
		add(v)
		if u, err := url.Parse(v); err == nil && u.Scheme != "" {
			add(u.Path)
		}
	}
	for _, v := range old {
		add(v)
	}
	if len(next) > maxRedactLiterals {
		next = next[:maxRedactLiterals] // 末尾 = 最も古い世代から捨てる
	}
	// 🛑 構築時の秘密は上限の外。切り詰めで落ちていたら戻す(順序は世代順のまま末尾)。
	for _, pin := range t.pinnedLits {
		if len([]rune(pin)) < minRedactLiteralLen || slices.Contains(next, pin) {
			continue
		}
		next = append(next, pin)
	}
	t.redactLits.Store(&next)
}

// redactBody は非200 応答の本文をログに載せる前に潰す。**メソッド**なのは、版非依存の
// 正規表現だけでは足りず(仮想URL のフォーマットは無保証)、login で確定した実文字列
// そのものを秘匿対象にする必要があるため。
//
// 🛑 並行性: ここで **t.mu を取らない**。redactBody に至る 3 経路で保持状況が揃わない:
// ① requestOnce → doGET は **t.mu を保持したまま**呼ぶ(ここで Lock すれば即デッドロック)
// ② login → doGET は t.mu を保持していない
// (③ だった streamMaster は S7 で削除済み。マスタ経路も t.request に載った)
// literal は atomic.Pointer で publish し、読み手はロック無しで読む。
func (t *Tachibana) redactBody(b []byte) string {
	var lits []string
	if p := t.redactLits.Load(); p != nil {
		lits = *p
	}
	return redactBodyWith(lits, b)
}

// 秘匿の順序は **literal が先、正規表現が後**。absURLInBody を先に走らせると、仮想URL に
// " や < が混ざっていた場合に前半だけが [redacted-url] に化け、**尾(= トークンの残り)が
// 平文で残ったまま** literal が当たらなくなる。切り詰めも秘匿の**後**(先に切ると literal が
// 途中で割れて当たらない)。
//
// literal 同士は**長い順**に当てる。短い literal が長い literal の部分文字列だと、先に短い方を
// 潰した瞬間に長い方が二度と当たらず、残り(= トークンの尾)が平文で出る。並べ替えは
// **コピーの上で**やる — 引数のスライスは atomic で publish 済みで、書き換えると読み手と競合する。
func redactBodyWith(lits []string, b []byte) string {
	const max = 256
	ordered := make([]string, 0, len(lits))
	for _, lit := range lits {
		if len([]rune(lit)) < minRedactLiteralLen {
			continue // 二重の歯止め: 空 literal の ReplaceAll は本文を壊す
		}
		ordered = append(ordered, lit)
	}
	sort.SliceStable(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	// 先に UTF-8 化してから切る(切り詰めが多バイト文字を割って文字化けしないように)。
	utf8, _, _ := transform.Bytes(japanese.ShiftJIS.NewDecoder(), b)
	s := string(utf8)
	for _, lit := range ordered {
		s = strings.ReplaceAll(s, lit, "[redacted-secret]")
	}
	s = absURLInBody.ReplaceAllString(s, "[redacted-url]")
	s = apiPathInBody.ReplaceAllString(s, "[redacted-path]")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…(truncated)"
	}
	return s
}

// Go は http/url の失敗を *url.Error で包み、その Error() に **URL 全体**を載せる。ここでの URL は
// セッション仮想パス(それ自体がセッショントークン)+ CLM ペイロード全体のクエリで、発注・取消では
// sSecondPassword を含む。呼び手は slog "err" で出すので、発注中の1回のタイムアウトだけで
// セッション + 第2パスワードが平文ログ1行に載る = 秘密鍵なしで発注できてしまう。operation と cause だけ残し、
// cause は包んだまま(errors.Is(ctx.Canceled / DeadlineExceeded) が再試行・停止判断で効き続けるように)。
func redactTransportErr(clmid string, err error) error {
	if clmid == "" {
		clmid = "?"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("tachibana %s: %s: %w", clmid, ue.Op, ue.Err)
	}
	return fmt.Errorf("tachibana %s: transport: %w", clmid, err)
}

// 呼び出し側は t.mu を保持していること(p_no を進めるため)。
func (t *Tachibana) envelope(clmid string, fields map[string]string) map[string]string {
	t.session.lastNo++
	m := map[string]string{
		"p_no":      strconv.FormatInt(t.session.lastNo, 10),
		"p_sd_date": t.sdDate(),
		"sCLMID":    clmid,
		"sJsonOfmt": tachiJsonOfmt,
	}
	for k, v := range fields {
		m[k] = v
	}
	return m
}

// p_errno=2 は1回だけ再ログイン再送。なお 2 なら業務 reject に化けさせず auth error にする。
//
// 🚨 **自分で無条件に張り直さない**。別の経路(日次の張り直し・
// 並行する照会)が張り直したばかりのセッションを logout して殺すと、「再ログインしても
// まだ無効」になる(p_errno=2 が連発する形)。`refreshSession` は **自分の
// 使った世代がまだ現役のときだけ**、しかも間隔を空けて張り直す(立花はログインを 1 日 1 回に留めるよう求めている)。
func (t *Tachibana) request(ctx context.Context, kind urlKind, clmid string, fields map[string]string, out any) error {
	gen, err := t.requestOnce(ctx, kind, clmid, fields, out)
	if err != nil {
		return err
	}
	if c, ok := out.(errNoCarrier); ok && c.errno() == tachiErrSessionInactive {
		if err := t.refreshSession(ctx, gen); err != nil {
			return err
		}
		if _, err := t.requestOnce(ctx, kind, clmid, fields, out); err != nil {
			return err
		}
		if c2, ok := out.(errNoCarrier); ok && c2.errno() == tachiErrSessionInactive {
			return fmt.Errorf("tachibana %s: session still inactive after re-login (p_errno=2)", clmid)
		}
	}
	return nil
}

// sessionGen は現在のセッションの世代。0 = 未ログイン。
func (t *Tachibana) sessionGen() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionGeneration
}

// refreshSession は **seenGen のセッションがまだ現役なら**張り直す。
//
// 🛑 待っている間に誰かが張り直していたら**何もしない**。ここで logout を撃つのが、
// 新しいセッションを殺していた経路そのもの。呼び手はそのまま retry すればよい
// (retry は新しいセッションで飛ぶ)。
func (t *Tachibana) refreshSession(ctx context.Context, seenGen uint64) error {
	t.refreshMu.Lock()
	defer t.refreshMu.Unlock()
	if t.sessionGen() != seenGen {
		return nil // 別の goroutine が先に張り直した
	}
	now := t.clock()
	if now.Before(t.reloginNotBefore) {
		return fmt.Errorf("%w until %s", errTachiReloginThrottled, t.reloginNotBefore.In(clock.JST).Format("15:04:05"))
	}
	if t.reloginBackoff == 0 || now.Sub(t.lastRelogin) > reloginQuietReset {
		t.reloginBackoff = reloginMinInterval
	} else {
		t.reloginBackoff = min(t.reloginBackoff*2, reloginMaxInterval)
	}
	t.lastRelogin = now
	t.reloginNotBefore = now.Add(t.reloginBackoff)
	return t.refreshLocked(ctx)
}

// p_errno=2 による張り直しの間隔。🚨 立花はログインを 1 日 1 回に留めるよう求めている
// (仮想URL は 1 日に 1 度取得すれば該当営業日は継続利用できる)。
// 張り直しても死んだまま(閉局中・同一 ID の別プロセスとの蹴り合い)のときは、間隔を
// 倍々に延ばす(天井 30 分)。1 時間張り直さずに済んでいれば最短から数え直す。
// 明示の RefreshToken(起動時・日次)はここを通らない。
const (
	reloginMinInterval = time.Minute
	reloginMaxInterval = 30 * time.Minute
	reloginQuietReset  = time.Hour
)

// p_no 採番と送信を t.mu で直列化する(送信順が p_no 順とずれると p_errno=6)。
//
// 返す世代は **実際に送信に使ったセッションのもの**。呼び手の外で捕まえると、捕まえてから
// 送信までの間に張り直しが挟まったとき「古い世代を見て新しいセッションを殺す」判定になる。
func (t *Tachibana) requestOnce(ctx context.Context, kind urlKind, clmid string, fields map[string]string, out any) (uint64, error) {
	// 待つのは t.mu を取る前(ロック下で待つと全 caller が1本の待ちに詰まる)。p_no は送信時にロック下で採番するので単調性は不変。
	if err := t.limiter.Wait(ctx); err != nil {
		return 0, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.session == nil {
		return 0, errTachiNoSession
	}
	gen := t.sessionGeneration
	uri := t.session.urlFor(kind)
	body, err := t.doGET(ctx, uri, t.envelope(clmid, fields))
	if err != nil {
		return gen, err
	}
	return gen, json.Unmarshal(body, out)
}

// sUrl* は RSA-OAEP(SHA-256)+base64 で暗号化されて届く(秘密鍵保持者だけが使える)。
type loginResp struct {
	commonResp
	ResultCode        string `json:"sResultCode"`
	URLRequest        string `json:"sUrlRequest"`
	URLMaster         string `json:"sUrlMaster"`
	URLPrice          string `json:"sUrlPrice"`
	URLEvent          string `json:"sUrlEvent"`
	URLEventWebSocket string `json:"sUrlEventWebSocket"`
	TaxKubun          string `json:"sZyoutoekiKazeiC"`
	KinsyouhouMidoku  string `json:"sKinsyouhouMidokuFlg"`
	// 予定日の告知(YYYYMMDD・未定は "" / "0")。判定式は APIUpdateDue(tachibana_notice.go)。
	UpdateInformAPISpec string `json:"sUpdateInformAPISpecFunction"`
	UpdateInformWebDoc  string `json:"sUpdateInformWebDocument"`
}

// base64 → RSA-OAEP(SHA-256・label なし)。空入力は空(未使用 leg)。詳細: docs/runtime/TACHIBANA_API_NOTES.md §1
func decryptVirtualURL(b64 string, key *rsa.PrivateKey) (string, error) {
	clean := strings.TrimSpace(strings.Trim(strings.TrimSpace(b64), `"`))
	if clean == "" {
		return "", nil
	}
	if key == nil {
		return "", fmt.Errorf("no private key configured")
	}
	ct, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		if ct, err = base64.RawStdEncoding.DecodeString(clean); err != nil {
			return "", fmt.Errorf("base64 decode: %w", err)
		}
	}
	pt, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, key, ct, nil)
	if err != nil {
		return "", fmt.Errorf("rsa-oaep decrypt: %w", err)
	}
	// 公式サンプルが utf-8-sig 相当なので先頭 BOM を落とす。
	return strings.TrimSpace(strings.TrimPrefix(string(pt), string(rune(0xFEFF)))), nil
}

// 公開鍵認証(認証 I/F は v4r9 / v4r10 で同一): 送るのは sAuthId だけ。認証の証明は仮想URL を復号して使えること。
func (t *Tachibana) login(ctx context.Context) error {
	// login は requestOnce を通らないので明示的に天井を通す(不安定時の refreshWithRetry が唯一の無制限経路になる)。
	if err := t.limiter.Wait(ctx); err != nil {
		return err
	}
	fields := map[string]string{
		"p_no":      "1",
		"p_sd_date": t.sdDate(),
		"sCLMID":    tachiCLMLogin,
		"sJsonOfmt": tachiJsonOfmt,
		"sAuthId":   t.authID,
	}
	body, err := t.doGET(ctx, t.authURL(), fields)
	if err != nil {
		return err
	}
	var r loginResp
	if err := json.Unmarshal(body, &r); err != nil {
		return fmt.Errorf("tachibana: parse login: %w", err)
	}
	// 金商法書面 未読(=1)は書面確認まで API 不可。公式サンプルもここで中断する。
	if r.PErrNo != tachiErrOK || r.ResultCode != tachiErrOK || r.KinsyouhouMidoku == "1" {
		return fmt.Errorf("%w (p_errno=%q result=%q midoku=%q: %s)", errTachiLogin, r.PErrNo, r.ResultCode, r.KinsyouhouMidoku, r.PErr)
	}
	reqURL, err := decryptVirtualURL(r.URLRequest, t.privKey)
	if err != nil {
		return fmt.Errorf("%w: decrypt sUrlRequest: %v", errTachiLogin, err)
	}
	priceURL, err := decryptVirtualURL(r.URLPrice, t.privKey)
	if err != nil {
		return fmt.Errorf("%w: decrypt sUrlPrice: %v", errTachiLogin, err)
	}
	masterURL, err := decryptVirtualURL(r.URLMaster, t.privKey)
	if err != nil {
		return fmt.Errorf("%w: decrypt sUrlMaster: %v", errTachiLogin, err)
	}
	eventURL, err := decryptVirtualURL(r.URLEvent, t.privKey)
	if err != nil {
		return fmt.Errorf("%w: decrypt sUrlEvent: %v", errTachiLogin, err)
	}
	// WebSocket 版 EVENT はまだ叩いていないが、**秘匿対象の網羅**のために復号だけする。
	// ここだけ best-effort(失敗しても login を落とさない)なのは、使っていない leg の
	// 復号失敗で bot 全体が起動不能になる方が危険だから。復号できなかった値は秘匿対象にも
	// 入れない(暗号文のままなら我々の要求 URL に載ることもない)。
	eventWSURL, wsErr := decryptVirtualURL(r.URLEventWebSocket, t.privKey)
	if wsErr != nil {
		eventWSURL = ""
	}
	if reqURL == "" || priceURL == "" {
		return fmt.Errorf("%w: login ok but virtual URLs missing (契約締結前書面 未読?)", errTachiLogin)
	}
	// 🛑 session を公開する**前に**秘匿 literal を登録する。逆順にすると、登録前に出た
	// 最初の 1 本が非200 を返した瞬間に仮想URL が平文でログに残る。
	// 🛑 t.session への代入と必ず同居させる(production の代入はここ 1 箇所だけ。
	// 2 箇所目を作ると literal 登録が黙って漏れる — 受入 ③ で本数を固定する)。
	t.rememberRedactLiterals(reqURL, priceURL, masterURL, eventURL, eventWSURL)
	t.mu.Lock()
	t.session = &tachiSession{
		requestURL: reqURL, masterURL: masterURL, priceURL: priceURL, eventURL: eventURL,
		eventWSURL: eventWSURL, lastNo: 1,
	}
	// 🛑 セッションを差し替えたら必ず世代を進める(この 1 行が「誰かが先に張り直したか」の
	// 唯一の根拠)。ここを忘れると refreshSession が常に「現役」と読み、同じ事故が戻る。
	t.sessionGeneration++
	t.lastLoginAt = t.clock()
	if r.TaxKubun != "" {
		t.taxKubun = r.TaxKubun
	}
	t.updateNotice = APIUpdateNotice{APISpecFunction: r.UpdateInformAPISpec, WebDocument: r.UpdateInformWebDoc}
	t.mu.Unlock()
	return nil
}

// LastLoginAt は直近の**成功した** login の時刻(ゼロ値 = 未ログイン)。日次の張り直しは
// これを見て「この集計窓でもう張ったか」を決める(p_errno=2 で張り直したぶんも含む)。
func (t *Tachibana) LastLoginAt() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastLoginAt
}

// best-effort。落としておかないと再ログインが同一 ID の古いセッションと衝突する。
func (t *Tachibana) logout(ctx context.Context) {
	t.mu.Lock()
	s := t.session
	t.mu.Unlock()
	if s == nil {
		return
	}
	var out commonResp
	_, _ = t.requestOnce(ctx, urlRequest, tachiCLMLogout, map[string]string{}, &out)
}

// 立花のセッションは引け / 03:30 / 再ログインで切れるので、ここは no-op にできない。
//
// 🛑 **張り直しは常に 1 本**。並行して撃つと、片方の logout が
// もう片方の login 直後のセッションを殺す。
func (t *Tachibana) RefreshToken(ctx context.Context) error {
	t.refreshMu.Lock()
	defer t.refreshMu.Unlock()
	return t.refreshLocked(ctx)
}

// refreshLocked は refreshMu を保持している呼び手のための本体。
func (t *Tachibana) refreshLocked(ctx context.Context) error {
	t.logout(ctx)
	return t.login(ctx)
}
