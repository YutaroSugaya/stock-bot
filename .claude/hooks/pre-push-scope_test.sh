#!/usr/bin/env bash
# pre-push-scope_test.sh — pre-push-scope.sh(H5・D-10)の自己テスト。全 pass で exit 0。
#
# 守っているのは「省略してよいときだけ省略する」こと。誤って省略を返すと、hook を壊した push が
# 自己テストを通らずに入る(fail-open)。判定できない形はすべて全件(rc=0)を要求する。
set -uo pipefail

# pre-push から make 経由で走るので、git が渡す GIT_DIR を落とす(下の一時リポが実リポに向かう)。
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_COMMON_DIR
unset STOCKBOT_PREPUSH_FULL

HOOKDIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=enforcement-paths.sh
. "$HOOKDIR/enforcement-paths.sh"
# shellcheck source=pre-push-scope.sh
. "$HOOKDIR/pre-push-scope.sh"

pass=0; fail=0
ok() { pass=$((pass + 1)); }
ng() { fail=$((fail + 1)); echo "FAIL [$1] $2"; }

TMPROOT="$(mktemp -d)"
trap 'rm -rf "$TMPROOT"' EXIT
Z=0000000000000000000000000000000000000000
NOW=2000000000
DAY=86400
STAMP="$TMPROOT/stamp"

R="$TMPROOT/repo"
git init -q "$R"
g() { git -C "$R" -c user.email=t@t -c user.name=t "$@"; }
# commit_file <path> — ファイルを書いて 1 コミット作り、SHA を返す。
commit_file() {
    mkdir -p "$R/$(dirname "$1")"
    echo "$RANDOM" >> "$R/$1"
    g add -A >/dev/null
    g commit -q -m "touch $1"
    g rev-parse HEAD
}
BASE="$(commit_file docs/README.md)"
# リモートにある位置を作る(新しいブランチの判定は --not --remotes を見る)。
g update-ref refs/remotes/origin/main "$BASE"

# expect <desc> <want_rc> <want_substr> <stdin>。カレントを $R にして判定を呼ぶ。
expect() {
    local out rc
    out="$(cd "$R" && printf '%s\n' "$4" | prepush_full_reason "$STAMP" "$NOW" "$PREPUSH_STAMP_TTL")"
    rc=$?
    if [ "$rc" != "$2" ]; then ng "$1" "rc=$rc want $2 (out=$out)"; return; fi
    if [ -n "$3" ] && ! printf '%s' "$out" | grep -qF -- "$3"; then ng "$1" "missing [$3] in [$out]"; return; fi
    ok
}
line() { printf 'refs/heads/main %s refs/heads/main %s' "$2" "$1"; }   # line <remote> <local>

echo "$NOW" > "$STAMP"   # 記録は新しい = 範囲だけで決まる状態から始める

# --- 範囲に enforcement の変更が無い → 省略 ---------------------------------------------
DOCS="$(commit_file docs/x.md)"
expect "docs だけなら省略"            1 "変更なし" "$(line "$BASE" "$DOCS")"
GO="$(commit_file backend/internal/x.go)"
expect "Go だけなら省略"              1 "変更なし" "$(line "$DOCS" "$GO")"
SKILL="$(commit_file .claude/skills/x/SKILL.md)"
expect "skills は enforcement の外"   1 "変更なし" "$(line "$GO" "$SKILL")"
RT="$(commit_file scripts/stockbot-routine.sh)"
expect "scripts の本体は enforcement の外" 1 "変更なし" "$(line "$SKILL" "$RT")"

# --- enforcement の変更 → 全件 ---------------------------------------------------------
H="$(commit_file .claude/hooks/x.sh)"
expect "hooks の変更"                 0 ".claude/hooks/x.sh" "$(line "$RT" "$H")"
S="$(commit_file .claude/settings.json)"
expect "settings の変更"              0 ".claude/settings.json" "$(line "$H" "$S")"
GH="$(commit_file .githooks/pre-push)"
expect "githooks の変更"              0 ".githooks/pre-push" "$(line "$S" "$GH")"
MK="$(commit_file Makefile)"
expect "Makefile の変更"              0 "Makefile" "$(line "$GH" "$MK")"
AG="$(commit_file scripts/arch-guard.sh)"
expect "guard scripts の変更"         0 "scripts/arch-guard.sh" "$(line "$MK" "$AG")"
ST="$(commit_file scripts/foo_test.sh)"
expect "scripts の自己テストの変更"   0 "scripts/foo_test.sh" "$(line "$AG" "$ST")"
# 範囲の先頭ではなく途中のコミットにあっても拾う。
D2="$(commit_file docs/y.md)"
expect "途中のコミットの変更も拾う"   0 "scripts/foo_test.sh" "$(line "$AG" "$D2")"
# 削除も変更。
g rm -q .claude/hooks/x.sh; g commit -q -m "rm hook"; DEL="$(g rev-parse HEAD)"
expect "hook の削除"                  0 ".claude/hooks/x.sh" "$(line "$D2" "$DEL")"
# 外へ移す rename。rename 検出が効くと移し先(docs/)だけになり範囲から消える。
H2="$(commit_file .claude/hooks/y.sh)"
g mv .claude/hooks/y.sh docs/y.sh; g commit -q -m "move hook out"; MV="$(g rev-parse HEAD)"
expect "hook を外へ移す rename"       0 ".claude/hooks/y.sh" "$(line "$H2" "$MV")"
# 複数行: 1 行目は docs だけ、2 行目に enforcement。
expect "2 行目の範囲も見る"           0 "Makefile" "$(line "$BASE" "$DOCS")
$(line "$GH" "$MK")"

