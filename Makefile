# stock-bot Makefile (plan §2.3, §3). check-backend is the merge gate.
.PHONY: help check-backend check test test-race vet build run start stop fmt fmt-check guard \
        guard-fast guard-hooks \
        secret-scan migrate-up migrate-down migrate-status reset-trades test-integration vet-integration \
        migrate-live-guard migrate-live-up migrate-live-down migrate-live-status \
        backup-now backup-install backup-list restore-drill fetch-daily forward-report \
        routine-install routine-uninstall routine-status

BACKEND := backend
GO := go

# 🚨 **git が hook の子プロセスへ渡す git 環境を、recipe へ引き継がない**(2026-08-21)。
#
# `make check-backend` / `guard-fast` / `guard-hooks` は `.githooks/pre-push` からも走る。git は hook へ
# `GIT_DIR` を渡し、**それは `git -C <dir>` や cwd より優先される**。すると guard 配下の
# 自己テスト(session-start / docs-sync-check / pre-stop-tdd / enforcement-guard)が
# 一時ディレクトリに作るはずの git リポが**実リポに向かってしまい**、一時リポが
# 存在しない前提で書かれた検査が総崩れになる。
#
# 症状は「単体では緑、`git push` 経由でだけ赤」。ブランチを問わず push が通らなくなる
# (サイクル3 の push で踏んで切り分けた)。pre-push 自身は自分の `git rev-parse` で
# GIT_DIR を使うので、hook 側では消さず **make の境界で落とす**のが正しい場所。
unexport GIT_DIR
unexport GIT_WORK_TREE
unexport GIT_INDEX_FILE
unexport GIT_OBJECT_DIRECTORY
unexport GIT_COMMON_DIR

.DEFAULT_GOAL := help

help: ## 全ターゲット一覧
	@echo "stock-bot targets:"
	@grep -E '^[a-zA-Z0-9_.-]+:.*## ' $(MAKEFILE_LIST) | sort | \
	  awk -F':.*## ' '{ printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 }'

# Merge gate: race tests + vet + build must all pass (plan §2.3, §10.2).
check-backend: test-race vet build ## マージゲート: test -race + vet + build

# 完全チェック: マージゲート + 層ガード + 自己テスト全件 + フォーマット差分。
# pre-push は hook の自己テスト(guard-hooks)を条件つきで回すので、これより軽い。
check: check-backend guard fmt-check ## check-backend + guard(自己テスト全件)+ fmt-check

test: ## go test ./... (非 race)
	cd $(BACKEND) && $(GO) test ./...

# -count=1 は必須(キャッシュ無効化)。Go のモジュールルートは backend/ で configs/ は
# その**外**にあるため、config だけを変更したコミットではテストキャッシュが効いてしまい、
# catastrophe_guards_test.go(live_allowed_strategies / 資金キャップ / order_boundary)が
# `(cached)` のまま素通りする。2026-08-07 実測: hard_limits.yaml に
# live_allowed_strategies を書き足しても `go test ./internal/config/` は ok (cached)。
# **yaml とテストの同時更新強制**という仕組みの土台なので、ここを外さない。
test-race: ## go test -race -count=1 ./...
	cd $(BACKEND) && $(GO) test -race -count=1 ./...

vet: ## go vet ./...
	cd $(BACKEND) && $(GO) vet ./...

# 出力先は bin/(gitignore)に固定する。backend/ で `go build ./cmd/<x>` を打つと
# backend/<x> に実行ファイルが落ちて残る(古いビルドが 3 本残っていた。REVIEW_TASKS B14)。
# 1 本だけ作るときも `cd backend && go build -o ../bin/ ./cmd/<x>`。
build: ## go build ./...(compile + link check。実行ファイルは bin/ へ)
	cd $(BACKEND) && $(GO) build -o ../bin/ ./...

fmt: ## gofmt -w (整形を適用)
	cd $(BACKEND) && gofmt -l -w .

fmt-check: ## gofmt 差分があれば fail (pre-push 用)
	@out=$$(cd $(BACKEND) && gofmt -l .); \
	if [ -n "$$out" ]; then echo "gofmt が必要なファイル:"; echo "$$out" | sed 's/^/  /'; exit 1; fi; \
	echo "fmt-check: clean"

guard: guard-fast guard-hooks ## 層規約ガード + 運用スクリプトと hook の自己テスト(全件)

