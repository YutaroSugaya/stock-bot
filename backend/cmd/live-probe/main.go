// Command live-probe は 立花 e支店の**本番ホスト read-only** 裏取りツール。デモ用の
// 認証セットが無いので、wire(公開鍵 login / 仮想URL復号 / 時価 / 日足 / 余力 / 建玉)の
// 確定を本番ホストに参照系だけ当てて行う。
//
// 発注系 API(PlaceOrder / ClosePosition / PlaceSettleOCO / CancelOrder)はこのファイル
// に記述されていない = フラグの付け忘れで実弾が飛ぶ経路が型として存在しない。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"stockbot/backend/internal/adapter/apiusage"
	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/port"
)

func main() {
	symbol := flag.String("symbol", "7203", "銘柄コード(時価/日足を引く)")
	margin := flag.Bool("margin", true, "信用建玉/維持率も参照する(一般信用を使う想定)")
	// 時価API が「どの列を返せるか」だけを見るモード。仕様書が repo に無いので、
	// 当日出来高が取れるか(当日出来高版の前提条件)を wire で決めるために要る。
	// 候補列は**1 つずつ**投げる — 未対応の列が混ざると要求ごと弾かれうるため。
	columns := flag.String("probe-columns", "", "時価API の列プローブ(カンマ区切りの候補を1つずつ試す。例: pDV,pDJ,pDY)")
	// 注文状態コードの実値を確かめる用。取消済/有効が実際にどの値で返るかは repo に
	// 記録が無く、docs も「Status 50 常駐」と書くだけ。締める前にここで実測する。
	orderRows := flag.Bool("probe-order-rows", false, "注文一覧の生レコードをそのまま表示する(状態コードの実値確認)")
	// 🚨 **約定の実額**を読むため。台帳の cold close / 補償の巻き戻しは
	// 約定値が同期に取れない経路なので **観測価格(mid)で記帳し fee_estimated=true** を立てる
	// (reconcile.go の coldClose / bookCloseAs)。実額はここでしか照合できない。
	// 🛑 carry(信用金利)は約定レコードに乗らない。手数料+消費税までしか取れないので、
	//    金利は証券会社の画面から人間が読むこと。
	executions := flag.Int("probe-executions", 0, "当日の約定を実額(単価/数量/手数料)で表示する。件数上限(0=全部、-1 で有効化のみ)")
	// 🛑 probe 専用。既定版(bot が叩く版)を動かさずに新版を実測する口(v4r9 → v4r10 移行)。
	apiVersion := flag.String("api-version", "", "叩く API 版を差し替える(例: v4r10)。空なら既定版のまま")
	flag.Parse()

	env := os.Getenv("STOCKBOT_TACHIBANA_ENV")
	if env != "production" {
		log.Fatalf("live-probe は本番ホスト参照用です。STOCKBOT_TACHIBANA_ENV=production を設定してください(現在 %q)。デモ認証があるなら cmd/demo-probe を使ってください", env)
	}

	cr, err := broker.TachibanaCredsFromEnv(false)
	if err != nil {
		log.Fatal(err)
	}

	// ocoVerified=false: 守り発注は fail-close のまま。
	tb := broker.NewTachibana("production", cr.AuthID, cr.Key, cr.SecondPW,
		false /*ocoVerified*/, *margin, nil)
	// 🚨 login より前に永続カウンタを挿す。立花は閉局・休日・メンテ時間帯のアクセスを禁じて
	// いるが、これを調べたとき、この経路だけ api-usage に残っておらず、いつ叩いたかを答えられなかった。
	tb.SetUsageRecorder(apiusage.Open(apiusage.DefaultDir(), "live-probe", nil))
	if *apiVersion != "" {
		if err := tb.UseAPIVersion(*apiVersion); err != nil {
			log.Fatal(err)
		}
	}
	ctx := context.Background()
	fmt.Printf("API 版: %s(env=%s host=%s)\n", tb.APIVersion(), tb.APIEnv(), tb.APIHost())

	fmt.Println("⚠ 本番ホスト(kabuka.e-shiten.jp)に接続します。本ツールは参照系のみ・発注は一切しません。")

	section("(1)(2) login / RefreshToken (公開鍵認証)")
	if err := tb.RefreshToken(ctx); err != nil {
		log.Fatalf("❌ login 失敗: %v\n  → AUTH_ID / 秘密鍵PEM / API利用申込・公開鍵登録の状態を確認。", err)
	}
	fmt.Println("✅ login 成功(仮想URL群を復号してセッション確立)")
	if err := tb.RefreshToken(ctx); err != nil {
		log.Fatalf("❌ 再login 失敗(無人 RefreshToken が通らない): %v", err)
	}
	fmt.Println("✅ 再login 成功(2FA 撤廃済 → 無人で通る想定どおり)")

	if *columns != "" {
		section("列プローブ: 時価API がこの列を返すか(前提条件)")
		for _, col := range strings.Split(*columns, ",") {
			col = strings.TrimSpace(col)
			if col == "" {
				continue
			}
			row, err := tb.ProbeMarketPriceColumns(ctx, *symbol, "pDPP,"+col)
			switch {
			case err != nil:
				fmt.Printf("  %-8s ❌ 要求そのものが弾かれた: %v\n", col, err)
			default:
				v, ok := row[col]
				if !ok {
					fmt.Printf("  %-8s ❌ 応答に列が現れない(この broker では取れない)\n", col)
					continue
				}
				fmt.Printf("  %-8s ✅ %q(同時取得の現在値 pDPP=%q)\n", col, v, row["pDPP"])
			}
		}
		fmt.Println("\n  → 取れた列が無ければ **当日出来高版は永久に不可**(それも記録する結論)。")
		return
	}

	if *orderRows {
		section("注文一覧の生レコード(状態コードの実値確認)")
		rows, err := tb.ProbeOrderRows(ctx, *symbol)
		if err != nil {
			log.Fatalf("❌ ProbeOrderRows: %v", err)
		}
		fmt.Printf("  %d件\n", len(rows))
		for _, row := range rows {
			keys := make([]string, 0, len(row))
			for k := range row {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var b strings.Builder
			for _, k := range keys {
				if row[k] == "" {
					continue
				}
				fmt.Fprintf(&b, "%s=%s ", k, row[k])
			}
			fmt.Printf("    %s\n", b.String())
		}
		return
	}

	if *executions != 0 {
		section("当日の約定(実額) — 台帳の推定値と照合する")
		limit := *executions
		if limit < 0 {
			limit = 0 // -1 は「有効化のみ・件数無制限」
		}
		ex, err := tb.GetExecutions(ctx, limit)
		if err != nil {
			log.Fatalf("❌ GetExecutions: %v", err)
		}
		fmt.Printf("  %d件\n", len(ex))
		fmt.Printf("  %-16s %-6s %-5s %8s %12s %10s\n", "注文番号", "銘柄", "売買", "数量", "約定単価", "手数料")
		for _, e := range ex {
			fmt.Printf("  %-16s %-6s %-5s %8d %12.2f %10.2f\n",
				e.ExecutionID, e.Symbol, e.Side, e.Quantity, e.Price, e.FeeJPY)
		}
		fmt.Println("\n  🛑 手数料は**注文単位の合計を先頭の約定に寄せて**いる(GetExecutions)。")
		fmt.Println("     行ごとに按分された値ではないので、注文単位で合計して読むこと。")
		fmt.Println("  🛑 carry(制度信用の金利)はこの API に乗らない。証券会社の画面から読むこと。")
		return
	}

	section("(4) read-only: 時価 / 日足 / 分足(非対応) / 余力 / 建玉")
	tk, tkErr := tb.GetTicker(ctx, *symbol)
	dump("GetTicker", tk, tkErr)
	if kl, err := tb.GetKlines(ctx, *symbol, port.PeriodDaily, 60); err != nil {
		fmt.Printf("  GetKlines(daily) ❌ %v\n", err)
	} else {
		fmt.Printf("  GetKlines(daily) ✅ %d本", len(kl))
		if n := len(kl); n > 0 {
			last := kl[n-1]
			fmt.Printf(" / 最新 %s O=%.1f H=%.1f L=%.1f C=%.1f V=%.0f",
				last.OpenTime.Format("2006-01-02"), last.Open, last.High, last.Low, last.Close, last.Volume)
		}
		fmt.Println()
		if len(kl) < 26 {
			fmt.Println("  ⚠ 日足が 26本未満: BNF(≥26本必要)には遡及本数が足りない → 遡及本数は TACHIBANA_API_NOTES §3 を確認")
		}
	}
	if _, err := tb.GetKlines(ctx, *symbol, port.Period1m, 10); err != nil {
		fmt.Printf("  GetKlines(1m) は error が正 ✅ %v\n", err)
	} else {
		fmt.Println("  ⚠ GetKlines(1m) が error を返さなかった(分足非対応の想定と不一致)")
	}
	am, amErr := tb.GetAccountMargin(ctx)
	dump("GetAccountMargin", am, amErr)
	ps, psErr := tb.GetPositions(ctx)
	dumpPositions(ps, psErr)

	// 建玉に対して**決済側の注文が実際に板に残っているか**。多日保有の守りは
	// sOrderExpireDay 次第で当日失効するので、翌朝これを見ないと裸に気づけない。
	// GetActiveOrders は参照系なので本ツールの「発注しない」性質は保たれる。
	for _, p := range ps {
		os, oerr := tb.GetActiveOrders(ctx, p.Symbol)
		if oerr != nil {
			fmt.Printf("  GetActiveOrders(%s) ❌ %v\n", p.Symbol, oerr)
			continue
		}
		// 🛑 決済側の注文の**数量**ではなく、**逆指値脚を持つ注文**の数量を数える。
		// 利確指値だけが残っている建玉は下方向に裸で、それを守りと数えたことが
		// 「✅ 守りあり」の誤報の正体だった。
		guard, sellSide := 0, 0
		for _, o := range os {
			if o.Side == p.Side.Opposite() {
				sellSide += o.Quantity
				if o.HasStopLeg {
					guard += o.Quantity
				}
			}
			kind := "通常(利確指値など・SLなし)"
			if o.HasStopLeg {
				kind = "逆指値あり(守り)"
			}
			fmt.Printf("    order %s %s %s qty=%d price=%.1f status=%s %s\n",
				p.Symbol, o.OrderID, o.Side, o.Quantity, o.Price, o.Status, kind)
		}
		mark := "✅ 守りあり"
		switch {
		case guard < p.Quantity && sellSide >= p.Quantity:
			mark = "🚨 裸(決済注文はあるが逆指値脚が足りない)"
		case guard < p.Quantity:
			mark = "🚨 守りが足りない(裸)"
		}
		if sellSide > p.Quantity {
			mark += fmt.Sprintf(" ⚠ 決済側が建玉超過(%d株 > %d株)", sellSide, p.Quantity)
		}
		fmt.Printf("  %s: 建玉 %d株 / 決済側 %d株 / うち逆指値あり %d株 → %s\n",
			p.Symbol, p.Quantity, sellSide, guard, mark)
	}

	section("完了(read-only)")
	fmt.Println("確定できたのは参照系の wire のみ。守り(逆指値の受理/常駐/発動方向/bot停止下約定)は")
	fmt.Println("発注を伴うため本ツールでは検証不能 → STOCKBOT_TACHIBANA_OCO_VERIFIED は閉じたまま。")
}

func section(title string) {
	fmt.Printf("\n===== %s =====\n", title)
}

func dump(label string, v any, err error) {
	if err != nil {
		fmt.Printf("  %s ❌ %v\n", label, err)
		return
	}
	fmt.Printf("  %s ✅ %+v\n", label, v)
}

func dumpPositions(ps []port.BrokerPosition, err error) {
	if err != nil {
		fmt.Printf("  GetPositions ❌ %v\n", err)
		return
	}
	fmt.Printf("  GetPositions ✅ %d件\n", len(ps))
	for _, p := range ps {
		fmt.Printf("    - %s side=%s qty=%d entry=%.1f exec=%s id=%s\n",
			p.Symbol, p.Side, p.Quantity, p.EntryPrice, p.ExecKind, p.BrokerPositionID)
	}
}
