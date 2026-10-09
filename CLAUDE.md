# stock-bot — 絶対ルール (CLAUDE.md)

日本株デイトレ bot(立花証券 e支店)。本書は**不変条件だけ**を書く。数値は config(`configs/*.yaml`)、障害時の挙動は
[FAILURE_MODES](docs/architecture/FAILURE_MODES.md) にある。設計の詳細は [docs/README.md](docs/README.md) から引く。

## 1. AI の作業境界

- live DB は SELECT のみ。書込は migration か、人間が個別に指定して `STOCKBOT_HUMAN_APPROVED_DB_WRITE=1` を前置したときだけ。DB を wipe しない(`docker compose down` / `DROP` / `TRUNCATE` 等)。稼働中に postgres を止めない。
- **bot の起動と停止は人間だけ**: `make start` / `make stop`、`go run ./cmd/stockbot`、ビルド済みの `bin/stockbot` の実行。`make start` には二重起動のガードが無く、2 つ目のプロセスも立花にログインして 1 つ目のセッションを失効させ、ログインの回数を増やす(立花は 1 日 1 回を求めている)。`make start` の起動前チェック(`ensure_db`)は DB が無い / 空なら最新 dump から戻し、打つこと自体がその承認になる。中身のある DB には書かず、dump より後に bot が動いていたら戻さずに止まる。起動・再起動が要るときは人間に頼んで待つ。
- 止める・消す・戻す側と live DB の migration も人間だけ: `make stop` / `reset-trades` / `migrate-down` / `migrate-live-up` / `migrate-live-down` / `restore-drill`、bot・postgres のプロセスの停止、制御 API への POST、emergency の解除(`POST /api/emergency-resume`)。再起動が要るときは人間に頼んで待つ。
- ハーネス(`.claude/hooks/`・`.claude/settings*`・`.githooks/`・`Makefile`・`scripts/{arch-guard,secret-scan,pre-commit}.sh`)は `STOCKBOT_HARNESS_EDIT_APPROVED=1` で起動した承認セッションでしか直さない。skills は通常のセッションで直してよい。`scripts/*_test.sh` は書き換え・新規作成はできるが、削除と空化はしない。
- `go test -tags integration` を生で打たない(実 DB を truncate する)。`make test-integration`(`_test` 末尾の DSN)経由のみ。
- `.env` / `~/.stockbot/env` はキー名だけ読む。
- deny hook は heredoc の本文も読む。長い文章は Write でスクラッチに書いてから `cat` で連結し、commit は `git commit -F <file>`。
- 再起動をまたいでも安全な設計にする。操作者は `make stop` / `make start` を曜日・時間帯を問わず打つ。建玉・採番・帳簿を持つ変更は「プロセスが死んで台帳から復元しても同じ意味になるか」を確かめる(運用手順で回避しない)。
- 安全側の修正(上限を下げる・テストの追加・docs・migration・backtest)は判断を仰がずにやってよい。リスクを増やす方向の変更は操作者の明示的な決定が要る。
- docs は現在形の事実だけを書き、訂正は上書きする(取り消し線・「訂正」・絵文字で履歴を積まない)。数値は書かずに測る場所を書く。規則の全文は [docs/README.md](docs/README.md) の「書き方」。

## 2. マージゲートと層規約

- `make check-backend`(`go test -race` + `go vet` + `go build`)が緑で main へ直接 push する。push 前の完全チェックは `make check`。
- strict TDD(Red → Green → Refactor、Refactor 必須)。Stop hook(`.claude/hooks/tdd-check-lib.sh`)が機械強制するのは「テストが存在すること」までで、Red を先に見た順序は人間の規律。逃げ道は `STOCKBOT_TDD_CHECK=off`。
- `domain` は純粋: `pgx`/`net/http`/`slog`/`os` を import しない。`time.Now()` 禁止(`clock.Clock` を注入)、`rand` 禁止(`SignalIDFn` を注入)。例外は `domain/market` の rolling window の mutex と、`domain/risk` / `domain/strategy` が `config` の型と定数だけを import すること(`scripts/arch-guard.sh` 規則 (l))。
- `usecase` は port interface 経由のみ(具体 adapter を import しない)。handler は parse → usecase → encode の薄い層で、repo を直接呼ばない。
- `port` は `config` を import しない(R1。mode 等は string で渡す)。
- CQRS: `usecase/command`(状態変更・Tx)と `usecase/query`(read-only・View DTO)を分ける。

## 3. 発注と守りの不変条件

