# stock-bot — 日本株デイトレ bot(立花証券 e支店 API)

> **English.** A Go trading bot for Japanese equities on the Tachibana Securities e-shiten JSON API, built
> around four engineering decisions: keep the LLM out of the order path, put every protective order on the
> broker's book, fail closed on anything unverified, and reconcile the ledger against broker executions.
> Strategies were screened offline (cost floor, three regimes, day-block bootstrap), then forward-tested
> on paper with pre-registered parameters, then run live with a minimal lot. The verification harness
> concluded that none of the tested strategies had an edge net of costs, so the system was never scaled.
> Docs and comments are in Japanese. MIT licensed. Not financial advice.

## これは何か(30 秒)

- 日本株を立花証券の API で売買する bot。紙執行(paper)・実フィード × 紙執行・実弾(live)の 3 モード、ローカルのダッシュボード、オフラインの backtest / エッジ検定ハーネス。
- 作ったものと同じくらい、**どう検証したか**に重きを置いている。backtest のスクリーン → 事前登録した paper の forward 検定 → 最小ロットの実弾、の順で進め、判定はコード(`cmd/edge-judge`)が出す。
- 結論は「検定したどの戦略も、コスト込みの forward でエッジを示せなかった」。ロットを増やさずに止めた。「無い」と言い切れる仕組みを先に作ったので、裁量で引き延ばさずに済んだ。

技術: Go(標準ライブラリ中心・`pgx` / `yaml.v3` のみ)、Postgres、Docker、macOS launchd、Claude Code の hook。テストは `-race`・integration(実 Postgres)・hook の自己テストまで。

## 設計のポイント(なぜそうしたか)

### 1. LLM を発注経路に入れない
- 発注に至る経路(`usecase/command` → `domain/risk` → `domain/strategy`)は決定論だけで動く。LLM が関わるのは「戦略 config の生成」と「寄り前の go/no-go の表示と記録」で、どちらも既定 OFF、出力は人間が読むファイルに留まる。
- 分離はテストで固定している(`backend/internal/app/gonogo/isolation_test.go`)。発注経路が go/no-go の出力を import した時点で落ちる。
- 理由: 再現性(同じ入力なら同じ発注)、監査(なぜ建てたかを config と台帳で説明できる)、コスト(毎 tick で LLM を呼ばない)。LLM は判事ではなく研究助手として使う。

### 2. 守りは broker 側に置く
- TP / SL は bot の監視ではなく、OCO / 逆指値として broker の板に置く。bot のプロセスが死んでも SL が効く。
- 多日建玉の板の守りは **stop-only**。立花は期日付き注文を翌日に繰り越すとき値幅制限を再検査し、TP 脚が帯の外だと SL 脚ごと失効させる。TP を板に載せると、かえって裸になる。
- 期日は 9 営業日(上限 10 営業日は発注日起点で、訂正では延びない)。残り 3 営業日を切った守りは寄り前に取消 → 再発注し、場中には撃たない。
- 呼値への丸めは domain の 1 か所だけ(`market.RoundToTickOf`)。0.1 刻みを掛け算で作ると二進の誤差で 17 桁の値段になり、注文ごと拒否される。

### 3. fail-close
- 確かめられないものは建てない。保証金の照会失敗 → reject、OCO の実機確認が無い → 発注しない(`STOCKBOT_TACHIBANA_OCO_VERIFIED`)、ユニバースのファイルが読めない → 起動拒否、貸借銘柄の一覧が無い → 全 SELL を reject、live の allowlist が空 → `no_trade` 以外を起動しない。
- 返済の拒否を「建玉が裸」と読まない。拒否されたら建玉照会で broker がまだ持っているかを確かめ、照会が落ちたら緊急停止。
- 緊急停止はファイルベースの write-once で、新規を止めるだけ。既存の建玉を自動では畳まない(畳む判断は人間)。
- 障害の種類ごとの反応(Rollback / Compensate / Trip / DEFER)は [docs/architecture/FAILURE_MODES.md](docs/architecture/FAILURE_MODES.md)。

