# Layer: Port

## 役割 (1 行)

Repository / Broker / Advisor / Notifier の **interface 定義のみ**。
usecase はここに依存し、adapter がここを実装する (= 依存性逆転)。

---

## やること (do)

- interface 定義のみを置く ([repository.go](../../../backend/internal/port/repository.go) / [broker.go](../../../backend/internal/port/broker.go) / [advisor.go](../../../backend/internal/port/advisor.go) / [notifier.go](../../../backend/internal/port/notifier.go) / [pending_positions.go](../../../backend/internal/port/pending_positions.go))
- usecase が必要とする最小 method set を定義する
- DB record / Broker DTO のような **境界 record 型** を定義する (`port.TradeRecord` / `port.StrategyConfigRecord`。足は record 型を作らず `domain/market.Candle` をそのまま運ぶ — `port.CandleRepository`)
- `domain` (`market` / `order` / `position`) は import してよい — port は domain の上、adapter/usecase の下

---

## やらないこと (don't)

- 具体実装 (Postgres SQL / 立花の HTTP) を書く (= adapter の責務)
- Domain entity を**そのまま** DB 行 / Broker DTO として再利用する (境界 record を別途定義し、Domain ↔ DB/Broker の独立性を担保する)
- 上位レイヤー (usecase / app / handler / adapter) を import する
- `internal/config` package を import する (現状: import ゼロ。これを **R1 guardrail** として保つ)

### config 依存の禁止 (R1 guardrail)

`port.StrategyConfigRecord.Mode` は **`string` 型のまま** にする (`StrategyConfigRecord.Status` も `"active" | "expired" | "rejected"` を string で運ぶ)。
[broker.go](../../../backend/internal/port/broker.go) 冒頭の package コメントも明記: *"Per R1 port imports domain types but NEVER config — mode and similar values cross the boundary as plain strings."*
`config.Mode` enum を port に持ち込むと `port → config` 依存が生まれ、依存方向が崩れる。
mode の type 変換は port の **外側** (usecase / adapter) で `string(...)` するのが正しい。

---

## 命名 / 配置

| 種別 | 場所 | ファイル名 | 例 |
|---|---|---|---|
| Repository Interface | `backend/internal/port/` | `repository.go` (集約) | `PositionRepository`, `PositionCloser`, `TradeRepository`, `TradeStrategyResolver`, `TradeScoreResolver`, `CandleRepository`, `SignalRejectionRepository`, `ScreenSnapshotRepository` |
| Broker Interface | 同上 | `broker.go` (集約) | `Broker`, `MarketFeed` (発注系を持たない read-only 断面), `ExecutionResolver`, `SettleFillReporter`, `OCOCloseOrderPlacer`, `SettleLegResolver`, `ProtectiveOrderBoard`, `LiveBroker` (合成) |
| Advisor Interface | 同上 | `advisor.go` | `Advisor`(LLM config 生成・既定 OFF)、`AdvisorRunRepository`(run 監査) |
| Notifier Interface | 同上 | `notifier.go` | `Notifier` |
| Pending tracker | 同上 | `pending_positions.go` | `PendingPositionTracker` (entry saga 中の broker_position_id を保持。reconcile が DB 未挿入の position を naked external として誤 adopt しないため)。id 系に加え **symbol-scoped family** (`MarkPendingSymbol` / `MarkResolvedSymbol` / `IsPendingSymbol`) を持つ — broker position id は fill 解決 (live で最大 ~15s) まで存在せず id 系だけではその窓が race になるため、saga は PlaceOrder **前**に symbol を mark する |
| 境界 record 型 | 同上 | `repository.go` / `broker.go` | `PositionInsertInput`, `MaxHoldExtended`, `TradeRecord`, `LastClose`, `StrategyConfigRecord`, `BrokerPosition`, `PlaceOrderResult`, `CloseRequest`, `CloseResult`, `CancelResult`, `ResolvedExecution`, `OCOCloseOrderInput`, `AdvisorRun`, `AdvisorRunRecord`, `AdvisorRunStatus` |

### Repository interface ナーミング

メソッドは「動詞 + 名詞」で、Tx 必要なものは内部で開始する。`time.Now()` は port が呼ばず、caller が `now`/`closedAt`/`claimedAt` を引数で渡す (境界の決定論性):

