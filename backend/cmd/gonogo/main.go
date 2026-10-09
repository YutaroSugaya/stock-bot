// Command gonogo は寄り前の銘柄判定(go/no-go)。**表示と記録だけ**で、bot の発注経路は読まない。
//
// 今日 arm しうる銘柄(paper の bnf 家族の入口と live の入口・arm と同じ関数で出す)について、
// 直近の急落が「平均回帰の前提を壊す固有の出来事」によるものかを claude CLI(WebSearch / WebFetch
// だけ)で判定し、~/.stockbot/gonogo/YYYY-MM-DD.jsonl へ追記・.md を書き直す。
//
// 起動経路は 3 つで、どれから起動しても同じ結果になる(冪等: 成功した判定がある銘柄は判定し直さない):
//  1. launchd com.stockbot.gonogo(取引日の 08:15 と 08:40。bot が起動していなくても動く)
//  2. make start の catchup の最後(バックグラウンド)
//  3. 手動: gonogo -symbols 6594,7203(端末、または bot の画面の「未判定を判定」ボタン)
//
// どの経路で起動したかは env STOCKBOT_GONOGO_TRIGGER(launchd / catchup / button。無ければ manual)で受け、
// 判定の各行に残す。フラグにしないのは、ビルドに失敗した日に古いバイナリが未知のフラグで落ちないため。
//
// 🛑 launchd 配下のバイナリは Desktop(repo)に触れた瞬間に止められる。読むのは ~/.stockbot の写しだけ
// (configs/ は catchup が写す)。プロンプトはバイナリに埋め込み済み。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"stockbot/backend/internal/adapter/candlecsv"
	adapter "stockbot/backend/internal/adapter/gonogo"
	"stockbot/backend/internal/app"
	"stockbot/backend/internal/app/gonogo"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/market"
)

// 実行の上限(全銘柄が寄り前に終わるように)。銘柄ごとのタイムアウト・同時実行数・1 回の実行の上限時間。
const (
	defaultConcurrency      = 3
	defaultPerSymbolTimeout = 6 * time.Minute
	defaultRunDeadline      = 40 * time.Minute
)

func main() {
	os.Exit(run())
}

func run() int {
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, ".stockbot")
	var (
		configDir   = flag.String("config-dir", filepath.Join(base, "configs"), "bot_config / strategy_config の写し(catchup が写す)")
		hardLimits  = flag.String("hard-limits", filepath.Join(base, "hard_limits.yaml"), "hard_limits.yaml の写し")
		dataDir     = flag.String("data", filepath.Join(base, "data"), "日足 CSV")
		universe    = flag.String("universe", filepath.Join(base, "universe", "today.txt"), "今日のユニバース")
		outDir      = flag.String("out", filepath.Join(base, "gonogo"), "判定ファイルの置き場")
		lockPath    = flag.String("lock", filepath.Join(base, "run", "gonogo.lock"), "実行の排他")
		workDir     = flag.String("workdir", filepath.Join(base, "tmp"), "claude CLI の作業ディレクトリ")
		cliPath     = flag.String("cli", filepath.Join(home, ".local", "bin", "claude"), "claude CLI")
		symbols     = flag.String("symbols", "", "手動: カンマ区切りの銘柄だけを判定する")
		dryRun      = flag.Bool("dry-run", false, "候補を出すだけで LLM を呼ばない")
		concurrency = flag.Int("concurrency", defaultConcurrency, "同時に走らせる claude の本数")
		perSymbol   = flag.Duration("per-symbol-timeout", defaultPerSymbolTimeout, "銘柄ごとのタイムアウト")
		deadline    = flag.Duration("run-deadline", defaultRunDeadline, "1 回の実行の上限時間")
		nowFlag     = flag.String("now", "", "検証用: 現在時刻を上書きする(RFC3339)。-out を別の場所にして使う")
	)
	flag.Parse()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil)).With("cmd", "gonogo")

	release, ok, err := adapter.AcquireLock(*lockPath)
	if err != nil {
		logger.Error("ロックを取れない", "path", *lockPath, "err", err)
		return 1
	}
	if !ok {
		logger.Info("別の gonogo が実行中 — 何もせず終わる", "lock", *lockPath)
		return 0
	}
	defer release()

	hl, err := config.LoadHardLimits(*hardLimits)
	if err != nil {
		logger.Error("hard_limits を読めない", "path", *hardLimits, "err", err)
		return 1
	}
	hours, err := hl.SessionHours.TradingHours()
	if err != nil {
		logger.Error("取引時間を組めない", "err", err)
		return 1
	}
	now := time.Now().In(clock.JST)
	if *nowFlag != "" {
		t, err := time.Parse(time.RFC3339, *nowFlag)
		if err != nil {
			logger.Error("-now を読めない", "err", err)
			return 2
		}
		now = t.In(clock.JST)
	}
	date := now.Format("2006-01-02")

	manual := splitSymbols(*symbols)
	var syms []string
	if len(manual) > 0 {
		syms = manual
	} else {
		syms, err = readUniverse(*universe, hl)
		if err != nil {
			logger.Info("今日のユニバースを読めない — 何もせず終わる(次の起動経路が拾う)", "path", *universe, "err", err)
			return 0
		}
	}
	daily := loadDaily(*dataDir, syms)
	pre := gonogo.Preflight{Hours: hours, Now: now, LatestBarDate: latestBarDate(daily)}
	if st, err := os.Stat(*universe); err == nil {
		pre.UniverseModTime = st.ModTime()
	}
	if reason := pre.Check(len(manual) == 0); reason != "" {
		logger.Info("前提を満たさない — 何もせず終わる(次の起動経路が拾う)", "reason", reason)
		return 0
	}

	var targets []gonogo.Target
	if len(manual) > 0 {
		for _, s := range manual {
			targets = append(targets, gonogo.Target{Symbol: s, Strategies: []string{}, Tracks: []string{"manual"}})
		}
	} else {
		in, err := loadInputs(*configDir, hl, daily)
		if err != nil {
			logger.Error("設定の写しを読めない", "config_dir", *configDir, "err", err)
			return 1
		}
		paper, live, err := gonogo.Candidates(in)
		if err != nil {
			logger.Error("候補を出せない", "err", err)
			return 1
		}
		targets = gonogo.MergeTargets(paper, live)
		logger.Info("判定の候補", "date", date, "paper", len(paper), "live", len(live), "symbols", len(targets))
	}
	if *dryRun {
		for _, t := range targets {
			fmt.Printf("%s\t%s\t%s\n", t.Symbol, strings.Join(t.Tracks, ","), strings.Join(t.Strategies, ","))
		}
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 場が引けたら止める(15:30 以降は判定の意味が無い)。
	if left := hours.MinutesUntilClose(now); left > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(left)*time.Minute)
		defer cancel()
	}
	journal := &adapter.Journal{Dir: *outDir}
	runner := &gonogo.Runner{
		Journal:     journal,
		Judge:       adapter.NewJudge(*cliPath, *workDir, *perSymbol),
		Clock:       clock.System(),
		Concurrency: *concurrency,
		RunDeadline: *deadline,
		Daily:       func(s string) []market.Candle { return daily[s] },
		Logf:        func(msg string, kv ...any) { logger.Info(msg, kv...) },
		Trigger:     triggerFromEnv(os.Getenv("STOCKBOT_GONOGO_TRIGGER")),
	}
	sum, err := runner.Run(ctx, date, targets)
	logger.Info("gonogo_done", "date", date, "targets", sum.Targets, "skipped_already_ok", sum.Skipped,
		"judged", sum.Judged, "no_go", sum.NoGo, "go", sum.Go, "unknown", sum.Unknown,
		"errors", sum.Errors, "usage_limited", sum.UsageLimited, "elapsed", sum.Elapsed.Round(time.Second),
		"md", journal.MarkdownPath(date))
	if err != nil {
		logger.Error("判定ファイルを書けない", "err", err)
		return 1
	}
	return 0
}

