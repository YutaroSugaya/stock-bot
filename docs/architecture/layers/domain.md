# Layer: Domain

## 役割 (1 行)

純粋 Go のビジネスモデル。Entity / Value Object / Domain Service を持ち、外部 I/O 一切なし。

---

## やること (do)

- ビジネスルールを **入力 → 出力** の純粋関数で表現する
- Entity と Value Object を定義する(`Position` / `Order` / `Signal` / `Ticker` / `Candle`)
- Domain Service として **同じ入力に対し同じ出力を返す** 計算を提供する(`TPSLPricesFromJPY` / `Engine.Evaluate` / `EvaluateSignal`)
- 時刻は `domain/clock` の `Clock`(`type Clock func() time.Time`)を注入で受ける。production は `clock.System()`(= `time.Now`)、テストは `clock.Fixed(t)`
- signal id は `strategy.SignalIDFn`(`type SignalIDFn func() string`)を `NewEngine` に注入する。backtest は決定的な generator を渡す([backtest engine](../../../backend/internal/backtest/engine.go) が `bt-<seq>` を注入)
- 単体テストは fake / mock を使わず純粋単体で書く(table-driven)

---

## やらないこと (don't)

- `pgx` / `net/http` / `log` / `log/slog` / `os` を import する
- `time.Now()` を直接呼ぶ(`clock.Clock` を注入、または caller が `now time.Time` を渡す。`EvalInput.Now` / `AccountSnapshot.Now` がそれ)
- `math/rand` / `crypto/rand` を呼ぶ(決定的 ID は `strategy.SignalIDFn` を caller が注入する)
- goroutine / channel / mutex を持つ(下記 §例外 の 2 型を除く。= 状態を持たない純粋関数の集合に留める)
- Repository interface(`port.PositionRepository` 等)を import する

### 例外: `domain/market` の stateful buffer(唯一の許容)

[`domain/market/aggregator.go`](../../../backend/internal/domain/market/aggregator.go) の `Aggregator` と
[`domain/market/candle.go`](../../../backend/internal/domain/market/candle.go) の `RingBuffer` の **2 型に限り
mutex(`sync.Mutex` / `sync.RWMutex`)を許容する**。これらは「複数 goroutine(price loop / minute loop /
handler)から共有される rolling window 時系列バッファ」であり、本質的に stateful 共有データ。
`app` 層に移すと candle resample / window aggregate の domain 計算を `app` が抱えることになり domain 純度が
さらに崩れる、かつ全 goroutine を同期する mutex は結局必要になるため、置き直しは責務分離にならない。

判断: **この 2 型以外の domain サブパッケージ(`position` / `order` / `risk` / `strategy` / `session` / `ta` /
`clock`)は mutex 禁止**。新規 stateful buffer を足す場合は、まず本ファイルに新例外として明記してから
実装する(= 暗黙の蓄積を防ぐ)。grep で `sync.` が当たるのはこの 2 ファイルのみであること。

---

## 命名 / 配置

| 種別 | 場所 | ファイル名 | 例 |
|---|---|---|---|
| Aggregate ディレクトリ | `domain/<aggregate>/` | — | `domain/position/` |
| Entity | `domain/<aggregate>/` | `<entity>.go` | `domain/position/position.go` |
| Value Object | `domain/<aggregate>/` | `<vo>.go` | `domain/market/tick.go`(呼値 tick 写像) |
| Domain Service | `domain/<aggregate>/` | `<service>.go` | `domain/position/price.go` |

### Aggregate 一覧

- [domain/clock/](../../../backend/internal/domain/clock/) — `Clock` 注入(`System()` / `Fixed()`)。`time.Now()` 直書き禁止の代替
- [domain/market/](../../../backend/internal/domain/market/) — `Ticker` / `Candle` / `RingBuffer` / `Aggregator` / `MarketSummary`。呼値は [tick.go](../../../backend/internal/domain/market/tick.go)(`TickSize` / `IsTickAlignedOf`)+ [tick_fine.go](../../../backend/internal/domain/market/tick_fine.go)(細かい呼値の銘柄・`TickSizeOf` / `RoundToTickOf`)。日足の分割吸収は [split_guard.go](../../../backend/internal/domain/market/split_guard.go)(`ChainLinkSplits`)。
  `MarketSummary.Advisory`(`json.RawMessage`・omitempty)は AI advisor 入力用の opaque パケット枠で、決定論戦略は読まない
