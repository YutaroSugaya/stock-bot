#!/usr/bin/env bash
# 年次期限チェック(check_expiries / write_expiry_manifest)の自己テスト。2 部構成:
#   Part 1 = 判定関数の単体。case 分岐の手前までを source して直接叩く
#   Part 2 = morning / weekly の e2e。HOME を使い捨てに差し替えて実際に走らせる
#
# なぜ e2e も要るか: しきい値の正しさは単体で殺せるが、「どの順で呼ばれるか」と
# 「壊れた manifest が日足更新を止めないか」は呼び出し側にしか無い(判定と入力は
# 別々に検証する)。
#
# ネットワークには一切出ない。BINDIR を空のままにするので run_bin が即失敗し、
# fetch-daily / universe-screen / forward-report は 1 リクエストも出さない。
#
# **catchup の e2e はここに入れていない。**catchup は go build ×3 と make forward-report と
# snapshot-env.sh(立花の秘密鍵を HOME 配下へ複製する)を走らせるので、
# 使い捨て HOME = /tmp 配下に認証情報を落とすことになる。写しを作る本体
# write_expiry_manifest は引数で repo を受けるので Part 1 で合成ツリーごと検査でき、
# 「catchup がそれを呼ぶ」ことは構造検査(並び)で見る。
#
# **BSD date 専用**。本体が launchd 上の macOS でしか動かない前提(GNU の `date -d` は
# 使えない)なので、テストも同じ前提に置く。配線先は `make guard`。
# GNU date の環境では黙って通さず skip したと言って抜ける。
set -uo pipefail

if ! date -j -f "%Y-%m-%d %H:%M:%S" "2026-01-01 00:00:00" +%s >/dev/null 2>&1; then
    echo "stockbot-routine_test: SKIP — BSD date(-j -f)が無い。本体は macOS/launchd 専用"
    exit 0
fi

# ROUTINE_SCRIPT で差し替えられるのは変異テスト用(実装の1行を壊してこのスイートが
# 赤くなるかを見る)。既定は隣の本体。
SCRIPT="${ROUTINE_SCRIPT:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/stockbot-routine.sh}"
pass=0; fail=0
TMPROOT=$(mktemp -d "${TMPDIR:-/tmp}/routine-test.XXXXXX")
cleanup() { chmod -R u+rwx "$TMPROOT" 2>/dev/null; rm -rf "$TMPROOT"; }
trap cleanup EXIT

check() { # check <説明> <期待> <実際>
    if [ "$2" = "$3" ]; then pass=$((pass+1))
    else fail=$((fail+1)); echo "FAIL: $1 (expected [$2], got [$3])"; fi
}
has()  { # has <説明> <正規表現> <本文>
    if printf '%s' "$3" | grep -qE "$2"; then pass=$((pass+1))
    else fail=$((fail+1)); echo "FAIL: $1 — 出ていない [$2]"; fi
}
hasnt() {
    if printf '%s' "$3" | grep -qE "$2"; then fail=$((fail+1)); echo "FAIL: $1 — 出てはいけない [$2]"
    else pass=$((pass+1)); fi
}

# ---------------------------------------------------------------------------
# Part 1: 判定関数の単体
# ---------------------------------------------------------------------------
# case 分岐の手前までが「定義だけの領域」。ここを source すると副作用は
# $HOME/.stockbot/{logs,tmp} の mkdir だけになる(HOME は下で差し替え済み)。
export HOME="$TMPROOT/home"; mkdir -p "$HOME"
DEFS="$TMPROOT/defs.sh"
awk 'index($0,"case \"${1:-}\" in")==1{exit} {print}' "$SCRIPT" > "$DEFS"
# 定義領域を取り出せていること自体を確かめる(seam が壊れたら以降が全部無意味になる)。
check "seam/定義領域に check_expiries がある" yes "$(grep -q '^check_expiries()' "$DEFS" && echo yes || echo no)"
check "seam/定義領域に case 本体が混ざらない" yes "$(grep -q 'run_bin fetch-daily' "$DEFS" && echo no || echo yes)"
# shellcheck disable=SC1090
. "$DEFS"

# --- ymd_epoch: 00:00:00 に固定していること(時刻を省くと2回の呼び出しが秒をまたいで
#     日数が1ずれる)。epoch を時刻に戻して確認する。
check "ymd_epoch/真夜中に固定" "00:00:00" "$(date -r "$(ymd_epoch 2026-01-01)" '+%H:%M:%S')"
check "ymd_epoch/1日は 86400 秒" "86400" "$(( $(ymd_epoch 2026-01-02) - $(ymd_epoch 2026-01-01) ))"
check "ymd_epoch/うるう日を跨ぐ"  "86400" "$(( $(ymd_epoch 2028-03-01) - $(ymd_epoch 2028-02-29) ))"
check "ymd_epoch/読めない値は空" "" "$(ymd_epoch notadate)"
check "ymd_epoch/空も空"         "" "$(ymd_epoch '')"
check "ymd_epoch/行ごと渡されたら空" "" "$(ymd_epoch '  calendar_through: 2026-12-31')"

