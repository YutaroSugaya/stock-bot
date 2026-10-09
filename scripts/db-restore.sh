#!/usr/bin/env bash
# db-restore.sh — Docker の Postgres が**空で**作り直されたときに、~/.stockbot/backups の最新 dump から
# 台帳を戻す。
#
# Docker のデータ(`~/Library/Containers/com.docker.docker/Data`)が丸ごと作り直されると、ボリュームも
# コンテナも消える。台帳(trades / positions)は dump が**唯一のコピー**。
#
# **`make start` が自動で呼ぶ**: `make start` を打つこと自体を人間の承認とみなし、
# stockbot-routine.sh の ensure_db が STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 を付けてここを呼ぶ。
# 手で打つときも同じ:
#   STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 bash scripts/db-restore.sh           # 既定の DB すべて
#   STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 bash scripts/db-restore.sh stockbot  # 指定 DB だけ
#
# 🛑 **中身のある DB には絶対に書かない**。書くのは「存在しない」か「public にテーブルが 0 個」の DB
# だけで、1 つでもテーブルがあれば skip する(dump は --clean 付きなので、流すと今の台帳を
# バックアップ時点へ巻き戻してしまう)。巻き戻しが本当に必要な状況はこのスクリプトの対象外。
#
# 🛑 **dump より後に bot が動いていたら戻さない**(exit 1)。その間の約定・決済は dump に無いので、
# 戻すと「欠けた台帳」で live が起動する(建った建玉は external に見え、決済済みの建玉は OPEN の
# まま守りを置き直しに行く)。自動復元を許したのは「失うものが無い」ときだけ。欠けを承知で
# 戻すなら人間が STOCKBOT_RESTORE_ACCEPT_STALE=1 を前置し、起動後に broker と突き合わせる。
# 「最後に動いていた時刻」は、bot が起動に成功したログ("stockbot started" を含む日次ログ)の
# 最終行。クラッシュで "stockbot stopped" が出なかった回も拾うため、停止行では判定しない。
set -euo pipefail

CONTAINER="${STOCKBOT_PG_CONTAINER:-stockbot-postgres}"
DB_USER="stockbot"
BACKUP_DIR="${STOCKBOT_BACKUP_DIR:-$HOME/.stockbot/backups}"
LOG_DIR="${STOCKBOT_LOG_DIR:-$HOME/.stockbot/logs}"
HTTP_PORT="$(printf '%s' "${STOCKBOT_HTTP_ADDR:-127.0.0.1:8090}" | sed 's/.*://')"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
# scripts/db-backup.sh の DB_NAMES と同じ集合(STOCKBOT_BACKUP_DBS で同じように上書きできる)。
if [ -n "${STOCKBOT_BACKUP_DBS:-}" ]; then
  read -ra ALL_DBS <<< "$STOCKBOT_BACKUP_DBS"
else
  ALL_DBS=("stockbot" "stockbot_c3" "stockbot_c4" "stockbot_live")
fi

if [ "${STOCKBOT_HUMAN_APPROVED_DB_WRITE:-0}" != "1" ]; then
  echo "==> ERROR: DB への書込です。STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 を前置して実行してください(CLAUDE.md DB / 安全)" >&2
  exit 1
fi

if lsof -tiTCP:"$HTTP_PORT" -sTCP:LISTEN >/dev/null 2>&1; then
  echo "==> ERROR: bot が稼働中です(port $HTTP_PORT)。make stop してから実行してください" >&2
  exit 1
fi

if ! docker exec "$CONTAINER" pg_isready -U "$DB_USER" -d postgres -q 2>/dev/null; then
  echo "==> ERROR: $CONTAINER が応答しない。先に: docker compose up -d db" >&2
  exit 1
fi

psql_c() { docker exec "$CONTAINER" psql -U "$DB_USER" -v ON_ERROR_STOP=1 -tA "$@"; }