- [domain/order/](../../../backend/internal/domain/order/) — `Side`(`Opposite()`) / `OrderType`(`IFDOCO` 含む) / `HoldingMode` / `ExecKind` / `PlaceOrderRequest` / `Execution` / `AccountMargin`(`MarginRatio` = 維持率)
- [domain/position/](../../../backend/internal/domain/position/) — `Position`(凍結出口幾何 + ratchet runtime 状態 / `MaxHoldUntil` / `UnrealizedJPY`)/ `TPSLPricesFromJPY` / `MaxExtendMinutes`(MaxHold 延長の 1 回あたり上限。延長の command と候補の query が同じ値を読む)([STATE_MACHINE.md](../../runtime/STATE_MACHINE.md))
- [domain/risk/](../../../backend/internal/domain/risk/) — risk gate(`EvaluateSignal` 全ゲート + `EvaluateHardSafety` 上書き不可サブセット)
- [domain/session/](../../../backend/internal/domain/session/) — 東証 calendar(`InTradingHours` / `IsAfterEntryCutoff` / `IsNearClose` / `MinutesUntilForceFlat`)。venue 日付の helper は `DayStart`(venue-midnight)/ `PrevTradingDay`(直前の営業日・見つからなければ ok=false)の 2 つに集約する
- [domain/ta/](../../../backend/internal/domain/ta/) — `SMA` / `ATR` / `Donchian` / `Slope`
- [domain/strategy/](../../../backend/internal/domain/strategy/) — `Engine` registry + `Evaluate`。`Signal` を返す純粋関数群

---

## 主要型と不変条件(コードから抜粋)

### `order`(売買種別・口座種別)

- `Side`(`"BUY"` / `"SELL"`)。`Opposite()` は不正な side に対し既定で `BUY` を返さず **空文字 `""` を返す**(誤入力を下流の `Valid()` で reject させるため)。
- `OrderType`: `MARKET` / `LIMIT` / `STOP` / `OCO` / `IFDOCO`(`IFDOCO` は broker 依存の単発 entry+TP+SL 形で、**現 adapter では未使用** — 立花は entry 約定後に `PlaceSettleOCO` で守りを置く二段構え)。
- `HoldingMode`: `intraday`(14:50 に引け前フラット化)/ `multiday`(建越し)。`Valid()` で検証。
- `ExecKind`: `cash`(現物)/ `margin_system`(制度信用 6 ヶ月・立花で通る信用区分)/ `margin_general`(一般信用・口座の種別によっては拒否される)/ `margin_oneday`(一日信用・立花の API に区分が無い)。`IsMargin()` で信用かどうかを 1 か所で判定する。
- `AccountMargin.MarginRatio` = 維持率、`AvailableJPY` = 取引可能額(risk チェック入力)。
- `Order.HasStopLeg` = **その注文が逆指値(SL)脚を持つか**。「決済側に注文がある」は「守られている」ではない(利確指値だけの建玉は下方向に裸)。
  守りを数える述語は side と数量に加えて**必ずこれを要求する**。broker 中立な真偽値にして、判別子の綴り(立花なら `sOrderGyakusasiOrderType`)を domain に持ち込まない。
  **取れないとき(`""`)は false = fail-close**。立花の OCO は注文番号 1 本に TP と SL が同居するので、注文一覧の見た目では「TP だけの指値」と区別できず、これが「TP/SL は broker 側」を検査する唯一の手段になる。

### `position`(凍結出口 + ratchet runtime)

- 出口幾何は **円/株の幅**で持つ(`TakeProfitJPY` / `StopLossJPY` / `MaxHoldMinutes` / ratchet 各種)。これに `TakeProfitPrice` / `StopLossPrice`(**建玉時に確定した絶対価格**・呼値グリッド丸め済み。broker OCO へそのまま送り `EvaluateExit` も直接比べる。0 = 未設定でその脚は評価しない)+ `HoldingMode` + `ExecKind` + `TickSizeAtEntry` を合わせ **entry 時に凍結**し以後不変(config 凍結)。
- runtime 状態(`PeakUnrealizedJPY` / `TroughUnrealizedJPY` / `RatchetArmed`)は `ManageOpenPositions.OnTick` が更新する(domain 自体は値型)。`RatchetFloorAtArm` は runtime 状態ではなく entry 時に凍結する版スタンプで、トレールの床が効く建玉かを表す。
  peak(MFE)/ trough(MAE)は ratchet を持たない建玉でも更新する(事後の反実仮想のための記録で、どの決済規則も trough を読まない)。`EvaluateExit` は極値が実際に動いたときだけ `ExcursionChanged` を立てる。
- `MaxHoldUntil()` は `OpenedAt + MaxHoldMinutes`(未設定なら zero time)。`UnrealizedJPY(price)` は side 符号付きの含み損益(**円/株**・呼値は関与しない)。

