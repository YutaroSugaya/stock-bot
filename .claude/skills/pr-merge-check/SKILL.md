---
name: "pr-merge-check"
description: "stock-bot の変更をマージしてよいか判定する手続き。完全チェック make check(= マージゲート make check-backend + guard + fmt-check)+ 層規約 grep + strict TDD + migration/docs 同期 + トレード不変条件を順に検査し、項目別に pass/fail を報告する。PR レビュー時、merge 可否を判断したいとき、push/PR 前の最終確認に使う。SSOT は CLAUDE.md と docs/architecture/PR_CHECKLIST.md。"
---

# push/マージ前チェック(実行版)

> [docs/architecture/PR_CHECKLIST.md](../../../docs/architecture/PR_CHECKLIST.md) の実行手順。全項目を満たすまで push しない。
> 機械ゲートの一部は Stop hook / `.githooks/pre-push` でも二重化されるが、本スキルは**人間/エージェントが能動的に回す**ためのミラー。
> 絶対ルールの SSOT は [CLAUDE.md](../../../CLAUDE.md)。

## 用語(Makefile が SSOT)

- **マージゲート** = `make check-backend`(`go test -race ./...` + `go vet ./...` + `go build ./...`)。Makefile の定義どおり最小ゲート。
- **完全チェック(推奨・push 前)** = `make check`(= `check-backend` + `guard` + `fmt-check`)。pre-push 相当をローカルで回す superset。

## 手順

### 1. ゲート(機械的)

```bash
cd "$(git rev-parse --show-toplevel)"
make check          # check-backend(test -race + vet + build)+ guard + fmt-check
```

- [ ] `make check` 緑(`test -race` / `vet` / `build` / `gofmt` 差分ゼロ)。
- [ ] `make guard`([scripts/arch-guard.sh](../../../scripts/arch-guard.sh))で**層規約 grep 違反ゼロ**:
  - [ ] `domain` が `pgx`/`net/http`/`log/slog`/`os`/`time.Now()`/`rand` を使っていない(`market` の mutex 例外のみ)。
  - [ ] `domain` が `config` を import するのは `domain/risk/gate.go` と `domain/strategy/` だけ(型と定数のみ・arch-guard 規則 (l))。
  - [ ] `domain`/`port` が上位層(adapter/app/usecase)を import していない。
  - [ ] `port` が `config` を import していない(R1)。
  - [ ] `usecase` 本番が具体 adapter を import していない(port のみ)。
  - [ ] `handler` が repository を直呼びしていない。
  - [ ] command / query が分離している。

### 2. TDD / テスト

- [ ] strict TDD: Red → Green → **Refactor**(Refactor 要約を commit message に書く)。詳細 [docs/workflows/TESTING.md](../../../docs/workflows/TESTING.md)。
- [ ] domain テストは fake/mock なしの純粋単体。usecase は real collaborator(`broker.Paper` / in-memory repo)中心。
- [ ] mock は 3 用途(境界 / 失敗注入 / 非決定性除去)のみ。残すなら型/ファイル先頭にどれかを明記。
- [ ] adapter(DB)は integration tag。`make test-integration`(`*_test` DSN)で確認(生 `go test -tags integration` 禁止)。

### 3. トレード不変条件([CLAUDE.md](../../../CLAUDE.md) / [FAILURE_MODES.md](../../../docs/architecture/FAILURE_MODES.md))

- [ ] TP/SL は broker 側 / config 凍結 / ナンピン禁止 / emergency_stop / 引け前フラット / 委託保証金・維持率 / 呼値 tick。
- [ ] 失敗時は log+continue ではなく Rollback / Compensate / emergency_stop trip / DEFER([FAILURE_MODES.md](../../../docs/architecture/FAILURE_MODES.md))。
- [ ] 新規 trip reason は `<scope>_<action>_<state>` の grep 可能命名 + [layers/safety.md](../../../docs/architecture/layers/safety.md) 更新。

### 4. Schema / Migration 変更時(あれば add-migration スキルへ)

- [ ] `migrations/NNNN_topic.(up|down).sql` のペア・4 桁連番 gap なし。
- [ ] [DATA_MODEL.md](../../../docs/runtime/DATA_MODEL.md) のテーブル定義と migration 履歴を同期(pre-stop hook が確認)。
- [ ] `close_reason` 等の enum を増やしたら CHECK 制約と `port` の string を同時拡張。

### 5. Docs 同期(docs-sync hook が確認)

- [ ] `port/repository.go` 変更 → [layers/port.md](../../../docs/architecture/layers/port.md) or [layers/usecase.md](../../../docs/architecture/layers/usecase.md)。
- [ ] `safety/*.go` 変更 → [layers/safety.md](../../../docs/architecture/layers/safety.md)(exported が動いたら [FAILURE_MODES.md](../../../docs/architecture/FAILURE_MODES.md) も)。
- [ ] `domain/position/**` の exported が動いた / ファイルを足した・消した → [STATE_MACHINE.md](../../../docs/runtime/STATE_MACHINE.md)。
- [ ] 新規 `internal/<pkg>` → [ARCHITECTURE.md](../../../docs/ARCHITECTURE.md) or `layers/*.md`。

### 6. Live readiness

- [ ] live に関わる変更なら「エッジ証明前にロット非増」「LLM を発注経路に入れない」を破っていない。
- [ ] `STOCKBOT_TACHIBANA_OCO_VERIFIED` が無いときの fail-close 分岐(`PlaceSettleOCO`/`ResolveSettleLegs`)を消していない。`=1` は実機で確かめてから。

## 報告フォーマット

各セクションを ✅(満たす)/ ❌(違反・要修正)/ ➖(該当なし)で1行ずつ報告し、❌ があれば具体的な該当ファイルと修正案を添える。**❌ が1つでもあれば push 不可**と明示する。