# 自己テストの置き場所はこの 2 つだけ(CI は 2026-08-10 に廃止)。新しい自己テストはどちらかに足す。
# 各ターゲットの中は速い順に並べる。make は 1 本落ちた時点で止まるので、重いものの後ろに
# 置くと、それが赤い間ここへ到達しない(2026-08-08 に実際そうなっていた)。
# 所要時間は数値を書かずに `time make guard-fast` / `time make guard-hooks` で測る。

# pre-push が毎回踏む軽い側。
# stockbot-routine_test.sh は**ここにしか置けない**: 本体が macOS/launchd 専用で
# BSD date に依存する(GNU date 環境では自分で skip する)。
guard-fast: ## 層規約 grep ガード(domain純粋性 / R1 / handler→repo 等)+ 運用スクリプトの自己テスト
	bash scripts/arch-guard.sh
	bash scripts/stockbot-routine_test.sh
	bash scripts/stop-backup_test.sh

# hook / guard の自己テスト全件。pre-push は push の範囲に enforcement の変更があるときと、
# 前回の全件から 7 日たったときだけ回す(REVIEW_TASKS 決定 D-10・.claude/hooks/pre-push-scope.sh)。
# 最後まで緑だったときだけ、その時刻を pre-push の「前回の全件」として記録する。
# docs-sync-check / pre-stop-tdd は .claude/hooks/e2e-harness.sh でケース単位に並列化してある
# (既定の並列度 = コア数 - 2)。pretooluse-deny_test は最も重いので最後。
guard-hooks: ## hook / guard の自己テスト全件(pre-push は enforcement の変更時と週 1 回だけ回す)
	bash .claude/hooks/pre-push-scope_test.sh
	bash .claude/hooks/pre-push_test.sh
	bash .claude/hooks/enforcement-guard_test.sh
	bash .claude/hooks/session-start_test.sh
	bash scripts/secret-scan_test.sh
	bash .claude/hooks/docs-sync-check_test.sh
	bash .claude/hooks/pre-stop-tdd_test.sh
	bash .claude/hooks/pretooluse-deny_test.sh
	@mkdir -p runtime/logs && date +%s > runtime/logs/pre-push-selftest-ok

secret-scan: ## tracked file の committed-secret スキャン
	bash scripts/secret-scan.sh

run: ## paper mode で起動(リポジトリ直下 configs/ + backend/data の日足を読む)
	cd $(BACKEND) && \
	  STOCKBOT_HARD_LIMITS="$${STOCKBOT_HARD_LIMITS:-../configs/hard_limits.yaml}" \
	  STOCKBOT_BOT_CONFIG="$${STOCKBOT_BOT_CONFIG:-../configs/bot_config.yaml}" \
	  STOCKBOT_STRATEGY_CONFIG="$${STOCKBOT_STRATEGY_CONFIG:-../configs/strategy_config.active.yaml}" \
	  STOCKBOT_DAILY_CANDLES_DIR="$${STOCKBOT_DAILY_CANDLES_DIR:-data}" \
	  $(GO) run ./cmd/stockbot

# .env は make が自前で読む(`source .env` を人間にやらせない)。make のレシピは
# **1行ごとに別シェル**なので、env を必要とする行それぞれで読み直す必要がある。
# `set -a` で以降の代入を自動 export し、`. ./.env`(POSIX。sh でも動く)で取り込む。
# 既に source 済みのシェルから実行しても同じ値が入るだけなので二重実行は無害。
LOADENV := set -a; . ./.env; set +a;