# --- ymd_in_file: 行内の**最初**の引用を取ること。貪欲だと年次更新のコメントから
#     古い日付を黙って拾う(値としては妥当なので検証もすり抜ける)。
y="$TMPROOT/y.yaml"
printf '  calendar_through: "2026-12-31"\n' > "$y"
check "ymd_in_file/素直な行" "2026-12-31" "$(ymd_in_file "$y" '^[[:space:]]*calendar_through:')"
printf '  calendar_through: "2027-12-31" # 前回は "2026-12-31" だった\n' > "$y"
check "ymd_in_file/行末コメントの旧値を拾わない" "2027-12-31" "$(ymd_in_file "$y" '^[[:space:]]*calendar_through:')"
printf '  calendar_through: "2027-12-31"\n  calendar_through: "2020-01-01"\n' > "$y"
check "ymd_in_file/重複キーは最初の1件" "2027-12-31" "$(ymd_in_file "$y" '^[[:space:]]*calendar_through:')"
printf '  calendar_through: 2026-12-31\n' > "$y"
check "ymd_in_file/引用が無ければ日付にならない" "" "$(ymd_epoch "$(ymd_in_file "$y" '^[[:space:]]*calendar_through:')")"
check "ymd_in_file/ファイルが無ければ空" "" "$(ymd_in_file "$TMPROOT/nope.yaml" '^x')"
printf 'const FineTickAsOf = "2026-07-27" // 検証日 / TOPIX500 は毎年10月見直し\n' > "$y"
check "ymd_in_file/Go の const 行" "2026-07-27" "$(ymd_in_file "$y" '^const FineTickAsOf')"

# --- check_expiries: manifest を差し替えて全分岐。**戻り値は常に 0**(morning を止めない)。
mkdir -p "$HOME/.stockbot"
MFST="$HOME/.stockbot/expiry-manifest"
ce() { # ce <manifest 本文 or NONE> → 出力を $CE_OUT に、戻り値を $CE_RC に
    if [ "$1" = "NONE" ]; then rm -f "$MFST"; else printf '%s' "$1" > "$MFST"; fi
    CE_OUT=$(check_expiries); CE_RC=$?
}
mf() { # mf <calendar の残り日数(負可)> <fine tick の経過日数> <snapshot の経過日数>
    # BSD date の -v は符号必須。正の残り日数には + を補う。
    local c="$1"; case "$c" in -*) ;; *) c="+$c" ;; esac
    printf 'calendar_through=%s\nfine_tick_as_of=%s\nsnapshot_at=%s\n' \
        "$(date -v"${c}d" +%F)" "$(date -v"-$2d" +%F)" "$(date -v"-$3d" +%F)"
}

# 休場カレンダー: 期限切れ / 30 日 / 60 日 の**境界を跨いで**固定する。
ce "$(mf 200 0 0)"; check "cal/余裕があれば黙る rc"  0 "$CE_RC"; hasnt "cal/余裕があれば黙る" '休場カレンダー' "$CE_OUT"
ce "$(mf 61 0 0)";  hasnt "cal/61 日は黙る"          '休場カレンダー' "$CE_OUT"
ce "$(mf 60 0 0)";  has   "cal/60 日ちょうどは ⚠"    '⚠ 休場カレンダー残り 60 日' "$CE_OUT"
ce "$(mf 31 0 0)";  has   "cal/31 日は ⚠"            '⚠ 休場カレンダー残り 31 日' "$CE_OUT"
ce "$(mf 30 0 0)";  has   "cal/30 日ちょうどは 🛑"    '🛑 休場カレンダー残り 30 日' "$CE_OUT"
ce "$(mf 1 0 0)";   has   "cal/1 日は 🛑"             '🛑 休場カレンダー残り 1 日'  "$CE_OUT"
ce "$(mf 0 0 0)";   has   "cal/当日は 🛑(まだ期限内)" '🛑 休場カレンダー残り 0 日'  "$CE_OUT"
ce "$(mf -1 0 0)";  has   "cal/1 日過ぎたら期限切れ"  '🛑 休場カレンダー期限切れ'   "$CE_OUT"
has "cal/期限切れに JPX の URL" 'jpx\.co\.jp/corporate/about-jpx/calendar' "$CE_OUT"
has "cal/期限切れに現在値"      "$(date -v-1d +%F)" "$CE_OUT"
check "cal/期限切れでも rc=0" 0 "$CE_RC"

# 呼値 tick: 300 / 365 日の境界。
ce "$(mf 200 299 0)"; hasnt "tick/299 日は黙る"       'FineTickAsOf' "$CE_OUT"
ce "$(mf 200 300 0)"; has   "tick/300 日ちょうどは ⚠" '⚠ FineTickAsOf が 300 日前' "$CE_OUT"
ce "$(mf 200 364 0)"; has   "tick/364 日は ⚠"         '⚠ FineTickAsOf が 364 日前' "$CE_OUT"
ce "$(mf 200 365 0)"; has   "tick/365 日ちょうどは 🛑" '🛑 FineTickAsOf が 365 日前' "$CE_OUT"
ce "$(mf 200 400 0)"; has   "tick/400 日は 🛑"         '🛑 FineTickAsOf が 400 日前' "$CE_OUT"

