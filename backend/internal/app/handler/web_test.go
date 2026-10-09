package handler

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"stockbot/backend/internal/usecase/command"
	"stockbot/backend/internal/usecase/query"
)

// ダッシュボードは DTO の JSON キーを直に読む素の JS なので、Go 側でキー名を
// 変えても JS はコンパイルエラーにならず、画面が黙って '–' になるだけ。
// 実際にトレールアーム(TP を持たない bnf_reversion_trail)の
// 建玉 4本(285A/7735/6526/5803)が「出口が何も表示されない行」として並んだ。
// TP=0 の建玉の本当の出口は ratchet なので、そのキーが DTO と HTML の両方に
// 存在することを機械で縛る。
func TestDashboardHTMLConsumesRatchetKeys(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("embed 読み出し: %v", err)
	}
	html := string(b)

	dtoKeys := jsonKeys(query.OpenPositionView{})
	for _, key := range []string{
		"ratchet_arm_jpy", "ratchet_giveback_jpy", "peak_unrealized_jpy", "ratchet_armed",
		// 「守りの2脚のうちどちらが先に効くか」の答えは domain が出す。
		// JS がこれを読まずに自前で判定すると EvaluateExit から静かにずれる。
		"protective_exit_price", "protective_exit_reason",
	} {
		if !dtoKeys[key] {
			t.Errorf("OpenPositionView に json:%q が無い — トレール建玉の出口が API に出ない", key)
		}
		if !strings.Contains(html, key) {
			t.Errorf("dashboard が %q を読んでいない — TP=0 の建玉が出口不明で表示される", key)
		}
	}
}

// 未決済(いま持っている建玉)のトータル含み損益。決済済みの net 合計とは
// 別枠で出す — 実現(net)と未実現(含み)を1つの数字に混ぜると、まだ何も
// 確定していない評価益が「稼いだ額」に見える。
// 合計は建値(entry_price)× 株数(quantity)から作るので、そのキーが DTO と
// HTML の両方にあることと、マウント先の #openTotals が消えていないことを縛る。
func TestDashboardShowsOpenUnrealizedTotal(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("embed 読み出し: %v", err)
	}
	html := string(b)

	dtoKeys := jsonKeys(query.OpenPositionView{})
	for _, key := range []string{"entry_price", "quantity"} {
		if !dtoKeys[key] {
			t.Errorf("OpenPositionView に json:%q が無い — 未決済トータルを組み立てられない", key)
		}
		if !strings.Contains(html, key) {
			t.Errorf("dashboard が %q を読んでいない — 未決済トータルが出ない", key)
		}
	}
	if !strings.Contains(html, `id="openTotals"`) {
		t.Error("dashboard に #openTotals が無い — 未決済トータルの表示先が消えている")
	}
	if !strings.Contains(html, `$('openTotals').innerHTML`) {
		t.Error("dashboard が #openTotals を描画していない — 枠だけあって中身が出ない")
	}
}

// 一括決済ボタン(サイクル境界の帳簿締めを UI から打つ)。応答のキーを JS が
// 直に読むので、Go の DTO とキーがずれたら画面に "undefined 本決済" が出る。
func TestDashboardHasFlattenAllButton(t *testing.T) {
	html := dashboardHTML(t)
	if !strings.Contains(html, `id="btnFlattenAll"`) {
		t.Error("dashboard に一括決済ボタン(#btnFlattenAll)が無い")
	}
	if !strings.Contains(html, "/api/flatten-all") {
		t.Error("一括決済ボタンが /api/flatten-all を叩いていない")
	}
	for key := range jsonKeys(command.CloseAllResult{}) {
		if !strings.Contains(html, key) {
			t.Errorf("dashboard が応答の %q を読んでいない — 決済結果が画面に出ない", key)
		}
	}
}

