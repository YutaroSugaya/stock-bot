package risk

import (
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/order"
)

// 口座全体の「1 営業日の新規本数」の上限。
// 暴落の初日に出た発火で枠を埋めると、その玉が続けて損切りされ、底の発火は見送りになる
// (2008-10 は 21 本中 16 本、2020-03 は 18 本中 15 本が SL)。上限で 2〜3 日目に資金を残す。
// 数えるのは銘柄・戦略を問わない口座全体の bot 建玉(決済済みも含む)。
func TestAccountEntriesPerDayCapRejectsOnceReached(t *testing.T) {
	cfg := nanpinCfg(config.ModeLive)
	sig := nanpinSig(config.StrategyBNFReversion, order.SideBuy)

	snap := nanpinSnap()
	snap.AccountMaxEntriesPerDay = 2
	snap.AccountEntriesToday = 1
	if d := EvaluateStructural(sig, cfg, snap, nil); !d.Allowed {
		t.Fatalf("その日 2 本目の建ては通る: reason=%q", d.Reason)
	}
	snap.AccountEntriesToday = 2
	d := EvaluateStructural(sig, cfg, snap, nil)
	if d.Allowed || d.Reason != "account_entries_per_day (2 >= cap 2)" {
		t.Fatalf("その日 3 本目の建てを止めていない: allowed=%v reason=%q", d.Allowed, d.Reason)
	}
}

// 0 = 無効。research(paper)の標本をこの理由で censoring しない(事前登録は動かさない)。
func TestAccountEntriesPerDayCapDisabledAtZero(t *testing.T) {
	snap := nanpinSnap()
	snap.AccountEntriesToday = 99
	if d := EvaluateStructural(nanpinSig(config.StrategyBNFReversion, order.SideBuy), nanpinCfg(config.ModePaper), snap, nil); !d.Allowed {
		t.Fatalf("上限 0 は無効のはず: reason=%q", d.Reason)
	}
}