### 4. 約定は照会で確かめてから記帳する(reconcile)
- 発注の受理は約定ではない。partial fill は約定数量で OCO と建玉を凍結し、残注文を取り消す。発注の通信エラーは「届かなかった」と仮定せず、照会 → 取消 / 補償 → 不能なら緊急停止。
- 決済も同じ。未約定・確認不能・部分約定は CLOSING のまま reconcile に渡し、観測価格では埋めない。
- broker 側の守りが約定した往復は、約定照会の値と実手数料で記帳する。一致しなければ推測せず `reconcile_cold_close` として残す。
- 再起動を跨いでも同じ意味になるよう、建玉時に config / TP / SL / 保有期限を Position に凍結する。設定を切り替えても既存の建玉には効かない。

### 5. 不変条件をテストで固定する
- ヘキサゴナル 4 層 + CQRS。`domain` は純粋(`time.Now()` と `rand` は注入、`net/http` / `pgx` / `slog` を import しない)。層規約は `scripts/arch-guard.sh` が grep で機械強制する。
- 「人間の commit でしか変えられない値」は yaml とテストの両方に書く。片方だけ変えると落ちる(`catastrophe_guards_test.go`: live に許す戦略・銘柄のホワイトリスト・保証金の定数)。
- strict TDD(Red → Green → Refactor)。マージゲートは `go test -race` + `vet` + `build`。

### 6. AI エージェントで書く前提のガードレール
- コードの大半は Claude Code で書いた。だから「AI に壊させない」仕組みを同梱している: 危険な操作(bot の起動停止・DB の破壊・ハーネス自身の改変)を hook で deny、Stop 時に secret-scan・層規約・テストを強制、commit と push の 2 層で秘密情報を拒否。
- hook 自体にも自己テストがある(`make guard`)。設計は [docs/runtime/HARNESS_SETUP.md](docs/runtime/HARNESS_SETUP.md)。

## 検証の進め方と結論

| 段階 | 何をするか | 落とす基準 |
|---|---|---|
| backtest のスクリーン | `cmd/backtest`(決定論リプレイ)→ `cmd/edge-eval` | コスト床を超えない / ¥1M 正規化で負け / 3 レジームのうち 2 つで負け / ベンチマークを超えない / day-block bootstrap の CI が 0 を跨ぐ |
| paper の forward 検定 | パラメータを事前登録して紙で回す。兄弟アーム(`X` / `X_trail`)は入口が同じで出口だけ違うので、本命の統計量はペア差 | N が足りない / OOS で符号が変わる / 事前登録した基準に届かない。結果を見てからパラメータを動かさない |
| 最小ロットの実弾 | 守り(受理・常駐・発動・bot 停止下の約定)と reconcile を実機で確かめる | 守りが板に無い状態が観測されたら止める |
| 判定 | `cmd/edge-judge` が機械で出す | 人間は判定を上書きしない。ロット(株数)を上げるのはエッジの証明後だけ |

結論: 25 日線乖離の逆張り系・トレンド系(donchian / ATR breakout / モメンタム)のいずれも、forward でコスト込みのエッジを示せなかった。
paper の黒字は特定の期間と少数の銘柄に依存していて、期間を伸ばすと消えた。だからロットを増やさず、戦略を増やして探し続けることもしなかった。

学んだこと:
- **検査装置が配線より先**。発注の配線は安いが、エッジの有無を測れる装置が無いと、単一レジームの偶然を本物と誤認する。
- **自分を騙さない規律はコードに書く**。事前登録・1 回だけ回す・格子の最良セルを採らない・不採用も記録する。文書の規律は守られない。
- **LLM は判事より研究助手**。急落銘柄の go/no-go を LLM に判定させると、ほぼ全部を no_go にした。仮説の列挙には使え、発注の可否には使わない。

