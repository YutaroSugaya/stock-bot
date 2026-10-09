package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"stockbot/backend/internal/adapter/apiusage"
	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/port"
)

// brokerSet は 1 プロセスで組み立てた broker 群。hybrid の live track は **同じ
// `*Tachibana` と同じ一括フィード**を共有しないといけない(立花のセッションは口座に
// 1 本で、2 本目の login は 1 本目を破棄する)ので、decorator の内側に埋まった素の
// 部品をここで外へ出す。
//
// tb / feed が nil = 実ブローカーを持たない構成(paper 単体)。その場合 live track は
// dry-run(紙 broker)としてしか起動できない。
type brokerSet struct {
	research    port.LiveBroker // 現行と同一の broker(研究トラック用)
	tb          port.LiveBroker // 素の立花。live track の発注はここへ届く
	feed        port.MarketFeed // 共有一括フィード(read-only)
	apiRequests func() int64
}

// buildBrokerSet selects the broker adapter from bot_config.
//
// apiRequests は立花への wire 呼び出し回数の数え口。実アダプタは decorator
// (BatchQuoteFeed → PaperLiveFeed)の内側に埋まって外から取り出せないので、
// 組み立てたここで一緒に返す。
//
// usage は**プロセスを跨いで残る**数え口(nil 可)。プロセス内 atomic は
// make stop/start でゼロに戻るので、1日の総数はそちらでは言えない。ここで挿さないと
// 本番だけ記録が無効になるので、配線は exec_broker_test が固定している。
func buildBrokerSet(cfg *config.BotConfig, hl *config.HardLimits, c clock.Clock, usage broker.UsageRecorder) (brokerSet, error) {
	brk, api, tb, feed, err := buildBrokerParts(cfg, hl, c, usage)
	if err != nil {
		// apiRequests は nil を返さない(呼び出し側の nil 判定漏れを 0 に倒す)。
		return brokerSet{apiRequests: noAPIRequests}, err
	}
	return brokerSet{research: brk, tb: tb, feed: feed, apiRequests: api}, nil
}

func buildBrokerParts(cfg *config.BotConfig, hl *config.HardLimits, c clock.Clock, usage broker.UsageRecorder) (port.LiveBroker, func() int64, port.LiveBroker, port.MarketFeed, error) {
	switch cfg.Broker.Kind {
	case config.BrokerPaper, "":
		return newPaperBroker(hl, c, true /*seedPrices*/), noAPIRequests, nil, nil, nil
	case config.BrokerPaperLiveFeed:
		// 実フィード(read-only)+ 紙執行。紙の約定を live として記録すると forward
		// 記録が嘘になるので live_config では使わせない(config 側でも reject 済)。
		if cfg.Mode == config.ModeLive {
			return nil, noAPIRequests, nil, nil, fmt.Errorf("broker kind=paper_live_feed は live_config では使えない(紙執行を live と記録しない)")
		}
		tb, err := buildTachibana(cfg, c, usage)
		if err != nil {
			return nil, noAPIRequests, nil, nil, err
		}
		// 時価取得を一括化して呼び出し数を銘柄数から切り離す(立花からの高負荷の
		// 指摘への対処)。defaultLoopConfig は broker.QuotesBatched を見て間隔を決めるので、
		// ここを外すと自動的に 30 秒へ退避する(速いまま素通しに戻る事故が起きない)。
		feed := broker.NewBatchQuoteFeed(tb, batchQuoteMaxAge(), c)
		// 種価格は入れない: 固定 2500 で建つと paper 損益が実勢と無関係な嘘になる。
		// 実フィードの最初のティックが来るまで約定は fail-close。
		return broker.NewPaperLiveFeed(newPaperBroker(hl, c, false /*seedPrices*/), feed), tb.APIRequests, tb, feed, nil
	case config.BrokerTachibana:
		// ここを開けるときは LiveBroker 全面のデコレータが要る = 一括化されないので
		// defaultLoopConfig が 30 秒に落とす。
		tb, err := buildTachibana(cfg, c, usage)
		if err != nil {
			return nil, noAPIRequests, nil, nil, err
		}
		feed := broker.NewBatchQuoteFeed(tb, batchQuoteMaxAge(), c)
		return tb, tb.APIRequests, tb, feed, nil
	default:
		return nil, noAPIRequests, nil, nil, fmt.Errorf("unknown broker kind %q", cfg.Broker.Kind)
	}
}

