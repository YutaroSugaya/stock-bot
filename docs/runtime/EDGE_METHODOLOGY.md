# エッジ検定の規律 — `cmd/edge-eval` / `cmd/edge-judge` が機械で強制すること

> 戦略を増やすより、検査装置(検定ハーネス)を信頼できる状態にする方が先。規律が「文書」であって
> 「パイプライン」でないと、ウィンドウの再スライスや銘柄の再プールで蜃気楼を本物と誤認する。
> だから判定はコード(`backend/internal/backtest/eval` / `judge` / `pairdiff`)が出し、手組みの集計で verdict を作らない。
> 実装の構造は [BACKTEST.md](../workflows/BACKTEST.md)、データは [EDGE_TESTING_DATA.md](EDGE_TESTING_DATA.md)。

## 0. 判定の要: day-block bootstrap と実効 N

- **CI は day-block bootstrap**(`judge.BootstrapMeanCIDayBlock`)。銘柄横断のトレードは市場全体の下落日にクラスタするので、
  トレードを独立にリサンプルした CI は反保守的(「95%CI」が実質それ以下)になり、`CI_lo>0` がエッジより先にクラスタした β で発火する。
  `judge.Judge` は `Days`(net 系列と同順同長の JST 日付)があれば day-block を使い、欠けた入力ではトレード単位に落ちるが
  reasons に `trade_level_bootstrap_anti_conservative` を必ず付ける。`cmd/forward-report -json` は `days` を出す。
- **実効 N は暦日数**: `DayBlocks` はその戦略がトレードした日数で、`judge.go` の `minDayBlocks` 未満なら
  `insufficient_day_blocks` で continue 固定。トレード数のゲート(`JudgeInput.Track`: intraday A / multiday B)と両方通す。
  トレードしない戦略の DayBlocks は 0 のまま増えない。

## 1. スクリーニング規約(`eval.go` の `decide()`)
1. **判定は net のみ**。gross が良くても net で消える。`cmd/backtest` は手数料・スリッページ・スプレッドに加えて
   carry(信用金利 / 貸株料)を `hard_limits.margin.*`(`buy_annual_rate_pct` / `sell_lending_annual_pct` /
   `settlement_business_days`)から本番と同じ `position.CarryCalc` で載せる(`-hard-limits` が読めなければ落ちる)。
   forward 台帳の `trades.carry_jpy` も同じ計算。
2. **net-of-cost floor**: net mean ≥ `cost_floor × -floor-multiple`。コストの床を割る候補は continue。
3. **嘘発見器(breadth)**: `universe_n < 3` は continue。
4. **¥1M notional 正規化**(価格水準差を消す): `position.Per1MNotional` = `net/(entry×qty)×1e6`。
   forward-report / edge-eval / pair-diff が同じ関数を使う。
5. **3 レジームで ≥ `-min-regimes-positive`(既定 2)が net 正**。レジーム境界は `-regime-bounds`(N 本 → N+1 区間)。
   単一レジーム合格は `single_regime_mirage_regimes_positive_below_min` で棄却。
6. **ベンチ超過**(long-β 殺し): `-benchmark` に TOPIX 連動のベンチ CSV を渡すと、各トレードの保有窓の buy&hold を
   差し引いた excess の CI_lo>0 を要求する(net の CI_lo>0 だけではただの β)。
   両 side / short の「short-leg 単独で net mean>0」は `decide()` に side 別ゲートが無く、手動確認。
7. **反証条件を検定前に宣言**し後から動かさない。緩めて通したら棄却。
8. **OOS は同符号**(`judge.go`): `OOSNetPnLJPY` があるとき OOS mean ≤ 0 か in-sample と符号が違えば reject。

## 2. 多重検定への対抗
- **パラメータは事前固定**。各 spec の暗黙ノブ(tp / sl / maxHold / SMA 長 / 出来高倍率 / 分割日)で実効検定数は
  10〜30 倍に膨らむ。グリッドの最良セルを verdict にしない(`max_hold.go` / `daily_candidates_v2.go` のコメントが理由)。
- **兄弟アームはペア差**: 入口が同一で出口だけ違う `X` / `X_trail` は、アーム単独の net を 2 群比較しない
  (同じトリガーなので独立でない)。本命の統計量は同一トリガーの (trail net − capped net) で、
  `backend/internal/backtest/pairdiff` / `cmd/pair-diff` が出す。
- **paired-negation**: ある仮説とその真逆(継続 vs フェード等)が両方 pass したら両方 overfit として棄却。
- **family stop**: ファミリー最軽量メンバーが net で死んだら、その近縁の検定を止める。
- `-test-count` は family の検定数 T で CI を Bonferroni-Šidák(α/T)に広げる。`-max-test-count` を超える T は reject になるが、
  総検定数のカウンタを作業のゲートにはしない。多重検定への対抗は ①事前固定 ②`minDayBlocks` ③forward 再確認 の 3 点。

## 3. ホールドアウト
- `-holdout-start` 以降は `cmd/edge-eval` が既定で読まない(`HoldoutReserved` に件数だけ出る)。
  in-sample スクリーンを通過した候補だけが `-burn-holdout`(素の bool フラグ + stderr 警告)で**一回だけ** burn できる。
- 覗いただけで burn 扱い。符号反転 / エッジ崩壊なら恒久棄却(再調整・再見禁止)。burn 済みの候補ファミリーは再検定不可で、
  この窓が有効なのは新候補だけ。hash 引数や永続記録は無いので、候補の識別と burn 済みの記録は人間が残す。

## 4. データ / コストの落とし穴(判定前に織り込む)
- **調整株価の配当落ち**: 調整済 OHLCV は配当を遡及調整するので、配当落ち日が偽の「下落日」になる。
  down-day / 逆張り / streak 系は raw close で down 判定するか配当日を除外する。
- **生存バイアス**: 現存銘柄だけの合格は上方バイアス。上場廃止を含む一覧で復元するまで割り引く(EDGE_TESTING_DATA B-3)。
- **ratchet の intrabar 極値**(`backtest/engine.go` の `RatchetArmJPY > 0` ブロック): バーの有利側極値を ratchet peak に入れる。
  live は tick で peak を見るので同等という立場と、反転バーで「届かない高値」が armed にする楽観という立場がある —
  未定の設計点(`engine.go` のコメント参照)。

## 5. 実弾昇格の前に forward で再確認する
in-sample と holdout を通っても、昇格の前に forward(paper)の台帳で同じ判定(`cmd/forward-report` → `cmd/edge-judge`)を
通す。ロットを上げてよいのはそのあとだけ(CLAUDE.md §4)。