規律の全文は [docs/runtime/EDGE_METHODOLOGY.md](docs/runtime/EDGE_METHODOLOGY.md)、立花 API で実際に踏んだ仕様(レート上限・OCO・期日・値幅制限)は [docs/runtime/TACHIBANA_API_NOTES.md](docs/runtime/TACHIBANA_API_NOTES.md)。

## できること

- **3 つの執行モード**: `paper`(偽フィード × 紙執行・口座不要)/ `paper_live_feed`(立花の実フィード × 紙執行)/ `live`(立花の本番口座で実弾)。
- **戦略カタログ**(`backend/internal/domain/strategy/catalog.go`): 25 日線乖離 × 出来高の逆張り(bnf 系とその trail / day2 / intraday 変種)、post-jump drift、高出来高、52 週高値、donchian / ATR breakout、絶対モメンタム、時系列モメンタム、`no_trade`。
- **守り**: OCO / 逆指値・ナンピン禁止・config 凍結・emergency stop・引け前フラット化・値幅制限と貸借銘柄の検問・保証金と維持率の確認・株式分割の権利落ち対応・再起動後の reconcile。
- **検定ハーネス**: `cmd/backtest` → `cmd/edge-eval` → `cmd/edge-judge`。forward(paper)の集計は `cmd/forward-report`。
- **日次ユニバース**: `cmd/universe-screen` が毎朝 `hard_limits.allowed_symbols` から規則で選ぶ。
- **ダッシュボード**: `http://localhost:8090`(PAPER / LIVE・建玉・損益・緊急停止・銘柄ごとの新規停止)。
- **永続化**: `STOCKBOT_DATABASE_URL` があれば Postgres、無ければ in-memory。

## 前提

| 用途 | 必要なもの |
|---|---|
| ビルド・テスト・backtest・paper | Go 1.25 以上(`backend/go.mod` の `toolchain` 行の版を `GOTOOLCHAIN=auto` が自動取得する) |
| 永続化(forward 記録・live) | Docker(Postgres 16。`docker-compose.yml`) |
| 実フィード・日足取得・live | 立花証券 e支店の口座と API(公開鍵認証の認証 ID・秘密鍵・第二パスワード) |
| 定期ジョブ(朝の日足取得・ユニバース選定・週次集計・DB backup) | macOS の launchd(`scripts/com.stockbot.*.plist`) |
| `make start` | `claude` CLI(advisor の自己更新を起動前に呼ぶ) |

## セットアップ

```bash
git clone git@github.com:YutaroSugaya/stock-bot.git && cd stock-bot
cd backend && go build ./... && go vet ./... && go test -short ./... && cd ..
```

### .env(立花の認証とパス)
```bash
cp .env.example .env
```
`.env.example` のコメントに従って埋める。主なキー:

| キー | 内容 |
|---|---|
| `STOCKBOT_TACHIBANA_ENV` | `demo` / `production` |
| `STOCKBOT_TACHIBANA_AUTH_ID` / `STOCKBOT_TACHIBANA_PRIVATE_KEY_FILE` / `STOCKBOT_TACHIBANA_SECOND_PASSWORD` | 立花の公開鍵認証(標準 Web 画面で発行) |
| `STOCKBOT_HARD_LIMITS` / `STOCKBOT_DAILY_CANDLES_DIR` | `configs/hard_limits.yaml` と日足 CSV の置き場所 |
| `STOCKBOT_DATABASE_URL` | research 台帳の Postgres DSN(無ければ in-memory) |
| `STOCKBOT_LIVE_*` | live トラックの config / DSN / emergency flag(research と別にする) |
| `STOCKBOT_HTTP_ADDR` / `STOCKBOT_API_TOKEN` | 制御 API の bind と token |

秘密鍵は `secrets/`(gitignore)に置く。`.env` と `*.pem` は pre-commit が拒否する。

