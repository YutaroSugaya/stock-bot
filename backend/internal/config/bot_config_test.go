package config

import "testing"

// paper_live_feed = 本番フィード(読み取り専用)+ paper 執行。実弾は飛ばないので
// live_config では使えない(live を名乗って紙で執行すると記録が嘘になる)。
// 執行区分は立花に合わせる(paper 検証の結果がそのまま live に移せるように)。
func TestBrokerPaperLiveFeed(t *testing.T) {
	if BrokerPaperLiveFeed != "paper_live_feed" {
		t.Fatalf("kind = %q, want paper_live_feed", BrokerPaperLiveFeed)
	}
	if BrokerPaperLiveFeed.SupportsExecKind(ExecMarginOneday) {
		t.Fatal("paper_live_feed must mirror tachibana: 一日信用は非対応")
	}
	if !BrokerPaperLiveFeed.SupportsExecKind(ExecCash) || !BrokerPaperLiveFeed.SupportsExecKind(ExecMarginSystem) {
		t.Fatal("paper_live_feed must support cash / margin_system")
	}
	hl := &HardLimits{AllowedSymbols: []string{"7203"}}
	live := &BotConfig{Mode: ModeLive, Symbols: []string{"7203"}, Broker: BrokerCfg{Kind: BrokerPaperLiveFeed}}
	if err := live.ValidateAgainstHardLimits(hl); err == nil {
		t.Fatal("live_config + paper_live_feed must be rejected (紙執行を live と記録しない)")
	}
	paper := &BotConfig{Mode: ModePaper, Symbols: []string{"7203"}, Broker: BrokerCfg{Kind: BrokerPaperLiveFeed}}
	if err := paper.ValidateAgainstHardLimits(hl); err != nil {
		t.Fatalf("paper_config + paper_live_feed must be allowed: %v", err)
	}
}

// 日足リフレッシュの可否は mode ではなく broker の能力で決まる。paper 単体は
// 日足を返さないが、paper_live_feed / tachibana は実 API の日足を返す
// (mode で判定すると paper_live_feed の日足が起動時シードのまま凍結する)。
func TestBrokerServesKlines(t *testing.T) {
	for kind, want := range map[BrokerKind]bool{
		BrokerTachibana:     true,
		BrokerPaperLiveFeed: true,
		BrokerPaper:         false,
		"":                  false,
	} {
		if got := kind.ServesKlines(); got != want {
			t.Fatalf("%q.ServesKlines() = %v, want %v", kind, got, want)
		}
	}
}