// noAPIRequests is the counter for brokers that never touch 立花 (paper)。
// nil を返さないのは、呼び出し側の nil 判定漏れを panic ではなく 0 に倒すため。
func noAPIRequests() int64 { return 0 }

// newPaperBroker builds the paper adapter. seedPrices=true は実フィードの無い
// オフライン開発用。paper_live_feed では false にして、最初のティックが届くまで
// PlaceOrder を fail-close させる。
func newPaperBroker(hl *config.HardLimits, c clock.Clock, seedPrices bool) *broker.Paper {
	pb := broker.NewPaper(c, hl.Paper.SimulatedSlippageTicks, float64(hl.Paper.APIFeeJPYPerTrade))
	pb.BalanceJPY = hl.Paper.BalanceJPY // 0 = 既定100万円
	if seedPrices {
		for _, sym := range hl.AllowedSymbols {
			pb.SetPrice(sym, paperSeedPrice)
		}
	}
	return pb
}

// tachibanaLogin is the initial-login seam (network). Tests stub it; production
// always logs in at startup so bad credentials fail loudly before the loop runs.
var tachibanaLogin = func(tb *broker.Tachibana) error { return tb.RefreshToken(context.Background()) }

func buildTachibana(cfg *config.BotConfig, c clock.Clock, usage broker.UsageRecorder) (*broker.Tachibana, error) {
	env := getenv("STOCKBOT_TACHIBANA_ENV", "demo")
	if env != "demo" && env != "production" {
		// NewTachibana fail-closes to demo, but a typo must still be loud rather
		// than silently deciding where real orders go.
		return nil, fmt.Errorf("STOCKBOT_TACHIBANA_ENV must be 'demo' or 'production', got %q", env)
	}
	// 公開鍵認証(v4r10。認証 I/F は v4r9 と同一): sAuthId + RSA秘密鍵(仮想URL復号用)
	// + 第2パスワード(発注/取消)。旧 v4r7 の USER_ID/PASSWORD は 2025-11-29 廃止 → 使わない。
	cr, err := broker.TachibanaCredsFromEnv(true)
	if err != nil {
		return nil, err
	}
	// 守り(逆指値/OCO)は実機未検証なので fail-close: デモで裏取りした人間が =1 を
	// 立てるまで PlaceSettleOCO は error を返し、entry saga が巻き戻る。
	ocoVerified := os.Getenv("STOCKBOT_TACHIBANA_OCO_VERIFIED") == "1"
	// 一般信用を使う設定なら、信用建玉/維持率もポーリングする。
	// 🛑 **区分を列挙しない**。制度信用を足したときにここが漏れると、信用建玉を
	// 持っているのに維持率も信用建玉一覧も引かない状態になる(守りが静かに消える)。
	marginEnabled := cfg.Holding.Multiday.ExecKind.IsMargin() || cfg.Holding.Intraday.ExecKind.IsMargin()
	tb := broker.NewTachibana(env, cr.AuthID, cr.Key, cr.SecondPW, ocoVerified, marginEnabled, c)
	// 🛑 login より**前**に挿す。最初の login も相手から見れば 1 リクエストで、
	// 起動のたびに数え漏れると再起動の多い日ほど過少申告になる。
	if usage != nil {
		tb.SetUsageRecorder(usage)
	}
	if err := tachibanaLogin(tb); err != nil {
		return nil, fmt.Errorf("tachibana initial login failed: %w", err)
	}
	return tb, nil
}

