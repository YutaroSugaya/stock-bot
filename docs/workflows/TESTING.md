# Testing — strict TDD (Red→Green→Refactor) + 古典派 + table-driven + integration tag

## 役割(1行)

stock-bot のテスト規律。strict R-G-R サイクル(Refactor 段の省略禁止)と古典派 mock 方針を両輪とし、層ごとのテスト種別と merge gate(`make check-backend`)を定義する不変条件。

---

## 1. 層ごとのテスト種別

| 層 | テスト種別 | mock / fake の使い方 |
|---|---|---|
| Domain | 純粋単体 | 不要。値の入出力だけ([domain.md](../architecture/layers/domain.md)) |
| Usecase (Command/Query) | 古典派単体 | 実 in-memory collaborator + safety は local interface fake([usecase.md](../architecture/layers/usecase.md)) |
| Adapter (Repository / Postgres) | 統合 | `//go:build integration` + 実 DB。`make test-integration` 経由のみ([adapter.md](../architecture/layers/adapter.md)) |
| Adapter (Broker tachibana) | HTTP fake | `httptest.NewServer` で fixture response を返す([adapter.md](../architecture/layers/adapter.md)) |
| Handler | 単体 | `net/http/httptest`([handler.md](../architecture/layers/handler.md)) |

> 注記: `backend/internal/app/handler/` の httptest テストは**整備済み**(`handler_test.go` / `advisor_runs_test.go` / `advisor_trigger_test.go` / `performance_test.go`)。ルート追加時は同じ形で必ずテストを足す([handler.md](../architecture/layers/handler.md))。

---

## 2. 古典派 (Classical / Detroit) TDD

**極力 mock や stub は使わず、実装をそのまま動かして検証する。**

Command/Query は **`broker.Paper` や real `repository.Closer`(with in-memory repo)を組み合わせて end-to-end に近い形でテスト** する([command_test.go](../../backend/internal/usecase/command/command_test.go) の `newHarness` がその雛形)。これによりリファクタで内部実装が変わってもテストは挙動を継続的に保証する(= 仕様ベース)。

### fake/mock の使用は以下 3 用途に限定

1. **システム境界**(= プロセス外部)を切り出すとき
   - 実 DB に繋げない単体テストでは Postgres を [`repository.NewInMemoryPositionRepo` / `NewInMemoryTradeRepo`](../../backend/internal/usecase/command/command_test.go) で代用する
   - 立花 REST は [`httptest.NewServer`](../../backend/internal/adapter/broker/tachibana_test.go) で fixture response を返す
   - CLI / 外部プロセスは関数注入で切る
2. **特定の失敗シナリオを注入したいとき**
   - emergency trip 後の entry block、oneday+multiday reject、stale reconcile の defer 等
   - その目的に特化した最小限の fake を使う
3. **時刻・乱数など非決定性を排除したいとき**
   - `time.Now` を [`clock.Fixed(now)`](../../backend/internal/usecase/command/command_test.go) で固定する(domain は `clock.Clock` 注入。`time.Now()` 直呼び禁止)
   - SignalID 生成は `strategy.NewEngine(func() string { return "sig-1" }, ...)` のように caller(wiring 層)から関数注入する

> **各 mock/fake の file/type コメントに 3 用途のどれに該当するか明記する**(例: [tachibana_test.go](../../backend/internal/adapter/broker/tachibana_test.go) の `// mockTachibana spins up an httptest server emulating the 立花 endpoints ...`)。

### 禁止

動作する `usecase/command` の実装を test 用 `mockX` で置き換えること。

### 理由

- mock 過多のテストは **テスト自身が二度書き** となり、リファクタ抵抗が高まる
- 実装と乖離した mock は **嘘の安全感** を生む(mock は通るが実装は壊れている)
- usecase の **本物の協調**(close saga / CAS / per-symbol 分離)を検証できないと、結合不具合を見逃す

---

## 3. テーブル駆動テスト (Table-Driven)

**同じ動作軸で複数ケースを検証するときは必ず table-driven にする。** domain の純粋関数がその第一候補:

```go
func TestTPSLPricesFromJPY(t *testing.T) {
    cases := []struct {
        name           string
        side           order.Side
        entry, tp, sl  float64
        wantTP, wantSL float64
    }{
        {"buy",  order.SideBuy,  2500, 20, 10, 2520, 2490},
        {"sell", order.SideSell, 2500, 20, 10, 2480, 2510},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            tp, sl := TPSLPricesFromJPY("7203", tc.side, tc.entry, tc.tp, tc.sl)
            if tp != tc.wantTP || sl != tc.wantSL {
                t.Fatalf("%s: tp/sl = %g/%g, want %g/%g", tc.name, tp, sl, tc.wantTP, tc.wantSL)
            }
        })
    }
}
```

