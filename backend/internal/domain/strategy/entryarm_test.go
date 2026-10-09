package strategy

import (
	"testing"

	"stockbot/backend/internal/config"
)

// メニューは「戦略が 12 個」ではなく **入口 7 / うち 5 に出口 2 通り = 12 アーム**
// (CLAUDE.md)。枠を配るときの単位は**アームではなく入口**でなければならない:
// 兄弟アームは入口が完全に同一で必ず同じ銘柄に乗るので、アーム単位で数えると
// **ペアの 2 本目だけが枠の境界で弾かれて ペア差が構造的に測れなくなる**
// (「ペア 0 件」になる壊れ方そのもの)。
func TestEntryArmOfFoldsTheTrailSibling(t *testing.T) {
	for _, tc := range []struct{ arm, want config.StrategyName }{
		{config.StrategyDonchianBreakoutV2Trail, config.StrategyDonchianBreakoutV2},
		{config.StrategyDonchianBreakoutV2, config.StrategyDonchianBreakoutV2},
		{config.StrategyBNFReversionTrail, config.StrategyBNFReversion},
		{config.StrategyBNFDay2ReversionTrail, config.StrategyBNFDay2Reversion},
		{config.StrategyBNFStabilizedReversionTrail, config.StrategyBNFStabilizedReversion},
		{config.StrategyBNFIntradayReversionTrail, config.StrategyBNFIntradayReversion},
		{config.StrategyHighVolumePremium, config.StrategyHighVolumePremium}, // ペア無し
	} {
		if got := EntryArmOf(tc.arm); got != tc.want {
			t.Errorf("EntryArmOf(%q) = %q, want %q", tc.arm, got, tc.want)
		}
	}
}

// 入口の建玉を数えるには「基のアーム」と「兄弟アーム」の両方の名前が要る。
// 片方だけを渡すと枠が兄弟のぶんだけ 2 倍に見える。
func TestSiblingArmsCoversBothLegs(t *testing.T) {
	got := SiblingArms(config.StrategyDonchianBreakoutV2Trail)
	want := []string{string(config.StrategyDonchianBreakoutV2), string(config.StrategyDonchianBreakoutV2Trail)}
	if len(got) != len(want) {
		t.Fatalf("SiblingArms = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SiblingArms = %v, want %v", got, want)
		}
	}
	// 基のアームから引いても同じ集合(順序込み)でなければ、枠が呼ぶ側で変わる。
	if base := SiblingArms(config.StrategyDonchianBreakoutV2); base[0] != got[0] || base[1] != got[1] {
		t.Fatalf("基 %v と兄弟 %v で集合が違う", base, got)
	}
}
