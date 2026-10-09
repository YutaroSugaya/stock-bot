# stock-bot アーキテクチャ

ヘキサゴナルの 4 層構成。本ファイルは全体図 + 依存方向 +
禁止事項の凝縮版。各レイヤーの責務・命名・テスト方針の **正本(SoT)は
[architecture/layers/](architecture/layers/)**。目的別の索引は [README.md](README.md)。

```
cmd/stockbot/ (wiring のみ)
  - 起動は `runtimeState` の 10 フェーズ(`loadConfig` → `buildBrokers` → `openStore` → `startFreshnessWatches` → `buildBundles` → `startSelector` → `startAdvisor` → `buildTracks` → `serveHTTP` → `startBackgroundJobs`)。`run()` はその列と shutdown の defer だけ。live トラックは `buildLiveTrack` → `loadLiveBotConfig`(検証)→ `assembleLiveTrack`(`liveTemplates` / `liveBundles`)の 3 段。
   ↓
Handler (app/handler/)            — HTTP 入口(薄い parse→usecase→encode)
   ↓
Usecase (usecase/command/, usecase/query/) ← CQRS
   ↓
Domain (domain/<aggregate>/)      ← 純粋ロジック / I/O・time.Now・rand 禁止
   ↓
Port (port/)                      ← interface のみ(config 非依存 = R1)
   ↑
Adapter (adapter/{broker,repository,advisor,notifier}) ← port を実装(tachibana / paper / pg / in-memory)

Safety (safety/)                  ← 全層から依存可(cross-cutting・リーフ)
```

各レイヤー契約:
[handler.md](architecture/layers/handler.md) /
[usecase.md](architecture/layers/usecase.md) /
[domain.md](architecture/layers/domain.md) /
[port.md](architecture/layers/port.md) /
[adapter.md](architecture/layers/adapter.md) /
[safety.md](architecture/layers/safety.md)

---

## 依存方向のルール(`go build` がコンパイルで強制)

- **上から下にだけ依存**(handler → usecase → domain)。
- usecase は **port**(interface)経由でのみ I/O する。具体 adapter を直接知らない。
- adapter は domain / port / config を import する(逆は禁止)。
- safety はリーフ依存。他から自由に import 可(usecase は local interface 経由で参照)。
- 循環依存は絶対禁止(import cycle = `go build` 失敗)。

依存方向で**捕まらない**規約(domain 純粋性 / R1 / handler→repo 等)は
[`scripts/arch-guard.sh`](../scripts/arch-guard.sh)(= `make guard`)が grep で enforce する。

---

## 禁止事項

- `cmd/stockbot/` に業務ロジックヘルパーを置く(wiring のみ)。
- `usecase` が具体型(`*broker.Tachibana` 等)を知る(interface 経由のみ)。
- `handler` が直接 repository を呼ぶ。
- `domain` が `pgx`/`net/http`/`log/slog`/`os`/`time.Now()`/`rand` を使う(`market` の mutex のみ例外)。
- `port` が `config` を import する(R1 guardrail)。

---

## 横串トピック(各 SoT)

| トピック | ファイル |
|---|---|
| Rollback / Compensate / Trip / DEFER の絶対ルール / Tx / mutex | [architecture/FAILURE_MODES.md](architecture/FAILURE_MODES.md) |
| Position 状態遷移(OPEN→CLOSING→CLOSED)/ close saga / reconcile | [runtime/STATE_MACHINE.md](runtime/STATE_MACHINE.md) |
| DB スキーマ + 不変条件 | [runtime/DATA_MODEL.md](runtime/DATA_MODEL.md) |
| テスト戦略(strict TDD R-G-R + 古典派 + table-driven + integration tag) | [workflows/TESTING.md](workflows/TESTING.md) |
| Backtest / エッジ検定の前提と手順 | [workflows/BACKTEST.md](workflows/BACKTEST.md) |
| Migration 命名 / forward-only / DATA_MODEL 同期義務 | [workflows/MIGRATIONS.md](workflows/MIGRATIONS.md) |
| ハーネス(hooks / git hooks / guard)の有効化 | [runtime/HARNESS_SETUP.md](runtime/HARNESS_SETUP.md) |
| **merge 前必須チェック** | [architecture/PR_CHECKLIST.md](architecture/PR_CHECKLIST.md) |