### 守りは broker 側に置く
- **TP/SL は broker 側**(OCO / 逆指値)。bot の OnTick 監視だけの守りは禁止。SL 欠落は致命的、TP 欠落は機会損失だけ — TP を持たない戦略(trail)は SL だけ板に置き、TP / ratchet は OnTick が持つ。
- `PlaceSettleOCO` / `ResolveSettleLegs` は `STOCKBOT_TACHIBANA_OCO_VERIFIED` が無ければ fail-close。
- **多日建玉(`HoldingMode==multiday`)の板の守りは stop-only**(`risk.ProtectiveTakeProfitOnBoard`)。立花は期日付き注文の繰越で翌日の値幅制限を再検査し、TP 脚が帯の外だと SL 脚ごと失効させる。新規建て・消えた守りの復旧・期日の置き直し・値段の変更の 4 経路がこの述語で決める。intraday は両脚。
- **値幅制限**: SL が当日の帯の外なら建てない(`risk.EvaluatePriceLimit`)。TP が外なら TP 脚だけ落として SL を置く(`risk.ProtectiveTakeProfitPlaceable`)。基準値段は前日終値。
- **多日建玉の守りは期日付き**: `PlaceSettleOCO` だけが 9 営業日先を入れる(`settleOrderExpiry`。立花の上限は発注日起点の 10 営業日で、訂正しても動かない)。新規建てと成行返済は当日限り。
  - `command.ReplaceProtectiveOrder` が寄り前に、残り 3 営業日を切った守りを取消 → 同条件で再発注する。取消と再発注の間は守りが消えるので**場中は撃たない**。取り消す前に今日の帯で検問する(SL が外なら取り消さない・TP が外なら TP 脚だけ落とす)。
  - `command.RearmUnguarded` が板から消えた守りを凍結値で置き直す(時間帯を縛らない)。
  - `command.RaiseTrailStops` が寄り前に、線が立っている trail 建玉の板の SL を利確の線まで引き上げる(上げるだけ・前日終値が線以下なら置かない)。期日の置き直しの後に同じ流れで回し、その間は守りの自動復旧を待たせる。
  - 休場カレンダーが尽きたら日付を捏造せず error にし、取り消さない。新規の多日建玉は期日が出せなければ建てない(`protective_expiry_unavailable_calendar_exhausted`)。
- **呼値**: TP/SL は `market.RoundToTickOf` で呼値に丸める。丸めた結果を掛け算で作らない(0.1 は二進で表せず 17 桁の値段になって注文ごと拒否される。1 円未満の刻みは逆数で割る)。丸めは domain の 1 か所だけで、adapter は丸めない(`TestFtoa_DoesNotRound`)。細かい呼値の判定は `market.InferTickRegimeFromBars` を使い、調整済みの日足を証拠にしない。誤りは常に粗い側へ倒す(`TestCoarseTickIsAlwaysAMultipleOfFineTick`)。

### 約定と記帳
- **約定数量が正**: partial fill は `FilledQuantity` で OCO と Position を凍結し、残注文を cancel する。発注の transport エラーは「届かなかった」と仮定せず、照会 → cancel / 補償 → 不能なら trip。
- **決済側も同じ**: 約定が非同期な broker(`SettleFillsAsync()`・`port.LiveBroker` の一員)では `ClosePosition` の受理は約定ではない。照会で確認できたときだけ台帳に書き、未約定 / 確認不能 / 部分約定は CLOSING のまま reconcile へ渡す(観測価格で埋めない)。**未約定の決済注文は cancel しない**(ストップ安の比例配分への唯一の参加経路。entry 側と意図的に非対称)。
- **返済の拒否を「建玉が裸」と読まない**: `closeOne` は拒否されたら建玉照会で broker がまだ持っているかを確かめ、持っていなければ trip せず実約定で締める。照会が落ちたら trip(fail-close)。拒否の文面では判定しない。`RearmUnguarded` も broker が持っていない建玉には守りを置かない。
- **broker 側の守りが約定した往復は実約定で記帳する**: `Reconcile` は建玉が消えたらまず `GetExecutions` を見て、決済の約定が建玉数量と一致したときだけその値と実手数料で締める(`stop_loss` / `take_profit` / `broker_close`)。一致しなければ推測せず `reconcile_cold_close`。carry は `closeOne` と同じ規約で載せる。
- **約定後に巻き戻した往復も台帳に残す**(`close_reason='entry_compensated'`)。再入場を止めるゲートは全部台帳を読むので、書けなかった枝では `ExecuteOrder.EntryBlocked` がその営業日の最後の栓。
- **config 凍結**: 建玉時に config_id / strategy_name / TP / SL / MaxHold / ratchet / ratchet_floor_at_arm / HoldingMode / ExecKind を Position に凍結する。active config の切替は既存の建玉に効かない(例外は `ExtendMaxHold`)。
- **多日建玉の時間切れ**は N 営業日目の 14:50 に損益に依らず決済する(`session.TradingHours.MaxHoldDeadline`)。
- **株式分割(併合)の権利落ち**では建玉を損益不変で言い直す(`position.SplitAdjusted`。`split_adjusted_on` で同じ日に二度割らない)。分割の断定は `market.ExDateSplitRatio`(前営業日終値基準の値幅制限の外 かつ 単純分割比)。paper は値段だけで言い直す。live は broker の建玉照会が分割後を示したときだけ言い直し、確かめられなければその建玉の自動決済をその日止めて `split_unconfirmed:<銘柄>` で緊急停止する(`command.SplitGuard`・銘柄ごとにその日最初のティックで照会)。分割で誤決済した往復は `close_reason='split_misfire'`。立花の公式の扱いは [TACHIBANA_API_NOTES §3.2](docs/runtime/TACHIBANA_API_NOTES.md)。