# manifest 自体の鮮度: 30 日の境界。
ce "$(mf 200 0 29)"; hasnt "snap/29 日は黙る"       'expiry-manifest が' "$CE_OUT"
ce "$(mf 200 0 30)"; has   "snap/30 日ちょうどは ⚠" '⚠ expiry-manifest が 30 日前' "$CE_OUT"
ce "$(mf 200 0 90)"; has   "snap/90 日は ⚠"         '⚠ expiry-manifest が 90 日前' "$CE_OUT"

# 壊れた入力。**黙って skip しない**(黙るとチェックごと消えて「警告が無い = 健全」と誤読される)
# かつ **rc は常に 0**(壊れた manifest が日足更新を止めるのは、このチェックが防ぐ事故そのもの)。
ce NONE;      check "壊/manifest 無し rc" 0 "$CE_RC"; has "壊/manifest 無しを報告" 'expiry-manifest が無い' "$CE_OUT"
ce "garbage
"; check "壊/garbage rc" 0 "$CE_RC"
has "壊/garbage で calendar_through が無い"  'calendar_through が無い'  "$CE_OUT"
has "壊/garbage で fine_tick_as_of が無い"   'fine_tick_as_of が無い'   "$CE_OUT"
has "壊/garbage で snapshot_at が無い"       'snapshot_at が無い'       "$CE_OUT"
has "壊/再生成で直らない場合の逃げ道を書く"  '抽出元の書式変更'         "$CE_OUT"
ce "calendar_through=
fine_tick_as_of=
snapshot_at=
"; has "壊/値だけ空でも報告" 'calendar_through が無い' "$CE_OUT"; check "壊/値だけ空 rc" 0 "$CE_RC"
ce "calendar_through=notadate
fine_tick_as_of=2026-13-45
snapshot_at=zzz
"; check "壊/読めない日付 rc" 0 "$CE_RC"
# 「期限切れ」だけを禁じると、壊れた入力から日数を捏造して「残り 0 日」と言う実装を通す。
# 読めない値からは**日数の話を一切しない**ことを禁止側で固定する。
hasnt "壊/読めない日付で誤った期限切れを出さない" '期限切れ' "$CE_OUT"
hasnt "壊/読めない日付から日数を捏造しない"       '休場カレンダー残り' "$CE_OUT"
hasnt "壊/読めない日付から日数を捏造しない(tick)" 'FineTickAsOf が' "$CE_OUT"
has "壊/読めない calendar_through を報告" "calendar_through が日付として読めない\('notadate'\)" "$CE_OUT"
has "壊/読めない fine_tick_as_of を報告"  "fine_tick_as_of が日付として読めない"  "$CE_OUT"
has "壊/読めない snapshot_at を報告"      "snapshot_at が日付として読めない"      "$CE_OUT"
ce "calendar_through=$(date -v+30d +%F)
calendar_through=2020-01-01
fine_tick_as_of=$(date +%F)
snapshot_at=$(date +%F)
"
has   "壊/重複キーは1件目を使う"       '🛑 休場カレンダー残り 30 日' "$CE_OUT"
hasnt "壊/重複キーを壊れ扱いにしない" '読めない' "$CE_OUT"
ce "calendar_through=$(date -v+200d +%F)
"; has "壊/部分 manifest でも在るキーは評価" 'fine_tick_as_of が無い' "$CE_OUT"
hasnt "壊/部分 manifest で在るキーは黙る" '休場カレンダー' "$CE_OUT"
ce "$(mf 200 0 0)"; rm -f "$MFST"; mkdir -p "$MFST"
CE_OUT=$(check_expiries); check "壊/manifest がディレクトリ rc" 0 "$?"
rmdir "$MFST"

