package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"stockbot/backend/internal/adapter/apiusage"
	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/order"
)

// paper_live_feed(本番フィード + 紙執行)の結線。認証 env が無ければ fail-close、
// live_config では組めない(config 側でも弾くが、wiring でも二重に閉じる)。
func TestBuildBrokerPaperLiveFeed(t *testing.T) {
	hl := &config.HardLimits{AllowedSymbols: []string{"7203"}}
	cfg := &config.BotConfig{
		Mode: config.ModePaper, Symbols: []string{"7203"},
		Broker: config.BrokerCfg{Kind: config.BrokerPaperLiveFeed},
	}

	t.Run("認証 env 欠落は fail-close", func(t *testing.T) {
		t.Setenv("STOCKBOT_TACHIBANA_AUTH_ID", "")
		t.Setenv("STOCKBOT_TACHIBANA_PRIVATE_KEY", "")
		t.Setenv("STOCKBOT_TACHIBANA_PRIVATE_KEY_FILE", "")
		if _, err := buildBrokerSet(cfg, hl, nil, nil); err == nil {
			t.Fatal("認証情報なしで paper_live_feed が組めてはいけない")
		}
	})

	t.Run("live_config では組めない", func(t *testing.T) {
		liveCfg := *cfg
		liveCfg.Mode = config.ModeLive
		if _, err := buildBrokerSet(&liveCfg, hl, nil, nil); err == nil {
			t.Fatal("live_config + paper_live_feed は reject されるべき")
		} else if !strings.Contains(err.Error(), "paper_live_feed") {
			t.Fatalf("エラーが理由を説明していない: %v", err)
		}
	})

	t.Run("組めたら PaperLiveFeed 型(実弾経路を持たない)", func(t *testing.T) {
		// login はネットワーク越しなので差し替える(seam は本番でも同じ経路)。
		orig := tachibanaLogin
		tachibanaLogin = func(*broker.Tachibana) error { return nil }
		defer func() { tachibanaLogin = orig }()
		t.Setenv("STOCKBOT_TACHIBANA_ENV", "production")
		t.Setenv("STOCKBOT_TACHIBANA_AUTH_ID", "authid")
		t.Setenv("STOCKBOT_TACHIBANA_PRIVATE_KEY", testPEM(t))
		t.Setenv("STOCKBOT_TACHIBANA_SECOND_PASSWORD", "second")
		set, err := buildBrokerSet(cfg, hl, nil, nil)
		b := set.research
		if err != nil {
			t.Fatalf("buildBrokerSet: %v", err)
		}
		if _, ok := b.(*broker.PaperLiveFeed); !ok {
			t.Fatalf("broker = %T, want *broker.PaperLiveFeed", b)
		}
		// ここが外れると価格ループが 1銘柄1リクエストに戻り、立花からの高負荷の
		// 指摘を再発させる。
		if !broker.QuotesBatched(b) {
			t.Fatal("paper_live_feed の時価取得が一括化されていない")
		}
	})
}