// 81本を一発で閉じる不可逆操作なので、誤クリックで飛ばない造りであることを縛る:
// ①二段確認(confirm + 件数入力の prompt)②live_config ではボタンを出さない
// (backend の 409 と二重の閉じ)。ここが消えたら「押したら全部消えた」が起きる。
func TestDashboardFlattenAllIsGuarded(t *testing.T) {
	html := dashboardHTML(t)
	if !strings.Contains(html, "confirm(") || !strings.Contains(html, "prompt(") {
		t.Error("一括決済の二段確認(confirm + prompt)が無い — 誤クリックで全建玉が飛ぶ")
	}
	if !strings.Contains(html, "live_config") {
		t.Error("live_config でボタンを隠すガードが無い(実弾の全清算は人間が証券会社の画面でやる判断)")
	}
}

// 戦績パネルは**サイクル境界で切る**のが既定。出口の規則が変わったサイクル同士は
// 合算した数字が台帳として意味を持たない。
// 全期間は**ユーザーが明示的に選んだときだけ**出す。
//
// 併せて「いま何で絞っているか」は**サーバの応答(since)**で出す。UI が自分の
// つもりを表示すると、絞り込みが効いていない応答を絞り込み済みと読ませられる。
func TestDashboardPerformanceIsScopedToCycle(t *testing.T) {
	html := dashboardHTML(t)

	if !strings.Contains(html, `id="perfSince"`) {
		t.Fatal("戦績の期間セレクタ(#perfSince)が無い — 全期間を合算した数字が出る")
	}
	// 選択肢(各サイクル・全期間)と既定はサーバが持つ。paper はサイクルごとに
	// DB が違うので、画面が日付を持っても正しい台帳を引けない。
	if !strings.Contains(html, "api('/performance/cycles')") {
		t.Error("サイクルの一覧をサーバ(/performance/cycles)から取っていない")
	}
	if !strings.Contains(html, "?cycle=") {
		t.Error("/api/performance に cycle を渡していない — セレクタが飾りになっている")
	}
	if regexp.MustCompile(`<option value="\d{4}-\d{2}-\d{2}"`).MatchString(html) {
		t.Error("サイクルの日付を画面に焼いている — 境界の SSOT はサーバの一覧(perf_cycles.go)")
	}
	// 一覧が取れないときに cycle 無し(= 全期間)で黙って読まない。
	if !strings.Contains(html, "サイクル一覧を取得できません") {
		t.Error("一覧の取得失敗を表示していない — 全期間の数字が既定のサイクルに見える")
	}
	if !jsonKeys(query.ForwardReportView{})["since"] || !jsonKeys(query.ForwardReportView{})["until"] {
		t.Fatal("ForwardReportView に since / until が無い — 適用中の期間を応答から出せない")
	}
	if !strings.Contains(html, `id="perfPeriod"`) || !strings.Contains(html, "p.since") || !strings.Contains(html, "p.until") {
		t.Error("適用中の期間をサーバ応答(p.since / p.until)から表示していない — 部分集合が全期間に見える")
	}
}

func dashboardHTML(t *testing.T) string {
	t.Helper()
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("embed 読み出し: %v", err)
	}
	return string(b)
}

func jsonKeys(v any) map[string]bool {
	out := map[string]bool{}
	rt := reflect.TypeOf(v)
	for i := 0; i < rt.NumField(); i++ {
		tag, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		if tag != "" && tag != "-" {
			out[tag] = true
		}
	}
	return out
}

