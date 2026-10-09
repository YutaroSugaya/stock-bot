package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"stockbot/backend/internal/adapter/repository/pg"
	"stockbot/backend/internal/app"
	"stockbot/backend/internal/app/journal"
	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/session"
)

// 引け後の記録ジョブを組み立てる。**人間の運用を make start / make stop だけにする**ための
// 配線 — launchd への登録は Makefile を触るので人間の作業になる。
//
// 🛑 ここに登録してよいのは **read-only の記録**だけ。発注も台帳の書込もしない。
// 取引経路とは何も共有しない(AfterClose 側が goroutine・recover・timeout を持つ)。
func newAfterCloseJobs(botCfg *config.BotConfig, hours session.TradingHours, clk clock.Clock, logger *slog.Logger) *app.AfterClose {
	// 大引け 15:30 の 10 分後。終値の確定と約定反映を待つ。
	ac := app.NewAfterClose("15:40", hours, clk, logger)
	// JPX のストップ配分 CSV は取り込まない(404 が続く・取引判断に不使用)。

	// (1) 日次総評の段1(決定論パケット)。DB が無い構成(in-memory)では登録しない —
	// 台帳が無いのに空の記録を作らない。
	if url := os.Getenv("STOCKBOT_DATABASE_URL"); url != "" {
		ac.Add("daily-review", func(ctx context.Context) error {
			pool, err := pg.Open(ctx, url)
			if err != nil {
				return err
			}
			defer pool.Close()
			p, err := journal.Build(ctx, pg.NewJournalRepo(pool), clk(), dataDirAbs(botCfg), journal.DefaultUniversePath())
			if err != nil {
				return err
			}
			path, _, err := journal.WriteJSON(journal.DefaultDir(), p)
			if err != nil {
				return err
			}
			logger.Info("daily journal written", "path", path, "closed", p.Ledger.Trades.N, "open", p.Ledger.Open.N)
			return nil
		})
	}
	// (1b) live トラックの日次総評(決定論パケットのみ)。**research とは別 DB・別ディレクトリ**。
	// これが無いと、実弾が動いた日でも引け後の記録が research の話しかせず、live の 1 日が
	// どこにも自動記録されない。
	// 🛑 **段2(LLM)は live には付けない** — live プロセスが LLM 子プロセスを起こさない、
	// という不変条件を env 1 個に落とさないため(下の段2 のコメントと同じ理由)。
	if url := os.Getenv("STOCKBOT_LIVE_DATABASE_URL"); url != "" {
		outDir := journalDirForTrack("live")
		ac.Add("daily-review-live", func(ctx context.Context) error {
			pool, err := pg.Open(ctx, url)
			if err != nil {
				return err
			}
			defer pool.Close()
			p, err := journal.Build(ctx, pg.NewJournalRepo(pool), clk(), dataDirAbs(botCfg), journal.DefaultUniversePath())
			if err != nil {
				return err
			}
			path, _, err := journal.WriteJSON(outDir, p)
			if err != nil {
				return err
			}
			logger.Info("daily journal written", "track", "live", "path", path,
				"closed", p.Ledger.Trades.N, "open", p.Ledger.Open.N)
			return nil
		})
	}

	// (2) 日次総評の段2(LLM が文章を書く)。**発注経路の外**で、入力は段1 の JSON だけ。
	// 段 2 は claude CLI を呼ぶ(課金・ネットワーク)ので opt-in: `STOCKBOT_DAILY_REVIEW_STAGE2=on` のときだけ配線する。
	//
	// 🛑 **`mode: live_config` では登録しない。** live の bot_config は advisor を構成として
	// 拒否し(`catastrophe_guards_test`)、「LLM は発注経路に入れない」を機械強制している。
	// 段2 は発注経路の外だが、live プロセスが LLM 子プロセスを起こさない保証を env 変数
	// 1個に落とさない(fail-close 側に倒す)。
	//
	// 🛑 AddSlow — 停止経路では走らせない(理由は afterclose.go)。
	if os.Getenv("STOCKBOT_DAILY_REVIEW_STAGE2") == "on" && botCfg.Mode != config.ModeLive {
		if url := os.Getenv("STOCKBOT_DATABASE_URL"); url != "" {
			cli := getenv("STOCKBOT_CLAUDE_BIN", "claude")
			dataDir := dataDirAbs(botCfg)
			stage2 := func(ctx context.Context) error {
				return runStage2Backfill(ctx, stage2Deps{
					cli: cli, outDir: journal.DefaultDir(), dbURL: url, dataDir: dataDir,
					now: clk(), logger: logger, rebuild: rebuildJournalDay, run: journal.RunStage2,
				})
			}
			// 朝枠: 前日の総評を**寄り前**に書く。段2 は前日の日足が要り、
			// それは翌朝の日足取得でしか入らないので、15:40 の枠だけだと丸 1 日遅れていた。
			// ready = 今朝のユニバース選定が済んだ(= 日足取得が終わった)。途中で書くと欠けた
			// 市況の総評が O_EXCL で永久に固定される。08:30 を締切にするのは advisor の最初の
			// 判断(08:45 頃)と Claude の枠を取り合わないため。書けなかった日は 15:40 の枠が拾う
			// (朝に試した日は試行印が付くので、15:40 に同じ日を二重に焼かない)。
			universe := journal.DefaultUniversePath()
			ac.AddMorning("daily-review-stage2-morning", stage2MorningFrom, stage2MorningUntil, stage2JobTimeout,
				func(now time.Time) bool { return universeSelectedToday(universe, now) }, stage2)
			ac.AddSlow("daily-review-stage2", stage2JobTimeout, stage2)
		}
	}
	return ac
}

