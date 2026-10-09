# Layer: Safety

## 役割 (1 行)

file ベースの emergency_stop と entry saga 進行中ポジションの in-memory tracker を持つ **cross-cutting** なリーフ層。上位レイヤ (domain / usecase / handler / adapter) を**一切 import しない**。

---

## やること (do)

- emergency_stop flag ファイルの `Active` / `Trip` / `Resume` / `Reason` を提供する (`emergency.go` の `EmergencyStop`)。
- `Trip` は **write-once / 冪等**: 既に flag があれば書き換えず `nil` を返し、**最初の trip 理由を保持**する (`os.Stat` で存在確認 → 既存なら no-op)。onTrip hook は trip ごとに 1 回だけ発火する。
- `Trip` は **まず in-memory フラグを立ててから** flag file を書く。file 書込が失敗しても稼働中プロセスは `Active()==true` で確実に halt する(flag file は再起動跨ぎの永続化用、in-memory はプロセス内の即時停止用)。書込失敗は握り潰さず `OnTripError` callback で surface し、error も返す。→ 「自動 Trip のエラー握り潰しで emergency が沈黙 no-op になる」欠陥を塞ぐ。
- `Active()` は **in-memory フラグ または flag file 存在**のいずれかで true(前者は当該プロセスの trip、後者は前回起動から引き継いだ trip)。
- `Preflight()` は起動時に flag file の親ディレクトリを作成し書込可能性を検証する。不可なら error を返し、wiring (main) は **起動を拒否**する(fail-close: emergency が engage できない状態で走り出さない)。既に flag がある(前回 trip)場合は触らない。
- `OnTripError(fn)` は trip の**永続化失敗**時に発火する callback を登録する(in-memory では halt 済み)。wiring は ERROR ログ + counter に配線する。
- `Resume` は **人間専用** 操作 (flag ファイル削除 + in-memory フラグ解除)。bot は `Trip` しか書かない。存在しない flag の削除は error にしない。
- entry saga 進行中の broker position id を保持する in-memory tracker (`pending_positions.go` の `PendingPositions`) を提供し、reconcile が in-flight entry を naked external position と誤認するのを防ぐ。`port.PendingPositionTracker` を満たす。
- tracker は **2 軸**を持つ。id 軸(`MarkPending` / `IsPending`)だけでは **PlaceOrder 前の窓が塞げない** — その時点で broker position id はまだ存在しないため、発注〜約定解決の間に reconcile が走ると約定直後の建玉を external と誤認する。そこで **PlaceOrder より前**に symbol 軸(`MarkPendingSymbol` / `IsPendingSymbol`)で印を付ける。
- symbol 軸は **boolean ではなくカウンタ**(`symbols map[string]int`)。同一銘柄で saga が同時に走ったとき、片方の完了が他方の印を消してしまうのを防ぐため、`MarkResolvedSymbol` は 1 より大きければデクリメントし、1 のときだけ delete する。

---

## やらないこと (don't)

- ビジネス判定 (TP / SL / risk gate / 維持率の閾値判定) を持つ — それは usecase / domain の責務。
- DB 接続 / HTTP / goroutine を持つ。
- 上位レイヤ (`usecase` / `handler` / `adapter` / `domain`) を import する。
- usecase 側から `safety` パッケージを直接 import する — usecase は **ローカル interface** (`command.EmergencyController` / `query.EmergencyReader`) 経由で参照する (下記参照)。

---

## 命名 / 配置

| 種別 | 場所 | ファイル名 | 例 |
|---|---|---|---|
| Emergency stop | `backend/internal/safety/` | `emergency.go` | `EmergencyStop`, `NewEmergencyStop`, `Active`, `Trip`, `Resume`, `Reason`, `Preflight`, `OnTripError` |
| Pending position tracker | `backend/internal/safety/` | `pending_positions.go` | `PendingPositions`, `NewPendingPositions`, `MarkPending`, `MarkResolved`, `IsPending`, `MarkPendingSymbol`, `MarkResolvedSymbol`, `IsPendingSymbol` |

### Magic number / 定数集約のルール

- timeout / 運用デフォルト値 (例: stale-position の grace / hard window) は、現状それを所有する usecase 側 (`reconcile.go` の `defaultStaleGracePeriod` / `defaultStaleHardTripPeriod`) に置かれている。`safety` 配下に専用の集約ファイル (例 `timeouts.go`) は現時点で**存在しない**(未定: 定数が増えた段階で `safety/timeouts.go` に寄せるか)。
- 呼値 tick / 価格計算式は domain の責務 (`safety` には置かない)。
- 新規追加するときは **1 ファイル 1 アグリゲートの原則** を守る。

### usecase からの参照経路 (層規約の要)

`safety.EmergencyStop` は具体型のまま usecase に渡されるが、usecase は具体型に依存せず**自分のローカル interface**だけを見る:

- [command/exit_executor.go](../../../backend/internal/usecase/command/exit_executor.go) の `EmergencyController` (`Active() bool` / `Trip(reason string, now time.Time) error`) — `ForceFlatten` / `ManageOpenPositions` / `Reconcile` / `SnapshotBuilder` が consume する。
- [query/get_bot_status.go](../../../backend/internal/usecase/query/get_bot_status.go) の `EmergencyReader` (`Active() bool` / `Reason() string`) — read-only の status view が consume する。

`*safety.EmergencyStop` が両 interface を満たすので、wiring (main) でのみ具体型が現れ、usecase は `safety` を import しない。

---

## テスト方法 (この層特有)

- emergency_stop: `t.TempDir()` 上に flag file を作って実 file で `Active` / `Trip` / `Resume` / `Reason` を検証する。trip の **write-once / 冪等** (2 回目の `Trip` で理由が上書きされない・onTrip が再発火しない) と、`Resume` 後の `Active()==false` を確認する。
- pending tracker: `MarkPending` → `IsPending==true` → `MarkResolved` → `IsPending==false` の対称性と、空 id (`""`) が決して pending にならないことを確認する。symbol 系 (`MarkPendingSymbol` / `MarkResolvedSymbol` / `IsPendingSymbol`。broker position id が fill 解決前で未確定な窓を塞ぐ) も同様に対称性を確認する — こちらはカウント式で、同一 symbol の並行 saga の数だけ resolve が要る。

```go
func TestEmergencyStop_TripIsWriteOnceAndIdempotent(t *testing.T) {
    path := filepath.Join(t.TempDir(), "emergency_stop.flag")
    tripped := 0
    es := NewEmergencyStop(path, func(string) { tripped++ })
    now := time.Date(2026, 6, 17, 14, 50, 0, 0, time.UTC)
    _ = es.Trip("first_reason", now)
    _ = es.Trip("second_reason", now.Add(time.Minute)) // no-op
    if tripped != 1 { t.Fatalf("onTrip fired %d times, want 1", tripped) }
    // Reason() は first_reason を保持し second_reason を含まない
}
```

---

## 既存実装の代表例

- [backend/internal/safety/emergency.go](../../../backend/internal/safety/emergency.go) — flag file の read / write-once / resume + onTrip hook。
- [backend/internal/safety/pending_positions.go](../../../backend/internal/safety/pending_positions.go) — `RWMutex` で守る `map[string]struct{}`。`port.PendingPositionTracker` 実装。
- [backend/internal/port/pending_positions.go](../../../backend/internal/port/pending_positions.go) — `PendingPositionTracker` interface 定義。

### PendingPositions の不変条件

Live entry saga では broker に order を置いてから DB INSERT が完了するまで、broker にはポジションがあるが DB には無い race-window がある。並行 reconcile がこの window で in-flight entry を naked external position と誤検出しないよう、`Reconcile` は `IsPending` を見て in-flight な broker position id を skip する。`MarkPending` / `MarkResolved` は対称呼び出しで運用し、`MarkPending("")` (空 id) は無視される。in-memory 実装なので bot プロセスごとに 1 instance を生成し、restart 中の race は startup reconcile 側で扱う前提。

### trip 経路 (= emergency_stop が発火する条件)

`Trip(reason, now)` を呼ぶ箇所と理由文字列 (production コードで検証済みのもの):

