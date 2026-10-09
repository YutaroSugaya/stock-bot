#!/usr/bin/env bash
# session-start_test.sh — session-start.sh(H1)の自己テスト。全 pass で exit 0。
#
# この hook は「セッション起動を絶対にブロックしない」ことが最上位の契約なので、
# 壊れた入力・壊れたツリー・書けない runtime/logs のいずれでも exit 0 を要求する。
set -uo pipefail

# 🚨 **このテスト自身も git 環境から独立していなければならない**(2026-08-21)。
#
# `make guard` は `git push` の pre-push フックからも走る。git は hook の子プロセスへ
# `GIT_DIR` を渡し、**それは `git -C <dir>` より優先される** —— つまり下の
# `git -C "$P" init` / `commit` が**一時ディレクトリではなく実リポに向かう**。
# 一時リポは作られないまま、`git rev-parse HEAD` は実リポの HEAD を返し、
# 「コミットゼロのリポ」を前提にした t29 系が落ちる(= push が通らない)。
#
# hook 側でも同じ unset をしているが、あちらは「本番で乗っ取られない」ため。
# ここは「テストが作る一時リポが本当に一時リポであること」のため —— 別の理由で両方要る。
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_COMMON_DIR
# `.env` を source したシェルから make guard を打っても結果が変わらないように、flag の env を落とす
# (使うケースは明示的に渡す)。
unset STOCKBOT_EMERGENCY_FLAG STOCKBOT_LIVE_EMERGENCY_FLAG

HOOKDIR="$(cd "$(dirname "$0")" && pwd)"
HOOK="$HOOKDIR/session-start.sh"
pass=0; fail=0; skip=0

ok()  { pass=$((pass + 1)); }
ng()  { fail=$((fail + 1)); echo "FAIL [$1] $2"; }
eq()  { # $1=desc $2=expected $3=actual
  if [ "$2" = "$3" ]; then ok; else ng "$1" "expected [$2] got [$3]"; fi
}
has() { # $1=desc $2=needle $3=haystack
  if printf '%s' "$3" | grep -qF -- "$2"; then ok; else ng "$1" "missing [$2]"; fi
}
hasnt() { # $1=desc $2=needle $3=haystack
  if printf '%s' "$3" | grep -qF -- "$2"; then ng "$1" "unexpected [$2]"; else ok; fi
}
rx() { # $1=desc $2=ERE $3=haystack
  if printf '%s' "$3" | grep -qE -- "$2"; then ok; else ng "$1" "no match /$2/"; fi
}

TMPROOT="$(mktemp -d)"
trap 'rm -rf "$TMPROOT"' EXIT

# STATUS.md は 30 行にしておく(hook が 25 行で切ることを 26 行目の不在で確かめるため)。
mkproj() {
  local d n=1
  d="$(mktemp -d "$TMPROOT/proj.XXXXXX")"
  mkdir -p "$d/docs/runtime" "$d/backend/runtime" "$d/backend/data"
  while [ "$n" -le 30 ]; do echo "STATUSLINE$n" >> "$d/docs/runtime/STATUS.md"; n=$((n + 1)); done
  # CSV は 2 本置いて古い方を辞書順で先に来る名前にする。1 本だけだと `ls -t` を `ls` に
  # 変異させても緑のまま = 「最新を選ぶ」ことが 1 ケースも拘束されない。
  echo "date,close" > "$d/backend/data/1111_daily.csv"
  touch -t 202601010000 "$d/backend/data/1111_daily.csv"
  echo "date,close" > "$d/backend/data/1234_daily.csv"
  git -C "$d" init -q
  git -C "$d" -c user.email=t@t -c user.name=t commit -q --allow-empty -m "commit one"
  git -C "$d" -c user.email=t@t -c user.name=t commit -q --allow-empty -m "commit two"
  git -C "$d" -c user.email=t@t -c user.name=t commit -q --allow-empty -m "commit three"
  git -C "$d" -c user.email=t@t -c user.name=t commit -q --allow-empty -m "commit four"
  printf '%s' "$d"
}

sj() { printf '{"session_id":"%s","source":"startup"}' "$1"; }

