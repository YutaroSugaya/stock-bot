//go:build integration

// Integration tests for the Postgres adapter. NEVER run with a raw
// `go test -tags integration` (the deny hook blocks it). Use
// `make test-integration` with INTEGRATION_TEST_DB_URL ending in _test. These
// are compile-checked by .githooks/pre-push via `make vet-integration`.
package pg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/port"
)

// requireDB returns a pool for the *_test DB, skipping when unset and failing
// closed via the double-wall DSN guard.
func requireDB(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	testDSN := os.Getenv("INTEGRATION_TEST_DB_URL")
	if testDSN == "" {
		t.Skip("INTEGRATION_TEST_DB_URL not set")
	}
	if err := repository.SafeIntegrationTestDSN(testDSN, os.Getenv("STOCKBOT_DATABASE_URL"), os.Getenv("STOCKBOT_LIVE_DATABASE_URL")); err != nil {
		t.Fatalf("unsafe integration DSN: %v", err)
	}
	pool, err := Open(context.Background(), testDSN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	truncateAll(t, pool)
	return pool, func() { pool.Close() }
}

func truncateAll(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`TRUNCATE trades, positions, candles, signal_rejections, strategy_configs RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func TestPg_PositionLifecycle(t *testing.T) {
	pool, done := requireDB(t)
	defer done()
	ctx := context.Background()

	cfgRepo := NewConfigRepo(pool)
	if err := cfgRepo.EnsureExists(ctx, port.StrategyConfigRecord{ConfigID: "cfg1", Symbol: "7203", Mode: "paper_config", StrategyName: "no_trade", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	posRepo := NewPositionRepo(pool)
	tradeRepo := NewTradeRepo(pool)
	closer := NewCloser(pool)

	id, err := posRepo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "bp1", Symbol: "7203", Side: order.SideBuy, Quantity: 100, EntryPrice: 2500,
		StrategyConfigID: "cfg1", HoldingMode: order.HoldingIntraday, ExecKind: order.ExecMarginOneday, OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	open, _ := posRepo.ListOpenOrClosing(ctx, "7203")
	if len(open) != 1 {
		t.Fatalf("want 1 open, got %d", len(open))
	}
	if ok, _ := posRepo.ClaimForClose(ctx, id, time.Now()); !ok {
		t.Fatal("first claim should succeed")
	}
	if ok, _ := posRepo.ClaimForClose(ctx, id, time.Now()); ok {
		t.Fatal("second claim should be benign skip")
	}
	ok, err := closer.CloseAndRecord(ctx, id, time.Now(), port.TradeRecord{
		PositionID: id, Symbol: "7203", Side: order.SideBuy, Quantity: 100, EntryPrice: 2500, ClosePrice: 2520,
		ProfitLossJPY: 2000, CloseReason: "take_profit", ClosedAt: time.Now(),
	})
	if err != nil || !ok {
		t.Fatalf("close: ok=%v err=%v", ok, err)
	}
	n, _ := tradeRepo.CountTradesSinceBySymbol(ctx, "7203", time.Now().Add(-time.Hour))
	if n != 1 {
		t.Fatalf("want 1 trade, got %d", n)
	}
	list, err := tradeRepo.ListClosedSince(ctx, time.Time{})
	if err != nil {
		t.Fatalf("ListClosedSince: %v", err)
	}
	if len(list) != 1 || list[0].Symbol != "7203" || list[0].Side != order.SideBuy || list[0].ProfitLossJPY != 2000 {
		t.Fatalf("forward 台帳の読み出しが往復しない: %+v", list)
	}
	names, err := posRepo.StrategyByPositionID(ctx, []int64{id})
	if err != nil || names[id] != "no_trade" {
		t.Fatalf("戦略別分計の join が往復しない: err=%v names=%+v (want no_trade)", err, names)
	}
}

// 戦略名は建玉に凍結した strategy_name が正(migration 0015)。config_id の join は
// strategy_name が空の行(external・0015 前の旧建玉)だけの fallback。
func TestPg_StrategyByPositionIDPrefersFrozenStrategyName(t *testing.T) {
	pool, done := requireDB(t)
	defer done()
	ctx := context.Background()

	cfgRepo := NewConfigRepo(pool)
	if err := cfgRepo.EnsureExists(ctx, port.StrategyConfigRecord{ConfigID: "cfg_bnf", Symbol: "*", Mode: "live_config", StrategyName: "bnf_reversion", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	posRepo := NewPositionRepo(pool)
	insert := func(bp, symbol, strategy string) int64 {
		t.Helper()
		id, err := posRepo.Insert(ctx, port.PositionInsertInput{
			BrokerPositionID: bp, Symbol: symbol, Side: order.SideBuy, Quantity: 100, EntryPrice: 2500,
			StrategyConfigID: "cfg_bnf", StrategyName: strategy,
			HoldingMode: order.HoldingMultiday, ExecKind: order.ExecMarginSystem, OpenedAt: time.Now(),
		})
		if err != nil {
			t.Fatalf("insert %s: %v", symbol, err)
		}
		return id
	}
	frozen := insert("bp1", "4385", "bnf_reversion_trail")
	legacy := insert("bp2", "5726", "")

	names, err := posRepo.StrategyByPositionID(ctx, []int64{frozen, legacy})
	if err != nil {
		t.Fatal(err)
	}
	if names[frozen] != "bnf_reversion_trail" {
		t.Errorf("凍結した strategy_name より config の名前が勝っている: got %q", names[frozen])
	}
	if names[legacy] != "bnf_reversion" {
		t.Errorf("strategy_name が空の行は config から引く: got %q", names[legacy])
	}
}

// ExtendMaxHold は OPEN-only CAS(WHERE status='OPEN')。非 OPEN や未知 id は
// (nil, nil) → handler が 404 に写像する(port.md / extend_maxhold.go の契約)。
func TestPg_ExtendMaxHoldOpenOnlyCAS(t *testing.T) {
	pool, done := requireDB(t)
	defer done()
	ctx := context.Background()

	cfgRepo := NewConfigRepo(pool)
	if err := cfgRepo.EnsureExists(ctx, port.StrategyConfigRecord{ConfigID: "cfg1", Symbol: "7203", Mode: "paper_config", StrategyName: "no_trade", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	posRepo := NewPositionRepo(pool)
	id, err := posRepo.Insert(ctx, port.PositionInsertInput{
		BrokerPositionID: "bp1", Symbol: "7203", Side: order.SideBuy, Quantity: 100, EntryPrice: 2500,
		StrategyConfigID: "cfg1", HoldingMode: order.HoldingMultiday, ExecKind: order.ExecCash,
		MaxHoldMinutes: 60, OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	res, err := posRepo.ExtendMaxHold(ctx, id, 30)
	if err != nil || res == nil || res.NewMaxMinutes != 90 {
		t.Fatalf("OPEN extend: got (%+v, %v), want NewMaxMinutes=90", res, err)
	}
	if ok, _ := posRepo.ClaimForClose(ctx, id, time.Now()); !ok {
		t.Fatal("claim should succeed")
	}
	// CLOSING(非 OPEN)への延長は (nil, nil)。
	res, err = posRepo.ExtendMaxHold(ctx, id, 30)
	if err != nil || res != nil {
		t.Fatalf("CLOSING extend: got (%+v, %v), want (nil, nil)", res, err)
	}
	// 未知 id も (nil, nil)。
	res, err = posRepo.ExtendMaxHold(ctx, 99999, 30)
	if err != nil || res != nil {
		t.Fatalf("unknown id: got (%+v, %v), want (nil, nil)", res, err)
	}
}

// 実事故の再現と恒久修正: strategy_configs_active_uidx(active は
// symbol×mode で1行)の下で、素の EnsureExists は default no_trade(active)と衝突し
// arm が silent 全滅した。ActivateExclusive は旧 active を expire して切り替える。
func TestPg_ActivateExclusiveSwitchesActiveConfig(t *testing.T) {
	pool, done := requireDB(t)
	defer done()
	ctx := context.Background()
	repo := NewConfigRepo(pool)

	def := port.StrategyConfigRecord{ConfigID: "no_trade_default_7203", Symbol: "7203",
		Mode: "paper_config", StrategyName: "no_trade", Status: "active", RawYAML: "y1"}
	llm := port.StrategyConfigRecord{ConfigID: "20260724-123000-7203", Symbol: "7203",
		Mode: "paper_config", StrategyName: "abs_momentum", Status: "active", RawYAML: "y2"}

	if err := repo.ActivateExclusive(ctx, def); err != nil {
		t.Fatalf("startup default: %v", err)
	}
	// **migration 0014 で前提が変わった**。active の一意キーが
	// (symbol, mode) → (symbol, mode, **strategy_name**) に緩んだので、**戦略が違えば
	// 2 枚目の active は正しく挿さる** —— それが「1 銘柄に複数アームを載せる」目的。
	// 旧テストはここで「挿せないこと」を前提にしており、0014 を当てた時点で落ちた
	// (integration 実行で発覚。integration は普段回らないので気づけなかった)。
	if err := repo.EnsureExists(ctx, llm); err != nil {
		t.Fatalf("戦略が違う 2 枚目の active が挿せない(ペアが成立しない): %v", err)
	}
	// **同じ戦略**なら従来どおり衝突する(緩めたのは戦略軸だけ)。
	dup := def
	dup.ConfigID = "no_trade_default_7203_dup"
	if err := repo.EnsureExists(ctx, dup); err == nil {
		t.Fatal("同一戦略で 2 枚目の active が挿さった — 索引が緩みすぎている")
	}
	// ActivateExclusive は**同じ戦略の**旧 active を expire して切り替える。
	if err := repo.ActivateExclusive(ctx, llm); err != nil {
		t.Fatalf("ActivateExclusive: %v", err)
	}
	// 🛑 (銘柄, mode) では一意に決まらないので、引くときは戦略まで指定する。
	got, err := activeByStrategy(ctx, pool, "7203", "paper_config", "abs_momentum")
	if err != nil || got == nil || got.ConfigID != llm.ConfigID {
		t.Fatalf("active が切り替わっていない: %v %+v", err, got)
	}
	// **no_trade の default は expire されない**(0014)。戦略が違うので
	// 競合しない —— これは in-memory 側の `app.ConfigSet`(base = no_trade + arms)と
	// 同じ形で、**base があるまま各アームが載る**のが正しい状態。
	// 旧テストはここで「default が expired になること」を期待していたが、それは
	// 1銘柄1 config 時代の契約。
	var defStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM strategy_configs WHERE config_id=$1`, def.ConfigID).Scan(&defStatus); err != nil || defStatus != "active" {
		t.Fatalf("no_trade の base が落とされた(戦略が違う config は共存する): %v %q", err, defStatus)
	}
	// 同一 config_id の再活性化は冪等。
	if err := repo.ActivateExclusive(ctx, llm); err != nil {
		t.Fatalf("再活性化が冪等でない: %v", err)
	}

	// 🛑 **同じ戦略の中では従来どおり 1 枚だけ**。緩めたのは戦略軸のみで、
	// 「同じアームの config が 2 枚 active」は起きてはいけない。
	llm2 := llm
	llm2.ConfigID = "20260724-124500-7203"
	if err := repo.ActivateExclusive(ctx, llm2); err != nil {
		t.Fatalf("同一戦略の切替: %v", err)
	}
	var oldLLM string
	if err := pool.QueryRow(ctx, `SELECT status FROM strategy_configs WHERE config_id=$1`, llm.ConfigID).Scan(&oldLLM); err != nil || oldLLM != "expired" {
		t.Fatalf("同一戦略の旧 active が expire されていない: %v %q", err, oldLLM)
	}
	got, err = activeByStrategy(ctx, pool, "7203", "paper_config", "abs_momentum")
	if err != nil || got == nil || got.ConfigID != llm2.ConfigID {
		t.Fatalf("同一戦略の切替に失敗: %v %+v", err, got)
	}
	// base は無傷のまま(アームの入れ替えが base を巻き込まない)。
	if err := pool.QueryRow(ctx, `SELECT status FROM strategy_configs WHERE config_id=$1`, def.ConfigID).Scan(&defStatus); err != nil || defStatus != "active" {
		t.Fatalf("アームの切替が base を巻き込んだ: %v %q", err, defStatus)
	}
}

