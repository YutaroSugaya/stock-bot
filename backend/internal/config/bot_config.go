package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BotConfig is the live-mutable operating config. Fail-safe default mode is paper.
type BotConfig struct {
	Mode    Mode      `yaml:"mode"`
	Broker  BrokerCfg `yaml:"broker"`
	Symbols []string  `yaml:"symbols"`
	// SymbolsFile は毎朝 cmd/universe-screen が書く日次ユニバース(1行1銘柄・先頭 `~` 展開)。
	// 指定時は Symbols より優先し、読めない/空なら起動拒否 — 黙って静的リストへ縮退すると
	// 「ユニバースを絞ったつもりで絞れていない」事故になる。
	SymbolsFile string      `yaml:"symbols_file"`
	Risk        RiskCfg     `yaml:"risk"`
	Advisor     AdvisorCfg  `yaml:"advisor_v2"`
	Holding     HoldingCfg  `yaml:"holding"`
	Selector    SelectorCfg `yaml:"selector"`

	// symbolsResolved latches when ResolveSymbols has run; ValidateAgainstHardLimits
	// refuses an unresolved SymbolsFile — 呼び忘れを起動経路で塞ぐ。
	symbolsResolved bool
}

// SelectorCfg controls the deterministic universe Selector (scan wide / arm narrow):
// all symbols start no_trade and the best-ranked candidate is armed while a position
// slot is free. It never disarms a symbol that holds a position. Default OFF.
type SelectorCfg struct {
	Enabled                bool `yaml:"enabled"`
	RearmIntervalMin       int  `yaml:"rearm_interval_min"`        // re-rank cadence; 0 = default
	MaxPositionNotionalJPY int  `yaml:"max_position_notional_jpy"` // arm-time filter: skip a symbol whose round lot (latest close × config quantity) exceeds this; 0 = disabled
	// PerStrategyNotionalJPY は **戦略ごと**の建玉金額上限。載っていない戦略は
	// MaxPositionNotionalJPY に倒す。
	//
	// 🛑 優先ティア(複数戦略の live)で要る: 同じ口座で bnf は 550,000 まで拾い、
	// donchian_breakout_v2_trail は 300,000 までに抑える、という指定を 1 つの数字では
	// 書けない。
	PerStrategyNotionalJPY map[string]int `yaml:"per_strategy_notional_jpy"`
}

// NotionalCapFor は戦略の建玉金額上限。個別指定が無ければ全体の既定に倒す。
func (c SelectorCfg) NotionalCapFor(name StrategyName) int {
	if v, ok := c.PerStrategyNotionalJPY[string(name)]; ok {
		return v
	}
	return c.MaxPositionNotionalJPY
}

// BrokerCfg selects the broker adapter.
type BrokerCfg struct {
	Kind BrokerKind `yaml:"kind"`
}

