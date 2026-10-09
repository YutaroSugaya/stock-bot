package config

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"stockbot/backend/internal/domain/market"
)

// These tests load the REAL committed configs and pin the catastrophe floors — the
// values that let the bot trade and lose some without risking ruin. A config edit that
// weakens any of them fails the merge gate, so relaxation is always an explicit,
// reviewed, two-place (yaml + test) human commit.

func repoConfig(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("..", "..", "..", "configs", name)
}

func TestCatastropheGuards_HardLimits(t *testing.T) {
	hl, err := LoadHardLimits(repoConfig(t, "hard_limits.yaml"))
	if err != nil {
		t.Fatalf("load hard_limits: %v", err)
	}

	if len(hl.AllowedSymbols) == 0 {
		t.Fatal("allowed_symbols must stay non-empty (fail-close)")
	}
	// ホワイトリストの件数を固定する(JPX プライム内国株式の4文字コードのみ)。ここは日次
	// ユニバースを選ぶプール。件数を固定するのは、上場/廃止で更新するときに「何銘柄変わったか」を
	// 人間が必ず一度見るため — JPX data_j.xls を取り直し、この定数と差分の理由を同じ commit に書く。
	const wantAllowed = 1544
	if len(hl.AllowedSymbols) != wantAllowed {
		t.Fatalf("allowed_symbols = %d 銘柄, want %d — プールの増減は人間 commit(yaml とこのテストを同時に更新すること)",
			len(hl.AllowedSymbols), wantAllowed)
	}
	// 形式ガード: 東証の4文字コード(数字4桁 または 数字3桁+英大文字1)。
	// 5文字コード(優先株・社債型種類株式)や貼り付け事故を弾く。重複も拒否する
	// (重複は「1銘柄に価格ループが2本」の芽になる)。
	seen := map[string]bool{}
	for _, s := range hl.AllowedSymbols {
		if seen[s] {
			t.Fatalf("allowed_symbols に %q が重複している", s)
		}
		seen[s] = true
		if len(s) != 4 {
			t.Fatalf("allowed_symbols の %q は4文字ではない(優先株等の5文字コードは普通株ではないので入れない)", s)
		}
		for i, r := range s {
			ok := r >= '0' && r <= '9'
			if i == 3 {
				ok = ok || (r >= 'A' && r <= 'Z') // 285A のような新形式
			}
			if !ok {
				t.Fatalf("allowed_symbols の %q は東証コードの形式ではない", s)
			}
		}
	}
	if hl.Quantity.Max > 3000 {
		t.Fatalf("quantity.max %d > 3000: lot ceiling raised — edge proof + human review required", hl.Quantity.Max)
	}
	// order_boundary is the absolute fat-finger backstop; 0 would disable it.
	// 1トレード上限は口座サイズの 1% を超えないこと — 絶対額で固定すると、口座を
	// 小さくしたときに上限だけが取り残される(paper.balance_jpy が正本)。
	balance := hl.Paper.BalanceJPY
	if balance <= 0 {
		balance = 1_000_000 // paper broker の既定
	}
	maxAllowed := balance / 100
	if hl.OrderBoundary.MaxLossPerTradeJPY <= 0 || hl.OrderBoundary.MaxLossPerTradeJPY > maxAllowed {
		t.Fatalf("order_boundary.max_loss_per_trade_jpy %d must be in (0, %d] (= 口座 %d 円の 1%%)",
			hl.OrderBoundary.MaxLossPerTradeJPY, maxAllowed, balance)
	}
	// 損切り/利確の上限は建値比 %。出口は ATR 倍数(SL=2.0×ATR)で、高ボラ銘柄の 2×ATR は
	// 建値比 30% 近くになりうる。上限が低いとそのエントリーがシグナルの後に弾かれて標本がボラで
	// censoring されるので 33 とし、建値の 1/3 を超える損切り(= 桁違い)だけを弾く。
	if hl.OrderBoundary.MaxStopLossPct <= 0 || hl.OrderBoundary.MaxStopLossPct > 33 {
		t.Fatalf("order_boundary.max_stop_loss_pct %g must be in (0, 33] (建値比%%)", hl.OrderBoundary.MaxStopLossPct)
	}
	if hl.OrderBoundary.MaxTakeProfitPct <= 0 || hl.OrderBoundary.MaxTakeProfitPct > 100 {
		t.Fatalf("order_boundary.max_take_profit_pct %g must be in (0, 100] (建値比%%)", hl.OrderBoundary.MaxTakeProfitPct)
	}
	// TP 上限は SL 上限 × 1.5(= 出口の R:R)。片方だけ動かすと「SL は通るが TP で
	// 落ちる」不整合な弾かれ方をして、原因が読めない reject が積み上がる。
	if want := hl.OrderBoundary.MaxStopLossPct * 1.5; hl.OrderBoundary.MaxTakeProfitPct != want {
		t.Fatalf("order_boundary.max_take_profit_pct %g must equal max_stop_loss_pct × 1.5 = %g (出口の R:R と一致させる)",
			hl.OrderBoundary.MaxTakeProfitPct, want)
	}
	// 出口を ATR 化した以上、%上限は「1トレード最大損失(円)」と整合していること。
	// 資金キャップ(単元 ≤ N 円)× SL上限% が per-trade 上限を超えていたら、
	// 高い方が binding して意図しない censoring が起きる。
	if lot := 1_000_000.0; lot*hl.OrderBoundary.MaxStopLossPct/100 > float64(hl.OrderBoundary.MaxLossPerTradeJPY) {
		t.Fatalf("単元上限 %.0f円 × SL上限 %g%% = %.0f円 が max_loss_per_trade_jpy %d を超える — 円の天井が先に効いて標本が削れる",
			lot, hl.OrderBoundary.MaxStopLossPct, lot*hl.OrderBoundary.MaxStopLossPct/100, hl.OrderBoundary.MaxLossPerTradeJPY)
	}
	// Margin floors (立花e支店 公式値; loosening these risks 追証).
	if hl.Margin.RequiredRate < 0.30 {
		t.Fatalf("margin.required_rate %.2f < 0.30", hl.Margin.RequiredRate)
	}
	if hl.Margin.MaintenanceRatio < 0.25 {
		t.Fatalf("margin.maintenance_ratio %.2f < 0.25", hl.Margin.MaintenanceRatio)
	}
	if hl.Margin.MinCollateralJPY < 300000 {
		t.Fatalf("margin.min_collateral_jpy %d < 300000", hl.Margin.MinCollateralJPY)
	}
	// **実運用が読む active config** を実 hard_limits に当てる。example 2 本は
	// engine_wiring_test が見ているが active.yaml だけ穴だった。ここが緑でないと、
	// range を絞った瞬間に起動時 `loadActiveConfigOrNil` が Warn だけ出して
	// 黙って no_trade へ縮退する(cmd/stockbot/main.go)。
	ac, err := LoadStrategyConfig(repoConfig(t, "strategy_config.active.yaml"))
	if err != nil {
		t.Fatalf("load strategy_config.active.yaml: %v", err)
	}
	if ac.Symbol == "*" { // UNIVERSE config は起動時に実銘柄へ展開される
		ac.Symbol = hl.AllowedSymbols[0]
	}
	if err := ac.ValidateAgainstHardLimits(hl); err != nil {
		t.Fatalf("strategy_config.active.yaml が hard_limits に適合しない — 起動時に黙って no_trade へ落ちる: %v", err)
	}
	// The 引け前フラット化 invariant: cutoff/flatten must stay before the bell.
	if hl.SessionHours.EntryCutoff != "14:55" {
		t.Fatalf("entry_cutoff %q must stay 14:55", hl.SessionHours.EntryCutoff)
	}
	if hl.SessionHours.ForceFlatAt != "14:50" {
		t.Fatalf("force_flat_at %q must stay 14:50", hl.SessionHours.ForceFlatAt)
	}
	if len(hl.SessionHours.Holidays) == 0 {
		t.Fatal("session_hours.holidays must not be empty — the venue holiday calendar is a trading guard (年次更新)")
	}
	// calendar_through は fail-close horizon。必須にすることで、年次のカレンダー更新が
	// holidays とこの horizon を同じ reviewed commit で触ることを強制する。
	if hl.SessionHours.CalendarThrough == "" {
		t.Fatal("session_hours.calendar_through must be set — it is the fail-close horizon for the holiday calendar (年次更新)")
	}
	th, err := hl.SessionHours.TradingHours()
	if err != nil {
		t.Fatalf("session_hours must convert cleanly: %v", err)
	}
	if th.CalendarThrough.IsZero() {
		t.Fatal("calendar_through must parse to a concrete date (fail-close horizon)")
	}

	// Adding a strategy to the live allowlist is the human PROMOTION commit; this
	// assertion forces the same reviewed commit to update this test — no silent promotion.
	promoted := map[string]bool{
		// 実弾で回してよい戦略だけを true にする。外した戦略はエントリごと消す(true のまま残すと
		// yaml に 1 行足すだけで戻せてしまい、二重ゲートが片側だけになる)。
		// 同梱の allowlist は空。昇格するときは yaml とここを同じ commit で足す。
	}
	for _, s := range hl.LiveAllowedStrategies {
		if !promoted[s] {
			t.Fatalf("live_allowed_strategies contains %q which is not acknowledged in catastrophe_guards_test.go — promotion must update BOTH the yaml and this test in one reviewed commit", s)
		}
	}
}

