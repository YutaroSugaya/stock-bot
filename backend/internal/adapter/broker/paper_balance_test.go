package broker

import (
	"context"
	"testing"
)

// 研究モード: paper の口座残高は設定可能にする。全トリガー採用で
// 同時多数ポジションを収集するとき、既定100万円のままだと collateral ゲートが
// 2本目以降の entry を静かに reject してサンプルを censoring するため。
// 既定は従来どおり 100万円(後方互換)。
func TestPaperBalanceConfigurable(t *testing.T) {
	p := NewPaper(nil, 0, 0)
	m, err := p.GetAccountMargin(context.Background())
	if err != nil || m.AvailableJPY != 1_000_000 {
		t.Fatalf("既定残高が変わっている: %v %+v", err, m)
	}
	p.BalanceJPY = 100_000_000
	m, err = p.GetAccountMargin(context.Background())
	if err != nil || m.AvailableJPY != 100_000_000 || m.Equity != 100_000_000 {
		t.Fatalf("設定残高が反映されない: %v %+v", err, m)
	}
}