// 段2 の予算。**1日あたりの上限 × 日数がジョブ枠に収まること**(guard test あり)。
//
// 🛑 **10分では足りない。** 同じ `--effort max` を使う advisor は実測 621秒(10分21秒)で、
// そのために `timeout_seconds: -1`(無制限)に倒してある(`catastrophe_guards_test` /
// `bot_config.advisor.yaml`)。段2 はさらに WebSearch のラウンドが乗る。短く切ると
// **毎日 timeout → 翌日 retry → また timeout** の無音ループになる(手動実行の実測は
// 約 3分だが、余裕を取る)。引け後なので長くても誰も待たない。
const (
	stage2JobTimeout    = 65 * time.Minute
	stage2PerDayTimeout = 30 * time.Minute
	stage2MaxDays       = 2  // 連打しない。3日以上溜まったら古い順から落ちる(承知の上)
	stage2LookbackDays  = 10 // **暦日**。これより古い日は諦める(人間が後から書ける)

	// 朝枠の時間帯 [from, until)。朝の日足取得(07:00〜約40分)の後、advisor の最初の判断の前。
	stage2MorningFrom  = "07:00"
	stage2MorningUntil = "08:30"
)

type (
	// stage2Fn は journal.RunStage2 のシグネチャ(テストで差し替える)。
	stage2Fn func(ctx context.Context, cli, outDir, date string, packet []byte, timeout time.Duration) (string, error)
	// rebuildFn は対象日の段1 を**測り直して** JSON を書き、その中身を返す。
	rebuildFn func(ctx context.Context, dbURL, dataDir, outDir, date string) ([]byte, error)
)

type stage2Deps struct {
	cli, outDir, dbURL, dataDir string
	now                         time.Time
	logger                      *slog.Logger
	rebuild                     rebuildFn
	run                         stage2Fn
}

// runStage2Backfill は「段1 の JSON はあるが .md が無い」**前日以前**の日を新しい順に書く。
//
// 🛑 **段1 を測り直してから段2 に渡す。** 引け直後に組んだ段1 は当日の日足がまだ無く、
// 市況が「取得できず」で埋まっている。段2 は `O_EXCL` で二度と書き直せないので、
// そのまま渡すと市況の無い総評が永久に固定される。
//
// **1日の失敗で他の日を落とさない**(1日ずつ独立に試し、最後にまとめて報告する)。
func runStage2Backfill(ctx context.Context, d stage2Deps) error {
	var failed []string
	for _, day := range journal.PendingStage2Days(d.outDir, d.now, stage2MaxDays, stage2LookbackDays) {
		// 成否に関わらず「今日はこの日を試した」印を先に残す。再起動のたびに
		// 同じ日へ Opus/max を投げ直さない(claude の枠は advisor と共有)。
		if err := journal.MarkStage2Attempt(d.outDir, day, d.now); err != nil {
			failed = append(failed, day+": 試行印を書けず: "+err.Error())
			continue
		}
		packet, err := d.rebuild(ctx, d.dbURL, d.dataDir, d.outDir, day)
		if err != nil {
			failed = append(failed, day+": 段1 の測り直しに失敗: "+err.Error())
			clearAttemptIfCancelled(ctx, d.outDir, day)
			continue
		}
		md, err := d.run(ctx, d.cli, d.outDir, day, packet, stage2PerDayTimeout)
		if err != nil {
			failed = append(failed, day+": "+err.Error())
			clearAttemptIfCancelled(ctx, d.outDir, day)
			continue
		}
		if d.logger != nil {
			d.logger.Info("daily review stage2 written", "date", day, "path", md)
		}
	}
	if len(failed) > 0 {
		// 段1 の JSON は残っているので数字は失われない。警告として上げる。
		return fmt.Errorf("段2 を書けなかった日: %v", failed)
	}
	return nil
}