### `position.TPSLPricesFromJPY`(Domain Service)

`(symbol, side, entry, tpJPY, slJPY) → (tp, sl)`。**円/株の幅**(建値からの距離)を絶対 broker 価格に変換する。BUY は TP 上 / SL 下、SELL は逆。幅 0 は「不在」を意味し価格 0 を返す。最後に `market.RoundToTickOf(symbol, …)` で**その銘柄の**呼値グリッドに丸める(呼値が登場するのはここだけ — 出口幾何は円で持ち、発注できる値段に乗せる最後の一手でだけ丸める)。

### `market` 呼値 tick([tick.go](../../../backend/internal/domain/market/tick.go) / [tick_fine.go](../../../backend/internal/domain/market/tick_fine.go))

- `TickSize(price)` は通常銘柄の呼値刻み(例: `<=3000` → 1、`<=5000` → 5、`<=30000` → 10 …)。細かい刻みのテーブルは `fineTickSize`。
- 銘柄別は `TickSizeOf(symbol, price)` / `RoundToTickOf` / `IsTickAlignedOf` を使う(表に無い・空文字 symbol は粗いテーブルへ倒す fail-safe — 粗いグリッドの値段は細かいグリッドにも必ず乗る)。broker に送る TP/SL は必ずこれを通す(risk gate が非整合の TP/SL を reject)。丸めた結果を掛け算で作らない(1 円未満の刻みは逆数で割る)。
- **どちらのテーブルに乗るかは手で並べない。** 判定は純粋関数 [`InferTickRegime`](../../../backend/internal/domain/market/tick_regime.go)(`TickRegime` = `Coarse` / `Fine`)で、**観測された約定値が根拠**: 粗い刻みに乗らない価格が 1 本でもあれば `Fine`、証拠が無ければ `Coarse`。
  非対称は意図的で、誤りの向きを固定する — `Fine`→`Coarse` の誤りは執行コストの過大計上で済むが、`Coarse`→`Fine` の誤りは刻み違反の TP/SL を作って broker に弾かれる。
  「粗い側へ倒せば必ず板に乗る」が成り立つのは粗い刻みが常に細かい刻みの整数倍だから(`TestCoarseTickIsAlwaysAMultipleOfFineTick` が固定する)。
- **バー列から判定するときは `InferTickRegimeFromBars` を使う**。運用側の日足は `ChainLinkSplits` 適用後の値なので、分割前のバーは raw/N になって粗いグリッドから外れる。調整値は約定値ではないので、2 つの独立した反証で落とす:
  1. **証拠の鮮度** — 最後に外れたバーより後ろが `staleTickEvidenceBars` 本以上クリーンなら棄却(本物の `Fine` はほぼ毎日外れた値を出し、調整の痕跡は権利落ち日で止まる)。
  2. **比の掛け戻し**(`splitAdjustmentExplains`)— 外れた値が継ぎ目より前だけに出ていて(末尾 `splitArtifactCleanTailBars` 本以上クリーン)、単一の `simpleSplitRatios` を掛け戻すと全て粗いグリッドへ戻るなら調整値。
  素の `InferTickRegime` は順序非依存の純粋関数のまま残す(`TestInferTickRegimeIsOrderIndependent`)。時系列を持つ呼び手だけが `FromBars` を使う。
- 銘柄表 `fineTickSymbols` は [tick_fine_gen.go](../../../backend/internal/domain/market/tick_fine_gen.go) の**生成物**(`cmd/tick-table` が `hard_limits.allowed_symbols` 全件の日足へ `InferTickRegimeFromBars` を適用して出力。domain は I/O を持たないので走査は cmd 側)。手編集しない。導出日は `FineTickDerivedOn`(朝ルーチンが年次の鮮度を見る)。
- プールとの整合は `internal/config` の `TestFineTickTableMatchesAllowedSymbols` が、生成物に焼いた `FineTickPoolDigest` / `FineTickPoolSize` と `allowed_symbols` の指紋を照合して縛る。
- **プールを減らすだけの再生成は、前回と同じ日足スナップショットで回す**(`-data` に以前の日足スナップショットを展開して渡し、由来の行はスナップショット名に書き換える)。最新の日足で回すと、分割直後で鮮度判定がまだ効かない銘柄の誤昇格がプール変更と同じ diff に混ざる。

### risk gate(順序付きゲート)

`EvaluateSignal(sig, cfg, snap, summary) → Decision{Allowed, Reason, QtyMultiplier}`。非 entry は無条件 allow。順序は **hard blow-up brake → soft / operator-overridable guard**。
構造ゲートは `structuralGates`(名前つきの順序付き表)を歩き、順序は `TestStructuralGateOrderIsPinned` が固定する:

1. `hard_brakes`(上書き不可): `emergency_stop` → `risk_state_unavailable`(repo 読取失敗は下流 cap が 0 相手に評価されるので fail-close)→ `outside_session_hours` → `end_of_day_flat_required` → `daily_loss` → `account_daily_loss`。
2. `exec_kind_coherence`(`ExecMarginOneday` × `HoldingMultiday` を reject)→ `tick_alignment`(TP/SL の呼値整合)→ `risk_per_trade`(1 本あたりの計画損失)。
3. `cooldown` → `consecutive_losses`(`ConsecutiveLossGuardsDisabled` で無効化可)→ `position_count_caps`(per-symbol | per (symbol, strategy) / account)→ `symbol_budget`(監視銘柄の予算)→ **`nanpin_block`** → `intraday_once_per_day` → `window_throttles` → `direction` → `spread`(薄商い)。
4. 担保(`EvaluateCollateral`): 委託保証金充足 → 最低委託保証金(`collateral_below_minimum`)→ レバレッジ上限(`gross_notional_cap`)。
5. 連敗 `>= consecutiveLossHalveThreshold` で `QtyMultiplier = consecutiveLossHalveMultiplier`(株数を減らす)。

貸借銘柄(`EvaluateShortLoanable`)と値幅制限(`EvaluatePriceLimit`)は `EvaluateSignal` の外の関数で、usecase の `TradingCycle` が順に呼ぶ([usecase.md](usecase.md))。

#### 2 相構成 — 担保ゲートを最後に置く理由

| 関数 | 見るもの | broker 照会 |
|---|---|---|
| `EvaluateStructural(sig, cfg, snap, summary)` | 上の 1〜3(我々の帳簿と時計だけ) | **不要** |
| `EvaluateCollateral(sig, snap)` | 上の 4(`snap` の broker 由来フィールド) | 必要 |
| `EvaluateSignal(...)` | 両方の合成 + 5 | 必要 |

usecase は `EvaluateStructural` を**先に**回し、通ったエントリーについてだけ口座照会を払ってから `EvaluateCollateral` を回す。
枠オーバーやナンピン禁止で捨てるエントリーに立花への照会を払わないため([TACHIBANA_API_NOTES.md §1.5](../../runtime/TACHIBANA_API_NOTES.md))。

- **`EvaluateStructural` は broker 由来フィールド(`MarginStatusUnknown` / `AvailableToTradeJPY` / `CollateralJPY`)を読まない**。読んだ瞬間、照会前に呼べるというこの関数の存在理由が消える。
- **未照会は fail-close**。`AccountSnapshot.MarginStatusUnknown` が「まだ訊いていない」も兼ね、照会し忘れた経路は `margin_status_unavailable` で reject される。
- 合成版 `EvaluateSignal` の振る舞いは分解前と同一でなければならない(`gate_test.go` が回帰網)。

#### 個々のゲートの契約

- **ナンピン block は hard gate**: 同 symbol 同 side(external 含む `OpenBuyInclExternal` / `OpenSellInclExternal`)が `MaxConcurrent`(既定 1)以上で reject。建玉数 cap とは独立、override 不可。
  - キーは mode で変わる。`cfg.Mode != live` では `OpenBuySameStrategyInclExternal` / `OpenSellSameStrategyInclExternal` = **(銘柄, 側, 戦略)**。`mode: live_config` は (銘柄, 側)。
  - **戦略が判らない建玉は全戦略をブロックする**(fail-close)。external と `strategy_name` が空の旧建玉は per-strategy counter の全てに数える。
  - 建玉数 cap も同じキー(`OpenPositionsSameStrategy`)。cap は側を見ないので、同一戦略が同一銘柄で買いと売りを同時に持つのを止めているのはそちら。
- **監視銘柄の予算**(`symbol_budget`): 入口ごとの枠(`EntryArmMaxOpenSymbols`)を先に、口座全体の枠(`AccountMaxOpenSymbols`)を後に見る。どちらも**新しい銘柄を開くときだけ**効く(既に持っている銘柄への建ては監視集合を増やさない)。
- **同日 1 回転**(`intraday_once_per_day`): 日中保有は (銘柄, 戦略) ごとにその営業日 1 回だけ建てる(`AccountSnapshot.EntriesTodaySameStrategy`)。
- **1 本あたりの計画損失**(`risk_per_trade`・`AccountSnapshot.MaxRiskPerTradeJPY`): 建値から SL までの距離 × 株数が上限を超える entry を `risk_per_trade <実額> > cap <上限>` で reject する。**0 = 無効**(紙の標本をこの理由で censoring しない)。
  - SL を狭める設定ではない(出口は戦略が決めたまま、重すぎる候補を建てないだけ)。損失の上限でもない(ギャップとストップ安は逆指値をすり抜ける)。
  - 構造側に置く(判定材料は Signal だけ)。SL の無いシグナルはここで拾わない(理由を混ぜない)。
  - 境界は **`>` で切る**(上限ちょうどは通す)。`app.Selector` の先出しフィルタと同じ境界に保つ。