```go
type PositionRepository interface {
    Insert(ctx context.Context, in PositionInsertInput) (int64, error)
    ListOpenOrClosing(ctx context.Context, symbol string) ([]position.Position, error)
    CountOpenAllSymbols(ctx context.Context) (int, error)
    ListOpenAllSymbols(ctx context.Context) ([]position.Position, error)
    GetByID(ctx context.Context, id int64) (*position.Position, error)
    ClaimForClose(ctx context.Context, id int64, claimedAt time.Time) (ok bool, err error)
    MarkClosed(ctx context.Context, id int64, closedAt time.Time) error
    // ...
}
```

### 建玉 store: `PositionRepository`

- `Insert(ctx, PositionInsertInput)` — entry saga 完了時の 1 行。**config_id と exit geometry をここで凍結**する (`StrategyConfigID` / `TakeProfitJPY` / `StopLossJPY` / `TakeProfitPrice` / `StopLossPrice`(約定値に幅を足し呼値グリッドに丸めた**絶対価格** — broker の OCO と `EvaluateExit` がそのまま使う)/ `MaxHoldMinutes` / `ExtensionMaxMinutes` / `ExtensionUnrealizedJPY` / `EarlyExitWindowMinutes` / `EarlyExitTargetJPY` / `RatchetArmJPY` / `RatchetGivebackJPY` / `HoldingMode` / `ExecKind` / `TickSizeAtEntry` / `Source` / **`StrategyName`**(migration 0015・ナンピン禁止と建玉枠の (銘柄, 戦略) キーを join なしで引くため。空 = 戦略不明で**全戦略をブロック**する fail-close 値)/ **`RatchetFloorAtArm`**(migration 0013・「トレールの床が効く建玉か」の版スタンプ。新規建玉のみ true — 無いと床の実装変更が既存の建玉にもその場で効き、測定対象が途中で入れ替わる)))。entry コストも同時に記録する (`EntryFeeJPY`。nil = broker 未報告)。active config を後で切り替えても既存建玉には効かない (config 凍結ルール、[CLAUDE.md](../../../CLAUDE.md))。
- `ClaimForClose(ctx, id, claimedAt)` — **double-close を防ぐ CAS**。`(ok bool, err error)` を返し、既に他者が掴んでいれば `ok=false`。
- `UpdateExcursion(ctx, id, peakUnrealized, troughUnrealized, armed)` — 建玉の MFE/MAE(円/株)と ratchet の armed を更新 (peak は monotonic increasing・trough は monotonic decreasing で負値、armed は一度 true で固定の想定)。**全建玉が対象**で、ratchet を持たない建玉でも peak/trough は動く(0010〜。旧名 `UpdateRatchetState` — ratchet 専用ではなくなったので改名した)。呼び手は戻り値を捨てない: 落とすと peak が静かに巻き戻り arm 判定がやり直しになる。
- `AdoptExternal(ctx, BrokerPosition, now)` — reconcile で発見した naked な broker 建玉を **external adoption** として記録 (表示するが bot は触らない・委託保証金にはカウントする)。
- `CountOpenAllSymbols(ctx)` — account-wide cap 用。全 symbol で OPEN+CLOSING を数える。
- `UpdateProtectivePrices(ctx, id, tpPrice, slPrice, tpJPY, slJPY)` — 人間が板の守りの値段を変えたとき(`RepriceProtectiveOrder`)に台帳の TP/SL を同じ値へ揃える。OPEN の建玉だけ(config 凍結の例外)。無いと守りが消えたとき `RearmUnguarded` が古い凍結値で置き直す。
- `CountOpenedSinceBySymbolStrategy(ctx, symbol, strategy, since)` — since 以降に (銘柄, 戦略) で建てた **bot 建玉**の数(状態を問わない = 決済済みも数える)。日中保有の「同日 1 回転」ゲートの材料。
- `CountOpenedSince(ctx, since)` — since 以降に**口座全体**で建てた **bot 建玉**の数(銘柄・戦略・状態を問わない・external は数えない)。口座全体の「1 営業日の新規本数」ゲート(`risk.account_max_entries_per_day`)の材料。上限 0(research)のときは snapshot が呼ばない。
- `ListOpenAllSymbols(ctx)` — 全 symbol の OPEN+CLOSING を id 昇順で列挙。**再起動時の紙帳簿復元**が使う: config の symbols で回すとユニバースから外れた銘柄の建玉を取りこぼし、永久に決済できなくなる。
- `GetByID(ctx, id)` — status 不問で 1 建玉を返す(未知 id は `(nil, nil)`)。個別の手動決済(`POST /api/positions/close` → [`command.CloseAllOpen.CloseOne`](usecase.md))が「そんな id は無い(404)」と「もう OPEN でない(409)」を区別するために必要 — OPEN/CLOSING しか返さない一覧では、既に CLOSED の建玉が「未知」に見えてしまう。両 adapter(in-memory / pg)に実装。

