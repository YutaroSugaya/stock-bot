# Backtest — 決定論 replay / candle 入力 / GROSS vs NET / edge-judge

ヒストリカル candle を**本番と同じ戦略・risk gate・出口ロジック**に通し、株コスト床(tick spread +
slippage + fee + carry)を引いて **gross と net の両方**を出す決定論ハーネス。`net - gross` がコスト床の
噛み具合そのもの。実装は
[backend/internal/backtest/](../../backend/internal/backtest/) と
[backend/cmd/backtest/main.go](../../backend/cmd/backtest/main.go) /
[backend/cmd/edge-judge/main.go](../../backend/cmd/edge-judge/main.go)。

---

## 1. 役割(1行)

> ⚠️ **データは2系統 — backtest は `backend/data_research` 専用**(調整済み日足・
> `EDGE_DATA_DIR=data_research`)。運用側 `backend/data`(立花のみ・250本・分割未調整)での
> 過去検定は**不可**(運用データは短く未調整)。
> 運用ユニバースのエッジ証拠は **forward 記録のみ**。**混ぜない**。

candle CSV を本番ロジックで replay し、**net-of-cost のエッジ証拠**(per-trade + 集計 metrics)を出す。
証拠は read-only に作られ、`edge-judge` が統計 gate で pass/reject を返す。**昇格は提示まで・live 投入は人間**。

---

## 2. やること(do)

- **決定論 replay**: 同じ入力 → 同じ出力。signal id は注入カウンタ(`bt-1`, `bt-2`, …、
  [engine.go](../../backend/internal/backtest/engine.go) の `Replay`)。`rand` も `time.Now()` も使わない
  ([domain.md](../architecture/layers/domain.md) の純粋性規約)。
- candle 入力は **semicolon CSV**(`TimestampJST;O;H;L;C;V`)を
  [candlecsv.Load](../../backend/internal/adapter/candlecsv/candlecsv.go) で読む。JST 時刻を UTC `OpenTime` に正規化。
  日足は `DateJST` だけでも可(`iv >= 24h`)。
- **GROSS は raw price 由来**(entry bar の close vs raw touch level)で**コストモデル非依存**にする。
  **NET は slippage 込みの fill** から fee を引き carry を足す。`net - gross` がコスト床全体
  ([engine.go](../../backend/internal/backtest/engine.go) の `closeTrade`)。
- carry は**本番と同じモデル**(`position.CarryCalc`: 約定金額 × 年率 × 受渡日の両端入れ)。料率・受渡日数・休場カレンダーは
  `-hard-limits`、執行区分は `-bot-config` の `ExecKindFor` から読み、建玉時に Position へ凍結する(信用の多日建玉だけに付く)。
- コスト床 = **spread + slippage + fee + carry**
  ([CostModel](../../backend/internal/backtest/cost.go))。per-leg adverse = `slippage + spread/2`(tick 単位、
  `market.TickSizeOf` で円換算 → `RoundToTickOf`)。fee は片側 % × 2 leg。carry は上の本番モデル。
- **本番と同じ risk gate を replay 内で発火**: `risk.EvaluateSignal` に replay の `AccountSnapshot`
  (consec loss / day loss / open 数 / session 内外 / 引け前 cutoff)を渡す(`snapshot`)。
  `-max-daily-loss-jpy` / `-max-consecutive-losses` で cap を有効化。
- **TP/SL 同一 bar 衝突は決定論ポリシで解決**: 既定 `pessimistic`(SL 優先 = 悲観)、`optimistic`(TP 優先)、
  `skip`(close しない)。衝突した bar 数は `AmbiguousBars` に記録([engine.go](../../backend/internal/backtest/engine.go) の `resolveExit`)。
- **look-ahead 防止**: 出口は「**厳密に過去の bar**で建てたポジ」だけを当 bar で評価する(`op.openedBar >= i` はスキップ)。
- metrics は **non-finite PF/RR を JSON で `null`** にする(`*float64`、[metrics.go](../../backend/internal/backtest/metrics.go) の `MarshalJSON`)。
  PF 規約: 勝ちあり負けなし = `+Inf`(→ null)、勝ちなし = `0`。
- 判定は**2段構え**: [cmd/edge-eval](../../backend/cmd/edge-eval/main.go)(**コード強制 screen** —
  ¥1M notional 正規化・レジーム分割・day-block bootstrap・holdout 予約(`-burn-holdout` まで読まない)・
  検定数の予算: `-test-count T` が Šidák 補正で CI を広げ、`-max-test-count`(既定 12)を超える T は holdout 以外で reject)→ [edge-judge](../../backend/cmd/edge-judge/main.go) →
  [judge.Judge](../../backend/internal/backtest/judge/judge.go)(統計 gate)。
  入力は per-trade net JPY 列(+ optional OOS / bench)。verdict は最大でも `promote_candidate`(= 提示)。