// ダッシュボードの JS は**素の <script> 1 本**なので、存在しない関数を 1 か所
// 呼ぶだけで初期化が途中で死ぬ。しかもコンソールを開かない限り無症状に見える:
// setTrack() の中の `refreshAdvisor && refreshAdvisor()`(そんな
// 関数は存在しない)が ReferenceError を投げ、後続の
//
//	setInterval(refresh, 2000) / restorePerfSince() / refreshPerformance()
//
// が**まるごと登録されなかった**。画面は初回描画までは出るので「開いただけでは
// 戦績が空、セレクタを触ると出る」という読み解けない挙動になった。
//
// 🛑 `undefinedFn && undefinedFn()` は**ガードにならない**。未宣言の識別子は
// 参照した時点で ReferenceError になる(安全なのは typeof だけ)。
//
// 呼んでいる名前がすべて「この script 内で定義済み」か「JS/DOM の組み込み」で
// あることを縛る。ブラウザを起動せずに ReferenceError の大半を落とせる。
func TestDashboardCallsOnlyDefinedFunctions(t *testing.T) {
	script := dashboardScript(t)

	defined := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)(?:async\s+)?function\s+([A-Za-z_$][\w$]*)\s*\(`).FindAllStringSubmatch(script, -1) {
		defined[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`(?m)(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=`).FindAllStringSubmatch(script, -1) {
		defined[m[1]] = true
	}

	// 制御構文と、実際に使っている JS/DOM の組み込みだけ。ここを安易に増やすと
	// テストが形骸化するので、増やすときは「本当に組み込みか」を確かめる。
	builtin := map[string]bool{
		"if": true, "for": true, "while": true, "switch": true, "catch": true,
		"function": true, "return": true, "typeof": true, "new": true, "await": true,
		"of": true, "async": true, "Date": true,
		"fetch": true, "setInterval": true, "setTimeout": true, "clearTimeout": true, "confirm": true,
		"prompt": true, "alert": true, "encodeURIComponent": true, "parseFloat": true,
		"parseInt": true, "isNaN": true, "Number": true, "String": true, "Boolean": true,
		"Error": true, "Object": true, "Array": true, "Promise": true, "Set": true, "Map": true,
	}

	var missing []string
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`(.?)\b([A-Za-z_$][\w$]*)\s*\(`).FindAllStringSubmatch(script, -1) {
		prev, name := m[1], m[2]
		// メソッド呼び出し(`.foo(`)と識別子の途中は対象外。
		if prev == "." || prev == "$" || (prev != "" && (isWordByte(prev[0]))) {
			continue
		}
		if defined[name] || builtin[name] || seen[name] {
			continue
		}
		seen[name] = true
		missing = append(missing, name)
	}
	if len(missing) > 0 {
		t.Errorf("dashboard が未定義の関数を呼んでいる %v — 参照した時点で ReferenceError になり、"+
			"以降の初期化(タイマー登録を含む)がまるごと実行されない", missing)
	}
}

func isWordByte(b byte) bool {
	return b == '_' || b == '$' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func dashboardScript(t *testing.T) string {
	t.Helper()
	html := dashboardHTML(t)
	_, rest, ok := strings.Cut(html, "<script>")
	if !ok {
		t.Fatal("dashboard に <script> が無い")
	}
	body, _, ok := strings.Cut(rest, "</script>")
	if !ok {
		t.Fatal("dashboard の </script> が無い")
	}
	return stripCommentsAndStrings(body)
}

// コメントと文字列リテラルを空白に潰す。日本語コメントの中の「(」や、
// '/api/live' のような文字列を**コードとして読まない**ため。潰すのは中身だけで
// 改行は残す(行番号がずれると失敗メッセージが役に立たなくなる)。
func stripCommentsAndStrings(src string) string {
	var out strings.Builder
	const (
		code = iota
		lineComment
		blockComment
		single
		double
		back
	)
	state := code
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch state {
		case code:
			switch {
			case c == '/' && i+1 < len(src) && src[i+1] == '/':
				state, i = lineComment, i+1
				out.WriteString("  ")
			case c == '/' && i+1 < len(src) && src[i+1] == '*':
				state, i = blockComment, i+1
				out.WriteString("  ")
			case c == '\'':
				state = single
				out.WriteByte(' ')
			case c == '"':
				state = double
				out.WriteByte(' ')
			case c == '`':
				state = back
				out.WriteByte(' ')
			default:
				out.WriteByte(c)
			}
		case lineComment:
			if c == '\n' {
				state = code
				out.WriteByte(c)
			} else {
				out.WriteByte(' ')
			}
		case blockComment:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				state, i = code, i+1
				out.WriteString("  ")
			} else if c == '\n' {
				out.WriteByte(c)
			} else {
				out.WriteByte(' ')
			}
		case single, double, back:
			closer := byte('\'')
			if state == double {
				closer = '"'
			} else if state == back {
				closer = '`'
			}
			switch {
			case c == '\\' && i+1 < len(src):
				i++
				out.WriteString("  ")
			case c == closer:
				state = code
				out.WriteByte(' ')
			case c == '\n':
				out.WriteByte(c)
			default:
				out.WriteByte(' ')
			}
		}
	}
	return out.String()
}