func TestCatastropheGuards_CommittedDefaultsAreFailSafe(t *testing.T) {
	// The committed bot_config must stay paper: live is only reachable via a
	// gitignored bot_config.live.yaml + STOCKBOT_LIVE_CONFIRMED=1 (human gate).
	bc, err := LoadBotConfig(repoConfig(t, "bot_config.yaml"))
	if err != nil {
		t.Fatalf("load bot_config: %v", err)
	}
	if bc.Mode == ModeLive {
		t.Fatal("committed bot_config.yaml must never default to live_config")
	}
	if bc.Advisor.Enabled {
		t.Fatal("committed bot_config.yaml must keep the LLM advisor disabled (CLAUDE.md)")
	}

	// bot_config.advisor.yaml is the ONE committed config that enables the LLM advisor,
	// so it must stay paper: advisor-enabled + live_config would put LLM-authored
	// configs on a real-money path.
	adv, err := LoadBotConfig(repoConfig(t, "bot_config.advisor.yaml"))
	if err != nil {
		t.Fatalf("load bot_config.advisor.yaml: %v", err)
	}
	if adv.Mode != ModePaper {
		t.Fatalf("bot_config.advisor.yaml enables the advisor so it MUST be paper_config, got %q", adv.Mode)
	}
	// 執行が紙である broker だけを許す。paper_live_feed は価格/日足だけ実市場から引き、
	// 実弾経路は型として存在しない(発注メソッドを持たない port.MarketFeed で保持する)。
	// 素の paper(固定価格)では損益が実勢と無関係になるので paper_live_feed を使う。
	switch adv.Broker.Kind {
	case BrokerPaper, BrokerPaperLiveFeed:
	default:
		t.Fatalf("bot_config.advisor.yaml must use a paper-execution broker (paper|paper_live_feed), got %q", adv.Broker.Kind)
	}

	// The committed active strategy config must stay no_trade until an edge is
	// proven and a human commits the switch (CLAUDE.md エッジ規律).
	sc, err := LoadStrategyConfig(repoConfig(t, "strategy_config.active.yaml"))
	if err != nil {
		t.Fatalf("load strategy_config.active: %v", err)
	}
	if sc.StrategyName != StrategyNoTrade {
		t.Fatalf("committed strategy_config.active.yaml names %q; the committed default must stay no_trade", sc.StrategyName)
	}
}