- [command/force_flatten.go](../../../backend/internal/usecase/command/force_flatten.go): 引け前フラット化 (`ForceFlatten`) の MARKET 強制返済が失敗 → `forced_liquidation_failed`。
- [command/manage_open_positions.go](../../../backend/internal/usecase/command/manage_open_positions.go): 維持率割れ breaker (`CheckMaintenance`、price 非依存で ticker 不健全時も発火) → `margin_maintenance_breach`。
- [command/exit_executor.go](../../../backend/internal/usecase/command/exit_executor.go): bot 発の決済で保護レッグを cancel 済みなのに ClosePosition が拒否された(= 保護なしの裸ポジ)→ `close_rejected_unprotected:<brokerPositionID>`。**自動の出口(ForceFlatten / ManageOpenPositions)のみ** — `CloseAllOpen`(画面の「成行決済」/ flatten-all)は「帳簿締めの拒否で bot を止めない」事前コミットにより拒否では trip しない。
- [command/exit_executor.go](../../../backend/internal/usecase/command/exit_executor.go): 守りの取消を **1 本でも確認できていない**まま返済も拒否された → `close_rejected_cancel_unconfirmed:<brokerPositionID>`。裸と断定する `close_rejected_unprotected` と分けるのは、運用の次の一手が正反対だから(裸 → 置き直す / 取消未確認 → 板を見る。置き直すと守りが二重になり返済可能数量超過で両方弾かれうる)。
- [command/exit_executor.go](../../../backend/internal/usecase/command/exit_executor.go): 保護レッグを cancel した後、決済が **accepted なのに約定も板への常駐もしない**(= 逆指値も決済注文も無い裸ポジ)→ `close_unfilled_unprotected:<brokerPositionID>`。板に決済注文が**残っていれば trip しない**(未約定の決済を残すのはストップ安の比例配分への唯一の参加経路で意図的)。**`CloseAllOpen` 経由でも鳴る** — 裸は帳簿締めの事前コミットの対象外で、`NewCloseAllOpen` の `unprotected` 引数がその宛先(live のみ配線・research は nil)。
- [command/execute_order.go](../../../backend/internal/usecase/command/execute_order.go): 部分約定の残注文が cancel も再 resolve でも確定できない(= 追跡不能な裸の残玉になりうる)→ `partial_fill_residual_uncancelled:<symbol>`。
- [command/split_guard.go](../../../backend/internal/usecase/command/split_guard.go): 株式分割の権利落ちを値段から断定したが、live で broker の建玉照会が分割後の株数・建単価を示さない(照会失敗含む)→ `split_unconfirmed:<symbol>`(銘柄ごとにその日 1 回)。その建玉の自動決済はその日止める。
- [command/reconcile.go](../../../backend/internal/usecase/command/reconcile.go): stale position が cold close 不能のまま hard window 超 → `reconcile_stale_position_unresolved:<id>`。grace 内は defer、grace 超は cold close(`reconcile_cold_close`)を先に試す。
- [command/reconcile.go](../../../backend/internal/usecase/command/reconcile.go): adopt した外部ポジに close 側 working order が無い(守りなし孤児)→ `external_adopt_unprotected:<brokerPositionID>`。
- [command/reconcile.go](../../../backend/internal/usecase/command/reconcile.go): CLOSING のまま再クローズも効かず hard window 超 → `close_stuck_unresolved:<brokerPositionID>`。
- [command/execute_order.go](../../../backend/internal/usecase/command/execute_order.go): entry saga の補償 close (`compensate`) 自体が失敗 → `compensating_close_failed:<brokerPositionID>`。守りなしの裸ポジが broker に残るため止血する。
- [command/execute_order.go](../../../backend/internal/usecase/command/execute_order.go): PlaceOrder transport エラー後、板/建玉の照会でも注文の行方を確定できない → `entry_submit_unconfirmed:<symbol>`。
- [cmd/stockbot/loops.go](../../../backend/cmd/stockbot/loops.go): 立花 API 閉局(03:30〜)明けの再認証失敗 → `token_refresh_failed`(閉局窓内の失敗では trip しない)。
- [app/handler/handler.go](../../../backend/internal/app/handler/handler.go): 人手の `POST /api/emergency-stop`(reason 省略時)→ `manual_api`。

trip 中は新規 entry が拒否される (status view の `EmergencyStop=true`、entry 経路は emergency active で reject)。

> **完全な trip マトリクスの SoT は [FAILURE_MODES.md](../FAILURE_MODES.md)**。trip 理由の探索は `grep -rE '\.Trip\("' backend/` で行い、`_test.go` 内の文字列は production 理由ではないので除外する。

---

## アンチパターン (= 避けるべき失敗)

- emergency_stop を usecase 内で `os.WriteFile` して直接管理する — `safety.EmergencyStop.Trip(reason, now)` に統一する (flag は file なので bot restart を跨いで残る)。
- usecase から `safety` パッケージを import する — `command.EmergencyController` / `query.EmergencyReader` のローカル interface 経由にする (層規約)。
- trip 理由を 2 回目で上書きできる実装にする — `Trip` は write-once、**最初の理由を保持**する。
- 自動 `Trip` 呼び出し側で error を握り潰す(`_ = ...Trip(...)`)だけで済ませ、flag path の書込可能性を起動時に検証しない — file 書込が失敗すると emergency が沈黙 no-op になる。`Trip` は in-memory フラグで当該プロセスを halt しつつ error を surface し、`Preflight` が起動時に fail-close する。
- `Resume` を bot 内部から自動で呼ぶ — resume は人間専用 (`POST /api/emergency-resume` 経由)。
- stale-position の grace / hard window を ad-hoc に複数箇所へ hardcode する — 1 箇所 (`reconcile.go` の定数) に集約する。

---

## 関連 docs

- [../FAILURE_MODES.md](../FAILURE_MODES.md) — どんな失敗で trip するかの絶対ルール / 完全な trip マトリクスの SoT。
- [../PR_CHECKLIST.md](../PR_CHECKLIST.md) — 不変条件チェック。
- [../../../CLAUDE.md](../../../CLAUDE.md) — emergency_stop / 維持率 / 引け前フラット化 のトレード不変条件。