# OUT / RC をグローバルに置く。$( ) の中で case を使わないのは bash 3.2 対策(f894b30)。
run_hook() { # $1=projdir $2=stdin
  # bot の稼働判定は lsof でポートを見る(session-start.sh は STOCKBOT_HTTP_ADDR を読む)。
  # ここを実マシンの状態任せにすると、bot を起動したまま make check を打つと t6 が必ず
  # 落ちる — 2026-08-10 は取引時間中ずっと赤だった。make は1本落ちた時点で止まるので、
  # その間 make guard の後続3本(secret-scan / docs-sync-check / pre-stop-tdd の各自己
  # テスト)が丸ごと走らない。誰も LISTEN していないポートを指して判定を決定的にする。
  # RUNNING 側は t11b が実ソケットを bind して同じやり方で見ている(対になる)。
  OUT="$(printf '%s' "$2" \
    | STOCKBOT_HTTP_ADDR="127.0.0.1:1" CLAUDE_PROJECT_DIR="$1" bash "$HOOK" 2>/dev/null)"
  RC=$?
}

# ---------------------------------------------------------------------------
# 1. 正常系 — exit 0 / 出力の中身 / 40 行以内
# ---------------------------------------------------------------------------
P="$(mkproj)"
run_hook "$P" "$(sj sess-normal)"
eq   "t1 exit 0"            0 "$RC"
lines="$(printf '%s\n' "$OUT" | wc -l | tr -d ' ')"
if [ "$lines" -le 40 ]; then ok; else ng "t2 40 行以内" "got $lines lines"; fi
# mkproj の STATUS.md には now マーカーが無い = 先頭 25 行への縮退の経路。
has  "t3 STATUS 1 行目"      "STATUSLINE1"  "$OUT"
has  "t4 STATUS 25 行目"     "STATUSLINE25" "$OUT"
hasnt "t5 STATUS 26 行目は出さない" "STATUSLINE26" "$OUT"
has  "t5b 縮退したことを見出しで言う" "now マーカーが揃っていない" "$OUT"
# lsof は CI(ubuntu)に無いことがある。「どちらでもよい」にすると分岐が拘束されなくなるので、
# 環境を見て期待値を切り替える。
if command -v lsof >/dev/null 2>&1; then
  has "t6 bot 稼働状態"           "bot: not running" "$OUT"
else
  has "t6 bot 稼働状態(lsof 無し)" "bot: 不明"        "$OUT"
fi
has  "t7 emergency 行(research)" "emergency_stop(research):" "$OUT"
has  "t7b emergency 行(live)"     "emergency_stop(live):"     "$OUT"
has  "t8 直近コミット"        "commit four"  "$OUT"
has  "t9 コミットは 3 件"      "commit two"   "$OUT"
hasnt "t10 4 件目は出さない"   "commit one"   "$OUT"
has  "t11 日足の鮮度"         "data: 最新 1234_daily.csv" "$OUT"
# mtime そのものを拘束する。ここが緩いと、GNU の `stat -f` が吐く FS 情報のゴミが
# mtime 欄に出ていても 44 件全緑のまま通る(レビュー実測 P1)。
rx   "t11a mtime が YYYY-MM-DD HH:MM" '^data: 最新 1234_daily\.csv = [0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2} ' "$OUT"
hasnt "t11d 古い方を最新と言わない" "最新 1111_daily.csv" "$OUT"

# 稼働中の側。**片方だけ試すと「RUNNING を出す分岐」が丸ごと未検証で残る**(実際、
# 変異テストでこの行を壊しても 41 件全緑だった)。実弾の 8090 は掴まずに、
# 空きポートを 1 つ listen して STOCKBOT_HTTP_ADDR で指す。
if command -v python3 >/dev/null 2>&1 && command -v lsof >/dev/null 2>&1; then
  portfile="$TMPROOT/port"
  python3 -c 'import socket,sys,time
s = socket.socket(); s.bind(("127.0.0.1", 0)); s.listen(1)
open(sys.argv[1], "w").write(str(s.getsockname()[1]))
time.sleep(20)' "$portfile" &
  lpid=$!
  n=0
  while [ ! -s "$portfile" ] && [ "$n" -lt 100 ]; do sleep 0.05; n=$((n + 1)); done
  lport="$(cat "$portfile" 2>/dev/null || true)"
  if [ -n "$lport" ]; then
    OUT="$(printf '%s' "$(sj sess-running)" \
      | STOCKBOT_HTTP_ADDR="127.0.0.1:$lport" CLAUDE_PROJECT_DIR="$P" bash "$HOOK" 2>/dev/null)"
    has "t11b listen 中は RUNNING" "bot: RUNNING (127.0.0.1:$lport)" "$OUT"
  else
    skip=$((skip + 1)); echo "SKIP t11b (listen ポートを取れなかった)"
  fi
  kill "$lpid" 2>/dev/null; wait "$lpid" 2>/dev/null