// logBrokerAPIVersion は「いま叩いている 立花 API」を起動ログに 1 行だけ残す。
// NewTachibana は logger を持たない(adapter は slog を知らない)ので、版を知っている
// 唯一の口 = *broker.Tachibana を、logger を持っている cmd 側で読む。
// 🛑 v4r9 廃止の移行中は、いま叩いている版がログに出ないと
// 「本番だけ旧版のまま」が黙って走る。env(設定値そのまま)と host(fail-close 解決後)を
// 対で出すので、「prod と書いてデモへ倒れていた」も同じ 1 行で読める。
// paper 構成(set.tb == nil)では何も出さない。
func logBrokerAPIVersion(logger *slog.Logger, set brokerSet) {
	if logger == nil {
		return
	}
	tb, ok := set.tb.(*broker.Tachibana)
	if !ok || tb == nil {
		return
	}
	logger.Info("tachibana api endpoint", "ver", tb.APIVersion(), "env", tb.APIEnv(), "host", tb.APIHost())
}

// brokerAPIUpdateNotice は起動時 login が返した予定日の告知を取り出す。paper 構成は false。
func brokerAPIUpdateNotice(set brokerSet) (broker.APIUpdateNotice, bool) {
	tb, ok := set.tb.(*broker.Tachibana)
	if !ok || tb == nil {
		return broker.APIUpdateNotice{}, false
	}
	return tb.APIUpdateNotice(), true
}

// apiUpdateNoticePath は前回受信値の置き場(~/.stockbot/state/。Desktop 配下に置かない理由は
// apiusage.DefaultDir と同じ)。🛑 パスは cmd 側で決めて adapter へ渡す — adapter が config を
// import すると arch-guard (i) で落ちる。
func apiUpdateNoticePath() string {
	return filepath.Join(filepath.Dir(apiusage.DefaultDir()), "tachibana-update-notice.json")
}

// apiUpdateNoticeState は「前回受信値」。判定式(v10:231)は初回を空白として扱うので、
// ファイルが無い = 全項目空でよい。
type apiUpdateNoticeState struct {
	APISpecFunction string `json:"api_spec_function"`
	WebDocument     string `json:"web_document"`
}

// warnAPIUpdateNotice は login 応答の予定日告知を v10:231 の判定式で見て、**新しく知った
// 予定日だけ** Warn する(同じ告知で毎日鳴らない)。今回の v4r9 → v4r10 移行も bot は
// 毎日ログインしながら一言も言わなかった。
// 🛑 fail-close にしない。読めない / 書けないは Warn 止まりで起動を止めない
// (告知が出た日に bot が上がらない方が危険)。
func warnAPIUpdateNotice(logger *slog.Logger, n broker.APIUpdateNotice, statePath string, now time.Time) {
	if logger == nil {
		return
	}
	var last apiUpdateNoticeState
	if b, err := os.ReadFile(statePath); err == nil {
		if err := json.Unmarshal(b, &last); err != nil {
			logger.Warn("tachibana update notice: state unreadable, treating as first sight", "path", statePath, "err", err)
			last = apiUpdateNoticeState{}
		}
	}
	for _, f := range []struct{ kind, planned, last, action string }{
		{"api_spec_function", n.APISpecFunction, last.APISpecFunction, "予定日までに立花 HP で変更内容(版の追加・旧版の廃止)を確認する"},
		{"web_document", n.WebDocument, last.WebDocument, "予定日までに標準 Web で交付書面を確認する(未読だと API が止まる)"},
	} {
		if broker.APIUpdateDue(f.planned, f.last, now) {
			logger.Warn("tachibana update notice", "kind", f.kind, "planned", f.planned, "last", f.last, "action", f.action)
		}
	}
	next := apiUpdateNoticeState{APISpecFunction: n.APISpecFunction, WebDocument: n.WebDocument}
	if next == last {
		return
	}
	if err := writeAPIUpdateNoticeState(statePath, next); err != nil {
		logger.Warn("tachibana update notice: state not saved (will warn again next start)", "path", statePath, "err", err)
	}
}

func writeAPIUpdateNoticeState(path string, st apiUpdateNoticeState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

const paperSeedPrice = 2500.0

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
