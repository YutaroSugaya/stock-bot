package risk

import (
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
)

// 制度信用の**売建**は貸借銘柄でしか出せない。無いと発注が
// 拒否されるだけ(約定後に守りが置けない型の事故にはならない)が、
// **拒否は標本の穴として静かに残る**ので、ホワイトリストで先に落とす。
//
// 🛑 fail-close: **リストが無い / 空なら売りシグナルを全 reject**。静的な推測に
// 縮退しない(`allowed_symbols` と同じ作法)。一覧を作るのは人間の作業なので、
// 用意されるまで売り側は標本ゼロ — それが「エラーも出さずにゼロ」にならないよう、
// reject 理由として台帳(signal_rejections)に残る形にする。

func loanSig(side order.Side) strategy.Signal {
	return strategy.Signal{
		Decision: strategy.DecisionEnter, SignalID: "s", Symbol: "7203",
		Side: side, Quantity: 100, EntryPrice: 2000,
		StrategyName: config.StrategyAbsMomentumV2, HoldingMode: order.HoldingMultiday,
	}
}

func TestLoanableGateAllowsEveryBuy(t *testing.T) {
	// 買いは貸借銘柄かどうかに関係なく通る(リストが空でも)。
	if d := EvaluateShortLoanable(loanSig(order.SideBuy), LoanableSymbols{}); !d.Allowed {
		t.Fatalf("買いが reject された: %q", d.Reason)
	}
}

func TestLoanableGateBlocksSellsWithoutAList(t *testing.T) {
	d := EvaluateShortLoanable(loanSig(order.SideSell), LoanableSymbols{})
	if d.Allowed {
		t.Fatal("貸借銘柄一覧が無いのに売りが通った(fail-close でない)")
	}
	if d.Reason == "" {
		t.Fatal("reason が空 — signal_rejections に残らない")
	}
}

// 🛑 **2 つの理由を混ぜない**。文書・STATUS・yaml は
// 「一覧が空なら `loanable_list_missing` が signal_rejections に残る」と約束しているが、
// 配線がメソッド値で常に非 nil だったため production では**一度も記録されなかった**。
// 「人間が一覧を commit し忘れている」と「一覧はあるがこの銘柄が非貸借」は
// **運用上まったく別の状態**なので、理由文字列で区別できなければならない。
func TestLoanableGateDistinguishesMissingListFromNonLoanableSymbol(t *testing.T) {
	missing := EvaluateShortLoanable(loanSig(order.SideSell), LoanableSymbols{})
	if missing.Reason != "loanable_list_missing" {
		t.Fatalf("一覧未 commit の理由 = %q, want loanable_list_missing", missing.Reason)
	}
	// 一覧はあるが対象外の銘柄。
	listed := LoanableSymbols{Configured: true, Allows: func(string) bool { return false }}
	other := EvaluateShortLoanable(loanSig(order.SideSell), listed)
	if other.Reason == "loanable_list_missing" {
		t.Fatal("一覧があるのに loanable_list_missing になった — 2 つの状態が区別できない")
	}
	// `recordRejection` は先頭トークンを種別キーにするので、そこが割れていること。
	if kindOf(missing.Reason) == kindOf(other.Reason) {
		t.Fatalf("reject 種別が同じ(%q) — signal_rejections で区別できない", kindOf(missing.Reason))
	}
}

func kindOf(reason string) string {
	for i := 0; i < len(reason); i++ {
		if reason[i] == ' ' {
			return reason[:i]
		}
	}
	return reason
}

func TestLoanableGateBlocksSellsOfNonLoanableSymbols(t *testing.T) {
	only6758 := LoanableSymbols{Configured: true, Allows: func(sym string) bool { return sym == "6758" }}
	if d := EvaluateShortLoanable(loanSig(order.SideSell), only6758); d.Allowed {
		t.Fatal("貸借銘柄でない銘柄の売建が通った")
	}
}

func TestLoanableGateAllowsSellsOfLoanableSymbols(t *testing.T) {
	only7203 := LoanableSymbols{Configured: true, Allows: func(sym string) bool { return sym == "7203" }}
	if d := EvaluateShortLoanable(loanSig(order.SideSell), only7203); !d.Allowed {
		t.Fatalf("貸借銘柄の売建が reject された: %q", d.Reason)
	}
}

// エントリー以外(NONE / NO_TRADE)は素通し — ゲートは発注する signal にだけ効く。
func TestLoanableGateIgnoresNonEntrySignals(t *testing.T) {
	sig := loanSig(order.SideSell)
	sig.Decision = strategy.DecisionNoTrade
	if d := EvaluateShortLoanable(sig, LoanableSymbols{}); !d.Allowed {
		t.Fatalf("非エントリーが reject された: %q", d.Reason)
	}
}
