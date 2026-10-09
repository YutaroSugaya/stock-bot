package command

import (
	"path/filepath"
	"testing"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/strategy"
)

// 出荷 config(configs/bot_config.advisor.yaml)の**研究の契約**を縛る。
// 🛑 `internal/config` の中には置けない — このテストは `domain/strategy` を読む必要があり、
// strategy は config を import しているので import cycle になる。両方を import できる
// 層(usecase/command)に置く。
func researchBotConfig(t *testing.T) *config.BotConfig {
	t.Helper()
	cfg, err := config.LoadBotConfig(filepath.Join("..", "..", "..", "..", "configs", "bot_config.advisor.yaml"))
	if err != nil {
		t.Fatalf("load bot_config.advisor.yaml: %v", err)
	}
	return cfg
}

// 🚨 `top_n` が `per_strategy_n × スクリーナー数` を下回ると、
// **ペアの片方が次ラウンドに回る**。次ラウンドは価格が動いているので**建値がずれ**、
// ペア比較(同一銘柄・同一建値の差を見る)の前提が静かに壊れる。
// 兄弟アームを足すとスクリーナーが増えるので、コメントではなくテストで縛る —
// 8 → 12 になった時点で旧値 20 は初めて拘束していた。
func TestAdvisorTopNCoversEveryScreenerSlot(t *testing.T) {
	cfg := researchBotConfig(t)
	per := cfg.Advisor.PerStrategyN
	if per <= 0 {
		per = 1
	}
	need := per * len(strategy.DefaultScreeners())
	if cfg.Advisor.TopN < need {
		t.Fatalf("top_n = %d は per_strategy_n(%d) × スクリーナー数(%d) = %d を下回る — "+
			"ペアの片方が次ラウンドに回り、建値がずれてペア比較が壊れる",
			cfg.Advisor.TopN, per, len(strategy.DefaultScreeners()), need)
	}
}

// heartbeat を 1 分にしたまま snapshot 周期を分離し忘れると、`screen_snapshots` が
// 1 日 432,000 行(45 倍)になる。出荷 config で分離が生きていることを縛る。
func TestAdvisorSnapshotCadenceIsSeparatedFromTheArmCadence(t *testing.T) {
	cfg := researchBotConfig(t)
	if cfg.Advisor.IntervalMin >= cfg.Advisor.SnapshotIntervalMin {
		t.Fatalf("arm 周期 %d 分 >= snapshot 周期 %d 分 — 分離できていない(書込量が arm 頻度に比例する)",
			cfg.Advisor.IntervalMin, cfg.Advisor.SnapshotIntervalMin)
	}
}

// 事前登録値。heartbeat 60分 → **1分**。
// 🛑 リテラルで固定するのは、**建玉時刻の分布**が事前登録の対象だから。
// 採点結果を見てここを動かすのは
// 事後フィッティング — 変えるなら台帳へ新規登録する。
func TestAdvisorHeartbeatIsThePreRegisteredOneMinute(t *testing.T) {
	if got := researchBotConfig(t).Advisor.IntervalMin; got != 1 {
		t.Fatalf("interval_min = %d, want 1(事前登録値)。"+
			"変更は 事前登録の更新と同時に — 建玉時刻の分布が変わる", got)
	}
}

// 🛑 research は決定論 arm。出荷 config が LLM 経路に戻っていないことを縛る
// (戻ると 97% 通過フィルタという交絡が測定に復活する)。
func TestResearchConfigArmsDeterministically(t *testing.T) {
	if researchBotConfig(t).Advisor.UsesLLM() {
		t.Fatal("advisor_v2.arm_source=llm — LLM が arm 経路に戻っている(交絡が復活する)")
	}
}

// 🛑 **回す入口 6 つ**を config と同時更新で縛る
// (`catastrophe_guards_test` と同じ作法: yaml だけ変えても、テストだけ変えても落ちる)。
// トレンド系(donchian_v2 / atr_v2 / abs_momentum_v2 / high_52w)は**停止**であって棄却ではない —
// コードのメニュー(DefaultScreeners)からは消さず、この一覧で絞る。測定中に動かさない。
func TestResearchConfigEntriesAreTheCycle4PreRegisteredSix(t *testing.T) {
	cfg := researchBotConfig(t)
	want := []config.StrategyName{
		config.StrategyBNFReversion,
		config.StrategyBNFDay2Reversion,
		config.StrategyBNFStabilizedReversion,
		config.StrategyBNFIntradayReversion,
		config.StrategyPostJumpDrift,
		config.StrategyHighVolumePremium,
	}
	if len(cfg.Advisor.Entries) != len(want) {
		t.Fatalf("advisor_v2.entries = %v, want %v(事前登録の値)", cfg.Advisor.Entries, want)
	}
	for i := range want {
		if cfg.Advisor.Entries[i] != want[i] {
			t.Fatalf("advisor_v2.entries[%d] = %q, want %q", i, cfg.Advisor.Entries[i], want[i])
		}
	}
	active, err := strategy.ScreenersForEntries(cfg.Advisor.Entries)
	if err != nil {
		t.Fatalf("entries が解決できない: %v", err)
	}
	// 入口 6 / うち 4 に出口 2 通り = 10 アーム(hvp / pjd にはペアが無い)。
	if len(active) != 10 {
		t.Fatalf("回るアーム = %d, want 10: %v", len(active), active)
	}
	for _, s := range active {
		switch strategy.EntryArmOf(s.Name()) {
		case config.StrategyDonchianBreakoutV2, config.StrategyATRBreakoutV2,
			config.StrategyAbsMomentumV2, config.StrategyHigh52wMomentum:
			t.Errorf("トレンド系 %q が回っている — 事前登録で停止", s.Name())
		}
	}
}
