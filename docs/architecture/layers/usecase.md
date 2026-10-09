# Layer: Usecase

## 役割 (1 行)

ビジネスフローの調整役。複数 domain 操作を順序立て、Repository / Broker を **port 経由**で呼び、entry/exit saga・reconcile・force-flatten・status read の状態遷移を完結させる。

---

## CQRS — Command と Query は明確に分離する

1 つの usecase が **書きと読みの両方** を持つことは禁止。本リポジトリでは物理的に別パッケージに分かれている。

### Command の規約

- 場所: [`backend/internal/usecase/command/`](../../../backend/internal/usecase/command/)
- 役割: 状態変更・複数ステップ saga・emergency_stop trip 判断
- パッケージ doc ([execute_order.go](../../../backend/internal/usecase/command/execute_order.go) 冒頭) の不変条件:
  「各 usecase は **port interface と domain/config のみ** に依存し、具体 adapter を一切 import しない。複数ステップの状態変更は失敗時に compensate する」
- 戻り値: 副作用 ID + 最小限の確認情報のみ (例: `ExecuteOrder.Execute` は新規 position id を返す)
- 外部システム (broker) を含む副作用は **claim / compensate / CAS パターン** を使う

### Query の規約

- 場所: [`backend/internal/usecase/query/`](../../../backend/internal/usecase/query/)
- パッケージ doc ([list_open_positions.go](../../../backend/internal/usecase/query/list_open_positions.go) 冒頭) の不変条件:
  「View DTO を返し、domain entity を境界に露出しない / 状態を変更しない」
- 戻り値: View 向け DTO (`OpenPositionView` / `BotStatusView` / `SymbolStatusView`)。`position.Position` を直接返さず `toView(p)` で変換する
- 読み取り専用 (Tx 不要)

### event-driven 命名 (例外)

`Execute` を基本とするが、tick / signal / loop 駆動の usecase は実態に合わせた命名を許可する。

| 種類 | 例 | パターン |
|---|---|---|
| Cycle 駆動 | `TradingCycle.Execute` | 1 tick の entry 判定パス |
| Signal 駆動 | `ExecuteOrder.Execute` | gate 通過済み entry の saga |
| Tick 駆動 | `ManageOpenPositions.OnTick` | 1 tick の exit 判定 |
| 近接駆動 | `ForceFlatten.FlattenIfNearClose` | 引け前フラット化トリガ |
| 周期駆動 | `Reconcile.Run` | broker→DB sync の 1 pass |

命名で実態を曲げない。長尺/周期フローを無理に `Execute` に揃えない。

---

## やること (do)

- Repository (`port.PositionRepository` / `port.TradeRepository`) / Broker (`port.Broker` / `port.LiveBroker`) / Closer (`port.PositionCloser`) を **port 経由**で呼ぶ
- domain の純粋関数を呼んで業務判定を作る — `risk.EvaluateSignal`、`position.TPSLPricesFromJPY`、`market.TickSizeOf`、`EvaluateExit` (exit 判定ロジック)
- entry saga の **compensate** (post-fill 失敗時に naked position を close)
- exit/flatten の **CAS claim** (`ClaimForClose` で二重 close を防ぐ)
- emergency_stop の **trip 判断** (維持率割れ / forced_liquidation_failed / reconcile stale)
- broker 側 OCO (TP+SL) を **必ず broker に置く** — bot 死でも守りが残る ([execute_order.go](../../../backend/internal/usecase/command/execute_order.go))
- intraday MaxHold を引け前フラット化までに **cap** し、建玉が引けを跨がないようにする ([trading_cycle.go](../../../backend/internal/usecase/command/trading_cycle.go))

### emergency_stop は local interface 経由 (重要イディオム)

usecase は `internal/safety` を **直接 import しない**。consumer 側の最小 interface を usecase パッケージ内に自前で定義する:

- command 側 ([exit_executor.go](../../../backend/internal/usecase/command/exit_executor.go)):
  ```go
  type EmergencyController interface {
      Active() bool
      Trip(reason string, now time.Time) error
  }
  ```
- query 側 ([get_bot_status.go](../../../backend/internal/usecase/query/get_bot_status.go)):
  ```go
  type EmergencyReader interface {
      Active() bool
      Reason() string
  }
  ```

