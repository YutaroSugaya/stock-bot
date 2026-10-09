package config

import (
	"fmt"
	"os"
)

// Env holds the resolved environment-derived runtime settings (paths, DSNs).
// Kept tiny and explicit so wiring (cmd/) stays the only place that reads os.Getenv.
type Env struct {
	BotConfigPath      string
	HardLimitsPath     string
	StrategyConfigPath string
	EmergencyFlagPath  string
	HTTPAddr           string
	APIToken           string // STOCKBOT_API_TOKEN; required on mutating endpoints when set / when bound non-loopback
	DatabaseURL        string // optional; empty in paper mode
}

// LoadEnv resolves runtime settings from the environment. HTTPAddr defaults to
// LOOPBACK (127.0.0.1) — the control API (emergency stop/resume) must not be reachable
// from the network by default; exposing it is an explicit, tokened opt-in.
func LoadEnv() Env {
	return Env{
		BotConfigPath:      getenv("STOCKBOT_BOT_CONFIG", "configs/bot_config.yaml"),
		HardLimitsPath:     getenv("STOCKBOT_HARD_LIMITS", "configs/hard_limits.yaml"),
		StrategyConfigPath: getenv("STOCKBOT_STRATEGY_CONFIG", "configs/strategy_config.active.yaml"),
		EmergencyFlagPath:  getenv("STOCKBOT_EMERGENCY_FLAG", "runtime/emergency_stop.flag"),
		HTTPAddr:           getenv("STOCKBOT_HTTP_ADDR", "127.0.0.1:8090"),
		APIToken:           os.Getenv("STOCKBOT_API_TOKEN"),
		DatabaseURL:        os.Getenv("STOCKBOT_DATABASE_URL"),
	}
}

// RequireLiveGuards fails closed when live mode is requested without the explicit
// human-set env gate.
func RequireLiveGuards(mode Mode) error {
	if mode != ModeLive {
		return nil
	}
	if os.Getenv("STOCKBOT_LIVE_CONFIRMED") != "1" {
		return fmt.Errorf("live_config requires STOCKBOT_LIVE_CONFIRMED=1 (human gate); refusing to start")
	}
	return nil
}

// RequireDurableBackend fails closed when live mode is requested without a Postgres
// DSN: in-memory persistence loses the daily-loss ledger, frozen config, hold-day
// counters and the bot's own open positions on restart.
func RequireDurableBackend(mode Mode, broker BrokerKind, databaseURL string) error {
	if databaseURL != "" {
		return nil
	}
	if mode == ModeLive {
		return fmt.Errorf("live_config requires a durable Postgres backend: set STOCKBOT_DATABASE_URL (refusing to run live on in-memory persistence — a restart would zero the daily-loss ledger and un-book open positions)")
	}
	// 🚨 紙執行でも **実フィード**なら台帳は forward 記録そのもの。DSN を忘れたまま
	// 起動すると in-memory へ黙って落ち、プロセスを落とした時点でその日の建玉と決済が
	// 消える。サイクルの標本は再生成できないので fail-close。
	// 合成フィードの `paper`(test / backtest)は従来どおり in-memory を許す。
	if broker == BrokerPaperLiveFeed {
		return fmt.Errorf("paper_live_feed requires a durable Postgres backend: set STOCKBOT_DATABASE_URL " +
			"(実フィードの forward 記録を in-memory に置くと、プロセスを落とした時点でそのサイクルの建玉と決済が消える。標本は再生成できない)")
	}
	return nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