- **レバレッジ上限**(`risk.WithinGrossNotionalCap`)は委託保証金充足とは別のゲート。委託保証金充足は「broker が建てさせてくれるか」、こちらは「我々が自分に課す上限」(`bot_config.risk.max_gross_notional_ratio`)で、建玉合計 ≤ 委託保証金 × 倍率。
  `ratio: 0` は無効(research は対象外)。保証金が読めない(0)ときは拒否(fail-close)。
- **最低委託保証金**(`AccountSnapshot.MinCollateralJPY` ← hard_limits `margin.min_collateral_jpy`): 保証金 `CollateralJPY` が下限を割ったら `collateral_below_minimum have X < min Y`。
  レバレッジ上限と同じ `CollateralJPY` で測る(2 つのゲートが別の「保証金」を見ない)。`0` = 無効。未照会は手前の `margin_status_unavailable` で落ちる。
- **貸借銘柄(SELL のみ)**: `risk.EvaluateShortLoanable(sig, LoanableSymbols)`。`Configured=false`(一覧未設定)なら `loanable_list_missing` で全 SELL を reject、載っていなければ `not_loanable <symbol>`。
  2 つを別の理由にするのは、人間の commit 漏れと真の非貸借を区別するため。BUY と非 entry は素通し。

#### 値幅制限ゲート([risk/price_limit_gate.go](../../../backend/internal/domain/risk/price_limit_gate.go))

`EvaluatePriceLimit(sig, refPrice) → Decision` / `ProtectiveTakeProfitPlaceable(side, tpPrice, refPrice) → bool` / `ProtectiveTakeProfitOnBoard(holding, side, tpPrice, refPrice) → (place, reason)` /
`PriceInsideLimitBand(price, refPrice) → (inside, ok)` / `LimitBandFor(refPrice) → (up, down, ok)`。
`refPrice` は値幅制限の基準値段 = **前日終値**(運用側の日足は `ChainLinkSplits` を通して保存済みなので最終バーの終値をそのまま渡す)。0 = 判定材料なし。階段表は [market/price_limit.go](../../../backend/internal/domain/market/price_limit.go)(呼値テーブルとは別物・流用禁止)。

| | 帯の外に出たとき | 理由 |
|---|---|---|
| SL | **entry を reject**(`stop_loss_outside_price_limit`) | 守りを板に置けない建玉は作らない |
| TP(intraday) | **entry は通す。TP 脚だけ落として SL を置く** | 帯外 TP で entry ごと止めると、急落が深いほど強い平均回帰のシグナルだけが系統的に消える |
| TP(multiday) | **帯の内外に依らず載せない(stop-only)** | 立花は期日付き注文を翌日へ繰り越すとき翌日の帯で再検査し、TP 脚が外だと SL 脚ごと失効させる |

- 基準値段が無ければ `EvaluatePriceLimit` は fail-close(`price_limit_reference_unavailable`)。
- `ok=false` は「判定できなかった」であって「外だった」ではない。呼び手はこの 2 つを混ぜない(混ぜると日足が欠けた日にだけ守りの形が変わる)。
- `ProtectiveTakeProfitOnBoard` が「TP 脚を板に載せるか」の**唯一の入口**: ① `tp<=0` → 載せない(`no_take_profit`)② holding が `intraday` 以外(空も含む)→ 載せない(`multiday_stop_only`)③ intraday は基準値段が無ければ載せる(`reference_unavailable`)、あれば帯で判定(`inside_price_limit` / `outside_price_limit`)。
  呼び手は ExecuteOrder / ArmProtectiveOrder / RepriceProtectiveOrder / ReplaceProtectiveOrder の 4 つ。`ProtectiveTakeProfitPlaceable` も内部でこれを使う。
- 立花は脚が 1 本でも帯の外だと注文ごと拒否し、拒否文はどちらの脚が外なのかも帯の値も言わない。`LimitBandFor` は人間に返す error 文へ帯の数字を載せるためにある。
- この判定は `EvaluateOrderBoundary`(fat-finger)の前に走る(値幅制限は broker が注文ごと拒否する確定事象なので理由を混ぜない)。

