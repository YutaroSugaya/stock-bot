# Layer: Handler

## 役割 (1 行)

HTTP の入口。リクエストを parse → usecase を呼び → View DTO を encode する薄い層。ビジネスロジックも DB/broker も持たない。

---

## やること (do)

- HTTP リクエストの query/JSON parse、レスポンスの JSON encode（`writeJSON`)
- 軽い validation（必須 query の存在チェックなど）
- usecase の直接呼び出し（read は `query.GetBotStatus.Execute` / `query.ListOpenPositions.Execute`)
- `EmergencyController` 経由の emergency_stop trip / resume
- err → HTTP status のマッピング（現状は `writeJSON` が err 非 nil を一律 `500`、不足 query を `400`)
- 埋め込み SPA（`web/index.html`）を `GET /{$}` で配信（[web.go](../../../backend/internal/app/handler/web.go))

---

## やらないこと (don't)

- Repository / Broker を **直接**呼ぶ（必ず usecase 経由。handler は `pgx`/`net/http` client/broker を import しない)
- ビジネスロジックを書く（TP/SL 計算、risk gate、ナンピン判定、Tx 内 update など → usecase/domain)
- Domain entity（`position.Position` など）をそのまま `json.Marshal` する（必ず `query` の View DTO を返す)
- `config` を import する（mode/broker_kind は string で usecase へ渡し済み。R1)
- `app` パッケージへ依存する（counters や per-symbol summary は `func() map[string]any`（`dashExtra`)として注入される。[handler.go](../../../backend/internal/app/handler/handler.go) は `app` を import しない)

---

## 命名 / 配置

| 種別 | 場所 | 例 |
|---|---|---|
| Handler 本体 + ルート | `backend/internal/app/handler/handler.go` | `Handler`, `Routes()`, `getStatus` |
| SPA 配信 | `backend/internal/app/handler/web.go` | `indexPage`, `//go:embed web/index.html` |
| 静的 SPA | `backend/internal/app/handler/web/index.html` | zero-build vanilla JS |
| read DTO | `backend/internal/usecase/query/` | `BotStatusView`, `OpenPositionView` |

### 現在のルート（[handler.go](../../../backend/internal/app/handler/handler.go) `Routes()`)