// migration 0007: config の出自(advisor_run_id)が保存・読出しで
// 保たれること。run 無し(人手・テスト・sentinel)の config は NULL のまま
// 保存できること。
func TestPg_ConfigAdvisorRunIDRoundTrip(t *testing.T) {
	pool, done := requireDB(t)
	defer done()
	ctx := context.Background()
	repo := NewConfigRepo(pool)

	withRun := port.StrategyConfigRecord{ConfigID: "20260731-090000-7203", Symbol: "7203",
		Mode: "paper_config", StrategyName: "abs_momentum", Status: "active", RawYAML: "y",
		AdvisorRunID: "20260731T000000Z-deadbeef"}
	if err := repo.ActivateExclusive(ctx, withRun); err != nil {
		t.Fatalf("activate with run id: %v", err)
	}
	got, err := activeByStrategy(ctx, pool, "7203", "paper_config", "abs_momentum")
	if err != nil || got == nil || got.AdvisorRunID != withRun.AdvisorRunID {
		t.Fatalf("advisor_run_id が往復しない: err=%v got=%+v", err, got)
	}

	// run 無し config も保存できる(NULL)。
	noRun := port.StrategyConfigRecord{ConfigID: "manual-7203", Symbol: "7203",
		Mode: "paper_config", StrategyName: "no_trade", Status: "active", RawYAML: "y2"}
	if err := repo.ActivateExclusive(ctx, noRun); err != nil {
		t.Fatalf("activate without run id: %v", err)
	}
	// 0014 以降、no_trade と abs_momentum は**別戦略なので両方 active のまま**。
	// 戦略を指定して引かないと、どちらが返るか順序に依存する(旧テストはここで
	// abs_momentum 側を掴んで落ちていた)。
	got, err = activeByStrategy(ctx, pool, "7203", "paper_config", "no_trade")
	if err != nil || got == nil || got.AdvisorRunID != "" {
		t.Fatalf("run 無し config の advisor_run_id は空であるべき: err=%v got=%+v", err, got)
	}

	// 再活性化(既存行への ON CONFLICT)でも既存の run_id を消さない(config 凍結)。
	if err := repo.ActivateExclusive(ctx, port.StrategyConfigRecord{
		ConfigID: withRun.ConfigID, Symbol: withRun.Symbol, Mode: withRun.Mode,
		StrategyName: withRun.StrategyName, Status: "active", RawYAML: "y"}); err != nil {
		t.Fatalf("re-activate: %v", err)
	}
	var runID *string
	if err := pool.QueryRow(ctx, `SELECT advisor_run_id FROM strategy_configs WHERE config_id=$1`, withRun.ConfigID).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if runID == nil || *runID != withRun.AdvisorRunID {
		t.Fatalf("再活性化で advisor_run_id が消えた: %v", runID)
	}
}