func testPEM(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

// paper_live_feed は種価格(2500固定)を持たない: 残っていると実勢と無関係な価格で
// 建ってしまい paper 損益が嘘になる。実フィード未着なら fail-close が正しい。
func TestPaperLiveFeedHasNoSeedPrice(t *testing.T) {
	orig := tachibanaLogin
	tachibanaLogin = func(*broker.Tachibana) error { return nil }
	defer func() { tachibanaLogin = orig }()
	t.Setenv("STOCKBOT_TACHIBANA_ENV", "production")
	t.Setenv("STOCKBOT_TACHIBANA_AUTH_ID", "authid")
	t.Setenv("STOCKBOT_TACHIBANA_PRIVATE_KEY", testPEM(t))
	t.Setenv("STOCKBOT_TACHIBANA_SECOND_PASSWORD", "second")

	hl := &config.HardLimits{AllowedSymbols: []string{"7203"}}
	cfg := &config.BotConfig{
		Mode: config.ModePaper, Symbols: []string{"7203"},
		Broker: config.BrokerCfg{Kind: config.BrokerPaperLiveFeed},
	}
	set, err := buildBrokerSet(cfg, hl, nil, nil)
	b := set.research
	if err != nil {
		t.Fatalf("buildBrokerSet: %v", err)
	}
	if _, err := b.PlaceOrder(context.Background(), order.PlaceOrderRequest{
		Symbol: "7203", Side: order.SideBuy, Type: order.OrderTypeMarket, Quantity: 100, ExecKind: order.ExecCash,
	}); err == nil {
		t.Fatal("実フィード未着でも約定した = 種価格が残っている(paper 損益が嘘になる)")
	}

	// 素の paper(kind: paper)は種価格で動く(オフライン開発用)。
	paperSet, err := buildBrokerSet(&config.BotConfig{Mode: config.ModePaper, Symbols: []string{"7203"},
		Broker: config.BrokerCfg{Kind: config.BrokerPaper}}, hl, nil, nil)
	pb := paperSet.research
	if err != nil {
		t.Fatalf("buildBrokerSet(paper): %v", err)
	}
	if _, err := pb.PlaceOrder(context.Background(), order.PlaceOrderRequest{
		Symbol: "7203", Side: order.SideBuy, Type: order.OrderTypeMarket, Quantity: 100, ExecKind: order.ExecCash,
	}); err != nil {
		t.Fatalf("素の paper は種価格で約定できるべき: %v", err)
	}
}

// 実アダプタは decorator(BatchQuoteFeed → PaperLiveFeed)の内側に埋まるので、
// buildBrokerSet が数え口を一緒に返さないと呼び出し回数を外から読めない。paper でも
// nil を返さないこと(呼び出し側の nil 判定漏れで panic させない)。
func TestBuildBrokerReturnsAPIRequestCounter(t *testing.T) {
	hl := &config.HardLimits{AllowedSymbols: []string{"7203"}}
	set, err := buildBrokerSet(&config.BotConfig{Mode: config.ModePaper, Symbols: []string{"7203"},
		Broker: config.BrokerCfg{Kind: config.BrokerPaper}}, hl, nil, nil)
	apiReqs := set.apiRequests
	if err != nil {
		t.Fatalf("buildBrokerSet(paper): %v", err)
	}
	if apiReqs == nil {
		t.Fatal("apiReqs が nil — 呼び出し側が nil 判定を忘れると panic する")
	}
	if got := apiReqs(); got != 0 {
		t.Errorf("paper で APIRequests=%d, want 0 — 立花に触らない構成で数字が動いている", got)
	}
}

// 🛑 永続カウンタは **buildBrokerSet が挿さないと本番だけ無効**になる。数え口は
// doGET 1 か所なので、そこに届いていなければ 1日の総数は永久に取れない
// (立花の指摘に回数で答えられなかった原因そのもの)。
func TestBuildBrokerAttachesUsageRecorderToTachibana(t *testing.T) {
	orig := tachibanaLogin
	tachibanaLogin = func(*broker.Tachibana) error { return nil }
	defer func() { tachibanaLogin = orig }()
	t.Setenv("STOCKBOT_TACHIBANA_ENV", "production")
	t.Setenv("STOCKBOT_TACHIBANA_AUTH_ID", "authid")
	t.Setenv("STOCKBOT_TACHIBANA_PRIVATE_KEY", testPEM(t))
	t.Setenv("STOCKBOT_TACHIBANA_SECOND_PASSWORD", "second")

	dir := t.TempDir()
	usage := apiusage.Open(dir, "test", nil)
	hl := &config.HardLimits{AllowedSymbols: []string{"7203"}}
	cfg := &config.BotConfig{
		Mode: config.ModePaper, Symbols: []string{"7203"},
		Broker: config.BrokerCfg{Kind: config.BrokerPaperLiveFeed},
	}
	if _, err := buildBrokerSet(cfg, hl, nil, usage); err != nil {
		t.Fatalf("buildBrokerSet: %v", err)
	}
	// 実フィードに触れない paper でも、記録先の受け口があること自体は壊さない。
	if _, err := buildBrokerSet(&config.BotConfig{Mode: config.ModePaper, Symbols: []string{"7203"},
		Broker: config.BrokerCfg{Kind: config.BrokerPaper}}, hl, nil, usage); err != nil {
		t.Fatalf("buildBrokerSet(paper): %v", err)
	}
	if got := usage.Snapshot().Total; got != 0 {
		t.Errorf("login を stub した構成で total=%d, want 0", got)
	}
}
