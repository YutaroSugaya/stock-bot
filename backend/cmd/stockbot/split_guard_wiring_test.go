package main

import (
	"log/slog"
	"testing"
	"time"

	"stockbot/backend/internal/adapter/broker"
	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/usecase/command"
)

// 🛑 live は**必ず** broker の建玉照会で分割を確かめる(値段の証拠だけで台帳を書き換えない)。
// mode で決める — broker の型で決めると、ラッパの変更ひとつで live が値段だけで言い直す側へ
// 黙って落ちる。paper は紙の帳簿ごと言い直す。
func TestSplitGuardFor_AuthorityFollowsTheMode(t *testing.T) {
	paper := broker.NewPaper(time.Now, 0, 0)
	for _, c := range []struct {
		mode config.Mode
		want command.SplitAuthority
	}{
		{config.ModeLive, command.SplitAuthorityBroker},
		{config.ModePaper, command.SplitAuthorityPrice},
	} {
		d := &wiringDeps{
			broker: paper, posRepo: repository.NewInMemoryPositionRepo(), candles: repository.NewInMemoryCandleRepo(),
			botCfg: &config.BotConfig{Mode: c.mode}, logger: slog.Default(),
		}
		g := d.splitGuard()
		if g.Authority() != c.want {
			t.Errorf("mode %s: authority = %v, want %v", c.mode, g.Authority(), c.want)
		}
		if d.splitGuard() != g {
			t.Errorf("mode %s: トラックに 1 つのはずが作り直された", c.mode)
		}
	}
}
