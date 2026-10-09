# Failure Modes — Rollback / Compensate / Trip / DEFER

エラー発生時の挙動の絶対ルール。本書は [CLAUDE.md](../../CLAUDE.md) のトレード不変条件を
障害シナリオ視点で具体化したもの。trip 経路の詳細は [layers/safety.md](layers/safety.md) を参照。

**ルール: 失敗時は "log + continue" ではなく "rollback / compensate / emergency_stop trip / DEFER"。**

> 4 つの正式カテゴリ:
> - **Rollback** — 途中で作った状態を巻き戻す(`ClaimForClose` CAS の取り消しは不要だが、DB Tx は `CloseAndRecord` が一括 commit/rollback)。
> - **Compensate** — broker 側を巻き戻せないので、約定済みの裸ポジを反対売買で閉じる(entry saga の補償)。
> - **emergency_stop trip** — ファイルフラグを write-once で立て、新規 entry を全停止。再開は人間のみ。
> - **DEFER** — 今は解決できないが状態も壊さない。grace/hard window 付きで次パスに再試行し、hard window 超過で初めて trip(reconcile の stale DB position)。
>
> DEFER は "log+continue" と異なり ①状態を破壊しない ②上限(hard window)があり最終的に trip する ③`ReconcileReport.Deferred` でカウントする。無条件の握り潰しは禁止。

---

## 1. Rollback / Compensate > Fallback の原則

エラー発生時:

- **悪い**: log して continue / 不完全な状態(裸ポジ・0 円トレード・片付かない CLOSING)のまま先に進む
- **良い**: 補償で裸ポジを閉じる / DB を書かずに `CLOSING` のまま残す / emergency_stop を発火する

### 具体例