| メソッド / パス | ハンドラ | 返すもの |
|---|---|---|
| `GET /{$}` | `indexPage` | 埋め込み SPA（HTML) |
| `GET /api/status` | `getStatus` | `query.BotStatusView`(per-symbol + account_open_count + emergency) |
| `GET /api/dashboard` | `getDashboard` | `{status: BotStatusView, counters, summaries, selector, gonogo}`(1 fetch で SPA 更新)。`gonogo` は今日の寄り前の判定(`query.GoNoGoView`・銘柄 → 判定・paper は bnf 家族だけ)で、画面がランキングの行と銘柄で結合する。表示だけ |
| `GET /api/positions` | `getPositions` | `[]query.OpenPositionView`(`?symbol=X` 必須) |
| `POST /api/positions/extend` | `postExtendMaxHold` | `port.MaxHoldExtended`(MaxHold 延長) |
| `POST /api/positions/close` | `postClosePosition` | `command.CloseOneResult`(1 建玉の手動成行決済 — dashboard「成行決済」。404 未知 id / 409 非 OPEN・external / 503 未配線。emergency 中でも通る) |
| `POST /api/flatten-all` | `postFlattenAll` | `command.CloseAllResult`(OPEN 全建玉の手動成行決済 = paper トラックの手動の帳簿締め。**live_config は 409**(実弾の全清算は人間の判断)/ 503 未配線。`WithManualClose(c, mode)` で usecase と mode を string 注入 — R1 と同型) |
| `POST /api/emergency-stop` | `postEmergencyStop` | `{emergency_stop:true, reason}` |
| `POST /api/emergency-resume` | `postEmergencyResume` | `{emergency_stop:false}` |
| `GET /api/performance` | `getPerformance` | `query.ForwardReportView`(戦績 = 決済済み台帳の集計。トータル / `by_day` / `by_strategy` / `by_symbol` / `trades`。`?cycle=<key>\|all` は期間(cycle)単位(知らないキーは `400`)、`?since=YYYY-MM-DD` は JST 0 時、壊れた値は `400`。未配線は `503`) |
| `GET /api/performance/cycles` | `getPerformanceCycles` | `{default, cycles:[{key,label}]}`。期間(cycle)= `perfcycles/cycles.yaml` の (台帳 DB, 開始日) の組(`handler.PerformanceCycle`・組み立ては `internal/app/perfcycles`)。paper の過去の期間は同じサーバの別 DB を**読み取り専用セッション**で開き、全期間は DB をまたいで合算する |
| `GET /api/advisor-runs` | `getAdvisorRuns` | `[]query.AdvisorRunView`(LLM 判断ジャーナル。`?symbol=&limit=`。未配線は `[]`) |
| `POST /api/advisor-trigger` | `postAdvisorTrigger` | `202 {started:true}` = 手動 LLM 判断を非同期起動 / `409` 実行中 / `503` advisor OFF。`WithAdvisorTrigger` の closure 注入(`app.AdvisorLoop.StartManual` — EmergencyController と同じ「app を import しない」前例) |
| `POST /api/gonogo/run` と `POST /api/live/gonogo/run` | `postGoNoGoRun` | 画面の「未判定を判定」。research と live の arm 済みで判定が無い銘柄の go/no-go を別プロセス(`gonogo -symbols`・起動経路 `button`)で起動して即 return。`202 {started:true, symbols}` / `200 {started:false, symbols:[]}` = 未判定なし / `409` 実行中(`port.ErrGoNoGoRunning`)/ `500` 起動できない / `503` 未配線。2 つのパスは同じ処理(画面の `api()` が live タブで `/api/live` を前置するため)。`WithGoNoGoRun` の closure 注入。**表示と記録だけ** |
| `GET /api/live/dashboard` | `getLiveDashboard` | live の 1 fetch 束(status + counters + summaries + selector.ranking + gonogo + symbol_blocks)。**未配線は 503** |
| `GET /api/live/status` | `getLiveStatus` | live トラックの `BotStatusView`。**未配線は 503** |
| `GET /api/live/positions` | `getLivePositions` | live の `[]OpenPositionView`(`?symbol=X` 必須) |
| `GET /api/live/performance` | `getLivePerformance` | live 台帳の `ForwardReportView`。`?cycle=` は live DB を期間で切る |
| `GET /api/live/performance/cycles` | `getLivePerformanceCycles` | live の期間一覧(境界は次の期間の開始日) |
| `POST /api/live/positions/close` | `postLiveClosePosition` | live 建玉 1 本の手動成行決済。**実弾 1 ポジを人間が返済する唯一の手段** |
| `POST /api/live/protective/replace` | `postLiveReplaceProtective` | **実弾の守りを取消 → 再発注**して期日を延ばす。訂正では発注日+10営業日の天井を超えられないので唯一の延長手段。**寄り前の窓で自動でも走る**(loops.go)が、この口は叩いたときにも走る。場中判定と**残り2営業日の閾値**は usecase 側が持つので、条件を満たさなければ no-op。返り値 `{replaced, skipped, errors}` — **`skipped` があるから `replaced: 0` の意味が割れる**(「対象なし」と「全部失敗」は別の状態)。**エラーを握り潰さない** — 置き直しの失敗は「建玉が裸」を意味しうる |
| `POST /api/live/protective/arm` | `postLiveArmProtective` | **守りが無い建玉に守りを置く**。body は `{symbol, take_profit, stop_loss}` で、**数量は受け取らない**(台帳から引く — 人間に打たせると建玉超過の返済注文になる)。板が空の状態から復旧する唯一の経路。**emergency 中でも通す** — emergency は新規建てを止めるものであって、裸の建玉に守りを置くのを止めるものではない(塞ぐと trip するほど危険になる逆立ちが起きる)。失敗は 400 + 文言そのまま(**建玉が裸のまま**を意味するので握り潰さない) |
| `POST /api/live/protective/reprice` | `postLiveRepriceProtective` | **守りの値段を変える**(取消 → 再発注)。body は `{symbol, take_profit, stop_loss}`。arm と違い**リスクが一時的に増える** —— 守りを一度板から降ろすので、再発注に失敗すればその建玉は裸のまま emergency trip。失敗は 400 + 文言そのまま(復旧コマンド入り)。画面にはボタンを置かない(事前に決めた出口を裁量で曲げる操作。`web_test.go` が固定) |
| `POST /api/live/positions/extend` | `postLiveExtendMaxHold` | live 建玉の max_hold 延長。**broker 側の守りには触らない**(期日は寄り前の `ReplaceProtectiveOrder` が取消 → 再発注で延ばす。訂正 `RenewProtectiveExpiry` は退役)。上限は保有区分で決まる(intraday 12時間 / multiday 30日)ので handler は数字を持たない。画面にはボタンを置かない(`web_test.go` が固定) |
| `GET /api/live/positions/extend-options?position_id=` | `getLiveExtendOptions` | 延長先の候補(**各営業日の引け前 14:50**・1 回の上限内・休場日は飛ばす)を `query.ListExtendOptions` が返す。返した候補の `add_minutes` を上の POST へ渡す。日付も上限も handler は持たない。画面からは使わない |
| `POST /api/live/emergency-stop` | `postLiveEmergencyStop` | `{emergency_stop:true, track:"live"}` — **research は止まらない**(別フラグファイル) |
| `POST /api/live/emergency-resume` | `postLiveEmergencyResume` | `{emergency_stop:false, track:"live"}` |
| `POST /api/live/symbol-blocks` | `postLiveSymbolBlock` | live の**その銘柄の新規だけ**を止める(人間のボタン)。body は `{symbol, note}`。銘柄は `allowed_symbols` に載っているものだけ(外は 400)。応答は停止中の一覧。決済・守り・引け前フラット化・max_hold 延長には効かない。停止中の一覧は `GET /api/live/dashboard` の `symbol_blocks`(読めなければ `error` が入り、live の新規は全部止まっている) |
| `POST /api/live/symbol-blocks/release` | `postLiveSymbolBlockRelease` | 停止を解く。body は `{symbol}`。止まっていなければ 404 |
| `GET /healthz` | inline | `200`(空ボディ) |