`*safety.EmergencyStop` がこれらを満たす。配線は `app`/`cmd` 層で行い、usecase は安全装置の具体実装を知らない。

---

## やらないこと (don't)

- `*broker.Paper` / `*broker.Tachibana` などの **具体型** を知る (port 経由のみ)
- `*repository.InMemoryPositionRepo` / `pg.*` を直接持つ (`port.PositionRepository` interface 経由)
- `internal/safety` を直接 import する (上記 local interface 経由)
- `pgx` / `net/http` / `slog` / `os` を import する (層規約。[CLAUDE.md](../../../CLAUDE.md))
- broker 成功 + DB 失敗を silent return する (= fail-open)。`ExecuteOrder` は post-fill 失敗で **compensate して close**、close 価格が取れなければ **0 円 trade を記録せず CLOSING のまま残す**
- Query の戻り値で domain entity (`position.Position`) をそのまま返す (View DTO 化)
- TP/SL を bot 内 OnTick 監視だけで守る (broker 側 OCO が primary。`OnTick` は時間/経路ベース exit + TP/SL backup)

---

## 命名 / 配置

| 種別 | 場所 | ファイル名 (snake_case) | 例 |
|---|---|---|---|
| Command Usecase | `usecase/command/` | `<action>.go` | `execute_order.go` / `force_flatten.go` |
| Query Usecase | `usecase/query/` | `<action>.go` | `get_bot_status.go` |
| 共有 helper (command 内) | `usecase/command/` | `<concept>.go` | `exit_executor.go` / `snapshot.go` |

### 構造体・コンストラクタ

```go
type ExecuteOrder struct {
    broker    port.LiveBroker
    posRepo   port.PositionRepository
    pending   port.PendingPositionTracker
    emergency EmergencyController
    clock     clock.Clock
}

func NewExecuteOrder(b port.LiveBroker, pr port.PositionRepository, pending port.PendingPositionTracker, em EmergencyController, c clock.Clock) *ExecuteOrder { ... }
func (e *ExecuteOrder) WithCloser(c port.PositionCloser) *ExecuteOrder { ... } // 巻き戻しを台帳に書く
func (e *ExecuteOrder) EntryBlocked(symbol string, now time.Time) bool { ... }  // 同日巻き戻し済みか

func (e *ExecuteOrder) Execute(ctx context.Context, in ExecuteOrderInput) (int64, error) { ... }
```

- 依存は struct field、`New*` で外部から差し込む (clock は `nil` で `clock.System()` フォールバック)
- input は struct 化 (`ExecuteOrderInput` / `EvalInput` / `SnapshotCaps`)

---

## テスト方法 (この層特有)

- **strict TDD** — Red → Green → **Refactor** の 3 段すべて必須。Refactor 段の省略は禁止 ([../../workflows/TESTING.md](../../workflows/TESTING.md))。
- **古典派 (Classical / Detroit) TDD** — mock 過多を避け、real collaborator を組み合わせる。
- 主要 real collaborator (test harness [command_test.go](../../../backend/internal/usecase/command/command_test.go)):
  - `broker.Paper` (`broker.NewPaper`) — broker 境界の real 実装 (in-memory match engine)
  - `repository.InMemoryPositionRepo` / `repository.InMemoryTradeRepo` — DB 境界の real 実装
  - `repository.Closer` (`repository.NewCloser`) — close saga (CLOSING→CLOSED + trade insert) の real 実装
  - `safety.EmergencyStop` — emergency_stop 副作用 (flag file 書き込み) の real 実装。`EmergencyController` / `EmergencyReader` を満たす
- mock の使用は以下 **3 用途に限定** (= Refactor 段で必ず再評価):
  1. システム境界 (= プロセス外部) を切り出すとき (DB / 立花 / 外部 CLI)
  2. 特定の失敗シナリオを注入したいとき (place 失敗 / resolve timeout / close reject)
  3. 時刻・乱数など非決定性を排除したいとき (`clock.Clock` 注入)
- 該当しない fake は real collaborator (`broker.Paper` / `repository.InMemory*Repo`) に置換する
- 同じ動作軸の複数ケースは **table-driven** にする (例: `EvaluateExit` の優先順序検証)
- 詳細は [../../workflows/TESTING.md](../../workflows/TESTING.md) を参照

