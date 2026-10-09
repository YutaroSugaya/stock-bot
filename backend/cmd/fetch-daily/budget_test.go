package main

import (
	"strings"
	"testing"

	"stockbot/backend/internal/adapter/apiusage"
)

// 見積りは**実際に送る本数**と一致していなければならない。login を数え落とすと
// 「ちょうど収まる」と言って 1 本超える。
func TestRequestsNeededCountsLoginAndBench(t *testing.T) {
	if got, want := requestsNeeded(1549, false), 1550; got != want {
		t.Errorf("bench 無し: %d, want %d(1,549 銘柄 + login)", got, want)
	}
	if got, want := requestsNeeded(1549, true), 1551; got != want {
		t.Errorf("bench 有り: %d, want %d(+ bench_topix)", got, want)
	}
}

// 🛑 立花の 10,000 回は dashboard に**表示されているだけで強制されていない**。
// 1日で最大の塊(1,550)を打つ直前が唯一の現実的な検問所。
func TestCheckRequestBudget(t *testing.T) {
	for _, tc := range []struct {
		name    string
		used    int64
		need    int
		wantErr bool
	}{
		{"朝いちばん(残枠まるごと)", 0, 1550, false},
		{"ちょうど使い切る量は通す(境界)", 8450, 1550, false},
		{"1 リクエストでも超えるなら落とす", 8451, 1550, true},
		{"既に上限を超えている日は落とす", 10500, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkRequestBudget(tc.used, tc.need, apiusage.BrokerDailyRequestCap)
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkRequestBudget(%d, %d) err=%v, wantErr=%v", tc.used, tc.need, err, tc.wantErr)
			}
		})
	}
}

// 🚨 **上限ガードだけでは今日の事故は止まらない**: bot が
// 8,062 回まで積んだ夕方に 2 回目を打っても 8,062 + 1,550 = 9,612 で**上限の内側**。
// それでも残枠を丸ごと食い潰すし、そもそも同じ日足を 2 回引くのは純粋な無駄
// (1 回ぶん約 1,550 リクエスト)。**「この窓で既に引いたか」を別に見る。**
func TestAlreadyFetchedThisWindow(t *testing.T) {
	for _, tc := range []struct {
		name      string
		history   int64
		need      int
		wholePool bool
		want      bool
	}{
		{"朝の取得前(履歴呼び出しゼロ)", 0, 1550, true, false},
		{"朝の取得の後(2 回目)", 1550, 1550, true, true},
		{"朝が途中で落ちた(半分だけ)", 700, 1550, true, false},
		// -symbols で対象を絞った修復実行は人間が意図して打っている。プールぶんの
		// 履歴が既にあっても止めない(止めると穴の空いた 1 銘柄を直せなくなる)。
		{"数銘柄だけの修復実行は対象外", 1550, 4, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := alreadyFetchedThisWindow(tc.history, tc.need, tc.wholePool); got != tc.want {
				t.Errorf("alreadyFetchedThisWindow(%d, %d, %v) = %v, want %v",
					tc.history, tc.need, tc.wholePool, got, tc.want)
			}
		})
	}
}

// エラー文は**人間がその場で判断できる**材料を持つこと。数字だけ出しても
// 「どうやって通すのか」が分からず、結局ソースを読んで逃げ道を探すことになる。
func TestBudgetErrorsNameTheNumbersAndTheEscapeHatch(t *testing.T) {
	t.Run("上限", func(t *testing.T) {
		err := checkRequestBudget(8451, 1550, apiusage.BrokerDailyRequestCap)
		if err == nil {
			t.Fatal("落ちるべき")
		}
		for _, want := range []string{"8451", "1550", "10000", "-ignore-budget"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("エラー文に %q が無い: %v", want, err)
			}
		}
	})
	t.Run("二重取得", func(t *testing.T) {
		err := duplicateFetchError(1550)
		if err == nil {
			t.Fatal("落ちるべき")
		}
		for _, want := range []string{"1550", "-check", "-ignore-budget"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("エラー文に %q が無い: %v", want, err)
			}
		}
	})
}

// 配線の判定表。**逃げ道は 1 つだけ**(`-ignore-budget`)で、それは両方のガードを
// 同時に外す — 片方だけ外れると「どっちで落ちたのか」を人間が追う羽目になる。
func TestEnforceRequestBudget(t *testing.T) {
	usage := func(total, history int64) apiusage.Usage {
		return apiusage.Usage{Total: total, ByCLMID: map[string]int64{clmidDailyHistory: history}}
	}
	for _, tc := range []struct {
		name    string
		in      budgetInputs
		wantErr bool
	}{
		{"朝の 1 回目", budgetInputs{u: usage(0, 0), symbols: 1549, bench: true, wholePool: true}, false},
		{"朝の後の 2 回目(上限の内側でも落とす)",
			budgetInputs{u: usage(8062, 1550), symbols: 1549, bench: true, wholePool: true}, true},
		{"残枠が足りない",
			budgetInputs{u: usage(9500, 0), symbols: 1549, bench: true, wholePool: true}, true},
		{"-symbols での修復実行は通す",
			budgetInputs{u: usage(8062, 1550), symbols: 3, bench: false, wholePool: false}, false},
		{"-ignore-budget は両方を外す",
			budgetInputs{u: usage(9500, 1550), symbols: 1549, bench: true, wholePool: true, ignore: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := enforceRequestBudget(tc.in); (err != nil) != tc.wantErr {
				t.Fatalf("enforceRequestBudget() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// 🛑 二重取得の判定に login を混ぜない。窓のカウンタは履歴 CLMID しか数えて
// いないので、混ぜると**完走した朝の直後でも「まだ引いていない」と誤判定する**
// (実測 1,550 vs 見積り 1,551 の 1 本差で、ガードが丸ごと無効になっていた)。
func TestHistoryRequestsNeededExcludesLogin(t *testing.T) {
	if got, want := historyRequestsNeeded(1549, true), 1550; got != want {
		t.Errorf("履歴の本数 = %d, want %d(実際の朝の取得回数と一致すること)", got, want)
	}
	if got := requestsNeeded(1549, true); got != historyRequestsNeeded(1549, true)+1 {
		t.Errorf("requestsNeeded は履歴 + login のはず: %d", got)
	}
}
