package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LiveTrackEnv は hybrid(実弾 × paper 並走)の live トラックの起動設定。
//
// **既定(env 未設定)は現行と完全に同一**の単トラック。これが hybrid の最重要の
// 互換条件で、コードが main に入っても研究モードの挙動・設定が 1bit も変わらない。
type LiveTrackEnv struct {
	// BotConfigPath が空 = live track 無効。ここが唯一の opt-in スイッチ。
	BotConfigPath string
	// StrategyConfigPath は live 専用の active config。STOCKBOT_STRATEGY_CONFIG は
	// 単一グローバルなので、live(advisor 無し = 人間 commit 固定)は自分の config を
	// 別パスで受け取らないと research の active config を実弾で走らせることになる。
	//
	// 🛑 **カンマ区切りで複数書ける。並び順がそのまま優先順位**(先頭が最優先)。
	// 読み出しは必ず StrategyConfigPaths() を通すこと。
	StrategyConfigPath string
	// EmergencyFlagPath は live 専用の emergency フラグ。片側の trip が他側を止めない
	// (紙は資本リスクゼロでデータ収集を続ける価値がある / 逆も同様)。
	EmergencyFlagPath string
	// SymbolBlocksPath は live の銘柄ごとの新規停止のファイル(`STOCKBOT_LIVE_SYMBOL_BLOCKS`)。
	// 読み出しは SymbolBlocksFile() を通す(空なら emergency フラグと同じディレクトリ)。
	SymbolBlocksPath string
	// DatabaseURL は live 専用 DB。**in-memory フォールバックは持たない** —
	// 記録が消える live は存在してはならない。
	DatabaseURL string
	// Disabled は **明示的に live を止める** opt-out(`STOCKBOT_LIVE_DISABLED=1`)。
	// これが無いと「BotConfigPath だけ消す」= 部分構成になり、fail-close で紙トラックごと
	// 起動できなくなる。
	Disabled bool
	// DryRun は Stage 1(配線だけ本番形・両トラックとも紙で1週間並走)用。
	// live track に紙 broker を挿すことだけを許し、本番口座を向いていたら拒否する。
	DryRun bool
}