---

## 既存実装の代表例

各ファイルの契約だけを書く。

### Command — entry

- [trading_cycle.go](../../../backend/internal/usecase/command/trading_cycle.go) — 1 tick の entry 決定パス。順序は
  `engine.Evaluate` → 起動時 reconcile の保留(`reconcile_unconfirmed`)→ `EntryLock`(`entry_lock_timeout`)→
  exec-kind 整合(`oneday_margin_cannot_hold_multiday`)→ intraday MaxHold を引け前フラット化までに cap(`no_time_before_force_flat`)→
  `executor.EntryBlocked`(`entry_rolled_back_today`)→ `snapshot.BuildStructural` → `risk.EvaluateStructural` → `FillCollateral` →
  `risk.EvaluateSignal` → `applyQtyMultiplier` → `risk.EvaluatePriceLimit`(`stop_loss_outside_price_limit`)→ `risk.EvaluateShortLoanable` →
  `risk.EvaluateOrderBoundary` → `risk.EvaluateHardSafety`(executor の直前に必ず通る)→ `executor.Execute`。
  - 値幅制限の基準値段は `priceLimitRef(in)`(日足の最終バーの終値 = 前日終値)。同じ値を `ExecuteOrderInput.PriceLimitRef` で executor に渡す。
  - `HoldEntriesUntilReconciled` を立てるのは `SymbolBundle.StartupReconcile` だけ、落とすのは reconcile の成功(`MarkReconciled`)だけ。止めるのは新規だけで、守りの置き直しと決済は止めない。
  - サーガの失敗も `recordRejection` する(`entry_execution_failed`)。
- [entry_serializer.go](../../../backend/internal/usecase/command/entry_serializer.go) — 口座単位の entry 排他(`EntrySerializer`。トラックに 1 つ)。
  snapshot の取得から `executor.Execute` までを囲み、最後の 1 枠を 2 銘柄が同時に取る TOCTOU を防ぐ。待ちに上限があり、超えたら `entry_lock_timeout`。
  内側の発注の往復は取り消さない(ctx を切ると約定照会と補償まで落ちる)。決済と守りは対象外。
- [execute_order.go](../../../backend/internal/usecase/command/execute_order.go) — entry saga。MARKET 発注 → `ResolveExecution` → `PlaceSettleOCO` →
  `ResolveSettleLegs` → `verifyProtectiveOrder` → `posRepo.Insert`(config_id / 出口幾何 / holding / exec / tick を凍結)。
  - post-fill のどのステップで失敗しても `compensate` で建玉を閉じてから error を返す。補償の決済が失敗したら `Trip("compensating_close_failed:<bpID>")`。発火は `OpsCounters.IncrCompensations()`。
  - 板に出す TP(`ocoTP`)と台帳に凍結する TP(`tp`)は別の変数。`risk.ProtectiveTakeProfitOnBoard` が板の TP を決め(多日は stop-only、intraday は帯の外なら TP 脚を落とす)、台帳の TP は OnTick が持つ。帯による縮退は `IncrProtectiveTPDropped` で数える。
  - `WithCloser` を挿すと、約定後に巻き戻した往復を `close_reason='entry_compensated'` で台帳に書く。決済の約定を確認できたときだけ書き、できなければ CLOSING のまま reconcile に渡す。
  - `EntryBlocked`: 巻き戻した銘柄はその営業日はもう建てない(台帳に書けなかった枝の最後の栓。銘柄単位)。
  - `recoverUnconfirmedSubmit` は発注数量より大きい建玉を閉じない(立花は信用建玉を銘柄単位に集約するので、超える分は人間の建玉でありうる)。所有を証明できなければ `entry_submit_unconfirmed:<symbol>` で trip。
- [oco_resting.go](../../../backend/internal/usecase/command/oco_resting.go) — 守りが板に実在するかを注文一覧(実 wire)で確かめる。決済側・同銘柄・数量 `>= qty` の注文が無ければ補償クローズ。
  照会の失敗も拒否(fail-close)。`ResolveSettleLegs` はプロセス内の記憶を読むだけなので、これが「broker 側に守りがある」を確かめる唯一の検査。paper も同じ段を通る(`TestEntrySagaStillOpensOnPaper`)。
