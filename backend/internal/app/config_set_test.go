package app

import (
	"testing"

	"stockbot/backend/internal/config"
)

// 建玉の一意性キーが 銘柄 →(銘柄, 戦略)になったので、1 銘柄には
// 戦略ごとの active config が同時に載る。**bundle が 1 config しか実行しないなら
// 2 本目のアームは動かない**(後から arm した方が上書きするか無視される)= 兄弟アームのペアが
// 「設計上のみ」のままになる。

func armCfg(sym string, name config.StrategyName, id string) *config.StrategyConfig {
	return &config.StrategyConfig{ConfigID: id, Symbol: sym, StrategyName: name, Mode: config.ModePaper}
}

func TestConfigSetStartsWithItsBaseConfig(t *testing.T) {
	base := armCfg("7203", config.StrategyNoTrade, "seed")
	cs := NewConfigSet(base)

	got := cs.Active()
	if len(got) != 1 || got[0].ConfigID != "seed" {
		t.Fatalf("arm が無いときは base 1 本: %+v", got)
	}
	if cs.IsArmed() {
		t.Fatal("no_trade だけの状態を armed と報告している")
	}
}

// 2 戦略を arm したら **両方**が実行対象になる。ここが 1 本しか返さないとペア比較は成立しない。
func TestConfigSetRunsEveryArmedStrategy(t *testing.T) {
	cs := NewConfigSet(armCfg("7203", config.StrategyNoTrade, "seed"))
	cs.Arm(armCfg("7203", config.StrategyBNFReversion, "a"))
	cs.Arm(armCfg("7203", config.StrategyBNFReversionTrail, "b"))

	got := cs.Active()
	if len(got) != 2 {
		t.Fatalf("armed 2 戦略なのに実行対象が %d 本: %+v", len(got), got)
	}
	// 順序は戦略名の昇順で決定的(同じ入力なら同じ順に発注される = 再現性)。
	if got[0].StrategyName != config.StrategyBNFReversion || got[1].StrategyName != config.StrategyBNFReversionTrail {
		t.Fatalf("評価順が戦略名の昇順でない: %s, %s", got[0].StrategyName, got[1].StrategyName)
	}
	if !cs.IsArmed() {
		t.Fatal("armed を報告していない")
	}
}

// arm があるときは base(no_trade)を混ぜない — 混ぜると毎ティック無駄に評価する。
func TestConfigSetDropsTheBaseOnceArmed(t *testing.T) {
	cs := NewConfigSet(armCfg("7203", config.StrategyNoTrade, "seed"))
	cs.Arm(armCfg("7203", config.StrategyAbsMomentumV2, "a"))

	for _, c := range cs.Active() {
		if c.ConfigID == "seed" {
			t.Fatal("arm 済みなのに base が実行対象に残っている")
		}
	}
}

// 同一戦略の再 arm は**置き換え**(2 本にならない)。
func TestConfigSetReArmReplacesTheSameStrategy(t *testing.T) {
	cs := NewConfigSet(armCfg("7203", config.StrategyNoTrade, "seed"))
	cs.Arm(armCfg("7203", config.StrategyAbsMomentumV2, "old"))
	cs.Arm(armCfg("7203", config.StrategyAbsMomentumV2, "new"))

	got := cs.Active()
	if len(got) != 1 || got[0].ConfigID != "new" {
		t.Fatalf("同一戦略の再 arm が置き換えになっていない: %+v", got)
	}
}

// disarm は **その戦略だけ**を外す。もう一方のアームは走り続ける。
func TestConfigSetDisarmIsPerStrategy(t *testing.T) {
	cs := NewConfigSet(armCfg("7203", config.StrategyNoTrade, "seed"))
	cs.Arm(armCfg("7203", config.StrategyBNFReversion, "a"))
	cs.Arm(armCfg("7203", config.StrategyBNFReversionTrail, "b"))

	cs.Disarm(config.StrategyBNFReversion)
	got := cs.Active()
	if len(got) != 1 || got[0].StrategyName != config.StrategyBNFReversionTrail {
		t.Fatalf("片方だけ外れていない: %+v", got)
	}

	// 全部外したら base に戻る(監視は続けるが発注しない状態)。
	cs.Disarm(config.StrategyBNFReversionTrail)
	got = cs.Active()
	if len(got) != 1 || got[0].ConfigID != "seed" {
		t.Fatalf("全 disarm 後に base へ戻っていない: %+v", got)
	}
	if cs.IsArmed() {
		t.Fatal("全 disarm 後に armed のまま")
	}
}

// ダッシュボード / reconcile 用の config_id 一覧。arm が複数あるなら複数返す。
func TestConfigSetReportsEveryConfigID(t *testing.T) {
	cs := NewConfigSet(armCfg("7203", config.StrategyNoTrade, "seed"))
	cs.Arm(armCfg("7203", config.StrategyBNFReversion, "a"))
	cs.Arm(armCfg("7203", config.StrategyBNFReversionTrail, "b"))

	ids := cs.ConfigIDs()
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("config_id 一覧 = %v, want [a b]", ids)
	}
}