// 「スキャン対象 200」だけを出す画面は、その 200 を全部建てうると読める。live には
// 1単元の建玉金額上限(selector.max_position_notional_jpy)が別に立っていて、上限超の
// 銘柄は**発火しても arm されない** — 実データでは 200 銘柄中 100 銘柄しか
// 上限内に無く、その日発火した 2 件(4704 / 6728)は両方とも上限超で対象外だった。
// 内数と上限額を画面が読む契約を機械で縛る(キー名がずれると黙って消えるため)。
func TestDashboardHTMLShowsAffordableInnerCount(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("embed 読み出し: %v", err)
	}
	html := string(b)
	// blocked_notional = 発火したが資金上限で arm されない行。印が無いと、絶対に
	// 発注されない候補が「発注直前」に見える(スクリーナーは資金を見ない)。
	for _, key := range []string{"affordable", "max_notional_jpy", "blocked_notional"} {
		if !strings.Contains(html, key) {
			t.Errorf("dashboard が selector.%s を読んでいない — 「スキャン対象 N」が"+
				"「N 銘柄を建てうる」と誤読される", key)
		}
	}
}

// 🛑 戦略の出口ではない決済(entry_compensated / external_close)を**戦績の外の
// 別枠カード**に出す UI は置かない。別枠にすると
// 全件がそれだった日に戦績が空に見え、口座で実際に動いた実額が画面から消える
// (live がその状態だった)。**普通のトレードとして戦績に計上する。**
//
// ただし「どちらの数え方か」は名乗る — 画面は口座ベース(counting=account)、
// cmd/forward-report → cmd/edge-judge はエッジ標本(除外)のままで、数字が違う。
func TestDashboardCountsNonStrategyClosesInPerformance(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("embed 読み出し: %v", err)
	}
	html := string(b)

	if strings.Contains(html, "perfNonStrategy") {
		t.Error("戦略外の決済だけを別枠に出す UI が残っている — 戦績に計上する方針に変えた(二重計上になる)")
	}
	dtoKeys := jsonKeys(query.ForwardReportView{})
	if !dtoKeys["counting"] {
		t.Error("ForwardReportView に json:\"counting\" が無い — 口座ベースとエッジ標本を画面で見分けられない")
	}
	if !strings.Contains(html, "counting") {
		t.Error("dashboard が counting を読んでいない — どちらの数え方の戦績か画面に出ない")
	}
	// 計上した以上、トレード一覧の理由欄が生の英字のままでは何が起きたか読めない。
	for _, reason := range []string{"entry_compensated", "external_close"} {
		if !strings.Contains(html, reason) {
			t.Errorf("dashboard に %q のラベルが無い — 戦績に混ざった行の正体が読めない", reason)
		}
	}
}

// 🚨 **トラック取り違えは同じ形で 2 回起きた**。一括決済ボタンは
// `fetch('/api/flatten-all')` とハードコードされていて、HARVEST タブから押すと
// **research の建玉を全部消す**。手動 arm ボタンも同じ形だった。
//
// 画面は 3 トラックを 1 枚で切り替える設計なので、**パスを直接書いた瞬間に必ず
// research 固定になる**。個別のボタンを直すのではなく、`api()` を通さない fetch を
// 存在させない。
func TestDashboardRoutesEveryAPICallThroughTheTrackHelper(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, bad := range []string{"fetch('/api", "fetch(\"/api", "fetch(`/api"} {
		if strings.Contains(src, bad) {
			t.Errorf("%s … — トラックを無視して research 固定になる。api('/…') を通すこと", bad)
		}
	}
	// api() 自体が 3 トラックを分岐していること(ヘルパが定数を返すようになったら無意味)。
	if !strings.Contains(src, "TRACK === 'paper' ? '/api'") {
		t.Error("api() が TRACK で分岐していない — ヘルパ経由でも research 固定になる")
	}
}