- DB 書込が要る経路は `SafeBacktestDSN`
  ([backtestdsn_guard.go](../../backend/internal/adapter/repository/backtestdsn_guard.go))で守る。
  ただし `cmd/backtest` 自体は read-only で live candle に触れない([CLAUDE.md](../../CLAUDE.md) の DB 安全規約)。

---

## 3. やらないこと(don't)

- **gross で昇格判断**しない。gross が良くても net で消えるのが全敗因([EDGE_METHODOLOGY.md](../runtime/EDGE_METHODOLOGY.md) §1)。
- slippage 0 / spread 0 で回して「素晴らしい PF」と判断しない(現実のコスト床を反映していない)。
- **同一 bar 衝突を TP 優先**を既定にしない(実 broker では SL 先発火の方が悲観的に正しい)。`optimistic` は感度分析専用。
- `cmd/backtest` の strategy に**本番未登録の戦略を回した結果**を、そのまま live 投入根拠にしない(§7 参照)。
- `edge-judge` の `promote_candidate` を**自動で live に流さない**。ロット(株数)を上げるのは人間判断のみ
  ([CLAUDE.md](../../CLAUDE.md) のエッジ規律)。
- live DB / live candle に書き込まない。`SafeBacktestDSN` を回避しない([CLAUDE.md](../../CLAUDE.md))。
- LLM を replay/判定の自動経路に入れない(advisor は config 生成 / go-no-go gate のみ)。

---

## 4. 実行コマンド

```bash
# 1) backtest: candle CSV を本番ロジックで replay し net metrics を出す
go run ./cmd/backtest \
    -csv data_research/7203_daily.csv \
    -symbol 7203 \
    -interval 1d \
    -strategy bnf_reversion \
    -tp 100 -sl 50

#   主なフラグ(cmd/backtest/main.go):
#   -interval 1m|5m|1h|1d   -holding intraday|multiday
#   -qty <株数> / -max-hold <分>(0=なし)
#   -daily-csv <日足CSV>   (sub-daily replay に日足文脈を注入; dual-timeframe 戦略
#                           e.g. bnf_intraday_reversion)
#   -ratchet-arm / -ratchet-giveback  (0=OFF; >0 で trailing 出口)
#   -fee-rate / -slippage-ticks / -spread-ticks   (コスト床)
#   -conflict pessimistic|optimistic|skip          (TP&SL 同一 bar)
#   -max-daily-loss-jpy / -max-consecutive-losses  (risk cap; 0=無効)
#   -bnf-dev / -bnf-vol / -bnf-stop-atr            (BNF tuning override)
#   -json  (full Result JSON; 既定は summary のみ)

# 2) edge-eval: 自己欺瞞スクリーンをコードで強制(¥1M 正規化・レジーム分割・
#    day-block bootstrap・holdout 予約・検定数の予算 -test-count / -max-test-count)。入力は cmd/backtest -json の Result 群
go run ./cmd/edge-eval -glob '/path/to/results/*.json' -test-count <T>

# 3) edge-judge: per-trade net 列(net.json)を統計 gate にかける
go run ./cmd/edge-judge -in net.json
```

`net.json` のスキーマ(`cmd/edge-judge/main.go` の `input`):
`{ "track": "A"|"B", "net_per_1m_jpy": [...], "net_pnl_jpy": [...], "oos_net_pnl_jpy": [...],
"bench_net_pnl_jpy": [...], "cost_floor_jpy": <float>, "floor_multiple": <float>, "universe_n": <int> }`。
`net_per_1m_jpy`(¥1M notional 正規化系列)が**在れば必ずそちらを判定に使う** — 生の円は建玉金額が
銘柄で 20 倍以上ばらつき「値がさ株を掴んだ戦略」が強く見えるため(`net_pnl_jpy` は後方互換)。
net 列は `Result.Trades[].NetJPY` から組み立てる(専用のヘルパは置いていない)。

---

## 5. テスト方法

- `make check-backend`(= `go test -race ./...` + `go vet ./...` + `go build ./...`)が緑であること
  ([PR_CHECKLIST.md](../architecture/PR_CHECKLIST.md))。新規挙動は strict TDD(Red → Green → Refactor)。
- replay の決定論: 同一 CSV を 2 回 `Replay` して `Result` が一致することをテストで固定する。
- コスト床の可視化: 既知の単一トレードで `gross - net` を手計算と突き合わせ
  (`adverseTicks`・`FeeJPY`・`CarryJPY` を [cost.go](../../backend/internal/backtest/cost.go) で逐条確認)。
- judge の gate 順序: `judge.Judge` は**最初に一致した gate が勝つ**(N 床 → CI 上限 < 0 → universe < 3 →
  コスト床倍率 → OOS 同符号 → bench 超過 → 昇格)。境界値ごとに verdict を固定する。

