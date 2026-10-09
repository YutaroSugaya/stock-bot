package config

import (
	"strings"
	"testing"
)

func researchCfg() *BotConfig {
	c := &BotConfig{Mode: ModePaper}
	c.Broker.Kind = BrokerPaperLiveFeed
	return c
}

func liveCfg() *BotConfig {
	c := &BotConfig{Mode: ModeLive}
	c.Broker.Kind = BrokerTachibana
	return c
}

func okLiveEnv() LiveTrackEnv {
	return LiveTrackEnv{
		BotConfigPath:      "configs/bot_config.live.yaml",
		StrategyConfigPath: "configs/strategy_config.live.yaml",
		EmergencyFlagPath:  "runtime/emergency_stop.live.flag",
		DatabaseURL:        "postgres://x/stockbot_live",
	}
}

const researchDSN = "postgres://x/stockbot"

func researchEnv() Env {
	return Env{DatabaseURL: researchDSN, EmergencyFlagPath: "runtime/emergency_stop.flag"}
}

// live track 無効(env 未設定)が既定。**ここが現行と完全に同一である**ことが
// hybrid の最重要の互換条件 — 研究モードの挙動が 1bit も変わらないこと。
func TestLiveTrackDisabledByDefault(t *testing.T) {
	if (LiveTrackEnv{}).Enabled() {
		t.Fatal("env 未設定で live track が有効になっている")
	}
	if err := ValidateHybrid(researchCfg(), nil, researchEnv(), LiveTrackEnv{}); err != nil {
		t.Fatalf("live track 無効なら現行どおり通るべき: %v", err)
	}
}

// 🚨 `STOCKBOT_LIVE_DISABLED=1` は **BOT_CONFIG が残っていても** live を止める。
// .env の案内は「止めたいならこの 1 行を足す」なので、
// 足しただけ(BOT_CONFIG を消していない)で実弾が動くのは案内どおりに操作した
// 人間を裏切る。部分構成の検査(Validate)も通ること。
func TestLiveDisabledWinsOverFullConfig(t *testing.T) {
	env := okLiveEnv()
	env.Disabled = true
	if env.Enabled() {
		t.Fatal("STOCKBOT_LIVE_DISABLED=1 なのに live track が有効(BOT_CONFIG が残っているだけで実弾が動く)")
	}
	if err := env.Validate(); err != nil {
		t.Fatalf("明示の opt-out は部分構成ではない: %v", err)
	}
}

func TestValidateHybrid_Accepts(t *testing.T) {
	if err := ValidateHybrid(researchCfg(), liveCfg(), researchEnv(), okLiveEnv()); err != nil {
		t.Fatalf("正しい2トラック構成が拒否された: %v", err)
	}
}

func TestValidateHybrid_Rejects(t *testing.T) {
	bothLive := func() *BotConfig { c := liveCfg(); return c }

	cases := []struct {
		name     string
		research *BotConfig
		live     *BotConfig
		env      func(LiveTrackEnv) LiveTrackEnv
		want     string
	}{
		{
			// research 側を live に取り違えた構成を通さない。
			name: "両方 live_config", research: bothLive(), live: liveCfg(),
			env: func(e LiveTrackEnv) LiveTrackEnv { return e }, want: "research",
		},
		{
			name:     "live 側が live_config でない",
			research: researchCfg(), live: researchCfg(),
			env: func(e LiveTrackEnv) LiveTrackEnv { return e }, want: "live_config",
		},
		{
			// 紙執行を live として記録しない(既存 bot_config 検証と同じ極性)。
			name:     "live 側の broker が paper",
			research: researchCfg(),
			live: func() *BotConfig {
				c := liveCfg()
				c.Broker.Kind = BrokerPaperLiveFeed
				return c
			}(),
			env: func(e LiveTrackEnv) LiveTrackEnv { return e }, want: "tachibana",
		},
		{
			// 記録が消える live は存在してはならない(in-memory フォールバック不可)。
			name: "live DSN 未設定", research: researchCfg(), live: liveCfg(),
			env:  func(e LiveTrackEnv) LiveTrackEnv { e.DatabaseURL = ""; return e },
			want: "STOCKBOT_LIVE_DATABASE_URL",
		},
		{
			// 物理分離の強制。paper を live 成績として読む事故面を型で塞ぐ。
			name: "live DSN が research と同値", research: researchCfg(), live: liveCfg(),
			env:  func(e LiveTrackEnv) LiveTrackEnv { e.DatabaseURL = researchDSN; return e },
			want: "同一",
		},
		{
			// live は advisor 無し = 人間 commit 固定。STOCKBOT_STRATEGY_CONFIG は
			// 単一グローバルなので、live が自分の config を別パスで受け取れないと
			// research の active config を実弾で走らせることになる。
			name: "live の strategy config パス未設定", research: researchCfg(), live: liveCfg(),
			env:  func(e LiveTrackEnv) LiveTrackEnv { e.StrategyConfigPath = ""; return e },
			want: "STOCKBOT_LIVE_STRATEGY_CONFIG",
		},
		{
			// 片側 trip が他側を巻き込まないための物理分離。
			name: "emergency フラグが research と同じパス", research: researchCfg(), live: liveCfg(),
			env: func(e LiveTrackEnv) LiveTrackEnv {
				e.EmergencyFlagPath = "runtime/emergency_stop.flag"
				return e
			},
			want: "emergency",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateHybrid(c.research, c.live, researchEnv(), c.env(okLiveEnv()))
			if err == nil {
				t.Fatalf("拒否されるべき構成が通った")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err=%q に %q が含まれていない(何を直せばよいか分からない)", err, c.want)
			}
		})
	}
}

// Stage 1(配線だけ本番形・両トラックとも紙で1週間並走)のための逃げ道。
// これが無いと 受入条件が Step 1 の検証と両立せず、起動すらできない。
func TestValidateHybrid_DryRunAllowsPaperBrokerOnLiveTrack(t *testing.T) {
	live := liveCfg()
	live.Broker.Kind = BrokerPaper
	env := okLiveEnv()
	env.DryRun = true
	if err := ValidateHybrid(researchCfg(), live, researchEnv(), env); err != nil {
		t.Fatalf("dry-run では live track に紙 broker を許すべき: %v", err)
	}
}

// 🛑 dry-run は**本番口座を向いた瞬間に無効**。「配線確認のつもりが実弾だった」を
// 型ではなく env の組み合わせで塞ぐ唯一の場所。
func TestValidateHybrid_DryRunRefusedAgainstProduction(t *testing.T) {
	t.Setenv("STOCKBOT_TACHIBANA_ENV", "production")
	live := liveCfg()
	live.Broker.Kind = BrokerPaper
	env := okLiveEnv()
	env.DryRun = true
	err := ValidateHybrid(researchCfg(), live, researchEnv(), env)
	if err == nil {
		t.Fatal("production 環境で dry-run を許してはいけない")
	}
	if !strings.Contains(err.Error(), "production") {
		t.Errorf("err=%q が production を名指ししていない", err)
	}
}

// dry-run でも**実 broker を指定したら通常の live 扱い**(紙に落とさない)。
// 「dry-run を立てっぱなしにしたら実弾が紙になっていた」の逆事故を塞ぐ。
func TestValidateHybrid_DryRunDoesNotDowngradeRealBroker(t *testing.T) {
	env := okLiveEnv()
	env.DryRun = true
	if err := ValidateHybrid(researchCfg(), liveCfg(), researchEnv(), env); err != nil {
		t.Fatalf("dry-run + tachibana は通常の live 構成として通すべき: %v", err)
	}
}
