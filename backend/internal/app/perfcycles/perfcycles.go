// Package perfcycles は画面の戦績を切り替える単位(期間)を組む。期間の表は
// cycles.yaml(データ)で、期間を区切るときに変えるのはそのファイルだけ。
package perfcycles

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"gopkg.in/yaml.v3"

	"stockbot/backend/internal/adapter/repository"
	"stockbot/backend/internal/adapter/repository/pg"
	"stockbot/backend/internal/app/handler"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/port"
	"stockbot/backend/internal/usecase/query"
)

//go:embed cycles.yaml
var raw []byte

// All は全期間(全ての期間を合算)の行のキー。
const All = "all"

// Def は期間 1 本。DB はその期間の台帳、Since は開始日(JST・YYYY-MM-DD)。
type Def struct {
	Key   string `yaml:"key"`
	Label string `yaml:"label"`
	DB    string `yaml:"db"`
	Since string `yaml:"since"`
}

// Table は期間の表。Current は画面の既定(= 最後の行)。
type Table struct {
	Current string `yaml:"current"`
	Cycles  []Def  `yaml:"cycles"`
}

// Load は埋め込みの cycles.yaml を読む。壊れていれば error(呼び手は画面の期間を出さずに続ける)。
func Load() (Table, error) { return parse(raw) }

func parse(b []byte) (Table, error) {
	var t Table
	if err := yaml.Unmarshal(b, &t); err != nil {
		return Table{}, fmt.Errorf("perf cycles: %w", err)
	}
	if len(t.Cycles) == 0 {
		return Table{}, fmt.Errorf("perf cycles: 期間が 1 本も無い")
	}
	seen := map[string]bool{}
	for _, d := range t.Cycles {
		if d.Key == "" || d.Key == All || seen[d.Key] {
			return Table{}, fmt.Errorf("perf cycles: キー %q が空・予約語・重複のどれか", d.Key)
		}
		seen[d.Key] = true
		if _, err := day(d.Since); err != nil {
			return Table{}, fmt.Errorf("perf cycles: %s の since %q: %w", d.Key, d.Since, err)
		}
	}
	if last := t.Cycles[len(t.Cycles)-1]; t.Current != last.Key {
		return Table{}, fmt.Errorf("perf cycles: current %q が最後の行 %q と違う", t.Current, last.Key)
	}
	return t, nil
}

func day(s string) (time.Time, error) { return time.ParseInLocation("2006-01-02", s, clock.JST) }

// mustDay は parse で検査済みの日付を時刻に直す(Build / BuildLive は検査済みの表しか受けない)。
func mustDay(s string) time.Time {
	t, err := day(s)
	if err != nil {
		panic(fmt.Sprintf("perf cycles: 検査していない日付 %q: %v", s, err))
	}
	return t
}

// Build は research トラックの期間一覧を組む。sources は DB 名 → 台帳
// (research 自身の DB は含めなくてよい — その期間は Report nil = 既定の台帳を期間で切る)。
//
// 🛑 **台帳を開けなかった期間は出さない**(in-memory 構成 / DB 欠落)。0 件の画面は
// 「その期間は取引ゼロ」と読まれる。
// 🛑 期間の上限は「次の期間が**同じ DB** のときだけ」その開始日で切る。DB が分かれて
// いれば DB 境界が期間の境界になる。
func Build(defs []Def, sources map[string]repository.TradeSource, researchDB string,
	opts func(port.TradeStrategyResolver) []query.ForwardReportOption) []handler.PerformanceCycle {
	var out []handler.PerformanceCycle
	var merged []repository.TradeSource
	seen := map[string]bool{}
	for i, d := range defs {
		c := handler.PerformanceCycle{Key: d.Key, Label: d.Label, Since: mustDay(d.Since)}
		if i+1 < len(defs) && defs[i+1].DB == d.DB {
			c.Until = mustDay(defs[i+1].Since)
		}
		src, ok := sources[d.DB]
		switch {
		case d.DB == researchDB:
			// 既定の台帳(Report nil)。
		case ok:
			c.Report = query.NewBuildForwardReport(src.Trades, opts(src.Strategies)...)
		default:
			continue
		}
		out = append(out, c)
		if ok && !seen[d.DB] {
			seen[d.DB] = true
			merged = append(merged, src)
		}
	}
	all := handler.PerformanceCycle{Key: All, Label: "全期間 (全台帳合算)"}
	if len(merged) > 1 {
		m := repository.NewMergedTrades(merged...)
		all.Report = query.NewBuildForwardReport(m, opts(m)...)
	} else {
		all.Label = "全期間 (この台帳)"
	}
	return append(out, all)
}

// BuildLive は live の期間一覧。live DB は 1 本なので全部を日付で切る。
func BuildLive(defs []Def) []handler.PerformanceCycle {
	out := make([]handler.PerformanceCycle, 0, len(defs)+1)
	for i, d := range defs {
		c := handler.PerformanceCycle{Key: d.Key, Label: d.Label, Since: mustDay(d.Since)}
		if i+1 < len(defs) {
			c.Until = mustDay(defs[i+1].Since)
		}
		out = append(out, c)
	}
	return append(out, handler.PerformanceCycle{Key: All, Label: "全期間 (live 通算)"})
}

// ArchiveDSN は research の DSN から同じサーバの別 database を指す DSN を作る。
// 🛑 **セッションを読み取り専用にする**(`default_transaction_read_only=on`)。過去の期間の
// 台帳は凍結済みの証拠で、画面の集計経路から書ける状態で開かない。
func ArchiveDSN(dsn, db string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("archive DSN: 解釈できない DSN")
	}
	u.Path = "/" + db
	q := u.Query()
	q.Set("default_transaction_read_only", "on")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// OpenArchives は過去の期間の DB を読み取り専用で開く。開けない DB は warn して飛ばす
// (画面の戦績のためだけに起動を止めない)。research 自身の DB(researchDB)は開かない。
func OpenArchives(ctx context.Context, researchDSN, researchDB string, defs []Def, logger *slog.Logger) (map[string]repository.TradeSource, func()) {
	sources := map[string]repository.TradeSource{}
	var closers []func()
	for _, d := range defs {
		if d.DB == researchDB || sources[d.DB].Trades != nil || researchDSN == "" {
			continue
		}
		dsn, err := ArchiveDSN(researchDSN, d.DB)
		if err != nil {
			logger.Warn("過去の期間の台帳を開けない — その期間は戦績の選択肢に出さない", "db", d.DB, "err", err)
			continue
		}
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		pool, err := pg.Open(c, dsn)
		cancel()
		if err != nil {
			logger.Warn("過去の期間の台帳を開けない — その期間は戦績の選択肢に出さない", "db", d.DB, "err", err)
			continue
		}
		closers = append(closers, pool.Close)
		sources[d.DB] = repository.TradeSource{Trades: pg.NewTradeRepo(pool), Strategies: pg.NewPositionRepo(pool)}
	}
	return sources, func() {
		for _, f := range closers {
			f()
		}
	}
}