**hybrid の `/api/live/*`**: live トラックの束は `WithLiveTrack(*LiveViews)` で
**research とは別インスタンス**を注入する。既存ルートは research のまま**不変**。

- **未配線は `503`**。空配列を返すと「live は動いているが建玉が無い」と読めてしまい、
  止まっている実弾を動いていると誤認する(`getPerformance` の 503 と同じ極性)。
- **`/api/live/flatten-all` は存在しない**(404)。全清算を人間がボタン 1 つでやる操作に
  しない — research 側の `/api/flatten-all` は paper トラックの手動の帳簿締めなので残す。
- live の個別 close は research 側の「警報なし」規約を**流用しない**。
  あれは「paper は資本リスクゼロ」という理由に依存していて live に移植できない。live が活かすのは
  `closeOne` 内蔵の **`close_unfilled_unprotected`** trip(守りの脚を cancel した後に決済が
  「受理されたのに約定も板への常駐もしない」= 逆指値も決済注文も無い)。
  **`close_rejected_unprotected` はこの経路では発火しない** — 「帳簿締めの close 拒否で
  bot を止めない」という `CloseAllOpen` の事前コミットを維持しているため(拒否は CLOSING のまま
  reconcile が回収)。
- ダッシュボードは LIVE を**最上段の独立セクション**に出し、paper と同じ表に並べない
  (損益の混読防止)。live 無し(503)ならセクションごと非表示。

`getPerformance` は `cmd/forward-report` と**同じ usecase**(`query.BuildForwardReport`)を読む — 集計規約(net = gross − fee + carry)を 2 箇所に書かないため。未配線(trade repo 無し)は空レポートではなく `503`: 「集計できない」を「損益ゼロ」と誤読させない。

`getPositions` は `?symbol=` が空なら `400 missing symbol`。`activeBySym func() map[string]string` が status 用の「symbol → active config id」マップを供給する（多 symbol bundle を expose する仕組み。`app` への直接依存を避けるため関数注入）。

### 配線（DI）

`handler.New(status, listOpen, em, extend, activeBySym, dashExtra)` で組む（[cmd/stockbot/main.go](../../../backend/cmd/stockbot/main.go))。`dashExtra` / `extend` は nil 可（nil の extend は endpoint が 503)。`EmergencyController` / `MaxHoldExtender` は handler 内に定義された consumer view interface で、handler は具体 adapter を知らない（`MaxHoldExtender` は `port.MaxHoldExtended` のみ参照）。

optional な面は builder chain で足す（[handler.go](../../../backend/internal/app/handler/handler.go))：`WithAuth(token, bindNonLoopback)` は状態変更系 endpoint の認証を配線し、`authorizeMutation` が gate する — token 設定時は不一致で `401`、token 無し + 非 loopback bind は `403` で fail-close（人間専用の emergency gate を無認証でネットワークに晒さない。read-only 系は対象外）。`WithAdvisorRuns` / `WithPerformance` は observability query の注入（nil 時の挙動はルート表のとおり `[]` / `503`)。

### ハンドラ追加条件

新ルートは `Routes()` に 1 行追加 + メソッド追加。状態を変える操作は **必ず usecase（command）を 1 つ介す**こと。handler に分岐ロジックが溜まってきたら command 側へ寄せる。

### `POST /api/positions/extend`（ExtendMaxHold)

手動 override で建玉の MaxHold を延長する endpoint(`config 凍結`の唯一の例外、[../../../CLAUDE.md](../../../CLAUDE.md)「config 凍結」)。body は `{"position_id": <int>, "add_minutes": <1..720>}`。handler は範囲 validation(範囲外は `400`)だけ行い、[`command.ExtendMaxHold`](usecase.md) を介して repo の CAS(`WHERE status='OPEN'`)を叩く。非 OPEN / 未知 id は command が `(nil,nil)` を返し handler が `404` に写像。broker 側 OCO には触れない。handler から repo は直呼びしない(`MaxHoldExtender` interface 経由)。

