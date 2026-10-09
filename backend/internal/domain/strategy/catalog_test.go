package strategy

import (
	"testing"

	"stockbot/backend/internal/config"
)

// 🛑 **メニューの増減は意識した commit でしか起きない**(速度バンプ)。
// 戦略を 1 本足す・外すときに触るのはカタログ(catalog.go)とこのテストだけ。
// engine の登録・screener・advisor の候補・backtest の名前解決は全部カタログから派生する。
func TestCatalogMenuIsExactlyThePreRegisteredArms(t *testing.T) {
	want := map[config.StrategyName]bool{
		config.StrategyBNFReversion:      true,
		config.StrategyBNFReversionTrail: true,
		// bnf ファミリーの新入口 2 つとその兄弟。
		config.StrategyBNFDay2Reversion:            true,
		config.StrategyBNFDay2ReversionTrail:       true,
		config.StrategyBNFStabilizedReversion:      true,
		config.StrategyBNFStabilizedReversionTrail: true,
		config.StrategyPostJumpDrift:               true,
		config.StrategyHighVolumePremium:           true,
		config.StrategyBNFIntradayReversion:        true,
		config.StrategyBNFIntradayReversionTrail:   true,
		// 「continue(灰色・CI 0跨ぎ)」テンプレ。
		config.StrategyHigh52wMomentum: true,
		// v2 が v1 を置き換える。並走は成立しない — v2 ⊂ v1 かつ
		// 1銘柄1 config + ナンピン禁止で、両アームは同じ母集団を割り合うだけになる。
		config.StrategyAbsMomentumV2:      true,
		config.StrategyATRBreakoutV2:      true,
		config.StrategyDonchianBreakoutV2: true,
		// トレンド系4戦略の兄弟アーム。**v3 ではない**。
		config.StrategyAbsMomentumV2Trail:      true,
		config.StrategyATRBreakoutV2Trail:      true,
		config.StrategyDonchianBreakoutV2Trail: true,
		config.StrategyHigh52wMomentumTrail:    true,
	}
	got := map[config.StrategyName]bool{}
	for _, n := range MenuNames() {
		if got[n] {
			t.Errorf("メニューに %q が重複している", n)
		}
		got[n] = true
	}
	for n := range want {
		if !got[n] {
			t.Errorf("メニューに %q が無い", n)
		}
	}
	for n := range got {
		if !want[n] {
			t.Errorf("メニューに想定外の %q がある(棄却済み・v1・ban 維持は Menu=false で backtest の再現用だけ)", n)
		}
	}
}

// カタログの名前は一意で、各行の実体は自分の名前を名乗る(取り違えると別の戦略へ dispatch する)。
func TestCatalogNamesAreUniqueAndSelfConsistent(t *testing.T) {
	seen := map[config.StrategyName]bool{}
	for _, s := range Catalog() {
		n := s.Strategy.Name()
		if seen[n] {
			t.Errorf("カタログに %q が 2 行ある", n)
		}
		seen[n] = true
		if got := ByName(n); got == nil || got.Name() != n {
			t.Errorf("ByName(%q) = %v", n, got)
		}
	}
	if ByName("no_such_strategy") != nil {
		t.Error("未知の名前は nil(backtest が loud に落ちる)")
	}
	if ByName(config.StrategyNoTrade) == nil {
		t.Error("no_trade も backtest で引けること")
	}
}

// 🚨 メニューに載る戦略は screener を持ち、Screen が**自分の名前**で候補を出すこと。
// screener が無いと Triggered にならず枠が配られない = 標本ゼロのまま静かに走る
// (high_52w_momentum がこれだった)。
func TestEveryMenuStrategyIsScreenable(t *testing.T) {
	scr := DefaultScreeners()
	if len(scr) != len(MenuNames()) {
		t.Fatalf("screener %d 本 / メニュー %d 本", len(scr), len(MenuNames()))
	}
	for i, n := range MenuNames() {
		if got := scr[i].Screen("7203", nil).Strategy; got != n {
			t.Errorf("メニューの %q の screener が %q を名乗る", n, got)
		}
	}
}

