package command

import "testing"

// 🚨 **表を手で列挙すると新しいアームが黙って漏れる**。
// ここは列挙をやめ、**メニュー全戦略を回す**。戦略を足したら自動で対象に入る。
// 向きと MaxHold が兄弟と一致することは戦略カタログの catalog_test / trail_arms_test が縛る。

func menuTemplate() *ArmTemplate { return newTmpl() }

// メニューの全戦略でテンプレートが作れて、hard limits も promote も通ること。
func TestEveryMenuStrategyBuildsAndPromotes(t *testing.T) {
	for _, name := range AdvisorCandidateStrategies {
		cfg, err := menuTemplate().Build("7203", name, 2000, tmplNow)
		if err != nil {
			t.Errorf("%s: テンプレートが作れない: %v", name, err)
			continue
		}
		if err := cfg.ValidateAgainstHardLimits(tmplHardLimits()); err != nil {
			t.Errorf("%s: hard limits を通らない: %v", name, err)
			continue
		}
		p := &Promoter{
			HardLimits: tmplHardLimits(), ExpectedSymbol: "7203",
			Menu: AdvisorCandidateStrategies, ExpectedStrategy: name,
		}
		if _, err := p.PromoteConfig(cfg); err != nil {
			t.Errorf("%s: promote を通らない: %v", name, err)
		}
	}
}
