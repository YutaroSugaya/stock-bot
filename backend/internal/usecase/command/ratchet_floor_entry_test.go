package command

import (
	"context"
	"testing"

	"stockbot/backend/internal/port"
)

// 🛑 床は Position の凍結値から実行時に計算されるので、
// **新規建玉が true で入らないと床は何も変わらない**(全建玉が旧規則のまま)。
// 逆に harvest の旧建玉は false のままでなければならない(そちらは DB の default)。
// 呼び手が 1 行忘れると静かに旧規則へ戻るので、entry saga の出力を直接見る。
func TestEntrySagaFreezesRatchetFloorOn(t *testing.T) {
	ctx := context.Background()
	f := newSaga(t, nil)

	posID, err := f.enter(ctx, 100)
	if err != nil {
		t.Fatalf("entry saga: %v", err)
	}
	p, err := f.posRepo.GetByID(ctx, posID)
	if err != nil || p == nil {
		t.Fatalf("建玉が読めない: %v", err)
	}
	if !p.RatchetFloorAtArm {
		t.Fatal("新規建玉に床が凍結されていない — トレールが建値割れで出続ける")
	}
}

// PositionInsertInput の zero 値は false(= 旧規則)。既存行を旧規則のまま残すための
// 既定であって、新規建玉の既定ではない、という非対称をここで明示しておく。
func TestPositionInsertInputDefaultsToLegacyRule(t *testing.T) {
	var in port.PositionInsertInput
	if in.RatchetFloorAtArm {
		t.Fatal("zero 値が true — DB の default false と食い違うと harvest の旧建玉に床が効く")
	}
}