- [snapshot.go](../../../backend/internal/usecase/command/snapshot.go) — `risk.AccountSnapshot` を 2 段で組む。
  - `BuildStructural` は repos + calendar + emergency だけで broker を呼ばない。`MarginStatusUnknown = true` を立てて返し、`FillCollateral` が照会に成功したときだけ降ろす。
  - `FillCollateral` は自分で wire を打たず `AccountMarginCache` から読む。`MinCollateralJPY` / `MaxGrossNotionalRatio` / `CollateralJPY` / `OpenGrossNotionalJPY` を埋める。ratio が正のときだけ `ListOpenAllSymbols` を引き、読めなければ `RepoStatusUnknown`(fail-close)。
  - 計画損失の上限(`MaxRiskPerTradeJPY`)は Signal だけで判定できるので `BuildStructural` 側に載せる。0 は無効。
  - 日中保有の建てでだけ `EntriesTodaySameStrategy`(同日 1 回転)を訊く。読めなければ fail-close。
  - 両方をまとめた `Build` は置かない(呼ぶだけで口座照会を払う入口を作らない)。caps は `SnapshotCaps` の plain value で受ける(R1)。
- [account_margin.go](../../../backend/internal/usecase/command/account_margin.go) — 口座の保証金照会を 1 か所に集める `AccountMarginCache`。
  `Get`(entry 経路・キャッシュ優先)/ `Refresh`(維持率ブレーカー・必ず wire)/ `Invalidate`(約定・決済。`SymbolBundle` が握る)。
  wire を打つのは初回・`Invalidate` 後・maxAge 経過だけ。失敗はキャッシュせず直前の値も捨てる(再試行には床 `accountMarginRetryFloor`)。
  含み損による余力の変動には追随しないが、ずれは「拒否される」か「見送る」側に倒れ、守りが消える方向には倒れない。

### Command — exit と台帳

- [manage_open_positions.go](../../../backend/internal/usecase/command/manage_open_positions.go) — `OnTick` で 1 銘柄の exit を評価する。判定は domain の
  `position.EvaluateExit`(優先順序: ratchet → take-profit → stop-loss → max-hold(+extension)→ early-exit。backtest と共有)。external は管理しない。
  維持率ブレーカーは独立メソッド `CheckMaintenance`(値段に依らず毎回走り、`AccountMarginCache.Refresh` で必ず実照会)。決済判定の前に `SplitGuard.Screen` を通す(下の「株式分割」)。
- [exit_executor.go](../../../backend/internal/usecase/command/exit_executor.go) — `closeExecutor.closeOne`(ManageOpenPositions / ForceFlatten / CloseAllOpen が共有)。
  - 板の照会は `ClaimForClose` より前。照会に失敗したら claim せずに OPEN のまま返す。守りの取消は板から営業日つきで引く(`port.ProtectiveOrderBoard`。台帳の leg id では撃たない)。paper は `GetActiveOrders` → `CancelOrder`。
  - `resolveSettleFill`: 受理は約定ではない。約定を確認できたときだけ書き、未約定(`port.ErrOrderNotFilled`)/ 確認不能 / 部分約定は CLOSING のまま reconcile に渡す。判別は broker の宣言 `SettleFillsAsync()`。**未約定の決済注文は cancel しない**(ストップ安の比例配分への唯一の参加経路)。
  - 返済が拒否されたら `positionGoneAtBroker` で建玉照会を見る。broker が持っていなければ trip せず `boardSettleFill` の実約定で締める(取れなければ CLOSING)。照会が落ちたら trip(fail-close)。拒否の文面では判定しない。
  - 台帳へ書く口は `bookSettled` の 1 本(net = gross − fee + carry・`RatchetCloseReason`)。close 価格が取れなければ 0 円 trade を書かず CLOSING のまま残す。
  - 守りを cancel した後に決済が約定も板への常駐もしなければ建玉は裸になる。この判定は `tripIfSettleLeavesPositionNaked` を決済経路と補償経路で共有し、鳴らす直前に約定を 1 回引き直す。
  - 取消の失敗は呼び手へ返すが、返済が通った枝では握り潰す。部分約定は検知するだけで、残数量の再送は無い。
