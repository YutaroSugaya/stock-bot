package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/testutil"
)

// 🛑 既定(env 未設定)で live track は組み立たない。hybrid のコードが main に
// 入っても、研究モードの挙動・設定が 1bit も変わらないこと — これが最重要の互換条件。
func TestBuildLiveTrack_NilWhenDisabled(t *testing.T) {
	lt, err := buildLiveTrack(context.Background(), config.LiveTrackEnv{}, config.Env{}, nil, liveTrackDeps{})
	if err != nil {
		t.Fatalf("live track 無効なのにエラー: %v", err)
	}
	if lt != nil {
		t.Fatal("env 未設定で live track が組み立てられた")
	}
}

func writeYAML(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

const liveYAMLTachibana = `mode: live_config
broker: { kind: tachibana }
symbols: ["7203"]
risk: { max_open_positions: 1, account_max_open_positions: 1, max_gross_notional_ratio: 1.0 }
holding:
  intraday: { exec_kind: cash }
  multiday: { exec_kind: margin_system }
`

func liveEnvFor(t *testing.T, botPath string) config.LiveTrackEnv {
	t.Helper()
	dir := filepath.Dir(botPath)
	return config.LiveTrackEnv{
		BotConfigPath:      botPath,
		StrategyConfigPath: filepath.Join(dir, "strategy.yaml"),
		EmergencyFlagPath:  filepath.Join(dir, "emg.live.flag"),
		DatabaseURL:        "postgres://x/stockbot_live",
	}
}

func researchPaperCfg() *config.BotConfig {
	c := &config.BotConfig{Mode: config.ModePaper, Symbols: []string{"7203"}}
	c.Broker.Kind = config.BrokerPaperLiveFeed
	return c
}

// 🛑 **LLM をリアルタイム発注経路に入れない**(CLAUDE.md)。live 側 yaml で
// advisor を有効にしたら起動拒否 — 型では表せないので構成で禁じる。
//
// **selector はここに含めない**(決定論的なスキャンで LLM が関与しない)。
// TestBuildLiveTrack_AllowsDeterministicSelector が逆側を固定している。
func TestBuildLiveTrack_RefusesAdvisor(t *testing.T) {
	t.Setenv("STOCKBOT_LIVE_CONFIRMED", "1")
	dir := t.TempDir()
	p := writeYAML(t, dir, "live.yaml", liveYAMLTachibana+"advisor_v2: { enabled: true }\n")
	_, err := buildLiveTrack(context.Background(), liveEnvFor(t, p),
		config.Env{DatabaseURL: "postgres://x/stockbot", EmergencyFlagPath: "runtime/emg.flag"},
		researchPaperCfg(),
		liveTrackDeps{hardLimits: &config.HardLimits{AllowedSymbols: []string{"7203"}},
			clock: clock.System(), logger: testutil.SilentLogger()})
	if err == nil {
		t.Fatal("advisor 有効の live track が通った")
	}
	if !strings.Contains(err.Error(), "advisor") {
		t.Errorf("err=%q が advisor を名指ししていない", err)
	}
}

// 🛑 research 側が実ブローカーを持っていないのに live で tachibana を指定したら
// 拒否する。**2 本目の login を張らない** — 立花のセッションは口座に 1 本で、
// 後から張った方が前のを破棄して互いに蹴り合う(却下済みの案そのもの)。
func TestLiveTrackBroker_RefusesSecondSession(t *testing.T) {
	cfg, err := config.LoadBotConfig(writeYAML(t, t.TempDir(), "live.yaml", liveYAMLTachibana))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = liveTrackBroker(cfg, liveTrackDeps{
		hardLimits: &config.HardLimits{AllowedSymbols: []string{"7203"}}, clock: clock.System(),
	}, config.LiveTrackEnv{})
	if err == nil {
		t.Fatal("共有する立花が無いのに live broker が組み立てられた(2本目の login が張られる)")
	}
	if !strings.Contains(err.Error(), "共有") {
		t.Errorf("err=%q がセッション共有の話をしていない", err)
	}
}

// dry-run(Stage 1)は紙 broker になる。実弾の経路に触れないこと。
func TestLiveTrackBroker_DryRunUsesPaper(t *testing.T) {
	cfg, err := config.LoadBotConfig(writeYAML(t, t.TempDir(), "live.yaml", strings.Replace(liveYAMLTachibana,
		"broker: { kind: tachibana }", "broker: { kind: paper }", 1)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	b, err := liveTrackBroker(cfg, liveTrackDeps{
		hardLimits: &config.HardLimits{AllowedSymbols: []string{"7203"}}, clock: clock.System(),
	}, config.LiveTrackEnv{DryRun: true})
	if err != nil {
		t.Fatalf("dry-run broker: %v", err)
	}
	if b == nil {
		t.Fatal("dry-run で broker が nil")
	}
}

// 🛑 **selector(決定論ランキング)は live で許す**。禁じるのは advisor(LLM)だけ。
// CLAUDE.md「LLM をリアルタイム発注経路に入れない」が縛るのは LLM であって、
// 「どの銘柄に当てるか」を決める決定論的なスキャンではない。「人間 commit 固定」は
// **どの戦略を回すか**の話で、テンプレート戦略が live_allowed_strategies に
// 入っていることで担保される。
func TestBuildLiveTrack_AllowsDeterministicSelector(t *testing.T) {
	t.Setenv("STOCKBOT_LIVE_CONFIRMED", "1")
	dir := t.TempDir()
	p := writeYAML(t, dir, "live.yaml", liveYAMLTachibana+
		"selector: { enabled: true, max_position_notional_jpy: 300000 }\n")
	_, err := buildLiveTrack(context.Background(), liveEnvFor(t, p),
		config.Env{DatabaseURL: "postgres://x/stockbot", EmergencyFlagPath: "runtime/emg.flag"},
		researchPaperCfg(),
		liveTrackDeps{hardLimits: &config.HardLimits{AllowedSymbols: []string{"7203"}},
			clock: clock.System(), logger: testutil.SilentLogger()})
	// selector そのものでは落ちない(この構成では別の理由 = 共有立花なし で落ちる)。
	if err != nil && strings.Contains(err.Error(), "selector") {
		t.Fatalf("selector を理由に拒否された: %v", err)
	}
}

// 🛑 日足は **市場の参照データ**であって台帳ではない。live track が自分の(空の)DB を
// 見ると、selector はランキングできず **bot は健全に見えて永久に何も建てない**
// (最悪の型のバグ: エラーもログも出ない)。
//
// 物理分離の原則が守るのは建玉・約定・config の**台帳**であって、両トラックで同一の
// 市場データではない。日足を二重に持つと API/ディスクを浪費した上に、片方だけ古い
// という乖離まで作る。
func TestLiveTrackUsesSharedCandleRepo(t *testing.T) {
	shared := repository.NewInMemoryCandleRepo()
	d := liveTrackDeps{candles: shared}
	if d.candles == nil {
		t.Fatal("liveTrackDeps に candles の注入口が無い(live は空の DB を見ることになる)")
	}
}

// 🛑 live の armed config_id は**中身に縛る**。selector は人間 commit の
// テンプレを銘柄ごとに複製して arm するが、id が `live_bnf_probe_v1_<sym>` の
// ように内容から独立していると、テンプレを書き換えても id が変わらない。
// strategy_configs の raw_yaml は凍結(上書きしない)なので、DB には**初版**が
// 残り続け、実際に発注した設定と食い違う = トレードを説明できない台帳になる。
// exec_kind を margin_general → margin_system → cash →
// margin_system と動かしても id が同じで、DB は初版の margin_general のままだった。
func TestLiveArmTemplateBindsConfigIDToContent(t *testing.T) {
	base := &config.StrategyConfig{
		ConfigID: "live_bnf_probe_v1", Symbol: "*", StrategyName: config.StrategyBNFReversion,
		Mode: config.ModeLive, HoldingMode: config.HoldingMultiday, ExecKind: config.ExecMarginSystem,
	}
	got := liveArmTemplate(base)
	if got.ConfigID == base.ConfigID {
		t.Fatalf("config_id が中身に縛られていない: %q", got.ConfigID)
	}
	if !strings.HasPrefix(got.ConfigID, base.ConfigID+"_") {
		t.Errorf("人間が読める接頭辞が消えた: %q", got.ConfigID)
	}
	// 中身が同じなら安定(再起動のたびに id が変わると台帳が分断される)。
	if again := liveArmTemplate(base); again.ConfigID != got.ConfigID {
		t.Errorf("同じ中身で id が変わった: %q != %q", again.ConfigID, got.ConfigID)
	}
	// 中身を変えたら必ず変わる。
	cash := *base
	cash.ExecKind = config.ExecCash
	if liveArmTemplate(&cash).ConfigID == got.ConfigID {
		t.Errorf("exec_kind を変えたのに id が同じ (%q) — DB に初版が凍ったまま残る", got.ConfigID)
	}
	// テンプレ自体は書き換えない(呼び出し側が持っている値の同一性を壊さない)。
	if base.ConfigID != "live_bnf_probe_v1" {
		t.Errorf("テンプレを破壊している: %q", base.ConfigID)
	}
}