// RiskCfg holds the account-wide and per-symbol risk caps.
type RiskCfg struct {
	MaxDailyLossJPY         int `yaml:"max_daily_loss_jpy"`
	MaxConsecutiveLosses    int `yaml:"max_consecutive_losses"`
	MaxOpenPositions        int `yaml:"max_open_positions"`
	AccountMaxOpenPositions int `yaml:"account_max_open_positions"`
	AccountMaxDailyLossJPY  int `yaml:"account_max_daily_loss_jpy"`

	// --- 監視銘柄の予算。0 = 無効 ---
	//
	// 🚨 **本数(account_max_open_positions)とは目的が違う。** 時価は 1 リクエストに
	// 120 銘柄まで積め、`BatchQuoteFeed` は 121 銘柄目で間隔をチャンク数だけ伸ばして
	// **通信量を一定に保つ**。つまり監視銘柄が増えても API 回数は 1 回も増えず、
	// 代わりに時価の実効間隔が 3秒 → 6秒 → 9秒 と落ちる(実測で 6秒へ落ちた)。
	// 監視集合は **トラック間で共有**なので、paper の建玉が live の
	// OnTick 決済判定と、バックテストの入力である分足の密度まで道連れにする。
	//
	// 同じ銘柄に 12 アーム乗っても監視集合は 1 銘柄なので、**建玉を増やすこと自体は
	// タダ**。だから枠は銘柄で持ち、**既に保有している銘柄への建ては枠を消費しない**
	// (risk gate)。「少ない銘柄に多くのアームを重ねて N を稼ぐ」方向のバイアスになる。

	// AccountMaxOpenSymbols は同時に建玉を持てる **銘柄数**の上限。
	AccountMaxOpenSymbols int `yaml:"account_max_open_symbols"`
	// PerEntryMaxOpenSymbols は **入口ごと**(兄弟アーム `X` / `X_trail` は同じ枠)の
	// 銘柄数上限。全体の枠を先着順で配ると発火の多い入口が食い尽くし、たまにしか
	// 発火しない入口が新しい銘柄を開けない(実測: donchian + atr が
	// 457 建玉中 322 本 = 70%、bnf は 3 本)。枠の**予約**として効く。
	// 🛑 アームではなく入口で数える。アーム単位だと枠の境界でペアの 2 本目だけが
	// 弾かれ、ペア差が構造的に測れなくなる。
	PerEntryMaxOpenSymbols int `yaml:"per_entry_max_open_symbols"`
	// MaxGrossNotionalRatio は建玉合計の上限を保証金の何倍にするか。
	// **0 = 無効**(research は対象外 — 紙で資本リスクがゼロ、かつ全トリガー採用の
	// サンプルを censoring したくない)。live の現在値と上げ方は事前にコミットした
	// 梯子(人間 commit)が SSOT — ここに数字を焼かない(焼くと必ず古くなる)。
	// 委託保証金率(required_rate 0.33)はレバ 3.0 倍まで**建てられる**
	// という broker 側の上限で、我々が自分に課す上限ではない。
	MaxGrossNotionalRatio float64 `yaml:"max_gross_notional_ratio"`
	// MaxRiskPerTradeJPY は **1本あたりの計画損失**(建値から SL までの距離 × 株数)の
	// 上限。**0 = 無効**(research / harvest は紙で資本リスクがゼロなので、全トリガー
	// 採用の標本をこの理由で censoring しない — MaxGrossNotionalRatio と同じ扱い)。
	//
	// 🛑 **SL を狭める設定ではない。** 出口の幾何は戦略が決めたまま動かさず、「その SL
	// だと 1 本で上限を超える」候補を建てないだけ。詳細は
	// `risk.AccountSnapshot.MaxRiskPerTradeJPY`(なぜ狭める案を採らなかったかも含む)。
	//
	// 🚨 **損失の上限ではない** — ギャップとストップ安は逆指値をすり抜ける。
	MaxRiskPerTradeJPY           int  `yaml:"max_risk_per_trade_jpy"`
	DisableConsecutiveLossGuards bool `yaml:"disable_consecutive_loss_guards"`
	// AccountMaxEntriesPerDay は口座全体の「1 営業日の新規本数」の上限。**0 = 無効**(research は
	// サイクルの事前登録を動かさない)。理由は `risk.AccountSnapshot.AccountMaxEntriesPerDay`。
	AccountMaxEntriesPerDay int `yaml:"account_max_entries_per_day"`
}