# 起動するのは research(実価格 × 紙執行)と、.env の live ブロックが有効なら live
# (本番口座で実弾)の 2 トラック(docs/ARCHITECTURE.md の「hybrid」)。live の有効・無効は
# config/hybrid.go の LiveTrackEnv.Enabled と同じ式(STOCKBOT_LIVE_BOT_CONFIG があり、
# STOCKBOT_LIVE_DISABLED=1 でない)で表示する。値は出さない。
start: ## research(実価格 × 紙執行)+ live(.env で有効なら本番口座で実弾)を起動(.env は自動読込・claude CLI 必須)
	@test -f .env || { echo "==> ERROR: .env が無い。立花の公開鍵認証(v4r9 / v4r10 共通)と STOCKBOT_DATABASE_URL を書いた .env をリポジトリ直下に置く"; exit 1; }
	@command -v claude >/dev/null 2>&1 || { echo "==> ERROR: 'claude' CLI が PATH に無い。引け後の日次総評(段 2)が claude をサブプロセスで呼ぶ"; exit 1; }
	@$(LOADENV) test -n "$$STOCKBOT_TACHIBANA_AUTH_ID" || { echo "==> ERROR: .env に立花の認証が無い(broker=paper_live_feed は実価格を引く)"; exit 1; }
	@$(LOADENV) test -n "$$STOCKBOT_DATABASE_URL" || echo "==> WARN: STOCKBOT_DATABASE_URL 未設定 → in-memory。停止すると research の記録(trades)が消えます(docker compose up -d db)"
	@echo "==> stock-bot start"
	@echo "==> research: mode paper_config / broker paper_live_feed(実価格 × 紙執行)"
	@$(LOADENV) if [ -n "$${STOCKBOT_LIVE_BOT_CONFIG:-}" ] && [ "$${STOCKBOT_LIVE_DISABLED:-}" != "1" ]; then \
	  echo "==> live: 有効 — 本番口座で実弾を発注する(.env の STOCKBOT_LIVE_BOT_CONFIG)"; \
	else \
	  echo "==> live: 無効(.env に STOCKBOT_LIVE_BOT_CONFIG が無いか STOCKBOT_LIVE_DISABLED=1)"; \
	fi
	@echo "==> 前提: PATH に claude(.env と日足は make が面倒を見る)"
	@echo "==> bot api: http://127.0.0.1:8090  (STOCKBOT_HTTP_ADDR で変更可)"
	@echo "==> Ctrl+C か make stop で停止"
	@echo ""
	@bash scripts/stockbot-routine.sh cli-update
	@bash scripts/stockbot-routine.sh catchup
	@$(LOADENV) cd $(BACKEND) && \
	  STOCKBOT_HARD_LIMITS="$${STOCKBOT_HARD_LIMITS:-../configs/hard_limits.yaml}" \
	  STOCKBOT_BOT_CONFIG="$${STOCKBOT_BOT_CONFIG:-../configs/bot_config.advisor.yaml}" \
	  STOCKBOT_STRATEGY_CONFIG="$${STOCKBOT_STRATEGY_CONFIG:-../configs/strategy_config.active.yaml}" \
	  STOCKBOT_DAILY_CANDLES_DIR="$${STOCKBOT_DAILY_CANDLES_DIR:-data}" \
	  $(GO) run ./cmd/stockbot

# stop は「API ポートを LISTEN している stockbot」だけを止める。pgrep/pkill で名前一致
# 全殺しにしないのは、同じマシンで名前の似た別プロセスが動いていても巻き込まないため。
# SIGTERM(graceful: HTTP shutdown + ループ停止)→ 5秒待って残っていたら SIGKILL。
# 建玉はフラット化しない(broker 側の守りは残る。意図的 — OPERATIONS_RUNBOOK §3)。
# bot 側は STOCKBOT_HTTP_ADDR を読む。.env で変えて起動したとき make stop が
# 8090 を見て「動いていません」と言い、bot が生き残るのを防ぐ。
STOCKBOT_ADDR ?= $(or $(STOCKBOT_HTTP_ADDR),127.0.0.1:8090)