#### `ExtendMaxHold`

`ExtendMaxHold(ctx, id, addMinutes) (*MaxHoldExtended, error)` は開いている建玉の MaxHold に分を加算する手動 override(`config 凍結`の唯一の例外)。

- in-memory ([inmemory.go](../../../backend/internal/adapter/repository/inmemory.go)) と Postgres ([pg/position_repo.go](../../../backend/internal/adapter/repository/pg/position_repo.go)) の **両 adapter に実装済み**。非 OPEN / 未知 id には `(nil, nil)` を返す(caller が 404 に写像)。
- usecase は [`command.ExtendMaxHold`](usecase.md)、API は `POST /api/positions/extend` /
  `POST /api/live/positions/extend`([handler.md](handler.md))。broker 側 TP/SL OCO には
  触れず、bot の時間決済だけを延ばす。
- **上限は保有区分で決まる**: intraday 720分(12時間)/ multiday 43,200分(30日)。
  **handler は数字を持たない** — 保有区分を読めるのは建玉を引ける usecase だけで、
  両方に書くと片方だけ動いてズレる。
- `MaxHoldExtended.Warning` は **multiday のとき必ず載る**: 「broker 側の守りの期日は
  動いていない」。200 を返すだけだと「30 日持てるようにした = その間 SL がある」と
  読まれる。守りの期日を延ばすのは寄り前の
  [`command.ReplaceProtectiveOrder`](usecase.md)(取消 → 再発注)だけ。

- **`ProtectiveOrderBoard.CancelProtectiveOrder`**。守りの注文を取り消す。
  `CancelOrder(id)` を使わないのは、あちらが営業日を**プロセス内 map** から引き空なら `"0"` に
  落ちるため —— 守りは前のプロセスが出した注文なので**再起動後の取消が必ず外れる**
  
  ここも**注文そのもの**を受け取り `BrokerRef` を使う。
- **`ProtectiveOrderInfo` に `LimitPrice` / `StopTrigger` / `StopPrice`**。
  立花の `sOrderExpireDay`「10営業日迄」は**発注日起点**で、訂正しても天井が動かない。
  期日を延ばす唯一の手段は **取消 → 同条件で再発注**で、そのとき
  **いま板にある値段を引き継ぐ**必要がある — Position の凍結値で出し直すと、人間が手で
  締めた TP/SL を勝手に元の広い幅へ戻す。

#### `JournalRepository.ClosedPositions`(反実仮想の入力)

`ClosedPositions(ctx, from, to, reasons) ([]ClosedPositionSnapshot, error)` は決済済み建玉の
**凍結された出口幾何**を返す(`cmd/counterfactual` / `cmd/holding-period` が読む)。

- **`MaxHoldMinutes` を含む**。反実仮想がこれを見ないと、`manual` /
  `forced_flat` で打ち切った建玉を「MaxHold も無かったら」で歩いてしまい、期限の後に
  来た TP/SL を拾って**存在しない出口**を数える。
- 呼び手は決済理由が `max_hold` のときだけ期限を渡さない(= 「キャップが無ければ
  どうだったか」を問う唯一の理由)。
- `JournalPosition`(journal.go)は建玉 1 本で、**凍結した出口幾何**(`TakeProfitPrice` / `StopLossPrice`)と**走っている極値**(`PeakUnrealizedJPY` = MFE / `TroughUnrealizedJPY` = MAE・いずれも**円/株**)を持つ。引け後パケットが「その建玉がその日どこにいたか」を持てるようにするため —— 台帳の極値は時系列ではないので、**当日に記録しないと経路が永久に失われる**([usecase.md](usecase.md) の `JournalOpenView`)。

### 往復台帳: `TradeRepository` / `PositionCloser`