// AdvisorCfg は paper の **arm ループ**(`advisor_v2` ブロック)の設定。キー名は履歴の都合で
// advisor のまま。arm の config を何で作るかは ArmSource が決め、LLM は既定で使わない。
// LLM は発注経路に入らない — config 生成 / go-no-go だけ。
type AdvisorCfg struct {
	Enabled       bool `yaml:"enabled"` // arm ループを回すか
	MaxConcurrent int  `yaml:"max_concurrent"`

	// ArmSource は arm する config の作り方。空 / `template` = 決定論のテンプレート
	// (command.ArmTemplate)、`llm` = LLM advisor。枠の配り方・
	// ランキング・risk gate・発注はどちらでも同じで、差し替わるのは config 生成の 1 点だけ。
	//
	// 🛑 template はLLM の性能への評価ではなく audition 設計の帰結 — 戦略を公平に測るとは
	// 戦略以外の要因(= 交絡)を取り除くこと。**LLM のコードとプロンプトは消さない**
	// (復活の条件と見直し日は ARCHITECTURE.md の advisor の節)。
	ArmSource ArmSource `yaml:"arm_source"`

	// Entries は paper で**回す入口**の一覧。
	// コードのメニュー(`strategy.DefaultScreeners()`)から入口単位で絞る — 兄弟 `_trail` は
	// 入口と一緒に通る。**空 = 絞らない**(このキーを持たない config はメニュー全部で回る)。
	// 🛑 綴りの誤り・兄弟名・重複は起動時 error(`strategy.ScreenersForEntries`・fail-close)。
	// 🛑 サイクル中に動かさない。稼働中の一覧は `research_config_guard_test` が固定する。
	Entries []StrategyName `yaml:"entries"`

	// When Enabled, the advisor loop drives arming and the deterministic Selector's
	// auto-arm is left off (they would otherwise fight over holders).
	CLIPath         string `yaml:"cli_path"`          // path to the `claude` binary
	PromptPath      string `yaml:"prompt_path"`       // prompts/generate_strategy_config.md
	WorkingDir      string `yaml:"working_dir"`       // subprocess CWD (repo root; empty = inherit)
	OutputDir       string `yaml:"output_dir"`        // raw advisor stdout archive dir (audit); empty = ~/.stockbot/ai_output
	PreOpenMin      int    `yaml:"pre_open_min"`      // 寄り前ウォームアップ(分): その日の最初の LLM ラウンドを寄り N 分前に前倒す。0=無効(場中のみ)
	ScanIntervalSec int    `yaml:"scan_interval_sec"` // deterministic universe scan cadence (no LLM cost); 0 = 60
	IntervalMin     int    `yaml:"interval_min"`      // LLM heartbeat: max quiet period in minutes; 0 = 60. New triggers fire immediately regardless.
	TimeoutSeconds  int    `yaml:"timeout_seconds"`   // per-call timeout; 0 = 120, 負値 = 制限なし
	TopN            int    `yaml:"top_n"`             // 1ラウンドの全体上限(実行時間/コストの安全弁); 0 = 1
	PerStrategyN    int    `yaml:"per_strategy_n"`    // **戦略あたり**の上限。枠を戦略に配る(スコアは戦略間で比較不能); 0 = 1

	// SnapshotIntervalMin は `screen_snapshots` の書込周期(分)。0 = arm ラウンドごと。
	// 🚨 heartbeat を 1 分にすると arm は 1 日 約270 ラウンドになる。snapshot は
	// 1 ラウンド 1,600 行なので、周期を共有したままだと 432,000 行/日(45 倍)。
	// ランキングは日中不変で毎回ほぼ同一内容なので、**分離すれば書込量は増えない**。
	SnapshotIntervalMin int `yaml:"snapshot_interval_min"`
}

// HoldingCfg sets the default holding mode and per-mode exec kind.
type HoldingCfg struct {
	Intraday struct {
		ExecKind ExecKind `yaml:"exec_kind"`
	} `yaml:"intraday"`
	Multiday struct {
		ExecKind ExecKind `yaml:"exec_kind"`
	} `yaml:"multiday"`
}

// LoadBotConfig reads and parses bot_config.yaml, defaulting to a fail-safe
// paper configuration on missing fields.
func LoadBotConfig(path string) (*BotConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read bot_config %q: %w", path, err)
	}
	var c BotConfig
	if err := decodeStrict(b, &c); err != nil {
		return nil, fmt.Errorf("parse bot_config %q: %w", path, err)
	}
	if c.Mode == "" {
		c.Mode = ModePaper
	}
	// A typo'd mode must fail closed — never fall through as "not live" while a real
	// broker still trades.
	if !c.Mode.Valid() {
		return nil, fmt.Errorf("bot_config mode %q is not a recognised mode (paper_config|live_config|disabled)", c.Mode)
	}
	if c.Broker.Kind == "" {
		c.Broker.Kind = BrokerPaper
	}
	switch c.Advisor.ArmSource {
	case "", ArmSourceTemplate, ArmSourceLLM:
	default:
		return nil, fmt.Errorf("bot_config advisor_v2.arm_source %q is not recognised (template|llm)", c.Advisor.ArmSource)
	}
	return &c, nil
}