### 新規を止めるゲート
- **ナンピン禁止**: 同 symbol・同 side・同 strategy の OPEN(external 含む)があれば新規 reject。cap と独立の hard gate で override 不可。キーに strategy が入るのは paper だけで、`mode: live_config` は (symbol, side) = 1 銘柄 1 建玉。戦略不明の建玉(external と `strategy_name` が空の旧建玉)は全戦略をブロックする。建玉数の cap も同じキー(`nanpin_per_strategy_test` / `base_holders_test` / `config_repo_mode_test`)。
- **live は loop の前に同期 reconcile を 1 周**回す。失敗した銘柄は、reconcile が 1 回成功するまで新規だけ止める(`reconcile_unconfirmed`)。守りの置き直しと決済は止めない。
- **entry は口座単位で直列化**する(snapshot → ゲート → 発注。待ちは 10 秒で `entry_lock_timeout`)。決済と守りは対象外。
- **口座全体の 1 営業日の新規本数**(live のみ・`risk.account_max_entries_per_day`。0 = 無効で research は 0): 台帳から数える(銘柄・戦略を問わず決済済みも含む)ので再起動しても同じ数。暴落の初日に枠を使い切らないため。
- **銘柄ごとの新規停止(live のみ)**: 人間がボタンで止めた銘柄は selector が arm せず、発注直前のゲートが `manual_symbol_block` で reject する(override 不可)。状態はファイル(`STOCKBOT_LIVE_SYMBOL_BLOCKS`)で、読めない・壊れていれば live の新規を全部止める(`symbol_blocks_unreadable`)。決済・守りには効かない。
- **emergency_stop**: ファイルベースの write-once。毎 iteration の冒頭で確認し、発動中は新規 entry を作らない。trip は新規を止めるだけで既存の建玉を自動フラット化しない。
- **`EvaluateHardSafety`**: 手動 override も daily_loss / account_daily_loss / emergency / session を越えない。`TradingCycle.Execute` が executor の直前に必ず通る。手動 entry の経路を足すなら TradingCycle を通す(ゲートを再実装しない)。`ExtendMaxHold` も emergency 中は reject。
- **引け前フラット化**: 14:50 に `HoldingMode==intraday` の bot 建玉を exec_kind に依らず成行で返済する(`ForceFlatten`)。external は一日信用のときだけ対象(立花は一日信用が無いので今は発火しない)。`margin_oneday` かつ multiday は gate で reject。
- **保証金と維持率**: 発注前に collateral を確かめ、照会失敗は fail-close で reject。保証金が `min_collateral_jpy` を割ったら新規を reject(`collateral_below_minimum`)。維持率割れで emergency trip。daily loss cap は net(fee / carry 込み)。
  - 口座照会は銘柄ごとに払わない: `command.AccountMarginCache` が wire を打つのは初回・約定 / 決済・1 時間経過だけ。維持率ブレーカー(`CheckMaintenance`)は必ず実照会する。
  - selector は資金枠を先に見る(`WithLeverageHeadroom`)。余力が無ければ 1 銘柄も arm しない。判定式は発注前ゲートと `risk.WithinGrossNotionalCap` を共有する。
- **1 本あたりの計画損失の上限**(live のみ・bot_config の `risk.max_risk_per_trade_jpy`): 建値から SL までの距離 × 株数が上限を超える候補は建てない(`risk_per_trade`)。SL を狭める設定ではなく母集団を絞るもので、損失の上限でもない(ギャップとストップ安は逆指値をすり抜ける)。research は 0(無効)。
- **空売りは貸借銘柄のみ**(`risk.EvaluateShortLoanable`): `loanable_symbols` が空なら全 SELL を reject(`loanable_list_missing`)、載っていなければ `not_loanable`。平均回帰は `buy_only`。
- 計画損失と貸借の 2 つは**枠を配る前(selector)にも落とす**。ゲートだけだと建たない候補が枠を占有し続ける。
- **休場と stale 価格**: 土日と `hard_limits` の holidays は取引日でない(年次更新)。前日終値 fallback の価格(`Ticker.Stale`)では評価しない。