| 状況 | 正しい挙動(stock-bot 実装) |
|---|---|
| Entry saga で fill 後の OCO 発注失敗 | `compensate()` が反対売買で裸ポジを閉じ、**その往復を台帳に書く**(`close_reason='entry_compensated'`・migration 0011。決済の約定が確認できなければ `CLOSING` のまま reconcile へ)。加えて **その銘柄はその営業日もう建てない**(`ExecuteOrder.EntryBlocked`)([execute_order.go](../../backend/internal/usecase/command/execute_order.go) `e.compensate`) |
| Entry saga で `ResolveSettleLegs` 失敗 | 同上、`compensate()` で close |
| **external 建玉が broker から消えた** | `MarkClosed` だけでなく **trade を書く**(`close_reason='external_close'`・migration 0012)。戦略成績ではないが口座には実額として効くので台帳から消さない。価格が観測できなければ trade は書かず建玉だけ閉じる(PnL 捏造禁止) |
| **守りの価格が当日の値幅制限の外** | **SL が帯の外なら建てない**(`risk.EvaluatePriceLimit` → `stop_loss_outside_price_limit`)。**TP が帯の外なら TP 脚だけ落として SL を置く**(`risk.ProtectiveTakeProfitPlaceable`)。TP は台帳に残り `OnTick` が引き継ぐ。**多日建玉は帯の内外に依らず stop-only**(`risk.ProtectiveTakeProfitOnBoard`): 立花は繰越時に翌日の帯で再検査し、TP 脚が外だと SL ごと失効させる |
| Entry saga で broker 成功 + `posRepo.Insert` 失敗 | 同上、`compensate()` で裸ポジを閉じる(broker と DB を一致させる) |
| **`compensate()` の close 自体が失敗** | **emergency_stop trip**(= `compensating_close_failed:<bpID>`)。守りなしの裸ポジが broker に残り saga 内で回収不能なため止血する(silent fail-open 禁止) |
| **`PlaceOrder` が transport エラー(到達不明)** | 「届かなかった」と**仮定しない**。`recoverUnconfirmedSubmit` が板と建玉を照会: 一致する working order は cancel、DB 未知の同 side 建玉は `compensate()` で close。**ただし発注数量より大きい建玉は閉じない**(立花は信用建玉を銘柄単位に集約するので、丸ごと閉じると人間の手動建玉まで売る)→ **trip**。**照会自体が失敗しても trip**(= `entry_submit_unconfirmed:<symbol>`) |
| **Partial fill(15s deadline で一部約定)** | **約定数量が正**: 残注文を cancel し、OCO も DB 凍結も `FilledQuantity` で行う(発注数量で OCO を置くとドテン売りになる)。残注文の cancel が確定できず再 resolve でも fill が確定しないなら、約定分を `compensate()` で close して **trip**(= `partial_fill_residual_uncancelled:<symbol>`)— 数量を推測して OCO を置かない([execute_order.go](../../backend/internal/usecase/command/execute_order.go)) |
| bot 発の決済(close saga) | claim(CAS)→ **保護レッグ(TP/SL)を cancel** → broker close → 記帳の順。働いている保護注文が建玉数量を拘束するため、cancel を飛ばすと返済が拒否される([exit_executor.go](../../backend/internal/usecase/command/exit_executor.go) `cancelProtectiveLegs`) |
| close 時に `ClaimForClose` が ok=false | benign skip(既に CLOSING/CLOSED の他経路が処理中)。再 close しない([exit_executor.go](../../backend/internal/usecase/command/exit_executor.go)) |
| close が **accepted だが約定を確認できず、板にも決済注文が無い** | 保護レッグは cancel 済みなので建玉は**完全に裸**。**trip**(= `close_unfilled_unprotected:<bpID>`)して `CLOSING` のまま残す。板に決済注文が**残っている**なら trip しない(未約定の決済を残すのは意図的 — ストップ安の引けは比例配分で、板の売り注文にしか配分されない)([exit_executor.go](../../backend/internal/usecase/command/exit_executor.go)) |
| close 時に `ClosePosition` が err / 未 accept | **まず建玉照会で「broker がまだこの建玉を持っているか」を確かめる**。**持っていない = 裸ではなく決済済み**(板の逆指値が先に約定した)なので trip せず、`GetExecutions` の**実約定**で台帳を締める。実約定が取れなければ `CLOSING` のまま reconcile へ渡す(やはり trip しない)。照会そのものが落ちた回は確かめられていないので従来どおり下の枝(fail-close)。**持っている**なら保護レッグは cancel 済みのため**まず trip**(= `close_rejected_unprotected:<bpID>`。**自動の出口(ForceFlatten / ManageOpenPositions)のみ** — `CloseAllOpen` 経由(`/api/flatten-all`・`/api/positions/close`・`/api/live/positions/close` = 画面の「成行決済」)は「帳簿締めの拒否で bot を止めない」事前コミットにより trip しない)した上で `CLOSING` のまま残す → **reconcile が回収**: broker がまだ持っていれば grace(90s)後に MARKET close を**再発行**、hard window(10 分)超で trip(= `close_stuck_unresolved:<bpID>`) |
| close 価格が 0 円(observed も ticker も取れない) | **0 円トレードを記録しない**。`CLOSING` のまま残して reconcile に委ねる(`closePrice <= 0` 分岐) |
| broker からポジが消えた(OCO 約定 / bot 外 close) | reconcile が grace(90s)内は DEFER、grace 超で記帳。**まず `GetExecutions` の実約定を探す**: 決済側の約定が建玉数量とぴったり一致したらその**約定値・実手数料**で締め、理由は凍結した守りのどちら側で約定したかで `stop_loss` / `take_profit`、どちらでもなければ `broker_close`(`fee_estimated=false`)。**一致しなければ**従来どおり観測価格の `reconcile_cold_close`(近似であることをラベルで残す)。価格が取れないまま hard window 超なら trip(= `reconcile_stale_position_unresolved:<id>`) |
| 引け前フラット化で close 失敗 | **emergency_stop trip**(= `forced_liquidation_failed`)([force_flatten.go](../../backend/internal/usecase/command/force_flatten.go)) |
| 維持率割れ検出 | **emergency_stop trip**(= `margin_maintenance_breach`)([manage_open_positions.go](../../backend/internal/usecase/command/manage_open_positions.go)) |
| **broker の margin 照会失敗** | **fail-close**: snapshot が `MarginStatusUnknown` を立て、gate が `margin_status_unavailable` で entry を reject(担保ゲートを黙って無効化しない) |
| 立花の保護 exit(OCO/逆指値)がデモ未検証 | **fail-close**: `STOCKBOT_TACHIBANA_OCO_VERIFIED=1` 未設定なら `PlaceSettleOCO`/`ResolveSettleLegs` がエラーを返し、live entry が守りなしで走るより中断する([tachibana_orders.go](../../backend/internal/adapter/broker/tachibana_orders.go)) |