// ArmSource は arm する config の作り方(`advisor_v2.arm_source`)。
type ArmSource string

const (
	ArmSourceTemplate ArmSource = "template" // 決定論のテンプレート(既定)
	ArmSourceLLM      ArmSource = "llm"      // LLM advisor(明示したときだけ)
)

// UsesLLM は arm の config を LLM に作らせるか。空は template(LLM は既定 OFF)。
func (a AdvisorCfg) UsesLLM() bool { return a.ArmSource == ArmSourceLLM }

// ResolveSymbols replaces Symbols with the contents of SymbolsFile (1行1銘柄・`#` 以降と
// 空行は無視)。読めない / 空 / 重複 は起動拒否で、静的な Symbols へは縮退しない。Call once
// at startup, BEFORE ValidateAgainstHardLimits.
//
// LoadBotConfig があえて呼ばないのは、config を読むだけのテスト・ツールが毎朝生成される
// 外部ファイルの存在に縛られると、朝のジョブが走る前は make check が赤くなるため。
func (c *BotConfig) ResolveSymbols() error {
	if c == nil {
		return fmt.Errorf("nil bot config")
	}
	if c.SymbolsFile == "" {
		c.symbolsResolved = true
		return nil
	}
	path, err := ExpandHome(c.SymbolsFile)
	if err != nil {
		return fmt.Errorf("bot_config.symbols_file %q: %w", c.SymbolsFile, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read bot_config.symbols_file %q: %w (fail-close: 静的な symbols: へは縮退しない)", path, err)
	}
	var syms []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		s := strings.TrimSpace(line)
		if s == "" {
			continue
		}
		if seen[s] {
			return fmt.Errorf("bot_config.symbols_file %q lists %q twice (同一銘柄の二重配線を防ぐため拒否)", path, s)
		}
		seen[s] = true
		syms = append(syms, s)
	}
	if len(syms) == 0 {
		return fmt.Errorf("bot_config.symbols_file %q is empty (fail-close: ユニバース不明のまま起動しない)", path)
	}
	c.Symbols = syms
	c.symbolsResolved = true
	return nil
}

// ExpandHome resolves a leading `~` so committed configs stay machine-independent
// (実データは repo の外・~/.stockbot 配下に置く必要がある)。exported なのは runtime が
// 同じ解決で symbols_file の mtime を読むため(ログの「どの日のユニバースか」がズレる)。
func ExpandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand ~: %w", err)
	}
	return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/")), nil
}

// DropSymbolsOutsideWhitelist は **symbols_file(日次ユニバースの生成物)から読んだ**銘柄のうち
// allowed_symbols に無いものを外し、外した銘柄を返す(呼び手が WARN で出す)。
//
// 🚨 07:00 の選定ファイルに、その後 allowed_symbols から外した銘柄(上場廃止)が
// 残っていて、make start が寄り 7 分前に起動を拒否した。選定ファイルは正本(hard_limits)より
// 古いことがありうる生成物なので、外れた銘柄は**除外して起動する**(減る方向 = fail-safe・
// whitelist 外の銘柄が監視に入ることは無い)。
// 🛑 静的な symbols:(人間が書いた設定)には効かない — 打ち間違いを黙って消すと気づけないので、
// 従来どおり ValidateAgainstHardLimits が起動を拒否する。全部外れて空なら同じく拒否。
func (c *BotConfig) DropSymbolsOutsideWhitelist(hl *HardLimits) []string {
	if c == nil || hl == nil || c.SymbolsFile == "" || !c.symbolsResolved {
		return nil
	}
	var kept, dropped []string
	for _, s := range c.Symbols {
		if hl.AllowsSymbol(s) {
			kept = append(kept, s)
		} else {
			dropped = append(dropped, s)
		}
	}
	c.Symbols = kept
	return dropped
}