// 🛑 arm しないトラック(live)で手動 arm ボタンを押せる場所に置かない。
func TestDashboardHidesTheManualArmButtonOffThePaperTrack(t *testing.T) {
	b, _ := webFS.ReadFile("web/index.html")
	src := string(b)
	if !strings.Contains(src, "ab.style.display = (t === 'paper') ? '' : 'none'") {
		t.Error("手動 arm ボタンが paper 以外でも出る — 押した人は『このトラックで arm が回った』と読む")
	}
	// live で arm 経路も塞ぐ(ボタンを隠すだけでは経路が残る)。
	if !strings.Contains(src, "LIVE は arm ラウンドを持ちません") {
		t.Error("live で arm 経路を塞いでいない(ボタンを隠すだけでは経路が残る)")
	}
}

// 🚨 **#positions を書き換える者は必ず lastPositionsHTML も戻す。**
//
// 「中身が同じなら DOM を書き換えない」キャッシュを入れたが、503 分岐
// (track が落ちている)が #positions を直接書き換えるのにキャッシュを戻していなかった。
// track が復帰したとき render() が同じ HTML を作った時点で DOM が書き換わらず、
// **実弾の建玉があるのに「track なし」のまま固まる**。bot の停止と起動は任意
// タイミングで打たれる前提(設計制約)なので日常的に起きる。
//
// 書込箇所とリセット箇所の数を突き合わせる。片方だけ増えたら落ちる。
func TestDashboardPositionsCacheIsResetByEveryWriter(t *testing.T) {
	// 🛑 dashboardScript は文字列リテラルを潰す(識別子解析用)ので、
	// 文字列の中身を見る検査には生 HTML を使う。
	script := dashboardHTML(t)

	writes := regexp.MustCompile(`\(['"]positions['"]\)\.innerHTML\s*=`).FindAllString(script, -1)
	resets := regexp.MustCompile(`lastPositionsHTML\s*=\s*null`).FindAllString(script, -1)
	if len(writes) == 0 {
		t.Fatal("#positions を書き換える箇所が見つからない — テストが対象を見失っている")
	}
	// render() 内の 1 箇所はキャッシュを **代入して更新する**(null に戻さない)ので、
	// null リセットが要るのは残り(setTrack / 503 分岐)。
	wantResets := len(writes) - 1
	if len(resets) < wantResets {
		t.Fatalf("#positions の書込が %d 箇所あるのに lastPositionsHTML=null が %d 箇所しかない — "+
			"戻し忘れた経路では、復帰後に表が古い表示のまま固まる(実弾の建玉が画面から消える)",
			len(writes), len(resets))
	}
}

// 🚨 **破壊的 POST に AbortSignal を付けない。**
//
// abort はサーバ側の r.Context() を cancel する。守りの置き直し
// (RepriceProtectiveOrder)は「取消 → 再発注」の間に明示的な裸の窓があり、そこで
// ctx が切れると PlaceSettleOCO が失敗して **守りが板から消えたまま
// protective_reprice_failed で trip** する。画面を固まらせないための期限が実弾の
// 守りを落とす。期限は watchdog(ログに出すだけ)で表現すること。
func TestDashboardDoesNotAbortDestructivePosts(t *testing.T) {
	script := dashboardScript(t)
	for _, bad := range []string{"AbortSignal", "AbortController", "signal:"} {
		if strings.Contains(script, bad) {
			t.Fatalf("破壊的 POST に %s を使っている — abort はサーバ側の ctx を cancel し、"+
				"守りの取消と再発注の間で落ちれば実弾の建玉が裸になる", bad)
		}
	}
	if !strings.Contains(script, "ACTION_WATCHDOG_MS") {
		t.Error("watchdog が無い — 応答が返らないときに人間へ何も伝わらない")
	}
}

