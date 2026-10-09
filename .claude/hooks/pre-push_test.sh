#!/usr/bin/env bash
# pre-push_test.sh — .githooks/pre-push の配線の自己テスト(H5・D-10)。全 pass で exit 0。
#
# 判定そのもの(範囲・記録)は pre-push-scope_test.sh が見る。ここは pre-push が
#   - 毎回の検査を必ず踏む / 判定に従って hook の自己テストを回す・省略する
#   - 判定できないときは全件に倒す / どれかが赤なら push を止める
# ことを、make の各ターゲットを stub にした一時リポで確かめる(実物の make check は重すぎる)。
set -uo pipefail
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_COMMON_DIR
unset STOCKBOT_PREPUSH_FULL

HOOKDIR="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HOOKDIR/../.." && pwd)"
pass=0; fail=0
ok() { pass=$((pass + 1)); }
ng() { fail=$((fail + 1)); echo "FAIL [$1] $2"; }
has()   { if printf '%s' "$3" | grep -qF -- "$2"; then ok; else ng "$1" "missing [$2]"; fi; }
hasnt() { if printf '%s' "$3" | grep -qF -- "$2"; then ng "$1" "unexpected [$2]"; else ok; fi; }

TMPROOT="$(mktemp -d)"
trap 'rm -rf "$TMPROOT"' EXIT

# mkrepo — pre-push と判定の lib は実物、secret-scan と make の各ターゲットは stub。
mkrepo() {
    local d
    d="$(mktemp -d "$TMPROOT/repo.XXXXXX")"
    mkdir -p "$d/.githooks" "$d/.claude/hooks" "$d/scripts" "$d/docs"
    cp "$ROOT/.githooks/pre-push" "$d/.githooks/pre-push"
    cp "$HOOKDIR/enforcement-paths.sh" "$HOOKDIR/pre-push-scope.sh" "$d/.claude/hooks/"
    printf '#!/usr/bin/env bash\necho "RAN secret-scan"\nexit "${FAKE_SECRET_RC:-0}"\n' > "$d/scripts/secret-scan.sh"
    {
        printf 'vet-integration check-backend fmt-check guard-fast:\n\t@echo "RAN $@"\n'
        printf 'guard-hooks:\n\t@echo "RAN guard-hooks"\n\t@test "$${FAKE_GUARD_HOOKS_RC:-0}" = 0\n'
        printf '\t@mkdir -p runtime/logs && date +%%s > runtime/logs/pre-push-selftest-ok\n'
    } > "$d/Makefile"
    git init -q "$d"
    git -C "$d" -c user.email=t@t -c user.name=t add -A
    git -C "$d" -c user.email=t@t -c user.name=t commit -q -m base
    git -C "$d" update-ref refs/remotes/origin/main HEAD
    printf '%s' "$d"
}
# commit_in <repo> <path> — 1 ファイル変えて commit し、SHA を返す。
commit_in() {
    mkdir -p "$1/$(dirname "$2")"
    echo "$RANDOM" >> "$1/$2"
    git -C "$1" -c user.email=t@t -c user.name=t add -A
    git -C "$1" -c user.email=t@t -c user.name=t commit -q -m "touch $2"
    git -C "$1" rev-parse HEAD
}
fresh_stamp() { mkdir -p "$1/runtime/logs"; date +%s > "$1/runtime/logs/pre-push-selftest-ok"; }
# run_pp <repo> <stdin> [env...] — OUT / RC をグローバルに置く。
run_pp() {
    local d="$1" in="$2"; shift 2
    OUT="$(cd "$d" && printf '%s\n' "$in" | env "$@" bash .githooks/pre-push 2>&1)"
    RC=$?
}
line() { printf 'refs/heads/main %s refs/heads/main %s' "$2" "$1"; }   # line <remote> <local>

# 1. enforcement の変更なし + 記録が新しい → 毎回の検査だけ、hook の自己テストは省略
P="$(mkrepo)"; BASE="$(git -C "$P" rev-parse HEAD)"; fresh_stamp "$P"
DOCS="$(commit_in "$P" docs/a.md)"
run_pp "$P" "$(line "$BASE" "$DOCS")"
has   "p1 rc=0"                       "pre-push: OK" "$OUT"
[ "$RC" -eq 0 ] && ok || ng "p1 rc" "rc=$RC"
has   "p1 secret-scan は毎回"         "RAN secret-scan" "$OUT"
has   "p1 vet-integration は毎回"     "RAN vet-integration" "$OUT"
has   "p1 check-backend は毎回"       "RAN check-backend" "$OUT"
has   "p1 fmt-check は毎回"           "RAN fmt-check" "$OUT"
has   "p1 guard-fast は毎回"          "RAN guard-fast" "$OUT"
hasnt "p1 hook の自己テストは省略"    "RAN guard-hooks" "$OUT"
has   "p1 省略したと言う"             "自己テストは省略" "$OUT"

