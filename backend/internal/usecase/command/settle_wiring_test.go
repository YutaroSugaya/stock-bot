package command

import (
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/port"
)

// 🚨 live の broker は生の *Tachibana ではなく **ラッパ**(`LiveQuoteShared`。
// 時価を共有フィードから引き、口座照会を TTL で併合する)。最初の実装は
// ラッパが `SettleFillsAsync` を転送しておらず、**型アサーションが黙って外れて live の
// 決済が丸ごと旧経路(受理を約定と読んで観測価格で台帳に書く)へ落ちていた**。
// 幽霊決済を止めたつもりが live では 1 行も効いていない、という壊れ方をする。
//
// 転送漏れは `port.LiveBroker` の一員にしたのでコンパイルエラーになるが、
// **usecase 側のアサーションが実配線の型で通ること**は別の話なのでここで固定する。
func TestLiveWiring_SatisfiesAsyncSettleBroker(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, clock.JST)
	pb := broker.NewPaper(clock.Fixed(now), 0, 0)

	// 前提: 紙は同期に約定するので、この経路に入ってはいけない。
	if async, ok := any(pb).(asyncSettleBroker); !ok {
		t.Fatal("paper must still be inspectable as a settle-fill reporter")
	} else if async.SettleFillsAsync() {
		t.Fatal("paper must declare synchronous settle fills (紙の測定を live の経路に入れない)")
	}

	// live track の実配線と同じ形に包む(cmd/stockbot/live_track.go の buildLiveBroker)。
	wrapped := broker.NewLiveQuoteShared(asyncLiveStub{pb}, pb, 5*time.Second, clock.Fixed(now))
	async, ok := any(wrapped).(asyncSettleBroker)
	if !ok {
		t.Fatalf("live wiring must satisfy asyncSettleBroker, got %T", wrapped)
	}
	if !async.SettleFillsAsync() {
		t.Fatal("the wrapper must FORWARD the inner broker's declaration, not answer for it")
	}
}

// asyncLiveStub は *Tachibana の立ち位置(決済の約定は非同期)。
type asyncLiveStub struct{ *broker.Paper }

func (asyncLiveStub) SettleFillsAsync() bool { return true }

var _ port.LiveBroker = asyncLiveStub{}