> **stock-bot 固有の注意**: entry saga(`ExecuteOrder`)の post-fill 失敗は **まず compensate(裸ポジを閉じる)**。
> broker 約定済みなので「閉じて巻き戻す」のが正。ただし **compensate の close 自体が失敗したら trip**
> (= `compensating_close_failed`)。守りなしの裸ポジを silent に放置しないため、Rollback が無理な時点で止血に倒す。

---

## 2. Transaction が必要なケース

複数テーブル更新 / 状態遷移を含むものは Tx 必須(具体実装は Postgres。in-memory リポジトリは同じ port を満たし同じ原子性を保証):

| ケース | Tx 内に閉じる操作 | port |
|---|---|---|
| ポジ決済 | `positions UPDATE status='CLOSED'` + `trades INSERT` を 1 Tx | `PositionCloser.CloseAndRecord`([repository.go](../../backend/internal/port/repository.go)) |
| Config promote | 旧 active を expired に + 新 active を INSERT を 1 Tx | adapter 実装 `ConfigRepo.ActivateExclusive`([pg/config_repo.go](../../backend/internal/adapter/repository/pg/config_repo.go)。advisor arm 経路が使用)。port 契約 `StrategyConfigPromoter` は deferred(YAML が config SoT — [repository.go](../../backend/internal/port/repository.go) の NOTE) |

port 契約としての 1-Tx は `PositionCloser.CloseAndRecord` のみ(config 切替の 1-Tx は adapter 内 `ActivateExclusive`)。

### Tx 内では外部 API(broker)を呼ばない

Tx 中に broker REST を入れると、API 遅延時に DB ロックを長時間保持してしまう。
順序は必ず **broker call → DB Tx**、または **broker call → DB Tx → broker compensation の Saga**。
`ExecuteOrder.Execute` はこの順(PlaceOrder → ResolveExecution → PlaceSettleOCO → `posRepo.Insert`)で、
DB 書込(`Insert`)は全 broker 呼び出しが終わった最後に 1 回だけ行う。

---

## 3. Mutex が必要なケース

複数 goroutine が同じリソースを変更しうる箇所のみ直列化(`domain` の mutex 禁止の例外は `domain/market` のみ):

| Mutex | 直列化する組 | 場所 |
|---|---|---|
| `EmergencyStop.mu` | `Trip` / `Resume` のフラグファイル書込を直列化(write-once を保証) | [safety/emergency.go](../../backend/internal/safety/emergency.go) |
| `Reconcile.mu` | `staleSince`(brokerPositionID → 初回 stale 時刻)/ `closingSince`(positionID → 初回 stuck CLOSING 時刻)の 2 map の read/write | [reconcile.go](../../backend/internal/usecase/command/reconcile.go) |
| pending tracker `sync.RWMutex` | entry saga in-flight の `MarkPending`/`MarkResolved`/`IsPending`(reconcile の誤 adopt 防止) | [safety/pending_positions.go](../../backend/internal/safety/pending_positions.go) |

> double-close 防止は mutex ではなく `PositionRepository.ClaimForClose` の **CAS** で行う(OPEN→CLOSING を 1 回だけ成功させる)。

---

## 4. 各障害シナリオ — bot の反応