---

### 株式分割の表示

建玉一覧の DTO に `split_factor` / `split_adjusted_on`(言い直した建玉は株数が 100 の倍数でなく建値が格子外になる理由)。
決済理由のラベルに `split_misfire: 分割の誤読で決済`。

## テスト方法 (この層特有)

- **`net/http/httptest`** で書く（`httptest.NewRequest` + `httptest.NewRecorder` → `h.Routes().ServeHTTP(rec, req)`)
- usecase / `EmergencyController` は **fake** に差し替える（`handler.New(...)` に渡すのは interface / 関数なので注入が容易)
- 検証対象: HTTP status code、レスポンス JSON、返す DTO が domain entity でないこと

```go
func TestGetPositions_MissingSymbol_Returns400(t *testing.T) {
    h := handler.New(statusQ, listQ, fakeEmergency{}, activeBySym, nil)
    req := httptest.NewRequest("GET", "/api/positions", nil)
    rec := httptest.NewRecorder()
    h.Routes().ServeHTTP(rec, req)
    if rec.Code != http.StatusBadRequest { t.Fatalf("got %d", rec.Code) }
}
```

> handler テストは httptest で書く（[handler_test.go](../../../backend/internal/app/handler/handler_test.go) が `POST /api/positions/extend` を 200/400/404/503 で検証する例)。usecase は consumer view interface の fake に差し替え、status code とレスポンス JSON を assert する。

---

## 既存実装の代表例

- [backend/internal/app/handler/handler.go](../../../backend/internal/app/handler/handler.go) — `Handler` 本体・`Routes()`・全エンドポイント・`writeJSON`・consumer view の `EmergencyController`。import は `context` / `crypto/subtle` / `encoding/json` / `errors` / `net/http` / `strconv` / `strings` / `time` / `port` / `usecase/command` / `usecase/query`（層規約を満たす最小依存)。`usecase/command` は `command.ErrEmergencyActive` → `409` 等の err sentinel 参照のみで、handler が command を直接実行するわけではない（層規約違反ではない)。
- [backend/internal/app/handler/web.go](../../../backend/internal/app/handler/web.go) — `//go:embed web/index.html` で SPA を 1 バイナリに同梱（node/build 不要・第 2 プロセス不要)。
- [backend/internal/usecase/query/get_bot_status.go](../../../backend/internal/usecase/query/get_bot_status.go) — `/api/status` の read DTO（`BotStatusView`: `mode` / `broker_kind` / `emergency_stop` / `account_open_count` / `per_symbol`)。`activeConfigBySymbol` を受け取り symbol ごとに `ListOpenOrClosing` を引く。
- [backend/internal/usecase/query/list_open_positions.go](../../../backend/internal/usecase/query/list_open_positions.go) — `/api/positions` の read DTO（`OpenPositionView`)。`toView` で `position.Position` → DTO へ落とす（entity を外へ出さない)。

---

## アンチパターン (= やってはいけない)

- handler に `pgxpool` / broker client を持って SQL や発注を直接叩く → usecase / port に閉じ込める
- `position.Position` を `json.Marshal` してそのまま返す → `OpenPositionView` を介す
- TP/SL・ナンピン判定・risk gate を handler 内に書く → domain / usecase の不変条件（[../../../CLAUDE.md](../../../CLAUDE.md))
- emergency resume を handler の都合で自動化する → 再開は人間の `POST /api/emergency-resume` のみ（write-once の trip と対)
- extend MaxHold で repo を直呼び → 必ず command を 1 つ挟む
- ⚠️ `postEmergencyStop` は現状 handler 内で `time.Now()` を直接呼ぶ（handler 層では許容。ただし domain/usecase 側へ時刻を渡す際は `clock.Clock` 注入に揃える。handler 側も clock 注入へ寄せるかは未定)

---

## 関連 docs

- [../PR_CHECKLIST.md](../PR_CHECKLIST.md) — マージ前チェック
- [../../../CLAUDE.md](../../../CLAUDE.md) — 層規約・トレード不変条件（handler → repo 直呼び禁止 / View DTO / R1)
- [../../../backend/internal/usecase/query/](../../../backend/internal/usecase/query/) — handler が返す read DTO（CQRS read 側)
- [port.md](port.md) / [usecase.md](usecase.md) / [safety.md](safety.md) — 隣接層の個別 doc
- [../FAILURE_MODES.md](../FAILURE_MODES.md) / [../../workflows/TESTING.md](../../workflows/TESTING.md) / [../../runtime/STATE_MACHINE.md](../../runtime/STATE_MACHINE.md)
