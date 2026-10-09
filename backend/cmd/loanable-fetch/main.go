// Command loanable-fetch は **貸借銘柄ホワイトリストの素材**を立花のマスタから取る
// read-only ツール。
//
// `hard_limits.loanable_symbols` が空だと `direction: both` の 8 アームの売りは
// 100% reject され、測りたい変数のうち「向き」が丸ごと欠測する。
//
// 判定は **2 つのマスタの AND**:
//
//	① CLMStkGetIssueSizyouMstKabu.sSinyouC … 信用区分(構造的な属性)
//	② CLMStkGetIssueSizyouKiseiKabu        … 銘柄市場規制。**規制がある銘柄だけ**載る
//	   sSeidoSinyouSinkiUritate = "1"(停止)なら制度信用の新規売建は出せない
//
// 🛑 読むのは **sSeidoSinyou…(制度信用)**。sIppanSinyou…(一般信用)は仕様に区分が
// あるのに口座で拒否された実績があるので、そちらで判定すると「売れると判定したのに
// 発注が拒否される」銘柄が混ざる(CLAUDE.md の実装メモ)。
//
// v4r9 の CLMEventDownload(連結 JSON + terminator)から v4r10 の
// 個別マスタ問合(単発 JSON)へ載せ替えた。完全性は terminator ではなく
// **下限件数ガード**が守る(internal/adapter/broker/tachibana_issue_master.go)。
//
// 🛑 **AND にするのは fail-close のため。** 広すぎるホワイトリストは broker に
// 発注ごと拒否され、「約定 → 守り拒否 → 補償で決済 → 再入場」の
// 往復を生む。狭すぎる側は標本が減るだけで台帳は汚れない。
//
// 🛑 **このツールは yaml を書き換えない。** ホワイトリストの commit は人間の作業
// (allowed_symbols と同じ作法)。
//
// 🛑 **発注系 API はこのファイルに記述されていない** = フラグの付け忘れで実弾が飛ぶ
// 経路が型として存在しない(live-probe と同じ規律)。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"stockbot/backend/internal/adapter/apiusage"
	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/config"
)

// 東証本則。銘柄市場マスタは (銘柄, 市場) の行なので、市場で絞らないと同じ銘柄が
// 複数回出て件数が母数を超える(実測: allowed 1,551 に対し 1,671 行)。
const tosyoMain = "00"