| 障害 | 反応 |
|---|---|
| `broker.PlaceOrder` 未 accept(business reject) | error 返却。DB に何も書かない。次 tick で再評価([execute_order.go](../../backend/internal/usecase/command/execute_order.go)) |
| `broker.PlaceOrder` transport エラー(到達不明) | `recoverUnconfirmedSubmit`: working order → cancel、DB 未知の同 side 建玉 → compensate close(**発注数量以下のときだけ**。超える = 他人の玉が混ざっているので trip)。照会不能なら **trip**(= `entry_submit_unconfirmed:<symbol>`) |
| Entry の partial fill(deadline) | 残注文を cancel し、`FilledQuantity` で OCO / DB 凍結(発注数量は使わない)。cancel 未確定 + 再 resolve でも fill が確定しないなら約定分を `compensate()` で close し **trip**(= `partial_fill_residual_uncancelled:<symbol>`) |
| Entry saga `PlaceSettleOCO` 失敗 | `compensate()` で裸ポジを close、error 返却(`守りなし` ポジを残さない) |
| Entry saga `ResolveSettleLegs` 失敗 | `compensate()` で close、error 返却 |
| Entry saga `posRepo.Insert` 失敗 | `compensate()` で close、error 返却(broker と DB の不一致を作らない) |
| Entry saga `compensate()` の close 失敗(err / 未 accept) | **emergency_stop trip**(= `compensating_close_failed:<bpID>`)。裸ポジ放置を止血 |
| 通常 exit で `ClaimForClose` ok=false | benign skip(他経路が処理中)。trip しない |
| 通常 exit で `ClosePosition` 失敗 | **建玉照会で確かめてから**。broker が持っていなければ決済済み = 実約定で記帳・trip しない。持っていれば保護レッグ cancel 済みの裸ポジなので **trip**(= `close_rejected_unprotected:<bpID>`。**自動の出口のみ** — `CloseAllOpen` 経由は trip しない)+ `CLOSING` のまま残す → reconcile が grace 後に MARKET close を再発行、hard 超で **trip**(= `close_stuck_unresolved:<bpID>`) |
| close 価格が 0 円で確定不能 | 0 円トレードを記録せず `CLOSING` のまま残す(PnL を捏造しない)。broker からポジが消えた後の cold close が回収 |
| 維持率が `maintenanceRatio` 割れ | **emergency_stop trip**(= `margin_maintenance_breach`)。OnTick 冒頭で `GetAccountMargin` を確認。**trip は新規停止のみで自動フラット化しない** — 追証リスクの手動削減は人間 |
| `GetAccountMargin` 失敗(entry 経路) | **fail-close**: `MarginStatusUnknown` → gate が `margin_status_unavailable` で reject |
| 引け前 force-flatten で close 失敗 | **emergency_stop trip**(= `forced_liquidation_failed`)。返済できない一日信用ポジを即可視化 |
| reconcile で naked broker position 検出 | entry saga in-flight(id/symbol pending)でなければ **external として adopt**(表示のみ・担保にカウント)。ただし **close 側 working order が無ければ守りなし孤児 → trip**(= `external_adopt_unprotected:<bpID>`)。一日信用の external は引け前フラット化の**対象**(ペナルティは所有者に関係なく発生) |
| reconcile で external ポジが broker から消えた | `MarkClosed`(trade 記帳なし)。放置すると同 symbol のナンピンゲートが永久に塞がるため |
| reconcile で stale DB position(grace 内 ≤90s) | **DEFER**(`ReconcileReport.Deferred++`)。broker 未追従と entry race を区別できないため |
| reconcile で stale DB position(grace 超) | **実約定で記帳**(= `stop_loss` / `take_profit` / `broker_close`、約定値+実手数料)。broker 側 OCO 約定の正規経路で、**これが live の通常の出口**。実約定が引けない場合だけ観測価格の `reconcile_cold_close` |
| reconcile で stale DB position(価格不能のまま hard 超 ≥10m) | **emergency_stop trip**(= `reconcile_stale_position_unresolved:<id>`) |
| 立花 API 閉局(03:30〜)明けの再認証失敗 | **emergency_stop trip**(= `token_refresh_failed`。閉局窓内の失敗では trip しない)([loops.go](../../backend/cmd/stockbot/loops.go)) |
| 立花 保護 exit がデモ未検証 | **fail-close**: `PlaceSettleOCO`/`ResolveSettleLegs` がエラー。live entry が中断(`STOCKBOT_TACHIBANA_OCO_VERIFIED`) |
| 手動 override が hard gate に当たる | **絶対バイパスしない**。`EvaluateHardSafety` が emergency / session / end_of_day / daily_loss / account_daily_loss / margin 不明 / insufficient_margin を再チェック([risk/gate.go](../../backend/internal/domain/risk/gate.go))。`POST /api/positions/extend` も emergency 中は 409 で reject |
| `/api/emergency-stop`(人手) | **emergency_stop trip**(= `manual_api`、reason 省略時の既定)([handler.go](../../backend/internal/app/handler/handler.go)) |
| active config なし | `no_trade` で起動(trip しない)。`EvaluateHardSafety` は entry 時 `no_active_config` で reject([main.go](../../backend/cmd/stockbot/main.go) `loadActiveConfigOrNil`) |
| 停止価格しか無い(前日終値 fallback / 売買停止) | ticker に `Stale` が立ち、**その tick は評価しない**(entry も exit も昨日の価格で動かさない。counters.stale_quotes) |
| live_config で allowlist 外の戦略 | **起動拒否**(hard_limits `live_allowed_strategies`。追加はエッジ証明後の人間 commit のみ) |
| クラッシュ再起動直後 | live/paper とも **loop 開始前に同期 reconcile を 1 周**(broker 建玉を adopt してから entry 判定が動く = ナンピンゲートの 30 秒盲点を塞ぐ。CLAUDE.md は live の必須条件として規定) |
| 株式分割(併合)の権利落ち日 | 決済判定の**前**に `SplitGuard` が「前営業日終値を基準にした値幅制限の外 かつ 単純分割比」で分割を断定し、建玉を株数 ×r・円/株 ÷r で言い直す(損益不変・`split_adjusted_on` で二度割らない)。paper は値段の証拠だけで言い直す。**live は broker の建玉照会の株数・建単価が分割後を示したときだけ**言い直し、確かめられない(照会失敗・株数/建単価の不一致・端数)ならその建玉の自動決済を**その日止めて** **emergency_stop trip**(= `split_unconfirmed:<symbol>`)。板の守りは触らない — 人間が broker の画面で建玉と守りを確かめる。`RearmUnguarded` は broker の株数が台帳と違う建玉に凍結値で守りを置かない(error で人間に回す)。立花の公式: 繰越注文は翌朝バッチ(3:30〜5:30)で「権利落ちのため」失効 / 整数倍の分割は夜間に建玉を株数 ×r・建単価 ÷r へ / **整数倍以外は株数そのまま建単価だけ下げる** → live は株数同じ・建単価 2% 超変化を検出したら言い直さず hold + trip(置き直しもしない)。⚠ 公式どおりに動くかは未実測 |