// engine に載せるのはメニューの実体(登録漏れは致命的: v2 の 3 本が
// menu と screener にだけ載って arm で unregistered として弾かれた)。
func TestMenuStrategiesAreTheMenu(t *testing.T) {
	ms := MenuStrategies()
	names := MenuNames()
	if len(ms) != len(names) {
		t.Fatalf("実体 %d 本 / 名前 %d 本", len(ms), len(names))
	}
	for i := range ms {
		if ms[i].Name() != names[i] {
			t.Errorf("%d 番目: 実体 %q / 名前 %q", i, ms[i].Name(), names[i])
		}
	}
}

// 🛑 live の selector が arm してよい戦略 = **出口を自前計算する戦略**のうち、live に出すと
// 決めたもの。広げるのはリスクを増やす変更なので人間の決定が要る(live allowlist とは別の栓)。
//
// GKM / PEAD / 52週高値 は config の exit を読むので、arm するには戦略ごとの
// テンプレートが要る(テンプレート無しで arm すると exit 欄 0 = TP/SL 無しの建玉になる)。
// `_trail` 兄弟は enterIfClearsTrail → enterIfClears → atrExitSignal で出口を自前計算する。
func TestCatalogLiveArmableIsExactlyTheDecidedSet(t *testing.T) {
	want := map[config.StrategyName]bool{
		config.StrategyBNFReversion:          true,
		config.StrategyBNFReversionTrail:     true,
		config.StrategyBNFDay2ReversionTrail: true,
		config.StrategyBNFIntradayReversion:  true,
		config.StrategyAbsMomentumV2:         true,
		config.StrategyATRBreakoutV2:         true,
		config.StrategyDonchianBreakoutV2:    true,
		// donchian の兄弟だけ live へ出す。
		config.StrategyDonchianBreakoutV2Trail: true,
	}
	for _, s := range Catalog() {
		n := s.Strategy.Name()
		scr := LiveArmScreeners(n)
		if LiveArmable(n) != want[n] {
			t.Errorf("LiveArmable(%s) = %v, want %v", n, LiveArmable(n), want[n])
		}
		if want[n] {
			if len(scr) != 1 || scr[0].Name() != n {
				t.Errorf("%s: live の screener = %v, want 自分自身 1 本", n, scr)
			}
			if !s.Menu {
				t.Errorf("%s: live で arm するのにメニュー外", n)
			}
			continue
		}
		if len(scr) != 0 {
			t.Errorf("%s は live で arm してはいけない(決定の外)", n)
		}
	}
	if LiveArmScreeners("no_such_strategy") != nil {
		t.Error("未知の名前は arm しない")
	}
}

// 事前登録: `both` は **4 入口とその兄弟**だけ。それ以外は buy_only。
// 兄弟は基のアームの値を引く(テーブルに 2 行書くと片方だけ直す事故が起きる)。
func TestEntryDirectionIsThePreRegisteredTable(t *testing.T) {
	both := map[config.StrategyName]bool{
		config.StrategyAbsMomentumV2:      true,
		config.StrategyATRBreakoutV2:      true,
		config.StrategyDonchianBreakoutV2: true,
		config.StrategyHigh52wMomentum:    true,
	}
	for _, n := range MenuNames() {
		want := config.DirectionBuyOnly
		if both[EntryArmOf(n)] {
			want = config.DirectionBoth
		}
		if got := EntryDirection(n); got != want {
			t.Errorf("%s の事前登録 direction = %q, want %q", n, got, want)
		}
	}
	if got := EntryDirection("no_such_strategy"); got != config.DirectionBuyOnly {
		t.Errorf("未知の名前の direction = %q, want buy_only(狭い側へ倒す)", got)
	}
}

// 入口の属性(向き・ルックバック窓)は**入口の行にだけ**書く。兄弟の行に書くと
// 基と兄弟で値が割れうる(ペアで動く変数を 1 つに保つ)。
func TestTrailRowsCarryNoEntryAttributes(t *testing.T) {
	for _, s := range Catalog() {
		n := s.Strategy.Name()
		if EntryArmOf(n) == n {
			continue
		}
		if s.Direction != "" || s.LookbackDays != 0 {
			t.Errorf("%s(兄弟)の行に入口の属性がある: direction=%q lookback=%d", n, s.Direction, s.LookbackDays)
		}
	}
}