# 取引履歴だけを消す(建玉 + 決済 + OCO leg)。日足 candles / advisor_runs(LLM 判断
# ジャーナル)/ signal_rejections(見送り記録)/ strategy_configs(arm 履歴)は**残す**。
# 出口幾何の単位を変えた等で、旧仕様の建玉・約定を検定標本から外したいときに使う。
# 人間承認 env が要る(plan §8.3)。実行前に自動バックアップを取る。
reset-trades: ## 取引履歴のみ削除(建玉/決済)。日足・LLM判断・見送り記録は残す
	@test -n "$$STOCKBOT_HUMAN_APPROVED_DB_WRITE" || { echo "==> ERROR: STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 を前置して実行してください(plan §8.3)"; exit 1; }
	@$(LOADENV) test -n "$$STOCKBOT_DATABASE_URL" || { echo "==> ERROR: STOCKBOT_DATABASE_URL が未設定"; exit 1; }
	@if lsof -tiTCP:$$(echo "$(STOCKBOT_ADDR)" | sed 's/.*://') -sTCP:LISTEN >/dev/null 2>&1; then 	  echo "==> ERROR: bot が稼働中です。make stop してから実行してください"; exit 1; fi
	@echo "==> バックアップ"; bash scripts/db-backup.sh
	@$(LOADENV) psql_url="$$STOCKBOT_DATABASE_URL"; \
	 db=$$(printf '%s' "$$psql_url" | sed -E 's#.*/([^/?]+)(\?.*)?$$#\1#'); \
	 test -n "$$db" || { echo "==> ERROR: STOCKBOT_DATABASE_URL から DB 名を取れない"; exit 1; }; \
	 echo "==> 対象 DB: $$db(STOCKBOT_DATABASE_URL 由来。2026-09-05 まで確認は DSN・削除は stockbot 固定で、harvest の台帳を消す配線だった)"; 	 echo "==> 削除前:"; 	 docker exec stockbot-postgres psql -U stockbot -d "$$db" -c \
	   "select 'positions' t, count(*) from positions union all select 'trades', count(*) from trades union all select 'candles(残す)', count(*) from candles union all select 'advisor_runs(残す)', count(*) from advisor_runs;"; 	 docker exec stockbot-postgres psql -U stockbot -d "$$db" -c \
	   "BEGIN; DELETE FROM trades; DELETE FROM positions; COMMIT;"; 	 echo "==> 削除後:"; 	 docker exec stockbot-postgres psql -U stockbot -d "$$db" -c \
	   "select 'positions' t, count(*) from positions union all select 'trades', count(*) from trades union all select 'candles(残す)', count(*) from candles union all select 'advisor_runs(残す)', count(*) from advisor_runs;"
	@rm -f backend/runtime/emergency_stop.flag && echo "==> emergency_stop.flag を削除(あれば)"
	@echo "==> 完了。make start で再開できます"

stop: ## 稼働中の stock-bot を停止(API ポートの LISTEN プロセスのみ・graceful)
	@port=$$(echo "$(STOCKBOT_ADDR)" | sed 's/.*://'); \
	 pids=$$(lsof -tiTCP:$$port -sTCP:LISTEN 2>/dev/null || true); \
	 if [ -z "$$pids" ]; then echo "==> stock-bot は動いていません (port $$port は空き)"; exit 0; fi; \
	 target=""; \
	 for pid in $$pids; do \
	   cmd=$$(ps -o command= -p $$pid 2>/dev/null || true); \
	   case "$$cmd" in \
	     *stockbot*) target="$$target $$pid";; \
	     *) echo "==> port $$port を握っているのは stock-bot ではないので触りません (pid=$$pid: $$cmd)";; \
	   esac; \
	 done; \
	 if [ -z "$$target" ]; then exit 0; fi; \
	 for pid in $$target; do \
	   echo "==> SIGTERM $$pid (+ 子プロセス)"; \
	   for child in $$(pgrep -P $$pid 2>/dev/null); do kill -TERM $$child 2>/dev/null || true; done; \
	   kill -TERM $$pid 2>/dev/null || true; \
	 done; \
	 for i in 1 2 3 4 5 6 7 8 9 10; do \
	   sleep 0.5; \
	   left=$$(lsof -tiTCP:$$port -sTCP:LISTEN 2>/dev/null || true); \
	   [ -z "$$left" ] && break; \
	 done; \
	 left=$$(lsof -tiTCP:$$port -sTCP:LISTEN 2>/dev/null || true); \
	 if [ -n "$$left" ]; then \
	   echo "==> graceful 停止に失敗 → SIGKILL ($$left)"; \
	   kill -9 $$left 2>/dev/null || true; \
	 fi; \
	 echo "==> 停止しました (port $$port)"; \
	 bash scripts/stop-backup.sh || echo "==> ⚠ バックアップに失敗(停止は完了している。~/.stockbot/logs/db-backup.log を確認)"

# DB backup (~/.stockbot/db-backup.sh + launchd com.stockbot.db-backup)。
# macOS TCC のため canonical script と保存先は ~/.stockbot/ 配下(Desktop 配下は
# launchd の background 実行から起動できない)。毎日 03:10 発火。対象は stockbot-postgres のみ。
backup-now: ## stockbot DB を pg_dump して ~/.stockbot/backups/ に保存(手動1回)
	@test -x ~/.stockbot/db-backup.sh || { echo "==> ~/.stockbot/db-backup.sh が無い。scripts/db-backup.sh が正本: make backup-install で設置"; exit 1; }
	~/.stockbot/db-backup.sh