else
  skip=$((skip + 1)); echo "SKIP t11b (python3 か lsof が無い)"
fi

# ---------------------------------------------------------------------------
# 2. emergency_stop フラグ
# ---------------------------------------------------------------------------
hasnt "t12 フラグ無しで発動中と言わない" "発動中" "$OUT"
touch "$P/backend/runtime/emergency_stop.flag"
run_hook "$P" "$(sj sess-emg)"
eq   "t13 フラグありでも exit 0" 0 "$RC"
has  "t14 フラグありで発動中"     "emergency_stop(research): 🛑 発動中" "$OUT"
# research の flag を live の発動と言わない(行を取り違えると live を止めたと誤読する)。
hasnt "t14a research の flag で live を発動中と言わない" "emergency_stop(live): 🛑" "$OUT"
rm -f "$P/backend/runtime/emergency_stop.flag"

# STOCKBOT_EMERGENCY_FLAG による差し替え。探索リストから外しても既定パスのケースは緑の
# ままなので、override 経路は別に拘束する。
touch "$P/alt_emergency.flag"
OUT="$(printf '%s' "$(sj sess-emgenv)" \
  | STOCKBOT_EMERGENCY_FLAG="$P/alt_emergency.flag" CLAUDE_PROJECT_DIR="$P" bash "$HOOK" 2>/dev/null)"
has "t14b env で指したフラグを見る" "alt_emergency.flag" "$OUT"
rm -f "$P/alt_emergency.flag"

# ---------------------------------------------------------------------------
# 2b. live の emergency_stop フラグ(S8)
# ---------------------------------------------------------------------------
# live の flag は既定値が無く、パスは .env の STOCKBOT_LIVE_EMERGENCY_FLAG にしか無い。
# research の flag だけを見ていた頃は、live が止まっていても「無し」と出ていた。
# .env が無い(mkproj の素の形)ときは「research のみ」と明記する。
run_hook "$P" "$(sj sess-live-noenv)"
has "t14c .env が無ければ live は不明" "emergency_stop(live): 不明" "$OUT"
has "t14d 不明のときは research のみと言う" "research のみ" "$OUT"

# 絶対パス。値(パス)は出力しない — パスに目印を入れて、出力に出ないことを見る。
PL="$(mkproj)"
LIVEDIR="$(mktemp -d "$TMPROOT/liveflag.XXXXXX")"
LIVEABS="$LIVEDIR/LIVEPATHTOKEN.flag"
{ echo "UNRELATED_KEY=SECRETVALUETOKEN"
  echo "STOCKBOT_LIVE_EMERGENCY_FLAG=$LIVEABS"; } > "$PL/.env"
run_hook "$PL" "$(sj sess-live-abs-off)"
has   "t14e flag が無ければ live は無し" "emergency_stop(live): 無し" "$OUT"
touch "$LIVEABS"
run_hook "$PL" "$(sj sess-live-abs-on)"
eq    "t14f live の flag ありでも exit 0" 0 "$RC"
has   "t14g live の flag ありで発動中" "emergency_stop(live): 🛑 発動中" "$OUT"
has   "t14h live の再開経路を案内する" "/api/live/emergency-resume" "$OUT"
hasnt "t14i research を発動中と言わない" "emergency_stop(research): 🛑" "$OUT"
hasnt "t14j flag のパスを出さない" "LIVEPATHTOKEN" "$OUT"
hasnt "t14k .env の他の値を出さない" "SECRETVALUETOKEN" "$OUT"
rm -f "$LIVEABS"

# 同じキーが 2 回あれば後勝ち(make は `. ./.env` で読むので最後の代入が効く)。
touch "$LIVEABS"
{ echo "STOCKBOT_LIVE_EMERGENCY_FLAG=$LIVEABS"
  echo "STOCKBOT_LIVE_EMERGENCY_FLAG=$LIVEDIR/other.flag"; } > "$PL/.env"
run_hook "$PL" "$(sj sess-live-last)"
has "t14l 同じキーは後勝ち" "emergency_stop(live): 無し" "$OUT"
rm -f "$LIVEABS"