# --- write_expiry_manifest: 合成 repo を食わせて抽出規則ごと検査する。
mkrepo() { # mkrepo <dir> <calendar 行> <fine tick 行>
    mkdir -p "$1/configs" "$1/backend/internal/domain/market"
    # **アンカーを外さないと拾ってしまうコメント行を必ず先に置く。**実ファイルがその形
    # (hard_limits.yaml はキーの直前に説明コメントがある)で、行選択の
    # `^[[:space:]]*` / `^const` はそこを外すためだけに在る。コメントに「キー名 + コロン」
    # と「const」まで含めないと、アンカーを緩める変異が緑のまま通る。
    # 日付も入れておく: 緩んだ実装は**古い方を黙って採用**するので値で判別できる。
    { echo '  # 旧: calendar_through: "2020-01-01" だった'
      printf '%s\n' "$2"; } > "$1/configs/hard_limits.yaml"
    { echo '// 旧値: const FineTickAsOf = "2020-01-01"'
      printf '%s\n' "$3"; } > "$1/backend/internal/domain/market/tick_fine.go"
}
R="$TMPROOT/repo"
mkrepo "$R" '  calendar_through: "2029-12-31"' 'const FineTickAsOf = "2029-07-27" // 検証日'
out=$(write_expiry_manifest "$R"); rc=$?
check "wm/rc=0" 0 "$rc"
check "wm/calendar_through" "2029-12-31" "$(sed -n 's/^calendar_through=//p' "$MFST")"
check "wm/fine_tick_as_of"  "2029-07-27" "$(sed -n 's/^fine_tick_as_of=//p' "$MFST")"
check "wm/snapshot_at=今日" "$(date +%F)" "$(sed -n 's/^snapshot_at=//p' "$MFST")"
has "wm/更新をログに出す" 'expiry-manifest 更新' "$out"
hasnt "wm/正常時は警告を出さない" '⚠' "$out"
# 置換が原子的であること: 先に .tmp を置いておくと、直接書き(非原子)なら残る。
: > "$MFST.tmp"
write_expiry_manifest "$R" >/dev/null
check "wm/.tmp を残さない" no "$([ -e "$MFST.tmp" ] && echo yes || echo no)"
# 置けない(~/.stockbot が書込不可)ときも make start を止めず、理由を言い、.tmp も残さない。
rm -f "$MFST"; chmod 500 "$HOME/.stockbot"
# 2>&1 で受けるのは、リダイレクト失敗そのものを bash が自分の stderr に出すため
# (本番ではこれが morning.log に残るのは有用なので、実装側では抑止しない)。
out=$(write_expiry_manifest "$R" 2>&1); check "wm/置けなくても rc=0" 0 "$?"
has   "wm/置けないことを報告" '⚠ expiry-manifest を書けない' "$out"
check "wm/失敗時も .tmp を残さない" no "$([ -e "$MFST.tmp" ] && echo yes || echo no)"
chmod 700 "$HOME/.stockbot"
# 抽出できない書式に変わったら、その場で言う(黙って空を書かない)。
mkrepo "$R" '  calendar_through: 2026-12-31' 'const FineTickAsOf = 2026-07-27'
out=$(write_expiry_manifest "$R"); check "wm/抽出失敗でも rc=0" 0 "$?"
has "wm/calendar の抽出失敗を報告" 'calendar_through を取り出せない' "$out"
has "wm/FineTick の抽出失敗を報告" 'FineTickAsOf を取り出せない'     "$out"
rm -rf "$R"; out=$(write_expiry_manifest "$R"); check "wm/repo 不在でも rc=0" 0 "$?"
has "wm/repo が無くても報告する" 'calendar_through を取り出せない' "$out"

# mv が失敗する経路(置き先が書込不可ディレクトリ)。redirect は成功して .tmp が出来るので、
# 後始末が無いと残骸になる。1 巡目の「chmod 500 の親」ケースは redirect 自体が落ちて
# .tmp が生まれず、後始末を 1 度も拘束していなかった。
mkrepo "$R" '  calendar_through: "2029-12-31"' 'const FineTickAsOf = "2029-07-27"'
rm -f "$MFST"; mkdir -p "$MFST"; chmod 500 "$MFST"
out=$(write_expiry_manifest "$R" 2>&1); check "wm/mv 失敗でも rc=0" 0 "$?"
has   "wm/mv 失敗を報告" '⚠ expiry-manifest を書けない' "$out"
check "wm/mv 失敗時に .tmp を残さない" no "$([ -e "$MFST.tmp" ] && echo yes || echo no)"
chmod 700 "$MFST"; rm -rf "$MFST"

# 朝ジョブの写しを repo に合わせる(**配布する**)。警告だけだと launchd は旧版を動かし続ける。
# バイナリ3本 / hard_limits.yaml / env snapshot は catchup が既に毎回配っており、
# このスクリプトと plist も同じ扱いにする。
LIVE="$HOME/.stockbot/stockbot-routine.sh"
mkdir -p "$TMPROOT/rp/scripts" "$HOME/.stockbot" "$HOME/Library/LaunchAgents"

# 写しが無ければ作る(初回セットアップでも朝ジョブが動く)。
rm -f "$LIVE"; printf 'a\n' > "$TMPROOT/rp/scripts/stockbot-routine.sh"
out=$(sync_routine_assets "$TMPROOT/rp"); check "sync/写しが無くても rc=0" 0 "$?"
check "sync/写しを作る"           "a"  "$(cat "$LIVE" 2>/dev/null)"
check "sync/実行ビットを立てる"   yes  "$([ -x "$LIVE" ] && echo yes || echo no)"
has   "sync/作成を報告する"       'スクリプトを更新' "$out"

# 同一なら黙る。毎回の make start でログを汚すと、本物の更新が埋もれる。
check "sync/同一なら黙る" "" "$(sync_routine_assets "$TMPROOT/rp")"

# 違えば**配る**。
printf 'b\n' > "$TMPROOT/rp/scripts/stockbot-routine.sh"
out=$(sync_routine_assets "$TMPROOT/rp"); check "sync/更新しても rc=0" 0 "$?"
check "sync/写しを更新する" "b" "$(cat "$LIVE")"

# 🛑 元が空なら上書きしない(fail-close)。切れた写しを launchd が実行すると**無音で死ぬ**。
: > "$TMPROOT/rp/scripts/stockbot-routine.sh"
sync_routine_assets "$TMPROOT/rp" >/dev/null
check "sync/空の元で上書きしない" "b" "$(cat "$LIVE")"

