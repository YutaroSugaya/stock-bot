# Layer: Adapter

## 役割 (1 行)

`port` interface の **具体実装**。立花証券 e支店 REST / paper match engine / Postgres 等、外部システムとの境界アダプタ。

---

## やること (do)

- `port.Broker` / `port.LiveBroker` を実 broker (立花) と paper match engine で実装する
- `port.PositionRepository` / `port.TradeRepository` / `port.CandleRepository` / `port.PositionCloser` を **in-memory** と **Postgres (pgx)** の両方で実装する (同じ port を満たす二系統。戦略 config の読み書きは **pg のみ**で port を持たない — [pg/config_repo.go](../../../backend/internal/adapter/repository/pg/config_repo.go))
- 外部システムの DTO ↔ port record の変換を **adapter 内側** で完結させる (立花の 売買区分 `sBaibaiKubun` "3"=買/"1"=売 や `sGenkinShinyouKubun` ↔ `order.ExecKind` 等)
- DB row ↔ domain entity の変換を repository adapter が担当する (`scanPosition` 等)
- multi-table 書込みは `withTx` (`pgxpool.Begin` + commit/rollback) に閉じ込め、close saga の atomic 性を守る
- コンパイル時に interface 充足を `var _ port.X = (*T)(nil)` で固定する

---

## やらないこと (don't)

- ビジネス判定をする (TP/SL 計算・risk gate・ナンピン判定などは domain/usecase の責務)
- `usecase` / `app` / `handler` を import する (adapter は port の下流)
- adapter 同士で直接呼び出す (= usecase 経由で合成する)
- `config` を import する (mode 等は string で受ける。R1)
- paper / test 経路で実 broker / 実 DB に触れる (paper は完全 in-memory、in-memory repo は DB 不要)
- 未検証の broker 守り経路を黙って通す (立花 OCO は fail-close。後述)

---

## 命名 / 配置

| 種別 | 場所 | ファイル名 | 例 |
|---|---|---|---|
| Paper Broker | [`backend/internal/adapter/broker/`](../../../backend/internal/adapter/broker/) | `paper.go` | `Paper` (`var _ port.LiveBroker`) |
| Paper×実フィード Broker | 同上 | `paper_live_feed.go` | `PaperLiveFeed` (実フィード read-only × 紙執行) |
| Live Broker (立花) | 同上 | `tachibana.go` / `tachibana_{transport,orders,market}.go` | `Tachibana` (`var _ port.LiveBroker`) |
| Feed デコレータ / rate limit | 同上 | `batch_feed.go` / `ratelimit.go` | `BatchQuoteFeed` / `rateLimiter` |
| In-memory Repo | [`backend/internal/adapter/repository/`](../../../backend/internal/adapter/repository/) | `<aggregate>_repo.go` / `inmemory.go` | `InMemoryPositionRepo` |
| In-memory Closer | 同上 | `close_saga.go` | `Closer` |
| DSN ガード | 同上 | `backtestdsn_guard.go` | `SafeBacktestDSN` / `SafeIntegrationTestDSN` |
| Postgres Repo | [`backend/internal/adapter/repository/pg/`](../../../backend/internal/adapter/repository/pg/) | `<aggregate>_repo.go` | `PositionRepo` / `TradeRepo` / `CandleRepo` |
| 台帳の合算(読むだけ) | [`backend/internal/adapter/repository/merged_trades.go`](../../../backend/internal/adapter/repository/merged_trades.go) | — | `MergedTrades`(画面の「全期間」の戦績で複数 DB を合算。建玉 id は上位ビットにソース番号を載せて名前空間を分ける — DB ごとに id が衝突するため) |
| Postgres pool/tx | 同上 | `pool.go` | `Open` / `withTx` |
| Notifier Impl | `backend/internal/adapter/notifier/` | `<vendor>.go` | `stdout.go`(`Stdout` — `port.Notifier` 実装) |
| 日足 / 分足 CSV の読み口 | [`backend/internal/adapter/candlecsv/`](../../../backend/internal/adapter/candlecsv/) | `candlecsv.go` | `Load` / `LoadDaily` / `DailyFile` / `DailySymbols`(`<sym>_daily.csv` の命名と列挙。bot・fetch-daily・日次総評・universe-screen・backtest が共有) |
| 寄り前の銘柄判定(go/no-go) | [`backend/internal/adapter/gonogo/`](../../../backend/internal/adapter/gonogo/) | `journal.go` / `claude.go` / `parse.go` / `lock.go` / `launch.go` | `Journal`(`port.GoNoGoReader`・`~/.stockbot/gonogo/<日付>.jsonl` と `.md`)/ `Judge`(claude CLI・WebSearch / WebFetch だけ・プロンプトは `go:embed`)/ `ParseJudgment`(出典の無い no_go は unknown)/ `AcquireLock` / `Launcher`(画面のボタンから `gonogo -symbols` を別プロセスグループで起動・実行中なら `port.ErrGoNoGoRunning`)。**表示と記録だけ** — 発注経路は import しない |
| live の銘柄ごとの新規停止 | [`backend/internal/adapter/symbolblock/`](../../../backend/internal/adapter/symbolblock/) | `file.go` | `FileStore`(`port.SymbolBlockStore` のファイル実装。毎回読む・tmp → fsync → rename で書く・壊れたファイルは error で返し上書きしない・操作を `.log` に JSONL で追記) |
| API 消費カウンタ | [`backend/internal/adapter/apiusage/`](../../../backend/internal/adapter/apiusage/) | `usage.go` | `Counter` / `Usage` / `WindowStart` / `DefaultDir` / `BrokerDailyRequestCap` |