---

## 永続化の現状

schema SSOT は `migrations/`(0001〜)+ [runtime/DATA_MODEL.md](runtime/DATA_MODEL.md)。
`STOCKBOT_DATABASE_URL` 設定時は Postgres(pgx、adapter は
`internal/adapter/repository/pg/`)、未設定時は同じ port を満たす in-memory 実装
(`internal/adapter/repository/`、paper / test / backtest 用)。CAS(`ClaimForClose`)・
close saga(`CloseAndRecord`)・per-symbol 分離の不変条件は in-memory でも保持。

## ループ

銘柄ごとに price loop — 間隔は `defaultLoopConfig`(純 paper は 1s、実フィードは一括取得なら
`batchedPriceInterval` / 非一括なら `realFeedPriceInterval`)。時価は全銘柄 1 本のレーンで、
env `STOCKBOT_PRICE_INTERVAL_SEC` で上書き可。reconcile loop(30s)は live 限定ではなく
paper 含む全モードで回り、起動時に同期 1 周する
([loops.go](../backend/cmd/stockbot/loops.go))。各 goroutine は
panic recovery + `ctx.Done()` で graceful shutdown。場の時間外は tick skip。

## 読み取り専用ツール層(request path 外)

発注ループの外側に、口座不要・broker 非依存の **read-only な app レベルヘルパー**が数個ある。
domain を再利用するだけで、`port`/`adapter`/DB には触れない。エッジ検定と手動判断の土台。

| package | 責務 | 依存 | 消費者 |
|---|---|---|---|
| `internal/backtest` | daily/分足 CSV ロード + オフライン戦略評価 | `domain`、`config` | `cmd/{backtest,edge-eval,edge-judge,fetch-daily,stockbot}` |
| `internal/advisor` | 単一銘柄の決定論「判定パケット」(screener 結果 + BNF 由来 OCO 幾何を呼値丸めで算出)。**発注しない・口座不要**。BNF の TP/SL は `strategy.BNFReversionExit`(Evaluate と単一ソース)。ニュース/LLM go-no-go はバイナリ**外**(CLAUDE.md: advisor 既定 OFF・リアルタイム発注経路に LLM を入れない) | `domain`(strategy/market/order/position/ta)、`config` | `internal/app`(advisor_loop) |

このほか `backend/cmd/` には発注ループ外の補助 cmd がある:

- `live-probe` — 立花 wire の実機裏取り(本番ホスト**参照系のみ**)
- `forward-report` — forward 台帳(trades)の読み出し面(SELECT のみ。発注 API も書込 SQL も型として無い)
- `fetch-daily` — 立花 API(参照系のみ)で `backend/data` の日足 CSV を更新
- `migrate` — migration の適用(唯一の書込 cmd。live DB は人間承認 env が必須)

## arm ループ(候補の選定と config 生成・既定 OFF)

スキャン → ランキング → **arm**(`holder.Set` で銘柄ごとの active config を差す)までを担う
経路。エントリー自体は決定論エンジン(ゲート → 発注)が別に回すので、この層は
「どの銘柄をどの戦略で待ち受けるか」だけを決める。research トラックでは **paper 強制**。
`mode: live_config` では advisor を登録せず、live は `hard_limits.live_allowed_strategies` の戦略を
`strategy_config.live.yaml` で直接動かす(CLAUDE.md §4)。

> ### config 生成は決定論
>
> `arm_source: template`(既定)では `command.ArmTemplate` が決定論で config を組み立て、この経路に LLM は入らない。
> `arm_source: llm` にすると休眠中の LLM 経路(`port.Advisor` / `adapter/advisor` / `Promoter`)が有効になる。
> 出力の保管先は `advisor_v2.output_dir`。

