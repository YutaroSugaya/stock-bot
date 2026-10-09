#!/usr/bin/env bash
# scripts/stop-backup.sh の自己テスト(`make stop` 直後の DB バックアップをいつ取るか)。
#   Part 1 = 判定関数 stop_backup_delay の単体(source して直接叩く)
#   Part 2 = e2e。バックアップ本体を STOP_BACKUP_CMD で差し替え、HOME 相当(STOCKBOT_RUN_DIR)を
#            使い捨てにして実際に走らせる。docker にも DB にも触らない。
set -uo pipefail

SCRIPT="${STOP_BACKUP_SCRIPT:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/stop-backup.sh}"
pass=0; fail=0
TMPROOT=$(mktemp -d "${TMPDIR:-/tmp}/stop-backup-test.XXXXXX")
cleanup() {
    # 予約した遅延ジョブを残さない(sleep したまま 15:31 に本物を走らせない)。
    for f in "$TMPROOT"/*/stop-backup-deferred.pid; do
        [ -f "$f" ] && { pkill -P "$(cat "$f")" 2>/dev/null; kill "$(cat "$f")" 2>/dev/null; }
    done
    rm -rf "$TMPROOT"
}
trap cleanup EXIT

check() { # check <説明> <期待> <実際>
    if [ "$2" = "$3" ]; then pass=$((pass+1))
    else fail=$((fail+1)); echo "FAIL: $1 (expected [$2], got [$3])"; fi
}

# ---- Part 1: 判定 ----------------------------------------------------------
# shellcheck source=/dev/null
source "$SCRIPT"
# 引数: 曜日(1=月..7=日) 時 分 秒 → 待つ秒数(0 = いま取る)
check "平日 10:00 は 15:31 まで待つ"      "19860" "$(stop_backup_delay 3 10 0 0)"
check "平日 15:29:59 もまだ場中扱い"     "61"    "$(stop_backup_delay 5 15 29 59)"
check "平日 15:30 ちょうどは即時"          "0"     "$(stop_backup_delay 5 15 30 0)"
check "平日 夜は即時"                      "0"     "$(stop_backup_delay 2 21 0 0)"
check "平日 早朝(寄り前)も待つ"          "34260" "$(stop_backup_delay 1 6 0 0)"
check "土曜は即時"                         "0"     "$(stop_backup_delay 6 10 0 0)"
check "日曜は即時"                         "0"     "$(stop_backup_delay 7 10 0 0)"

# ---- Part 2: e2e -----------------------------------------------------------
run() { # run <case> <STOP_BACKUP_NOW>
    local dir="$TMPROOT/$1"; mkdir -p "$dir"
    STOCKBOT_RUN_DIR="$dir" STOP_BACKUP_NOW="$2" \
        STOP_BACKUP_CMD="echo ran >> '$dir/ran'" bash "$SCRIPT" >"$dir/out" 2>&1
    echo $?
}

# 場外: その場で取る(従来どおり)。
check "場外の停止は exit 0" "0" "$(run night '2 21 00 00')"
check "場外の停止はその場でバックアップ" "ran" "$(cat "$TMPROOT/night/ran" 2>/dev/null)"
check "場外は遅延ジョブを作らない" "no" "$([ -f "$TMPROOT/night/stop-backup-deferred.pid" ] && echo yes || echo no)"

# 場中: その場では取らず、遅延ジョブを 1 本だけ予約して即座に戻る。
start=$(date +%s)
check "場中の停止は exit 0" "0" "$(run day '3 10 00 00')"
check "場中の停止は待たずに戻る" "yes" "$([ $(( $(date +%s) - start )) -lt 5 ] && echo yes || echo no)"
check "場中はその場でバックアップしない" "" "$(cat "$TMPROOT/day/ran" 2>/dev/null)"
pidf="$TMPROOT/day/stop-backup-deferred.pid"
sleep 0.3
check "場中は遅延ジョブを予約する" "alive" "$([ -f "$pidf" ] && kill -0 "$(cat "$pidf")" 2>/dev/null && echo alive || echo dead)"
first=$(cat "$pidf" 2>/dev/null)
check "予約を知らせる" "yes" "$(grep -q '15:31' "$TMPROOT/day/out" && echo yes || echo no)"

# 場中に何度 stop しても、予約は 1 本だけ(同じ時刻に dump を何本も取らない)。
check "2 回目の場中停止も exit 0" "0" "$(run day '3 11 00 00')"
check "予約は増えない" "$first" "$(cat "$pidf" 2>/dev/null)"
check "既に予約済みと知らせる" "yes" "$(grep -q '予約済み' "$TMPROOT/day/out" && echo yes || echo no)"

# ---- Part 3: 1 日 1 回 ------------------------------------------------------
# 停止のたびに dump が増えていた。その日すでに**停止時の dump が成功していれば**取らない。
# 03:10 の定期分(launchd)は数えない — 数えると停止時の dump が一度も取られなくなる。
run3() { # run3 <case> <STOP_BACKUP_NOW> <YYYY-MM-DD> [cmd]
    local dir="$TMPROOT/$1"; mkdir -p "$dir"
    STOCKBOT_RUN_DIR="$dir" STOP_BACKUP_NOW="$2" STOP_BACKUP_TODAY="$3" \
        STOP_BACKUP_CMD="${4:-echo ran >> '$dir/ran'}" bash "$SCRIPT" >"$dir/out" 2>&1
    echo $?
}
ran_count() { grep -c ran "$TMPROOT/$1/ran" 2>/dev/null || echo 0; }

run3 once '2 16 00 00' 2026-09-30 >/dev/null
check "同じ日の 2 回目の停止も exit 0" "0" "$(run3 once '2 21 00 00' 2026-09-30)"
check "同じ日の 2 回目は取らない" "1" "$(ran_count once)"
check "取らない理由を知らせる" "yes" "$(grep -q '取得済み' "$TMPROOT/once/out" && echo yes || echo no)"
run3 once '3 16 00 00' 2026-10-01 >/dev/null
check "翌日はまた取る" "2" "$(ran_count once)"

# 失敗した dump は「取った」に数えない(次の停止でやり直す)。
check "失敗した dump は非 0 で返す" "1" "$(run3 failed '2 16 00 00' 2026-09-30 'false')"
run3 failed '2 21 00 00' 2026-09-30 >/dev/null
check "失敗した日の次の停止はやり直す" "1" "$(ran_count failed)"

# 場中の予約(15:31)が成功したら、その日の場外の停止ではもう取らない。
dir="$TMPROOT/deferred"; mkdir -p "$dir"
STOCKBOT_RUN_DIR="$dir" STOP_BACKUP_NOW='3 10 00 00' STOP_BACKUP_TODAY=2026-09-30 STOP_BACKUP_DEFER_UNTIL=36001 \
    STOP_BACKUP_CMD="echo ran >> '$dir/ran'" bash "$SCRIPT" >/dev/null 2>&1
for _ in 1 2 3 4 5 6 7 8 9 10; do [ -f "$dir/ran" ] && break; sleep 0.5; done
sleep 0.3
check "予約した dump が走った" "1" "$(ran_count deferred)"
run3 deferred '3 16 00 00' 2026-09-30 >/dev/null
check "予約分が成功した日の場外停止は取らない" "1" "$(ran_count deferred)"

# ---- Part 4: db-backup.sh が寄り前の go/no-go の判定ファイルも退避する ----------------
# 判定ファイル(~/.stockbot/gonogo)は git の外の唯一のコピー(判定の記録)。
# DB が止まっていても(コンテナ無し)退避する。docker には触らない(存在しないコンテナ名を渡す)。
DB_BACKUP="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/db-backup.sh"
run4() { # run4 <case> → exit code。HOME を使い捨てにする
    local home="$TMPROOT/$1"
    HOME="$home" STOCKBOT_PG_CONTAINER="no-such-container-for-test" bash "$DB_BACKUP" >"$home/out" 2>&1
    echo $?
}
mkdir -p "$TMPROOT/gonogo-bk/.stockbot/gonogo"
echo '{"date":"2026-10-05","symbol":"6594"}' > "$TMPROOT/gonogo-bk/.stockbot/gonogo/2026-10-05.jsonl"
check "DB が止まっていても exit 0" "0" "$(run4 gonogo-bk)"
arch=$(ls "$TMPROOT/gonogo-bk/.stockbot/backups/gonogo/"gonogo-*.tar.gz 2>/dev/null | head -1)
check "判定ファイルを退避した" "yes" "$([ -n "$arch" ] && echo yes || echo no)"
check "退避に jsonl が入っている" "yes" \
    "$(tar -tzf "$arch" 2>/dev/null | grep -q 'gonogo/2026-10-05.jsonl' && echo yes || echo no)"
check "DB の dump の置き場に混ぜない(復元が拾わない)" "" \
    "$(ls "$TMPROOT/gonogo-bk/.stockbot/backups/"*.tar.gz 2>/dev/null)"
mkdir -p "$TMPROOT/gonogo-none"
check "判定ファイルが無くても exit 0" "0" "$(run4 gonogo-none)"
check "無ければ退避を作らない" "" "$(ls "$TMPROOT/gonogo-none/.stockbot/backups/gonogo/" 2>/dev/null)"

echo "stop-backup_test: pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