// triggerFromEnv は起動経路。知らない値・空は手動(端末で打った)として残す。
func triggerFromEnv(v string) string {
	switch v {
	case gonogo.TriggerLaunchd, gonogo.TriggerCatchup, gonogo.TriggerButton:
		return v
	}
	return gonogo.TriggerManual
}

func splitSymbols(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// readUniverse は今日のユニバース(bot と同じ読み方)から allowed_symbols の外を落とす。
func readUniverse(path string, hl *config.HardLimits) ([]string, error) {
	bc := &config.BotConfig{SymbolsFile: path}
	if err := bc.ResolveSymbols(); err != nil {
		return nil, err
	}
	bc.DropSymbolsOutsideWhitelist(hl)
	return bc.Symbols, nil
}

// loadDaily は日足 CSV(fetch-daily が連鎖調整済み)を、selector と同じ本数・同じ篩で読む。
func loadDaily(dir string, syms []string) map[string][]market.Candle {
	out := make(map[string][]market.Candle, len(syms))
	for _, s := range syms {
		cs, err := candlecsv.LoadDaily(dir, s)
		if err != nil {
			continue
		}
		if n := app.SelectorCandleLookback; len(cs) > n {
			cs = cs[len(cs)-n:]
		}
		if ok, _ := app.ScreenableDaily(cs); !ok {
			continue
		}
		out[s] = cs
	}
	return out
}

func latestBarDate(daily map[string][]market.Candle) string {
	latest := ""
	for _, cs := range daily {
		if len(cs) == 0 {
			continue
		}
		if d := cs[len(cs)-1].OpenTime.In(clock.JST).Format("2006-01-02"); d > latest {
			latest = d
		}
	}
	return latest
}

// configManifest は catchup が写した設定の一覧(~/.stockbot/configs/gonogo-configs.txt)。
// 1 行 1 件の `paper=<file>` / `live_bot=<file>` / `live_strategy=<file>`(優先順)。live が無効なら
// live の行は無い。🛑 env のパス(repo = Desktop 配下)は launchd から読めないので、写しの名前だけを使う。
const configManifest = "gonogo-configs.txt"

func loadInputs(dir string, hl *config.HardLimits, daily map[string][]market.Candle) (gonogo.Inputs, error) {
	in := gonogo.Inputs{Universe: daily, HardLimits: hl}
	raw, err := os.ReadFile(filepath.Join(dir, configManifest))
	if err != nil {
		return in, fmt.Errorf("設定の写しの一覧が無い(make start の catchup が写す): %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		key, file, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		path := filepath.Join(dir, filepath.Base(file))
		switch key {
		case "paper":
			if in.Paper, err = config.LoadBotConfig(path); err != nil {
				return in, fmt.Errorf("paper: %w", err)
			}
		case "live_bot":
			if in.Live, err = config.LoadBotConfig(path); err != nil {
				return in, fmt.Errorf("live: %w", err)
			}
		case "live_strategy":
			c, err := config.LoadStrategyConfig(path)
			if err != nil {
				return in, fmt.Errorf("live strategy: %w", err)
			}
			in.LiveTemplates = append(in.LiveTemplates, c)
		}
	}
	if in.Paper == nil {
		return in, fmt.Errorf("%s に paper の行が無い", configManifest)
	}
	if in.Live != nil && len(in.LiveTemplates) == 0 {
		return in, fmt.Errorf("%s に live_strategy の行が無い", configManifest)
	}
	return in, nil
}
