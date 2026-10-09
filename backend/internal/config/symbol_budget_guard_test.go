package config_test

// 監視銘柄の予算が「時価の実効間隔 3秒(遅くとも 6秒)」を守れる値かを **config から**
// 機械強制する。
//
// 🚨 なぜ本数ではなく銘柄数か: 時価は 1 リクエストに 120 銘柄まで積め、
// `BatchQuoteFeed` は 121 銘柄目で間隔をチャンク数だけ伸ばして通信量を一定に保つ。
// つまり監視銘柄が増えても **API 回数は 1 回も増えない**(時価は 6,600回/日 で固定)。
// 増えるのは実効間隔で、監視和集合が 120 を超えて
// **3秒 → 6秒**へ落ちた。監視集合は research と live で共有なので、paper の建玉が live の
// OnTick 決済判定と、バックテストの入力である分足の密度まで道連れに粗くする。
//
// 🛑 このテストが落ちたら **config の値を直す**。閾値を緩めて緑にするのは、
// 「3秒〜6秒を維持する」という事前登録そのものを消すこと。

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/strategy"
)

// live トラックの監視銘柄の予約。`bot_config.live.yaml` は gitignore なので config からは読めない。
// live の selector は空き枠のぶんだけ arm するので、監視は
// **建玉 + armed ≤ account_max_open_positions** で頭打ちになる。予約は live の
// `risk.account_max_open_positions` 以上にする。**live の account_max を上げるときはここも上げる**。
const liveWatchReserve = 10

func TestSymbolBudgetKeepsQuoteIntervalWithinPolicy(t *testing.T) {
	cfg, err := config.LoadBotConfig(researchConfigPath(t, "bot_config.advisor.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	g := cfg.Risk.AccountMaxOpenSymbols
	s := cfg.Risk.PerEntryMaxOpenSymbols
	if g <= 0 || s <= 0 {
		t.Fatalf("研究モードの銘柄予算が無効になっている: account=%d per_entry=%d "+
			"(0 = 無効。監視 120 を超えて時価の間隔が落ちる再発経路)", g, s)
	}

	// 回るのは `advisor_v2.entries` で絞った集合(入口 6 / 10 アーム)。
	// 予算はメニュー全部ではなく**実際に回る入口**で組む(止めた入口は枠を消費しない)。
	active, err := strategy.ScreenersForEntries(cfg.Advisor.Entries)
	if err != nil {
		t.Fatalf("advisor_v2.entries: %v", err)
	}
	entries := countEntryArms(active)
	if entries <= 0 {
		t.Fatal("入口の数が 0 — メニューの正本(戦略カタログ)か entries が壊れている")
	}
	// 実効上限は「全体の枠」と「入口の枠 × 入口数」の小さい方。
	effective := entries * s
	if g < effective {
		effective = g
	}
	// 入口の枠だけで全体の枠に届かないと、全体の枠が一度も効かない(= backstop として
	// 死んでいる)。
	if entries*s < g {
		t.Errorf("入口の枠の総和 %d(= %d 入口 × %d)が全体の枠 %d に届かない — "+
			"全体の枠が backstop として一度も効かない", entries*s, entries, s, g)
	}

	batch := quoteBatchSize(t)
	armed := armedWatchReserve(cfg, len(active))

	// 監視和集合の上限が **3秒(= 1 チャンク)に収まる**こと。収まらない値なら、
	// サイクルの残り全部が 6秒のままになる。
	worst := effective + armed + liveWatchReserve
	if worst > batch {
		t.Errorf("監視和集合の上限 %d が 1 チャンク(%d)を超える — 3秒に収まらない。"+
			"内訳: research %d + armed %d + live %d",
			worst, batch, effective, armed, liveWatchReserve)
	}
}

// countEntryArms は回る screener を入口へ畳んだ数(兄弟アームは 1 つに数える)。
func countEntryArms(active []strategy.Screener) int {
	seen := map[config.StrategyName]bool{}
	for _, sc := range active {
		seen[strategy.EntryArmOf(sc.Name())] = true
	}
	return len(seen)
}

// armedWatchReserve は「arm 済みだがまだ建玉になっていない銘柄」の上限。
// 1 ラウンドで arm されるのは per_strategy_n × 回る screener 数(全体の top_n が上限)。
func armedWatchReserve(cfg *config.BotConfig, arms int) int {
	n := cfg.Advisor.PerStrategyN * arms
	if cfg.Advisor.TopN > 0 && n > cfg.Advisor.TopN {
		n = cfg.Advisor.TopN
	}
	if n <= 0 {
		n = arms // knob 未設定の構成でも過小に見積もらない
	}
	return n
}

// 一括取得の 1 リクエスト上限は cmd/stockbot が正本(main パッケージなので import できない)。
// UNIV_TOP_N を scripts から読むのと同じ作法で、正本の値を直接読む。
func quoteBatchSize(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "cmd", "stockbot", "quote_chunks.go"))
	if err != nil {
		t.Fatalf("read quote_chunks.go: %v", err)
	}
	m := regexp.MustCompile(`tachibanaQuoteBatchSize = (\d+)`).FindSubmatch(b)
	if m == nil {
		t.Fatal("cmd/stockbot/quote_chunks.go に tachibanaQuoteBatchSize が見つからない")
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil || n <= 0 {
		t.Fatalf("tachibanaQuoteBatchSize が読めない: %q", m[1])
	}
	return n
}