# export 付き・二重引用・相対パス。相対は bot の cwd(backend/)基準で解く。
mkdir -p "$PL/backend/runtime"
echo 'export STOCKBOT_LIVE_EMERGENCY_FLAG="runtime/live_emergency.flag"' > "$PL/.env"
touch "$PL/backend/runtime/live_emergency.flag"
run_hook "$PL" "$(sj sess-live-rel)"
has "t14m export・引用・相対(backend 基準)" "emergency_stop(live): 🛑 発動中" "$OUT"
rm -f "$PL/backend/runtime/live_emergency.flag"
run_hook "$PL" "$(sj sess-live-rel-off)"
has "t14n 相対の flag が無ければ無し" "emergency_stop(live): 無し" "$OUT"

# 一重引用 + ~/ と、$HOME/ ・${HOME}/。HOME は使い捨てに差し替える。
LHOME="$(mktemp -d "$TMPROOT/livehome.XXXXXX")"; touch "$LHOME/live.flag"
echo "STOCKBOT_LIVE_EMERGENCY_FLAG='~/live.flag'" > "$PL/.env"
OUT="$(printf '%s' "$(sj sess-live-tilde)" | HOME="$LHOME" CLAUDE_PROJECT_DIR="$PL" bash "$HOOK" 2>/dev/null)"
has "t14o 一重引用 + ~/ を HOME で解く" "emergency_stop(live): 🛑 発動中" "$OUT"
# shellcheck disable=SC2016 # $HOME はここでは展開させない(.env に書かれた字面を作る)
echo 'STOCKBOT_LIVE_EMERGENCY_FLAG=$HOME/live.flag' > "$PL/.env"
OUT="$(printf '%s' "$(sj sess-live-home)" | HOME="$LHOME" CLAUDE_PROJECT_DIR="$PL" bash "$HOOK" 2>/dev/null)"
has "t14p \$HOME/ を HOME で解く" "emergency_stop(live): 🛑 発動中" "$OUT"
# shellcheck disable=SC2016
echo 'STOCKBOT_LIVE_EMERGENCY_FLAG="${HOME}/live.flag"' > "$PL/.env"
OUT="$(printf '%s' "$(sj sess-live-homebrace)" | HOME="$LHOME" CLAUDE_PROJECT_DIR="$PL" bash "$HOOK" 2>/dev/null)"
has "t14q \${HOME}/ を HOME で解く" "emergency_stop(live): 🛑 発動中" "$OUT"

# .env にキーが無ければ、プロセスの env を見る(.env を source したシェルから起動した形)。
echo "OTHER_KEY=1" > "$PL/.env"
touch "$LIVEABS"
OUT="$(printf '%s' "$(sj sess-live-env)" \
  | STOCKBOT_LIVE_EMERGENCY_FLAG="$LIVEABS" CLAUDE_PROJECT_DIR="$PL" bash "$HOOK" 2>/dev/null)"
has "t14r .env に無ければ env を見る" "emergency_stop(live): 🛑 発動中" "$OUT"
rm -f "$LIVEABS"

# ---------------------------------------------------------------------------
# 2c. STATUS.md の now マーカー(H10)
# ---------------------------------------------------------------------------
# mkstatus は STATUS.md を引数の行で書き直す。
mkstatus() { local d="$1"; shift; printf '%s\n' "$@" > "$d/docs/runtime/STATUS.md"; }

PM="$(mkproj)"
mkstatus "$PM" "# STATUS" "PRELINE" "<!-- now:begin -->" "NOWLINE1" "NOWLINE2" "NOWLINE3" \
  "<!-- now:end -->" "POSTLINE"
run_hook "$PM" "$(sj sess-now)"
eq    "t40 マーカーありでも exit 0" 0 "$RC"
has   "t41 節の中身を出す(先頭)" "NOWLINE1" "$OUT"
has   "t42 節の中身を出す(末尾)" "NOWLINE3" "$OUT"
hasnt "t43 節の前を出さない" "PRELINE" "$OUT"
hasnt "t44 節の後を出さない" "POSTLINE" "$OUT"
hasnt "t45 マーカー自体を出さない" "now:begin" "$OUT"
hasnt "t45b 終わりのマーカーも出さない" "now:end" "$OUT"
has   "t46 見出しは現在地の節" "STATUS.md(現在地の節・SSOT)" "$OUT"
hasnt "t46b 節が短ければ切った旨を出さない" "行で切った" "$OUT"

# 終わりのマーカーが無い = 揃っていない → 先頭 25 行へ縮退(節の後ろを全部流さない)。
mkstatus "$PM" "# STATUS" "PRELINE" "<!-- now:begin -->" "NOWLINE1" "POSTLINE"
run_hook "$PM" "$(sj sess-now-noend)"
has "t47 終わりが無ければ先頭へ縮退" "now マーカーが揃っていない" "$OUT"
has "t47b 縮退時は先頭から出す" "PRELINE" "$OUT"