### repository の二系統について

`adapter/repository/` (in-memory) と `adapter/repository/pg/` (Postgres) は **同一 port を満たす二実装**。
`STOCKBOT_DATABASE_URL` が設定されていれば pg、未設定なら in-memory にフォールバックする (paper / test / backtest は in-memory のまま)。
in-memory 側も `ClaimForClose` CAS (二重 close 防止)・`CloseAndRecord` の atomic 性・per-symbol query 分離という不変条件を保持する ([repository/inmemory.go](../../../backend/internal/adapter/repository/inmemory.go) 冒頭コメント参照)。

### notifier subdir の責務

`adapter/notifier/` は `port.Notifier` (`Notify(ctx, level, title, message)`) の実装を集める。`stdout.go`(`Stdout`)が最初の実装で、1 行 1 通知を stdout に出す(`clock.Clock` 注入でテスト可能)。Slack/webhook は別 adapter として後から追加できる。
通知は best-effort で、失敗しても取引経路をブロックしない (`port.Notifier` のコメント) — この不変条件を破る実装を書かない。

### apiusage subdir の責務

`adapter/apiusage/` は**立花 API の消費回数を先方と同じ集計単位で数える**。
port を実装せず、`broker.UsageRecorder`(`Record(clmid string)`)という
**adapter 側が宣言した狭い interface** を満たす — broker は具体実装を知らず、
`cmd` が組み立てて挿す。

契約は5つで、**どれも「先方の数字と突き合わせられること」から来ている**:

- **窓は JST 5:30 起点**(`WindowStart`)。立花の集計期間は「開局している時間帯
  (5:30〜翌3:30)」で**暦日ではない**。暦日で切ると夜間の日足取得が翌日ぶんに計上され、
  弊方の申告と先方の計測が永久にずれる。
- **プロセスを跨いで合算する**。`bot` と `fetch-daily` は別プロセスだが、先方の集計は
  **口座単位**。プロセスごとに別ファイルへ書き、読むときに合算する
  (**ロックを持たない** — ロック待ちが発注経路のレイテンシに乗らない)。
- **再起動でゼロに戻らない**。`Tachibana.APIRequests()` は プロセス内 `atomic` なので
  `make stop/start` のたびに消える。「今日いくつ叩いたか」に答えられるのはこちらだけ。
- **CLMID 別に残す**。先方の集計は CLMID 単位なので、総数しか持っていないと
  「どの処理が原因か」に答えられない。先方基準の比較用に **login を除いた数**
  (`TotalExcludingLogin`)も出す — 立花は `CLMAuthLoginRequest` を計上していない。

- **先方の上限もここに置く**(`BrokerDailyRequestCap`)。立花が求める
  1 日 1 万回以下という上限で、**弊方の予算ではない**
  (内部の `dailyQuoteRequestBudget` = 時価レーンだけの枠とは別物)。
  **これを見る側が 1 プロセスではない**のが、値をこのパッケージへ置く理由 — `cmd/stockbot` は
  dashboard に「N / 10,000」を出し、`cmd/fetch-daily` は自分の 1,550 リクエストを打つ前に
  残枠として読む。literal を 2 か所に書くと、片方だけ直したときに**通信量の判断が静かに
  食い違う**。数え口が既にここに集まっている以上、上限も同じ場所が正しい。
  ⚠ **この定数は強制しない**。runtime に「超えたら止める」経路は無く、判断するのは
  読む側(`cmd/fetch-daily` の発射前ガードと dashboard の表示)。

**観測は取引を止めない**: 書けない場所を渡されても `Record` は素通りし、プロセス内の
数字だけは保つ。壊れた JSON は読み手が飛ばす(そのぶんは過少申告になるので、
書き込みは temp+rename で原子的に行いそもそも作らない)。
置き場 `DefaultDir()` は `~/.stockbot/state/api-usage`(launchd 起動のバイナリは macOS の TCC で
`~/Desktop` 配下を読めないので、Desktop 配下に置かない)。

---

## テスト方法 (この層特有)

### In-memory repository / paper broker

- 通常の `go test ./...` (= `make check-backend`) でそのまま走る。外部依存なし
- `paper.go` は full match engine なので、約定・slippage・OCO leg 記録まで unit test で検証できる

### Postgres repository (integration)

- **実 Postgres を使う** integration test。ファイル先頭に `//go:build integration` ([pg/integration_test.go](../../../backend/internal/adapter/repository/pg/integration_test.go))
- `INTEGRATION_TEST_DB_URL` で `_test` 接尾辞の test DB に繋ぐ。未設定なら `t.Skip`
- **生で `go test -tags integration` を打たない** (deny hook が拒否)。`make test-integration` 経由のみ
- test DB は `requireDB` 内で `SafeIntegrationTestDSN` の二重壁を通り、live DSN と異なる事を強制してから接続・`TRUNCATE` する
- `.githooks/pre-push` の `make vet-integration`(= `go vet -tags integration ./...`)でコンパイルだけ検証

