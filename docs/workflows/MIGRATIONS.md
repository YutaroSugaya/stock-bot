# migration 規律

- 命名 `NNNN_topic.up.sql` / `NNNN_topic.down.sql`(4 桁連番、gap 禁止)。
- `up` は idempotent(`CREATE TABLE IF NOT EXISTS` 等)。`down` は逆操作(comment-only 禁止)。
- live 前は `0001_init` に squash 可(1 回限り・paper 限定)。**live 後は forward ALTER のみ**(squash / 既存編集禁止)。
- `pre-stop` hook(`.claude/hooks/pre-stop-checks.sh`)が「up/down 対の完備・連番 gap・`DATA_MODEL.md` 同期」を確認する。
- スキーマ変更は [`docs/runtime/DATA_MODEL.md`](../runtime/DATA_MODEL.md) のテーブル定義と「migration 履歴」を同期更新する。履歴の表は DATA_MODEL だけが持つ。各版の down が何を戻すかは `migrations/NNNN_*.down.sql` を読む。
- 適用は `make migrate-up`。live DB は SELECT のみ、書込は migration / 人間承認(`STOCKBOT_HUMAN_APPROVED_DB_WRITE=1`)のみ。live DB への適用(`make migrate-live-up` / `-down`)は人間が打つ(deny hook が AI からの実行を止める)。

## 手順(追加するとき)

1. `migrations/NNNN_topic.up.sql` と `.down.sql` を**ペアで**作る(連番は直前 +1、gap 禁止)。
2. `up` を idempotent に書く。`down` は逆操作(`DROP`/`ALTER ... DROP COLUMN` 等。comment-only 禁止)。
3. [DATA_MODEL.md](../runtime/DATA_MODEL.md) のテーブル定義と「migration 履歴」を同期更新する
   (pre-stop hook が未更新を block する)。
4. **rollback テスト**: `make migrate-up` → `make migrate-down` → `make migrate-up` が通り、
   スキーマが元に戻ることを throwaway な `*_backtest` DB で確認(`cmd/migrate` は DB 名が `_backtest` なら
   `SafeBacktestDSN` で自動許可。非 `_backtest`(例 `*_test`)は `STOCKBOT_HUMAN_APPROVED_DB_WRITE=1` 前置が要る。
   **live DB では実行しない**)。詳細は add-migration スキル。
5. enum(`close_reason` 等)を増やすときは CHECK 制約と `port` の string を**同時拡張**する。

## 適用時の注意

- バイナリは全トラックで共通なので、CHECK 制約や列を変える migration は**全トラックの DB に当てる**。
  当てていない DB では、その値を書く決済や INSERT が落ちる。DB の一覧は `.env` の DSN(`STOCKBOT_DATABASE_URL`)と、`backend/internal/app/perfcycles/cycles.yaml` に並べた追加の台帳 DB。
- 取り下げの migration(0021 が 0020 を戻す等)は、元の migration と続けて同じ DB 群に当てる。

### 0007 適用後の確認

60 秒窓に run が 2 件以上あった config は意図的に `advisor_run_id` を NULL のまま残す(誤リンクよりリンク無し)ので、適用後に残数を人間が確認する。

### 0008 の retention 方針

screen_snapshots は全量保持し、削除するなら人間承認の migration で行う(自動削除は入れない)。