// 🚨 **操作の結果は再描画されない場所に残す。**
// 建玉表は 2 秒ごとに innerHTML ごと差し替わるので、ボタンのラベルに書いた結果は
// 1 秒で消える(HTTP 500 の本文が誰の目にも触れなかった)。
// ダイアログは Chrome の「これ以上表示しない」で無効化されうるので、alert だけにも
// 依存しない。
func TestDashboardKeepsActionResultsOutsideTheTable(t *testing.T) {
	html := dashboardHTML(t)
	if !strings.Contains(html, `id="actionLog"`) {
		t.Fatal("操作ログの表示先(#actionLog)が無い — 結果が次の描画で消える")
	}
	// 🛑 dashboardScript は文字列リテラルを潰す(識別子解析用)ので、
	// 文字列の中身を見る検査には生 HTML を使う。
	script := dashboardHTML(t)
	if !strings.Contains(script, "function logAction(") {
		t.Fatal("logAction が無い")
	}
	for _, fn := range []string{"closePosition", "controlAction"} {
		if !strings.Contains(script, fn) {
			t.Errorf("%s が無い", fn)
		}
	}
	if n := strings.Count(script, "logAction("); n < 8 {
		t.Errorf("logAction の呼び出しが %d 箇所しかない — 成功・失敗・送信不能・watchdog を"+
			"全経路で残すこと(押して何も起きない実弾操作を作らない)", n)
	}
}

// 🚨 **緊急停止と再開は結果を確かめる。**従来は r.ok も本文も見ておらず、live の
// 緊急停止が失敗しても画面は何も言わなかった。押して何も起きない緊急停止は無いより悪い。
func TestDashboardEmergencyButtonsCheckTheResponse(t *testing.T) {
	// 🛑 dashboardScript は文字列リテラルを潰す(識別子解析用)ので、
	// 文字列の中身を見る検査には生 HTML を使う。
	script := dashboardHTML(t)
	for _, id := range []string{"btnStop", "btnResume"} {
		if !strings.Contains(script, "$('"+id+"')") {
			t.Fatalf("%s の配線が無い", id)
		}
	}
	if !strings.Contains(script, "controlAction('緊急停止'") {
		t.Error("緊急停止が結果を確かめる経路(controlAction)を通っていない")
	}
	if !strings.Contains(script, "controlAction('再開'") {
		t.Error("再開が結果を確かめる経路(controlAction)を通っていない")
	}
}

// 🚨 裸の判定は **server の籠**を読む。画面で `!bg` から推測すると、照会が失敗した
// 銘柄(板を確認できていない)を「守りが無い」と読んでしまう。逆に籠を読まないと
// 決済中の裸(live で実際に起きた形)が画面に出ない。
func TestDashboardReadsTheProtectiveBoardBuckets(t *testing.T) {
	// 🛑 dashboardScript は文字列リテラルを潰す(識別子解析用)ので、
	// 文字列の中身を見る検査には生 HTML を使う。
	script := dashboardHTML(t)
	for _, k := range []string{"unguarded", "closing_naked", "unknown"} {
		if !strings.Contains(script, k) {
			t.Errorf("dashboard が protective_board.%s を読んでいない — "+
				"その状態が画面に出ない(裸に気づけない / 障害が無言になる)", k)
		}
	}
}

