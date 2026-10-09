package risk

import (
	"fmt"

	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/strategy"
)

// EvaluateShortLoanable は「この売建を出せる銘柄か」。
// 制度信用の**売建は貸借銘柄でしか出せない**。
//
// 🛑 **fail-close**: `loanable == nil`(一覧が無い / 空)なら売りシグナルを全 reject する。
// 静的な推測に縮退しない — `allowed_symbols` と同じ作法。一覧を作るのは人間の作業
// (JPX の貸借銘柄一覧 → 4 文字コード → commit)なので、用意されるまで売り側は標本ゼロ。
//
// 🛑 **買いには一切効かない**。向きを開ける変更が、買い側の測定に触れないこと。
//
// 落とすのを発注前にするのは、broker に拒否させると「拒否は標本の穴として静かに残る」
// から。ここで落とせば `signal_rejections` に理由が残り、**売り側がゼロなのは
// 一覧が無いからだ**と後から分かる。
func EvaluateShortLoanable(sig strategy.Signal, loanable LoanableSymbols) Decision {
	if !sig.IsEntry() || sig.Side != order.SideSell {
		return Decision{Allowed: true}
	}
	// 🛑 **「一覧がまだ無い」と「一覧はあるがこの銘柄が非貸借」を区別する。**
	// 両方 `not_loanable` にすると、signal_rejections を見ても
	// 「人間が一覧を commit し忘れている」という本当の原因にたどり着けない
	// (配線がメソッド値で常に非 nil だったため、
	//  文書が約束する `loanable_list_missing` は production では到達不能だった)。
	if !loanable.Configured || loanable.Allows == nil {
		return Decision{Reason: "loanable_list_missing"}
	}
	if !loanable.Allows(sig.Symbol) {
		return Decision{Reason: fmt.Sprintf("not_loanable %s(制度信用の売建は貸借銘柄のみ)", sig.Symbol)}
	}
	return Decision{Allowed: true}
}

// LoanableSymbols は貸借銘柄ホワイトリストの参照口。
//
// `Configured=false` = **一覧そのものが未 commit**(人間の作業待ち)。`Allows` が
// 単に false を返すのと区別できるようにするためだけに存在する — 区別が無いと、
// 「売り標本がゼロなのは一覧が無いからだ」と後から気づけない。
type LoanableSymbols struct {
	Configured bool
	Allows     func(symbol string) bool
}