// ValidateAgainstHardLimits enforces the symbol whitelist (fail-close) and blocks
// live mode unless the broker is a real adapter.
func (c *BotConfig) ValidateAgainstHardLimits(hl *HardLimits) error {
	if c == nil {
		return fmt.Errorf("nil bot config")
	}
	if hl == nil {
		return fmt.Errorf("nil hard limits")
	}
	// 呼び忘れると日次ユニバースのつもりで static な symbols: で走る。起動が必ず通る
	// ここで閂を掛ける。
	if c.SymbolsFile != "" && !c.symbolsResolved {
		return fmt.Errorf("bot_config.symbols_file %q was never resolved — call ResolveSymbols() before validating (fail-close)", c.SymbolsFile)
	}
	if len(c.Symbols) == 0 {
		return fmt.Errorf("bot_config.symbols is empty")
	}
	seen := make(map[string]bool, len(c.Symbols))
	for _, s := range c.Symbols {
		if seen[s] {
			return fmt.Errorf("bot_config.symbols lists %q twice (同一銘柄の二重配線を防ぐため拒否)", s)
		}
		seen[s] = true
		if !hl.AllowsSymbol(s) {
			return fmt.Errorf("symbol %q not in allowed_symbols whitelist (fail-close)", s)
		}
	}
	// live_config は実執行のみ。paper / paper_live_feed(紙執行)を live と記録させない。
	if c.Mode == ModeLive && (c.Broker.Kind == BrokerPaper || c.Broker.Kind == BrokerPaperLiveFeed) {
		return fmt.Errorf("live_config mode requires a real broker, not %q", c.Broker.Kind)
	}
	// 🛑 監視銘柄の予算は**紙トラックの通信・解像度の道具**で、実弾の建玉枠ではない。
	// live の枠は account_max_open_positions(現在値は bot_config.live.yaml)。研究モードの設定を実弾へ
	// 持ち込まない前例(全トリガー採用 / account_max_open_positions)に揃える。
	if c.Mode == ModeLive && c.Risk.AccountMaxOpenSymbols != 0 {
		return fmt.Errorf("live_config mode must not set risk.account_max_open_symbols (=%d): "+
			"銘柄予算は紙トラック(research / harvest)の通信・解像度の道具。live の建玉枠は account_max_open_positions",
			c.Risk.AccountMaxOpenSymbols)
	}
	if c.Mode == ModeLive && c.Risk.PerEntryMaxOpenSymbols != 0 {
		return fmt.Errorf("live_config mode must not set risk.per_entry_max_open_symbols (=%d): "+
			"入口ごとの枠は 12 アームを同時 audition する紙トラックの道具で、実弾のメニューは live_allowed_strategies が決める",
			c.Risk.PerEntryMaxOpenSymbols)
	}
	return nil
}

// ValidateBrokerCapabilities fails closed when bot_config asks for an 執行区分 the
// selected broker cannot execute. Without it the adapter would place 現物 while the
// frozen Position records margin (silent book/Position ExecKind divergence).
func (c *BotConfig) ValidateBrokerCapabilities() error {
	if c == nil {
		return fmt.Errorf("nil bot config")
	}
	for _, m := range []HoldingMode{HoldingIntraday, HoldingMultiday} {
		e := c.ExecKindFor(m)
		if e == "" {
			continue
		}
		if !c.Broker.Kind.SupportsExecKind(e) {
			return fmt.Errorf("broker %q does not support exec_kind %q (%s holding); 立花は cash(現物) または margin_system(制度信用6ヶ月)のみ。一日信用は API 非対応、一般信用は口座で拒否された — fix holding.%s.exec_kind",
				c.Broker.Kind, e, m, m)
		}
	}
	return nil
}

// ExecKindFor returns the configured exec kind for the given holding mode.
func (c *BotConfig) ExecKindFor(m HoldingMode) ExecKind {
	switch m {
	case HoldingMultiday:
		return c.Holding.Multiday.ExecKind
	default:
		return c.Holding.Intraday.ExecKind
	}
}