# 始まりが無く終わりだけ。
mkstatus "$PM" "# STATUS" "PRELINE" "NOWLINE1" "<!-- now:end -->" "POSTLINE"
run_hook "$PM" "$(sj sess-now-nobegin)"
has "t48 始まりが無ければ先頭へ縮退" "now マーカーが揃っていない" "$OUT"

# 中身が空 — 黙って空のセクションにしない。
mkstatus "$PM" "# STATUS" "PRELINE" "<!-- now:begin -->" "<!-- now:end -->" "POSTLINE"
run_hook "$PM" "$(sj sess-now-empty)"
has "t49 空の節は先頭へ縮退" "PRELINE" "$OUT"

# 長すぎる節は 25 行で切り、切ったことを言う。合計 40 行の予算も守る。
long=( "# STATUS" "<!-- now:begin -->" )
n=1; while [ "$n" -le 30 ]; do long+=( "LONGLINE$n" ); n=$((n + 1)); done
long+=( "<!-- now:end -->" )
mkstatus "$PM" "${long[@]}"
run_hook "$PM" "$(sj sess-now-long)"
has   "t50 長い節も 25 行目までは出す" "LONGLINE25" "$OUT"
hasnt "t51 26 行目からは出さない" "LONGLINE26" "$OUT"
has   "t52 切ったことを言う" "30 行あるので 25 行で切った" "$OUT"
lines="$(printf '%s\n' "$OUT" | wc -l | tr -d ' ')"
if [ "$lines" -le 40 ]; then ok; else ng "t53 長い節でも 40 行以内" "got $lines lines"; fi

# ---------------------------------------------------------------------------
# 3. session-start-sha(T12 が読む印)
# ---------------------------------------------------------------------------
P2="$(mkproj)"
head_sha="$(git -C "$P2" rev-parse HEAD)"
run_hook "$P2" "$(sj sess-sha)"
eq "t15 sha ファイルの中身" "$head_sha" "$(cat "$P2/runtime/logs/session-start-sha.sess-sha" 2>/dev/null)"
# Stop 側は sha の**有無**では「SessionStart が動いたか」を判定できない(pre-stop-checks.sh が
# 先に同名を作るため)。SessionStart だけが書く別名の印が要る。
# 印は**中身に SHA を持つ**。空ファイルだと、Stop 配列 2 番目の docs-sync-check.sh が
# 先行 hook の backfill を開始点と誤読する(T12 2 巡目レビュー)。
eq "t15b 印は SHA を持つ" "$head_sha" \
   "$(cat "$P2/runtime/logs/session-start-mark.sess-sha" 2>/dev/null)"

# 既にあるなら上書きしない(compact / resume で SessionStart が再発火してもセッション
# 開始点を失わないため)。
echo "deadbeef" > "$P2/runtime/logs/session-start-sha.sess-sha"
run_hook "$P2" "$(sj sess-sha)"
eq "t16 既存 sha を上書きしない" "deadbeef" "$(cat "$P2/runtime/logs/session-start-sha.sess-sha" 2>/dev/null)"

# jq が使えないときの grep フォールバック(pre-stop-checks.sh と同じ経路)。
STUBBIN="$TMPROOT/stubbin"; mkdir -p "$STUBBIN"
printf '#!/bin/sh\nexit 1\n' > "$STUBBIN/jq"; chmod +x "$STUBBIN/jq"
P3="$(mkproj)"
head_sha3="$(git -C "$P3" rev-parse HEAD)"
OUT="$(printf '%s' "$(sj sess-nojq)" | PATH="$STUBBIN:$PATH" CLAUDE_PROJECT_DIR="$P3" bash "$HOOK" 2>/dev/null)"
RC=$?
eq "t17 jq が壊れていても exit 0" 0 "$RC"
eq "t18 grep フォールバックで sid を取る" "$head_sha3" \
   "$(cat "$P3/runtime/logs/session-start-sha.sess-nojq" 2>/dev/null)"

# jq が「存在しない」場合。/usr/bin/jq があるので、hook が実際に使うツールだけを集めた
# PATH を作って jq を外す(/bin と /usr/bin を丸ごと張ると 3 秒かかり make guard に響く)。
FARM="$TMPROOT/farm"; mkdir -p "$FARM"
for t in bash sh git grep sed head cat ls stat cut basename mkdir tr wc lsof date rm; do
  p="$(command -v "$t" 2>/dev/null || true)"
  [ -n "$p" ] && ln -sf "$p" "$FARM/$t" 2>/dev/null
