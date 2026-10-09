package config

import "testing"

// 🚨 実フィードで forward 記録を採るなら **DSN 必須**。paper 執行でも
// `paper_live_feed` は「本物の価格でサイクルの台帳を書いている」ので、DSN を
// 忘れたまま起動すると in-memory へ黙って落ち、**プロセスを落とした時点でその日の
// 建玉と決済が消える**。live だけを守っていては足りない。
func TestRequireDurableBackend(t *testing.T) {
	t.Run("live は DSN 必須(従来どおり)", func(t *testing.T) {
		if err := RequireDurableBackend(ModeLive, BrokerTachibana, ""); err == nil {
			t.Fatal("live が DSN 無しで通った")
		}
	})
	t.Run("paper_live_feed も DSN 必須", func(t *testing.T) {
		if err := RequireDurableBackend(ModePaper, BrokerPaperLiveFeed, ""); err == nil {
			t.Fatal("paper_live_feed が DSN 無しで通った — サイクルの台帳が再起動で消える")
		}
	})
	t.Run("純 paper(合成フィード)は従来どおり in-memory を許す", func(t *testing.T) {
		if err := RequireDurableBackend(ModePaper, BrokerPaper, ""); err != nil {
			t.Fatalf("test / backtest 用の in-memory を塞いだ: %v", err)
		}
	})
	t.Run("DSN があれば通る", func(t *testing.T) {
		for _, k := range []BrokerKind{BrokerPaper, BrokerPaperLiveFeed, BrokerTachibana} {
			if err := RequireDurableBackend(ModePaper, k, "postgres://x/y"); err != nil {
				t.Errorf("%s: %v", k, err)
			}
		}
	})
}