- [board_settle_fill.go](../../../backend/internal/usecase/command/board_settle_fill.go) — 建玉が broker から消えたときに何が起きたかを実額で確定する。
  - `positionGoneAtBroker(ctx, brk, p) (gone, checked bool)`: 判定の権威は建玉照会だけ。銘柄が残っていれば「消えていない」と読む(誤る向きは必ず「まだ在る」)。照会が落ちたら `checked=false`。
  - `boardSettleFill`: 決済側の約定が建玉数量とぴったり一致したときだけ採る(他人の売買の値段で締めない)。
  - `boardFillReason`: 凍結した守りのどちら側で約定したかで `stop_loss` / `take_profit`、どちらでもなければ `broker_close`。SL を先に見る。凍結値が 0 の脚は判定に使わない。意味づけは推測しない。
- [force_flatten.go](../../../backend/internal/usecase/command/force_flatten.go) — `FlattenIfNearClose` が `hours.IsNearClose` のときだけ `HoldingMode==intraday` の bot 建玉を MARKET で返済する。
  返済の失敗は `Trip("forced_liquidation_failed")`。
- [close_all.go](../../../backend/internal/usecase/command/close_all.go) — 手動手仕舞いの運用ツール(戦略の出口ではない)。`Execute` は OPEN かつ非 external の全建玉、`CloseOne(ctx, id)` は 1 建玉を閉じる。
  close_reason は `manual`。帳簿締めの拒否では trip しない(CLOSING のまま reconcile)が、裸になる枝だけは必須引数 `unprotected EmergencyController` で trip する(live は `lt.emergency`)。
  価格は cmd が注入する `QuoteLookup`。emergency 中でも通る(決済はリスクを減らす方向)。`CloseAllResult` の `failed` は「CLOSING のまま reconcile 待ち」の数。
- [reconcile.go](../../../backend/internal/usecase/command/reconcile.go) — `Run` が broker の建玉と台帳を突き合わせる。
  - pending でない broker 建玉は `AdoptExternal` で external として採用。台帳にあって broker に無い建玉は grace 内は defer、hard window を超えたら `Trip("reconcile_stale_position_unresolved:<id>")`。PnL は推測しない。
  - 建玉が消えたら `coldClose` はまず `boardSettleFill` の実約定と実手数料で締める(`stop_loss` / `take_profit` / `broker_close`・`fee_estimated=false`)。採れなければ観測価格の `reconcile_cold_close`。carry は `closeOne` と同じ規約(`WithCarry`)。
  - external 建玉が消えたら `close_reason='external_close'` で trade を書く(口座には実額で効く)。観測価格が無ければ trade を書かず建玉だけ閉じる。
  - CLOSING の再送枝(`resolveStuckClosing`)は `closeOne` と同じ約定解決を使う。自分の返済注文が板にあるかは `hasRestingSettleOrder`(側 + 数量 `>=` 建玉数量。逆指値脚を要求しない)で見て、待ちに上限(`restingLimit`)を置く。
  - `ReconcileReport.StuckClosing` = 決済を出したが約定を確認できず CLOSING のままの件数。呼び手(`SymbolBundle.ReconcileTick`)が Warn に出す。
- [ops_counters.go](../../../backend/internal/usecase/command/ops_counters.go) — `OpsCounters` local interface(`IncrCompensations` / `IncrExternalAdoptions` / `IncrProtectiveTPDropped`)。app を import せずに `/api/status` へ出す。nil-safe。

### Command — broker 側の守り(live の多日建玉)

期日と値幅制限の規則は [CLAUDE.md](../../../CLAUDE.md) §3。共通の縛り:
値段は板か台帳の凍結値から、`ExecKind` と `BrokerPositionID` は台帳から取る。建玉を台帳から 1 つに確定できなければ触らない。
帯の判定は `WithPriceLimitRef` で挿し(挿さなければ判定しない)、基準値段が読めなければ何も変えない。