制御 API(ダッシュボード)は既定で `127.0.0.1:8090` だけに bind する。`0.0.0.0` など loopback 以外に向けるなら `STOCKBOT_API_TOKEN` が必須で、変更系(POST)も参照系(GET)も `X-Api-Token` か `Authorization: Bearer` で照合する。token 無しの非 loopback bind は全部 403(fail-close)。ダッシュボードの JS は token を送らないので、画面は実質 loopback 専用。

### Postgres(任意)
```bash
docker compose up -d db
STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 make migrate-up
make migrate-status
```
DB 名は `docker-compose.yml` の `POSTGRES_DB` と `.env` の DSN を揃える。live 用の台帳は別 DB にして `make migrate-live-up`。

### データ
- 運用の日足は `backend/data/<sym>_daily.csv`(`make fetch-daily` が立花から取得・マージ。要 `.env`)。
- 検定用の日足は `backend/data_research/`(gitignore)。CSV の形式と入手の方針は [docs/runtime/EDGE_TESTING_DATA.md](docs/runtime/EDGE_TESTING_DATA.md)。
- どちらも同梱しない。

## 動かす

### paper(口座不要)
```bash
make run                     # configs/bot_config.yaml + configs/strategy_config.active.yaml・in-memory
open http://localhost:8090
```
同梱の `strategy_config.active.yaml` は `no_trade` なので何も建たない。`strategy_name` をカタログの名前に替える。

### research(実フィード × 紙執行)+ live
```bash
make start                   # .env を自動読込。live は .env の STOCKBOT_LIVE_* が揃っていれば一緒に起動
make stop
```
- research は `configs/bot_config.advisor.yaml`。`advisor_v2.entries` に並べた戦略を同時に紙で回し、forward 記録を貯める。監視銘柄は `symbols_file`(`cmd/universe-screen` の出力)から読む。
- live は `configs/bot_config.live.yaml` と `configs/strategy_config.live.yaml`(gitignore。雛形は `configs/*.live.example.yaml`)。`mode: live_config` は `hard_limits.live_allowed_strategies` にある戦略しか起動せず(同梱は空)、`STOCKBOT_LIVE_CONFIRMED=1` と `STOCKBOT_TACHIBANA_OCO_VERIFIED=1`(OCO の実機確認後)が要る。
- **`make start` は起動以外のことも自動でやる**(`scripts/stockbot-routine.sh`)。知らずに打たないこと:
  `claude update`(CLI の自己更新・ネットワーク)と `claude -p` の疎通確認(課金)、Docker Desktop の起動と `docker compose up`、DB が空なら最新 dump からの復元(打つこと自体を承認とみなす)、
  日足が古ければ `make fetch-daily`(銘柄数ぶんの立花 API 呼び出し)、秘密鍵と env の `~/.stockbot/` への複製、ビルド済みバイナリの `~/.stockbot/bin` への配置、
  launchd ジョブ 4 本(`com.stockbot.{morning,weekly,db-backup,gonogo}`)の `~/Library/LaunchAgents` への設置と load、寄り前の go/no-go(`claude` を銘柄ごとに呼ぶ・課金)。
  引け後の日次総評の段 2(`claude` + WebSearch・課金)は `STOCKBOT_DAILY_REVIEW_STAGE2=on` を立てたときだけ走る(既定 off)。
- 定期ジョブだけを入れるなら `make routine-install`(平日 07:00 の日足取得 + ユニバース選定 + 集計・金曜の週次集計)と `make backup-install`(DB backup)。
  plist の `/Users/USERNAME` は `make start` 経由なら自動で `$HOME` に置換される。`make routine-install` を直接使うときは先に置換する:
  ```bash
  sed -i '' "s|/Users/USERNAME|$HOME|g" scripts/com.stockbot.*.plist
  ```
  launchd は `~/Desktop` 配下を読めない(macOS の TCC)ので、実体は `~/.stockbot/` に置く。