# 🛑 原子的置換: .tmp を残さない(残骸を朝ジョブが拾う経路を作らない)。
check "sync/.tmp を残さない" no "$([ -e "$LIVE.tmp" ] && echo yes || echo no)"

# 元ごと無くても make start を止めない。
rm -rf "$TMPROOT/rp"
out=$(sync_routine_assets "$TMPROOT/rp"); check "sync/元が無くても rc=0" 0 "$?"
check "sync/元が無ければ写しはそのまま" "b" "$(cat "$LIVE")"
rm -f "$LIVE"

# catchup が write_expiry_manifest を、make fetch-daily(約40分)より**前**に呼ぶこと。
# ここだけ構造検査(振る舞いでなく並び)なのは、catchup の e2e が go build ×3 と
# snapshot-env.sh(立花の秘密鍵を HOME 配下へ複製する)を伴い、使い捨て
# HOME = /tmp 配下に認証情報を落とすことになるため。
cb=$(awk '/^  catchup\)/{f=1} f{print} f&&/^    ;;$/{exit}' "$SCRIPT")
check "catchup/write_expiry_manifest を呼ぶ" yes \
    "$(printf '%s' "$cb" | grep -q 'write_expiry_manifest' && echo yes || echo no)"
w=$(printf '%s' "$cb" | grep -n 'write_expiry_manifest' | head -1 | cut -d: -f1)
# 行頭アンカーで**コメント中の言及**を除く(直上のコメントが同じ綴りを含む)。
ff=$(printf '%s' "$cb" | grep -nE '^[[:space:]]*make fetch-daily' | head -1 | cut -d: -f1)
check "catchup/fetch-daily より前で呼ぶ" yes \
    "$([ -n "$w" ] && [ -n "$ff" ] && [ "$w" -lt "$ff" ] && echo yes || echo no)"
check "catchup/sync_routine_assets を呼ぶ" yes \
    "$(printf '%s' "$cb" | grep -q 'sync_routine_assets' && echo yes || echo no)"

# 起動前の forward-report は repo の .env の DSN で集計する(シェルに残った別の DSN を拾わない)。
# bot 本体(Makefile の LOADENV)も launchd 側も .env が勝つ。振る舞いでなく構造で
# 見る理由は上の write_expiry_manifest と同じ(catchup の e2e は認証情報を HOME へ複製する)。
envl=$(printf '%s' "$cb" | grep -nE '^[[:space:]]*if \[ -r \./\.env \]; then set -a; \. \./\.env' | head -1 | cut -d: -f1)
frl=$(printf '%s' "$cb" | grep -nE '^[[:space:]]*make forward-report' | head -1 | cut -d: -f1)
check "catchup/.env を DSN の有無に依らず forward-report より前で読む" yes \
    "$([ -n "$envl" ] && [ -n "$frl" ] && [ "$envl" -lt "$frl" ] && echo yes || echo no)"
check "catchup/DSN の有無を条件に .env を読まない" no \
    "$(printf '%s' "$cb" | grep -qE 'DATABASE_URL[^#]*(source|\.)[[:space:]]+\./\.env' && echo yes || echo no)"

# --- tachibana_off_hours: 立花に触ってはいけない時間か(真 = 触らない)。
# 閉局中・休日・メンテ中は立花に触らない。触ってよいのは取引日の 06:00〜24:00。
offh() { # offh <calendar> <YYYY-MM-DD> <HHMM> → off / on
    if tachibana_off_hours "$1" "$2" "$3"; then echo off; else echo on; fi
}
CAL="$TMPROOT/cal.yaml"
# 罠を先に置く: holidays の外のリスト・コメント行・行末コメントに出る日付は休場日ではない。
cat > "$CAL" <<'YAML'
  allowed_dates:
    - "2026-10-14"
  # 旧: calendar_through: "2020-01-01" だった
  calendar_through: "2027-12-31"
  holidays:
    - "2026-10-12"   # スポーツの日(前回は "2026-10-15" だった)
    # - "2026-10-13"   コメントアウトした行は休場日ではない
    - "2026-11-03"   # 文化の日

paper:
  later_dates:
    - "2026-10-16"