> 現状 [position_test.go](../../backend/internal/domain/position/position_test.go) は `TestTPSLPricesFromJPY_Buy` / `_Sell` / `_RoundsToTickGrid` を **個別関数** で持つ(実体は [price.go](../../backend/internal/domain/position/price.go) の `TPSLPricesFromJPY` — 呼値丸めは symbol から引く)。同一動作軸(buy/sell/tick 丸め)を増やすときは上記の table 形に集約する。

### ルール

- ケース名は短く目的を示す(`"buy"`, `"sell"`, `"tick_rounding"`)
- `t.Run(tc.name, ...)` で必ずサブテスト化(`-run` でケース単独実行可能に)
- 失敗時のメッセージに `tc.name` か入力値を含める(どのケースが落ちたか即わかる)
- ケース数が増えて table 列が **8 を超えるならテストを分割**

### 例外

- 完全に異なるセットアップが必要なケース(e.g. `HoldingMode=intraday` vs `multiday`、`mode=paper` vs `live`)は別の `Test...` 関数
- 1 ケースしかない検証(`TestMaxHoldUntil` 等)は table にしない

---

## 4. テスト命名

```
TestTradingCycle_EntersAndExecuteSagaCreatesPosition
TestTradingCycle_RejectsOnedayHoldingMultiday
TestTradingCycle_EmergencyBlocksEntry
TestTachibana_OCOFailCloseUntilVerified
```

`<Type>_<Scenario>_<ExpectedBehavior>` を基本とする([command_test.go](../../backend/internal/usecase/command/command_test.go) 準拠)。table-driven のサブテスト名は `_` 区切り(`/` は go test runner が階層として扱うため避ける)。

---

## 5. strict TDD: Red → Green → Refactor(3 段すべて必須)

> **Refactor 段の省略は禁止**([CLAUDE.md](../../CLAUDE.md) マージゲート: strict TDD「Red → Green → Refactor、Refactor 必須」)。Refactor は cycle の 1/3 の比重で、「ついでに refactor もしておきました」ではなく **cycle の中で必ず通る関門**。

### 5.1 Red — 失敗するテストを先に書く

- 期待挙動を assertion で表現する。
- **compile error / assertion error で「実際に失敗していること」を目視確認** してから Green に進む。「書いた瞬間に通ってしまった」テストは Red になっていない(= 既存挙動を観測しているだけで仕様駆動でない)。テストを inversion させて本当に落ちることを確認する。

### 5.2 Green — テストを通すだけの最小コードを書く

- 設計や命名は二の次。重複 OK、命名雑 OK、関数長い OK。とにかく green を作る。
- 「ここで設計を整えたい」誘惑は Refactor に回す。

### 5.3 Refactor — テストを green に保ったまま設計を整える(= **必須**)

Refactor 段で最低限点検する:

- **命名見直し**(Green で雑に命名した変数 / 関数を意図が伝わる名前に)
- **重複削除**(Green で許した duplication を helper / 関数抽出で集約。例: `newHarness` / `rewire` のような共有 setup)
- **関数抽出 / インライン化**(1 関数 1 責務に近づける)
- **SOLID 違反の解消**(特に SRP / DIP のズレ)
- **mock の必要性再評価** — Green で fake を増やしたなら、それが [§2 古典派 3 用途](#2-古典派-classical--detroit-tdd) のどれに該当するかを自問する。該当しないなら **real な実装**(`repository.NewInMemory*Repo` / `broker.NewPaper` 等)に置換する。

**完了の判定基準**: 「次に書きたいテスト / コードがすぐ書ける状態」になったら完了。

### 5.4 PR / commit 単位の規律

- **Refactor を skip した PR は不可**。
- PR description に **Refactor 段階で何をしたか** を 1 項目以上書く(命名変更 / 関数抽出 / 重複削除 / mock 置換 のいずれか)。
- Refactor が「特に無し」になりそうなら Green が雑でなかった or 本当に小さい変更。後者なら問題ないが、前者は Red を強化して再 cycle。

### 5.5 新規コードへの適用範囲

- 新規 usecase / domain service / adapter 追加時は **必ず strict R-G-R**。
- 既存コードへの bug fix も同じ — まず regression test を Red で書き、Green で fix、Refactor で関連 callsite / helper を整理。
- adapter(HTTP fake)は §2 用途①(システム境界)なので Refactor 段では「fixture が現行 API spec と乖離していないか」を確認する。

---

## 6. Integration test (build tag `integration`)

Postgres など外部依存を持つテストは build tag `integration` で分離する。ファイル先頭は必ず `//go:build integration`([pg/integration_test.go](../../backend/internal/adapter/repository/pg/integration_test.go)):

```go
//go:build integration

package pg

func requireDB(t *testing.T) (*pgxpool.Pool, func()) {
    testDSN := os.Getenv("INTEGRATION_TEST_DB_URL")
    if testDSN == "" {
        t.Skip("INTEGRATION_TEST_DB_URL not set")
    }
    if err := repository.SafeIntegrationTestDSN(testDSN, os.Getenv("STOCKBOT_DATABASE_URL")); err != nil {
        t.Fatalf("unsafe integration DSN: %v", err)
    }
    // ... open real pool, truncateAll, exec
}
```

### 実行コマンド(`make test-integration` 経由のみ)

```bash
export INTEGRATION_TEST_DB_URL='postgres://.../stockbot_test?sslmode=disable'  # 末尾は必ず _test
make test-integration
```

通常の `make test` / `make test-race` は `integration` タグを付けないので **これらのテストはスキップされる**(Docker が無い環境でも安定して通る)。タグ付きのコードが黙ってコンパイル不能になるのを防ぐため、`.githooks/pre-push` が `make vet-integration`(= `go vet -tags integration ./...`)を回す。

### ⚠️ 生で `go test -tags integration` を打たない(本番 DB 保護)

- **生 `go test -tags integration` は deny hook が拒否** する([.claude/hooks/pretooluse-deny.sh](../../.claude/hooks/pretooluse-deny.sh): `raw 'go test -tags integration'(実 DB を truncate)。make test-integration を使う`)。理由: integration suite は `truncateAll` で `positions_live / trades / positions / candles / signal_rejections / strategy_configs` を `TRUNCATE ... RESTART IDENTITY CASCADE` する([pg/integration_test.go](../../backend/internal/adapter/repository/pg/integration_test.go))。
- **DB 名は接尾辞 `_test` を必ず付ける**。`make test-integration` は `INTEGRATION_TEST_DB_URL` 未設定なら **`INTEGRATION_TEST_DB_URL must be set (and its DB name must end in _test)` で exit 1**、`_test` 接尾辞違反も **`DB 名は _test で終わる必要があります` で exit 1**([Makefile](../../Makefile) のガード)。
- **二重壁(double-wall)** で fail-close する: `repository.SafeIntegrationTestDSN` が ① db 名が `_test` で終わること ② live の `STOCKBOT_DATABASE_URL` と異なること を強制する([backtestdsn_guard.go](../../backend/internal/adapter/repository/backtestdsn_guard.go))。env が mis-set でも閉じる。
- DB は SELECT のみが原則。書込は migration / 人間承認(`STOCKBOT_HUMAN_APPROVED_DB_WRITE=1`)のみ([CLAUDE.md](../../CLAUDE.md))。

### 現在の integration ファイル

- [backend/internal/adapter/repository/pg/integration_test.go](../../backend/internal/adapter/repository/pg/integration_test.go)

---

## 7. Race detector (`-race`)

`go test -race` を **必須** で通す。`make test-race`(= merge gate `make check-backend` の一部)が race を有効にする。

- mutex を取らずに共有 state を書き換えていないか(domain で mutex が許されるのは `domain/market` の rolling window のみ。[CLAUDE.md](../../CLAUDE.md) 層規約)
- channel / goroutine の競合がないか
- CAS / per-symbol close saga が並行下でも壊れないか([command_test.go](../../backend/internal/usecase/command/command_test.go) の reconcile / claim 系)

---

## 8. Merge gate と層ガード

- **merge gate = `make check-backend`** = `test-race`(`go test -race ./...`)+ `vet`(`go vet ./...`)+ `build`(`go build ./...`)が全て緑([Makefile](../../Makefile) / [PR_CHECKLIST.md](../architecture/PR_CHECKLIST.md))。
- **層規約の grep ガードは [scripts/arch-guard.sh](../../scripts/arch-guard.sh)**(domain 純粋性 / R1 / handler→repo 直呼び等を機械 enforce)。`_test.go` は domain 純粋性チェックから除外される。
  - `make guard`(= `bash scripts/arch-guard.sh`)として [Makefile](../../Makefile) に**定義済み**。`make check` は check-backend + guard + fmt-check を一括で回す。
- coverage 閾値(`make test-cover` 等)は **現状 Makefile に未定義**(未定)。代わりに PR レビューで未カバーのクリティカルパスを指摘する。
- 静的解析(マージゲートの外・手で回す): staticcheck と deadcode は **`go run` で最新版を使う**。
  インストール済みのバイナリは古い Go でビルドされていると、このモジュールの Go 版を解析できない。
  `go run` は必要なら新しい Go ツールチェーンを自動で取ってくる(初回はネットワークが要る)。

  ```bash
  cd backend
  go run honnef.co/go/tools/cmd/staticcheck@latest ./...          # 全チェック(U1000 = 未使用も含む)
  go run golang.org/x/tools/cmd/deadcode@latest -test ./...        # 本番からもテストからも呼ばれない関数
  ```

  どちらも 0 件が正。テストからしか呼ばれない本番の関数は deadcode には出ないので、
  テストは本番と同じ関数を通す(薄いラッパを本番側に置かない)。

---

## 9. 共有テストヘルパ

shared helper パッケージ [backend/internal/testutil](../../backend/internal/testutil/testutil.go) を**整備済み**(`SilentLogger` / `TempFlagPath` — ログ抑止と一時ファイルの足回りだけを置き、fake/mock のたぐいは置かない。production コードからは import しない)。寄せるのは**綴りがずれると意味が変わる重複だけ**: `clock.Fixed` や `math.Abs` の直呼びはラップしない(1 行のラッパを挟む利点が無く、誰にも呼ばれないまま残るだけになる)。パッケージ固有の setup は従来どおり各テストファイル内の `t.Helper()` 付きヘルパ([command_test.go](../../backend/internal/usecase/command/command_test.go) の `newHarness` / `rewire`、[pg/integration_test.go](../../backend/internal/adapter/repository/pg/integration_test.go) の `requireDB` / `truncateAll`)に置く。

---

## 10. 一時的に skip するテスト

- skip 理由を必ず文字列で書く(`t.Skip("INTEGRATION_TEST_DB_URL not set")`)。
- 環境依存スキップ以外は `// TODO:` で再開条件を書く。
- `t.SkipNow()` の単独使用は禁止(理由不明)。

---

## 11. アンチパターン

- `usecase` のテストで全 dependency を `mockBroker` / `mockRepo` / `mockNotifier` で書いた(→ `broker.Paper` + in-memory repo で組む)
- table case を 10 ケース以上 + 列を 12 列にして読めなくした
- `t.Helper()` を使わずに helper 内で `t.Fatalf` した(失敗時スタックトレースが helper 行になる)
- race 関連バグを「テスト不安定」として `t.Skip` した(= 根本原因を直す)
- **Red 段階を飛ばして Green から書き始めた**(= テストが既存挙動を観測しているだけで仕様駆動でない)
- **Refactor 段を skip した**(「Green で通ったから OK」は cycle 不完全。次の cycle で技術的負債を踏む)
- Green で増やした fake を、Refactor で「§2 古典派 3 用途のどれに該当するか」を自問せずそのまま PR に出した
- 生で `go test -tags integration` を実行した(deny hook が拒否。台帳テーブルを TRUNCATE する)
- `INTEGRATION_TEST_DB_URL` を `_test` 接尾辞なし / live と同一 DSN に向けた(`SafeIntegrationTestDSN` が fail-close する)

---

## 12. 関連 docs

- [../architecture/layers/domain.md](../architecture/layers/domain.md) — 純粋単体テスト / `clock.Clock` 注入
- [../architecture/layers/usecase.md](../architecture/layers/usecase.md) — 古典派 collaborator 選択
- [../architecture/layers/handler.md](../architecture/layers/handler.md) — httptest による handler テスト(整備済み)
- [../architecture/layers/adapter.md](../architecture/layers/adapter.md) — integration tag + HTTP fake
- [../architecture/layers/port.md](../architecture/layers/port.md) — port interface(usecase の fake 差し替え先)
- [../architecture/layers/safety.md](../architecture/layers/safety.md) — emergency / hard safety の trip 検証
- [../architecture/PR_CHECKLIST.md](../architecture/PR_CHECKLIST.md) — merge 前 `make check-backend` チェック
- [../../CLAUDE.md](../../CLAUDE.md) — マージゲート / strict TDD / 層規約の不変条件