- `PositionCloser.CloseAndRecord(ctx, positionID, closedAt, trade)` — 建玉を CLOSED に倒し trade を記録するのを **1 トランザクションで atomic** に行う (close saga)。`(ok bool, err error)` で冪等性を担保。
- `TradeRecord.ProfitLossJPY` は **GROSS のまま維持**。net = gross − FeeJPY + CarryJPY は downstream で導出する (`ProfitLossJPY` のコメント)。`FeeEstimated` は手数料を推定で補完したかのフラグ。
- `TradeRecord.CloseReason` は string enum: `take_profit` / `stop_loss` / `max_hold` / `early_exit` / `ratchet_takeprofit` / `manual` / `reconcile_cold_close` / `broker_close` / `forced_flat` / `entry_compensated`(migration 0011)/ `external_close`(migration 0012)/ `ratchet_giveback_loss`(migration 0013)。**`entry_compensated` と `external_close` の 2 つだけが戦略の出口ではない** — 定数と判定は `port.CloseReasonEntryCompensated` / `port.CloseReasonExternalClose` / `port.IsNonStrategyClose` / `port.CountsTowardEntryGates` が正本(文字列を各層に散らすとずれても全部緑で通る)。`external_close` だけは **per-symbol の再入場ゲート(連敗 / cooldown / 窓の取引回数)から外す** — 人間の勝ちで bot の安全ゲートが解除されるため。日次損失の合計は両方数える(委託保証金・維持率には人間の損失も効く)。**値を増やすときは `trades.close_reason` の CHECK 制約 migration も必ず同時拡張** (正本は [DATA_MODEL.md](../../runtime/DATA_MODEL.md))。
  - 定数は `port.CloseReasonStopLoss` / `CloseReasonTakeProfit` / `CloseReasonBrokerClose` も正本。前 2 つの値は `domain/position` の `ExitDecision.Reason` と同じで、**bot が出した決済と broker 側の守りの約定で同じ理由を書く**(同じ出口が経路によって別の名前で載ると戦略別の集計が経路の分だけ割れる)。`broker_close` は **broker 側で決済されたが凍結 TP/SL のどちら側でもなかった**往復。`reconcile_cold_close` と分ける — あちらは**値段すら観測できず時価で近似した**という記録の質のラベルで、出口の種類ではない。
- `RatchetCloseReason(reason, grossJPY)` はトレール決済のラベルを **realized gross の符号**で `ratchet_takeprofit` / `ratchet_giveback_loss` に分ける(損失の往復に「利確」のラベルを付けないため)。**「決済値 vs 建値」の価格比較で分類しない** — SELL 建玉で逆になる。**どちらも戦略の出口**なので `IsNonStrategyClose` には足さない。呼ぶのは台帳へ書く直前(`closeExecutor.closeOne`)だけで、**判定時の時価ではなく約定値**から分類する。
  - `entry_compensated`(migration 0011)は **約定した後に守りを board に置けず entry saga が巻き戻した往復**。**戦略の出口ではない**ので**エッジ判定からだけ**除外する(`port.IsNonStrategyClose`。SQL 側(`pg.PairRepo`)は同じ集合を `port.NonStrategyCloseReasons()` でパラメータとして受け取る)。ダッシュボードの戦績と引け後パケットは**口座ベース**で、普通のトレードとして戦略に計上する(別枠に逃がすと全件がそれだった日に戦績が空に見える)。これを書かないと再入場を止めるゲート(cooldown / 窓の取引回数 / 日次損失 / 連敗 / ナンピン禁止)が**全部が台帳を読む**ゆえに同時に盲目化する。

#### per-symbol / account-wide 集計分離 (multi-symbol risk gate)

`TradeRepository` は損失合計・件数・連敗の集計を **per-symbol 系 (`*BySymbol`) と account-wide 系の 2 経路**で持つ:

| 用途 | per-symbol | account-wide |
|---|---|---|
| 損失合計 (JPY) | `SumClosedLossJPYSinceBySymbol(ctx, symbol, since)` | `SumClosedLossJPYSince(ctx, since)` |
| 件数 | `CountTradesSinceBySymbol(ctx, symbol, since)` | — |
| 連敗 | `ConsecutiveLossesBySymbol(ctx, symbol)` | — |

`*BySymbol` family は **per-symbol gate が sibling symbol の trade で汚染されない** ように分けている。複数 symbol を抱えた瞬間に account-wide method を per-symbol cap に流用すると「他 symbol の負けが自 symbol の cap を誤発火させる」regression になるため、意図的に分離する。

#### 決済直後のクールダウン: `LastCloseBySymbol`