func TestStrategyConfig_YAMLTextRoundTrips(t *testing.T) {
	// YAMLText feeds strategy_configs.raw_yaml (audit). It must render the
	// live-relevant fields and parse back to the same config.
	c := &StrategyConfig{ConfigID: "cfg-yaml", Symbol: "7203", StrategyName: StrategyName("bnf_reversion"), Mode: ModePaper}
	c.Entry.Direction = DirectionBuyOnly
	c.Risk.Quantity = 100
	c.Tuning = map[string]float64{"bnf_dev": -0.12}

	text := c.YAMLText()
	if text == "" {
		t.Fatal("YAMLText must not be empty for a real config")
	}
	var back StrategyConfig
	if err := yaml.Unmarshal([]byte(text), &back); err != nil {
		t.Fatalf("rendered YAML must parse back: %v", err)
	}
	if back.ConfigID != c.ConfigID || back.Symbol != c.Symbol || back.StrategyName != c.StrategyName ||
		back.Risk.Quantity != 100 || back.Tuning["bnf_dev"] != -0.12 {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
	var nilCfg *StrategyConfig
	if nilCfg.YAMLText() != "" {
		t.Fatal("nil config must render empty (degrade, not panic)")
	}
}

// AdvisorRunID は runtime が刻む出自メタデータ(migration 0007)。監査 raw_yaml に
// 混入せず、YAML から注入もできない(`yaml:"-"`)。
func TestStrategyConfig_AdvisorRunIDStaysOutOfYAML(t *testing.T) {
	c := &StrategyConfig{ConfigID: "cfg-run", Symbol: "7203", StrategyName: StrategyName("bnf_reversion"), Mode: ModePaper}
	c.AdvisorRunID = "20260731T000000Z-deadbeef"
	if text := c.YAMLText(); strings.Contains(text, "advisor_run_id") || strings.Contains(text, c.AdvisorRunID) {
		t.Fatalf("advisor_run_id が raw_yaml に混入した:\n%s", text)
	}
	var back StrategyConfig
	if err := yaml.Unmarshal([]byte("config_id: x\nadvisor_run_id: forged\n"), &back); err != nil {
		t.Fatalf("unknown key は無害に無視されるべき: %v", err)
	}
	if back.AdvisorRunID != "" {
		t.Fatalf("YAML から advisor_run_id が注入された: %q", back.AdvisorRunID)
	}
}

func TestHardLimits_AllowsLiveStrategy(t *testing.T) {
	hl := &HardLimits{LiveAllowedStrategies: []string{"bnf_reversion"}}
	if !hl.AllowsLiveStrategy(StrategyNoTrade) {
		t.Fatal("no_trade must always be live-allowed")
	}
	if !hl.AllowsLiveStrategy(StrategyName("bnf_reversion")) {
		t.Fatal("an allowlisted strategy must pass")
	}
	if hl.AllowsLiveStrategy(StrategyName("time_series_momentum")) {
		t.Fatal("a non-allowlisted strategy must fail-close")
	}
	empty := &HardLimits{}
	if empty.AllowsLiveStrategy(StrategyName("bnf_reversion")) {
		t.Fatal("an empty allowlist must refuse every real strategy")
	}
}

// 有限の timeout を戻すと毎 run が timeout し、fail-closed で config を一切 arm しない
// (= advisor 経路の forward 収集が丸ごと消える)。長考するモデルは 300 秒でも未完了だった。
// 戻すなら先に実測で完了時間を確かめること。
func TestAdvisorConfig_NoTimeoutLimit(t *testing.T) {
	cfg, err := LoadBotConfig(repoConfig(t, "bot_config.advisor.yaml"))
	if err != nil {
		t.Fatalf("load advisor config: %v", err)
	}
	if cfg.Advisor.TimeoutSeconds >= 0 {
		t.Fatalf("advisor.timeout_seconds は負値(制限なし)であること: got %d", cfg.Advisor.TimeoutSeconds)
	}
}

// 資金キャパシティ・フィルタが出荷 config から消えないことを強制する。値がさ株を混ぜて測ると
// 実弾で再現できない成績が台帳に載る。結果を見てから緩めるのは事後の勝ち探し。
func TestAdvisorConfigKeepsNotionalCap(t *testing.T) {
	cfg, err := LoadBotConfig(repoConfig(t, "bot_config.advisor.yaml"))
	if err != nil {
		t.Fatalf("load bot_config.advisor.yaml: %v", err)
	}
	// ユニバース規則の単価上限と同じ値。paper は live より母集団が広いので、live の段階判定は
	// `forward-report -max-notional-jpy` の部分集合で読む。
	const want = 2_000_000
	if got := cfg.Selector.MaxPositionNotionalJPY; got != want {
		t.Fatalf("max_position_notional_jpy = %d, want %d — 事前登録値(結果を見てから緩めない)", got, want)
	}
	// 決定論セレクタは advisor 経路と二重に arm するので同時には有効化しない。
	if cfg.Selector.Enabled {
		t.Fatal("bot_config.advisor.yaml で selector.enabled=true は不可(advisor 経路と二重 arm)")
	}
}

// 日次ユニバースの契約を出荷 config 側で固定する。これが
// 無いと、symbols_file を消して静的リストを貼り戻しても全テストが緑のまま通る。
func TestResearchConfigsUseTheDailyUniverseFile(t *testing.T) {
	for _, name := range []string{"bot_config.advisor.yaml"} {
		cfg, err := LoadBotConfig(repoConfig(t, name))
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		if cfg.SymbolsFile == "" {
			t.Fatalf("%s は symbols_file(毎朝の日次ユニバース)を持つこと — 静的リストへ戻すのは事前登録の変更", name)
		}
		if len(cfg.Symbols) != 0 {
			t.Fatalf("%s に静的な symbols: を併記しない(%d 件)。symbols_file が読めなかったときに"+
				"静的リストへ縮退したように見え、どちらで走ったのか後から判別できなくなる", name, len(cfg.Symbols))
		}
		// account cap の下限は **research_cap_test.go に 1 本だけ**置く
		// (`TestResearchAccountCapExceedsUniverseTimesArms`)。弱い条件を 2 箇所に置くと、
		// 強い方を直しても弱い方が「通っている」と嘘をつく。
	}
}

// 呼値テーブルが hard_limits.allowed_symbols から導出されたものであることを強制する。
// プールを広げたときに手書きの表が取り残されると、新しい銘柄だけが粗いテーブルへ倒れて紙執行の
// 往復コストが数倍で記録され、「入れ替えた銘柄は成績が悪い」一方向のバイアスになる。
func TestFineTickTableMatchesAllowedSymbols(t *testing.T) {
	hl, err := LoadHardLimits(repoConfig(t, "hard_limits.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got, want := market.FineTickPoolSize, len(hl.AllowedSymbols); got != want {
		t.Errorf("呼値テーブルの導出プール = %d 銘柄, allowed_symbols = %d 銘柄 — "+
			"プールを変えたら `go run ./cmd/tick-table -allow-file ../configs/hard_limits.yaml` で再生成すること", got, want)
	}
	syms := append([]string(nil), hl.AllowedSymbols...)
	sort.Strings(syms)
	sum := sha256.Sum256([]byte(strings.Join(syms, ",")))
	want := hex.EncodeToString(sum[:])[:16]
	if market.FineTickPoolDigest != want {
		t.Errorf("呼値テーブルの導出プール digest = %s, allowed_symbols = %s — "+
			"プールの中身が変わっている。`go run ./cmd/tick-table -allow-file ../configs/hard_limits.yaml` で再生成し、diff を人間が読むこと",
			market.FineTickPoolDigest, want)
	}
}

// --- hybrid(実弾 × paper 並走)の破局ガード ---
//
// 🛑 これらは「設定を間違えたら実弾が紙になる / 紙が実弾になる」経路を塞ぐ。
// 型では表現できない env の組み合わせなので、テストが唯一の歯止め。

// live track は **env 未設定で完全に無効**。hybrid のコードが main に入っても
// 研究モードの挙動・設定が 1bit も変わらないこと。
func TestCatastropheGuards_HybridOffByDefault(t *testing.T) {
	t.Setenv("STOCKBOT_LIVE_BOT_CONFIG", "")
	t.Setenv("STOCKBOT_LIVE_DATABASE_URL", "")
	t.Setenv("STOCKBOT_LIVE_STRATEGY_CONFIG", "")
	t.Setenv("STOCKBOT_LIVE_EMERGENCY_FLAG", "")
	t.Setenv("STOCKBOT_LIVE_TRACK_DRYRUN", "")
	if LoadLiveTrackEnv().Enabled() {
		t.Fatal("env 未設定で live track が有効になっている")
	}
}

// STOCKBOT_LIVE_BOT_CONFIG **だけ**を立てても、残りが揃うまで起動しない。
// 「とりあえず live を足してみる」が半端な構成で走り出さないこと。
func TestCatastropheGuards_HybridNeedsEveryLivePath(t *testing.T) {
	research := &BotConfig{Mode: ModePaper}
	research.Broker.Kind = BrokerPaperLiveFeed
	live := &BotConfig{Mode: ModeLive}
	live.Broker.Kind = BrokerTachibana
	env := Env{DatabaseURL: "postgres://x/stockbot", EmergencyFlagPath: "runtime/emergency_stop.flag"}

	only := LiveTrackEnv{BotConfigPath: "configs/bot_config.live.yaml"}
	if err := ValidateHybrid(research, live, env, only); err == nil {
		t.Fatal("BotConfigPath だけで live track が起動できてしまう")
	}
}

// dry-run(Stage 1 の配線確認)が**本番口座**と併用できないこと。
// ここが唯一「配線確認のつもりが実弾」を塞ぐ場所。
func TestCatastropheGuards_HybridDryRunNeverTouchesProduction(t *testing.T) {
	t.Setenv("STOCKBOT_TACHIBANA_ENV", "production")
	research := &BotConfig{Mode: ModePaper}
	research.Broker.Kind = BrokerPaperLiveFeed
	live := &BotConfig{Mode: ModeLive}
	live.Broker.Kind = BrokerPaper
	env := Env{DatabaseURL: "postgres://x/stockbot", EmergencyFlagPath: "runtime/emergency_stop.flag"}
	le := LiveTrackEnv{
		BotConfigPath: "a.yaml", StrategyConfigPath: "b.yaml",
		EmergencyFlagPath: "runtime/emergency_stop.live.flag",
		DatabaseURL:       "postgres://x/stockbot_live", DryRun: true,
	}
	if err := ValidateHybrid(research, live, env, le); err == nil {
		t.Fatal("STOCKBOT_LIVE_TRACK_DRYRUN=1 が production で通った")
	}
}

// 🛑 live の例 config が **レバ 1.0 倍以内**で出荷されること。
// 実弾は保証金の 1.0 倍以内から開始する。委託保証金率 0.33 のぶん余力はレバ 3.0 倍まであり、
// 満額で建てると bot の 30% ブレーカーが建値比 −3.0% で発火する = 構造的に運用不能。
// 上げるのは人間 commit。
func TestCatastropheGuards_LiveExampleStartsAtOneTimesLeverage(t *testing.T) {
	c, err := LoadBotConfig("../../../configs/bot_config.live.example.yaml")
	if err != nil {
		t.Fatalf("load live example: %v", err)
	}
	got := c.Risk.MaxGrossNotionalRatio
	if got <= 0 {
		t.Fatal("live の例 config で max_gross_notional_ratio が無効(0)— レバ規律が機械強制されていない")
	}
	if got > 1.0 {
		t.Errorf("max_gross_notional_ratio=%v — 実弾は 1.0 倍(現物相当)から開始する事前コミット", got)
	}
}
