# レイヤ別契約

各レイヤーの設計契約。全ファイルは [../../ARCHITECTURE.md](../../ARCHITECTURE.md) と同じく
**設計契約の正本(SoT)**。依存方向は `go build ./...` が、それで捕まらない規約は
[`scripts/arch-guard.sh`](../../../scripts/arch-guard.sh)(= `make guard`)が enforce する。

## 階層全体図

```
cmd/stockbot/ (wiring のみ)
  → Handler (app/handler/)              — HTTP 入口
  → Usecase (usecase/command/, usecase/query/) ← CQRS
  → Domain (domain/<aggregate>/)        — 純粋ロジック
  → Port (port/)                        — interface のみ
  ← Adapter (adapter/broker, adapter/repository) — port を実装
Safety (safety/)                        — cross-cutting(全層から参照可)
```

## 各レイヤー(詳細は個別ファイル)

| Layer | ファイル | 一言 |
|---|---|---|
| Handler | [handler.md](handler.md) | HTTP 入口。parse → usecase → encode の薄い層 |
| Usecase | [usecase.md](usecase.md) | CQRS で Command / Query 分離。port 経由で I/O |
| Domain | [domain.md](domain.md) | 純粋 Go。Entity / VO / Domain Service。I/O 禁止 |
| Port | [port.md](port.md) | Repository / Broker / Notifier / Advisor の interface(config 非依存=R1) |
| Adapter | [adapter.md](adapter.md) | port の具体実装(tachibana / paper / Postgres / in-memory) |
| Safety | [safety.md](safety.md) | emergency_stop / pending tracker。リーフ依存 |

## レイヤ別の許可 / 禁止依存

| 層 | 許可される依存 | 禁止 |
|---|---|---|
| `domain/*` | 他の domain、標準 lib(`time`/`math`/`sort`)、`config`(schema VO のみ・許容) | `pgx`/`net/http`/`log/slog`/`os`、`time.Now()`、`rand`(`market` の mutex のみ例外) |
| `config` | `domain`(session/order)、`yaml.v3`、`os` | `port`/`usecase`/`adapter`/`app` への依存 |
| `port` | `domain` | **`config`(R1)**、`adapter`、`usecase` |
| `usecase/command` | `port`、`domain`、`config` | 具体 `adapter` |
| `usecase/query` | `port`、`domain` | 状態変更、domain entity の境界露出(View DTO のみ) |
| `adapter/*` | `port`、`domain`、`config`、外部 lib | `usecase`、`app`、`cmd` |
| `app` / `app/handler` | `usecase`、`port`、`domain`、`config` | repo 直呼び、ビジネスロジック |
| `cmd/stockbot` | すべて(配線のみ) | ロジックの実装 |

## 関連 docs

- [../../ARCHITECTURE.md](../../ARCHITECTURE.md) — 全体図と禁止事項の凝縮版
- [../FAILURE_MODES.md](../FAILURE_MODES.md) — Rollback / Compensate / Trip / DEFER
- [../PR_CHECKLIST.md](../PR_CHECKLIST.md) — merge 前必須チェック
- [../../workflows/TESTING.md](../../workflows/TESTING.md) — テスト戦略