- [protective_positions.go](../../../backend/internal/usecase/command/protective_positions.go) — 守り系コマンドの共有 helper(`multidayGuardedPositions` / `multidayPositions` / `ownsProtectiveOrder` / `matchProtectivePosition` / `needsRenewal` / `expiryLabel`)。
  条件を 1 か所に置くのは、片方だけ触る建玉を作らないため。CLOSING を含めるのは arm と rearm だけで、値段や期日を触る経路は決済中の建玉に走らせない。
- [replace_protective_order.go](../../../backend/internal/usecase/command/replace_protective_order.go) — 期日が近い守りを取消 → 同条件で再発注する(唯一の期日の延長手段。訂正では発注日起点の天井を超えられない)。
  場外でしか撃たない / 残りが `replaceProtectiveWithin` を切った守りだけ / 取消が失敗したら再発注しない / 再発注が失敗したら trip / 取り消す前に今日の帯で検問(SL が外なら取り消さない、TP が外なら TP 脚を落とす。多日は `risk.ProtectiveTakeProfitOnBoard` で stop-only)。
  見送りは `Skipped` で返す(`Replaced: 0` を「対象なし」と「全部失敗」に割る)。
- [arm_protective_order.go](../../../backend/internal/usecase/command/arm_protective_order.go) — 守りが 1 本も無い多日建玉に守りを置く手動経路(`POST /api/live/protective/arm`)。
  人間が渡すのは値段だけ。SL 必須 / 板に逆指値の守りか決済側の注文があれば置かない / external・intraday・同銘柄に複数 → 置かない / SL が帯の外なら置かず帯の数字を error で返す / 常に stop-only。
  emergency 中でも場中でも撃てる(建玉を作らず、リスクを減らす方向にしか動かない)。休場カレンダーが尽きたら error。
- [rearm_unguarded.go](../../../backend/internal/usecase/command/rearm_unguarded.go) — 板から消えた守りを台帳の凍結値(`frozenLegs`)で置き直す。live に `rearmProtectiveInterval` 周期で配線。
  CLOSING も対象 / 時間帯で縛らない / emergency 中でも走る / 凍結 SL が無ければ error。broker が持っていない建玉には置かない(`Settled` に数える)。建玉照会は 1 周に 1 回で、落ちた回は置きに行く。
  broker の株数が台帳と違う建玉には置かない(株数 r 倍かつ建単価 1/r なら分割として言い直してから置く)。`Armed` と `Skipped` を割る。1 銘柄の失敗で他を止めず、並びは決定論。
- [reprice_protective_order.go](../../../backend/internal/usecase/command/reprice_protective_order.go) — 守りの値段を変える(取消 → 指定値段で再発注)。設計上、場中でも撃つ。
  取り消す前に全部検証する: 建玉の確定 / SL あり / 値段の向き / 板に守りが実在 / 呼値の格子(`market.IsTickAlignedOf`)/ 今日の帯。取消失敗は trip しない、再発注失敗は trip(文言に復旧コマンド)。
  成功したら台帳の TP/SL も揃える(`UpdateProtectivePrices`。TP 0 は台帳の利確を据え置く。更新の失敗は trip せず食い違いを名指しで返す)。
- [extend_maxhold.go](../../../backend/internal/usecase/command/extend_maxhold.go) — 開いた建玉の MaxHold を延ばす(config 凍結の唯一の例外・`POST /api/positions/extend`)。
  1 回の上限は `position.MaxExtendMinutes(保有区分)`(`ErrInvalidExtendMinutes`)。emergency 中は `ErrEmergencyActive`。broker 側の守りには触らず、多日の応答には「守りの期日は動いていない」警告が載る。

### Command — 株式分割(併合)の権利落ち

- [split_guard.go](../../../backend/internal/usecase/command/split_guard.go) — `ManageOpenPositions.OnTick` が決済判定の前に `Screen` を通す(`WithSplitGuard`)。
  - 前日終値は運用側の日足の最終バーが前営業日のときだけ使う。見つからない結果は短時間だけ覚える。
  - 分割の断定は `market.ExDateSplitRatio`、言い直しは `position.SplitAdjusted`、書込は `PositionRepository.ApplySplit`(CAS)。
  - 権限はトラックごとに明示(`SplitAuthority`): paper は値段の証拠だけで言い直し、紙の帳簿(`port.SplitAdjustableBook`)も揃える。live は broker の建玉照会の株数 ×r かつ建単価 ÷r(`brokerSplitFactor`)が示したときだけ言い直す。銘柄ごとにその日最初のティックで 1 度照会する。
  - 株数が同じで建単価だけ変わった建玉(`brokerRightsProcessed`。立花は整数倍以外の分割で株数を増やさない)は言い直さず、その日は自動決済しない(`hold`)で `split_unconfirmed:<symbol>` を trip(live だけ)。
  - 権利落ち日に建てた建玉・external・CLOSING は触らない。