// 🚨 **なぜ 1 本も arm されていないのか**が画面から読めること。
// 余力ゼロで枠待ちなのか、口座照会が読めていないのかは運用上まったく別の状態で、
// 前者を後者と読むと「壊れている」と思って再起動し、後者を前者と読むと障害を見逃す。
func TestDashboardReadsTheSelectorLeverageState(t *testing.T) {
	script := dashboardHTML(t)
	for _, k := range []string{"leverage_state", "leverage_headroom_jpy", "資金枠なし"} {
		if !strings.Contains(script, k) {
			t.Errorf("dashboard が %q を出していない — 資金枠で arm が止まっていることが画面から読めない", k)
		}
	}
}

// 時間切れの期限が画面に無いと、寄りで突然成行決済されて初めて気付く(人間が
// 画面から期限を知る手段が無い)。
// 期限は domain が決めた値を読む。JS が opened_at + 分 を自前で足すと engine からずれる。
func TestDashboardShowsMaxHoldDeadline(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("embed 読み出し: %v", err)
	}
	html := string(b)
	dtoKeys := jsonKeys(query.OpenPositionView{})
	for _, key := range []string{"max_hold_until", "max_hold_hard_until"} {
		if !dtoKeys[key] {
			t.Errorf("OpenPositionView に json:%q が無い", key)
		}
		if !strings.Contains(html, key) {
			t.Errorf("dashboard が %q を読んでいない — 時間切れの期限が建玉一覧に出ない", key)
		}
	}
	if strings.Contains(html, "max_hold_minutes *") || strings.Contains(html, "max_hold_minutes*") {
		t.Error("dashboard が max_hold_minutes から期限を自前で計算している — max_hold_until を読むこと")
	}
}

// 「気配のみ・未約定」のバナーは、**建玉がある銘柄**と**監視しているだけの銘柄**を分ける。
// live で arm しただけの銘柄(建玉なし)が寄り直後にまだ約定していないと、
// バナーが「約定が無いので TP/SL の評価も止まっています」と出る。建玉が無いので止まる
// TP/SL は存在せず、人間は「ポジションが無いのに TP/SL? バグ?」と読む。
// TP/SL が止まっているのは建玉つきの銘柄だけ — 監視のみの銘柄はエントリー判定が約定待ちなだけ。
func TestDashboardHaltedBannerSeparatesHeldFromWatchOnly(t *testing.T) {
	// dashboardScript は文字列リテラルを潰すので文言を探せない。生の HTML を読む。
	script := dashboardHTML(t)
	for _, id := range []string{"haltedHeld", "haltedWatch"} {
		if !strings.Contains(script, id) {
			t.Errorf("バナーが %s を持たない — 建玉つきと監視のみの銘柄を同じ文言で出している", id)
		}
	}
	i := strings.Index(script, "TP/SL の評価も止まって")
	if i < 0 {
		t.Fatal("建玉つき銘柄向けの「TP/SL の評価も止まって」の文言が無い")
	}
	// TP/SL の文言は haltedHeld を描く式の中にだけ置く(監視のみの行に付けない)。
	j := strings.LastIndex(script[:i], "haltedHeld")
	if j < 0 {
		t.Fatal("「TP/SL の評価も止まって」が haltedHeld の描画に属していない")
	}
	line := script[j:i]
	if strings.Contains(line, "haltedWatch") {
		t.Error("「TP/SL の評価も止まって」が監視のみ銘柄の行に掛かっている")
	}
	if strings.Count(script, "TP/SL の評価も止まって") != 1 {
		t.Error("「TP/SL の評価も止まって」が複数ある — 監視のみの行にも付いている疑い")
	}
}

// 🛑 **画面に「期限延長」と「TP/SL 変更」を置かない**。
// どちらも事前に決めた出口を裁量で曲げる操作。押せる場所にボタンがあることが
// 手を入れるきっかけになるので、画面から外す。
// API(`POST /api/live/positions/extend`・`POST /api/live/protective/reprice`)は残す —
// 経路が残っていることは live_test.go と csrf_test.go が見ている。
func TestDashboardHasNoDiscretionaryExitButtons(t *testing.T) {
	html := dashboardHTML(t)
	for _, bad := range []string{
		`class="extend"`,
		`class="reprice"`,
		`id="extendDialog"`,
		`id="repriceDialog"`,
		`function extendCell(`,
		`function repriceCell(`,
		`/positions/extend`,
		`/protective/reprice`,
	} {
		if strings.Contains(html, bad) {
			t.Errorf("画面に %q が残っている — 事前に決めた出口を画面から曲げられる", bad)
		}
	}
}

