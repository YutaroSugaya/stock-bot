#!/usr/bin/env bash
# `make stop` の直後に取る DB バックアップの**時刻**を決める。
#
#   - 場外(平日 15:30 以降 / 土日)… その場で scripts/db-backup.sh を走らせる(従来どおり)
#   - 平日 15:30 より前 ………………… その場では取らず、**15:31 に 1 本だけ**予約して即座に戻る
#
# なぜ場中は遅らせるか: 場中の make stop はほぼ「stop → すぐ start」の再起動で、その間
# 実弾の建玉は bot に監視されていない。dump(数秒)がその窓を広げる。停止直後の dump の
# 目的は「次の make start で DB が消えていても自動復元できる」ことだが、場中の再起動では
# DB は残っている。場中に止めたまま放置した日も、15:31 の dump が最終状態を拾う。
#
# ⚠ 予約は `make stop` を打った端末の子プロセス(nohup)。Mac がスリープすると sleep も
# 止まるので 15:31 より遅れて走る。電源を落とせば消える — その夜は 03:10 の launchd が保険。
# ⚠ 祝日の平日も「場中扱い」で遅らせる(カレンダーを読まない)。遅れるだけで害は無い。
# 予約が既に生きていれば増やさない(何度再起動しても 15:31 の dump は 1 本)。
#
# **1 日 1 回**: 停止のたびに dump が増えるので、その日(JST)に
# **停止時の dump が成功していれば**以後の停止では取らない(印は $RUN_DIR/stop-backup-last.date)。
# 失敗した dump は数えない(次の停止でやり直す)。03:10 の定期分(launchd)も数えない — 数えると
# 停止時の dump が一度も取られなくなる。
# ⚠ 代償: その日 2 回目以降の停止の後に bot が動いた分は、次の dump(03:10 か翌日の停止)まで
# dump に入らない。`make start` の自動復元(ensure_db)は「dump より後に bot が動いていたら戻さず
# 止まる」ので、欠けた台帳で起動することはない(止まるだけ)。手動で取るなら `make backup-now`。
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_DIR="${STOCKBOT_RUN_DIR:-$HOME/.stockbot/run}"
PID_FILE="$RUN_DIR/stop-backup-deferred.pid"
BACKUP_CMD="${STOP_BACKUP_CMD:-bash '$REPO/scripts/db-backup.sh'}"
DEFER_UNTIL_S="${STOP_BACKUP_DEFER_UNTIL:-$(( 15 * 3600 + 31 * 60 ))}" # 15:31(引け 15:30 の 1 分後)。テストだけ差し替える
STAMP_FILE="$RUN_DIR/stop-backup-last.date"

# stop_backup_delay <曜日 1=月..7=日> <時> <分> <秒> → 待つ秒数(0 = いま取る)
stop_backup_delay() {
    local dow=$1 now_s=$(( 10#$2 * 3600 + 10#$3 * 60 + 10#$4 ))
    if [ "$dow" -le 5 ] && [ "$now_s" -lt $(( 15 * 3600 + 30 * 60 )) ]; then
        echo $(( DEFER_UNTIL_S - now_s ))
    else
        echo 0
    fi
}

main() {
    # テストは STOP_BACKUP_NOW="曜日 時 分 秒" で時刻を差し替える。既定は JST の現在時刻。
    local now today
    now="${STOP_BACKUP_NOW:-$(TZ=Asia/Tokyo date '+%u %H %M %S')}"
    today="${STOP_BACKUP_TODAY:-$(TZ=Asia/Tokyo date '+%Y-%m-%d')}"

    if [ -f "$STAMP_FILE" ] && [ "$(cat "$STAMP_FILE" 2>/dev/null)" = "$today" ]; then
        echo "==> 今日($today)の停止時の DB バックアップは取得済みなので取りません(1 日 1 回。手動なら make backup-now)"
        return 0
    fi
    # shellcheck disable=SC2086
    local delay
    delay=$(stop_backup_delay $now)

    if [ "$delay" -eq 0 ]; then
        echo "==> 停止直後の DB バックアップ(次の make start で DB が消えていても、この dump から自動復元できる)"
        bash -c "$BACKUP_CMD"
        local rc=$?
        if [ "$rc" -eq 0 ]; then
            mkdir -p "$RUN_DIR" && echo "$today" > "$STAMP_FILE"
        fi
        return "$rc"
    fi

    mkdir -p "$RUN_DIR"
    if [ -f "$PID_FILE" ] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
        echo "==> 場中の停止: DB バックアップは 15:31 に予約済み(pid $(cat "$PID_FILE"))— 追加しません"
        return 0
    fi
    # 子が自分の pid を書き、終わったら消す。stdout/stderr は db-backup.sh 自身のログに任せる。
    nohup bash -c "echo \$\$ > '$PID_FILE'; sleep $delay; $BACKUP_CMD && echo '$today' > '$STAMP_FILE'; rm -f '$PID_FILE'" \
        >/dev/null 2>&1 &
    # 子が pid を書く前に 2 回目の stop が来ても重複しないよう、親も書いておく(同じ値)。
    echo $! > "$PID_FILE"
    echo "==> 場中の停止なので、DB バックアップは 15:31 に回しました(pid $!・今夜 03:10 の定期分とは別)"
}

# source されたとき(テスト)は関数定義だけ。
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    main
fi