backup-install: ## バックアップ正本(scripts/db-backup.sh)と launchd を ~/.stockbot に設置
	@mkdir -p ~/.stockbot/logs ~/.stockbot/backups
	cp scripts/db-backup.sh ~/.stockbot/db-backup.sh
	chmod +x ~/.stockbot/db-backup.sh
	cp scripts/com.stockbot.db-backup.plist ~/Library/LaunchAgents/com.stockbot.db-backup.plist
	launchctl unload ~/Library/LaunchAgents/com.stockbot.db-backup.plist 2>/dev/null || true
	launchctl load ~/Library/LaunchAgents/com.stockbot.db-backup.plist
	@echo "==> 設置完了(毎日 03:10 JST に pg_dump・30世代 rotation)"

backup-list: ## ~/.stockbot/backups/ の dump 一覧(古い順・末尾が最新)
	@ls -lhrt ~/.stockbot/backups/ 2>/dev/null | tail -30 || echo "(no backups yet)"

# 復元先は stockbot_test 固定。運用 DB (stockbot) は絶対に対象にしない。
# 破壊的 SQL を含むため人間が手動で実行する(エージェントは deny hook で実行不可)。
restore-drill: ## 最新 backup を stockbot_test へ復元して件数検証(月1・人間が実行)
	@latest=$$(ls -1t ~/.stockbot/backups/stockbot-*.sql.gz 2>/dev/null | head -1); \
	 if [ -z "$$latest" ]; then echo "no backup found in ~/.stockbot/backups/" 1>&2; exit 1; fi; \
	 echo "==> restore-drill: $$latest → stockbot_test (運用 DB stockbot は不可侵)"; \
	 docker exec stockbot-postgres psql -U stockbot -d postgres -v ON_ERROR_STOP=1 \
	   -c "$(RESTORE_DRILL_RESET_SQL)" >/dev/null; \
	 gunzip -c "$$latest" | docker exec -i stockbot-postgres psql -U stockbot -d stockbot_test -v ON_ERROR_STOP=1 >/dev/null || { echo "==> FAIL: 復元に失敗(バックアップが壊れている可能性)"; exit 1; }; \
	 for t in advisor_runs trades positions; do \
	   n=$$(docker exec stockbot-postgres psql -U stockbot -d stockbot_test -tAc "SELECT count(*) FROM $$t" 2>/dev/null | tr -d '[:space:]'); \
	   if [ -z "$$n" ] || [ "$$n" = "0" ]; then echo "==> FAIL: $$t が復元されていない (rows=$${n:-none})"; exit 1; fi; \
	   echo "==> ok: $$t = $$n rows"; \
	 done; \
	 echo "==> restore-drill 成功(復元可能性を確認)"

# drill 用の使い捨て DB を作り直す SQL(stockbot_test のみ・運用 DB は対象外)。
RESTORE_DRILL_RESET_SQL := DROP DATABASE IF EXISTS stockbot_test; CREATE DATABASE stockbot_test;

# 当日(JST)の未確定バーは既定で取り込まない(append-only で固定されるため)。
# 推奨は翌朝(寄り前)の実行。STOCKBOT_BENCH_SYMBOL(立花の TOPIX コード・裏取り後に
# .env へ)を設定すると bench_topix.csv も同時更新する。
fetch-daily: ## 立花 API から日足を取得し backend/data/<sym>_daily.csv へマージ(朝の寄り前推奨・要 立花 公開鍵認証 env)
	cd $(BACKEND) && $(GO) run ./cmd/fetch-daily -out data -bench-symbol "$${STOCKBOT_BENCH_SYMBOL:-}"

# forward 検証の集計(READ-ONLY・SELECT のみ)。trades は唯一のエッジ証拠
# (EDGE_RESULTS #27)なので、判定は OPERATIONS_RUNBOOK の事前コミット基準で。
forward-report: ## forward 記録(trades)の集計を表示(要 STOCKBOT_DATABASE_URL・READ-ONLY)
	cd $(BACKEND) && $(GO) run ./cmd/forward-report $(or $(ARGS),)