#### `EvaluateHardSafety`

**絶対に上書きできないゲートだけ**を再検査する(`emergency_stop` / `risk_state_unavailable` / session / EOD / daily_loss / account_daily_loss / `margin_status_unavailable` / 委託保証金充足)。
`TradingCycle.Execute` が executor を呼ぶ直前に必ず通すので、手動 override の経路を足しても TradingCycle を通る限りこれらをバイパスできない。

### `session`(東証 calendar・純粋)

`TradingHours{TZ, Sessions, EntryCutoff, ForceFlatAt, Holidays, CalendarThrough}`。すべて caller が `now` を渡す純粋関数:
`IsTradingDay`(土日 + `Holidays` は非取引日。`CalendarThrough` を超えると**期限切れとして fail-close** — 年次更新の人間 commit まで取引日にならない)/ `InTradingHours` / `IsAfterEntryCutoff` / `IsNearClose`(引け前フラット化)/ `MinutesUntilClose` / `MinutesUntilForceFlat`(intraday Signal の `MaxHoldMinutes` を cap する)/ `MaxHoldDeadline`(多日建玉の時間切れ = N 営業日目の `ForceFlatAt`)。

`CloseAlignedDeadlines(after, through)` は after より後・through 以前の各営業日の `ForceFlatAt` を昇順に返す(MaxHold 延長の候補用。延長先を `MaxHoldDeadline` と同じ引け前に揃える)。営業日の判定は `AddBusinessDays` と同じ。`Sessions` / `ForceFlatAt` が読めなければ空(時刻を捏造しない)。

### `ta`(純粋テクニカル)

`SMA` / `SMACloses` / `ATR`(n+1 本必要)/ `Donchian`(breakout channel、`ok=false` で不足)/ `Slope`(直近 n 本の平均ステップ変化)。データ不足は 0 / `ok=false` を返す。

### `strategy`(Engine + Strategy)

- `Strategy` interface: `Name()` + `Evaluate(EvalInput) Signal`。`Evaluate` は I/O・DB・rand なしの純粋関数(price tick 毎に呼べ、backtest が決定的になる)。
- `Engine` は active config が指す strategy に dispatch。config が `nil` / 期限切れ(`IsExpired`)/ 未知 strategy のときは **`NO_TRADE` を返す fail-safe**。entry には `SignalIDFn` で `SignalID` を付与。`NewEngine` は `no_trade` を常にフェイルセーフ既定として登録する。
- **`Screener` interface(`Name()` + `Screen(symbol, daily) Candidate`)** はランキングを作る側。`Candidate` は `Triggered` / `Score` / `Detail` / `Conditions` / **`Side`**(入口が成立した向き。空 = 未成立 または買い専用)を持つ。
  `Side` が無いと、貸借でない銘柄の売り候補を枠の配分前に落とせない。screener に居ないアームは `Triggered` にならず標本ゼロのまま走るので、screener はメニューから派生する(戦略カタログ・`catalog_test` が縛る)。
- **兄弟アーム**: `X` と `X_trail` は入口が同一で出口だけが違う。`EntryArmOf` が `_trail` を畳んで入口名を返し、`direction` / `MaxHold` / 入口の枠はそれで引く。
- **bnf ファミリーの派生入口**([bnf_variants.go](../../../backend/internal/domain/strategy/bnf_variants.go)): `bnf_day2_reversion` / `bnf_stabilized_reversion` とその `_trail`。
  ① 閾値を新しく持たず、全部 `bnf.go` の定数を参照する。② 出口は共有ヘルパー `bnfCappedExit` / `bnfTrailExit` を通る(本体の `bnf_reversion` / `bnf_reversion_trail` も同じ関数)。stabilized は前日終値以上の日には bnf と 1 ビットも違わない建玉になる(`TestBNFStabilized_IsIdenticalToBNF…`)。
  ③ day2 は前日までの確定足で 25 日線を引く(`ConfirmedDailyBefore`)。当日の場中条件は `Summary.CurrentRate.Last` で判定し、日足の screener は前日条件しかミラーしない。stabilized のゲート(`still_falling`)は bnf の入口条件の後に置く。
- **日中版**([bnf_intraday.go](../../../backend/internal/domain/strategy/bnf_intraday.go)): capped(`bnf_intraday_reversion`)と `_trail` は入口を `bnfIntradayEnter` で共有し、trail は TP を「arm = 距離 A / giveback = 多日 trail と同じ比 × A」の ratchet に置き換える。
  SL / MaxHold / `HoldingIntraday` は兄弟間で同一(`TestBNFIntradayTrail_EntersExactlyWhenCappedDoes`)。床は `bnfIntradayCostFloor` を共有。前場だけ買う(`bnfiEntryCutoffMin`・理由 `outside_morning_entry_window`)。