// StrategyConfigPaths は live のテンプレートを**優先順位つき**で返す(先頭が最優先)。
//
// `STOCKBOT_LIVE_STRATEGY_CONFIG` はカンマ区切りで複数書ける。1 本だけなら従来と
// まったく同じ挙動(後方互換)。
//
// 🛑 **並び順を優先順位にした**のは、優先度を別の設定項目へ切り出すと「ファイルは 2 本
// あるが優先度表には 1 本しか無い」のような**2 箇所が食い違う形**を作るから。
// 空白と空要素は落とす(人間は `a.yaml, b.yaml` や末尾カンマを書く)。
func (e LiveTrackEnv) StrategyConfigPaths() []string {
	var out []string
	for _, p := range strings.Split(e.StrategyConfigPath, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// SymbolBlocksFile は銘柄ごとの新規停止のファイルのパス。env が空なら live の emergency フラグと
// 同じディレクトリの live_symbol_blocks.json(停止の状態はフラグと同じく再起動をまたいで残る)。
func (e LiveTrackEnv) SymbolBlocksFile() string {
	if e.SymbolBlocksPath != "" {
		return e.SymbolBlocksPath
	}
	return filepath.Join(filepath.Dir(e.EmergencyFlagPath), "live_symbol_blocks.json")
}

// LoadLiveTrackEnv resolves the live-track settings from the environment.
func LoadLiveTrackEnv() LiveTrackEnv {
	return LiveTrackEnv{
		BotConfigPath:      os.Getenv("STOCKBOT_LIVE_BOT_CONFIG"),
		StrategyConfigPath: os.Getenv("STOCKBOT_LIVE_STRATEGY_CONFIG"),
		EmergencyFlagPath:  os.Getenv("STOCKBOT_LIVE_EMERGENCY_FLAG"),
		SymbolBlocksPath:   os.Getenv("STOCKBOT_LIVE_SYMBOL_BLOCKS"),
		DatabaseURL:        os.Getenv("STOCKBOT_LIVE_DATABASE_URL"),
		DryRun:             os.Getenv("STOCKBOT_LIVE_TRACK_DRYRUN") == "1",
		Disabled:           os.Getenv("STOCKBOT_LIVE_DISABLED") == "1",
	}
}

// Enabled reports whether the live track should be started at all.
//
// 🚨 `STOCKBOT_LIVE_DISABLED=1` は **BOT_CONFIG が残っていても勝つ**。
// 以前は BotConfigPath だけを見ていたので、.env の案内どおり「この 1 行を足す」だけでは
// 止まらず、止めたつもりの実弾が普通に発注されていた。
func (e LiveTrackEnv) Enabled() bool { return e.BotConfigPath != "" && !e.Disabled }

// Validate は **部分構成**を落とす(opt-in の変数だけ書き忘れると黙って無効化されるため)。
//
// 🚨 opt-in は `STOCKBOT_LIVE_BOT_CONFIG` の 1 変数だけなので、他の LIVE_* を書いて
// これだけ忘れると live トラックは**警告も出さず丸ごと無効化**される。実弾建玉が
// OPEN のまま誰にも監視されず、broker 側の守りの期日も延ばされない。
// 「半分だけ設定されている」は設定ミス以外に解釈しようがない。
func (e LiveTrackEnv) Validate() error {
	if e.Enabled() {
		return nil
	}
	// 🛑 **明示の opt-out**。`.env` は「LIVE_BOT_CONFIG の 1 行をコメントアウトすれば
	// live だけ無効になる」と案内しており、そこで部分構成として落とすと**紙 2 トラック
	// ごと起動不能**になる。「止めたい」と「書き忘れた」を
	// 区別する口をここに置く — 意図的に止めるならこの 1 行を足す。
	if e.Disabled {
		return nil
	}
	var set []string
	if e.DatabaseURL != "" {
		set = append(set, "STOCKBOT_LIVE_DATABASE_URL")
	}
	if e.EmergencyFlagPath != "" {
		set = append(set, "STOCKBOT_LIVE_EMERGENCY_FLAG")
	}
	if e.StrategyConfigPath != "" {
		set = append(set, "STOCKBOT_LIVE_STRATEGY_CONFIG")
	}
	if len(set) == 0 {
		return nil // live を使わない構成。正常。
	}
	return fmt.Errorf("live の部分構成: %v は設定されているのに STOCKBOT_LIVE_BOT_CONFIG が空 — "+
		"live は opt-in が bot_config だけなので、このまま起動すると live トラックは警告も出さずに"+
		"無効化され、実弾建玉が誰にも監視されないまま残る(守りの期日も延びない)。"+
		"【意図的に live を止めるなら】STOCKBOT_LIVE_DISABLED=1 を .env に足す(実弾建玉が"+
		"OPEN のまま止めることの意味を理解したうえで)。【書き忘れなら】"+
		"STOCKBOT_LIVE_BOT_CONFIG を戻すか、上記の LIVE_* を全部コメントアウトする", set)
}

// ValidateHybrid fails closed on any 2-track configuration that could record paper
// as live, run live off the research config, or lose the live ledger.
//
// live が nil / env 無効なら現行の単トラックなので何も検査しない。
func ValidateHybrid(research, live *BotConfig, researchEnv Env, e LiveTrackEnv) error {
	if !e.Enabled() {
		return nil
	}
	if live == nil {
		return fmt.Errorf("STOCKBOT_LIVE_BOT_CONFIG=%q が設定されているのに live 側 bot_config が読めていない", e.BotConfigPath)
	}
	// research 側を live に取り違えた構成を通さない。**両方が実弾**になる事故は
	// 「live 側が正しい」だけでは防げないので、research 側も明示的に見る。
	if research != nil && research.Mode == ModeLive {
		return fmt.Errorf("research 側 bot_config の mode が live_config になっている(hybrid では research は必ず紙。取り違えの可能性が高いので起動しない)")
	}
	if live.Mode != ModeLive {
		return fmt.Errorf("live 側 bot_config の mode は live_config でなければならない: %q", live.Mode)
	}
	// 紙執行を live として記録しない。dry-run(Stage 1)のときだけ、**本番口座を
	// 向いていない**ことを条件に紙 broker を許す。
	if live.Broker.Kind != BrokerTachibana {
		if !e.DryRun {
			return fmt.Errorf("live 側 bot_config の broker.kind は tachibana でなければならない: %q (Stage 1 の配線確認なら STOCKBOT_LIVE_TRACK_DRYRUN=1)", live.Broker.Kind)
		}
		if os.Getenv("STOCKBOT_TACHIBANA_ENV") == "production" {
			return fmt.Errorf("STOCKBOT_LIVE_TRACK_DRYRUN=1 は STOCKBOT_TACHIBANA_ENV=production と併用できない(配線確認のつもりで本番口座に向いている)")
		}
	}
	if e.StrategyConfigPath == "" {
		return fmt.Errorf("live track には STOCKBOT_LIVE_STRATEGY_CONFIG が必要(STOCKBOT_STRATEGY_CONFIG は research と共有のグローバルなので流用しない)")
	}
	if e.DatabaseURL == "" {
		return fmt.Errorf("live track には STOCKBOT_LIVE_DATABASE_URL が必要(live に in-memory フォールバックは無い — 記録が消える live は存在してはならない)")
	}
	if SameDatabase(e.DatabaseURL, researchEnv.DatabaseURL) {
		return fmt.Errorf("live と research の DATABASE_URL が同一(物理分離の強制 — 単一 DB + track 列は paper を live 成績として読む事故面が広い)")
	}
	if e.EmergencyFlagPath == "" {
		return fmt.Errorf("live track には STOCKBOT_LIVE_EMERGENCY_FLAG が必要(emergency は track 別)")
	}
	return ValidateHybridEmergencyPaths(researchEnv.EmergencyFlagPath, e.EmergencyFlagPath)
}

// ValidateHybridEmergencyPaths fails closed when both tracks would trip the same
// flag file. 分離してあることが「片側 trip → 他側継続」の全根拠なので、パスが同じ
// なら hybrid の分離は成立していない。
func ValidateHybridEmergencyPaths(researchPath, livePath string) error {
	if researchPath == livePath {
		return fmt.Errorf("live と research の emergency フラグが同一パス %q(片側の trip が他側を巻き込む)", livePath)
	}
	return nil
}