### Query

- [get_bot_status.go](../../../backend/internal/usecase/query/get_bot_status.go) — `/api/status` の `BotStatusView`(mode / broker_kind / emergency(`EmergencyReader`)/ 口座の建玉数 / 銘柄ごとの active config と建玉)。
- [list_open_positions.go](../../../backend/internal/usecase/query/list_open_positions.go) — 銘柄の OPEN / CLOSING 建玉を `OpenPositionView` で返す(`toView(p)`)。
  時間切れの期限 `max_hold_until` と延長上限 `max_hold_hard_until` を載せる(画面は自前で足さずこの値を読む)。
- [list_extend_options.go](../../../backend/internal/usecase/query/list_extend_options.go) — MaxHold 延長の候補を各営業日の引け前(`session.CloseAlignedDeadlines`)で、1 回の上限以内だけ列挙する(`until` / `business_days` / `beyond_calendar`)。
  未知 id・OPEN でない建玉は (nil, nil) → 404、期限の無い建玉は候補 0。画面は選んだ候補の `add_minutes` を `ExtendMaxHold` へ渡すだけ。
- [forward_report.go](../../../backend/internal/usecase/query/forward_report.go) — trades 台帳(forward 検証の唯一のエッジ証拠)を `port.ClosedTradeReader.ListClosedSince` から `ForwardReportView` に集計する。
  - net = gross − fee + carry(daily loss cap と同じ規約)。`NetPnLJPY` は closed_at 昇順で `cmd/edge-judge -in` にそのまま渡せる。`ExecuteRange(since, until)` で終了日(排他)も切れる。
  - オプション: `WithStrategyResolver`(戦略別の分計・pg のみ。注入しないと `Trades[].Strategy` は空)/ `WithStrategyFilter` / `WithSideFilter`(診断用で昇格ゲートではない)/ `WithScoreResolver`(score の分位別分計。復元不能は `ScoreMissingN`)。
  - `entry_compensated` と `external_close` はエッジ標本から外す(判定は `port.IsNonStrategyClose`)。黙って落とさず `CompensatedN` / `ExternalN` と `CompensatedNetJPY` / `ExternalNetJPY` を必ず出す(0 と「集計していない」を分けるので `omitempty` を付けない)。集計は絞った後に行う。
  - 画面(`/api/performance`・`/api/live/performance`)は口座ベースで、`WithNonStrategyClosesCounted()` で上の 2 つも戦略に計上する。出力は `counting`(`edge_sample` / `account`)で名乗り、取り違えは `cmd/edge-judge` の `checkCounting` が fail-close で弾く。組み立ては `cmd/stockbot` の `perfReportOptions` の 1 本。
- `daily_journal.go`(引け後パケット = 段 2 LLM の入力)は口座ベース。何がどう閉じたかは `by_reason` と `closed[].close_reason` に残す。
  `JournalOpenView` は引け時点の建玉の居場所(凍結した出口幾何・MFE / MAE は台帳から、終値と含み損益は app 層の `journal.EnrichOpenPositions` が当日の日足から)を持つ。
  単位を混ぜない(MFE / MAE と `unrealized_per_share_jpy` は円 / 株、`unrealized_jpy` は総額)。当日の日足が無ければ 0 で埋めず `unavailable` に理由を入れる。日記の数字を判定に持ち込まない。

---

## AI advisor コマンド (config 生成・既定 OFF)

LLM は発注経路に入らず **config を吐くだけ**。usecase は
`app` を import しないので arm は行わず、検証済み config を返して呼出側(app/cmd)が
`holder.Set` する。研究トラックの出荷値(`advisor_v2.arm_source: template`)では `command.ArmTemplate` が config を作り、LLM はこの経路に居ない。