```go
//go:build integration

func requireDB(t *testing.T) (*pgxpool.Pool, func()) {
    testDSN := os.Getenv("INTEGRATION_TEST_DB_URL")
    if testDSN == "" { t.Skip(...) }
    if err := repository.SafeIntegrationTestDSN(testDSN, os.Getenv("STOCKBOT_DATABASE_URL")); err != nil {
        t.Fatalf("unsafe integration DSN: %v", err)
    }
    // open pool, TRUNCATE, run lifecycle test
}
```

### tachibana broker

- REST は `net/http` 直書き (stdlib + `golang.org/x/text` の Shift-JIS 変換)。transport 層が login (公開鍵認証・仮想URL の RSA 復号。認証 I/F は v4r9 / v4r10 で同一)・p_no 採番・Shift-JIS/JSON encode-decode を一手に担う
- HTTP fake (`httptest.NewServer`) で fixture response を返し検証する ([tachibana_test.go](../../../backend/internal/adapter/broker/tachibana_test.go) の `newMockTachibana`)。現状カバー: 公開鍵認証の login 契約 (`sAuthId` のみ送信・仮想URL 復号)、版の真実源 (`tachibana_version_test.go`: `authBase` が `ver` から組まれること・版の綴りのピン・v4r9 廃止期限の仕掛け線)、`GetTicker`/`GetKlines`、発注 shaping (`sBaibaiKubun` / `sGenkinShinyouKubun`)、OCO の fail-close (`TestTachibana_OCOFailCloseUntilVerified`)、検証時の 通常+逆指値 配置、約定解決、余力ゲート

詳細は [../../workflows/TESTING.md](../../workflows/TESTING.md)。

---

## 既存実装の代表例

### Broker

- [broker/live_quote_shared.go](../../../backend/internal/adapter/broker/live_quote_shared.go) —
  **hybrid(実弾 × paper 並走)の live トラック用デコレータ**。立花のセッションは
  口座に 1 本しか張れないので、live track も research track と**同じ `*Tachibana`・同じ一括
  クォートフィード**を共有する(2 プロセス並走は login が互いのセッションを破棄して蹴り合う)。
  - **時価は共有 feed から**返す → live track が何銘柄増えても時価レーンの本数は変わらない。
    素通しにすると `QuotesBatched=false` になり、価格ループが 30 秒へ退避する。
  - **`ProtectiveOrderBoard` の 2 本(`ListProtectiveOrders` / `CancelProtectiveOrder`)も転送する**
    。転送を忘れると守りの期日が誰にも延ばされないまま切れるが、
    `LiveBroker` の一員にしてあるので**消すとコンパイルが通らない**
    (`SettleFillReporter` と同じ作法)。
  - **建玉 / 余力照会は短 TTL の併合キャッシュ**。この 2 つは口座全体の照会を呼び出し側が
    symbol で絞っているだけなので、素通しだと N 銘柄が同じ質問を N 回投げる
    (時価の `BatchQuoteFeed` とまったく同じ構図)。
  - **発注系は絶対にキャッシュしない**(状態変更を畳んだら発注そのものが消える)。
    さらに **発注 / 決済 / 取消 / OCO の直後は口座キャッシュを捨てる** — 「建てたのに建玉ゼロ」を
    返すとナンピン禁止ゲートと `Reconcile` が古い像で判断する。**通信量のための併合が
    守りの誤作動になってはいけない。**
  - エラーはキャッシュしない(古い値を新鮮なふりで返さないのと同じ極性)。
  - **broker の「宣言」は必ず内側へ転送する**(`SettleFillsAsync` は `s.tb` へ委譲)。
    転送を忘れると usecase の型アサーションが黙って外れて **live の決済が丸ごと
    旧経路(受理を約定と読んで観測価格で台帳に書く)へ落ちる** — 幽霊決済を止めたつもりが
    live では 1 行も効いていない、という壊れ方をする。以後この種の宣言は `port.LiveBroker` の
    一員にして、転送漏れをコンパイルエラーにする。
- [broker/paper.go](../../../backend/internal/adapter/broker/paper.go) — 完全 in-memory match engine。MARKET を現値 ± `slippageTicks * market.TickSizeOf(symbol, price)` で即時約定し (BUY は高く・SELL は安く、コスト床のモデル化)、`feeJPYPerTrade` を fee に乗せる。`port.LiveBroker` を満たす (`var _ port.LiveBroker = (*Paper)(nil)`)。`GetKlines` は `nil, nil` を返す (履歴は candle repo / backtest 経路が供給)。`PlaceSettleOCO` / `ResolveSettleLegs` は OCO leg を記録するだけのシミュレーション。
  `ListProtectiveOrders` は **nil**、`CancelProtectiveOrder` は
  **no-op**— paper の
  守りに期日は無い(プロセス内の帳簿で引けに消えたりしない)。「延ばした」と嘘をつくのではなく、
  **延ばすべき対象がそもそも存在しない**ことを空で表す。