YAML
check "off/平日の朝 07:00 は触ってよい"     on  "$(offh "$CAL" 2026-10-13 0700)"
check "off/土曜は触らない"                   off "$(offh "$CAL" 2026-10-10 0700)"
check "off/日曜は触らない"                   off "$(offh "$CAL" 2026-10-11 0700)"
check "off/休場日(holidays)は触らない"      off "$(offh "$CAL" 2026-10-12 0700)"
check "off/休場日リストの 2 件目も効く"       off "$(offh "$CAL" 2026-11-03 0700)"
check "off/コメントアウトした日は休場日でない" on  "$(offh "$CAL" 2026-10-13 0700)"
check "off/行末コメントの日付は休場日でない"   on  "$(offh "$CAL" 2026-10-15 0700)"
check "off/holidays より前のリストは見ない"    on  "$(offh "$CAL" 2026-10-14 0700)"
check "off/holidays の後のリストは見ない"      on  "$(offh "$CAL" 2026-10-16 0700)"
# 閉局(03:30〜05:30)に余裕を持たせて 06:00 から。日を跨いだ 0 時台も触らない。
check "off/05:59 は触らない"                 off "$(offh "$CAL" 2026-10-13 0559)"
check "off/06:00 から触ってよい"             on  "$(offh "$CAL" 2026-10-13 0600)"
check "off/23:59 は触ってよい"               on  "$(offh "$CAL" 2026-10-13 2359)"
check "off/00:00 は触らない"                 off "$(offh "$CAL" 2026-10-14 0000)"
check "off/03:40(閉局中)は触らない"        off "$(offh "$CAL" 2026-10-14 0340)"
# 🛑 カレンダーが読めない / 期限切れは**触らない側**(bot の IsTradingDay と同じ fail-close)。
check "off/カレンダーが無ければ触らない"       off "$(offh "$TMPROOT/no-such-cal.yaml" 2026-10-13 0700)"
printf '  calendar_through: "2026-10-12"\n  holidays:\n    - "2026-01-01"\n' > "$TMPROOT/cal-old.yaml"
check "off/calendar_through を過ぎたら触らない" off "$(offh "$TMPROOT/cal-old.yaml" 2026-10-13 0700)"
check "off/calendar_through 当日は触ってよい"   on  "$(offh "$TMPROOT/cal-old.yaml" 2026-10-12 0700)"
printf '  holidays:\n    - "2026-01-01"\n' > "$TMPROOT/cal-nothrough.yaml"
check "off/calendar_through が無ければ触らない" off "$(offh "$TMPROOT/cal-nothrough.yaml" 2026-10-13 0700)"
check "off/読めない日付は触らない"             off "$(offh "$CAL" 2026-13-45 0700)"
# 引数を省くと今の日時で判定する(朝ジョブと catchup はこの形で呼ぶ)。
now_want=$(offh "$CAL" "$(date +%F)" "$(date +%H%M)")
check "off/引数省略は今の日時" "$now_want" "$(if tachibana_off_hours "$CAL"; then echo off; else echo on; fi)"

# catchup も make fetch-daily の**前**で同じ判定を通す(土日に make start しても履歴を引かない)。
oh=$(printf '%s' "$cb" | grep -nE '^[[:space:]]*(el)?if tachibana_off_hours' | head -1 | cut -d: -f1)
check "catchup/fetch-daily より前で tachibana_off_hours を見る" yes \
    "$([ -n "$oh" ] && [ -n "$ff" ] && [ "$oh" -lt "$ff" ] && echo yes || echo no)"
check "catchup/判定に repo の hard_limits.yaml を渡す" yes \
    "$(printf '%s' "$cb" | grep -qE 'tachibana_off_hours "\$REPO/configs/hard_limits\.yaml"' && echo yes || echo no)"

# --- 朝ジョブの launchd は**平日だけ**発火する(土日に起動しない)。曜日の無い定義は毎日動く。
PL="$(dirname "$SCRIPT")/com.stockbot.morning.plist"
plcal=$(plutil -extract StartCalendarInterval json -o - "$PL" 2>/dev/null)
plent=$(printf '%s' "$plcal" | grep -o '{[^}]*}')
check "plist/morning は 5 本" 5 "$(printf '%s\n' "$plent" | grep -c '{')"
check "plist/morning の曜日は月〜金" "1 2 3 4 5" \
    "$(printf '%s\n' "$plent" | grep -o '"Weekday":[0-9]*' | cut -d: -f2 | sort -un | tr '\n' ' ' | sed 's/ $//')"
check "plist/曜日の無い発火が無い" 0 "$(printf '%s\n' "$plent" | grep -c -v '"Weekday"')"
check "plist/morning は 07:00" 5 "$(printf '%s\n' "$plent" | grep '"Hour":7' | grep -c '"Minute":0')"

# ---------------------------------------------------------------------------
# Part 2: morning / weekly の e2e(HOME 差し替え・バイナリ無しでネットワークに出ない)
# ---------------------------------------------------------------------------
E="$TMPROOT/e2e"; mkdir -p "$E/.stockbot/data"
: > "$E/.stockbot/data/9999_daily.csv"
touch "$E/.stockbot/last-intraday-backfill"   # 分足補填の subprocess を呼ばせない
run_task() { # run_task <task> <manifest 本文 or NONE> → $OUT / $RC
    if [ "$2" = "NONE" ]; then rm -f "$E/.stockbot/expiry-manifest"
    else printf '%s' "$2" > "$E/.stockbot/expiry-manifest"; fi
    OUT=$(HOME="$E" bash "$SCRIPT" "$1" 2>&1); RC=$?
}