// clearAttemptIfCancelled は「停止で切られただけ」の試行印を消す。
//
// 🛑 15:40 の直後に停止されると段2 は毎回途中で切られる。印を残すとその日は
// もう書かれず、**毎日欠測する**。消しておけば次の起動でそのまま書ける。
// 消すのは中断のときだけ — 本物の失敗(CLI エラー等)は印を残して連打を防ぐ。
func clearAttemptIfCancelled(ctx context.Context, outDir, day string) {
	if ctx.Err() == nil {
		return
	}
	_ = journal.ClearStage2Attempt(outDir, day)
}

// rebuildJournalDay は対象日の段1 を組み直して JSON を上書きし、その中身を返す。
// ユニバースは `today.txt` ではなく**その日の archive** を引く(today.txt は毎朝
// 上書きされるので、過去日を今日の銘柄集合で組むとズレる)。
func rebuildJournalDay(ctx context.Context, dbURL, dataDir, outDir, date string) ([]byte, error) {
	day, err := time.ParseInLocation("2006-01-02", date, clock.JST)
	if err != nil {
		return nil, err
	}
	pool, err := pg.Open(ctx, dbURL)
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	p, err := journal.Build(ctx, pg.NewJournalRepo(pool), day, dataDir, journal.ArchiveUniversePath(date))
	if err != nil {
		return nil, err
	}
	// 🚨 **空の DB で過去日を測り直して上書きしない**。
	//
	// `WriteJSON` の docstring は「台帳と CSV しか読まないので測り直しても同じ数字に
	// なる」ことを上書きの根拠にしていた。その前提は **DB を期間ごとに物理分離した
	// 瞬間に崩れる**: 既定 DSN を新しい DB に切り替えた後、
	// 段2 の backfill が「段1 JSON はあるが .md が無い」前の期間の日を拾い、
	// **その期間の trades が 1 行も無い DB** で組み直してしまう。空 DB でもエラーには
	// ならないので、前の期間の最終日の JSON が全ゼロで上書きされ、その上で LLM が
	// 「取引ゼロの日」の総評を書き、**`.md` は O_EXCL なので二度と書き直せない**。
	//
	// したがって fail-close: **既存 JSON に中身があるのに測り直しが空になったら
	// 上書きも段2 も行わない**。人間が旧 DSN を指して書き直せる状態のまま残す。
	if err := journal.RefuseEmptyRebuild(outDir, date, p); err != nil {
		return nil, err
	}
	_, blob, err := journal.WriteJSON(outDir, p)
	return blob, err
}

// journalDirForTrack は日次パケットの出力先。**トラックごとに分ける** — ファイル名は
// 日付なので、research と live を同じディレクトリに書くと片方が上書きで消える。
// "" = research(既定の ~/.stockbot/journal)。
func journalDirForTrack(track string) string {
	if track == "" {
		return journal.DefaultDir()
	}
	return filepath.Join(journal.DefaultDir(), track)
}

// dataDirAbs は日足 CSV / 記録の置き場。bot の CWD 依存を避けて絶対パスにする。
func dataDirAbs(botCfg *config.BotConfig) string {
	dir := getenv("STOCKBOT_DAILY_CANDLES_DIR", "data")
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// universeSelectedToday は今朝のユニバース選定が済んでいるか(ファイルの更新日が今日 JST)。
// 朝ジョブ・catchup とも**日足取得の後に**選定するので、これが真なら前日の日足は揃っている。
func universeSelectedToday(path string, now time.Time) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	return st.ModTime().In(clock.JST).Format("2006-01-02") == now.In(clock.JST).Format("2006-01-02")
}