---

## 6. 既存実装の代表例

- replay 本体 / same-bar / look-ahead 防止: [engine.go](../../backend/internal/backtest/engine.go)
  (`Replay`, `resolveExit`, `closeTrade`, `snapshot`)。
- コスト床: [cost.go](../../backend/internal/backtest/cost.go)(`CostModel`, `adverseTicks`, `EntryFill`,
  `ExitFill`, `FeeJPY`, `CarryJPY`)。carry の計算は [position/carry.go](../../backend/internal/domain/position/carry.go)、
  組み立ては `config.HardLimits.CarryCalc`(bot と共有)。
- Trade / Metrics / Result 型: [result.go](../../backend/internal/backtest/result.go)。
- metrics 計算 + non-finite の `*float64` JSON: [metrics.go](../../backend/internal/backtest/metrics.go)。
- CSV ローダ: [candlecsv.go](../../backend/internal/adapter/candlecsv/candlecsv.go)(bot・fetch-daily・日次総評と共有)。
- 統計 gate: [judge/judge.go](../../backend/internal/backtest/judge/judge.go)(`Verdict`, `JudgeInput`, `Judge`)。
- CLI: [cmd/backtest/main.go](../../backend/cmd/backtest/main.go) / [cmd/edge-judge/main.go](../../backend/cmd/edge-judge/main.go)。

### 本番登録 戦略 vs backtest 専用

- 戦略の一覧は戦略カタログ(`internal/domain/strategy/catalog.go`)が正。`Menu` 列の戦略が本番の engine に
  載り(`no_trade` は `NewEngine` が常時追加)、`cmd/backtest -strategy` はカタログの全行(`strategy.ByName`)を引ける。
  **登録だけでは何も建たない** — active config が指名した戦略だけが動く。paper で回すアームは
  `advisor_v2.entries`、live で起動してよい戦略は `hard_limits.live_allowed_strategies` が決める。
- `Menu=false` の行(`ma_cross` / `rsi2_reversion` / `bollinger_reversion` / `gap_reversion` / `bnf_euphoria_short`・v1 など)は
  **検定候補テンプレ**として replay できるだけ。検定で未証明の戦略を live に載せない。

### 出口ロジックの依存方向(移設済み)

backtest は**本番の出口ロジックを再利用**する。出口判断は **`domain/position` に移設済み**で、
`resolveExit` は `position.EvaluateExit` を呼ぶ([engine.go](../../backend/internal/backtest/engine.go))。
backtest は **usecase ではなく domain にだけ依存**し、replay は「本番と同じ純粋ロジック」を保ったまま
usecase 層から切り離されている
([domain.md](../architecture/layers/domain.md) / [usecase.md](../architecture/layers/usecase.md))。

---

## 7. アンチパターン

- candle が不足したまま replay する(期間前半が flat 集計になり、metrics が嘘になる)。
- `-slippage-ticks 0 -spread-ticks 0`(= コスト床ゼロ)で出た PF を実力と誤認する。
- `pessimistic` 以外を既定にして「TP が先に当たる」前提の楽観 metrics を作る。
- jq で per-trade net を手組みして judge を回避する。ウィンドウ再スライス・銘柄再プールし放題になり
  蜃気楼を本物と誤認する([EDGE_METHODOLOGY.md](../runtime/EDGE_METHODOLOGY.md) §0)。判定は `cmd/edge-judge` 経由のみ。
- `promote_candidate` をもって株数を上げる(昇格は提示・live は人間判断)。
- ratchet の intrabar 楽観(`resolveExit` が intrabar 高値を peak に入れる)を理解せず wide-stop 系を昇格させる
  ([EDGE_METHODOLOGY.md](../runtime/EDGE_METHODOLOGY.md) §4、要修正項目)。
- short-leg / multiday の合格をそのまま昇格(貸株料・信用金利が未計上のため楽観バイアス。
  モデル化まで promote 禁止、[EDGE_METHODOLOGY.md](../runtime/EDGE_METHODOLOGY.md) §4)。

---

## 8. 関連 docs

- [EDGE_METHODOLOGY.md](../runtime/EDGE_METHODOLOGY.md) — エッジ規律の SoT(自己欺瞞を機械で防ぐ方法論)。
- [layers/domain.md](../architecture/layers/domain.md) — replay が検証する戦略 / risk gate / 出口ロジック。
- [layers/usecase.md](../architecture/layers/usecase.md) — 出口移設後の依存方向。
- [FAILURE_MODES.md](../architecture/FAILURE_MODES.md) — コスト床・楽観バイアスの故障モード。
- [PR_CHECKLIST.md](../architecture/PR_CHECKLIST.md) — マージゲート。
- [CLAUDE.md](../../CLAUDE.md) — エッジ規律 / DB 安全の不変条件。