run_task morning "$(mf -1 400 90)"
check "e2e/morning rc=0" 0 "$RC"
has "e2e/期限切れ警告"     '🛑 休場カレンダー期限切れ' "$OUT"
has "e2e/FineTick 警告"    '🛑 FineTickAsOf が 400 日前' "$OUT"
has "e2e/manifest 鮮度警告" '⚠ expiry-manifest が 90 日前' "$OUT"
# 警告は**全ての I/O より前**に出す。後ろに置くと、fetch-daily(約40分)や分足補填が
# 詰まった朝に 1 行も出ない。
a=$(printf '%s' "$OUT" | grep -n '休場カレンダー期限切れ' | head -1 | cut -d: -f1)
b=$(printf '%s' "$OUT" | grep -nE '== fetch-daily ==|場中のため' | head -1 | cut -d: -f1)
check "e2e/警告が fetch-daily より前" yes "$([ -n "$a" ] && [ -n "$b" ] && [ "$a" -lt "$b" ] && echo yes || echo no)"
# 誤誘導文言: 「DB 停止中?」は TCC 障害でも同じ文言が出て誤診断を招く。
has   "e2e/morning の forward-report 文言" 'forward-report 失敗\(DB 停止 / TCC / snapshot 未生成 のいずれか — 直上の行を確認\)' "$OUT"
hasnt "e2e/morning に旧文言が残っていない" 'DB 停止中\?' "$OUT"

# 壊れた manifest / 未生成でも morning は走り切る(最重要)。
for m in "garbage
" "NONE" "calendar_through=notadate
"; do
    run_task morning "$m"
    check "e2e/壊れた manifest でも rc=0" 0 "$RC"
    has   "e2e/壊れた manifest でも fetch-daily 段に到達" '== fetch-daily ==|場中のため' "$OUT"
    has   "e2e/壊れた manifest でも forward-report 段に到達" '== forward-report ==' "$OUT"
done

# 休場日の朝は fetch-daily を起動しない。今日を休場日にしたカレンダーを置く。
# 日足は昨日付にしておく(「本日更新済み」の skip と取り違えないため)。
touch -t "$(date -v-1d +%Y%m%d)1200" "$E/.stockbot/data/9999_daily.csv"
printf '  calendar_through: "2099-12-31"\n  holidays:\n    - "%s"\n' "$(date +%F)" > "$E/.stockbot/hard_limits.yaml"
run_task morning "$(mf 200 0 0)"
check "e2e/休場日でも morning rc=0" 0 "$RC"
has   "e2e/休場日は fetch-daily を飛ばすと言う" 'skip: 立花の休場日・時間外' "$OUT"
hasnt "e2e/休場日は fetch-daily を起動しない" 'fetch-daily のバイナリが無い|fetch-daily 失敗' "$OUT"
has   "e2e/休場日でも universe-screen 段に到達" '== universe-screen' "$OUT"
# カレンダーが無ければ触らない(fail-close)。
rm -f "$E/.stockbot/hard_limits.yaml"
run_task morning "$(mf 200 0 0)"
hasnt "e2e/カレンダーが無ければ fetch-daily を起動しない" 'fetch-daily のバイナリが無い|fetch-daily 失敗' "$OUT"
# 取引日の時間内(場中を除く)に走ったときだけ、撃ちに行くことも確かめる(バイナリが無いので失敗で終わる)。
printf '  calendar_through: "2099-12-31"\n  holidays:\n    - "2000-01-01"\n' > "$E/.stockbot/hard_limits.yaml"
if ! tachibana_off_hours "$E/.stockbot/hard_limits.yaml" && ! in_session_hours; then
    run_task morning "$(mf 200 0 0)"
    has "e2e/取引日の時間内は fetch-daily を起動する" 'fetch-daily のバイナリが無い' "$OUT"
fi
rm -f "$E/.stockbot/hard_limits.yaml"

# env snapshot が壊れていても警告は出る。`. env` は `set -u` 下で未定義変数参照に当たると
# **非対話 shell ごと即死**し、`2>/dev/null` が理由まで飲むので、check_expiries を
# load_snapshot_env の後ろに置くと 1 行も出さずに morning が死ぬ。
printf 'X=${THIS_VAR_IS_NOT_SET}\n' > "$E/.stockbot/env"
run_task morning "$(mf -1 0 0)"
has "e2e/壊れた env snapshot でも期限警告は出る" '🛑 休場カレンダー期限切れ' "$OUT"
rm -f "$E/.stockbot/env"

printf 'STOCKBOT_DATABASE_URL=postgres://example\n' > "$E/.stockbot/env"
run_task weekly "$(mf 200 0 0)"
has   "e2e/weekly の forward-report 文言" 'forward-report 失敗\(DB 停止 / TCC / snapshot 未生成 のいずれか — 直上の行を確認\)' "$OUT"
hasnt "e2e/weekly に旧文言が残っていない" 'DB 停止中\?' "$OUT"