---

## 5. emergency_stop trip — 名前付け規約

`EmergencyController.Trip(reason, now)` の `reason` 文字列は **grep 可能** に統一:

- 形式は `<scope>_<action>_<state>` の `_` 区切り(grep 一発で経路を特定できる)。
- 現行の reason(コード grep で確認できる全件):
  - `forced_liquidation_failed` — 引け前フラット化失敗(force_flatten)
  - `margin_maintenance_breach` — 維持率割れ(manage_open_positions)
  - `token_refresh_failed` — 立花 API 閉局(03:30〜)明けの再認証失敗(loops)
  - `manual_api` — `/api/emergency-stop` の既定(handler)
  - `reconcile_stale_position_unresolved:<brokerPositionID>` — stale DB position が cold close 不能のまま hard window 超(reconcile。`:` の後に id を付す prefix 形式)
  - `compensating_close_failed:<brokerPositionID>` — entry saga の補償 close が失敗し裸ポジが残った(execute_order)
  - `entry_submit_unconfirmed:<symbol>` — 発注 transport エラー後、板/建玉の照会でも注文の行方を確定できなかった(execute_order)
  - `external_adopt_unprotected:<brokerPositionID>` — adopt した外部ポジに close 側 working order が無い = 守りなし孤児(reconcile)
  - `close_stuck_unresolved:<brokerPositionID>` — CLOSING のまま再クローズも効かず hard window 超(reconcile)
  - `close_rejected_unprotected:<brokerPositionID>` — bot 発の決済で保護レッグを cancel 済みなのに ClosePosition が拒否され、**かつ建玉照会で broker がまだその建玉を持っていること**を確認した(= 保護なしの裸ポジ)(exit_executor)。**持っていない場合は鳴らさない** — 板の逆指値が先に約定して建玉が消えただけのことがあり、「裸」と「決済済み」は正反対。拒否の文面では判定しない(同じ文面は建玉の指定違いでも出る)。**自動の出口のみ** — `CloseAllOpen`(画面の「成行決済」/ flatten-all)は帳簿締めの事前コミットにより拒否では trip しない
  - `close_resting_too_long:<brokerPositionID>` — CLOSING の建玉の決済側に注文が見え続けたまま上限(既定 8h)を超えた(reconcile)。守りは cancel 済みで、見えている注文が**自分のものである保証は無い**(決済注文 id を持たないので人間の注文と区別できない)。上限が無いと「裸のまま静かに滞留する」状態になる
  - `close_rejected_cancel_unconfirmed:<brokerPositionID>` — 守りの取消を**1 本でも確認できていない**まま返済も拒否された(exit_executor)。`close_rejected_unprotected` と分けるのは**運用の次の一手が正反対**だから: 裸なら守りを置き直す / 取消未確認なら板に守りが残っている公算が高く、置き直すと二重になって返済可能数量を超え**両方**弾かれうる。人間はまず板を見る
  - `close_unfilled_unprotected:<brokerPositionID>` — 保護レッグを cancel した後、決済が **accepted なのに約定も板への常駐もしない**(= 逆指値も決済注文も無い裸ポジ)(exit_executor)。板に決済注文が残っていれば trip しない。**`CloseAllOpen` 経由でも鳴る**(裸は帳簿締めの事前コミットの対象外)。research は警報を配線しないので鳴らない
  - `partial_fill_residual_uncancelled:<symbol>` — 部分約定の残注文が cancel も再 resolve でも確定できない(= 追跡不能な裸の残玉になりうる)(execute_order)
  - `split_unconfirmed:<symbol>` — 株式分割(併合)の権利落ちと値段から断定したが、**broker の建玉照会で分割後の株数・建単価を確かめられなかった**(照会失敗 / 不一致 / 端数)(split_guard)。その建玉の自動決済はその日止まる(分割前の SL・株数で決済すると一部だけ売れて残りが裸になる)。**live だけ**(paper は台帳が正本なので trip しない)