- [broker/tachibana.go](../../../backend/internal/adapter/broker/tachibana.go)(+ `tachibana_transport.go` / `tachibana_orders.go` / `tachibana_market.go` / `tachibana_creds.go`)— `立花証券 e支店` の実 REST (公開鍵認証・Shift-JIS/JSON)。
  - **API 版は `defaultTachibanaAPIVersion` 定数 1 箇所が唯一の真実源**(`authBase` は構築後に `tachibanaAuthBase(t.host, t.ver)` で組む = 版リテラルを 2 箇所に置けない)。
  - `APIVersion()` / `APIEnv()`(設定値そのまま)/ `APIHost()`(fail-close 解決後)が起動ログの口。
  - **`UseAPIVersion(ver)` は probe 専用の差し替え口**(`cmd/live-probe` / `cmd/loanable-fetch` の `-api-version`。login 後は拒否・形式外は fail-close。`cmd/stockbot` には配線しない)。
  - `ProbeRawMaster(ctx, clmid)` は MASTER 宛の応答トップレベルを解釈せず返す read-only プローブ(`t.request` 経由 = limiter と p_no に載る)。
  - **版更新告知**: login 応答の `sUpdateInformAPISpecFunction` / `sUpdateInformWebDocument` を `APIUpdateNotice()` で保持し、判定式 v10:231「予定日 ≧ 当日 AND 予定日 ≠ 前回受信値」は純粋関数 `APIUpdateDue(planned, last, today)`(`tachibana_notice.go`)。
  - 前回値の保存先と Warn は cmd 側(`warnAPIUpdateNotice`・`~/.stockbot/state/tachibana-update-notice.json`)で、adapter は slog も config も知らない。
  - fail-close にしない(告知の日に bot が上がらない方が危険)。
  - env `demo` で `demo-kabuka.e-shiten.jp`、`production` で `kabuka.e-shiten.jp` に向く。
  - `RefreshToken` は実 logout+login の再認証(日次 API 閉局 03:30〜 対策)。
  - `ResolveExecution` は注文照会を 500ms 間隔・15s 上限でポーリングする(1 決済あたり最大 ~30 リクエスト。張り付きは必ず 15s フルに使うので API 予算に効く)。
  - **`SettleFillsAsync() = true`** を宣言する — 「`ClosePosition` は受理しか返さない(約定値は非同期)」という契約で、usecase 側はこれを見て決済の約定を照会で確かめる(幽霊決済の禁止)。
  - **paper は宣言しない**(即約定で同期に返すため)。
  - `GetKlines` は日足のみ対応(intraday は aggregator)。
  - `port.LiveBroker` を満たす (`var _ port.LiveBroker = (*Tachibana)(nil)`)。
  - **`CancelProtectiveOrder` = `CLMKabuCancelOrder`**。**営業日は注文
    そのもの(`BrokerRef`)から取る** — `CancelOrder(id)` はプロセス内 map で空なら `"0"` に
    落ち、前プロセスが出した守りの取消が再起動後に必ず外れる。`BrokerRef` が空なら
    **API を叩かずに error**(当日扱いで送ると別の注文を取り消しうる)。
  - `ListProtectiveOrders` は `sOrderGyakusasiZyouken` / `sOrderGyakusasiPrice` /
    `sOrderOrderPrice` も読む。**取消 → 再発注で値段を引き継ぐ**のに要る。
  - 訂正(`CLMKabuCorrectOrder`)は実装しない(訂正では発注日起点の天井を超えられない)。期日は `ReplaceProtectiveOrder` の取消 → 再発注で延ばす。
  - **`ListProtectiveOrders`** は板に残る決済注文を期日つきで返す。`"0"` / 空 / 解釈不能な
    期日は **zero**(= 当日限り扱い)— 読めなかったものを「先の日付」と読むと、切れる守りを
    「まだ大丈夫」と誤判定する。
  - **`tachibana_issue_master.go`(read-only)**: 銘柄マスタの取得。用途は
    `cmd/loanable-fetch`(貸借銘柄ホワイトリストの素材)**だけ**で、research / live の
    どのトラックにも配線が無い。
    **v4r10 の個別マスタ問合**を使う
    (`CLMStkGetIssueSizyouMstKabu` / `CLMStkGetIssueSizyouKiseiKabu`・**単発 JSON**)。
    旧経路は「複数 JSON が連なるストリーム + `CLMEventDownloadComplete` の後も接続が開いたまま」
    という特殊な転送形式だった(v4r9 の `CLMEventDownload`)。
    - **完全性の担保は terminator ではなく「下限件数」**。v4r10 のマニュアル全文に
      `p_errno` は 1 件もヒットしないので、**`p_errno` は有るときだけ**見て
      (空は通す)、完全性は件数で守る。資料どおりの封筒を初回で落とすゲートは、本番の
      失敗直後に運用者へ「とりあえず緩める」を強いるだけで安全を足さない。
      **0 件は `p_errno` に関わらず落とす**(`gateMasterResp`)。校正値は v4r10 本番の実測
      × 0.9 = `masterMinRowsIssueMarket` 3,999 / `masterMinRowsIssueKisei` 491(実測 4,444 /
      546)。
    - **`map[string]string` で受けない**(型付き `StockIssueMarket` /
      `StockIssueRegulation`)。項目名が変わっても全行 `""` で黙って通り、
      「貸借 0 銘柄」に化ける。識別キー(`sIssueCode` / `sZyouzyouSizyou`)は
      **1 行でも空なら** error、値項目は **全行が空なら** error(個別行の空値は正規の値であり得る)。
    - 貸借判定に読むのは **`sSeidoSinyou…`(制度信用)**。`sIppanSinyou…`(一般信用)は
      仕様に区分があっても口座の種別によっては拒否されるので、そちらで判定すると
      「売れると判定したのに発注が拒否される」銘柄が混ざる。
    - マスタ取得も `t.request` に載せる(**limiter と `p_no` の直列化を迂回する経路を作らない**)。
    - `ProbeRawMaster(ctx, clmid)` は残す(応答トップレベルを解釈せず返す read-only プローブ)。
      **ゲートを通さない**ので、ここから貸借一覧を作らない。
  - **OCO は fail-close**: `PlaceSettleOCO` / `ResolveSettleLegs` は env `STOCKBOT_TACHIBANA_OCO_VERIFIED=1` で `ocoVerified` が立つまで error を返す(デモ実機で 発動方向・常駐・bot 非依存の約定 を裏取りしてから人間が立てる)。検証後は 既定 stop-only(逆指値 `'1'`)、`TakeProfit` 指定時は **単一注文の 通常+逆指値 `'2'`(TP 指値 + SL 逆指値の OCO)** — 注文番号は 1 つで legs は分割されない。
  - **`GetActiveOrders` は「板に残っているか」を残数量で判定する**。全部約定でない
    **かつ 残数量 `sOrderCurrentSuryou` > 0** のみ返す。**約定状態(`sOrderYakuzyouStatus`)
    だけでは取消済と区別できない** — 取消済も未約定 `"0"` のまま。死んだ逆指値を守りと読むと、裸の建玉が entry ゲートも
    `external_adopt_unprotected` も素通りする。状態コードの全体表は一次資料で確認できて
    いないのでコードの whitelist は書けないが、残数量はコード表に依存しない
    (実測値は [../../runtime/TACHIBANA_API_NOTES.md](../../runtime/TACHIBANA_API_NOTES.md))。
  - **`order.Order.HasStopLeg` に `sOrderGyakusasiOrderType` を写す**(0通常 / 1逆指値 /
    2通常+逆指値)。**決済側に注文があることは守りの証明ではない** — 利確指値だけが板にある
    建玉は下方向に裸。判別子の綴りは adapter に閉じ、domain には broker 中立な真偽値だけを
    渡す。取れないとき(`""`)は **false = fail-close**。
  - **`ProbeOrderRows` は read-only の生レコード表示**(`cmd/live-probe -probe-order-rows`)。
    未知の wire 項目の実値を締める前に確かめる用途で、`ProbeMarketPriceColumns` と同じ位置づけ。
    **発注系は含めない** — フラグの付け忘れで実弾が飛ぶ経路を型として作らない。
  - **`APIRequests()` は wire 回数**。broker へ報告する回数の根拠。加算点は `doGET` ただ 1 か所で、**送信の直前**に数える。この 2 つが契約:
    - 数える場所を `doGET` に固定するので、login・`p_errno=2` の再送・一括取得の 120銘柄チャンク分割が**全部入る** = 相手のサーバから見た回数と一致する。`GetTickers` の呼び出し回数で数えると、監視銘柄が 120 を超えて実負荷が倍になっても数字が動かない。
    - 送信**後**ではなく**前**に数えるので、届いたのに手元で失敗した送信(タイムアウト・切断)も入る。過少申告は削減の証拠として使えない。
  - 実アダプタは decorator(`BatchQuoteFeed` → `PaperLiveFeed`)の内側に埋まって外から取り出せないので、`cmd/stockbot` の `buildBrokerSet` が数え口を一緒に返す(paper 構成は nil ではなく常に 0 を返す関数)。`cmd/fetch-daily` は実行終了時に自分の回数を出力する — 日次で一番大きい塊(1銘柄1リクエスト)がここ。
  - **認証材料の読み込みは `TachibanaCredsFromEnv(requireSecondPW bool)` の 1 か所**
    (`tachibana_creds.go`)。`_FILE` が生 PEM より優先で、壊れた鍵は
    初回 login まで遅らせず**起動時に**落とす。発注系は `requireSecondPW=true`(第二暗証番号)、
    参照専用(probe / `fetch-daily`)は `false`。複数のバイナリが同じ読み込みを手写しすると
    順序と規律が綴りごとにずれるので 1 か所に集約する。
  - **`SetUsageRecorder` は永続カウンタの挿し口**。`UsageRecorder` は
    `Record(clmid string)` だけの interface で、**adapter 側が宣言し実装は知らない**
    (`adapter/apiusage` を import しない)。加算は `apiRequests.Add(1)` と**同じ 1 行**で
    行う — 数え口が分かれると定義がずれ、先方の集計と突き合わせられなくなる。
    **login より前に挿す**こと(最初の login も先方から見れば 1 リクエスト。挿し忘れると
    再起動の多い日ほど過少申告になる)。
  - **`QuoteBatchSize()` は分割上限(120 = `broker.DefaultQuoteBatchSize`。cmd/stockbot の上限ログ定数との一致は `TestQuoteBatchSizeMatchesBroker` が固定)の申告**。`BatchQuoteFeed` はこの値からチャンク数を
    数えて間隔を決める(下記)。**申告しないとフィードは「何銘柄でも1リクエスト」と誤解し、
    予算保護が本番だけ効かない**(テストの fake は申告するので緑のまま通る)= 最悪の壊れ方を
    するため、接続は `TestTachibanaDeclaresQuoteBatchSizeToTheFeed` がコンパイル時に固定する。