# bot が最後に動いていた時刻(起動に成功した日次ログの最終行)。"YYYY-MM-DD HH:MM:SS"(JST)。
last_active=""
for f in $(ls -1r "$LOG_DIR"/bot-*.log 2>/dev/null); do
  if grep -q '"msg":"stockbot started"' "$f" 2>/dev/null; then
    last_active=$(grep -oE '"time":"[^"]+"' "$f" | tail -1 | sed -E 's/"time":"([^"]+)"/\1/' | cut -c1-19 | tr 'T' ' ')
    break
  fi
done

latest_migration=$(ls -1 "$REPO/migrations"/*.up.sql 2>/dev/null | sed -E 's#.*/([0-9]+)_.*#\1#' | sort | tail -1)

dbs=("$@")
[ ${#dbs[@]} -eq 0 ] && dbs=("${ALL_DBS[@]}")

restored=0
refused=0
for db in "${dbs[@]}"; do
  dump=$(ls -1t "$BACKUP_DIR/$db"-*.sql.gz 2>/dev/null | head -1 || true)

  exists=$(psql_c -d postgres -c "SELECT 1 FROM pg_database WHERE datname = '$db'")
  if [ "$exists" = "1" ]; then
    ntables=$(psql_c -d "$db" -c "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'")
    if [ "$ntables" != "0" ]; then
      echo "==> skip $db: 既にテーブルが $ntables 個ある — 上書きしない"
      continue
    fi
  fi

  if [ -z "$dump" ]; then
    echo "==> 🚨 $db が無い / 空なのにバックアップが無い($BACKUP_DIR/$db-*.sql.gz)"
    refused=$((refused + 1))
    continue
  fi

  dump_at=$(stat -f '%Sm' -t '%Y-%m-%d %H:%M:%S' "$dump")
  if [ -n "$last_active" ] && [[ "$last_active" > "$dump_at" ]] && [ "${STOCKBOT_RESTORE_ACCEPT_STALE:-0}" != "1" ]; then
    echo "==> 🚨 $db は戻さない: bot が最後に動いていたのは $last_active で、最新 dump($dump_at)より後"
    echo "      その間の約定・決済は dump に無い。欠けを承知で戻すなら(戻した後に broker と必ず突き合わせる):"
    echo "      STOCKBOT_RESTORE_ACCEPT_STALE=1 STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 bash scripts/db-restore.sh $db"
    refused=$((refused + 1))
    continue
  fi

  [ "$exists" = "1" ] || psql_c -d postgres -c "CREATE DATABASE $db" >/dev/null
  echo "==> restore $db <- $(basename "$dump")(取得 $dump_at / bot の最終稼働 ${last_active:-不明})"
  # 1 トランザクションで流す: 途中で落ちたら空のまま残り、次回また対象になる(半端な台帳を作らない)。
  gunzip -c "$dump" | docker exec -i "$CONTAINER" psql -U "$DB_USER" -d "$db" \
    -v ON_ERROR_STOP=1 --single-transaction -q >/dev/null
  restored=$((restored + 1))

  ver=$(psql_c -d "$db" -c "SELECT coalesce(max(version), '') FROM schema_migrations" 2>/dev/null || echo "")
  counts=$(psql_c -d "$db" -c "SELECT (SELECT count(*) FROM positions WHERE status = 'OPEN') || ' open / ' || (SELECT count(*) FROM positions) || ' positions / ' || (SELECT count(*) FROM trades) || ' trades'" 2>/dev/null || echo "?")
  echo "    ok: $counts / migration $ver"
  if [ -n "$latest_migration" ] && [ -n "$ver" ] && [[ "$ver" < "$latest_migration" ]]; then
    echo "    ⚠ repo の最新 migration は ${latest_migration}。STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 make migrate-up を $db に対して実行すること"
  fi
done

echo "==> 完了: $restored DB を復元 / $refused DB は戻せず"
[ "$restored" -gt 0 ] && echo "==> 起動後に GET /api/live/dashboard で live の建玉が broker と一致するか確認"
[ "$refused" -eq 0 ]