done
if [ -x "$FARM/git" ] && ! PATH="$FARM" command -v jq >/dev/null 2>&1; then
  P4="$(mkproj)"
  head_sha4="$(git -C "$P4" rev-parse HEAD)"
  OUT="$(printf '%s' "$(sj sess-jqgone)" | PATH="$FARM" CLAUDE_PROJECT_DIR="$P4" bash "$HOOK" 2>/dev/null)"
  RC=$?
  eq "t19 jq 不在でも exit 0" 0 "$RC"
  eq "t20 jq 不在でも sid を取る" "$head_sha4" \
     "$(cat "$P4/runtime/logs/session-start-sha.sess-jqgone" 2>/dev/null)"
  # lsof も無い環境(launchd 下で PATH が痩せる形。R1 で実際に踏んだ)。
  rm -f "$FARM/lsof"
  OUT="$(printf '%s' "$(sj sess-nolsof)" | PATH="$FARM" CLAUDE_PROJECT_DIR="$P4" bash "$HOOK" 2>/dev/null)"
  eq  "t20b lsof 不在でも exit 0" 0 "$?"
  has "t20c lsof 不在は不明と言う" "bot: 不明" "$OUT"
else
  skip=$((skip + 4)); echo "SKIP t19/t20/t20b/t20c (jq 除外 PATH を作れなかった)"
fi

# ---------------------------------------------------------------------------
# 4. 壊れた入力 — セッション起動を絶対に止めない
# ---------------------------------------------------------------------------
P5="$(mkproj)"
run_hook "$P5" ""
eq "t21 stdin 空でも exit 0" 0 "$RC"
run_hook "$P5" 'not json at all {{{'
eq "t22 壊れた stdin でも exit 0" 0 "$RC"
run_hook "$P5" '{"session_id":""}'
eq "t23 session_id 空でも exit 0" 0 "$RC"
if ls "$P5/runtime/logs/session-start-sha.nosession-"* >/dev/null 2>&1; then ok
else ng "t24 sid 不明時は nosession-<ppid> に退避" "no such file"; fi

# CLAUDE_PROJECT_DIR 未設定(SessionStart で渡らない環境)でも落ちない。cwd へ縮退する契約
# なので、本物のリポジトリを汚さないよう使い捨てツリーの中で走らせる。
P5b="$(mkproj)"
OUT="$(cd "$P5b" && printf '%s' "$(sj sess-nodir)" | env -u CLAUDE_PROJECT_DIR bash "$HOOK" 2>/dev/null)"
eq "t25 CLAUDE_PROJECT_DIR 未設定でも exit 0" 0 "$?"
has "t25b cwd へ縮退して STATUS を読む" "STATUSLINE1" "$OUT"

# STATUS.md が無い / git リポジトリでない / runtime/logs が書けない。
P6="$(mkproj)"; rm -f "$P6/docs/runtime/STATUS.md"
run_hook "$P6" "$(sj sess-nostatus)"
eq  "t26 STATUS.md 欠落でも exit 0" 0 "$RC"
# 見出し行にも "STATUS.md" は出るので、それでは「黙っていない」ことの証明にならない。
has "t27 STATUS.md 欠落を黙らない" "読めない" "$OUT"

# 「在るが読めない」— `[ -r ]` を `[ -f ]` に変異させると本文も警告も出ない無音セクションに
# なるが、削除ケース(t26/t27)だけでは両者が一致してしまい検出できない。
P6b="$(mkproj)"; chmod 000 "$P6b/docs/runtime/STATUS.md" 2>/dev/null
if [ -r "$P6b/docs/runtime/STATUS.md" ]; then
  skip=$((skip + 2)); echo "SKIP t27b/t27c (root 実行で chmod 000 が効かない)"
else
  run_hook "$P6b" "$(sj sess-unreadable)"
  eq  "t27b 読めない STATUS.md でも exit 0" 0 "$RC"
  has "t27c 読めない STATUS.md を黙らない" "読めない" "$OUT"
fi
chmod 644 "$P6b/docs/runtime/STATUS.md" 2>/dev/null

P7="$(mktemp -d "$TMPROOT/bare.XXXXXX")"
run_hook "$P7" "$(sj sess-nogit)"
eq "t28 git リポでなくても exit 0" 0 "$RC"
# 空の sha ファイルを置くと Stop hook 側が `[ -f ]` だけを見て永久にセッション範囲を
# 失う。書けないなら「置かない」が正しい。
if [ -e "$P7/runtime/logs/session-start-sha.sess-nogit" ]; then
  ng "t29 空の sha ファイルを残さない" "file exists"