| package | 責務 | fail-close |
|---|---|---|
| `port.Advisor` | `Generate(summary)→AdvisorRun`(status/parsed YAML)。`Promotable()`=success のみ | 非 success は昇格スキップ |
| `internal/adapter/advisor` | `claude -p`(Read-only tools・model/effort pinned)をサブプロセス実行 → YAML fence 除去 → parse。timeout / usage-limit / transient を分類 | 全失敗モードで arm しない |
| `usecase/command.ArmTemplate` | **決定論の config 生成**。戦略ごとの出荷値(出荷値は `ArmTemplate` を見る)と `ArmDirection` 表からテンプレートを組む。出口幾何は**戦略の `Evaluate` が Signal で返す**ので config には入れない | 作れなければ nil = arm 0 |
| `usecase/command.Promoter` | (LLM 経路のみ)LLM YAML を検証: **mode=paper_config 強制**・symbol 一致・**候補メニュー**(`AdvisorCandidateStrategies` + no_trade)内・**ExpectedStrategy**(round-robin 枠の戦略拘束 — 配られた戦略か no_trade しか通さない)・hard_limits。arm はしない(検証専任) | 違反は reject → arm 0 |
| `usecase/command.AdvisorCycle` | summary→Generate→(非promotable/reject は arm 0)→検証 config を返す。arm は app/cmd 層(holder.Set) | 非 infra 失敗は (nil,run,nil) |
| `app.AdvisorLoop` | **毎分の安価スキャン**(TSE 場中+寄り前 `pre_open_min` 窓)→ universe を決定論 rank。LLM は `shouldAdvise` が「①その日の最初のラウンド(**寄り前ウォームアップ** `pre_open_min` 分前に前倒し — 入力は前日確定足なので判断材料は寄り実行と同一。窓が無効なら寄り付き session_open)②**新規トリガー(立ち上がりエッジ)** ③heartbeat」のときだけ起動 → top 候補の summary(= `advisor.Build` パケットを `MarketSummary.Advisory` に載せる)で AdvisorCycle → `Arm(cfg)` で `holders[sym].Set`。例外: **手動トリガー**(dashboard「▶ 今すぐ判断」→ `StartManual`)は人間起点の観測用で場外でも 1 回だけ走る(単一飛行・panic 隔離・場外実行は session_open/heartbeat 状態を消費しない)— エントリーは risk gate が場中しか許さない | 場外/空/reject は arm 0 |
| `port.AdvisorRunRepository` | 全 run(成功/失敗とも)を `advisor_runs` に監査保存: 入力 summary / 生 stdout / parsed / **LLM が述べた理由**(regime type・confidence・reason)。in-memory と pg の 2 実装 | **best-effort**(書込失敗は log のみ・取引を止めない) |

**配線**(`cmd/stockbot`): `bot_config.advisor_v2.enabled=true`(既定 OFF)のとき AdvisorLoop が arming を担い、
**決定論 Selector の auto-arm は無効化**(競合回避)。`advisor.enabled` は **bot mode paper 必須**(fail-loud)。

**Selector は優先ティア方式**。`app.SelectorTier` を並び順 = 優先順位で持ち、
**前のティアが空き枠を取り切ったら後ろのティアは 1 本も arm されない**。live の配線は
`STOCKBOT_LIVE_STRATEGY_CONFIG` のカンマ区切り(先頭が最優先)。
1 ティアに 2 戦略は入れない(`ValidateSelectorScreeners`)—— score は戦略間で比較不能なので、
**優先ティアは score を一度も跨いで比較しない**ことでその問題を構造的に避ける。
**上位ティアはトリガー済みの候補でしか枠を取らない**: `RankCandidates` は未トリガーの候補も
返すので、素朴に実装すると最優先ティアが毎 Tick 未トリガーの首位で枠を埋め切り、
**下位ティアが永久に arm されない**。最下位ティアだけ従来どおり未トリガーでも arm する。