新しい trip 経路を追加するときは:

1. 上記命名規約に従う(`<scope>_<action>_<state>`、id 付きは `<reason>:<id>`)。
2. [layers/safety.md](layers/safety.md) の trip 経路リストを更新する。
3. テストで trip 後に `Active() == true` を assert する(strict TDD、Red→Green→Refactor)。

---

## 6. アンチパターン

- `if err != nil { log.Warn(...) }` だけで続行(= silent fail-open)。
- entry saga の post-fill 失敗を補償せず error だけ返す(= broker に裸ポジを残す)。
- close 価格 0 円のまま `CloseAndRecord` して 0 円トレードを記録(= PnL 捏造)。
- Tx 中で `broker.PlaceOrder` / broker REST を呼ぶ(API 遅延で DB ロック長期保持)。
- stale DB position を grace なしで即 trip(= entry saga の race-window を誤検知)。
- 手動 override で `EvaluateHardSafety` をスキップ(= daily_loss / emergency / session を抜ける)。
- `ClaimForClose` を経ずに直接 close(= double-close)。double-close 防止は CAS が唯一の門。
- 立花 OCO/逆指値をデモ未検証のまま `STOCKBOT_TACHIBANA_OCO_VERIFIED=1` を立てて live entry(= 守りなし発注)。

---

## 7. 関連 docs

- [CLAUDE.md](../../CLAUDE.md) — 不変条件の SoT(本書はその障害視点の具体化)
- [layers/safety.md](layers/safety.md) — emergency_stop trip 経路の詳細
- [layers/port.md](layers/port.md) — `PositionCloser` など port 契約
- [PR_CHECKLIST.md](PR_CHECKLIST.md) — 毎 PR の不変条件チェック
- [../workflows/TESTING.md](../workflows/TESTING.md) — trip / 補償経路のテスト方法
- [../runtime/STATE_MACHINE.md](../runtime/STATE_MACHINE.md) — OPEN → CLOSING → CLOSED 遷移
