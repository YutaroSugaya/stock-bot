#!/usr/bin/env bash
# stock-bot postgres backup script(~/.stockbot/ が canonical copy)。
#
# launchd から実行するため、macOS TCC で保護された Desktop/ 外に置く
# (Desktop 配下のスクリプトは launchd の background 実行で Operation not permitted)。
#
# 日足 CSV(~/.stockbot/data)はここでは扱わない。退避は **cmd/fetch-daily が書き換える直前**に
# 取る。守りたいのが「その書き換え」なので、別プロセスの定期バックアップだと壊した後の世代しか
# 残らない窓ができる。DB は CSV から再シードできるが CSV は復旧不能。

set -euo pipefail

BACKUP_DIR="$HOME/.stockbot/backups"
LOG_DIR="$HOME/.stockbot/logs"
LOG_FILE="$LOG_DIR/db-backup.log"
CONTAINER="${STOCKBOT_PG_CONTAINER:-stockbot-postgres}" # テストだけ存在しない名前に差し替える
# 寄り前の go/no-go の判定ファイル(cmd/gonogo が書く・git の外の唯一のコピー・判定の記録)。
# DB ではないが同じ世代数で退避する。置き場は dump と分ける(db-restore と鮮度監視は *.sql.gz だけを見る)。
GONOGO_DIR="$HOME/.stockbot/gonogo"
GONOGO_BACKUP_DIR="$BACKUP_DIR/gonogo"
DB_USER="stockbot"
# `make stop` の直後にも取るので、1 日に複数世代増える日がある(再起動を繰り返した日)。
# 遡れる日数を保つための世代数。
KEEP_GENERATIONS=60

# dump は trades / positions の**唯一のコピー**(日足 CSV と違い再生成できない)。
# 存在しない DB は静かに飛ばす(無い DB でスクリプト全体を落とすと、その日のバックアップが
# **全部**取れなくなる)。
# 既定は research の台帳(集計期間ごとに 1 DB)と live の台帳。別のレイアウトなら
# STOCKBOT_BACKUP_DBS="db1 db2" で上書きする(scripts/db-restore.sh の ALL_DBS と揃える)。
if [ -n "${STOCKBOT_BACKUP_DBS:-}" ]; then
  read -ra DB_NAMES <<< "$STOCKBOT_BACKUP_DBS"
else
  DB_NAMES=("stockbot" "stockbot_c3" "stockbot_c4" "stockbot_live")
fi

mkdir -p "$BACKUP_DIR" "$LOG_DIR"

log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" | tee -a "$LOG_FILE" >&2; }

ts="$(date -u +%Y%m%dT%H%M%SZ)"
failed=0
dumped=0

# 判定ファイルの退避。DB が止まっていても取る(コンテナの確認より前)。無ければ何もしない。
backup_gonogo() {
  if ! ls "$GONOGO_DIR"/*.jsonl >/dev/null 2>&1; then
    log "skip: gonogo の判定ファイルが無い($GONOGO_DIR)"
    return 0
  fi
  mkdir -p "$GONOGO_BACKUP_DIR"
  local out="$GONOGO_BACKUP_DIR/gonogo-$ts.tar.gz"
  local tmp="$out.tmp"
  if ! tar -czf "$tmp" -C "$(dirname "$GONOGO_DIR")" "$(basename "$GONOGO_DIR")" 2>>"$LOG_FILE"; then
    log "FAIL: gonogo の判定ファイルを退避できない; removing partial $tmp"
    rm -f "$tmp"
    return 1
  fi
  mv "$tmp" "$out"
  log "ok: wrote $out"
  ls -1t "$GONOGO_BACKUP_DIR"/gonogo-*.tar.gz 2>/dev/null \
    | tail -n +$((KEEP_GENERATIONS + 1)) \
    | while read -r old; do
        log "rotate: removing $old"
        rm -f "$old"
      done
}
backup_gonogo || failed=1

# コンテナが動いていなければ静かに skip(bot 停止中は通常運用)。
if ! docker ps --format '{{.Names}}' 2>/dev/null | grep -qx "$CONTAINER"; then
  log "skip: container $CONTAINER not running"
  [ "$failed" -eq 0 ] || exit 1
  exit 0
fi

for DB_NAME in "${DB_NAMES[@]}"; do
  # 未作成の DB は skip(1 本の不在で全体を落とすと、その日の他の DB も取れなくなる)。
  if ! docker exec "$CONTAINER" pg_isready -U "$DB_USER" -d "$DB_NAME" -q 2>/dev/null; then
    log "skip: $DB_NAME is not ready (未作成なら正常)"
    continue
  fi

  out="$BACKUP_DIR/$DB_NAME-$ts.sql.gz"
  tmp="$out.tmp"

  if ! docker exec "$CONTAINER" \
         pg_dump --no-owner --no-privileges --clean --if-exists \
                 -U "$DB_USER" "$DB_NAME" 2>>"$LOG_FILE" \
       | gzip -c > "$tmp"; then
    log "FAIL: pg_dump $DB_NAME errored (see above); removing partial $tmp"
    rm -f "$tmp"
    failed=1
    continue
  fi

  mv "$tmp" "$out"
  bytes=$(stat -f%z "$out" 2>/dev/null || wc -c < "$out")
  log "ok: wrote $out ($bytes bytes)"
  dumped=$((dumped + 1))

  # 世代管理は **DB ごと**。1 つのプールで回すと、DB が 3 本になった瞬間に
  # 各 DB の保持世代が 1/3 に減る(気づかないうちに 10 世代しか残らない)。
  ls -1t "$BACKUP_DIR/$DB_NAME"-*.sql.gz 2>/dev/null \
    | tail -n +$((KEEP_GENERATIONS + 1)) \
    | while read -r old; do
        log "rotate: removing $old"
        rm -f "$old"
      done
done

# 🛑 1 本でも dump に失敗したら非ゼロで終わる(launchd のログに残す)。
# 「どれも取れなかった」も失敗 — バックアップが静かに止まるのが一番危ない。
if [ "$failed" -ne 0 ]; then
  exit 1
fi
if [ "$dumped" -eq 0 ]; then
  log "skip: no database was ready (bot 停止中なら正常)"
fi
exit 0