## 4. 資金とエッジの規律

- **ロット(株数)を上げるのはコスト込みのエッジが証明されてからだけ**。paper で複数単元を回しても標本数 N は増えない。
- **live allowlist(`hard_limits.live_allowed_strategies`)は「実弾で起動してよいか」の許可であって、エッジの証明ではない**。`mode: live_config` は allowlist(+ `no_trade`)の戦略しか起動しない。追加は人間の commit だけで、`catastrophe_guards_test.go` が yaml とテスト(`promoted`)の同時更新を強制する。
- **LLM をリアルタイム発注経路に入れない**。advisor は config 生成 / go-no-go gate のみで既定 OFF。引け後の日次総評 段 2 は許す(入力は決定論パケットの JSON、出力は文章だけ)。`mode: live_config` では advisor を登録しない。
- 寄り前の銘柄判定(go/no-go・`cmd/gonogo`)は**表示と記録だけ**許す。入力は決定論の材料と Web 検索、出力は判定ファイル(`~/.stockbot/gonogo/`)だけで、発注経路(`usecase/command`・`domain/risk`・`domain/strategy`)はそれを読まない(`isolation_test` が固定)。止めるのは人間の銘柄停止ボタン。LLM に拒否権を持たせるなら本節の変更と操作者の明示的な決定が要る。
- 撤退・減ロットの基準は事前にコミットしておき、負けている最中に裁量で決めない。裁量の手動エントリーはしない。

## 5. 変わりにくい構成の事実

- **broker**: paper / paper_live_feed(実フィード read-only × 紙執行)/ 立花 e支店(公開鍵認証。API 版は `defaultTachibanaAPIVersion` の 1 行)。立花で使えるのは現物と制度信用 6 ヶ月だけ(`SupportsExecKind`)。一日信用は API に区分が無く、一般信用は口座の種別によっては拒否されるので使わない。
- **データは 2 系統で混ぜない**: 運用は `backend/data`(立花の日足 250 本・`~/.stockbot/data` への symlink。Desktop 配下だと launchd から TCC で触れない)、検定は `backend/data_research`(調整済み日足・`EDGE_DATA_DIR=data_research`)。運用の日足は分割未調整なので `market.ChainLinkSplits` で吸収する。`fetch-daily` は書き換える前に必ず退避する(CSV は唯一のコピー)。鮮度監視はデータセット全体。停止・廃止銘柄の `retired/` への退避は人間がやる。
- **ユニバースは日次で fail-close**: プールは `hard_limits.allowed_symbols`(人間 commit のホワイトリスト。本数は `catastrophe_guards_test` が固定)。毎朝 `cmd/universe-screen` が選んで `~/.stockbot/universe/today.txt` に書き、bot は `bot_config.symbols_file` から読む。読めない / 空なら起動を拒否する。一覧外の銘柄は外して WARN で起動する(`DropSymbolsOutsideWhitelist`。静的な `symbols:` は起動拒否)。選定規則は forward 検定の間は固定する(規則は `cmd/universe-screen`)。
- **監視銘柄の予算**を絞る理由は API の回数ではなく時価の間隔。時価は 1 リクエスト 120 銘柄までで、`BatchQuoteFeed` は通信量を一定に保つため 121 銘柄目から間隔を伸ばす。監視集合は research と live で共有なので、paper の建玉が live の決済判定まで粗くする。枠はアームではなく入口で数え、既に持っている銘柄への別戦略の建ては枠を消費しない(`symbol_budget_guard_test.go`)。
- **アームは `advisor_v2.entries` で数える**(コードのメニューで数えない。形は `menu_shape_test` / `research_config_guard_test` が固定)。兄弟 `X` / `X_trail` は入口が同一で出口だけが違う。戦略の登録は戦略カタログ(`domain/strategy/catalog.go`)1 か所で、棄却済みのテンプレートはメニュー外(`catalog_test`)。
- **live の config 2 本(`bot_config.live.yaml` / `strategy_config.live.yaml`)は gitignore**で唯一のコピー。切り替えの順序は strategy_config が先、`hard_limits.yaml` が後(逆にすると live の起動 error で research まで止まる)。
- 永続化は `STOCKBOT_DATABASE_URL` があれば Postgres、無ければ in-memory。両者は同じ port を満たし、CAS / close saga / 銘柄ごとの分離を保つ。

## 6. 既知の穴

- 守り 4 点のうち ③発動方向 ④bot 停止下の約定 は本番で未実証。
- selector が `buy_only` では建たない売り候補を arm し、枠を占有する。売りを出しうる戦略を live で使う前に直す。
- paper は 1:1.2 のような小さい分割を拾えない。
