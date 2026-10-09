# stock-bot Docs — 目的別ガイド

「何を知りたいか」で入口を選ぶ。使い方とセットアップは [../README.md](../README.md)、絶対ルールの SoT は [CLAUDE.md](../CLAUDE.md)、
本ディレクトリはその詳細・根拠。

---

## まず読む

| 状況 | 開くべきファイル |
|---|---|
| 使い方・セットアップ | [../README.md](../README.md) |
| 全体像をざっくり掴みたい | [ARCHITECTURE.md](ARCHITECTURE.md) — 設計契約の凝縮版(先頭の図だけで十分) |
| 発注と守りの不変条件 | [../CLAUDE.md](../CLAUDE.md) §3 |
| push してよいかを確認したい | [architecture/PR_CHECKLIST.md](architecture/PR_CHECKLIST.md) |
| ハーネス(Claude Code hooks / git hooks)を有効化したい | [runtime/HARNESS_SETUP.md](runtime/HARNESS_SETUP.md) |

---

## 書き方(どの docs も)

- **本文は現在形の事実だけ**。事実が変わったら上書きする。取り消し線・「訂正」・絵文字で履歴を積まない。事故ログは docs に積まない(規則だけを書く)。
- **数値は書かず、測る場所を書く**(config のキー・API・SQL)。値を書くと config を変えるたびに docs が嘘になる。
- 太字は規則の核になる述語だけに使う。
- 節番号(`§N`)はコードのコメントから引用されているので動かさない。

---

## Claude Code スキル → `.claude/skills/`

| スキル | こんな時 | 主な本文ソース |
|---|---|---|
| `pr-merge-check` | 変更を merge してよいか項目別に判定 | [architecture/PR_CHECKLIST.md](architecture/PR_CHECKLIST.md) |
| `add-migration` | DB migration を追加する | [workflows/MIGRATIONS.md](workflows/MIGRATIONS.md) |

スキルは `.claude/skills/` からプロジェクトスキルとして直接ロードされる。enforcement hooks の配線は [runtime/HARNESS_SETUP.md](runtime/HARNESS_SETUP.md)。

---

## コードを書く前 / レビュー時 → `architecture/`

| ファイル | 内容 |
|---|---|
| [architecture/layers/](architecture/layers/) | 各レイヤー(handler/usecase/domain/port/adapter/safety)の do / don't / 命名 / テスト方法 |
| [architecture/FAILURE_MODES.md](architecture/FAILURE_MODES.md) | Rollback / Compensate / Trip / DEFER の絶対ルール + 障害→反応マトリクス |
| [architecture/PR_CHECKLIST.md](architecture/PR_CHECKLIST.md) | merge 前必須チェック(`make check` + 層規約 grep) |

---

## bot の動き方 → `runtime/`

| ファイル | 内容 |
|---|---|
| [runtime/STATE_MACHINE.md](runtime/STATE_MACHINE.md) | Position 状態遷移 OPEN → CLOSING → CLOSED |
| [runtime/DATA_MODEL.md](runtime/DATA_MODEL.md) | Postgres スキーマ(SSOT = `migrations/` + DATA_MODEL.md)+ 不変条件 |
| config(hard_limits / bot_config / strategy_config) | 専用 doc は無い。[../README.md](../README.md) の「設定ファイル」、[CLAUDE.md](../CLAUDE.md) §3 の「config 凍結」、各 yaml のコメントを参照 |
| [runtime/HARNESS_SETUP.md](runtime/HARNESS_SETUP.md) | `.claude/settings.json` の配線 + `core.hooksPath` 有効化 + 各 hook の挙動 |
| [runtime/TACHIBANA_API_NOTES.md](runtime/TACHIBANA_API_NOTES.md) | 立花 e支店 API の実装仕様・wire 裏取りノート(API 版は `defaultTachibanaAPIVersion`) |

---

## 開発作業の進め方 → `workflows/`

| ファイル | こんな時に開く |
|---|---|
| [workflows/TESTING.md](workflows/TESTING.md) | テストを書く / 方針を確認(strict TDD・古典派・3用途 mock・integration tag) |
| [workflows/MIGRATIONS.md](workflows/MIGRATIONS.md) | DB schema を変更する / migration を追加する |
| [workflows/BACKTEST.md](workflows/BACKTEST.md) | strategy を backtest で検証する |

---

## エッジ検定 → `runtime/EDGE_*`

| ファイル | 内容 |
|---|---|
| [runtime/EDGE_METHODOLOGY.md](runtime/EDGE_METHODOLOGY.md) | スクリーニング規律(¥1M 正規化 / day-block bootstrap / 3 regime / holdout) |
| [runtime/EDGE_TESTING_DATA.md](runtime/EDGE_TESTING_DATA.md) | 検定用 candle CSV の形式と入手の方針 |

---

## ディレクトリ構造

```
docs/
├── README.md                  ← 今ここ(目的別 index)
├── ARCHITECTURE.md            ← 設計契約の凝縮版
├── architecture/
│   ├── layers/{README,handler,usecase,domain,port,adapter,safety}.md
│   ├── FAILURE_MODES.md
│   └── PR_CHECKLIST.md
├── runtime/
│   ├── STATE_MACHINE.md  DATA_MODEL.md  HARNESS_SETUP.md  TACHIBANA_API_NOTES.md
│   └── EDGE_METHODOLOGY.md  EDGE_TESTING_DATA.md
└── workflows/{TESTING,MIGRATIONS,BACKTEST}.md
```

---

## 関連リンク

- [../README.md](../README.md) — 使い方・セットアップ
- [../CLAUDE.md](../CLAUDE.md) — 絶対ルール(SoT)
- [../.claude/skills/](../.claude/skills/) — 手続きスキル(pr-merge-check / add-migration)
- [../Makefile](../Makefile) — `make help` で全ターゲット一覧
