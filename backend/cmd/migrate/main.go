// Command migrate applies the SQL migrations in order. It tracks
// applied versions in schema_migrations and supports up/down/status. Writes are
// gated: when the target DB name ends in _backtest the SafeBacktestDSN guard
// must pass; the live DB requires the human-approved env.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"stockbot/backend/internal/adapter/repository"
)

var migPattern = regexp.MustCompile(`^(\d{4})_.+\.(up|down)\.sql$`)

func main() {
	var (
		dir = flag.String("dir", "migrations", "migrations directory")
		cmd = flag.String("cmd", "status", "up|down|status")
	)
	flag.Parse()

	dsn := os.Getenv("STOCKBOT_DATABASE_URL")
	if dsn == "" {
		fatal(fmt.Errorf("STOCKBOT_DATABASE_URL is required"))
	}
	// Write-guard: a *_backtest target must pass the double-wall; the live DB
	// requires the explicit human-approved env.
	if strings.HasSuffix(dbName(dsn), "_backtest") {
		// 🛑 protected に渡すのは **target 以外の実データ DSN**。target(= dsn 自身)を
		// 混ぜてはいけない — 第2壁は「target が実データと一致しないこと」なので、自分自身を
		// 渡すと必ず一致して *_backtest 宛てが**常に落ちる**。
		// STOCKBOT_DATABASE_URL を二重に渡すと、add-migration が指示する rollback 手順
		// (STOCKBOT_DATABASE_URL に *_backtest を入れて make migrate-up)が構造的に実行
		// 不能だった。research 側(STOCKBOT_DATABASE_URL)は **db 名が _backtest で終わる**
		// という第1壁で守られる(実データの DB 名は stockbot / stockbot_live)。
		// 🚨 **harvest も守る**。DB が 3 本になったのに二重壁は live しか
		// 見ていないと、harvest(= 旧台帳)は
		// 第1壁(db 名が _backtest で終わる)だけで守られていた。名前が違う以上は
		// 通らないが、**守る対象の一覧に載っていないこと自体が次の事故の形**
		// (backup / restore-drill / reset-trades が全部 harvest を取りこぼしていた)。
		if err := repository.SafeBacktestDSN(dsn,
			os.Getenv("STOCKBOT_LIVE_DATABASE_URL"),
			os.Getenv("STOCKBOT_HARVEST_DATABASE_URL")); err != nil {
			fatal(err)
		}
	} else if os.Getenv("STOCKBOT_HUMAN_APPROVED_DB_WRITE") != "1" {
		fatal(fmt.Errorf("migrating a non-_backtest DB requires STOCKBOT_HUMAN_APPROVED_DB_WRITE=1"))
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fatal(err)
	}
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		fatal(err)
	}

	switch *cmd {
	case "status":
		must(status(ctx, conn, *dir))
	case "up":
		must(up(ctx, conn, *dir))
	case "down":
		must(down(ctx, conn, *dir))
	default:
		fatal(fmt.Errorf("unknown -cmd %q (up|down|status)", *cmd))
	}
}

func versions(dir, kind string) ([]string, map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	files := map[string]string{}
	var vers []string
	for _, e := range entries {
		m := migPattern.FindStringSubmatch(e.Name())
		if m == nil || m[2] != kind {
			continue
		}
		files[m[1]] = filepath.Join(dir, e.Name())
		vers = append(vers, m[1])
	}
	sort.Strings(vers)
	return vers, files, nil
}

func applied(ctx context.Context, conn *pgx.Conn) (map[string]bool, error) {
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

func up(ctx context.Context, conn *pgx.Conn, dir string) error {
	vers, files, err := versions(dir, "up")
	if err != nil {
		return err
	}
	done, err := applied(ctx, conn)
	if err != nil {
		return err
	}
	for _, v := range vers {
		if done[v] {
			continue
		}
		sql, err := os.ReadFile(files[v])
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply %s: %w", v, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, v); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		fmt.Printf("applied %s\n", v)
	}
	return nil
}

func down(ctx context.Context, conn *pgx.Conn, dir string) error {
	vers, files, err := versions(dir, "down")
	if err != nil {
		return err
	}
	done, err := applied(ctx, conn)
	if err != nil {
		return err
	}
	// roll back the highest applied version only
	for i := len(vers) - 1; i >= 0; i-- {
		v := vers[i]
		if !done[v] {
			continue
		}
		sql, err := os.ReadFile(files[v])
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("rollback %s: %w", v, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version=$1`, v); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		fmt.Printf("rolled back %s\n", v)
		return nil
	}
	fmt.Println("nothing to roll back")
	return nil
}

func status(ctx context.Context, conn *pgx.Conn, dir string) error {
	vers, _, err := versions(dir, "up")
	if err != nil {
		return err
	}
	done, err := applied(ctx, conn)
	if err != nil {
		return err
	}
	for _, v := range vers {
		mark := "pending"
		if done[v] {
			mark = "applied"
		}
		fmt.Printf("%s  %s\n", v, mark)
	}
	return nil
}

func dbName(dsn string) string {
	if i := strings.LastIndex(dsn, "/"); i >= 0 {
		name := dsn[i+1:]
		if j := strings.IndexAny(name, "?"); j >= 0 {
			name = name[:j]
		}
		return name
	}
	return ""
}

func must(err error) {
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "migrate:", err)
	os.Exit(1)
}