### バックテストとエッジ検定
```bash
cd backend
go run ./cmd/backtest -csv data_research/7203_daily.csv -symbol 7203 -interval 1d -strategy bnf_reversion -holding multiday -json > out.json
go run ./cmd/edge-eval -glob 'out.json' -benchmark data_research/bench_topix.csv
go run ./cmd/edge-judge -in net.json
make forward-report          # paper の forward 記録の集計(要 STOCKBOT_DATABASE_URL)
```
手順は [docs/workflows/BACKTEST.md](docs/workflows/BACKTEST.md)。

## 設定ファイル

| ファイル | 役割 |
|---|---|
| `configs/hard_limits.yaml` | 人間が commit する上限: 銘柄のホワイトリスト、貸借銘柄、live に許す戦略、休場日、保証金の定数 |
| `configs/bot_config.*.yaml` | トラックごとの実行設定(mode / broker / risk / advisor / holding) |
| `configs/strategy_config.*.yaml` | 戦略 1 本の入口・出口・サイズ(建玉時に Position へ凍結される) |

値はコードのコメントと各 yaml に書いてある。変えると落ちるテスト(`catastrophe_guards_test` 等)が番人。

## 構成

```
cmd/stockbot ─▶ app(配線・ループ)─▶ handler(HTTP・薄い)─▶ usecase(command / query・CQRS)─▶ domain(純粋)
                                                                      │
                                                                      ▼ port(interface)◀── adapter(broker / repository / advisor)
                                                                      safety(emergency・hard limits)
```
層の契約は [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) と [docs/architecture/layers/](docs/architecture/layers/)。守りの不変条件の全文は [CLAUDE.md](CLAUDE.md) §3。

`backend/cmd/`: `stockbot`(本体)/ `backtest` `edge-eval` `edge-judge` `counterfactual` `pair-diff` `holding-period`(検定)/
`fetch-daily` `universe-screen` `loanable-fetch` `tick-table`(データ)/ `forward-report` `daily-review` `gonogo` `gonogo-score`(集計と判定)/ `migrate` `live-probe`。

## 既知の制限

- 守りのうち「逆指値の発動方向」と「bot 停止中の約定」は本番で未実証。`STOCKBOT_TACHIBANA_OCO_VERIFIED` を立てるのは実機で確かめてから。
- selector は `buy_only` の戦略でも売り候補を arm して枠を占有する(売りを出す戦略を live で使う前に直す)。
- paper は 1:1.2 のような小さい株式分割を拾えない(live は broker の建玉照会で拾う)。
- `make start` に二重起動のガードは無い。2 つ目のプロセスも立花にログインし、1 つ目のセッションを失効させる。
- 立花で使えるのは現物と制度信用 6 ヶ月だけ。一日信用は API に区分が無い。

## 検証

```bash
make check-backend           # go test -race + vet + build(マージゲート)
make check                   # + 層規約ガード + hook の自己テスト + gofmt
make test-integration        # 実 Postgres(末尾 _test の DSN が必須)
```

## Claude Code で開くとき

`.claude/settings.json` と `.claude/hooks/` はリポジトリを Claude Code で開いた瞬間に効く。bot の起動と停止・DB の破壊・ハーネス自体の改変を deny し、
Stop 時に secret-scan・層規約・`make check-backend` を回す。外すには `.claude/settings.json` を消すか、`STOCKBOT_PRESTOP_CHECKS=off` / `STOCKBOT_TDD_CHECK=off` を立てる。
deny hook は `make start` / `make stop` / `go run ./cmd/stockbot` / `bin/stockbot` も止めるので、エージェント経由では bot を起動できない(起動と停止は人間がターミナルで打つ)。
設計は [docs/runtime/HARNESS_SETUP.md](docs/runtime/HARNESS_SETUP.md)。

## ドキュメント

[docs/README.md](docs/README.md) が目的別の索引。

## ライセンスと免責

MIT([LICENSE](LICENSE))。投資助言ではなく、利用によって生じた損失について作者は責任を負わない。実弾で動かす前に「既知の制限」を読むこと。立花証券 e支店 API・JPX のデータ・各データ提供元の利用規約は各自で確認すること。