else ok; fi

# commit ゼロの git リポ。`git rev-parse HEAD` は rc=128 なのに stdout へ `HEAD` を出すので、
# 非空チェックだけだと印ファイルに文字列 `HEAD` が入り、読み手側で範囲が黙って空になる。
P7b="$(mktemp -d "$TMPROOT/unborn.XXXXXX")"; git -C "$P7b" init -q
run_hook "$P7b" "$(sj sess-unborn)"
eq "t29b commit ゼロでも exit 0" 0 "$RC"
if [ -e "$P7b/runtime/logs/session-start-sha.sess-unborn" ]; then
  ng "t29c commit ゼロで印を置かない" "content=[$(cat "$P7b/runtime/logs/session-start-sha.sess-unborn")]"
else ok; fi
# sha を置けなかったのに mark だけ置くと、Stop 側が「開始点が分かっている」と誤読して
# 直前コミットを検査から落とす(fail-OPEN)。
if [ -e "$P7b/runtime/logs/session-start-mark.sess-unborn" ]; then
  ng "t29d sha 無しで mark を置かない" "mark exists"
else ok; fi

# `backend/data` が無いときの `${HOME:-}/.stockbot/data` フォールバック。ここを消しても
# backend/data 側のケースは緑のままなので、別に拘束する(CLAUDE.md が言う「実体」の経路)。
P8c="$(mkproj)"; rm -rf "$P8c/backend/data"
FAKEHOME="$(mktemp -d "$TMPROOT/home.XXXXXX")"; mkdir -p "$FAKEHOME/.stockbot/data"
echo "date,close" > "$FAKEHOME/.stockbot/data/7203_daily.csv"
OUT="$(printf '%s' "$(sj sess-homedata)" \
  | HOME="$FAKEHOME" CLAUDE_PROJECT_DIR="$P8c" bash "$HOOK" 2>/dev/null)"
has "t30e ~/.stockbot/data へ縮退する" "data: 最新 7203_daily.csv" "$OUT"