- `LastCloseBySymbol(ctx, symbol) (*LastClose, error)` — その銘柄の**直近の決済**(`ClosedAt` と net = gross − fee + carry)。`nil` = 決済履歴なし。
- 用途は `usecase/command.SnapshotBuilder` の cooldown 判定ひとつ: 直近が**負けなら `after_loss`**、**勝ちなら `after_take_profit`** の待ち時間を `AccountSnapshot.InCooldown` に載せ、risk gate が新規 entry を弾く。損切りと利確で待ち時間を分けるのは、負けは「そのセットアップが壊れた」合図だが勝ちは壊れていないため。
- **なぜ port に足したか**: cooldown は `hard_limits.cooldown.*` と `risk.AccountSnapshot.InCooldown` の両方に存在したのに、設定する経路が無ければ常に false になる。損切りした次の秒に同じ銘柄へ再エントリーすると、チャーンのコストに加え、相関した再エントリーが**独立標本として数えられ** audition の統計を歪める。
- 実装は in-memory / pg 両方。`NetJPY` の式は台帳と同じ規約(`ProfitLossJPY - FeeJPY + CarryJPY`)で、**変えるときは両実装と `query.BuildForwardReport` を同時に**。

#### forward 台帳の読み出し: `ListClosedSince`

- `ListClosedSince` だけを持つ **`ClosedTradeReader`** に切り出してあり、`TradeRepository` はそれを埋め込む。戦績の集計(`query.BuildForwardReport`)はこの狭い口しか受け取らない — 複数 DB の合算(`repository.MergedTrades`)が書込系を持たずに満たせるようにするため。

- `ListClosedSince(ctx, since)` — closed trade を **closed_at 昇順**(同時刻は id 順)で列挙する read 側。risk gate は使わず、forward 検証台帳の読み出し(`usecase/query.BuildForwardReport` → `cmd/forward-report`)専用。in-memory / pg 両実装は並び順を揃える (keep in sync)。since はゼロ値で全期間。

#### 戦略別分計: `TradeStrategyResolver`

- `StrategyByPositionID(ctx, positionIDs)` — position を**建てた**戦略名を解決する read 専用ポート。正は建玉に凍結した `positions.strategy_name` で、空の行(external・migration 0015 前の旧建玉)だけ `positions.config_id` → `strategy_configs.strategy_name` の join に落とす。advisor 経路は複数戦略を建てるため、forward 検証の「どの戦略が損益を出したか」はこの分計でしか判定できない(`usecase/query.BuildForwardReport` の `WithStrategyResolver` / `cmd/forward-report -strategy`)。実装は Postgres の `PositionRepo` のみ(in-memory 構成は forward 記録自体が揮発するため提供しない)。どちらも引けない position は `"unknown"`(隠すより見せる)。

#### score 復元: `TradeScoreResolver`

- `ScoreByPositionID(ctx, positionIDs) → map[positionID]TradeScore{Score, RunID}` — 「エントリー時にその戦略の screener が出していた score」を `advisor_runs.input_json` の `advisory.screens[]` から復元する read 専用ポート(`usecase/query.BuildForwardReport` の `WithScoreResolver`)。**復元できなかった position は map に現れない** — 呼び出し側が「復元不能 N 件」を明示し、黙って落とさない。
- 実装は Postgres の `ScoreRepo` のみ([pg/score_repo.go](../../../backend/internal/adapter/repository/pg/score_repo.go))。参照解決は `strategy_configs.advisor_run_id`(migration 0007)直参照 → 無い行のみ「±60秒に success run がちょうど1件」の時刻 fallback(曖昧は復元不能 — 誤リンクはリンク無しより悪い)。復元ロジックはこの1関数に閉じる。

### 戦略 config: `StrategyConfigRecord`

- pg の `ConfigRepo`([pg/config_repo.go](../../../backend/internal/adapter/repository/pg/config_repo.go))は書き込み(`ActivateExclusive` / `EnsureExists`)だけを持つ。本番に読み手は無いので読み取りメソッドは置かない。`mode` は string (R1)。
- `StrategyConfigRecord.AdvisorRunID`(migration 0007)— この config を書いた `advisor_runs.run_id` への参照。runtime(`AdvisorCycle`)だけが刻み、LLM の YAML からは注入不可(`config.StrategyConfig` 側は `yaml:"-"`)。人手/テスト/sentinel config は空 = NULL 保存。既存値は上書きしない(config 凍結 — pg 側は `COALESCE`)。
- **現状の config SoT は YAML**(`configs/` の outbox を起動時に読む)。DB の `strategy_configs` は建玉の FK と監査のための記録。
- **DB 側の config promote(旧 active を expire し新 config を atomic に入れる)は未配線**。以前あった `StrategyConfigPromoter` interface は phantom だったため削除済みで、読み側の `StrategyConfigRepository` interface も本番呼び出しが無いので置かない(port には record 型だけが残る)。DB を SoT にする段階で 1-Tx の pg 実装と共に port を再導入する。

