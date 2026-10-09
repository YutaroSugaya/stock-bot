# push 前チェックリスト(マージゲート)

main へ push する前に毎回確認。全項目を満たすまで push しない。

## ゲート(機械的)

- [ ] **`make check` 緑** = `check-backend`(`go test -race ./...` + `go vet ./...` + `go build ./...`)
      + `guard`([`scripts/arch-guard.sh`](../../scripts/arch-guard.sh))+ `fmt-check`(`gofmt` 差分ゼロ)。
- [ ] `make guard` で**層規約 grep 違反ゼロ**:
  - [ ] `domain` が `pgx`/`net/http`/`log/slog`/`os`/`time.Now()`/`rand` を使っていない(`market` の mutex 例外のみ)。
  - [ ] `domain` が `config` を import するのは `domain/risk/gate.go` と `domain/strategy/` だけ(型と定数のみ・arch-guard 規則 (l))。
  - [ ] `domain`/`port` が上位層(adapter/app/usecase)を import していない。
  - [ ] `port` が `config` を import していない(R1)。
  - [ ] `usecase` 本番が具体 adapter を import していない(port のみ)。
  - [ ] `handler` が repository を直呼びしていない。
  - [ ] command / query が分離している。

## TDD / テスト

- [ ] strict TDD: Red → Green → **Refactor**(Refactor 要約を commit message に書く)。詳細 [../workflows/TESTING.md](../workflows/TESTING.md)。
  - 機械強制は**テストの存在**まで(Stop hook Step 8.5)。**Red を先に見たか / アサートが
    意味を持つかは機械では見ていない** — ここは人間レビューの担当。hook が緑でも
    「TDD した」の証明にはならない。
- [ ] domain テストは fake/mock なしの純粋単体。usecase は real collaborator(`broker.Paper` / in-memory repo)中心。
- [ ] mock は 3 用途(境界 / 失敗注入 / 非決定性除去)のみ。残すなら型/ファイル先頭にどれかを明記。
- [ ] adapter(DB)は integration tag。`make test-integration`(`*_test` DSN)で確認(生 `go test -tags integration` 禁止)。

## トレード不変条件([../../CLAUDE.md](../../CLAUDE.md) / [FAILURE_MODES.md](FAILURE_MODES.md))

- [ ] TP/SL は broker 側 / config 凍結 / ナンピン禁止 / emergency_stop / 引け前フラット / 委託保証金・維持率 / 呼値 tick。
- [ ] 失敗時は log+continue ではなく Rollback / Compensate / emergency_stop trip / DEFER([FAILURE_MODES.md](FAILURE_MODES.md))。
- [ ] 新規 trip reason は `<scope>_<action>_<state>` の grep 可能命名 + [layers/safety.md](layers/safety.md) 更新。

## Schema / Migration 変更時

- [ ] `migrations/NNNN_topic.(up|down).sql` のペア・4 桁連番 gap なし。
- [ ] [../runtime/DATA_MODEL.md](../runtime/DATA_MODEL.md) のテーブル定義と migration 履歴を同期(pre-stop hook が確認)。
- [ ] `close_reason` 等の enum を増やしたら CHECK 制約と `port` の string を同時拡張。

## Docs 同期(docs-sync hook が確認)

- [ ] `port/repository.go` 変更 → [layers/port.md](layers/port.md) or [layers/usecase.md](layers/usecase.md)。
- [ ] `safety/*.go` 変更 → [layers/safety.md](layers/safety.md)(exported が動いたら [FAILURE_MODES.md](FAILURE_MODES.md) も)。
- [ ] `domain/position/**` の exported が動いた / ファイルを足した・消した → [../runtime/STATE_MACHINE.md](../runtime/STATE_MACHINE.md)。
- [ ] 新規 `internal/<pkg>` → [../ARCHITECTURE.md](../ARCHITECTURE.md) or `layers/*.md`。
- 他の層の変更で layers/*.md を直すのは、その層の契約が変わったときだけ(hook は要求しない)。

## Live readiness

- [ ] live に関わる変更なら「エッジ証明前にロット非増」「LLM を発注経路に入れない」を破っていない。
- [ ] `STOCKBOT_TACHIBANA_OCO_VERIFIED` が無いときの fail-close 分岐(`PlaceSettleOCO`/`ResolveSettleLegs`)を消していない。`=1` は実機で確かめてから。③発動方向 ④bot 停止下の約定は未実証。

---

## 関連 docs

- [../ARCHITECTURE.md](../ARCHITECTURE.md) — 設計契約の凝縮版
- [layers/](layers/) — レイヤー別契約
- [FAILURE_MODES.md](FAILURE_MODES.md) — Rollback / Compensate / Trip / DEFER
- [../workflows/TESTING.md](../workflows/TESTING.md) — テスト戦略
- [../runtime/HARNESS_SETUP.md](../runtime/HARNESS_SETUP.md) — hooks の有効化(CI は無い。このチェックリストと `.githooks/pre-push` が最後のゲート)
