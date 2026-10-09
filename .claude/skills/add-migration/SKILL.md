---
name: "add-migration"
description: "stock-bot に DB migration を追加する手続き。NNNN_topic.up/down.sql のペアを連番(gap 禁止)で作り、up は idempotent・down は逆操作(comment-only 禁止)、DATA_MODEL.md(テーブル定義と migration 履歴)を同期、throwaway な *_backtest DB で rollback テスト、enum 拡張時は CHECK 制約と port の string を同時拡張する。migration/スキーマ変更(カラム追加・ALTER TABLE・index 追加)が必要なときに使う。live DB は SELECT のみ・書込は migration / 人間承認のみ。"
---

# migration を追加する(実行版)

> [docs/workflows/MIGRATIONS.md](../../../docs/workflows/MIGRATIONS.md) の実行手順。`pre-stop` hook がペア完備・連番・DATA_MODEL 同期を block で確認するので、本スキルはそれを**先回りで満たす**ための手順。

## 不変条件(先に確認)

- 命名 `NNNN_topic.up.sql` / `NNNN_topic.down.sql`(4 桁連番、**gap 禁止**)。
- `up` は idempotent(`CREATE TABLE IF NOT EXISTS` 等)。`down` は逆操作(comment-only 禁止)。
- **forward ALTER のみ**(squash / 既存編集禁止)。
- schema 変更は [docs/runtime/DATA_MODEL.md](../../../docs/runtime/DATA_MODEL.md) を同期更新。
- 適用は `make migrate-up`(Postgres 採用後)。**live DB は SELECT のみ**、書込は migration / 人間承認(`STOCKBOT_HUMAN_APPROVED_DB_WRITE=1`)のみ。
- DDL を repository / port / cmd/migrate の Go に直書きしない(`pre-stop` の Step 6.5 が block)。schema-as-code の正本は `migrations/`。

## 手順(追加するとき)

1. **直前の version を確認**して +1 する(gap 禁止)。

   ```bash
   ls migrations/ | sort
   ```

2. `migrations/NNNN_topic.up.sql` と `.down.sql` を**ペアで**作る。
3. `up` を idempotent に書く。`down` は逆操作(`DROP` / `ALTER ... DROP COLUMN` 等。comment-only 禁止)。
4. [DATA_MODEL.md](../../../docs/runtime/DATA_MODEL.md) のテーブル定義と「migration 履歴」表を同期更新する(pre-stop hook が未更新を block する)。履歴の表は DATA_MODEL だけが持つ(MIGRATIONS.md は手順と適用時の注意)。
5. **rollback テスト**(throwaway な `*_backtest` DB で。**live DB は絶対に指さない**):

   ```bash
   # migrate は STOCKBOT_DATABASE_URL を読む(INTEGRATION_TEST_DB_URL は make test-integration 用で、migrate には効かない)。
   # DB 名が _backtest なら cmd/migrate の SafeBacktestDSN が自動ガードする(人間承認 env 不要)。
   export STOCKBOT_DATABASE_URL=postgres://.../stockbot_backtest   # DB 名は _backtest で終わること
   make migrate-up && make migrate-down && make migrate-up          # 往復してスキーマが元に戻る
   make migrate-status
   # 非 _backtest(例: *_test)DB を使うなら STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 の前置も必要(write-guard)。
   ```

6. enum(`close_reason` 等)を増やすときは **CHECK 制約と `port` の string を同時拡張**。CHECK だけ・port だけの片側拡張は不可。

## 完了前チェック

- [ ] up/down ペア完備・4 桁連番 gap なし。
- [ ] up idempotent / down が実逆操作(comment-only でない)。
- [ ] DATA_MODEL.md 同期済み(pre-stop hook が要求)。
- [ ] `*_backtest`(throwaway)DB で up→down→up が緑(非 _backtest を使うなら +`STOCKBOT_HUMAN_APPROVED_DB_WRITE=1`)。
- [ ] enum 拡張なら CHECK + port string を同時拡張。
- [ ] `make check-backend` 緑(その後 pr-merge-check スキルで最終確認)。