func TestPg_SignalRejectionInsert(t *testing.T) {
	pool, done := requireDB(t)
	defer done()
	ctx := context.Background()

	cfgRepo := NewConfigRepo(pool)
	if err := cfgRepo.EnsureExists(ctx, port.StrategyConfigRecord{ConfigID: "cfg-rej", Symbol: "7203", Mode: "paper_config", StrategyName: "no_trade", Status: "active", RawYAML: "config_id: cfg-rej\n"}); err != nil {
		t.Fatal(err)
	}
	rej := NewRejectionRepo(pool)
	if err := rej.InsertRejection(ctx, port.SignalRejection{Symbol: "7203", ConfigID: "cfg-rej", Reason: "emergency_stop", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("insert rejection: %v", err)
	}
	// Empty config id must store as NULL, not fail.
	if err := rej.InsertRejection(ctx, port.SignalRejection{Symbol: "7203", Reason: "no_active_config", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("insert rejection with empty config: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM signal_rejections WHERE symbol='7203'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("want 2 rejection rows, got %d (err=%v)", n, err)
	}
	// raw_yaml audit: the config row must carry the full YAML text.
	var raw string
	if err := pool.QueryRow(ctx, `SELECT raw_yaml FROM strategy_configs WHERE config_id='cfg-rej'`).Scan(&raw); err != nil || raw == "" {
		t.Fatalf("raw_yaml must be persisted non-empty, got %q (err=%v)", raw, err)
	}
}

// migration 0009: reason は安定種別・detail は可変部で往復すること。
func TestPg_SignalRejectionDetailRoundTrip(t *testing.T) {
	pool, done := requireDB(t)
	defer done()
	ctx := context.Background()

	rej := NewRejectionRepo(pool)
	if err := rej.InsertRejection(ctx, port.SignalRejection{
		Symbol: "7203", Reason: "cooldown", Detail: "after_loss until 09:37:25", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var reason, detail string
	if err := pool.QueryRow(ctx, `SELECT reason, detail FROM signal_rejections WHERE symbol='7203'`).Scan(&reason, &detail); err != nil {
		t.Fatal(err)
	}
	if reason != "cooldown" || detail != "after_loss until 09:37:25" {
		t.Fatalf("kind/detail が往復しない: %q %q", reason, detail)
	}
}

// migration 0008: 1ラウンドぶんの screen 結果が COPY で入ること。
func TestPg_ScreenSnapshotInsertRound(t *testing.T) {
	pool, done := requireDB(t)
	defer done()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `TRUNCATE screen_snapshots RESTART IDENTITY`); err != nil {
		t.Fatalf("truncate screen_snapshots: %v", err)
	}

	repo := NewScreenSnapshotRepo(pool)
	at := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
	rows := []port.ScreenSnapshot{
		{RoundAt: at, Symbol: "7203", Strategy: "abs_momentum", Triggered: true, Score: 1.42, Picked: true},
		{RoundAt: at, Symbol: "7203", Strategy: "donchian_breakout", Triggered: false, Score: 0.98},
		{RoundAt: at, Symbol: "6758", Strategy: "abs_momentum", Triggered: true, Score: 1.10},
	}
	if err := repo.InsertRound(ctx, rows); err != nil {
		t.Fatalf("InsertRound: %v", err)
	}
	if err := repo.InsertRound(ctx, nil); err != nil {
		t.Fatalf("空ラウンドは no-op であるべき: %v", err)
	}
	var n, picked int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE picked) FROM screen_snapshots`).Scan(&n, &picked); err != nil {
		t.Fatal(err)
	}
	if n != 3 || picked != 1 {
		t.Fatalf("rows=%d picked=%d, want 3/1", n, picked)
	}
	var score float64
	if err := pool.QueryRow(ctx, `SELECT score FROM screen_snapshots WHERE symbol='7203' AND strategy='abs_momentum'`).Scan(&score); err != nil || score != 1.42 {
		t.Fatalf("score が往復しない: %v %v", score, err)
	}
}

// 修正1: エントリー時 score の復元。advisor_run_id 直参照(0007)と
// 時刻 fallback(60秒窓)の両経路、および窓超過 = 復元不能を1テストで固定する。
func TestPg_ScoreRestoreByRunIDAndTimeFallback(t *testing.T) {
	pool, done := requireDB(t)
	defer done()
	ctx := context.Background()

	cleanup := func() {
		if _, err := pool.Exec(ctx, `DELETE FROM advisor_runs WHERE run_id LIKE 'run-score-%'`); err != nil {
			t.Logf("cleanup: %v", err)
		}
	}
	cleanup()
	defer cleanup()

	jst := clock.JST
	runs := NewAdvisorRunRepo(pool)
	screens := func(strategy string, score float64) string {
		return `{"symbol":"x","advisory":{"screens":[{"strategy":"` + strategy + `","score":` + fmt.Sprintf("%g", score) + `}]}}`
	}
	// A: FK 直参照。started_at は config 時刻とかけ離れていても FK が勝つ。
	if err := runs.Insert(ctx, port.AdvisorRunRecord{RunID: "run-score-A", Symbol: "7203", Status: port.AdvisorRunSuccess,
		StartedAt: time.Date(2026, 7, 31, 0, 0, 0, 0, jst), InputJSON: screens("abs_momentum", 1.5)}); err != nil {
		t.Fatal(err)
	}
	// B: FK 無し → 時刻 fallback(config 10:00:00 に対し started_at 10:00:21 = 窓内)。
	if err := runs.Insert(ctx, port.AdvisorRunRecord{RunID: "run-score-B", Symbol: "6758", Status: port.AdvisorRunSuccess,
		StartedAt: time.Date(2026, 7, 31, 10, 0, 21, 0, jst), InputJSON: screens("donchian_breakout", 1.01)}); err != nil {
		t.Fatal(err)
	}
	// C: FK 無し・最近傍 run が窓外(2時間ズレ)→ 復元不能。
	if err := runs.Insert(ctx, port.AdvisorRunRecord{RunID: "run-score-C", Symbol: "9984", Status: port.AdvisorRunSuccess,
		StartedAt: time.Date(2026, 7, 31, 13, 0, 0, 0, jst), InputJSON: screens("atr_breakout", 1.2)}); err != nil {
		t.Fatal(err)
	}
	// D: FK 無し・60秒窓に success run が **2件** → 曖昧 = 復元不能
	// (バックフィルと同じポリシー。近い方を推測で採らない)。
	for i, sec := range []int{10, 40} {
		if err := runs.Insert(ctx, port.AdvisorRunRecord{RunID: fmt.Sprintf("run-score-D%d", i), Symbol: "8035", Status: port.AdvisorRunSuccess,
			StartedAt: time.Date(2026, 7, 31, 12, 0, sec, 0, jst), InputJSON: screens("abs_momentum", 1.3)}); err != nil {
			t.Fatal(err)
		}
	}
	// E: FK 無し・窓内に success 1件 + cli_error 1件 → 失敗 run は候補外なので
	// 曖昧にならず、success の1件で復元できる。
	if err := runs.Insert(ctx, port.AdvisorRunRecord{RunID: "run-score-E1", Symbol: "9433", Status: port.AdvisorRunSuccess,
		StartedAt: time.Date(2026, 7, 31, 14, 0, 5, 0, jst), InputJSON: screens("donchian_breakout", 1.05)}); err != nil {
		t.Fatal(err)
	}
	if err := runs.Insert(ctx, port.AdvisorRunRecord{RunID: "run-score-E2", Symbol: "9433", Status: port.AdvisorRunCLIError,
		StartedAt: time.Date(2026, 7, 31, 14, 0, 2, 0, jst), InputJSON: ""}); err != nil {
		t.Fatal(err)
	}

	cfgRepo := NewConfigRepo(pool)
	mkCfg := func(id, sym, strat, runID string) {
		t.Helper()
		if err := cfgRepo.EnsureExists(ctx, port.StrategyConfigRecord{ConfigID: id, Symbol: sym,
			Mode: "paper_config", StrategyName: strat, Status: "expired", RawYAML: "y", AdvisorRunID: runID}); err != nil {
			t.Fatalf("cfg %s: %v", id, err)
		}
	}
	mkCfg("20260731-093000-7203", "7203", "abs_momentum", "run-score-A")
	mkCfg("20260731-100000-6758", "6758", "donchian_breakout", "")
	mkCfg("20260731-110000-9984", "9984", "atr_breakout", "")
	mkCfg("20260731-120020-8035", "8035", "abs_momentum", "")
	mkCfg("20260731-140000-9433", "9433", "donchian_breakout", "")

	mkPos := func(sym, cfgID string) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO positions (symbol, side, quantity, entry_price, config_id, holding_mode, exec_kind, status, opened_at)
			VALUES ($1,'BUY',100,1000,$2,'multiday','cash','CLOSED',now()) RETURNING id`, sym, cfgID).Scan(&id); err != nil {
			t.Fatalf("pos %s: %v", sym, err)
		}
		return id
	}
	pA := mkPos("7203", "20260731-093000-7203")
	pB := mkPos("6758", "20260731-100000-6758")
	pC := mkPos("9984", "20260731-110000-9984")
	pD := mkPos("8035", "20260731-120020-8035")
	pE := mkPos("9433", "20260731-140000-9433")

	got, err := NewScoreRepo(pool).ScoreByPositionID(ctx, []int64{pA, pB, pC, pD, pE})
	if err != nil {
		t.Fatalf("ScoreByPositionID: %v", err)
	}
	if s, ok := got[pA]; !ok || s.Score != 1.5 || s.RunID != "run-score-A" {
		t.Fatalf("FK 直参照の復元が違う: %+v", got[pA])
	}
	if s, ok := got[pB]; !ok || s.Score != 1.01 || s.RunID != "run-score-B" {
		t.Fatalf("時刻 fallback の復元が違う: %+v", got[pB])
	}
	if _, ok := got[pC]; ok {
		t.Fatalf("窓外の run で復元してはいけない(誤リンク): %+v", got[pC])
	}
	if _, ok := got[pD]; ok {
		t.Fatalf("曖昧(窓内2件)は復元不能であるべき: %+v", got[pD])
	}
	if s, ok := got[pE]; !ok || s.Score != 1.05 || s.RunID != "run-score-E1" {
		t.Fatalf("失敗 run は候補外・success 1件で復元されるべき: %+v", got[pE])
	}
}

// TestPg_AdvisorRunModelRoundTrip — advisor 監査行の書込/読出が実 Postgres で
// 通ること、特に **どの LLM 世代が config を書いたか**(model, migration 0004)が
// 往復すること。この経路は best-effort insert(失敗はログのみで握り潰す)なので、
// SQL の列ズレは実行時に誰にも気付かれない。ここで固定する。
//
// 後片付けは自テストが作った run_id だけを消す(advisor_runs は監査証跡なので
// 一括削除しない — truncateAll に足さないのも同じ理由)。
func TestPg_AdvisorRunModelRoundTrip(t *testing.T) {
	pool, done := requireDB(t)
	defer done()
	ctx := context.Background()

	cleanup := func() {
		if _, err := pool.Exec(ctx, `DELETE FROM advisor_runs WHERE run_id LIKE 'run-model-%'`); err != nil {
			t.Logf("cleanup: %v", err)
		}
	}
	cleanup()
	defer cleanup()

	repo := NewAdvisorRunRepo(pool)
	started := time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond)
	rec := port.AdvisorRunRecord{
		RunID: "run-model-1", Symbol: "7203", Status: port.AdvisorRunSuccess,
		StartedAt: started, FinishedAt: started.Add(3 * time.Second),
		InputJSON: `{"symbol":"7203"}`, OutputYAML: "config_id: c1\n", ParsedYAML: "config_id: c1\n",
		RegimeType: "panic_crash", RegimeConfidence: 0.8, RegimeReason: "25日線-15%",
		Model: "claude-opus-5",
	}
	if err := repo.Insert(ctx, rec); err != nil {
		t.Fatalf("insert: %v", err)
	}

	find := func(runID string) (port.AdvisorRunRecord, bool) {
		t.Helper()
		rows, err := repo.List(ctx, "7203", 200)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, r := range rows {
			if r.RunID == runID {
				return r, true
			}
		}
		return port.AdvisorRunRecord{}, false
	}

	got, ok := find("run-model-1")
	if !ok || got.Model != "claude-opus-5" {
		t.Fatalf("model が往復していない: %+v (found=%v)", got, ok)
	}

	// upsert(同 run_id の再 insert)でも model が更新されること。
	rec.Model = "claude-opus-5.1"
	if err := repo.Insert(ctx, rec); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got, ok = find("run-model-1"); !ok || got.Model != "claude-opus-5.1" {
		t.Fatalf("upsert で model が更新されない: %+v (found=%v)", got, ok)
	}

	// model 未設定(古い CLI / envelope なし)は空文字で入る = 「不明」。
	if err := repo.Insert(ctx, port.AdvisorRunRecord{
		RunID: "run-model-2", Symbol: "7203", Status: port.AdvisorRunCLIError,
		StartedAt: started, ErrorMsg: "boom",
	}); err != nil {
		t.Fatalf("insert without model: %v", err)
	}
	if got, ok = find("run-model-2"); !ok || got.Model != "" {
		t.Fatalf("model 未設定は空文字(不明)であるべき: %+v (found=%v)", got, ok)
	}
}

// activeByStrategy はキー(銘柄, mode, 戦略)で active を 1 行引く(テストの読み口。
// 本番に読み手は無い — config の SoT は YAML)。無ければ (nil, nil)。
func activeByStrategy(ctx context.Context, pool *pgxpool.Pool, symbol, mode, strategy string) (*port.StrategyConfigRecord, error) {
	var rec port.StrategyConfigRecord
	var runID *string
	err := pool.QueryRow(ctx, `
		SELECT config_id, symbol, mode, strategy_name, status, raw_yaml, advisor_run_id, activated_at
		FROM strategy_configs WHERE symbol=$1 AND mode=$2 AND status='active' AND strategy_name=$3`,
		symbol, mode, strategy).Scan(&rec.ConfigID, &rec.Symbol, &rec.Mode, &rec.StrategyName,
		&rec.Status, &rec.RawYAML, &runID, &rec.ActivatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if runID != nil {
		rec.AdvisorRunID = *runID
	}
	return &rec, nil
}