- [broker/paper_live_feed.go](../../../backend/internal/adapter/broker/paper_live_feed.go) — `PaperLiveFeed`。**実市場フィード(read-only)× 紙執行**の合成 broker(forward 検証用)。フィードは `port.MarketFeed`(発注系を持たない狭い interface)としてのみ保持するので**実弾が飛ぶ経路が型として存在しない**安全設計。Stale quote は紙の帳簿に入れない(`Stale` フラグ付きで呼び手へ返し、評価スキップは呼び手の責務)。フィード取得失敗時は paper の最終価格に落ちず error を返す。
  **デコレータ越しの観測は素通しする**(`QuotesBatched` / `QuoteChunks`)— 内側の
  `BatchQuoteFeed` にしか無い情報なので、素通し経路が無いと 120銘柄超えの警告と
  予算計算が**本番だけ「常に 1」**で回る。
- [broker/batch_feed.go](../../../backend/internal/adapter/broker/batch_feed.go) — `BatchQuoteFeed`(`NewBatchQuoteFeed`)。per-symbol の並行 `GetTicker` を一括取得+キャッシュに畳む `MarketFeed` デコレータ。監視銘柄全部を 1 本のレーンで取り直す(段階ウォッチの別レーンは撤去した — 回数はレーン数 × 頻度で決まり、別レーンは足し算になるだけだった)。
  - **1日の総リクエスト数を監視銘柄数から切り離すのがこの型の契約**。
    上流は 1 リクエストに 120銘柄までしか積めないので、**121銘柄目から 1 回の取り直しが
    2 リクエストに割れる**。間隔が固定だと通信量が階段状に倍増する(建玉は決済まで
    積み上がるので「いつ踏むか」の問題だった)。`effectiveMaxAge = base × chunks`
    (`chunks = ceil(監視銘柄数 ÷ QuoteBatchSize)`)とすることで、
    **`chunks` が約分されて回数が一定になる**。
    - **代償は 120 超えのときの鮮度低下**(3秒→6秒)。黙って通信量が倍になるより、
      観測できる形で解像度を落とす方を選んだ。**1分足のサンプル密度が落ちる**ので、
      分足を検定に使う側が気づけるよう `QuoteChunks()` を外へ出す
      (`cmd/stockbot` が 15 分ごとに見て、段数が変わったときだけ Warn)。
    - **忘却(`quoteForgetFactor`)も実効間隔基準**。間隔だけ伸ばして忘却を据え置くと、
      120 超えのときに「まだ現役の銘柄が刈られて次の tick で登録し直される」振動になる。
    - レーンごとに自分が引く銘柄数で `chunks` を数える(cold は `want` 全体、hot は hot 集合)
      — レーンの回数は足し算なので、片方の銘柄数でもう片方の間隔を決めない。
  - **上流が落ちている間も取り直しの間隔を守る**。「エラー時はキャッシュを
    触らない」(= 古い価格を新鮮なふりで返さない)の副作用で、**全 bundle が各自
    fetch をやり直していた**。実測: 41銘柄・3秒間隔で **正常 60 → 障害時 24,600
    リクエスト/60秒**。レートリミッタ(2 req/s)が頭打ちにするが、それは
    ①場中ずっと 39,600回/日 ②**発注リクエストがこの再取得の行列の後ろで待たされる**
    (リミッタに優先度が無い)を意味する。**②の方が重い — 障害中こそ決済を通したい。**
    直近の失敗を `fetchFailure` としてレーンごとに覚え、次の取り直し時刻まで上流を叩かない。
    **エラーをキャッシュするのではない**: 呼び手には引き続き error を返し、
    価格は決して返さない(`TestBatchQuoteFeedDoesNotStormUpstreamWhileItIsDown` が
    「呼び出し回数」と「価格を返していないこと」の両方を固定する)。