### Candle store: `CandleRepository`

`Upsert(ctx, symbol, candles)` / `List(ctx, symbol, period, limit)` で backtest/replay 経路の candle を永続化する。`period` は port が定義する `KlinePeriod` enum (`Period1m` / `Period5m` / `Period1h` / `PeriodDaily`)。

### 拒否監査: `SignalRejectionRepository`

- `InsertRejection(ctx, SignalRejection{Symbol, ConfigID, Reason, Detail, CreatedAt})` — gate/cycle が reject した entry を signal_rejections に追記する(「なぜエントリーしなかったか」の再起動を跨ぐ trail・migration 0009)。`Reason` は GROUP BY できる安定種別(gate reason の先頭トークン)、可変部(時刻・数量)は `Detail`。エッジ記録(同一 symbol × 種別 × config × JST日 の連続は書かない)は**挿入側**(`TradingCycle`)の責務で、repo は言われたものを書くだけ。
- **observability 専用・best-effort**: 呼び出し元(`TradingCycle.recordRejection`)は insert 失敗を握って続行してよい**唯一の**repository 経路(取引状態を持たないため。FAILURE_MODES の "log+continue 禁止" は取引状態が対象)。
- `CreatedAt` は caller(`EvalInput.Now`)が渡す(port の決定論性)。実装は in-memory ([rejection_repo.go](../../../backend/internal/adapter/repository/rejection_repo.go)) と pg ([pg/rejection_repo.go](../../../backend/internal/adapter/repository/pg/rejection_repo.go)、空 ConfigID は NULL)。

### screen 監査: `ScreenSnapshotRepository`(migration 0008)

- `InsertRound(ctx, []ScreenSnapshot{RoundAt, Symbol, Strategy, Triggered, Score, Picked})` — LLM ラウンドごとの**全銘柄 × 全スクリーナー**の screen 結果を一括追記する(`AdvisorLoop.persistScreenSnapshots`)。`Picked` = そのラウンドで per-strategy round-robin の枠を得たか。advisor_runs には選ばれた銘柄しか残らないため、「枠に入らなかった候補のその後」= 枠配分ロジック自体の検証はこの記録だけが可能にする。
- **observability 専用・best-effort**(SignalRejection と同じ免除): 書込失敗はログのみでラウンドを止めない。取引経路はこのテーブルを読まない。実装は in-memory ([screen_snapshot_repo.go](../../../backend/internal/adapter/repository/screen_snapshot_repo.go)) と pg(COPY 一括・[pg/screen_snapshot_repo.go](../../../backend/internal/adapter/repository/pg/screen_snapshot_repo.go))。

### Broker interface (`Broker` / `LiveBroker`)

`Broker` は証券会社非依存の共通 interface で、paper / 立花 が満たす:
`GetTicker` / `GetKlines` / `GetAccountMargin` / `GetPositions` / `GetActiveOrders` / `GetExecutions` / `PlaceOrder` / `ClosePosition` / `CancelOrder` / `RefreshToken`。
`RefreshToken` は再認証 (立花の日次 API 閉局 (03:30〜) 対策。立花は実 logout+login)。paper は no-op で実装する。

`MarketFeed` は `Broker` から発注系を除いた **read-only 断面** (`GetTicker` / `GetKlines` / `RefreshToken`)。paper_live_feed(実フィード × 紙執行)の価格ソースはこの型で受け取る — 発注メソッドを持たないので**型として実弾を送れない**のが安全担保。

`ErrOrderNotFilled` は `ResolveExecution` の sentinel error: 注文は板で working と観測できたが期限内に**約定しなかった** (ストップ高/安・売買停止・薄商い) = confirmed zero-fill。entry saga は clean abort として扱う (残注文 cancel のみ。fill が無いので補償 close も emergency trip も不要)。

**2 段階の保護 exit** は `Broker` 本体から分離した 3 つの小 interface に置く:

| interface | method | 役割 |
|---|---|---|
| `ExecutionResolver` | `ResolveExecution(ctx, orderID) (ResolvedExecution, error)` | MARKET 注文の約定 (price/fee/broker position id) を fill するまで poll |
| `SettleFillReporter` | `SettleFillsAsync() bool` | **決済の約定値が同期に返るか**の宣言。true(立花)なら呼び手は `ClosePosition` の受理を約定と読まず `ResolveExecution` で確かめてから台帳に書く(幽霊決済の禁止)。false(paper)は即約定。**`LiveBroker` の一員にしてあるのは意図的** — ラッパ(`LiveQuoteShared`)が転送を忘れると usecase の型アサーションが黙って外れ、live の決済が丸ごと旧経路へ落ちる。interface の一員なら転送漏れはコンパイルエラー | `CloseResult.FilledPrice`=0 の読み方も同じ分岐: 同期 adapter(paper)は観測価格へ fallback、非同期(立花)は `ResolveExecution` で確認できるまで書かない。
| `OCOCloseOrderPlacer` | `PlaceSettleOCO(ctx, OCOCloseOrderInput) (rootOrderID, err)` | broker 側に TP+SL を置き、**bot 死でも守りが残る**ようにする |
| `SettleLegResolver` | `ResolveSettleLegs(ctx, brokerPositionID, symbol) (tpOrderID, slOrderID, err)` | `PlaceSettleOCO` 後に TP/SL 脚の order id を解決 |

| `ProtectiveOrderBoard` | `ListProtectiveOrders(ctx, symbol) ([]ProtectiveOrderInfo, error)` | **broker 側の守りの照会と取消**(寄り前の置き直しと closeOne の土台)。立花の `sOrderExpireDay` は最大 10 営業日なので、これが無いと多日保有は 9 営業日目に SL を失い、再設置経路も無い。期日を延ばす手段は取消 → 同条件の再発注だけ(訂正では発注日起点の天井を超えられない)。取消と再発注の間は守りが消えるので、`ReplaceProtectiveOrder` は場外でしか撃たない。**`LiveBroker` の一員にしてあるのは `SettleFillReporter` と同じ理由** — ラッパの転送漏れがコンパイルエラーになる |

`ProtectiveOrderInfo` は板に残っている決済注文 1 本: `OrderID` / `Symbol` / `ExpireOn`(**zero = 当日限り**。zero を「無期限」と読まない — 多日保有では最も危険な状態)/ `HasStopLeg`(逆指値脚を持つか。利確指値だけの注文は守りではない)/ `Side` `Quantity`(bot の建玉の守りかを絞る)/ `BrokerRef`(broker 固有の追加キー)。

`LiveBroker` は上 6 つの合成 interface (`Broker` + `ExecutionResolver` + `SettleFillReporter` + `OCOCloseOrderPlacer` + `SettleLegResolver` + `ProtectiveOrderBoard`)。実 broker (立花 / paper) とラッパ (`LiveQuoteShared`) はこれを満たす。

> ⚠️ 立花の OCO/逆指値は実機未検証。`PlaceSettleOCO` / `ResolveSettleLegs` はデモ検証後に人間が `STOCKBOT_TACHIBANA_OCO_VERIFIED=1` を立てるまで fail-close ([CLAUDE.md](../../../CLAUDE.md))。

### Advisor / Notifier

- `Advisor.Generate(ctx, summary) (*AdvisorRun, error)` — LLM が `StrategyConfig` YAML を生成する **既定 OFF** の advisor。`AdvisorRun{RunID, StartedAt, FinishedAt, InputJSON, OutputYAML, ParsedYAML, Status, ErrorMsg, Model, UsageLimited}` を返し、`Status.Promotable()` は **success のみ true**(他は全て fail-close で昇格スキップ)。advisor は**注文を出さず、リアルタイム発注経路に乗らない** — config を吐くだけで、検証は `usecase/command.Promoter`、執行は決定論エンジン([CLAUDE.md](../../../CLAUDE.md) のエッジ規律)。実装は [adapter/advisor](../../../backend/internal/adapter/advisor/)(Claude CLI)。
- `AdvisorRunRepository.Insert / List` — 全 run(成功・失敗とも)の監査保存(`advisor_runs`・migration 0002)。入力 summary / 生 stdout / parsed / **LLM が述べた理由**(regime type・confidence・reason)を残す。**best-effort**: 書込失敗は log のみで取引を止めない。実装は in-memory と pg の 2 つ。
- `Notifier.Notify(ctx, level, title, message) error` — emergency trip / forced-flatten 失敗等の運用アラート配信。best-effort で、失敗しても取引経路を block しない。実装は [adapter/notifier/stdout.go](../../../backend/internal/adapter/notifier/stdout.go)(`Stdout`)。