- **回す入口を config で絞る `ScreenersForEntries`**([entries.go](../../../backend/internal/domain/strategy/entries.go)): `DefaultScreeners()`(コードのメニュー)を入口単位で絞る純粋関数。空 = 絞らない。
  **fail-close**: メニューに無い名前・兄弟名・重複は error。呼び手は `cmd/stockbot`(`advisor_v2.entries` → ScanProvider と AdvisorLoop)。停止した入口はメニューから消さない(停止であって棄却ではない)。稼働中の一覧は `research_config_guard_test` が yaml と同時更新で固定する。
- **戦略カタログ**([catalog.go](../../../backend/internal/domain/strategy/catalog.go)): 戦略の登録はここ 1 か所。行ごとに実体・メニュー掲載(`Menu`)・live の selector が arm してよいか(`LiveArmable`)・入口の向き(`Direction`)・ルックバック窓(`LookbackDays`)を持つ。
  engine の登録(`MenuStrategies`)・screener(`DefaultScreeners`)・advisor の候補(`MenuNames` = `command.AdvisorCandidateStrategies`)・live の screener(`LiveArmScreeners`)・backtest の名前解決(`ByName`)・`MaxHoldBusinessDays` の窓・`command.ArmDirection` はそこから派生する。形は `catalog_test.go` が固定し、戦略を 1 本足すときに触るのはこの 2 ファイルだけ。`Menu=false` の行は `cmd/backtest` の再現用。live で起動してよいかは別の栓(`hard_limits.live_allowed_strategies` と `catastrophe_guards_test`)。
  **登録だけでは何も建たない** — active config が指名した戦略だけが動く。paper で実際に回るのは `advisor_v2.entries` で絞った集合。全戦略 **未証明**で、net-of-cost edge を証明するまで株数を上げない([../../../CLAUDE.md](../../../CLAUDE.md) §4)。

### 株式分割(併合)の権利落ち

- `market.ExDateSplitRatio(prevClose, price)` — **前営業日終値を基準にした値幅制限の外** かつ **単純分割比(`simpleSplitRatios`)に一致**のときだけ株数の倍率(1:5 → 5、10:1 併合 → 0.1)を返す。
  帯の内側の値動きは比が分割比に似ていても分割と読まない(読むと本物の暴落で SL を割り引く = fail-open)。1:1.2 のような小さい比は帯の内側に収まるので値段からは断定できない(live は broker の建玉照会で拾い、paper は拾えない)。
- `position.SplitAdjusted(p, r, day)` — 株数 ×r、円/株の凍結値(建値・TP/SL の価格と幅・ratchet・peak/trough・延長/早期利確)÷r。建玉金額と各出口までの損益は不変(単位の書き直し)。
  TP/SL 価格は `RoundToTickOf` で格子へ乗せ、建値は丸めない。株数が整数にならなければ error。

---

## テスト方法 (この層特有)

- **fake / mock 不要**。値の入出力だけで完結する(`time.Now()` は `now` 引数注入、ID は `SignalIDFn` 注入なので決定的)。
- 同じ動作軸の複数ケースは **table-driven** で書く。

```go
func TestTPSLPricesFromJPY(t *testing.T) {
    // 7735 は細かい呼値の銘柄・15,890円 → 呼値 5円(実テストは TestTPSLPricesFromJPY_Buy 等)。
    cases := []struct {
        name                string
        symbol              string
        side                order.Side
        entry, tpJPY, slJPY float64
        wantTP, wantSL      float64
    }{
        {"BUY", "7735", order.SideBuy, 15890, 500, 250, 16390, 15640},
        {"SELL", "7735", order.SideSell, 15890, 500, 250, 15390, 16140},
        {"呼値に乗らない幅は丸める", "7735", order.SideBuy, 15890, 502, 253, 16390, 15635},
        {"zero width = absent", "7735", order.SideBuy, 15890, 0, 0, 0, 0},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) { /* ... */ })
    }
}
```

詳細は [../../workflows/TESTING.md](../../workflows/TESTING.md) を参照。

---

## 既存実装の代表例