**Selector は資金条件を 2 つ持つ**: `selector.max_position_notional_jpy`(1単元の建玉金額)と
`risk.max_risk_per_trade_jpy`(**1本あたりの計画損失** = 計画SL幅 × 株数・`WithMaxRiskPerTradeJPY`)。
前者は**ティアごと**(`selector.per_strategy_notional_jpy`)、後者は**ティア共通**。
後者は発注前ゲート(`risk_per_trade`)の**先出し**であって二重化ではない —— ゲートだけだと
上限超の候補が毎 Tick 同じ首位として arm され続け、**口座の建玉枠を永久に占有する**
(沈黙のデッドロック)。判定の境界は両者で同一(`> cap` で切る)。
計画SL幅は `strategy.Candidate.StopLossJPY` が運ぶ(**0 = 算出できなかった**であって
「損失ゼロ」ではない → selector は素通しし、最終判定をゲートに委ねる)。

**3 つ目は口座の資金枠**(`WithLeverageHeadroom`)。順序は **口座 → 候補**:
まず「そもそも 1 本でも建てられるか」(余力 = 保証金 × `risk.max_gross_notional_ratio` − 建玉合計)を
見て、無ければ **1 銘柄も arm しない**(`LeverageFundless` = 枠待ち)。あるなら候補ごとに
①1単元 ≤ ティア上限 ②1単元 ≤ 残余力(同じ Tick で arm したぶんを引きながら)を見る。
判定式は発注前ゲートと同じ `risk.WithinGrossNotionalCap` を**共有**する。
**照会不能は arm しない**(fail-close)—— `risk_per_trade` の「未算出は素通し」と逆に倒すのは、
通してもゲートが `margin_status_unavailable` で必ず落とすので空回りが増えるだけだから。
余力は `command.AccountMarginCache` 経由で読むので、selector が回るたびに wire は飛ばない。
状態は dashboard の「資金枠(レバ余力)」(`selector.leverage_state` / `leverage_headroom_jpy`)。

**2つの allowlist は別物**: `AdvisorCandidateStrategies`(paper で arm してよいメニュー。実際に回すアームは
`advisor_v2.entries` で絞る)と `hard_limits.live_allowed_strategies`(**live 実行**ゲート。中身は
`configs/hard_limits.yaml` を見る)。前者を広げても Promoter が
paper を強制するので live 経路は開かない。既存の `EvaluateHardSafety`(emergency/daily_loss/
session/no-nanpin/引け前フラット)は advisor 由来 config も**絶対バイパス不可**。

## hybrid(実弾 × paper 並走)

**構成**: 1 プロセス・2 トラック(research = 実価格 × 紙執行 / live = 本番口座)。共有するのは立花の
セッションと一括クォートフィードだけで、執行・台帳(DB)・リスク状態・emergency はトラック別。
live は `STOCKBOT_LIVE_BOT_CONFIG` などの env が揃ったときだけ有効になる opt-in(未設定なら単トラックと
同じ挙動)。live は advisor(LLM)を構成として拒否し、銘柄は決定論の selector が選ぶ。

### 却下した案(この形にした理由)

| 却下案 | 理由 |
|---|---|
| 2 プロセス並走 | 立花のセッションは口座に 1 本。login が既存セッションを破棄し(TACHIBANA_API_NOTES §1)、互いに `p_errno=2` → 再ログインの蹴り合いになる。rate limit(2 req/s)も口座共有 |
| 1 エンジン内で戦略ごとに live/paper 執行分岐 | risk gate・config 凍結・ナンピン・台帳が全部「どっちのポジか」の条件分岐になり live 経路が複雑化 |
| 単一 DB + track 列 | 全クエリにフィルタが要り、paper を live 成績として読む事故面が広い。運用 `data` / 検定 `data_research` と同じ「物理で混ぜない」原則に従う |