# 日次/週次ルーチンの launchd 設置(人手ゼロ運用)。canonical script は ~/.stockbot/
# (TCC: launchd は Desktop 配下の script を起動できない — db-backup と同じパターン)。
#   com.stockbot.morning  毎日 07:00: fetch-daily(前日確定分)+ universe-screen(日次ユニバース)+ forward-report ログ追記
#                         (土日祝は取得0本で無害・場中への遅延発火は script 側で skip)
#                         weekly と同じく **ビルド済みバイナリ + env snapshot** で走る。
#                         make/.env 経由は TCC で4日連続 no-op だった(morning.log 実測)
#   com.stockbot.weekly   金曜 15:40: 週次 forward-report(READ-ONLY)。launchd は TCC で
#                         repo の make/go を回せないため ~/.stockbot/bin/forward-report
#                         (ここでビルド・make start でも再ビルド)を直接叩く
routine-install: ## 朝ルーチン(毎日07:00)+ 週次集計(金15:40)を launchd に設置
	@mkdir -p ~/.stockbot/logs ~/.stockbot/tmp ~/.stockbot/bin
	@# stockbot-routine.sh が JSON の読取に使う(標準ライブラリだけ。PyPI 依存は入れない)。
	@test -x ~/.stockbot/venv/bin/python || python3 -m venv ~/.stockbot/venv
	cd $(BACKEND) && $(GO) build -o ~/.stockbot/bin/forward-report ./cmd/forward-report
	cd $(BACKEND) && $(GO) build -o ~/.stockbot/bin/fetch-daily ./cmd/fetch-daily
	cd $(BACKEND) && $(GO) build -o ~/.stockbot/bin/universe-screen ./cmd/universe-screen
	@# 日次ユニバース選定が読む allowed_symbols の写し。正本は configs/hard_limits.yaml で、
	@# これは launchd(TCC で Desktop を読めない)向けの読み取り専用コピー。写しが古いと
	@# 候補が少し減るだけ(= fail-safe 方向)で、ホワイトリスト外の銘柄は決して選ばれない。
	cp configs/hard_limits.yaml ~/.stockbot/hard_limits.yaml
	bash scripts/snapshot-env.sh
	cp scripts/stockbot-routine.sh ~/.stockbot/stockbot-routine.sh
	chmod +x ~/.stockbot/stockbot-routine.sh
	cp scripts/com.stockbot.morning.plist ~/Library/LaunchAgents/com.stockbot.morning.plist
	cp scripts/com.stockbot.weekly.plist ~/Library/LaunchAgents/com.stockbot.weekly.plist
	launchctl unload ~/Library/LaunchAgents/com.stockbot.morning.plist 2>/dev/null || true
	launchctl unload ~/Library/LaunchAgents/com.stockbot.weekly.plist 2>/dev/null || true
	launchctl load ~/Library/LaunchAgents/com.stockbot.morning.plist
	launchctl load ~/Library/LaunchAgents/com.stockbot.weekly.plist
	@echo "==> 設置完了: 毎朝07:00 fetch-daily+universe-screen+forward-report / 金曜15:40 週次集計"
	@echo "==> claude CLI 更新と forward 集計は make start 側でも走る(launchd 不要)"
	@echo "==> ログ: ~/.stockbot/logs/{morning,weekly}.log / 確認: make routine-status"

routine-uninstall: ## ルーチン launchd を解除(db-backup は対象外)
	-launchctl unload ~/Library/LaunchAgents/com.stockbot.morning.plist 2>/dev/null
	-launchctl unload ~/Library/LaunchAgents/com.stockbot.weekly.plist 2>/dev/null
	rm -f ~/Library/LaunchAgents/com.stockbot.morning.plist ~/Library/LaunchAgents/com.stockbot.weekly.plist
	@echo "==> ルーチンを解除しました"