# 2. hook の変更 → 全件
H="$(commit_in "$P" .claude/hooks/x.sh)"
run_pp "$P" "$(line "$DOCS" "$H")"
has "p2 hook の変更で全件"            "RAN guard-hooks" "$OUT"
has "p2 理由を言う"                   ".claude/hooks/x.sh" "$OUT"

# 3. stdin が空(git から行が渡らない)→ 全件
run_pp "$P" ""
has "p3 範囲が空なら全件"             "RAN guard-hooks" "$OUT"

# 4. 記録が無い → 全件 → 緑なら記録が書かれる
rm -f "$P/runtime/logs/pre-push-selftest-ok"
run_pp "$P" "$(line "$BASE" "$DOCS")"
has "p4 記録が無ければ全件"           "RAN guard-hooks" "$OUT"
if [ -s "$P/runtime/logs/pre-push-selftest-ok" ]; then ok; else ng "p4 全件が緑なら記録する" "no stamp"; fi

# 5. 判定の lib が読めない → 全件
P5="$(mkrepo)"; B5="$(git -C "$P5" rev-parse HEAD)"; fresh_stamp "$P5"
D5="$(commit_in "$P5" docs/a.md)"
rm -f "$P5/.claude/hooks/pre-push-scope.sh"
run_pp "$P5" "$(line "$B5" "$D5")"
has "p5 lib が無ければ全件"           "RAN guard-hooks" "$OUT"
has "p5 理由を言う"                   "読めない" "$OUT"

# 6. env で全件を強制
run_pp "$P" "$(line "$BASE" "$DOCS")" STOCKBOT_PREPUSH_FULL=1
has "p6 env で全件"                   "RAN guard-hooks" "$OUT"

# 7. hook の自己テストが赤 → push を止める・記録しない
rm -f "$P/runtime/logs/pre-push-selftest-ok"
run_pp "$P" "$(line "$DOCS" "$H")" FAKE_GUARD_HOOKS_RC=1
[ "$RC" -ne 0 ] && ok || ng "p7 自己テストが赤なら止める" "rc=$RC"
hasnt "p7 OK と言わない"              "pre-push: OK" "$OUT"
if [ -e "$P/runtime/logs/pre-push-selftest-ok" ]; then ng "p7 赤なら記録しない" "stamp exists"; else ok; fi

# 8. secret-scan が赤 → そこで止まる
run_pp "$P" "$(line "$BASE" "$DOCS")" FAKE_SECRET_RC=1
[ "$RC" -ne 0 ] && ok || ng "p8 secret-scan が赤なら止める" "rc=$RC"
hasnt "p8 後続を走らせない"           "RAN vet-integration" "$OUT"

# 9. 実物の Makefile: 自己テストは全部 guard-fast か guard-hooks で回り、記録は最後に書く。
# 置き場所から漏れた自己テストはどこからも走らない(CI 廃止のときに 1 本そうなった)。
# 記録を先に書くと、後ろのテストが赤でも「前回の全件は緑」になる。
recipes() { # $1 = ターゲット名。その recipe 行(タブ始まり)を出す。
    awk -v t="$1" '$0 ~ "^"t":" { on = 1; next } on && /^\t/ { print; next } on { exit }' "$ROOT/Makefile"
}
FAST="$(recipes guard-fast)"; HOOKS="$(recipes guard-hooks)"
for t in "$ROOT"/.claude/hooks/*_test.sh "$ROOT"/scripts/*_test.sh; do
    [ -e "$t" ] || continue
    rel="${t#"$ROOT"/}"
    if printf '%s\n%s\n' "$FAST" "$HOOKS" | grep -qF "bash $rel"; then ok
    else ng "p9 $rel が guard-fast / guard-hooks に無い" "Makefile"; fi
done
last_hooks="$(printf '%s\n' "$HOOKS" | tail -1)"
has "p9 guard-hooks の最後で記録する" "runtime/logs/pre-push-selftest-ok" "$last_hooks"
n_stamp="$(printf '%s\n' "$HOOKS" | grep -c 'pre-push-selftest-ok')"
[ "$n_stamp" -eq 1 ] && ok || ng "p9 記録は最後の 1 か所だけ" "count=$n_stamp"
# 依存は `##` の説明を落としてから見る(説明文の「guard」に当たらないように)。
deps() { grep -E "^$1:" "$ROOT/Makefile" | head -1 | sed -e 's/##.*//' -e "s/^$1://"; }
case " $(deps guard) " in *" guard-fast "*) ok ;; *) ng "p9 guard は guard-fast を回す" "$(deps guard)" ;; esac
case " $(deps guard) " in *" guard-hooks "*) ok ;; *) ng "p9 guard は guard-hooks を回す" "$(deps guard)" ;; esac
case " $(deps check) " in *" guard "*) ok ;; *) ng "p9 check は guard を含む" "$(deps check)" ;; esac
# check-backend の build は実行ファイルを bin/ に出す(backend 直下に落ちて残っていた。B14)。
has "p9 build は bin/ に出す" "build -o ../bin/ ./..." "$(recipes build)"

echo "pre-push_test: pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
