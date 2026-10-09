package pg

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/port"
)

// 決済済み trade について「エントリー時にその戦略の screener が出していた score」を復元する。score は
// advisor_runs.input_json(advisory.screens[])にしか無いので、鎖は trades → positions → strategy_configs
// → advisor_runs → input_json。解決順は (1) strategy_configs.advisor_run_id があれば直参照、
// (2) 無い旧 config だけ config_id の時刻 ±scoreMatchWindow に success run が**ちょうど1件**のとき採用。
// 窓超過・曖昧・run 無し・screens に該当戦略が無い行は **map に入れない**(誤リンクはリンク無しより悪い)。
type ScoreRepo struct{ pool *pgxpool.Pool }

func NewScoreRepo(pool *pgxpool.Pool) *ScoreRepo { return &ScoreRepo{pool: pool} }

// 時刻 fallback が許す |started_at − config時刻| の上限。実測(N=72)で 2 秒以内 69 件 /
// 60 秒以内 72 件(最大ズレ 21 秒)、60 秒窓なら 100% 復元・誤リンクなしだった。
const scoreMatchWindow = 60 * time.Second

// ScoreByPositionID は **screen_snapshots を一次ソース**にし、そこに無い分だけ
// advisor_runs へ落ちる。
//
// 理由: advisor(LLM)を arm 経路から外すと advisor_runs は書かれなく
// なり、旧経路だけでは**決定論 arm のトレードが丸ごと score 復元不能**になる。
// screen_snapshots は LLM の有無に関係なく毎ラウンド
// 全候補ぶん書かれるので、そもそもこちらが正しいソース — advisor_runs には
// 「LLM に掛けた銘柄」しか無く、落選候補の score が存在しない。
//
// 併用するのは移行のためではなく**旧建玉のため**: screen_snapshots は
// migration 0008 以降にしか無い。
func (r *ScoreRepo) ScoreByPositionID(ctx context.Context, ids []int64) (map[int64]port.TradeScore, error) {
	out, err := r.scoresFromSnapshots(ctx, ids)
	if err != nil {
		return nil, err
	}
	missing := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := out[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	legacy, err := r.scoresFromAdvisorRuns(ctx, missing)
	if err != nil {
		return nil, err
	}
	for id, sc := range legacy {
		out[id] = sc
	}
	return out, nil
}

// scoresFromSnapshots は建玉した銘柄×戦略について、**建玉の直前で最後に書かれた
// ラウンド**の score を採る。同日内ならランキングは動かない(日足が凍結されるので
// 場中にスコアは変化しない)ので、どのラウンドを引いても同じ値になる — 逆に
// **日を跨いだ score は別物**なので 1 日より前は採らない(古い score を貼らない)。
func (r *ScoreRepo) scoresFromSnapshots(ctx context.Context, ids []int64) (map[int64]port.TradeScore, error) {
	out := make(map[int64]port.TradeScore, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT p.id, s.score
		FROM positions p
		JOIN strategy_configs sc ON sc.config_id = p.config_id
		JOIN LATERAL (
			SELECT ss.score
			FROM screen_snapshots ss
			WHERE ss.symbol = sc.symbol AND ss.strategy = sc.strategy_name
			  AND ss.round_at <= p.opened_at
			  AND (ss.round_at AT TIME ZONE 'Asia/Tokyo')::date = (p.opened_at AT TIME ZONE 'Asia/Tokyo')::date
			ORDER BY ss.round_at DESC
			LIMIT 1
		) s ON true
		WHERE p.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var score float64
		if err := rows.Scan(&id, &score); err != nil {
			return nil, err
		}
		// RunID は空 — この score は run ではなくラウンドの記録に由来する。
		out[id] = port.TradeScore{Score: score}
	}
	return out, rows.Err()
}

func (r *ScoreRepo) scoresFromAdvisorRuns(ctx context.Context, ids []int64) (map[int64]port.TradeScore, error) {
	out := make(map[int64]port.TradeScore, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	// 1. position → (symbol, strategy, config_id, advisor_run_id)。
	type posInfo struct {
		id       int64
		symbol   string
		strategy string
		configID string
		runID    string // "" = FK 無し → 時刻 fallback へ
	}
	rows, err := r.pool.Query(ctx, `
		SELECT p.id, sc.symbol, sc.strategy_name, sc.config_id, COALESCE(sc.advisor_run_id, '')
		FROM positions p
		JOIN strategy_configs sc ON sc.config_id = p.config_id
		WHERE p.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	infos := make([]posInfo, 0, len(ids))
	needFallback := false
	for rows.Next() {
		var pi posInfo
		if err := rows.Scan(&pi.id, &pi.symbol, &pi.strategy, &pi.configID, &pi.runID); err != nil {
			rows.Close()
			return nil, err
		}
		if pi.runID == "" {
			needFallback = true
		}
		infos = append(infos, pi)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 2. 時刻 fallback: ±scoreMatchWindow に success run がちょうど1件のときだけ採る(input_json はまだ引かない)。
	if needFallback {
		type runAt struct {
			runID     string
			startedAt time.Time
		}
		runsBySymbol := map[string][]runAt{}
		symbols := make([]string, 0, 8)
		seen := map[string]bool{}
		for _, pi := range infos {
			if pi.runID == "" && !seen[pi.symbol] {
				seen[pi.symbol] = true
				symbols = append(symbols, pi.symbol)
			}
		}
		// success 以外は config を書いた run ではあり得ない(失敗 run が最近傍を奪って曖昧化するのを防ぐ)。
		rrows, err := r.pool.Query(ctx, `
			SELECT run_id, symbol, started_at FROM advisor_runs
			WHERE symbol = ANY($1) AND status = 'success'`, symbols)
		if err != nil {
			return nil, err
		}
		for rrows.Next() {
			var id, sym string
			var at time.Time
			if err := rrows.Scan(&id, &sym, &at); err != nil {
				rrows.Close()
				return nil, err
			}
			runsBySymbol[sym] = append(runsBySymbol[sym], runAt{runID: id, startedAt: at})
		}
		rrows.Close()
		if err := rrows.Err(); err != nil {
			return nil, err
		}
		for i, pi := range infos {
			if pi.runID != "" {
				continue
			}
			armedAt, ok := parseConfigIDTime(pi.configID)
			if !ok {
				continue // sentinel / 手動 config — 復元不能のまま
			}
			// ちょうど1件のときだけ採用。2件以上は曖昧 = 復元不能(誤リンクはリンク無しより悪い)。
			match, inWindow := "", 0
			for _, ra := range runsBySymbol[pi.symbol] {
				gap := ra.startedAt.Sub(armedAt)
				if gap < 0 {
					gap = -gap
				}
				if gap <= scoreMatchWindow {
					inWindow++
					match = ra.runID
				}
			}
			if inWindow == 1 {
				infos[i].runID = match
			} // それ以外は "" のまま = 復元不能
		}
	}

	// 3. 必要な run の input_json だけを引き、advisory.screens[] から score を抜く。
	runIDs := make([]string, 0, len(infos))
	seenRun := map[string]bool{}
	for _, pi := range infos {
		if pi.runID != "" && !seenRun[pi.runID] {
			seenRun[pi.runID] = true
			runIDs = append(runIDs, pi.runID)
		}
	}
	if len(runIDs) == 0 {
		return out, nil
	}
	scoreByRunStrategy := map[string]map[string]float64{} // run_id → strategy → score
	jrows, err := r.pool.Query(ctx, `
		SELECT run_id, input_json FROM advisor_runs WHERE run_id = ANY($1)`, runIDs)
	if err != nil {
		return nil, err
	}
	for jrows.Next() {
		var runID, inputJSON string
		if err := jrows.Scan(&runID, &inputJSON); err != nil {
			jrows.Close()
			return nil, err
		}
		scoreByRunStrategy[runID] = screenScores(inputJSON)
	}
	jrows.Close()
	if err := jrows.Err(); err != nil {
		return nil, err
	}

	for _, pi := range infos {
		if pi.runID == "" {
			continue
		}
		if s, ok := scoreByRunStrategy[pi.runID][pi.strategy]; ok {
			out[pi.id] = port.TradeScore{Score: s, RunID: pi.runID}
		}
	}
	return out, nil
}

// JST に DST は無いので固定オフセットで正しい(行ごとの LoadLocation を避ける)。
var scoreJST = clock.JST

// "20260730-143130-3436" → 2026-07-30 14:31:30 JST。形式外(sentinel / 手動 config)は ok=false。
func parseConfigIDTime(configID string) (time.Time, bool) {
	if len(configID) < 15 {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("20060102-150405", configID[:15], scoreJST)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// 壊れた JSON は空 map = 復元不能として数える(黙って 0 点にしない)。
func screenScores(inputJSON string) map[string]float64 {
	var doc struct {
		Advisory struct {
			Screens []struct {
				Strategy string  `json:"strategy"`
				Score    float64 `json:"score"`
			} `json:"screens"`
		} `json:"advisory"`
	}
	if err := json.Unmarshal([]byte(inputJSON), &doc); err != nil {
		return map[string]float64{}
	}
	out := make(map[string]float64, len(doc.Advisory.Screens))
	for _, s := range doc.Advisory.Screens {
		out[s.Strategy] = s.Score
	}
	return out
}

var _ port.TradeScoreResolver = (*ScoreRepo)(nil)