# --- 新しいブランチ(remote が 0 埋め)→ リモートに無いコミットだけを見る ----------------
g checkout -q -b side "$BASE"
NB_DOCS="$(commit_file docs/z.md)"
expect "新ブランチ docs だけなら省略" 1 "変更なし" "refs/heads/side $NB_DOCS refs/heads/side $Z"
NB_H="$(commit_file .claude/hooks/z.sh)"
expect "新ブランチの hook の変更"     0 ".claude/hooks/z.sh" "refs/heads/side $NB_H refs/heads/side $Z"
# 新ブランチ側でも rename を分けて見る。hook をリモートに載せてから、外へ移す。
g update-ref refs/remotes/origin/side "$NB_H"
g mv .claude/hooks/z.sh docs/z.sh; g commit -q -m "move hook out"; NB_MV="$(g rev-parse HEAD)"
expect "新ブランチの rename も分ける" 0 ".claude/hooks/z.sh" "refs/heads/side2 $NB_MV refs/heads/side2 $Z"
expect "新ブランチの解けない SHA"     0 "範囲を解けない" "refs/heads/side3 2222222222222222222222222222222222222222 refs/heads/side3 $Z"
g checkout -q main 2>/dev/null || g checkout -q master

# --- 判定できない → 全件 --------------------------------------------------------------
expect "stdin が空"                   0 "範囲が空" ""
expect "削除の push だけ"             0 "範囲が空" "(delete) $Z refs/heads/old $DOCS"
expect "remote の SHA が欠けた行"     0 "行が読めない" "refs/heads/main $DOCS"
expect "解けない remote SHA"          0 "範囲を解けない" "$(line 1111111111111111111111111111111111111111 "$DOCS")"
OUT="$(cd "$R" && printf '%s\n' "$(line "$BASE" "$DOCS")" | STOCKBOT_PREPUSH_FULL=1 prepush_full_reason "$STAMP" "$NOW" "$PREPUSH_STAMP_TTL")"
if [ $? -eq 0 ] && printf '%s' "$OUT" | grep -qF "STOCKBOT_PREPUSH_FULL=1"; then ok; else ng "env で全件を強制" "out=$OUT"; fi
OUT="$(cd "$R" && printf '%s\n' "$(line "$BASE" "$DOCS")" | ENF_PATH_RE='' prepush_full_reason "$STAMP" "$NOW" "$PREPUSH_STAMP_TTL")"
if [ $? -eq 0 ] && printf '%s' "$OUT" | grep -qF "読めない"; then ok; else ng "集合が空なら全件" "out=$OUT"; fi
OUT="$(cd "$R" && printf '%s\n' "$(line "$BASE" "$DOCS")" | ENF_TEST_PATH_RE='' prepush_full_reason "$STAMP" "$NOW" "$PREPUSH_STAMP_TTL")"
if [ $? -eq 0 ] && printf '%s' "$OUT" | grep -qF "読めない"; then ok; else ng "テストの集合が空なら全件" "out=$OUT"; fi

# --- 週 1 回の全件(記録) ---------------------------------------------------------------
DOCSLINE="$(line "$BASE" "$DOCS")"
rm -f "$STAMP"
expect "記録が無ければ全件"           0 "記録が無い" "$DOCSLINE"
echo "garbage" > "$STAMP"
expect "記録が壊れていれば全件"       0 "記録が無い" "$DOCSLINE"
: > "$STAMP"
expect "記録が空なら全件"             0 "記録が無い" "$DOCSLINE"
echo $((NOW + 60)) > "$STAMP"
expect "記録が未来なら全件"           0 "未来" "$DOCSLINE"
echo $((NOW - 7 * DAY)) > "$STAMP"
expect "ちょうど 7 日で全件"          0 "7 日" "$DOCSLINE"
echo $((NOW - 7 * DAY + 1)) > "$STAMP"
expect "7 日未満なら省略"             1 "6 日" "$DOCSLINE"
echo "$NOW" > "$STAMP"
expect "記録が今なら省略"             1 "0 日" "$DOCSLINE"

echo "pre-push-scope_test: pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