- [broker/ratelimit.go](../../../backend/internal/adapter/broker/ratelimit.go) — `rateLimiter`。broker API 呼び出しの ctx-aware token bucket(立花の呼び出し回数の上限に合わせる)。超過は落とさず**待たせる**(backpressure — 価格を捨てると紙約定が歪む)。ここを通らない API 経路を作らない。

### Repository (in-memory)

- [repository/inmemory.go](../../../backend/internal/adapter/repository/inmemory.go) — `InMemoryPositionRepo`。`ClaimForClose` は OPEN→CLOSING の CAS (CLOSING/CLOSED は `ok=false` の benign skip = 二重 close 防止)。`AdoptExternal` の holding_mode は **exec kind 依存**(`margin_oneday`→`intraday` で引け前フラット化が守る/それ以外→`multiday` — 人の現物・一般信用建玉を bot が引けで強制返済しないため。pg 側と同期)。
- [repository/trade_repo.go](../../../backend/internal/adapter/repository/trade_repo.go) — `InMemoryTradeRepo`。`*BySymbol` 系 (`SumClosedLossJPYSinceBySymbol` / `CountTradesSinceBySymbol` / `ConsecutiveLossesBySymbol`) で per-symbol gate を sibling から隔離。daily-loss は **NET**(net = gross − fee + carry)で判定 — fee で損が深くなるぶん gross より**早く** cap が発火する保守的なブレーキ([CLAUDE.md](../../../CLAUDE.md) 「daily loss cap は net」と一致)。
- [repository/candle_repo.go](../../../backend/internal/adapter/repository/candle_repo.go) — `InMemoryCandleRepo`。symbol+period キーで時系列保持 (backtest/replay 用)。
- [repository/close_saga.go](../../../backend/internal/adapter/repository/close_saga.go) — `Closer`。position repo の lock 下で CLOSING→CLOSED と trade Insert を 1 ステップで行う (withTx close saga の in-memory アナログ)。CLOSING でなければ `ok=false`。
- [repository/rejection_repo.go](../../../backend/internal/adapter/repository/rejection_repo.go) / [repository/screen_snapshot_repo.go](../../../backend/internal/adapter/repository/screen_snapshot_repo.go) — `InMemoryRejectionRepo` / `InMemoryScreenSnapshotRepo`。pg 側と同じ port(`SignalRejectionRepository` / `ScreenSnapshotRepository`)の paper/dev 実装(append-only ミラー)。