func main() {
	hardLimitsPath := flag.String("hard-limits", "configs/hard_limits.yaml", "allowed_symbols の出所(ここに載っていない銘柄は出さない)")
	loanableCodes := flag.String("loanable-codes", "1", "sSinyouC のうち『制度信用の売建が出せる』とみなす値(カンマ区切り)")
	sample := flag.Int("sample", 0, "生レコードを何件表示するか(項目名を目で確かめる用)")
	// 🛑 probe 専用。既定版(bot が叩く版)を動かさずに新版を実測する口(v4r9 → v4r10 移行)。
	apiVersion := flag.String("api-version", "", "叩く API 版を差し替える(例: v4r10)。空なら既定版のまま")
	// v4r10 の個別マスタ問合(CLMStkGetIssueSizyouMstKabu 等)を**解釈せず**トップレベルごと
	// 1 行 JSON で stdout に出す。p_errno / p_err / 配列キー / 件数を目で確かめる(Q13 / Q11 / Q7)。
	probeRaw := flag.String("probe-raw", "", "指定した CLMID を MASTER に投げ、応答のトップレベルを 1 行 JSON で表示して終了(解釈しない)")
	flag.Parse()

	if env := os.Getenv("STOCKBOT_TACHIBANA_ENV"); env != "production" {
		log.Fatalf("loanable-fetch は本番ホスト参照用です。STOCKBOT_TACHIBANA_ENV=production を設定してください(現在 %q)", env)
	}
	cr, err := broker.TachibanaCredsFromEnv(false)
	if err != nil {
		log.Fatal(err)
	}
	// ocoVerified=false / marginEnabled=false: 参照しかしないので両方閉じたまま。
	tb := broker.NewTachibana("production", cr.AuthID, cr.Key, cr.SecondPW, false, false, nil)
	// 🚨 login より前に永続カウンタを挿す。立花は閉局・休日・メンテ時間帯のアクセスを禁じて
	// いるが、これを調べたとき、この経路だけ api-usage に残っておらず、いつ叩いたかを答えられなかった。
	tb.SetUsageRecorder(apiusage.Open(apiusage.DefaultDir(), "loanable-fetch", nil))
	if *apiVersion != "" {
		if err := tb.UseAPIVersion(*apiVersion); err != nil {
			log.Fatal(err)
		}
	}
	ctx := context.Background()

	fmt.Fprintf(os.Stderr, "⚠ 本番ホストに接続します。参照系のみ(マスタ取得 2 リクエスト)。API 版: %s\n", tb.APIVersion())
	if err := tb.RefreshToken(ctx); err != nil {
		log.Fatalf("❌ login 失敗: %v", err)
	}
	fmt.Fprintln(os.Stderr, "✅ login 成功")

	if *probeRaw != "" {
		// 本文は stdout・進捗は stderr(`grep '^{'` で 1 個の JSON が取れる契約)。
		raw, err := tb.ProbeRawMaster(ctx, *probeRaw)
		if err != nil {
			log.Fatalf("❌ %s の取得に失敗: %v", *probeRaw, err)
		}
		line, err := json.Marshal(raw)
		if err != nil {
			log.Fatalf("❌ 応答を JSON に戻せない: %v", err)
		}
		fmt.Println(string(line))
		return
	}

	// v4r10 の個別マスタ問合(単発 JSON)。完全性は adapter 側の下限件数ゲートが守る。
	issues, err := tb.FetchStockIssueMarkets(ctx)
	if err != nil {
		log.Fatalf("❌ 銘柄市場マスタの取得に失敗: %v", err)
	}
	kisei, err := tb.FetchStockIssueRegulations(ctx)
	if err != nil {
		log.Fatalf("❌ 銘柄市場規制の取得に失敗: %v", err)
	}
	fmt.Printf("✅ 銘柄市場マスタ %d 行 / 銘柄市場規制 %d 行\n", len(issues), len(kisei))

	if *sample > 0 {
		printSample("銘柄市場マスタ", len(issues), *sample, func(i int) any { return issues[i] })
		printSample("銘柄市場規制", len(kisei), *sample, func(i int) any { return kisei[i] })
	}

	// 東証本則だけに絞ってから重複を落とす。
	main00 := map[string]broker.StockIssueMarket{}
	for _, r := range issues {
		if r.ListedMarket != tosyoMain {
			continue
		}
		main00[r.IssueCode] = r
	}
	// 規制: 制度信用の新規売建が停止(= "1")されている銘柄。
	// 🛑 東証本則の行だけを見る。他市場の規制でプライム銘柄を落とすと標本が理由不明で減る。
	shortHalted := map[string]bool{}
	kiseiMain00 := map[string]bool{}
	for _, r := range kisei {
		if r.ListedMarket != tosyoMain {
			continue
		}
		kiseiMain00[r.IssueCode] = true
		if r.SystemShortNew == "1" || r.HaltClass == "1" || r.HaltClass == "2" {
			shortHalted[r.IssueCode] = true
		}
	}

	fmt.Println("\n── sSinyouC(信用区分)の分布・東証本則のみ・重複排除後 ──")
	fmt.Println("   ※ 意味は分布と既知銘柄で必ず裏取りすること(一次資料の表記が揺れている)")
	dist := map[string]int{}
	for _, r := range main00 {
		dist[r.MarginClass]++
	}
	codes := make([]string, 0, len(dist))
	for c := range dist {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	for _, c := range codes {
		fmt.Printf("  %-4q %d 銘柄\n", c, dist[c])
	}
	fmt.Printf("  制度信用の新規売建が停止中: %d 銘柄\n", len(shortHalted))

	fmt.Println("\n── 既知銘柄の実値(マッピングの裏取り)──")
	known := map[string]string{"7203": "トヨタ", "6501": "日立", "9432": "NTT", "4751": "サイバーエージェント", "1309": "中国株上証50ETF"}
	kcodes := make([]string, 0, len(known))
	for c := range known {
		kcodes = append(kcodes, c)
	}
	sort.Strings(kcodes)
	for _, c := range kcodes {
		r, ok := main00[c]
		if !ok {
			fmt.Printf("  %s(%s): 東証本則のマスタに無い\n", c, known[c])
			continue
		}
		fmt.Printf("  %s(%s): sSinyouC=%q 売建停止=%v\n", c, known[c], r.MarginClass, shortHalted[c])
	}

	hl, err := config.LoadHardLimits(*hardLimitsPath)
	if err != nil {
		log.Fatalf("❌ hard_limits 読み込み失敗(%s): %v", *hardLimitsPath, err)
	}
	want := map[string]bool{}
	for _, c := range strings.Split(*loanableCodes, ",") {
		if c = strings.TrimSpace(c); c != "" {
			want[c] = true
		}
	}
	var out []string
	missing, wrongCode, halted := 0, 0, 0
	for _, s := range hl.AllowedSymbols {
		r, ok := main00[s]
		if !ok {
			missing++
			continue
		}
		if !want[r.MarginClass] {
			wrongCode++
			continue
		}
		if shortHalted[s] {
			halted++
			continue
		}
		out = append(out, s)
	}
	sort.Strings(out)

	fmt.Printf("\n── allowed_symbols %d のうち ──\n", len(hl.AllowedSymbols))
	fmt.Printf("  マスタに無い(上場廃止など)   : %d\n", missing)
	fmt.Printf("  sSinyouC が対象外            : %d\n", wrongCode)
	fmt.Printf("  制度信用の新規売建が停止中   : %d\n", halted)
	fmt.Printf("  ✅ 貸借銘柄として出す        : **%d 銘柄**\n", len(out))

	// 🛑 **一覧を印字する前に落とす**。ここが無いと 0 件でも素通りして
	// 空の `loanable_symbols:` を提案し、人間がそれを commit すると
	// `risk.EvaluateShortLoanable` が `loanable_list_missing` で**全 SELL を fail-close で
	// reject** する = エラーも出さずに売り側の標本が丸ごと欠測する。
	//
	// 判定順は**根本原因から症状へ**。「0 件でした」ではなく「規制マスタが届いていない」と
	// 言えるようにするため。件数の内訳は上で印字済みなので、人間が原因を診断できる。
	if err := sanityGate(len(kiseiMain00), len(shortHalted), missing, len(hl.AllowedSymbols), len(out)); err != nil {
		log.Fatalf("❌ %v", err)
	}

	fmt.Println("\n🛑 以下を configs/hard_limits.yaml の loanable_symbols へ**人間が** commit する。")
	fmt.Println("loanable_symbols:")
	for _, c := range out {
		fmt.Printf("  - \"%s\"\n", c)
	}
}

func printSample(label string, n, want int, at func(int) any) {
	fmt.Printf("\n── %s の生レコード(先頭 %d 件)──\n", label, want)
	for i := 0; i < n && i < want; i++ {
		b, _ := json.Marshal(at(i))
		fmt.Println(" ", string(b))
	}
}

// sanityGate は「意味的カバレッジ」。
// 下限件数ガード(adapter 側)が転送の完全性を守るのに対し、こちらは
// **allowed_symbols と突き合わせて「一覧として使えるか」**を見る。
//
// 🛑 判定順は根本原因 → 症状。allowed は "1301"〜"9997" の全域に散っているので、
// 配列が前方で切り詰められると missing が発火する = **件数ガードより鋭い検出器**。
func sanityGate(kiseiRows, halted, missing, allowed, symbols int) error {
	switch {
	case kiseiRows == 0:
		return fmt.Errorf("規制マスタが東証本則で 0 行 — 届いていない。0 行を「規制なし」と読むと一覧が広くなり、発注が板で拒否される")
	case halted == 0:
		return fmt.Errorf("制度信用の新規売建が停止中の銘柄が 0 — 以前のスナップショットでは 27 銘柄あった。規制の読み方(sSeidoSinyouSinkiUritate / sTeisiKubun)が変わった可能性")
	case missing > allowed/100:
		return fmt.Errorf("allowed %d のうち %d 銘柄がマスタに無い(許容 %d)— 配列が前方で切り詰められている可能性。実測の非カバーは 2/1,551 = 0.13%%", allowed, missing, allowed/100)
	case symbols < 1000:
		return fmt.Errorf("貸借銘柄が %d しか出ない(下限 1000)— 凍結スナップショットは 1,450/1,551 = 93.5%%。この一覧を commit すると売り側の標本が偏る", symbols)
	}
	return nil
}