# CSV が 1 本も無いとき。ここを `:` に変異させても全緑だと、STATUS.md 側で塞いだのと
# 同型の**無音セクション**が 7 行下に残る。
P8d="$(mkproj)"; rm -f "$P8d"/backend/data/*_daily.csv
OUT="$(printf '%s' "$(sj sess-nocsv)" \
  | HOME="$TMPROOT/nonexistent-home" CLAUDE_PROJECT_DIR="$P8d" bash "$HOOK" 2>/dev/null)"
has "t30f CSV 皆無を黙らない" "見つからない" "$OUT"

P8="$(mkproj)"; mkdir -p "$P8/runtime"; : > "$P8/runtime/logs"
run_hook "$P8" "$(sj sess-nolog)"
eq "t30 runtime/logs が作れなくても exit 0" 0 "$RC"
# リダイレクト自体の失敗は `> f 2>/dev/null` では抑えられず素の stderr に出る。
# SessionStart の stderr はオーナーの画面に出るので、静かに失敗すること自体が契約。
err="$(printf '%s' "$(sj sess-nolog)" | CLAUDE_PROJECT_DIR="$P8" bash "$HOOK" 2>&1 >/dev/null)"
if [ -z "$err" ]; then ok; else ng "t30b stderr を汚さない" "stderr=[$err]"; fi

# HOME 未設定(裸の $HOME を `set -u` が撃つ形。出力が途中で切れて exit 1 になる)。
P8b="$(mkproj)"
OUT="$(printf '%s' "$(sj sess-nohome)" | env -u HOME CLAUDE_PROJECT_DIR="$P8b" bash "$HOOK" 2>/dev/null)"
eq  "t30c HOME 未設定でも exit 0" 0 "$?"
has "t30d HOME 未設定でも最後まで出す" "commit four" "$OUT"

# ---------------------------------------------------------------------------
# 5. 構造の不変条件(md §3 H1 の制約)
# ---------------------------------------------------------------------------
src="$(cat "$HOOK" 2>/dev/null)"
has   "t31 set -uo pipefail"      "set -uo pipefail" "$src"
hasnt "t32 set -e を付けない"       "set -e"           "$src"
# ネットワーク・立花 API・重い更新チェックを一切呼ばない(総実行 1 秒未満の前提)。
# コメントは落としてから見る — 「なぜ呼ばないか」を書いた why コメント(C1 で残すべきもの)を
# 実行と取り違えると、正しい記録を消す方向に圧力がかかる。
code="$(printf '%s\n' "$src" | sed -e 's/^[[:space:]]*#.*$//' -e 's/[[:space:]]#[[:space:]].*$//')"
for forbidden in curl wget fetch-daily nc\  ping ssh; do
  if printf '%s' "$code" | grep -qE "(^|[^A-Za-z0-9_-])${forbidden}"; then
    ng "t33 ネットワークを叩かない($forbidden)" "found in code"
  else ok; fi
done
# 上の sed が効きすぎて全部空になっていないこと(検査が無音で消える形)。
has "t33b コメント除去後もコードが残る" "lsof" "$code"

# 1 秒未満(実測は環境依存なので余裕を持って 3 秒で頭打ちにする)。
P9="$(mkproj)"
t0=$(date +%s)
run_hook "$P9" "$(sj sess-time)"
t1=$(date +%s)
if [ $((t1 - t0)) -le 3 ]; then ok; else ng "t34 実行が遅すぎる" "$((t1 - t0))s"; fi


# ---------------------------------------------------------------------------
# RF3 #7: runtime/logs の GC(30 日超・prefix 限定)
# ---------------------------------------------------------------------------
# 棚卸し時点で 208 ファイル / 最古 2026-06-18 が GC 無しで積んでいた。
# 🛑 消してよいのは hook が作る印だけ。**prefix を限定する**(ログ本体や人間が置いた
# ファイルを巻き込まない)。
# 🛑 **生存中のセッションの印は消さない** — `docs-sync-seen` / `tdd-check-seen` /
# `*-block-count` は「この検査がこのセッションで既に走ったか」の印で、消すと
# `session_first_stop=1` に戻って `HEAD~..HEAD` が範囲に復活する。30 日閾値なら
# 現実には踏まないが、当該セッション ID は明示的に除外する。
PGC="$(mkproj)"
mkdir -p "$PGC/runtime/logs"
OLD=200001010000   # 十分に古い固定値(date -v / date -d の方言差を避ける)

# 消えるべきもの(GC 対象 prefix × 古い)
for f in session-start-sha.dead session-start-mark.dead enforcement-ack.dead.abc \
         pre-stop-block-count.dead docs-sync-block-count.dead \
         docs-sync-seen.dead tdd-check-seen.dead; do
  : > "$PGC/runtime/logs/$f"; touch -t "$OLD" "$PGC/runtime/logs/$f"
done
# 残るべきもの
: > "$PGC/runtime/logs/session-start-sha.fresh"                       # 新しい
: > "$PGC/runtime/logs/pre-stop-checks.log"; touch -t "$OLD" "$PGC/runtime/logs/pre-stop-checks.log"
: > "$PGC/runtime/logs/docs-sync-check.log"; touch -t "$OLD" "$PGC/runtime/logs/docs-sync-check.log"
: > "$PGC/runtime/logs/human-note.txt";      touch -t "$OLD" "$PGC/runtime/logs/human-note.txt"
# 当該セッションの印(古くても消さない)
: > "$PGC/runtime/logs/docs-sync-seen.sess-gc"; touch -t "$OLD" "$PGC/runtime/logs/docs-sync-seen.sess-gc"

run_hook "$PGC" "$(sj sess-gc)"
eq "t35 GC があっても exit 0" 0 "$RC"

gone=0
for f in session-start-sha.dead session-start-mark.dead enforcement-ack.dead.abc \
         pre-stop-block-count.dead docs-sync-block-count.dead \
         docs-sync-seen.dead tdd-check-seen.dead; do
  [ -e "$PGC/runtime/logs/$f" ] && gone=$((gone + 1))
done
eq "t36 30 日超の印は全 prefix が消える" 0 "$gone"

kept=0
for f in session-start-sha.fresh pre-stop-checks.log docs-sync-check.log human-note.txt; do
  [ -e "$PGC/runtime/logs/$f" ] || kept=$((kept + 1))
done
eq "t37 新しい印 / ログ / 無関係ファイルは消さない" 0 "$kept"

if [ -e "$PGC/runtime/logs/docs-sync-seen.sess-gc" ]; then ok
else ng "t38 当該セッションの印を消さない" "消えている"; fi

echo "pass=$pass fail=$fail skip=$skip"
[ "$fail" -eq 0 ]