- `command.Promoter` — LLM の YAML を検証専任(状態変更なし)。**fail-closed**: `mode=paper_config` 強制・`ExpectedSymbol` 一致・
  `AdvisorCandidateStrategies`(+ no_trade)内・`ExpectedStrategy`(枠を配った戦略か no_trade のみ)・`ValidateAgainstHardLimits`・
  `holding_mode` 必須・`exec_kind` は cash / margin_system のみ(`margin_oneday` は拒否)・`quantity>0`・config-exit 戦略は `stop_loss_jpy>0` 必須・
  `direction` は `command.ArmDirection` の事前登録表と一致。違反は error → 呼出側は arm 0。
  - `direction` はトレンド系 4 戦略(abs_momentum_v2 / atr_breakout_v2 / donchian_breakout_v2 / high_52w_momentum)が `both`、それ以外は `buy_only`(平均回帰の空売りは踏み上げ側に無限の裾が残る)。同梱の research config は全アームを `buy_only` で回す(`advisor_v2.entries`)。
  - 売り(SELL)には貸借銘柄ゲートが要る(`risk.EvaluateShortLoanable`。一覧が未設定なら `loanable_list_missing`、載っていなければ `not_loanable`)。
  - `_trail` 兄弟アームの `direction` / `MaxHold` はサフィックスを剥がして基のアームから引く(入口が同一でないとペア差を出口に帰属できない)。
  - `ValidateAgainstHardLimits` は hard_limits.yaml に range を書いた全項目を縛る。**range を足したらここに 1 行足す**。0 は「未設定」として素通しする。
- `command.AdvisorCycle` — `BuildSummary → Advisor.Generate → (非 Promotable / reject は arm 0) → Promoter.Promote`。非 infra 失敗は `(nil, run, nil)` で返す(fail-close)。
- **2 allowlist 分離**: `AdvisorCandidateStrategies`(paper 提案メニュー) ≠ `hard_limits.live_allowed_strategies`(live 実行ゲート)。
  Promoter が paper を強制するのでメニュー拡張は live を開かない。詳細は [../../ARCHITECTURE.md](../../ARCHITECTURE.md) の arm ループ節。

## アンチパターン (= やってはいけない失敗)

- advisor の非 success run / reject を「昇格」する — `Promotable()`=success のみ、reject は arm 0 (fail-close)
- advisor 由来 config で `mode=live_config` を通す — Promoter が paper 強制。live は human commit の別経路のみ
- Broker 成功 + `Insert` 失敗で log + return (silent fail-open) — `ExecuteOrder` は compensate して close する
- `ResolveExecution` timeout で 0 fill のまま DB INSERT — fill が解決できなければ saga を進めない
- close 価格が取れないのに概算で 0 円 trade を記録 — `closeOne` は記録せず CLOSING を残す
- TP/SL を bot 内監視だけで守る — broker 側 OCO (`PlaceSettleOCO`) が primary
- reconcile で drift を推測して PnL を捏造 — defer か trip のみ
- usecase から `internal/safety` を直接 import — local `EmergencyController` / `EmergencyReader` 経由
- Query の戻り値で `position.Position` をそのまま返す — View DTO 化
- Green で増やした fake を **Refactor 段で再評価せず** PR に出す (3 用途のどれにも該当しない fake は real collaborator に置換)

---

## 関連 docs

- [port.md](port.md) — interface 設計 (`Broker` / `PositionRepository` / `PositionCloser` 等)
- [safety.md](safety.md) — emergency_stop / EvaluateHardSafety
- [../FAILURE_MODES.md](../FAILURE_MODES.md) — Rollback > Fallback / compensate / claim
- [../PR_CHECKLIST.md](../PR_CHECKLIST.md) — 不変条件チェック
- [../../workflows/TESTING.md](../../workflows/TESTING.md) — usecase テスト戦略
- [../../runtime/STATE_MACHINE.md](../../runtime/STATE_MACHINE.md) — position 状態遷移 (OPEN→CLOSING→CLOSED)
- [../../../CLAUDE.md](../../../CLAUDE.md) — 不変条件・層規約