# copy_gonogo_configs: launchd 配下の gonogo は repo を読めないので、
# bot と同じ env が指す設定を ~/.stockbot/configs へ写し、一覧を書く。live が無効なら live の行は無い。
(
  # shellcheck disable=SC1090
  . "$DEFS"
  REPO="$TMPROOT/gonogo-repo"; mkdir -p "$REPO/configs"
  for f in bot_config.advisor.yaml bot_config.live.yaml strategy_config.live.yaml strategy_config.live2.yaml; do
    echo "x: 1" > "$REPO/configs/$f"
  done
  unset STOCKBOT_BOT_CONFIG STOCKBOT_LIVE_DISABLED
  STOCKBOT_LIVE_BOT_CONFIG="../configs/bot_config.live.yaml"
  STOCKBOT_LIVE_STRATEGY_CONFIG="configs/strategy_config.live.yaml, configs/strategy_config.live2.yaml"
  copy_gonogo_configs >/dev/null
  echo "LIST=$(tr '\n' ' ' < "$HOME/.stockbot/configs/gonogo-configs.txt")"
  echo "COPIED=$(ls "$HOME/.stockbot/configs" | tr '\n' ' ')"
  STOCKBOT_LIVE_DISABLED=1 copy_gonogo_configs >/dev/null
  echo "DISABLED=$(tr '\n' ' ' < "$HOME/.stockbot/configs/gonogo-configs.txt")"
) > "$TMPROOT/gonogo-copy.out" 2>&1
GC=$(cat "$TMPROOT/gonogo-copy.out")
has   "gonogo/paper の行"                 'LIST=paper=bot_config.advisor.yaml' "$GC"
has   "gonogo/live の行は優先順"           'live_bot=bot_config.live.yaml live_strategy=strategy_config.live.yaml live_strategy=strategy_config.live2.yaml' "$GC"
has   "gonogo/写しがある"                 'COPIED=.*strategy_config.live2.yaml' "$GC"
hasnt "gonogo/live 無効なら live の行が無い" 'DISABLED=.*live_' "$GC"
has   "gonogo/live 無効でも paper は残る"   'DISABLED=paper=' "$GC"

# forward_report_brief: 起動前の forward-report は全文をファイルへ書き、端末には期間と ¥1M 正規化の 2 行と
# 置き場だけを出す(全文は約 45 行で、起動のたびに端末を埋めていた)。
(
  # shellcheck disable=SC1090
  . "$DEFS"
  f="$TMPROOT/fr.txt"
  {
    echo "cd backend && go run ./cmd/forward-report -since 2026-09-14"
    echo "── forward-report(trades 台帳・net = gross − fee + carry)──"
    echo "期間: 2026-09-14 以降  N=12  勝ち 5 (41.7%)  net +12345 JPY(gross +13000 / fee 0 / carry -655)  銘柄数 6"
    echo "¥1M notional 正規化: net -2345 JPY / 1本あたり -195  ← edge-judge はこちらを判定する"
    echo "戦略別(net 降順 — 実弾判断はこの分計と edge-judge で):"
    echo "  bnf_reversion            N=4   勝ち 1   net +1000  (¥1M正規化 -500)"
  } > "$f"
  forward_report_brief "$f"
  echo "EMPTY=$(forward_report_brief "$TMPROOT/no-such-report.txt" | tr '\n' ' ')"
) > "$TMPROOT/fr-brief.out" 2>&1
FR=$(cat "$TMPROOT/fr-brief.out")
has   "forward-report/期間の行"            '期間: 2026-09-14 以降  N=12' "$FR"
has   "forward-report/¥1M の行"            '¥1M notional 正規化: net -2345' "$FR"
has   "forward-report/全文の置き場"         'fr.txt' "$FR"
hasnt "forward-report/戦略別は出さない"      '戦略別' "$FR"
hasnt "forward-report/コマンド行は出さない"   'go run ./cmd/forward-report' "$FR"
has   "forward-report/無ければ失敗と言う"    'EMPTY=.*集計を読めない' "$FR"

# ---------------------------------------------------------------------------
# ensure_db が呼ぶ scripts/*.sh: `$var` の直後に全角文字を置かない
# ---------------------------------------------------------------------------
# macOS の bash 3.2 は UTF-8 ロケールで全角文字の先頭バイトを変数名の一部として読み、`set -u` の
# 下では "latest_migration\xe3: unbound variable" で落ちる。全角文字が続くときは `${var}` と波括弧で囲む。
bare_vars=$(LC_ALL=C grep -nE '\$[A-Za-z_][A-Za-z0-9_]*[^ -~[:space:]]' "$(dirname "$SCRIPT")"/*.sh || true)
check "scripts/*.sh に全角文字が直後に続く \$var が無い" "" "$bare_vars"

# ---------------------------------------------------------------------------
# docker-compose.yml: Postgres のデータは Docker の仮想ディスクの外(Mac 側のフォルダ)に置く
# ---------------------------------------------------------------------------
# Docker Desktop が `~/Library/Containers/com.docker.docker/Data` ごと作り直すと、名前付きボリュームに
# 入っていた台帳が消える。bind mount なら仮想ディスクが消えても残る。
COMPOSE=$(cat "$(dirname "$SCRIPT")/../docker-compose.yml")
has   "compose/pgdata は Mac 側のフォルダ"      '\$\{HOME\}/\.stockbot/pgdata:/var/lib/postgresql/data' "$COMPOSE"
hasnt "compose/名前付きボリュームに置かない"     'stockbot_pgdata:/var/lib/postgresql/data' "$COMPOSE"

echo "stockbot-routine_test: pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