// live の銘柄ごとの新規停止(Phase C)。ランキングの行(arm 済みの行を含む)に「停止」、
// 停止中の行に「停止中」と「解除」を出し、停止中の一覧を live タブに置く。paper には出さない。
func TestDashboardHasLiveSymbolBlockButtons(t *testing.T) {
	html := dashboardHTML(t)
	for _, want := range []string{
		`id="symbolBlocks"`,
		"function blockCell(",
		"function renderSymbolBlocks(",
		"function symbolBlockAction(",
		"api(release ? '/symbol-blocks/release' : '/symbol-blocks')",
		"button.symblock",
		"button.symrelease",
		"停止中",
		"symbol_blocks",
		// 読めないときは live の新規が全部止まっている。画面で叫ぶ。
		"sb.error",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("%q が無い", want)
		}
	}
	for _, fn := range []string{"function blockCell(", "function renderSymbolBlocks("} {
		i := strings.Index(html, fn)
		if i < 0 || !strings.Contains(html[i:i+300], "isLive()") {
			t.Errorf("%s が live 以外でも出る — paper は止めない", fn)
		}
	}
	// ランキングの行に停止ボタンを並べる。
	if !strings.Contains(html, "blockCell(x.symbol)") {
		t.Error("ランキングの行に停止ボタンが無い")
	}
	// 押したら確認する(1 回だけ。連続ダイアログは Chrome に抑止される)。
	i := strings.Index(html, "function symbolBlockAction(")
	if i < 0 {
		t.Fatal("symbolBlockAction が無い")
	}
	seg := html[i : i+1200]
	if !strings.Contains(seg, "confirm(") || !strings.Contains(seg, "prompt(") {
		t.Error("停止・解除に確認ダイアログが無い")
	}
	if !strings.Contains(seg, "logAction(") {
		t.Error("結果を操作ログに残していない")
	}
}

// arm 済みなのに判定が無い銘柄があれば「未判定を判定」ボタンを出す(research と live の両方)。
// fetch はほかのボタンと同じく api() を通す(research は /api/gonogo/run・live は /api/live/gonogo/run)。
func TestDashboardHasGoNoGoRunButton(t *testing.T) {
	html := dashboardHTML(t)
	for _, want := range []string{"button.gonogorun", "未判定を判定", "fetch(api('/gonogo/run')"} {
		if !strings.Contains(html, want) {
			t.Errorf("%q が無い", want)
		}
	}
}

// LLM の go/no-go(Phase D)は research と live の両方のランキングの行に出す(表示だけ)。
// 印(GO / NO-GO / 不明 / 未判定 / 失敗)・分類・要約(展開で全文)・出典のリンク・判定時刻。
func TestDashboardShowsGoNoGoOnRankingRows(t *testing.T) {
	html := dashboardHTML(t)
	for _, want := range []string{
		"function gonogoCell(",
		"gonogoCell(x.symbol)",
		"d.gonogo",
		"未判定", "失敗", "NO-GO", "不明",
		"<details>",
		"judged_at",
		"missing_armed",
		`rel="noopener noreferrer"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("%q が無い", want)
		}
	}
	i := strings.Index(html, "function gonogoCell(")
	if i < 0 {
		t.Fatal("gonogoCell が無い")
	}
	seg := html[i : i+1600]
	if strings.Contains(seg, "isLive()") {
		t.Error("判定が live だけに出る — paper の bnf 家族にも出す")
	}
	// 出典の URL は http(s) だけをリンクにする(javascript: を踏ませない)。
	if !strings.Contains(seg, "https?:") {
		t.Error("出典の URL の scheme を確かめていない")
	}
}