- [backend/internal/domain/market/aggregator.go](../../../backend/internal/domain/market/aggregator.go) — tick を複数 interval の rolling OHLCV candle に畳む(mutex 例外その 1)
- [backend/internal/domain/market/candle.go](../../../backend/internal/domain/market/candle.go) — `RingBuffer`(最古を捨てる固定長 candle buffer、mutex 例外その 2)。面は `Push` / `Snapshot` の 2 本だけ
- [backend/internal/domain/market/tick.go](../../../backend/internal/domain/market/tick.go) — 価格 → 呼値刻みの写像(`TickSize`)と `IsTickAlignedOf`
- [backend/internal/domain/position/price.go](../../../backend/internal/domain/position/price.go) — `TPSLPricesFromJPY`(円/株の出口幅 → 絶対 broker 価格、`RoundToTickOf` で銘柄別呼値グリッド丸め)
- [backend/internal/domain/risk/gate.go](../../../backend/internal/domain/risk/gate.go) — 順序付きリスクゲート(`EvaluateSignal` / `EvaluateStructural` / `EvaluateCollateral` / `EvaluateHardSafety`)。`EvaluateStructural` は `structuralGates` の表を歩く(順序は `TestStructuralGateOrderIsPinned`)
- [backend/internal/domain/risk/price_limit_gate.go](../../../backend/internal/domain/risk/price_limit_gate.go) — 値幅制限ゲート
- [backend/internal/domain/market/price_limit.go](../../../backend/internal/domain/market/price_limit.go) — 値幅制限の階段表(`LimitUp` / `LimitDown`。呼値テーブルとは別物・流用禁止)
- [backend/internal/domain/strategy/engine.go](../../../backend/internal/domain/strategy/engine.go) — strategy registry + `Evaluate`(fail-safe = `NO_TRADE`)
- [backend/internal/domain/strategy/time_series_momentum.go](../../../backend/internal/domain/strategy/time_series_momentum.go) — 日足トレンド/breakout のテンプレ実装例(`Menu=false`。`cmd/backtest` 用に残す)
- [backend/internal/domain/strategy/edge_gate.go](../../../backend/internal/domain/strategy/edge_gate.go) — `CostFloor` / `applyCostFloor`(往復コスト床を超えない entry を `NO_TRADE` に落とす)

---

## 既知の例外 / 注意点

### `domain` → `config` 依存(許容される弱い違反)

`domain/strategy` と `domain/risk` は `config.StrategyConfig` / `config.HardLimits` 等の型と定数を import する。
`config` package が strategy schema と value object の正本でもあるための許容で、範囲は `risk/gate.go` と `strategy/` だけ
(`scripts/arch-guard.sh` 規則 (l) が他サブパッケージへの拡散を止める)。

### IFDOCO / TP・SL の broker 側保持

`OrderType.IFDOCO` は型として存在するが現 adapter では未使用。TP/SL は必ず broker 側(OCO/逆指値)で持つ
不変条件は [../../../CLAUDE.md](../../../CLAUDE.md) §3 と [../FAILURE_MODES.md](../FAILURE_MODES.md)。それを検査する述語は `Order.HasStopLeg`(上の `order` 節)。

## アンチパターン (= やってはいけない失敗)

- domain 内で `time.Now()` を直接呼ぶ → `clock.Clock` 注入 か `now time.Time` 引数(`EvalInput.Now` / `AccountSnapshot.Now`)で受ける
- `strategy.Engine.Evaluate` の中で `rand` / `crypto/rand` を呼び ID 生成 → `SignalIDFn` を `NewEngine` に注入する
- `domain/position` や `domain/risk` に新しい mutex / goroutine を持ち込む → 許容は `market` の `Aggregator` / `RingBuffer` の 2 型のみ。新規 buffer は本ファイルに例外を明記してから
- `Side.Opposite()` の不正入力で既定 `BUY` にフォールバック → 空文字 `""` を返し下流 `Valid()` で reject(誤った反対売買を防ぐ)
- Position entity に DB マッピング tag を付ける → 永続化 record は port 層に分離(domain は broker / DB 非依存)
- broker へ送る TP/SL を `RoundToTickOf` 前に渡す → 呼値非整合で risk gate が reject する

---

## 関連 docs

- [port.md](port.md) — domain が依存する interface(domain は port を import しない側)
- [safety.md](safety.md) — risk gate / emergency / 引け前フラット化 の安全層
- [../FAILURE_MODES.md](../FAILURE_MODES.md) — TP/SL broker 側保持などの失敗モード
- [../PR_CHECKLIST.md](../PR_CHECKLIST.md) — domain 純度(import / mutex / time.Now)チェック
- [../../workflows/TESTING.md](../../workflows/TESTING.md) — 古典派 TDD のドメインテスト
- [../../runtime/STATE_MACHINE.md](../../runtime/STATE_MACHINE.md) — Position の状態遷移(`OPEN` / `CLOSING` / `CLOSED`)
- [../../../CLAUDE.md](../../../CLAUDE.md) — 不変条件・層規約・エッジ規律