### Repository (Postgres)

- [pg/pool.go](../../../backend/internal/adapter/repository/pg/pool.go) — `Open` (pool 生成 + Ping)、`withTx` (commit/rollback、panic 時 rollback して re-panic)。multi-table 書込みは全て withTx 経由。
- [pg/position_repo.go](../../../backend/internal/adapter/repository/pg/position_repo.go) — `PositionRepo`。`ClaimForClose` は `UPDATE ... WHERE id=$1 AND status='OPEN'` の `RowsAffected()==1` で CAS。`AdoptExternal` は sentinel `externalConfigID` ("external_adoption") を参照 (FK 充足)。
- [pg/pair_repo.go](../../../backend/internal/adapter/repository/pg/pair_repo.go) / [pg/journal_repo.go](../../../backend/internal/adapter/repository/pg/journal_repo.go) — 除外する close_reason は `port.NonStrategyCloseReasons()` を `$2::text[]` で渡す(SQL リテラルを持たない)。`ClosedPositions` の戦略名は凍結列 `positions.strategy_name` を先に読み、migration 0015 以前の行だけ `strategy_configs` の join に落ちる(`PairRepo` と同じ)
- [pg/trade_repo.go](../../../backend/internal/adapter/repository/pg/trade_repo.go) — `TradeRepo`。in-memory と同じ per-symbol 系を SQL で実装。
- [pg/closer.go](../../../backend/internal/adapter/repository/pg/closer.go) — `Closer`。`CloseAndRecord` が positions UPDATE (`status='CLOSING'` 条件) + trades INSERT を **1 Tx** で実行 (close saga atomic 性)。`RowsAffected()!=1` なら benign skip。
- [pg/config_repo.go](../../../backend/internal/adapter/repository/pg/config_repo.go) — `ConfigRepo`。`positions.config_id` の FK を満たすため `EnsureExists` で active config / external sentinel 行を idempotent に upsert。
- [pg/candle_repo.go](../../../backend/internal/adapter/repository/pg/candle_repo.go) — `CandleRepo`。`(symbol, interval, open_time)` PK で `ON CONFLICT DO NOTHING` (重複登録防止)。`List` は降順取得後に時系列順へ反転。
- [pg/score_repo.go](../../../backend/internal/adapter/repository/pg/score_repo.go) — `ScoreRepo`(`port.TradeScoreResolver`)。closed trade の「エントリー時に screener が出していた score」を `strategy_configs.advisor_run_id`(migration 0007)→ `advisor_runs.input_json` から復元。曖昧・窓超過は map に入れない(誤リンクはリンク無しより悪い — 推測で埋めない)。
- [pg/rejection_repo.go](../../../backend/internal/adapter/repository/pg/rejection_repo.go) — `RejectionRepo`(`port.SignalRejectionRepository`)。「なぜエントリーしなかったか」の永続監査 (`signal_rejections`。reason は安定種別・detail は可変部 — migration 0009)。
- [pg/screen_snapshot_repo.go](../../../backend/internal/adapter/repository/pg/screen_snapshot_repo.go) — `ScreenSnapshotRepo`(`port.ScreenSnapshotRepository`)。ラウンド毎の全 screen ランキング (`screen_snapshots` — migration 0008)。1 ラウンド ≈ 222銘柄 × 8スクリーナー = 1,776 行なので行単位 INSERT でなく **COPY で 1 往復**。

### DSN ガード (write 経路の二重壁)

- [repository/backtestdsn_guard.go](../../../backend/internal/adapter/repository/backtestdsn_guard.go) — `SafeBacktestDSN` / `SafeIntegrationTestDSN`。db 名が `_backtest` / `_test` で終わり、かつ live `DATABASE_URL` と異なる事の両方を強制。write 経路に置くので生 `go run` でも env 誤設定なら fail-close する。

### Notifier

- [backend/internal/adapter/notifier/stdout.go](../../../backend/internal/adapter/notifier/stdout.go) — `port.Notifier` の stdout 実装(`Stdout`、`var _ port.Notifier = (*Stdout)(nil)`)。

---

## `adapter/advisor` — Claude CLI(port.Advisor 実装・既定 OFF)

`port.Advisor.Generate` を **Claude CLI サブプロセス**で実装。Go から Claude API は叩かず、
`claude -p --no-session-persistence --tools "" --disallowed-tools Bash,Edit,Write,… --model … --effort max`
を実行して stdout を `StrategyConfig` YAML として捕捉する。**ツールを一切与えない**(`--tools ""` で
built-in を無効化 + `--disallowed-tools` で acting 系を permission 層で拒否 = defense-in-depth。ある
CLI バージョンで `--tools` が無視されても行為系ツールは拒否される)ので subprocess は machine 上で
行為できず、broker tool も無いので発注不能。