routine-status: ## ルーチンの launchd 状態と直近ログを表示
	@launchctl list | grep com.stockbot || echo "(no stockbot agents loaded)"
	@# 年次期限の警告は morning の**先頭**に出る(詰まった朝でも必ず出すため)が、
	@# その後ろに universe-screen が 200 行のランキングを吐くので tail では絶対に見えない。
	@# -a が要る: launchd の StandardOutPath は異常終了や書き込み途中断で NUL が混じり、
	@# 1 バイトでも入ると grep が "Binary file … matches" だけ出して警告を隠す(実測)。
	@echo "--- 直近の morning の警告(🛑/⚠) ---"; \
	  grep -a -E '🛑|⚠' ~/.stockbot/logs/morning.log 2>/dev/null | tail -8 || true
	@echo "--- morning.log (tail) ---"; tail -6 ~/.stockbot/logs/morning.log 2>/dev/null || echo "(no log yet)"
	@echo "--- weekly.log (tail) ---"; tail -6 ~/.stockbot/logs/weekly.log 2>/dev/null || echo "(no log yet)"

# integration tests MUST go through this target (never `go test -tags integration`
# raw — the deny hook blocks it; plan §8.5, §10.2). Requires a *_test DSN。
test-integration: ## integration テスト(*_test DSN 必須)
	@test -n "$(INTEGRATION_TEST_DB_URL)" || { echo "INTEGRATION_TEST_DB_URL must be set (and its DB name must end in _test)"; exit 1; }
	@case "$(INTEGRATION_TEST_DB_URL)" in \
	  *_test|*_test\?*) ;; \
	  *) echo "INTEGRATION_TEST_DB_URL の DB 名は _test で終わる必要があります(got: $(INTEGRATION_TEST_DB_URL))"; exit 1;; \
	esac
	cd $(BACKEND) && INTEGRATION_TEST_DB_URL="$(INTEGRATION_TEST_DB_URL)" $(GO) test -tags integration ./...

vet-integration: ## integration タグの compile チェック(実行はしない・安全)
	cd $(BACKEND) && $(GO) vet -tags integration ./...

# migrations are forward-only after live (plan §8.4). Requires STOCKBOT_DATABASE_URL.
# A non-_backtest target also requires STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 (plan §8.3).
migrate-up: ## migration を up
	cd $(BACKEND) && $(GO) run ./cmd/migrate -dir ../migrations -cmd up

migrate-down: ## migration を down
	cd $(BACKEND) && $(GO) run ./cmd/migrate -dir ../migrations -cmd down

migrate-status: ## migration の適用状況
	cd $(BACKEND) && $(GO) run ./cmd/migrate -dir ../migrations -cmd status

# live track の DB(STOCKBOT_LIVE_DATABASE_URL)へ **同じ migrations/** を適用する
# (docs/ARCHITECTURE.md の「hybrid」)。cmd/migrate は STOCKBOT_DATABASE_URL
# しか読まないので、そこへ live の DSN を差し替えて渡す。
# 🛑 差し替えると cmd/migrate 側の二重壁(SafeBacktestDSN の protectedDSNs)から research の
# DSN が消える — live に research の DSN が入っていても向こうでは同じ文字列に見えて検出できない。
# 取り違えはここで落とす(fail-close)。
# STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 は**人間が前置する**(Makefile では設定しない。
# Makefile が人間ゲートを肩代わりしたらゲートが消える)。
migrate-live-guard:
	@test -n "$(STOCKBOT_LIVE_DATABASE_URL)" || { echo "STOCKBOT_LIVE_DATABASE_URL must be set (live track の台帳は research とは別 DB)"; exit 1; }
	@test "$(STOCKBOT_LIVE_DATABASE_URL)" != "$(STOCKBOT_DATABASE_URL)" || { echo "STOCKBOT_LIVE_DATABASE_URL が STOCKBOT_DATABASE_URL と同一です(live と research を同じ DB に向けない)"; exit 1; }

migrate-live-up: migrate-live-guard ## live track DB の migration を up
	cd $(BACKEND) && STOCKBOT_DATABASE_URL="$(STOCKBOT_LIVE_DATABASE_URL)" $(GO) run ./cmd/migrate -dir ../migrations -cmd up

migrate-live-down: migrate-live-guard ## live track DB の migration を down
	cd $(BACKEND) && STOCKBOT_DATABASE_URL="$(STOCKBOT_LIVE_DATABASE_URL)" $(GO) run ./cmd/migrate -dir ../migrations -cmd down

migrate-live-status: migrate-live-guard ## live track DB の migration 適用状況
	cd $(BACKEND) && STOCKBOT_DATABASE_URL="$(STOCKBOT_LIVE_DATABASE_URL)" $(GO) run ./cmd/migrate -dir ../migrations -cmd status