---

### 株式分割

- `PositionRepository.ApplySplit(ctx, adj)` — `position.SplitAdjusted` の結果を書く。CAS は **OPEN かつその日にまだ言い直していない**行だけ。
- `SplitAdjustableBook`(broker.go)— 建玉帳をプロセス内に持つ broker(paper)だけが実装する。実ブローカーは実装しない
  (live は建玉照会で**確かめる**側)。
- `CloseReasonSplitMisfire = "split_misfire"`(migration 0022)— 分割を損切り/利確と誤読して決済した往復。`NonStrategyCloseReasons` に入る。

## テスト方法 (この層特有)

interface 自体には実装がないので **直接テストしない**。代わりに:

1. usecase テストで fake 実装を作るときに interface を満たしているか go compile が検証する
2. adapter 実装テスト (in-memory / integration) で同じ interface を満たす adapter が正しく動くか検証する

実装側には **compile-time assert** を置く (慣習: `var _ port.X = (*adapter.Y)(nil)`):

```go
var _ port.PositionRepository = (*PositionRepo)(nil)   // pg/position_repo.go
var _ port.LiveBroker         = (*Paper)(nil)          // broker/paper.go
var _ port.LiveBroker         = (*Tachibana)(nil)      // broker/tachibana.go
var _ port.Notifier           = (*Stdout)(nil)         // notifier/stdout.go
var _ port.Advisor            = (*ClaudeCLIAdvisor)(nil) // advisor/claude_cli.go
```

---

## 既存実装の代表例

- [backend/internal/port/repository.go](../../../backend/internal/port/repository.go) — 全 Repository interface + 境界 record (`PositionInsertInput`, `TradeRecord`, `LastClose`, `StrategyConfigRecord` 等)
- [backend/internal/port/broker.go](../../../backend/internal/port/broker.go) — `Broker` / `LiveBroker` interface (paper / 立花 共通) + `KlinePeriod` enum
- [backend/internal/port/advisor.go](../../../backend/internal/port/advisor.go) — `Advisor` interface (既定 OFF。実装: [adapter/advisor](../../../backend/internal/adapter/advisor/)(Claude CLI))
- [backend/internal/port/notifier.go](../../../backend/internal/port/notifier.go) — `Notifier` interface (実装: [adapter/notifier/stdout.go](../../../backend/internal/adapter/notifier/stdout.go))
- [backend/internal/port/pending_positions.go](../../../backend/internal/port/pending_positions.go) — `PendingPositionTracker` (実装は [safety/pending_positions.go](../../../backend/internal/safety/pending_positions.go))
- adapter 側: [inmemory.go](../../../backend/internal/adapter/repository/inmemory.go) (paper/test/backtest 用) / [pg/](../../../backend/internal/adapter/repository/pg/) (本番 Postgres)

---

## アンチパターン (= やってはいけない失敗)

- Repository record に ORM tag (`gorm.Model` / `bun` tag 等) を埋め込む — adapter 側に閉じ込める
- Domain entity を境界 record としてそのまま再利用する — Domain ↔ DB/Broker を別 record で分離する
- `port.Broker` に証券会社特有のメソッド (立花 固有) を追加する — adapter のメソッドとして閉じ込める
- Repository が内部で `time.Now()` を呼ぶ (= 境界の決定論性を破壊) — caller が `now`/`closedAt`/`claimedAt` を渡す
- `port.StrategyConfigRecord.Mode` を `config.Mode` 型にする (= R1 依存方向違反) — `string` で据え置く

---

## 関連 docs

- [../PR_CHECKLIST.md](../PR_CHECKLIST.md) — 層規約 / 不変条件チェック
- [DATA_MODEL.md](../../runtime/DATA_MODEL.md) — `TradeRecord.CloseReason` 等の値の正本
- [../../../CLAUDE.md](../../../CLAUDE.md) — 層規約 (R1 / domain 純粋性) とトレード不変条件
- [adapter.md](adapter.md) / [usecase.md](usecase.md) — port を実装する側 / 依存する側