- [claude_cli.go](../../../backend/internal/adapter/advisor/claude_cli.go) — `ClaudeCLIAdvisor.Generate`。timeout(ctx と TimeoutSeconds の短い方)・usage-limit(リトライしない)・server-side rate-limit/transient(即リトライ)を分類。全失敗モードで `Promotable()=false`。`OutputDir` 設定時は raw stdout を監査アーカイブ(success→`.yaml` / failure→`.fail.yaml`)。
- [parse.go](../../../backend/internal/adapter/advisor/parse.go) — `ResponseParser`(sanitize→fence除去→config_id アンカー→プレビュー排除。go-yaml サニタイズ = HTML実体スペース/コロン欠落/TZラベル) + `classifyCLIOutput`(**serverRateLimit を usageLimit より先に**判定)。
- [prompt.go](../../../backend/internal/adapter/advisor/prompt.go) — `PromptAssembler`(prompt.md + summary JSON を stdin payload 化)。
- 出力の検証・arm は adapter でなく `usecase/command.Promoter` の責務(層分離)。`var _ port.Advisor = (*ClaudeCLIAdvisor)(nil)`。
- **`OutputDir` には入出力の両方**を残す: `*.input.json`(LLM が見た summary)+ `*.yaml` / `*.fail.yaml`(生 stdout)。入力が無いと「なぜその判断か」を再現できない。
- [repository/advisor_run_repo.go](../../../backend/internal/adapter/repository/advisor_run_repo.go)(in-memory)/ [pg/advisor_run_repo.go](../../../backend/internal/adapter/repository/pg/advisor_run_repo.go) — `port.AdvisorRunRepository`(`advisor_runs` 表・migration 0002)。pg は `run_id` PK で **ON CONFLICT DO UPDATE**(best-effort 再 insert が重複で落ちない)。observability 専用で取引経路は読まない。

### 株式分割の言い直し

- pg / in-memory の `ApplySplit` は同じ CAS(OPEN かつ `split_adjusted_on` がその日でない)。pg は日付を `YYYY-MM-DD` の文字列で
  渡し `to_char` で読む(JST 0 時の `time.Time` を DATE に直に渡すと UTC で前日にずれうる)。
- `broker.Paper.AdjustPositionForSplit` は紙の帳簿の株数・建値を揃える(`PaperLiveFeed` は埋め込みで昇格)。

## アンチパターン (= 設計上の落とし穴)

- advisor subprocess に acting tool(Bash/Write/Edit)を許可 → machine 上で行為可能になる。`--tools ""` + `--disallowed-tools` で二重に塞ぐ(`--tools` を無視する CLI バージョン対策)
- server-side 429("not your usage limit")を usage-limit と誤分類 → retry せず判断ロスト。`serverRateLimitRE` を先に判定
- 立花の JSON struct を usecase に流して直接読ませる → adapter で `port.BrokerPosition` / `order.Execution` 等へ変換してから返す
- `pool.Begin` の呼び出しを usecase 側に漏らす → `withTx(ctx, pool, fn)` に閉じ込める
- 信用の返済を建日種類「指定なし」で送る → 立花に拒否される。信用返済は区分を問わず建日順(`'2'`)で送る(ナンピン禁止で 1 銘柄 1 建玉なので一意に決まる。`tachibana_orders.go` の `newOrderFields`)
- 立花 OCO を「リンク無しの TP/SL 二本」で別注文として置く → 両約定して逆ポジを開く。立花は**単一の 通常+逆指値注文 (`'2'`)** で置き、デモ検証 (`STOCKBOT_TACHIBANA_OCO_VERIFIED=1`) 前は fail-close
- 注文一覧の「有効」を約定状態だけで判定する → **取消済が有効として返る**(取消済も未約定 `"0"`)。残数量 > 0 を必ず要求する
- 決済側の注文があることを「守られている」と読む → 利確指値だけの建玉は下方向に裸。`HasStopLeg`(逆指値脚)を必ず要求する
- `paper.GetKlines` に履歴取得を期待する → `nil, nil`。立花 `GetKlines` は**日足のみ**(intraday 履歴は candle repo / backtest 経路から取る)
- backtest/ingest の書込先を live DSN にする → `SafeBacktestDSN` の二重壁を必ず通す

---

## 関連 docs

- [port.md](port.md) — interface 定義 (`port.Broker` / `port.LiveBroker` / `port.*Repository` / `port.Notifier`)
- [../PR_CHECKLIST.md](../PR_CHECKLIST.md) — マージゲート / 不変条件チェック
- [../../workflows/MIGRATIONS.md](../../workflows/MIGRATIONS.md) — DB schema 変更時の repository 影響
- [../../workflows/TESTING.md](../../workflows/TESTING.md) — integration / HTTP fake
- [../../runtime/STATE_MACHINE.md](../../runtime/STATE_MACHINE.md) — position の OPEN→CLOSING→CLOSED 遷移
- [../FAILURE_MODES.md](../FAILURE_MODES.md) — 外部 I/O の失敗ハンドリング (立花 OCO fail-close 等)
- [../../../CLAUDE.md](../../../CLAUDE.md) — 不変条件 (物理分離 / TP-SL broker 側 / DSN 安全)
