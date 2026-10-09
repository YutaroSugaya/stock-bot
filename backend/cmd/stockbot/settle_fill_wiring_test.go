package main

import (
	"context"
	"strings"
	"testing"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/port"
)

// 🚨 本番で踏んだ穴の回帰。**live track の broker は生の *Tachibana ではなく
// ラッパ**(`LiveQuoteShared`)で、これが `SettleFillsAsync` を転送していなかったため
// usecase の型アサーションが黙って外れ、決済は丸ごと旧経路(受理を約定と読んで
// 観測価格で台帳に書く = 幽霊決済)へ落ちていた。「止めたつもりで止まっていない」。
//
// 固定するのは **`liveTrackBroker()` が実際に返す型**。usecase 側で手組みしたラッパを
// 見るテストでは、配線がもう 1 枚包んだ日に同じ穴がまた開く(踏んだのは配線側だった)。
func TestLiveTrackBroker_ReportsSettleFillsAsync(t *testing.T) {
	cfg, err := config.LoadBotConfig(writeYAML(t, t.TempDir(), "live.yaml", liveYAMLTachibana))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	inner := asyncTachibanaStub{broker.NewPaper(clock.System(), 0, 0)}
	b, err := liveTrackBroker(cfg, liveTrackDeps{
		hardLimits: &config.HardLimits{AllowedSymbols: []string{"7203"}}, clock: clock.System(),
		tb: inner, feed: broker.NewPaper(clock.System(), 0, 0),
	}, config.LiveTrackEnv{})
	if err != nil {
		t.Fatalf("live broker: %v", err)
	}
	if !b.SettleFillsAsync() {
		t.Fatalf("%T が内側の宣言を落としている — usecase が決済の約定を確認せず台帳に書く経路へ落ちる", b)
	}
}

// dry-run の紙 broker は逆に「同期に約定する」と名乗ること(紙の測定を live 用の
// 約定解決の経路に入れない)。
func TestLiveTrackBroker_DryRunReportsSynchronousFills(t *testing.T) {
	cfg, err := config.LoadBotConfig(writeYAML(t, t.TempDir(), "live.yaml", strings.Replace(liveYAMLTachibana,
		"broker: { kind: tachibana }", "broker: { kind: paper }", 1)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	b, err := liveTrackBroker(cfg, liveTrackDeps{
		hardLimits: &config.HardLimits{AllowedSymbols: []string{"7203"}}, clock: clock.System(),
	}, config.LiveTrackEnv{DryRun: true})
	if err != nil {
		t.Fatalf("dry-run broker: %v", err)
	}
	if b.SettleFillsAsync() {
		t.Fatal("紙 broker が非同期約定を名乗ると、observedPrice の従来経路が使われなくなる")
	}
}

// asyncTachibanaStub は *Tachibana の立ち位置(決済の約定は非同期)。
type asyncTachibanaStub struct{ *broker.Paper }

func (asyncTachibanaStub) SettleFillsAsync() bool { return true }

func (s asyncTachibanaStub) ClosePosition(context.Context, port.CloseRequest) (*port.CloseResult, error) {
	return &port.CloseResult{OrderID: "ord-1", Accepted: true}, nil // 受理のみ(価格は非同期)
}

var _ port.LiveBroker = asyncTachibanaStub{}
