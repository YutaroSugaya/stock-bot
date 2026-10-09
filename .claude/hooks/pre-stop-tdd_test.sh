#!/usr/bin/env bash
# A2(テストの存在の機械強制)の自己テスト。2 部構成。
#
#   Part 1 = 判定関数 tdd_violations() の単体テスト。tdd-check-lib.sh を source して
#            「変更ファイル一覧 + diff」を引数で渡す(git 状態に依存しない)。
#   Part 2 = pre-stop-checks.sh を使い捨て git リポジトリで実際に流す e2e。
#            Part 1 は「何を渡されたらどう判定するか」しか見ないので、
#            **git から何を拾うか**(作業ツリー / 直近コミット / session 範囲 / session
#            が不明なときの縮退)はここでしか検出できない。A1 のレビューで
#            「修正は正しいがテストが別経路を見ている」が 4 巡連続で再発したので、
#            片方だけにしない。
#
# 🧊 **凍結(2026-10-01・REVIEW_TASKS 決定 D-11)。新しいケースを足さない。**
# 守っているのは「Go の変更にテストが存在するか」の検査だけなのに、ファイルが最も大きく
# guard-hooks で最も時間のかかる 1 本になった(行数は `wc -l` で測る)。
#   - 既存ケースの修正と、対応するコードを消したときのケースの削除はしてよい。
#   - pre-stop-checks.sh の別の Step や lib の回帰を固定したいときは、その Step / lib 用の
#     小さな自己テストを別ファイルに作る(例: pre-stop の既定 on は scripts/secret-scan_test.sh、
#     pre-push の判定は pre-push-scope_test.sh)。
#   - 新しいファイルも make の guard-fast / guard-hooks のどちらかに足す(pre-push_test が見る)。
set -uo pipefail

HOOKDIR="$(cd "$(dirname "$0")" && pwd)"
LIB="$HOOKDIR/tdd-check-lib.sh"
PATLIB="$HOOKDIR/secret-patterns.sh"   # RF3 #4: secret パターンの正本(hook が source する)
STOPLIB="$HOOKDIR/stop-hook-lib.sh"    # RF3 #5: Stop 系 2 本が共有する session スコープ解決
ENFLIB="$HOOKDIR/enforcement-paths.sh" # RF3 #6: enforcement 対象パスの正本
HOOK="$HOOKDIR/pre-stop-checks.sh"
TMPROOT="$(mktemp -d)"
trap 'rm -rf "$TMPROOT"' EXIT

# ケースの並列実行と集計は共有ハーネス(RF3 #8)。無いまま走ると全ケースが黙って
# 消えて「pass=0 fail=0」の嘘の緑になるので fail-CLOSED にする。
# shellcheck source=/dev/null
. "$HOOKDIR/e2e-harness.sh" 2>/dev/null || true
command -v h_defer >/dev/null 2>&1 || { echo "e2e-harness.sh が読めない" >&2; exit 1; }
h_init "$TMPROOT"

if [ ! -f "$LIB" ]; then
    echo "FAIL: $LIB が存在しない"
    echo "pre-stop-tdd_test: pass=0 fail=1"
    exit 1
fi
# shellcheck source=/dev/null
. "$LIB"

# Part 1: tdd_violations() の単体テスト
# mktree の tree は空白区切り。**空白を含むパスは改行区切り**で書く(改行が 1 つでもあれば改行区切り)。
# ⚠ **改行分岐を拘束しているのは t34 の 1 ケースだけ**。t34 を消す/書き換えるなら代わりを必ず残す。
#
# 🛑 **この case を `$( )` の中に書かない。**bash 3.2 は command substitution の中の case を
# 解析できず(パターンの `)` を閉じ括弧と誤読する。bash 4 で修正済み)、macOS 実機の /bin/bash は
# 3.2 なので unit/* が 28 件まとめて落ちていた。`bash -n` は `$( )` の中身を解析しないので
# 構文チェックでは気づけない。関数に出しておけば、呼ぶ側は case を含まない。
mktree() {
    local tree="$1" f
    case "$tree" in
        *"
"*) while IFS= read -r f; do [ -z "$f" ] && continue
                mkdir -p "$(dirname "$f")"; : > "$f"
            done <<< "$tree" ;;
        *)  for f in $tree; do mkdir -p "$(dirname "$f")"; : > "$f"; done ;;
    esac
}

unit() {
    local id="$1" want="$2" desc="$3" tree="$4" added="$5" changed="$6" diff="$7"
    local dir="$TMPROOT/u_$id" out got
    mkdir -p "$dir"
    out="$( cd "$dir" || exit 1
            mktree "$tree"
            tdd_violations "$added" "$changed" "$diff" )"
    if [ -n "$out" ]; then got=block; else got=pass; fi
    check "unit/$id $desc" "$want" "$got"
    UNIT_OUT="$out"
}

# tdd_is_exempt は tdd_violations 越しには全分岐を観測できない(*_test.go は
# 「同じディレクトリにテストがある」「テストを触っている」のどちらかで先に落ちるため、
# 除外を外しても結果が変わらない = mutation testing で survived になった)。直接見る。
exempt() { # $1 = path, $2 = yes|no
    local got=no
    tdd_is_exempt "$1" && got=yes
    check "unit/exempt $1" "$2" "$got"
}
exempt backend/internal/domain/foo/foo_test.go   yes
exempt backend/cmd/stockbot/main.go              yes
exempt backend/internal/testutil/testutil.go     yes
exempt backend/internal/domain/foo/kind_string.go yes
exempt backend/internal/domain/foo/api.pb.go     yes
exempt backend/internal/domain/foo/testdata/sample.go yes
exempt backend/internal/domain/foo/foo.go        no
exempt backend/internal/adapter/repository/pg.go no
exempt scripts/x.go                              yes
exempt docs/notes.md                             yes

# 既存ファイルへの `+func NewX` を含む最小の unified diff。
diff_new_exported() { # $1 = path, $2 = 追加する行
    printf 'diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -1,0 +2,1 @@\n+%s\n' "$1" "$1" "$1" "$1" "$2"
}

unit t1_block_new_no_test block "新規 .go / 同一パッケージに *_test.go 無し" \
    "backend/internal/domain/foo/foo.go" \
    "backend/internal/domain/foo/foo.go" \
    "backend/internal/domain/foo/foo.go" \
    ""
expect_msg "unit/t1 メッセージに期待 _test.go パスがある" \
    "backend/internal/domain/foo/foo_test.go" "$UNIT_OUT"

unit t2_pass_new_with_test pass "新規 .go + 同名 _test.go" \
    "backend/internal/domain/foo/foo.go backend/internal/domain/foo/foo_test.go" \
    "backend/internal/domain/foo/foo.go
backend/internal/domain/foo/foo_test.go" \
    "backend/internal/domain/foo/foo.go
backend/internal/domain/foo/foo_test.go" \
    ""

unit t3_pass_new_pkg_has_other_test pass "新規 .go / 同一パッケージに別名の _test.go" \
    "backend/internal/domain/foo/foo.go backend/internal/domain/foo/bar_test.go" \
    "backend/internal/domain/foo/foo.go" \
    "backend/internal/domain/foo/foo.go" \
    ""

unit t4_block_exported_no_test_change block "既存ファイルに exported 追加 / _test.go 変更なし" \
    "backend/internal/usecase/command/x.go backend/internal/usecase/command/x_test.go" \
    "" \
    "backend/internal/usecase/command/x.go" \
    "$(diff_new_exported backend/internal/usecase/command/x.go 'func NewX() int { return 1 }')"

unit t5_pass_exported_with_test_change pass "同上 + _test.go も変更" \
    "backend/internal/usecase/command/x.go backend/internal/usecase/command/x_test.go" \
    "" \
    "backend/internal/usecase/command/x.go
backend/internal/usecase/command/x_test.go" \
    "$(diff_new_exported backend/internal/usecase/command/x.go 'func NewX() int { return 1 }')"

unit t6_pass_cmd_only pass "backend/cmd/** の変更のみ(除外)" \
    "backend/cmd/stockbot/main.go" \
    "backend/cmd/stockbot/main.go" \
    "backend/cmd/stockbot/main.go" \
    "$(diff_new_exported backend/cmd/stockbot/main.go 'func Run() int { return 1 }')"

unit t7_pass_test_only pass "*_test.go のみの変更" \
    "backend/internal/domain/foo/foo.go backend/internal/domain/foo/foo_test.go" \
    "" \
    "backend/internal/domain/foo/foo_test.go" \
    "$(diff_new_exported backend/internal/domain/foo/foo_test.go 'func TestX(t *testing.T) {}')"

unit t8_pass_unexported pass "unexported の追加は Tier 2 に当たらない" \
    "backend/internal/domain/foo/foo.go backend/internal/domain/foo/foo_test.go" \
    "" \
    "backend/internal/domain/foo/foo.go" \
    "$(diff_new_exported backend/internal/domain/foo/foo.go 'func newX() int { return 1 }')"

unit t9_block_exported_method block "exported メソッドの追加" \
    "backend/internal/domain/foo/foo.go backend/internal/domain/foo/foo_test.go" \
    "" \
    "backend/internal/domain/foo/foo.go" \
    "$(diff_new_exported backend/internal/domain/foo/foo.go 'func (s *Svc) DoIt() error { return nil }')"

unit t10_pass_removed_exported pass "exported の削除だけ(- 行)は当たらない" \
    "backend/internal/domain/foo/foo.go backend/internal/domain/foo/foo_test.go" \
    "" \
    "backend/internal/domain/foo/foo.go" \
    "$(printf 'diff --git a/backend/internal/domain/foo/foo.go b/backend/internal/domain/foo/foo.go\n--- a/backend/internal/domain/foo/foo.go\n+++ b/backend/internal/domain/foo/foo.go\n@@ -1,1 +1,0 @@\n-func Old() int { return 1 }\n')"

unit t11_pass_comment_only pass "コメント / 本体だけの変更" \
    "backend/internal/domain/foo/foo.go backend/internal/domain/foo/foo_test.go" \
    "" \
    "backend/internal/domain/foo/foo.go" \
    "$(diff_new_exported backend/internal/domain/foo/foo.go '// Foo は…')"

unit t12_pass_generated pass "生成コード(*_string.go)の新規追加" \
    "backend/internal/domain/foo/kind_string.go" \
    "backend/internal/domain/foo/kind_string.go" \
    "backend/internal/domain/foo/kind_string.go" \
    ""

unit t13_pass_testutil pass "testutil(テスト支援パッケージ)は除外" \
    "backend/internal/testutil/helper.go" \
    "backend/internal/testutil/helper.go" \
    "backend/internal/testutil/helper.go" \
    ""

unit t14_pass_non_backend pass "backend 配下でない .go / 非 .go" \
    "scripts/x.go docs/notes.md" \
    "scripts/x.go
docs/notes.md" \
    "scripts/x.go
docs/notes.md" \
    ""

# unit は on 前提(既定 on)。off は env を変えて別に見る — t1 と同じ入力で結果が反転する
# ことが逃げ道の証明なので、入力を t1 に揃える。
off_out="$( cd "$TMPROOT" || exit 1
            mkdir -p off/backend/internal/domain/foo && cd off || exit 1
            : > backend/internal/domain/foo/foo.go
            STOCKBOT_TDD_CHECK=off tdd_violations \
                "backend/internal/domain/foo/foo.go" "backend/internal/domain/foo/foo.go" "" )"
check "unit/t15 STOCKBOT_TDD_CHECK=off" "" "$off_out"

# 逆向き: **未設定なら検査する**(既定 on = fail-close)。このリポは人間のシェルに
# STOCKBOT_TDD_CHECK=on を export しているので、unit は既定値を一度も踏んでいなかった
# — `${STOCKBOT_TDD_CHECK:-on}` を `:-off` に書き換える変異が全緑のまま通り、
# 「env 1 行で A2 の検査が丸ごと消える」向きが無拘束だった(2026-08-09 の変異テストで発覚)。
# 入力は t15 と同じにする(同じ入力で結果が反転することが既定値の証明)。
def_out="$( cd "$TMPROOT" || exit 1
            mkdir -p defon/backend/internal/domain/foo && cd defon || exit 1
            : > backend/internal/domain/foo/foo.go
            unset STOCKBOT_TDD_CHECK
            tdd_violations \
                "backend/internal/domain/foo/foo.go" "backend/internal/domain/foo/foo.go" "" )"
check "unit/t15b 未設定なら検査する(既定 on)" block "$([ -n "$def_out" ] && echo block || echo pass)"
# 「非空か」だけだと、別の Tier が誤って発火しても緑になる。期待どおり Tier 1 が出したかを見る。
expect_msg "unit/t15b Tier 1 のメッセージである" \
    "backend/internal/domain/foo/foo.go にテストが無い" "$def_out"

# 新規ファイルでも、パッケージにテストがあれば Tier 1 は通す。そこを Tier 2 が見ないと
# 「テストのあるパッケージに新規ファイルを足す」= 機能追加の最も普通の形が素通りする。
unit t17_block_new_in_tested_pkg block "テスト有パッケージへの新規ファイル + exported 追加" \
    "backend/internal/domain/foo/foo.go backend/internal/domain/foo/bar_test.go" \
    "backend/internal/domain/foo/foo.go" \
    "backend/internal/domain/foo/foo.go" \
    "$(diff_new_exported backend/internal/domain/foo/foo.go 'func NewFoo() int { return 1 }')"

# Tier 1 で報告済みの新規ファイルを Tier 2 が二重に報告しない。
# ファイル間の移動は「exported の追加」ではない。+ 側と - 側で同じ宣言行が現れたら相殺する
# (A1 の exported_touched と同じ考え方)。相殺しないと**テストのあるパッケージ内での
# ファイル分割**が「exported を追加している」という事実と違うメッセージで block される。
unit t18_pass_moved_exported pass "同一セッション内で exported を別ファイルへ移動" \
    "backend/internal/domain/foo/a.go backend/internal/domain/foo/b.go backend/internal/domain/foo/foo_test.go" \
    "backend/internal/domain/foo/b.go" \
    "backend/internal/domain/foo/a.go
backend/internal/domain/foo/b.go" \
    "$(printf 'diff --git a/backend/internal/domain/foo/a.go b/backend/internal/domain/foo/a.go\n--- a/backend/internal/domain/foo/a.go\n+++ b/backend/internal/domain/foo/a.go\n@@ -1,1 +0,0 @@\n-func Existing() int { return 1 }\n+++ b/backend/internal/domain/foo/b.go\n@@ -0,0 +1,1 @@\n+func Existing() int { return 1 }\n')"

# 相殺は正規化後の行で比べる。移動のついでに行末コメントを落とす / 整列が変わるのは
# よくあるので、生の行で比べると相殺できず誤 block する。
unit t20_pass_moved_with_comment pass "移動時に行末コメントが落ちても相殺する" \
    "backend/internal/domain/foo/a.go backend/internal/domain/foo/b.go backend/internal/domain/foo/foo_test.go" \
    "backend/internal/domain/foo/b.go" \
    "backend/internal/domain/foo/a.go
backend/internal/domain/foo/b.go" \
    "$(printf 'diff --git a/backend/internal/domain/foo/a.go b/backend/internal/domain/foo/a.go\n--- a/backend/internal/domain/foo/a.go\n+++ b/backend/internal/domain/foo/a.go\n@@ -1,1 +0,0 @@\n-func  Existing() int { return 1 }   // 旧コメント\n+++ b/backend/internal/domain/foo/b.go\n@@ -0,0 +1,1 @@\n+func Existing() int { return 1 }\n')"

# 相殺は「同じ宣言行」に限る。シグネチャ変更は相殺せず block したまま
# (名前だけで相殺すると契約変更を丸ごと見逃す)。
unit t19_block_signature_change block "exported のシグネチャ変更は相殺しない" \
    "backend/internal/domain/foo/a.go backend/internal/domain/foo/foo_test.go" \
    "" \
    "backend/internal/domain/foo/a.go" \
    "$(printf 'diff --git a/backend/internal/domain/foo/a.go b/backend/internal/domain/foo/a.go\n--- a/backend/internal/domain/foo/a.go\n+++ b/backend/internal/domain/foo/a.go\n@@ -1,1 +1,1 @@\n-func Existing() int { return 1 }\n+func Existing() int64 { return 1 }\n')"

# パスは `$2` で取ると空白で切れ、`.go` で終わらなくなって tdd_is_exempt に無言で
# 落とされる(= 判定から消える)。ヘッダ全体から取る。
unit t21_block_path_with_space block "パスに空白を含むファイル" \
    "backend/internal/domain/foo/foo_test.go" \
    "" \
    "backend/internal/domain/foo/new api.go" \
    "$(printf 'diff --git a/x b/x\n--- a/backend/internal/domain/foo/new api.go\n+++ b/backend/internal/domain/foo/new api.go\n@@ -0,0 +1,1 @@\n+func NewAPI() int { return 9 }\n')"
expect_msg "unit/t21 パスが切り詰められていない" "new api.go" "$UNIT_OUT"

# ヘッダは末尾にタブ(+ タイムスタンプ)が付く形もある。除去しないとパスに紛れ込む。
unit t22_block_tab_header block "ヘッダ末尾にタブが付く形" \
    "backend/internal/domain/foo/foo_test.go" \
    "" \
    "backend/internal/domain/foo/tabbed.go" \
    "$(printf 'diff --git a/x b/x\n--- a/backend/internal/domain/foo/tabbed.go\t2026-08-08\n+++ b/backend/internal/domain/foo/tabbed.go\t2026-08-08\n@@ -0,0 +1,1 @@\n+func NewAPI() int { return 9 }\n')"
expect_msg "unit/t22 タブがパスに残っていない" "tabbed.go が" "$UNIT_OUT"

unit t23_dedupe_two_exported block "同じファイルの exported 2 つで報告 1 行" \
    "backend/internal/domain/foo/y.go backend/internal/domain/foo/foo_test.go" \
    "" \
    "backend/internal/domain/foo/y.go" \
    "$(printf 'diff --git a/backend/internal/domain/foo/y.go b/backend/internal/domain/foo/y.go\n--- a/backend/internal/domain/foo/y.go\n+++ b/backend/internal/domain/foo/y.go\n@@ -0,0 +1,2 @@\n+func One() int { return 1 }\n+func Two() int { return 2 }\n')"
check "unit/t23 報告行数" "1" "$(printf '%s\n' "$UNIT_OUT" | sed '/^$/d' | wc -l | tr -d ' ')"

# 正規化のコメント除去は**文字列の中の // を落とさない**。落とすと
# `var X = "https://…/v1"` → `"https://…/v2"` が同じ行に見えて相殺され、値の変更が消える。
unit t24_block_url_value_change block "文字列内の // を落とさない(URL の値変更)" \
    "backend/internal/domain/foo/u.go backend/internal/domain/foo/foo_test.go" \
    "" \
    "backend/internal/domain/foo/u.go" \
    "$(printf 'diff --git a/backend/internal/domain/foo/u.go b/backend/internal/domain/foo/u.go\n--- a/backend/internal/domain/foo/u.go\n+++ b/backend/internal/domain/foo/u.go\n@@ -1,1 +1,1 @@\n-var BaseURL = "https://api.example.com/v1"\n+var BaseURL = "https://api.example.com/v2"\n')"

# Tier 3: 唯一の *_test.go を消してパッケージのテストが 0 になる。「テストの存在」を
# 最も直接に壊す形なのに、Tier 1(新規ファイルだけが対象)にも Tier 2(テストを 1 つでも
# 触れば免除)にも当たらない = **消せば黙る**、になっていた(5 巡目レビュー)。
unit t25_block_test_deleted block "唯一の _test.go を消してテストが 0 になる" \
    "backend/internal/domain/foo/foo.go" \
    "" \
    "backend/internal/domain/foo/foo.go
backend/internal/domain/foo/foo_test.go" \
    ""
expect_msg "unit/t25 メッセージが Tier 3 のもの" "テストが 0 個" "$UNIT_OUT"

unit t26_pass_test_renamed pass "_test.go を rename(テストは残る)" \
    "backend/internal/domain/foo/foo.go backend/internal/domain/foo/renamed_test.go" \
    "" \
    "backend/internal/domain/foo/foo_test.go
backend/internal/domain/foo/renamed_test.go" \
    ""

unit t27_pass_pkg_removed pass "パッケージごと消す(残る .go が無い)" \
    "" \
    "" \
    "backend/internal/domain/foo/foo.go
backend/internal/domain/foo/foo_test.go" \
    ""

# ディレクトリは残るが、残っているのが除外対象 / 非 .go だけ。守るべきコードが無いので通す。
unit t28_pass_only_exempt_left pass "テスト削除後、残るのが生成コード / 非 .go だけ" \
    "backend/internal/domain/foo/kind_string.go backend/internal/domain/foo/README.md" \
    "" \
    "backend/internal/domain/foo/foo_test.go" \
    ""

# testdata/ は Go ツールチェインがビルドもテストもしないので、そこに *_test.go を
# 要求するのは構造的に不可能な要求 = 必ず誤検知する。golden-file テストは Go の標準的な
# 次の一手なので、入れた瞬間に「無視されるガード」になる。
unit t29_pass_testdata_new pass "testdata/ の新規 .go" \
    "backend/internal/domain/foo/testdata/sample.go" \
    "backend/internal/domain/foo/testdata/sample.go" \
    "backend/internal/domain/foo/testdata/sample.go" \
    ""
unit t30_pass_testdata_test_deleted pass "testdata/ の _test.go 削除" \
    "backend/internal/domain/foo/testdata/fixture.go" \
    "" \
    "backend/internal/domain/foo/testdata/golden_test.go" \
    ""

# コメント除去は**引用の外の // だけ**を落とす。`"` を見た時点で諦める実装だと、
# 文字列を含む宣言行のコメントだけを直したときに相殺できず誤 block する
# (「コメントだけの変更では止めない」という元々の意図が、文字列を含む行でだけ死んでいた)。
unit t31_pass_comment_only_with_string pass "文字列を含む宣言行のコメントだけ直す" \
    "backend/internal/domain/foo/url.go backend/internal/domain/foo/foo_test.go" \
    "" \
    "backend/internal/domain/foo/url.go" \
    "$(printf 'diff --git a/backend/internal/domain/foo/url.go b/backend/internal/domain/foo/url.go\n--- a/backend/internal/domain/foo/url.go\n+++ b/backend/internal/domain/foo/url.go\n@@ -1,1 +1,1 @@\n-var BaseURL = "https://api.example.com/v1" // 旧コメント\n+var BaseURL = "https://api.example.com/v1" // 新コメント\n')"

# 生文字列の中の // はコメントではない。落とすと中身の変更が相殺されて消える。
unit t32_block_rawstring_change block "生文字列の中の // を落とさない" \
    "backend/internal/domain/foo/r.go backend/internal/domain/foo/foo_test.go" \
    "" \
    "backend/internal/domain/foo/r.go" \
    "$(printf 'diff --git a/backend/internal/domain/foo/r.go b/backend/internal/domain/foo/r.go\n--- a/backend/internal/domain/foo/r.go\n+++ b/backend/internal/domain/foo/r.go\n@@ -1,1 +1,1 @@\n-const Doc = `see https://old // note`\n+const Doc = `see https://new // note`\n')"

# Tier 3 の報告はディレクトリ単位で 1 行(同じ穴を消えたテストの数だけ報告しない)。
unit t33_tier3_dedup block "同一ディレクトリの _test.go 2 つ削除で報告 1 行" \
    "backend/internal/domain/foo/foo.go" \
    "" \
    "backend/internal/domain/foo/a_test.go
backend/internal/domain/foo/b_test.go" \
    ""
check "unit/t33 報告行数" "1" "$(printf '%s\n' "$UNIT_OUT" | sed '/^$/d' | wc -l | tr -d ' ')"

# seen_dirs の照合は部分文字列でなく完全一致。空白入りのディレクトリ名だと「短い方が長い方に
# 含まれる」ため、違反が黙って 1 件消える(`sort -u` 済みなので ' ' < '/' で悪い順に並ぶ)。
unit t34_tier3_dir_prefix block "似た名前のディレクトリを取り違えない" \
    "backend/internal/my pkg/a.go
backend/internal/my/b.go" \
    "" \
    "backend/internal/my pkg/x_test.go
backend/internal/my/x_test.go" \
    ""
check "unit/t34 報告行数" "2" "$(printf '%s\n' "$UNIT_OUT" | sed '/^$/d' | wc -l | tr -d ' ')"

# strip_comment の分岐ごとの固定。エスケープ / rune リテラルは実 Go の宣言行に普通に出る。
unit t35_block_escaped_quote_change block "文字列内のエスケープ済み // の変更" \
    "backend/internal/domain/foo/q.go backend/internal/domain/foo/foo_test.go" \
    "" \
    "backend/internal/domain/foo/q.go" \
    "$(printf 'diff --git a/backend/internal/domain/foo/q.go b/backend/internal/domain/foo/q.go\n--- a/backend/internal/domain/foo/q.go\n+++ b/backend/internal/domain/foo/q.go\n@@ -1,1 +1,1 @@\n-var Q = "a\\"//x"\n+var Q = "a\\"//y"\n')"

unit t36_pass_rune_dquote_comment pass "rune リテラル '\"' の行のコメントだけ直す" \
    "backend/internal/domain/foo/r.go backend/internal/domain/foo/foo_test.go" \
    "" \
    "backend/internal/domain/foo/r.go" \
    "$(printf 'diff --git a/backend/internal/domain/foo/r.go b/backend/internal/domain/foo/r.go\n--- a/backend/internal/domain/foo/r.go\n+++ b/backend/internal/domain/foo/r.go\n@@ -1,1 +1,1 @@\n-const DQ = %s"%s // 旧\n+const DQ = %s"%s // 新\n' "'" "'" "'" "'")"

unit t37_pass_rune_escaped_quote pass "rune リテラル '\\'' の行のコメントだけ直す" \
    "backend/internal/domain/foo/s.go backend/internal/domain/foo/foo_test.go" \
    "" \
    "backend/internal/domain/foo/s.go" \
    "$(printf 'diff --git a/backend/internal/domain/foo/s.go b/backend/internal/domain/foo/s.go\n--- a/backend/internal/domain/foo/s.go\n+++ b/backend/internal/domain/foo/s.go\n@@ -1,1 +1,1 @@\n-const SQ = %s\\%s%s // 旧\n+const SQ = %s\\%s%s // 新\n' "'" "'" "'" "'" "'" "'")"

# testdata/ の *_test.go は Go が絶対に走らせない。Tier 1 が「テスト」に数えないのに
# Tier 2 の免除には数える、という矛盾を残さない。
unit t38_block_testdata_test_not_exempting block "testdata/ の _test.go は Tier 2 を免除しない" \
    "backend/internal/domain/foo/foo.go backend/internal/domain/foo/foo_test.go" \
    "" \
    "backend/internal/domain/foo/foo.go
backend/internal/domain/foo/testdata/fixture_test.go" \
    "$(diff_new_exported backend/internal/domain/foo/foo.go 'func NewFoo() int { return 1 }')"

unit t16_single_report block "新規 + exported 追加でも報告は 1 行" \
    "backend/internal/domain/foo/foo.go" \
    "backend/internal/domain/foo/foo.go" \
    "backend/internal/domain/foo/foo.go" \
    "$(diff_new_exported backend/internal/domain/foo/foo.go 'func NewFoo() int { return 1 }')"
check "unit/t16 報告行数" "1" "$(printf '%s\n' "$UNIT_OUT" | sed '/^$/d' | wc -l | tr -d ' ')"

# Part 2: pre-stop-checks.sh の e2e(git から何を拾うか)
TPL="$TMPROOT/_tpl"
build_template() {
    mkdir -p "$TPL"/backend/internal/domain/bar "$TPL"/scripts "$TPL"/runtime/logs \
             "$TPL"/.claude/hooks
    # hook は本番と同じくリポジトリの中から動かす(補助ライブラリの解決も含めて実物を見る)。
    # 🛑 hook が source する lib は**全部**テンプレへ持ち込む。忘れると fail-CLOSED 側の
    # ガードが一斉に発火して大量に赤くなる(secret-patterns.sh 追加時に実際そうなった)。
    cp "$HOOK" "$LIB" "$PATLIB" "$STOPLIB" "$ENFLIB" "$TPL/.claude/hooks/"
    # 先頭 `/` 必須: 無いと gitignore が任意階層に当たる。
    printf '/runtime/\n' > "$TPL/.gitignore"
    printf 'package bar\n\nfunc helper() int { return 1 }\n' > "$TPL/backend/internal/domain/bar/bar.go"
    printf 'package bar\n\nfunc TestHelper(t *testing.T) {}\n' > "$TPL/backend/internal/domain/bar/bar_test.go"
    printf '#!/usr/bin/env bash\nexit 0\n' > "$TPL/scripts/arch-guard.sh"
    printf 'check-backend:\n\t@true\n' > "$TPL/Makefile"
    git -C "$TPL" init -q
    git -C "$TPL" config user.email test@example.com
    git -C "$TPL" config user.name test
    git -C "$TPL" add -A
    git -C "$TPL" commit -qm base1
    printf '\n// second\n' >> "$TPL/backend/internal/domain/bar/bar.go"
    git -C "$TPL" add -A
    git -C "$TPL" commit -qm base2
}
build_template

new_repo() { local repo="$TMPROOT/$1"; cp -r "$TPL" "$repo"; echo "$repo"; }

run_hook() { # $1 = repo, $2 = stdin, $3 = env 追加(空可); exit code を echo
    local repo="$1" stdin="$2" extra="${3:-}" rc=0
    # 人間のシェルに STOCKBOT_PRESTOP_CHECKS=off / STOCKBOT_TDD_CHECK=off が export されていても
    # テストが素通りしない(= 嘘の緑にならない)よう明示的に on を渡す。
    # $extra は既定の後ろに置く(env は後勝ちなので、ケース側から off へ上書きできる)。
    printf '%s' "$stdin" \
        | ( cd "$repo" && env STOCKBOT_PRESTOP_CHECKS=on STOCKBOT_TDD_CHECK=on $extra \
            CLAUDE_PROJECT_DIR="$repo" bash "$repo/.claude/hooks/pre-stop-checks.sh" \
            >/dev/null 2>/dev/null ) || rc=$?
    echo "$rc"
}

SID='{"session_id":"selftest"}'

c_e01() {
repo="$(new_repo e1)"
mkdir -p "$repo/backend/internal/domain/foo"
printf 'package foo\n\nfunc New() int { return 1 }\n' > "$repo/backend/internal/domain/foo/foo.go"
check "e2e/e1 新規 .go(テスト無し・作業ツリー)" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e01

c_e02() {
repo="$(new_repo e2)"
mkdir -p "$repo/backend/internal/domain/foo"
printf 'package foo\n\nfunc New() int { return 1 }\n' > "$repo/backend/internal/domain/foo/foo.go"
printf 'package foo\n\nfunc TestNew(t *testing.T) {}\n' > "$repo/backend/internal/domain/foo/foo_test.go"
check "e2e/e2 新規 .go + _test.go" "0" "$(run_hook "$repo" "$SID")"
}
h_defer c_e02

c_e03() {
repo="$(new_repo e3)"
mkdir -p "$repo/backend/internal/domain/foo"
printf 'package foo\n\nfunc New() int { return 1 }\n' > "$repo/backend/internal/domain/foo/foo.go"
check "e2e/e3 STOCKBOT_TDD_CHECK=off" "0" "$(run_hook "$repo" "$SID" "STOCKBOT_TDD_CHECK=off")"
}
h_defer c_e03

c_e04() {
# e3b: 逆向き。**未設定なら Step 8.5 は走る**(既定 on = fail-close)。
# run_hook は明示的に on を渡すので、既定値そのものはどのケースも踏んでいなかった —
# `${STOCKBOT_TDD_CHECK:-on}` を `:-off` に書き換える変異が全緑のまま通り、
# **env 1 行で Step 8.5 が丸ごと消える**向きが無拘束だった(2026-08-09 の変異テストで発覚)。
# 空値を渡すのは env で unset にできないため。`${VAR:-on}` は未設定でも空でも既定を採るので
# 同じ分岐を踏む。入力は e3 と同じにする(同じ入力で結果が反転することが既定値の証明)。
repo="$(new_repo e3b)"
mkdir -p "$repo/backend/internal/domain/foo"
printf 'package foo\n\nfunc New() int { return 1 }\n' > "$repo/backend/internal/domain/foo/foo.go"
check "e2e/e3b 未設定なら Step 8.5 が走る(既定 on)" "2" "$(run_hook "$repo" "$SID" "STOCKBOT_TDD_CHECK=")"
}
h_defer c_e04

# e4/e5 は同じ状態を「セッション最初の Stop か」だけ変えて比較する。
#   コミット済み: テスト無しの新規 .go / 作業ツリー: テストのあるパッケージの内部変更。
#   最初の Stop なら HEAD~..HEAD を見るので block、2 回目以降は見ないので pass。
seed_commit_and_dirty() { # $1 = repo
    mkdir -p "$1/backend/internal/domain/foo"
    printf 'package foo\n\nfunc New() int { return 1 }\n' > "$1/backend/internal/domain/foo/foo.go"
    git -C "$1" add -A
    git -C "$1" commit -qm "add foo without test"
    printf '\nfunc more() int { return 2 }\n' >> "$1/backend/internal/domain/bar/bar.go"
}

c_e05() {
repo="$(new_repo e4)"
seed_commit_and_dirty "$repo"
check "e2e/e4 最初の Stop は直前コミットを見る" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e05

c_e06() {
repo="$(new_repo e5)"
seed_commit_and_dirty "$repo"
mkdir -p "$repo/runtime/logs"
git -C "$repo" rev-parse HEAD > "$repo/runtime/logs/session-start-sha.selftest"
: > "$repo/runtime/logs/tdd-check-seen.selftest"
check "e2e/e5 2 回目以降は直前コミットを見ない" "0" "$(run_hook "$repo" "$SID")"
}
h_defer c_e06

c_e07() {
# e5b: 🛑 **意図的な仕様の固定**。直前コミット由来の block は 1 セッション 1 回で、
# 何も直さず再 Stop すると通る。「このコミットは今セッションのものか前セッションのものか」は
# session 開始点が分からない限り**判定不能**で、持続的に block すると
# 「Go を触っていないセッションが前セッションのコミットで止められる」誤検知になる。
# 誤検知するガードは無視されるようになるので、取りこぼす側に倒してある。
# **真の解決は H1(SessionStart hook)/ T12** — そこで開始 SHA が確定すれば両方消える。
# 作業ツリー / session 範囲由来の block は持続する(e7 が固定)ので、抜けるのはこの経路だけ。
repo="$(new_repo e5b)"
seed_commit_and_dirty "$repo"
check "e2e/e5b 直前コミット由来は 1 回だけ block(既知の穴)" "2" "$(run_hook "$repo" "$SID")"
check "e2e/e5b 2 回目は通る(H1/T12 で解消)"                "0" "$(run_hook "$repo" "$SID")"
}
h_defer c_e07

c_e08() {
# e7b: 作業ツリー由来の違反は何度 Stop しても block し続ける(e5b の穴がここに漏れていない)。
repo="$(new_repo e7b)"
mkdir -p "$repo/backend/internal/domain/foo"
printf 'package foo\n\nfunc New() int { return 1 }\n' > "$repo/backend/internal/domain/foo/foo.go"
check "e2e/e7b 作業ツリー由来は 1 回目も block" "2" "$(run_hook "$repo" "$SID")"
check "e2e/e7b 作業ツリー由来は 2 回目も block" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e08

c_e09() {
# e6: session_id が取れないときは作業ツリーだけに縮退する(印が毎回別名になり
#     「常に最初の Stop」= 永久 block になるのを避ける。docs-sync と同じ判断)。
repo="$(new_repo e6)"
seed_commit_and_dirty "$repo"
check "e2e/e6 session 不明なら作業ツリーだけ" "0" "$(run_hook "$repo" '{}')"
}
h_defer c_e09

c_e10() {
# e7: session 範囲(session-start-sha..HEAD)のコミットは 2 回目以降でも見る。
repo="$(new_repo e7)"
mkdir -p "$repo/runtime/logs"
git -C "$repo" rev-parse HEAD > "$repo/runtime/logs/session-start-sha.selftest"
: > "$repo/runtime/logs/tdd-check-seen.selftest"
seed_commit_and_dirty "$repo"
check "e2e/e7 session 範囲のコミットは常に見る" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e10

c_e11() {
# e8: backend に一切触らないセッションは早期 exit で通る(回帰)。
repo="$(new_repo e8)"
printf 'note\n' > "$repo/notes.md"
check "e2e/e8 Go 変更なしは素通り" "0" "$(run_hook "$repo" "$SID")"
}
h_defer c_e11

c_e12() {
# e11: Tier 2 の e2e。Part 1 は diff を手で渡すので、hook が **diff を集めているか**は
# ここでしか見えない(`tdd_diff` を空文字にする変異が単体だけでは全緑で生き残った)。
repo="$(new_repo e11)"
printf '\nfunc Exported() int { return 3 }\n' >> "$repo/backend/internal/domain/bar/bar.go"
git -C "$repo" add -A
git -C "$repo" commit -qm "add exported without touching tests"
check "e2e/e11 Tier 2(exported 追加・テスト未変更)" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e12

c_e13() {
# e12: --no-renames が効いているか。テストの無いパッケージへ git mv すると、
# リネームを R に畳んだままだと --diff-filter=A が何も出さず Tier 1 が素通りする。
repo="$(new_repo e12)"
mkdir -p "$repo/backend/internal/domain/baz"
git -C "$repo" mv backend/internal/domain/bar/bar.go backend/internal/domain/baz/baz.go
git -C "$repo" commit -qm "move bar.go to a package without tests"
check "e2e/e12 リネームを新規追加として見る" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e13

c_e14() {
# e13: 🛑 untracked（= Write でファイルを作ってまだコミットしていない）状態の新規ファイルを
# Tier 2 が見るか。Tier 1 は untracked を見る（e1）のに Tier 2 の diff だけ見ていなかった
# ため、「テストのあるパッケージに新規ファイルを足す」= P0-2 で塞いだはずの穴が、
# **Claude が実際に作業している未コミット状態でだけ**素通りしていた（2 巡目レビュー）。
repo="$(new_repo e13)"
printf 'package bar\n\nfunc BrandNewAPI() int { return 9 }\n' > "$repo/backend/internal/domain/bar/newapi.go"
check "e2e/e13 untracked 新規 × テスト有パッケージ" "2" "$(run_hook "$repo" "$SID")"
# exit code だけ見ると**別のファイルの名前で block していても緑**になる。合成 diff の
# ヘッダが採用されているかは、報告されたパスでしか判別できない(実際に、ヘッダの
# 前置を外す変異が exit code だけのテストでは生き残った)。
e13err="$TMPROOT/e13.err"
printf '%s' "$SID" | ( cd "$repo" && env STOCKBOT_PRESTOP_CHECKS=on STOCKBOT_TDD_CHECK=on \
    CLAUDE_PROJECT_DIR="$repo" bash "$repo/.claude/hooks/pre-stop-checks.sh" >/dev/null 2>"$e13err" ) || true
if grep -q 'bar/newapi\.go' "$e13err"; then
    check "e2e/e13 報告されるパスが合成 diff のもの" "ok" "ok"
else
    check "e2e/e13 報告されるパスが合成 diff のもの" "ok" "$(grep -o 'backend/[^ ]*' "$e13err" | head -1)"
fi
}
h_defer c_e14

c_e15() {
# e16: untracked を合成 diff にするとき .go 以外を混ぜない。判定そのものは
# tdd_is_exempt が落とすので結果は変わらない(このケースはフィルタの有無に依らず緑)。
# フィルタの目的は**バイナリ / 巨大生成物を行単位で読み込まない**こと。
repo="$(new_repo e16)"
printf 'package bar\n\nfunc note() int { return 1 }\n' > "$repo/backend/internal/domain/bar/note.go"
printf '# メモ\n\nfunc Exported() int { return 1 }\n' > "$repo/backend/internal/domain/bar/NOTES.md"
check "e2e/e16 untracked の非 .go は判定に影響しない" "0" "$(run_hook "$repo" "$SID")"
}
h_defer c_e15

c_e16() {
# e17: 🛑 合成 diff の継ぎ目。最終改行の無いファイルを `sed 's/^/+/'` で流すと、
# 次の `+++ b/…` ヘッダが最終行に連結されて**消える** → path が前のファイルのまま据え置かれ、
# 後続ファイルの exported が見えなくなる(または別ファイルの名前で報告される)。
# gofmt 前の「Write で作ったばかりのファイル」がまさにこの状態なので、
# 2 巡目で塞いだ対象状態でそのまま踏む。
# 先行ファイルを除外対象(backend/cmd/**)にするのが判別子。飲み込まれると path が
# 除外対象のまま据え置かれ、後続の exported が**まるごと消える**(exit 0)。
# 同一パッケージ内で試すと「別ファイルの名前で報告される」だけで exit 2 になり、
# バグを見逃す(実際に 1 度この形で書いて緑になった)。
repo="$(new_repo e17)"
mkdir -p "$repo/backend/cmd/stockbot"
printf 'package main\n\nfunc wire() {}' > "$repo/backend/cmd/stockbot/wire.go"
printf 'package bar\n\nfunc NewAPI() int { return 9 }\n' > "$repo/backend/internal/domain/bar/zzz.go"
check "e2e/e17 最終改行の無い untracked が後続を飲み込まない" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e16

c_e17() {
# e18: 合成 diff は中身を全行 `+` 前置するので、中身の `++ b/<path>` が `+++ b/<path>` に
# 化けてファイルヘッダとして解釈されうる。ヘッダは直前の `--- ` でしか始まらない形にして、
# 中身から偽装できないようにする(中身は必ず `+` 前置なので `--- ` は原理的に作れない)。
repo="$(new_repo e18)"
printf 'package bar\n\nconst doc = `\n++ b/backend/internal/testutil/fake.go\n`\n\nfunc NewAPI() int { return 9 }\n' \
    > "$repo/backend/internal/domain/bar/spoof.go"
check "e2e/e18 中身からファイルヘッダを偽装できない" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e17

c_e18() {
# e14/e15: テストのあるパッケージ内でのファイル分割は block しない（振る舞い不変なので
# 「exported を追加した」は事実ではない）。commit 済みと untracked の両方を見る。
repo="$(new_repo e14)"
printf 'package bar\n\nfunc Existing() int { return 1 }\n' > "$repo/backend/internal/domain/bar/bar.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "add exported"
git -C "$repo" rev-parse HEAD > "$repo/runtime/logs/session-start-sha.selftest"
printf 'package bar\n' > "$repo/backend/internal/domain/bar/bar.go"
printf 'package bar\n\nfunc Existing() int { return 1 }\n' > "$repo/backend/internal/domain/bar/split.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "split into split.go"
check "e2e/e14 テスト有パッケージ内のファイル分割(commit 済み)" "0" "$(run_hook "$repo" "$SID")"
}
h_defer c_e18

c_e19() {
repo="$(new_repo e15)"
printf 'package bar\n\nfunc Existing() int { return 1 }\n' > "$repo/backend/internal/domain/bar/bar.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "add exported"
# 分割そのものだけを範囲に入れる(直前の「exported を足したコミット」は今回の対象外)。
git -C "$repo" rev-parse HEAD > "$repo/runtime/logs/session-start-sha.selftest"
: > "$repo/runtime/logs/tdd-check-seen.selftest"
printf 'package bar\n' > "$repo/backend/internal/domain/bar/bar.go"
printf 'package bar\n\nfunc Existing() int { return 1 }\n' > "$repo/backend/internal/domain/bar/split.go"
check "e2e/e15 同じ分割が untracked のとき" "0" "$(run_hook "$repo" "$SID")"
}
h_defer c_e19

c_e20() {
# e10: CLAUDE_PROJECT_DIR が hook のツリーと別でも、補助ライブラリを自分の隣から解決する。
# 相対パス(`.claude/hooks/…`)に戻すと lib 欠落 = 同じ exit 2 になるので、
# exit code ではなく**どちらの理由で止まったか**を見る。
repo="$(new_repo e10)"
rm -rf "$repo/.claude"
mkdir -p "$repo/backend/internal/domain/foo"
printf 'package foo\n\nfunc New() int { return 1 }\n' > "$repo/backend/internal/domain/foo/foo.go"
err="$TMPROOT/e10.err"
# .claude/ を消したこと自体が Step 4(enforcement warn-once)に当たるので 2 回流す。
for _ in 1 2; do
    printf '%s' "$SID" \
        | ( cd "$repo" && env STOCKBOT_PRESTOP_CHECKS=on STOCKBOT_TDD_CHECK=on \
            CLAUDE_PROJECT_DIR="$repo" bash "$HOOK" >/dev/null 2>"$err" ) || true
done
if grep -q "TEST EXISTENCE CHECK BLOCKED" "$err"; then
    check "e2e/e10 lib は hook の隣から解決する" "ok" "ok"
else
    check "e2e/e10 lib は hook の隣から解決する" "ok" "$(head -3 "$err" | tr '\n' ' ')"
fi
}
h_defer c_e20

c_e21() {
# e9: 判定ライブラリを消したら fail-CLOSED(検査が黙って消えない)。
# lib を消すと Step 4(enforcement warn-once)にも当たるので、1 回流して warn を消化してから
# 2 回目を見る。これをしないと「enforcement warn で止まった」を fail-close と誤読する
# (実際に mutation testing で survived になり発覚)。
repo="$(new_repo e9)"
rm -f "$repo/.claude/hooks/tdd-check-lib.sh"
mkdir -p "$repo/backend/internal/domain/foo"
printf 'package foo\n\nfunc New() int { return 1 }\n' > "$repo/backend/internal/domain/foo/foo.go"
run_hook "$repo" "$SID" >/dev/null
check "e2e/e9 lib 欠落は fail-CLOSED" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e21

c_e22() {
# e11 / e12 (RF3 #2): lib が「在るが使えない」場合も fail-CLOSED であること。
# 🛑 e9 の守りは `[ -f "$tdd_lib" ]` = **不在しか見ていない**。空ファイル / 関数定義を
# 欠いた lib は `[ -f ]` を通過し、以降 `tdd_violations` が command-not-found(127)になる。
# ところが呼び出し側が `tdd_out="$(tdd_violations … || true)"` と **127 を飲む**ので、
# 出力が空 → `[ -n "$tdd_out" ]` が偽 → **A2 が黙って消えて hook は exit 0**(RF2 実測)。
# `: > tdd-check-lib.sh` の 1 手で検査が無効化できる形を残さない。
# lib を書き換えると Step 4(enforcement warn-once)に当たるので e9 と同じく 2 回流す。
repo="$(new_repo e11)"
: > "$repo/.claude/hooks/tdd-check-lib.sh"          # 空ファイル
mkdir -p "$repo/backend/internal/domain/foo"
printf 'package foo\n\nfunc New() int { return 1 }\n' > "$repo/backend/internal/domain/foo/foo.go"
run_hook "$repo" "$SID" >/dev/null
check "e2e/e11 lib が空でも fail-CLOSED" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e22

c_e23() {
repo="$(new_repo e12)"
# 構文的には正しいが判定関数を定義していない(cp の途中で切れた / 別物を置いた)。
printf '#!/usr/bin/env bash\n# 判定関数を定義しない\ntdd_unrelated() { :; }\n' \
    > "$repo/.claude/hooks/tdd-check-lib.sh"
mkdir -p "$repo/backend/internal/domain/foo"
printf 'package foo\n\nfunc New() int { return 1 }\n' > "$repo/backend/internal/domain/foo/foo.go"
run_hook "$repo" "$SID" >/dev/null
check "e2e/e12 lib に判定関数が無ければ fail-CLOSED" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e23

c_e24() {
# 対照: 正常な lib なら従来どおり検査が走って block する(上の 2 件が
# 「何をしても 2 になる」だけの嘘の緑でないことの担保)。
repo="$(new_repo e13)"
mkdir -p "$repo/backend/internal/domain/foo"
printf 'package foo\n\nfunc New() int { return 1 }\n' > "$repo/backend/internal/domain/foo/foo.go"
check "e2e/e13 対照: 正常な lib では A2 が block" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e24

c_e25() {
# e14: 判定関数は定義できているが**末尾が壊れている** lib。sentinel は通過してしまうので、
# ここだけが `. "$tdd_lib" ||` のガードを拘束する(これが無いと変異テストで survive した)。
# cp が途中で切れた / 追記が壊れた状態に相当し、lib の残りが実行されていない可能性がある
# 以上 fail-CLOSED が正しい。
# 🛑 リポは **A2 的に違反ゼロ**(テスト付き)にする。違反のあるリポだと「lib が壊れていても
# 検査が通って block」= 別の理由の exit 2 になり、ガードを拘束できない(実際に 1 度そうなった)。
repo="$(new_repo e14)"
mkdir -p "$repo/backend/internal/domain/foo"
printf 'package foo\n\nfunc New() int { return 1 }\n' > "$repo/backend/internal/domain/foo/foo.go"
printf 'package foo\n\nimport "testing"\n\nfunc TestNew(t *testing.T) { _ = New() }\n' \
    > "$repo/backend/internal/domain/foo/foo_test.go"
check "e2e/e14 対照: 正常な lib + テスト付きなら通る" "0" "$(run_hook "$repo" "$SID")"

# e14b は exit code だけでは拘束できない(2 になる理由が他にもある)。
# `run_hook_err` / `expect_msg` が定義されたあと、**メッセージ**で見る(下の e14b を参照)。
}
h_defer c_e25

# 以下は「exit code だけ見ると緑になる」経路。報告メッセージまで見る。
run_hook_err() { # $1 = repo, $2 = stdin -> stderr の中身を echo
    # 🛑 `$$` はサブシェルでも親の PID なので、並列実行では受け皿が衝突する(RF3 #8)。
    local repo="$1" stdin="$2" ef="$TMPROOT/err.${H_CASE_ID}"
    printf '%s' "$stdin" | ( cd "$repo" && env STOCKBOT_PRESTOP_CHECKS=on STOCKBOT_TDD_CHECK=on \
        CLAUDE_PROJECT_DIR="$repo" bash "$repo/.claude/hooks/pre-stop-checks.sh" >/dev/null 2>"$ef" ) || true
    cat "$ef"
}
c_e26() {
# e14b / e14c (RF3 #2): lib が「読めるが壊れている」2 形。sentinel は通過するので、
# ここは `. "$tdd_lib" ||` のガードと bash 自身の挙動が守っている。
# 🛑 exit code だけでは拘束できない — この状況は secret scan など別経路でも 2 になり得る。
# 実際に 1 度「ガードを外しても緑」の嘘の拘束を書いた。**メッセージで見る。**
# lib を書き換えると Step 4(enforcement warn-once)に当たるので 1 回流して消化する。
#
# e14b = 構文エラー。🛑 hook は `set -euo pipefail` なので、**source 中の構文エラーでは
# bash が即座に exit 2 し、`|| {…}` のガードには到達しない**(実測)。fail-CLOSED では
# あるが hook 自身のメッセージは出ないので、「bash の syntax error が表に出ること」＝
# 黙って素通りしないことを固定する。
repo="$(new_repo e14b)"
{ cat "$LIB"; printf '\nif [ ; then\n'; } > "$repo/.claude/hooks/tdd-check-lib.sh"
mkdir -p "$repo/backend/internal/domain/foo"
printf 'package foo\n\nfunc New() int { return 1 }\n' > "$repo/backend/internal/domain/foo/foo.go"
printf 'package foo\n\nimport "testing"\n\nfunc TestNew(t *testing.T) { _ = New() }\n' \
    > "$repo/backend/internal/domain/foo/foo_test.go"
run_hook "$repo" "$SID" >/dev/null
expect_msg "e2e/e14b lib が構文エラーなら黙って通さない" \
    "syntax error" "$(run_hook_err "$repo" "$SID")"
}
h_defer c_e26

c_e27() {
# e14c = 構文は正しいが lib の中のコマンドが失敗する(cp が壊れた / 依存が無い等)。
# 🛑 **これが `set +e` で囲う理由**。素で source すると `set -euo pipefail` により
# bash がその場で **exit 1** して死ぬ。Stop hook は **exit 2 だけが block** なので、
# exit 1 は「非ブロックのエラー」= **A2 が黙って飛んだうえで Stop が通る**(実測)。
# ここだけがそのガードを拘束する。
repo="$(new_repo e14c)"
{ cat "$LIB"; printf '\nfalse\n'; } > "$repo/.claude/hooks/tdd-check-lib.sh"
mkdir -p "$repo/backend/internal/domain/foo"
printf 'package foo\n\nfunc New() int { return 1 }\n' > "$repo/backend/internal/domain/foo/foo.go"
printf 'package foo\n\nimport "testing"\n\nfunc TestNew(t *testing.T) { _ = New() }\n' \
    > "$repo/backend/internal/domain/foo/foo_test.go"
run_hook "$repo" "$SID" >/dev/null
expect_msg "e2e/e14c lib 内のコマンドが失敗したら block(exit 1 で素通りしない)" \
    "tdd-check-lib.sh の読み込みに失敗" "$(run_hook_err "$repo" "$SID")"
check "e2e/e14c は exit 2(block)であること" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e27

# e21x (RF3 #5): stop-hook-lib.sh の fail-CLOSED 3 段。
# この lib は **block_stop が定義される前**に要る(session_id を決めるのがこの lib 自身)
# ので、fail-close の出口は裸の `echo BLOCKED; exit 2`。Step 4 の warn-once より前に
# 出るため 2 回流す必要は無いが、🛑 exit code だけ見ると secret scan など別経路の 2 と
# 区別できないので**メッセージで拘束する**(§2.5 (c))。
# リポは A2 的に違反ゼロ(テスト付き)にする — 違反があると lib が生きていても 2 になり、
# ガードを 1 つも拘束しない嘘の緑になる(e14 の注記と同じ理由)。
stoplib_repo() { # $1 = case id -> テスト付き(= 他の理由で block しない)リポを作って echo
    local repo; repo="$(new_repo "$1")"
    mkdir -p "$repo/backend/internal/domain/foo"
    printf 'package foo\n\nfunc New() int { return 1 }\n' > "$repo/backend/internal/domain/foo/foo.go"
    printf 'package foo\n\nimport "testing"\n\nfunc TestNew(t *testing.T) { _ = New() }\n' \
        > "$repo/backend/internal/domain/foo/foo_test.go"
    echo "$repo"
}

c_e28() {
repo="$(stoplib_repo e21a)"
rm -f "$repo/.claude/hooks/stop-hook-lib.sh"
expect_msg "e2e/e21a stop-hook-lib 欠落は fail-CLOSED" \
    "stop-hook-lib.sh が見つからない" "$(run_hook_err "$repo" "$SID")"
check "e2e/e21a は exit 2(block)であること" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e28

c_e29() {
# 空ファイルは `[ -f ]` も `.` も通過する。sentinel(代表関数の command -v)だけが守り。
repo="$(stoplib_repo e21b)"
: > "$repo/.claude/hooks/stop-hook-lib.sh"
expect_msg "e2e/e21b stop-hook-lib が空でも fail-CLOSED" \
    "stop-hook-lib.sh に hooklib_" "$(run_hook_err "$repo" "$SID")"
check "e2e/e21b は exit 2(block)であること" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e29

c_e30() {
# 関数を 1 つだけ欠く(別物を置いた / 名前が変わった形)。構文も rc も正常なので、
# ここだけが「sentinel が 4 関数**すべて**を見ているか」を拘束する。
repo="$(stoplib_repo e21c)"
sed 's/^hooklib_first_stop()/hooklib_renamed_first_stop()/' "$STOPLIB" \
    > "$repo/.claude/hooks/stop-hook-lib.sh"
expect_msg "e2e/e21c stop-hook-lib に関数が欠けたら fail-CLOSED" \
    "stop-hook-lib.sh に hooklib_" "$(run_hook_err "$repo" "$SID")"
}
h_defer c_e30

c_e31() {
# 🛑 ここが `set +e` で囲う理由(§2.4)。素で source すると bash が exit 1 で死に、
# Stop hook のプロトコルでは exit 1 は block しない = 検査が黙って飛ぶ。
repo="$(stoplib_repo e21d)"
{ cat "$STOPLIB" 2>/dev/null; printf '\nfalse\n'; } > "$repo/.claude/hooks/stop-hook-lib.sh"
expect_msg "e2e/e21d stop-hook-lib 内のコマンド失敗で block(exit 1 で素通りしない)" \
    "stop-hook-lib.sh の読み込みに失敗" "$(run_hook_err "$repo" "$SID")"
check "e2e/e21d は exit 2(block)であること" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e31

c_e32() {
repo="$(stoplib_repo e21e)"
{ cat "$STOPLIB" 2>/dev/null; printf '\nif [ ; then\n'; } > "$repo/.claude/hooks/stop-hook-lib.sh"
expect_msg "e2e/e21e stop-hook-lib が構文エラーなら黙って通さない" \
    "syntax error" "$(run_hook_err "$repo" "$SID")"
}
h_defer c_e32

c_e33() {
# 対照: 正常な lib のままなら通る(上の 5 件が「何をしても 2」の嘘の緑でないことの担保)。
repo="$(stoplib_repo e21f)"
check "e2e/e21f 対照: 正常な stop-hook-lib なら通る" "0" "$(run_hook "$repo" "$SID")"
}
h_defer c_e33

c_e34() {
# e19: Tier 1 の untracked 経路。unexported だけ / テスト無しパッケージにすると
# Tier 2 が当たらないので、Tier 1 が untracked を見ているかを単独で測れる。
repo="$(new_repo e19)"
mkdir -p "$repo/backend/internal/domain/foo"
printf 'package foo\n\nfunc helper() int { return 1 }\n' > "$repo/backend/internal/domain/foo/foo.go"
expect_msg "e2e/e19 Tier 1 が untracked を見る" "foo.go にテストが無い" "$(run_hook_err "$repo" "$SID")"
}
h_defer c_e34

c_e35() {
# e20: 逆側。既存ファイルへの unexported だけの変更を「新規」と呼ばない(--diff-filter=A)。
repo="$(new_repo e20)"
mkdir -p "$repo/backend/internal/domain/noz"
printf 'package noz\n\nfunc helper() int { return 1 }\n' > "$repo/backend/internal/domain/noz/noz.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "add noz (no tests)"
git -C "$repo" rev-parse HEAD > "$repo/runtime/logs/session-start-sha.selftest"
: > "$repo/runtime/logs/tdd-check-seen.selftest"
printf 'package noz\n\nfunc helper() int { return 2 }\n' > "$repo/backend/internal/domain/noz/noz.go"
check "e2e/e20 既存ファイルの変更を新規と呼ばない" "0" "$(run_hook "$repo" "$SID")"
}
h_defer c_e35

c_e36() {
# e21: 合成 diff の **1 行目**での偽装(e18 は 4 行目)。
repo="$(new_repo e21)"
printf '++ b/backend/cmd/stockbot/main.go\npackage bar\n\nfunc NewAPI() int { return 9 }\n' \
    > "$repo/backend/internal/domain/bar/spoof2.go"
expect_msg "e2e/e21 合成 diff の 1 行目で偽装できない" "bar/spoof2.go" "$(run_hook_err "$repo" "$SID")"
}
h_defer c_e36

c_e37() {
# e22: 🛑 **実 git diff からの偽装**。追跡ファイルの中身も +/- を前置されるので、
# 中身が `-- ` で始まれば diff 行は `--- `、`++ ` で始まれば `+++ ` になり、
# hunk の中でヘッダのペアがそのまま作れる。合成 diff 側だけ塞いでも無意味。
repo="$(new_repo e22)"
printf 'package bar\n\nconst sql = `\n-- old comment\n`\n' > "$repo/backend/internal/domain/bar/q.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "add q.go"
git -C "$repo" rev-parse HEAD > "$repo/runtime/logs/session-start-sha.selftest"
: > "$repo/runtime/logs/tdd-check-seen.selftest"
printf 'package bar\n\nconst sql = `\n++ b/backend/cmd/stockbot/main.go\n`\n\nfunc BrandNew() int { return 1 }\n' \
    > "$repo/backend/internal/domain/bar/q.go"
expect_msg "e2e/e22 実 diff の中身からも偽装できない" "bar/q.go" "$(run_hook_err "$repo" "$SID")"
}
h_defer c_e37

c_e38() {
# e23: 非 ASCII のファイル名。git は既定(core.quotePath)で C-quote して出すので、
# 先頭が `"` になり `.go` で終わらなくなって tdd_is_exempt に無言で落とされる。
repo="$(new_repo e23)"
mkdir -p "$repo/backend/internal/domain/jp"
printf 'package jp\n\nfunc New() int { return 1 }\n' > "$repo/backend/internal/domain/jp/日本語.go"
expect_msg "e2e/e23 非 ASCII のパスが消えない" "にテストが無い" "$(run_hook_err "$repo" "$SID")"
}
h_defer c_e38

c_e39() {
# e24: git config で diff の接頭辞を変えられても検査が消えない(fail-OPEN させない)。
repo="$(new_repo e24)"
git -C "$repo" config diff.mnemonicPrefix true
git -C "$repo" config diff.external /bin/true
printf '\nfunc Exported() int { return 3 }\n' >> "$repo/backend/internal/domain/bar/bar.go"
expect_msg "e2e/e24 diff の git config で無効化されない" "bar/bar.go" "$(run_hook_err "$repo" "$SID")"
}
h_defer c_e39

c_e40() {
# e25: 🛑 Step 8.5 に**到達するか**を決める early exit(`changed_go`)は Step 8.5 の外にあり、
# そこは素の git を使っている。非 ASCII のパスは既定で C-quote されるので
# `^backend/.*\.go$` に当たらず、**hook ごと素通り**する(arch-guard も check-backend も
# 走らない)。Step 8.5 の中だけ `-c core.quotePath=false` にしても届かない。
# HEAD~..HEAD に Go を含めない形にしないと early exit を通らないので、
# Go を触らないコミットを 1 つ積んでから試す。
repo="$(new_repo e25)"
printf 'note\n' > "$repo/notes.md"
git -C "$repo" add -A && git -C "$repo" commit -qm "docs only"
mkdir -p "$repo/backend/internal/domain/jp"
printf 'package jp\n\nfunc New() int { return 1 }\n' > "$repo/backend/internal/domain/jp/日本語.go"
expect_msg "e2e/e25 非 ASCII だけのセッションが early exit で消えない" "にテストが無い" "$(run_hook_err "$repo" "$SID")"
}
h_defer c_e40

c_e41() {
# e28: early exit の quotePath は untracked(ls-files)だけでなく**作業ツリーの diff**にも要る。
# 追跡済みの非 ASCII ファイルを編集しただけのセッションが素通りしないこと。
repo="$(new_repo e28)"
mkdir -p "$repo/backend/internal/domain/jp"
printf 'package jp\n\nfunc helper() int { return 1 }\n' > "$repo/backend/internal/domain/jp/日本語.go"
printf 'package jp\n\nfunc TestX(t *testing.T) {}\n' > "$repo/backend/internal/domain/jp/jp_test.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "add jp package with test"
printf 'note\n' > "$repo/notes.md"
git -C "$repo" add -A && git -C "$repo" commit -qm "docs only"
printf 'package jp\n\nfunc helper() int { return 1 }\n\nfunc Exported() int { return 2 }\n' \
    > "$repo/backend/internal/domain/jp/日本語.go"
expect_msg "e2e/e28 非 ASCII の作業ツリー変更が early exit で消えない" "追加 / 変更" "$(run_hook_err "$repo" "$SID")"
}
h_defer c_e41

c_e42() {
# e29 / e30: early exit の quotePath は 4 経路ある。e25(untracked)と e28(作業ツリー diff)
# の他に、**直前コミット**(HEAD~..HEAD)と **session 範囲**(sha..HEAD)。
# e29 は「実装 → コミット → Stop」という標準フローそのもの。
repo="$(new_repo e29)"
printf 'note\n' > "$repo/notes.md"
git -C "$repo" add -A && git -C "$repo" commit -qm "docs only"
mkdir -p "$repo/backend/internal/domain/jp"
printf 'package jp\n\nfunc New() int { return 1 }\n' > "$repo/backend/internal/domain/jp/日本語.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "add jp (non-ascii, no test)"
expect_msg "e2e/e29 直前コミットの非 ASCII が early exit で消えない" "にテストが無い" "$(run_hook_err "$repo" "$SID")"
}
h_defer c_e42

c_e43() {
repo="$(new_repo e30)"
mkdir -p "$repo/runtime/logs"
git -C "$repo" rev-parse HEAD > "$repo/runtime/logs/session-start-sha.selftest"
: > "$repo/runtime/logs/tdd-check-seen.selftest"
mkdir -p "$repo/backend/internal/domain/jp"
printf 'package jp\n\nfunc New() int { return 1 }\n' > "$repo/backend/internal/domain/jp/日本語.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "add jp (non-ascii, no test)"
printf 'note\n' > "$repo/notes.md"
git -C "$repo" add -A && git -C "$repo" commit -qm "docs only"
expect_msg "e2e/e30 session 範囲の非 ASCII が early exit で消えない" "にテストが無い" "$(run_hook_err "$repo" "$SID")"
}
h_defer c_e43

c_e44() {
# e31: 🛑 **範囲をまたいだ相殺**。Tier 2 に渡す本文を「作業ツリー」と「session 範囲」の
# 2 本の diff の**連結**にすると、session 範囲の `+宣言行` を作業ツリーの `-宣言行` が
# 相殺する。両者の差が norm() で消える編集(行末コメント / 空白 / gofmt のタブ→スペース)
# だけだと、**正しく出ていた block が黙って消える**。継ぎ目の無い 1 本の diff にすること。
repo="$(new_repo e31)"
mkdir -p "$repo/runtime/logs"
git -C "$repo" rev-parse HEAD > "$repo/runtime/logs/session-start-sha.selftest"
: > "$repo/runtime/logs/tdd-check-seen.selftest"
printf 'package bar\n\nvar BaseURL = "https://api.example.com/v1" // 旧エンドポイント\n' \
    > "$repo/backend/internal/domain/bar/url.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "add exported var"
check "e2e/e31 session 範囲由来は block する" "2" "$(run_hook "$repo" "$SID")"
# 行末コメントを 1 語直すだけ(exported は増えても減ってもいない)
printf 'package bar\n\nvar BaseURL = "https://api.example.com/v1" // 新エンドポイント\n' \
    > "$repo/backend/internal/domain/bar/url.go"
check "e2e/e31 コメント 1 語の手直しで block が消えない" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e44

c_e45() {
# e32: 🛑 **前セッションのコミットが免除に使われる穴**。最初の Stop は `HEAD~..HEAD` を
# 範囲に入れるが、その範囲は Tier 2 の免除判定(「*_test.go を 1 つでも触ったか」)の
# 入力でもある。前セッションの最後のコミットがテストを触っていると、**今セッションの
# 作業ツリー変更が丸ごと免除**される。実リポでは backend を触った 11 コミットが 11 件とも
# テストを触っているので、ほぼ常時この状態になっていた。
# メッセージが「**このセッションで** *_test.go を 1 つも変更していない」と言っているのに
# 免除が前セッションから来ている、という意味論の漏れでもある。
repo="$(new_repo e32)"
printf 'package bar\n\nfunc TestHelper(t *testing.T) {}\n\nfunc TestMore(t *testing.T) {}\n' \
    > "$repo/backend/internal/domain/bar/bar_test.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "previous session: touch tests"
printf '\nfunc Exported() int { return 3 }\n' >> "$repo/backend/internal/domain/bar/bar.go"
expect_msg "e2e/e32 前コミットのテストで今セッションが免除されない" "追加 / 変更" "$(run_hook_err "$repo" "$SID")"
}
h_defer c_e45

c_e46() {
# e33: 逆側の回帰。実装とテストを 1 コミットで入れた直後の Stop は通す(自己完結している)。
repo="$(new_repo e33)"
mkdir -p "$repo/backend/internal/domain/newpkg"
printf 'package newpkg\n\nfunc New() int { return 1 }\n' > "$repo/backend/internal/domain/newpkg/n.go"
printf 'package newpkg\n\nfunc TestNew(t *testing.T) {}\n' > "$repo/backend/internal/domain/newpkg/n_test.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "impl + test in one commit"
check "e2e/e33 実装+テストを 1 コミットで入れた直後は通る" "0" "$(run_hook "$repo" "$SID")"
}
h_defer c_e46

c_e47() {
# e34: 「前コミットが自分でテストを持っているか」の判定でも testdata/ はテストに数えない。
# 数えると、testdata の fixture を触っただけのコミットが「自己完結」扱いになり、
# 同じコミットが足したテスト無しパッケージ(Tier 1)を 1 回きりの検査から取りこぼす。
repo="$(new_repo e34)"
mkdir -p "$repo/backend/internal/domain/bar/testdata" "$repo/backend/internal/domain/newpkg"
printf 'package testdata\n\nfunc TestFixture(t *testing.T) {}\n' \
    > "$repo/backend/internal/domain/bar/testdata/x_test.go"
printf 'package newpkg\n\nfunc helper() int { return 1 }\n' > "$repo/backend/internal/domain/newpkg/n.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "prev: testdata fixture + untested package"
printf '\nfunc more() int { return 2 }\n' >> "$repo/backend/internal/domain/bar/bar.go"
expect_msg "e2e/e34 前コミット判定でも testdata はテストに数えない" "newpkg/n.go にテストが無い" "$(run_hook_err "$repo" "$SID")"
}
h_defer c_e47

c_e48() {
# e35 / e36: 🛑 **検出範囲 ⊇ 免除範囲**。前コミットを「テストを持つから自己完結」と
# みなして**検出からも**外すと、(a) 唯一のテストを消したコミット、(b) テストも触りつつ
# テスト無しパッケージも足した混在コミット、が Tier 1/3 から消える。
# 実リポの backend コミットは 10/10 がテストを触っているので (b) は例外形ではなく標準形で、
# 「直前コミットを見る」機構そのものが死ぬ。免除だけを狭め、検出は広いまま保つこと。
repo="$(new_repo e35)"
git -C "$repo" rm -q backend/internal/domain/bar/bar_test.go
git -C "$repo" commit -qm "remove the only test"
expect_msg "e2e/e35 テストを消したコミットを Tier 3 が見る" "テストが 0 個" "$(run_hook_err "$repo" "$SID")"
}
h_defer c_e48

c_e49() {
repo="$(new_repo e36)"
mkdir -p "$repo/backend/internal/domain/newpkg"
printf 'package bar\n\nfunc TestHelper(t *testing.T) {}\n\nfunc TestMore(t *testing.T) {}\n' \
    > "$repo/backend/internal/domain/bar/bar_test.go"
printf 'package newpkg\n\nfunc helper() int { return 1 }\n' > "$repo/backend/internal/domain/newpkg/n.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "mixed: touch tests + add untested package"
expect_msg "e2e/e36 混在コミットのテスト無しパッケージを Tier 1 が見る" "newpkg/n.go にテストが無い" "$(run_hook_err "$repo" "$SID")"
}
h_defer c_e49

# e37: 🛑 `set -o pipefail` + 末尾 `grep -q` は上流を SIGPIPE(141)で殺し、パイプライン
# 全体を非ゼロにする = 判定が**非決定的に反転**する(plan §1 の地雷リストに明記されている
# のに踏んだ)。前コミットが大きいほど出やすい。10 回流して結果が割れないことを見る
# (正しい実装なら必ず一致するので、この形の揺らぎでしか落ちない)。
# 🛑 10 回は**独立したリポジトリで並列に**流す。同じリポを直列に 10 回流す形だと
# このケース 1 つで 25 秒かかり、`make guard` 全体の臨界パスになっていた(RF3 #8)。
# 各回は runtime/logs を持たない新しいリポなので、直列版で毎回 rm -rf していたのと同じ状態。
e37_tpl="$TMPROOT/_e37tpl"
build_e37_template() {
    cp -r "$TPL" "$e37_tpl"
    mkdir -p "$e37_tpl/backend/internal/domain/bulk"
    e37_i=0
    while [ "$e37_i" -lt 200 ]; do
        printf 'package bulk\n\nfunc TestBulk%d(t *testing.T) {}\n' "$e37_i" \
            > "$e37_tpl/backend/internal/domain/bulk/b${e37_i}_test.go"
        e37_i=$((e37_i + 1))
    done
    git -C "$e37_tpl" add -A && git -C "$e37_tpl" commit -qm "bulk: 200 test files"
    printf '\nfunc Exported() int { return 3 }\n' >> "$e37_tpl/backend/internal/domain/bar/bar.go"
}
build_e37_template

c_e37_run() { # $1 = 何回目か
    local repo="$TMPROOT/e37_$1"
    cp -r "$e37_tpl" "$repo"
    check "e2e/e37#$1 大きい前コミットでも判定が揺らがない" "2" "$(run_hook "$repo" "$SID")"
}
e37_n=1
while [ "$e37_n" -le 10 ]; do
    h_defer c_e37_run "$e37_n"
    e37_n=$((e37_n + 1))
done

c_e51() {
# e38: 前コミット判定の testdata 除外。9 巡目に `tdd_revs` を広げた結果 e34 がこの経路を
# 通らなくなり(Tier 1 のメッセージが tdd_prev_has_test を経由せず出るようになった)、
# 拘束が消えていた = 「テストが実装と別経路を見ている」の再発。exported 追加で経路を固定する。
repo="$(new_repo e38)"
mkdir -p "$repo/backend/internal/domain/bar/testdata"
printf 'package testdata\n\nfunc TestFixture(t *testing.T) {}\n' \
    > "$repo/backend/internal/domain/bar/testdata/x_test.go"
printf '\nfunc Exported() int { return 3 }\n' >> "$repo/backend/internal/domain/bar/bar.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "prev: testdata fixture + exported"
expect_msg "e2e/e38 前コミット判定の testdata 除外" "追加 / 変更" "$(run_hook_err "$repo" "$SID")"
}
h_defer c_e51

c_e52() {
# e39: `tdd_diff_base` の is-ancestor ガード。最初の Stop が docs-only で早期 exit すると
# `session-start-sha` だけ書かれて `tdd-check-seen` は書かれない、という状態が実際に起きる。
# そこで無条件に `HEAD~` へ狭めると session 範囲の Tier 2 が丸ごと落ちる。
repo="$(new_repo e39)"
mkdir -p "$repo/runtime/logs"
git -C "$repo" rev-parse HEAD > "$repo/runtime/logs/session-start-sha.selftest"
printf '\nfunc Exported() int { return 3 }\n' >> "$repo/backend/internal/domain/bar/bar.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "C1: exported, no test"
printf '\nfunc helperMore() int { return 4 }\n' >> "$repo/backend/internal/domain/bar/bar.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "C2: unexported only"
expect_msg "e2e/e39 session 範囲が is-ancestor ガードで落ちない" "追加 / 変更" "$(run_hook_err "$repo" "$SID")"
}
h_defer c_e52

c_e53() {
# e40: 🛑 **secret scan の fail-OPEN**。Step 3 の `echo "$all_added" | grep -q` は、
# 追加行がパイプバッファ(64KB)を超えると `grep -q` の早期 exit で上流が SIGPIPE(141)、
# `set -o pipefail` で条件が偽になり **秘密が 1 文字も変わっていないのに素通り**する。
# A2 由来ではないが 9 巡目に同型を潰した直後に見つかったもので、壊れ方が fail-OPEN。
# この hook の自己テストはここしか無いので A2 のスイートに置く。
# 追加行は `git diff HEAD` から取るので、かさ増しするファイルは**追跡済み**にしてから
# 作業ツリーで膨らませる(untracked のままだと diff に出ず、この経路を通らない)。
repo="$(new_repo e40)"
printf 'package bar\n' > "$repo/backend/internal/domain/bar/bulk.go"
printf 'package bar\n\nfunc TestBulk(t *testing.T) {}\n' > "$repo/backend/internal/domain/bar/bulk_test.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "add bulk.go"
printf '\n// AKIA0123456789ABCDEF\n' >> "$repo/backend/internal/domain/bar/bar.go"
{ i=0; while [ "$i" -lt 20000 ]; do printf '// filler line %d\n' "$i"; i=$((i + 1)); done; } \
    >> "$repo/backend/internal/domain/bar/bulk.go"
check "e2e/e40 大量の追加行があっても secret を見落とさない" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e53

c_e54() {
# e26: Tier 3 の e2e。唯一のテストを消すと block する。
repo="$(new_repo e26)"
git -C "$repo" rm -q backend/internal/domain/bar/bar_test.go
expect_msg "e2e/e26 唯一の _test.go を消すと block" "テストが 0 個" "$(run_hook_err "$repo" "$SID")"
}
h_defer c_e54

c_e55() {
# e27: Tier 2 のメッセージ文言。「追加」と言い切らない(相殺は行単位なので整形・値変更でも出る)。
repo="$(new_repo e27)"
printf '\nfunc Exported() int { return 3 }\n' >> "$repo/backend/internal/domain/bar/bar.go"
expect_msg "e2e/e27 Tier 2 の文言は「追加 / 変更」" "追加 / 変更" "$(run_hook_err "$repo" "$SID")"
}
h_defer c_e55

# T12: 無条件の `HEAD~..HEAD` を外す(セッション開始 SHA が SessionStart 由来なら不要)。
# H1 が入るまでは「作業ツリー clean・当セッションで無変更」でも直前コミットのファイルが
# 変更一覧に載り、Step 5 の early exit を抜けて make check-backend まで走っていた。
# 直前コミットが repository/ を触っていると Step 10 が INTEGRATION_TEST_DB_URL を要求し、
# **何もしていないターンで 3 連続ブロック**された(直近 20 コミット中 8 件が該当)。
seed_session_sha() { # $1 = repo; SessionStart(H1)が置いた 2 つの印を模す
    mkdir -p "$1/runtime/logs"
    git -C "$1" rev-parse HEAD > "$1/runtime/logs/session-start-sha.selftest"
    # 🛑 印は**空ファイルではなく SHA を持つ**。空にすると、Stop 配列 2 番目の
    # docs-sync-check.sh が pre-stop の backfill を開始点と誤読する(T12 2 巡目レビュー)。
    git -C "$1" rev-parse HEAD > "$1/runtime/logs/session-start-mark.selftest"
}

c_e56() {
# e41: 直前コミットが backend/**.go を触っている + 作業ツリー clean + 印あり → early exit。
repo="$(new_repo e41)"
seed_session_sha "$repo"
check "e2e/e41 無変更ターンは直前コミットを見ない" "0" "$(run_hook "$repo" "$SID")"
check "e2e/e41b early exit している(exit code だけでは判別できない)" "ok" \
    "$(grep -q "no backend Go or migration changes — skip" \
        "$repo/runtime/logs/pre-stop-checks.log" && echo ok || echo missing)"
}
h_defer c_e56

c_e57() {
# e42: T12 の最も深刻な形。直前コミットが repository/ を触っていると、無変更ターンでも
# Step 10 が INTEGRATION_TEST_DB_URL を要求して block していた。
repo="$(new_repo e42)"
mkdir -p "$repo/backend/internal/adapter/repository"
printf 'package repository\n' > "$repo/backend/internal/adapter/repository/x.go"
printf 'package repository\n\nfunc TestX(t *testing.T) {}\n' > "$repo/backend/internal/adapter/repository/x_test.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "touch repository"
seed_session_sha "$repo"
check "e2e/e42 無変更ターンで DB integration を要求しない" "0" "$(run_hook "$repo" "$SID")"
}
h_defer c_e57

c_e58() {
# e43: 印が無い(SessionStart が動かなかった)ときは従来どおり直前コミットを見る。
# ここを落とすと、H1 が失敗した環境で検査範囲が黙って縮む = fail-OPEN になる。
repo="$(new_repo e43)"
mkdir -p "$repo/backend/internal/adapter/repository"
printf 'package repository\n' > "$repo/backend/internal/adapter/repository/x.go"
printf 'package repository\n\nfunc TestX(t *testing.T) {}\n' > "$repo/backend/internal/adapter/repository/x_test.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "touch repository"
check "e2e/e43 印が無ければ従来どおり直前コミットを見る" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e58

c_e59() {
# e43b: 🛑 sha だけあって mark が無い = pre-stop-checks 自身が前の Stop で作った状態。
# ここを「SessionStart が動いた」と誤読すると直前コミットが検査から黙って落ちる(fail-OPEN)。
repo="$(new_repo e43b)"
mkdir -p "$repo/backend/internal/adapter/repository"
printf 'package repository\n' > "$repo/backend/internal/adapter/repository/x.go"
printf 'package repository\n\nfunc TestX(t *testing.T) {}\n' > "$repo/backend/internal/adapter/repository/x_test.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "touch repository"
mkdir -p "$repo/runtime/logs"; git -C "$repo" rev-parse HEAD > "$repo/runtime/logs/session-start-sha.selftest"
check "e2e/e43b sha だけで mark が無ければ従来どおり" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e59

c_e60() {
# e44: 印があってもセッション中にコミットしていれば、その範囲は当然見る。
repo="$(new_repo e44)"
seed_session_sha "$repo"
mkdir -p "$repo/backend/internal/domain/foo"
printf 'package foo\n\nfunc New() int { return 1 }\n' > "$repo/backend/internal/domain/foo/foo.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "session commit without test"
check "e2e/e44 セッション中のコミットは見る" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e60

c_e61() {
# e45: 印が壊れている(解決できない SHA)ときも従来どおりに倒す(fail-CLOSED)。
repo="$(new_repo e45)"
mkdir -p "$repo/backend/internal/adapter/repository"
printf 'package repository\n' > "$repo/backend/internal/adapter/repository/x.go"
printf 'package repository\n\nfunc TestX(t *testing.T) {}\n' > "$repo/backend/internal/adapter/repository/x_test.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "touch repository"
mkdir -p "$repo/runtime/logs"; echo "0000000000000000000000000000000000000000" > "$repo/runtime/logs/session-start-sha.selftest"
check "e2e/e45 印が解決できなければ従来どおり" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e61

c_e62() {
# e46: changed_files を絞るだけでは足りない — A2 の tier 範囲(tdd_revs / tdd_diff_base)も
# 最初の Stop で HEAD~..HEAD を足すので、そこを直さないと**前セッションのコミットで
# 今セッションが block される**形が残る(§5 A1/A2 が「H1 で消える」と書いた穴)。
repo="$(new_repo e46)"
mkdir -p "$repo/backend/internal/domain/prevpkg"
printf 'package prevpkg\n\nfunc New() int { return 1 }\n' > "$repo/backend/internal/domain/prevpkg/prevpkg.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "prev session: テスト無しパッケージ"
seed_session_sha "$repo"
printf '\nfunc another() int { return 2 }\n' >> "$repo/backend/internal/domain/bar/bar.go"
check "e2e/e46 tier 範囲にも直前コミットを入れない" "0" "$(run_hook "$repo" "$SID")"
}
h_defer c_e62

c_e63() {
# e46b: 同じ状況で印が無ければ従来どおり block する(e46 が「検査ごと消した」のではないこと)。
repo="$(new_repo e46b)"
mkdir -p "$repo/backend/internal/domain/prevpkg"
printf 'package prevpkg\n\nfunc New() int { return 1 }\n' > "$repo/backend/internal/domain/prevpkg/prevpkg.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "prev session: テスト無しパッケージ"
printf '\nfunc another() int { return 2 }\n' >> "$repo/backend/internal/domain/bar/bar.go"
check "e2e/e46b 印が無ければ従来どおり block" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e63

c_e64() {
# e47: 🛑 mark はあるが sha が無い。ここを mark だけで判定すると、下の backfill が
# **今の HEAD** を書いて範囲が空になり、`HEAD~..HEAD` も外れて検査が丸ごと消える
# (GC / 手動削除 / 古い sid の --resume で到達する)。両方を要求すること。
repo="$(new_repo e47)"
mkdir -p "$repo/backend/internal/adapter/repository"
printf 'package repository\n' > "$repo/backend/internal/adapter/repository/x.go"
printf 'package repository\n\nfunc TestX(t *testing.T) {}\n' > "$repo/backend/internal/adapter/repository/x_test.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "touch repository"
mkdir -p "$repo/runtime/logs"; : > "$repo/runtime/logs/session-start-mark.selftest"
check "e2e/e47 印が空なら従来どおり" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e64

c_e65() {
# e48: mark はあるが sha が解決できない。`session_range_ok` を条件から落とすと素通りする
# (レビューの生存変異 M10)。
repo="$(new_repo e48)"
mkdir -p "$repo/backend/internal/adapter/repository"
printf 'package repository\n' > "$repo/backend/internal/adapter/repository/x.go"
printf 'package repository\n\nfunc TestX(t *testing.T) {}\n' > "$repo/backend/internal/adapter/repository/x_test.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "touch repository"
mkdir -p "$repo/runtime/logs"
echo "0000000000000000000000000000000000000000" > "$repo/runtime/logs/session-start-mark.selftest"
check "e2e/e48 印の SHA が解決できなければ従来どおり" "2" "$(run_hook "$repo" "$SID")"
}
h_defer c_e65

c_e66() {
# e49: 🛑 e45/e47/e48 は repository/ を触るので **Step 10 で先に block** し、A2 の経路に
# 到達しない。そのため `tdd_prev_in_scope` 側の `session_range_ok` が無拘束だった
# (2 巡目レビューの生存変異 M5)。repository/ を避けて A2 でだけ止まる形を作る。
repo="$(new_repo e49)"
mkdir -p "$repo/backend/internal/domain/prevpkg"
printf 'package prevpkg\n\nfunc New() int { return 1 }\n' > "$repo/backend/internal/domain/prevpkg/prevpkg.go"
git -C "$repo" add -A && git -C "$repo" commit -qm "prev session: テスト無しパッケージ"
mkdir -p "$repo/runtime/logs"
echo "0000000000000000000000000000000000000000" > "$repo/runtime/logs/session-start-mark.selftest"
printf '\nfunc another() int { return 2 }\n' >> "$repo/backend/internal/domain/bar/bar.go"
expect_msg "e2e/e49 印の SHA が解決できなければ A2 の範囲も従来どおり" "テスト" "$(run_hook_err "$repo" "$SID")"
}
h_defer c_e66

# e50x (RF3 #6): enforcement のパス集合を 1 か所から読む。
# 🛑 同じ概念の regex が Step 4(git のパス一覧に当てる・行頭固定)と T5(コマンド文字列に
# 当てる・任意位置)で **2 綴り**になり、`Makefile` の扱いが実際に割れていた。
# 中核の集合を `enforcement-paths.sh` の 1 か所に置き、2 つの形はそこから導く。

enf_repo() { # $1 = case id -> A2 的に違反ゼロ(= 他の理由で block しない)リポを echo
    local repo; repo="$(new_repo "$1")"
    echo "$repo"
}
# Step 4 は warn-once なので **1 回目**の stderr を見る。
enf_msg() { # $1 = repo -> 1 回目の stderr
    run_hook_err "$1" "$SID"
}

c_enf_covers() { # $1 = case id, $2 = 相対パス
    local repo; repo="$(enf_repo "$1")"
    mkdir -p "$(dirname "$repo/$2")"
    printf '\n# enforcement change\n' >> "$repo/$2"
    expect_msg "e2e/enf $2 は enforcement として警告される" \
        "ENFORCEMENT FILES CHANGED" "$(enf_msg "$repo")"
}
h_defer c_enf_covers enf_makefile   "Makefile"
h_defer c_enf_covers enf_archguard  "scripts/arch-guard.sh"
h_defer c_enf_covers enf_secretscan "scripts/secret-scan.sh"
h_defer c_enf_covers enf_precommit  "scripts/pre-commit.sh"
h_defer c_enf_covers enf_githooks   ".githooks/pre-push"
h_defer c_enf_covers enf_settings   ".claude/settings.json"
h_defer c_enf_covers enf_settingsl  ".claude/settings.local.json"
# `.*` を 1 行足すだけで docs-sync 検査を全無効化できるので、allowlist も enforcement。
h_defer c_enf_covers enf_allowlist  ".claude/hooks/spec-sync-allowlist.txt"

# 🛑 `$( )` の中に `case` を書かない。macOS の bash 3.2 は解析できず、最初の `)` で
# 打ち切って残りをリテラルとして返す(§2.3 の地雷。ここで実際に踏んだ)。
enf_says() { # $1 = 語, $2 = 本文 -> present / absent
    case "$2" in
        *"$1"*) echo present ;;
        *) echo absent ;;
    esac
}

# 逆側: 普通のファイルで enforcement 警告を出さない(出すと毎回 warn されて無視される)。
c_enf_not_covers() {
    local repo; repo="$(enf_repo enf_plain)"
    mkdir -p "$repo/docs"
    printf 'note\n' > "$repo/docs/notes.md"
    check "e2e/enf 普通の docs 変更は enforcement 扱いしない" "absent" \
        "$(enf_says "ENFORCEMENT FILES CHANGED" "$(enf_msg "$repo")")"
}
h_defer c_enf_not_covers

# 🛑 定義ファイルが欠けたら fail-CLOSED(黙って「enforcement 対象ゼロ」に縮退しない)。
c_enf_lib_missing() {
    local repo; repo="$(enf_repo enf_lib_missing)"
    rm -f "$repo/.claude/hooks/enforcement-paths.sh"
    expect_msg "e2e/enf 定義ファイル欠落は fail-CLOSED" \
        "enforcement-paths.sh が見つからない" "$(enf_msg "$repo")"
}
h_defer c_enf_lib_missing

c_enf_lib_empty() {
    local repo; repo="$(enf_repo enf_lib_empty)"
    : > "$repo/.claude/hooks/enforcement-paths.sh"
    expect_msg "e2e/enf 定義ファイルが空でも fail-CLOSED" \
        "enforcement-paths.sh に ENF_" "$(enf_msg "$repo")"
}
h_defer c_enf_lib_empty

c_enf_lib_false() {
    local repo; repo="$(enf_repo enf_lib_false)"
    { cat "$ENFLIB" 2>/dev/null; printf '\nfalse\n'; } > "$repo/.claude/hooks/enforcement-paths.sh"
    expect_msg "e2e/enf 定義ファイルの読み込み失敗は fail-CLOSED" \
        "enforcement-paths.sh の読み込みに失敗" "$(enf_msg "$repo")"
}
h_defer c_enf_lib_false

# H1 (2026-10-01): 最後に緑だった作業ツリーと同じなら make check-backend / test-integration を
# 再実行しない。セッション範囲(開始 SHA..HEAD)は Go を 1 度 commit すると毎ターン載るので、
# 何もしていないターンでも -race の全テストが走っていた(clean tree で 14 秒)。
# 🛑 鍵は「何を変えたら再実行されるか」で拘束する。HEAD・追跡ファイル・untracked・
# gitignore された configs/ のどれか 1 つでも鍵から漏れると、古い緑で新しいコードが通る。
cache_repo() { # $1 = case id -> make の実行回数を runtime/logs/*-count に数えるリポを echo
    local repo; repo="$(new_repo "$1")"
    printf 'check-backend:\n\t@echo x >> runtime/logs/cb-count; test ! -f runtime/logs/cb-fail\ntest-integration:\n\t@echo x >> runtime/logs/it-count\n' > "$repo/Makefile"
    printf '/runtime/\n/configs/*.live.yaml\n' > "$repo/.gitignore"
    mkdir -p "$repo/configs"
    printf 'a: 1\n' > "$repo/configs/hard_limits.yaml"
    git -C "$repo" add -A && git -C "$repo" commit -qm "cache base"
    mkdir -p "$repo/runtime/logs" "$repo/backend/internal/domain/foo"
    # Makefile の変更を範囲から外す(Step 4 の warn-once に当たらないように)。
    git -C "$repo" rev-parse HEAD > "$repo/runtime/logs/session-start-mark.selftest"
    printf 'package foo\n\nfunc New() int { return 1 }\n' > "$repo/backend/internal/domain/foo/foo.go"
    printf 'package foo\n\nfunc TestNew(t *testing.T) {}\n' > "$repo/backend/internal/domain/foo/foo_test.go"
    echo "$repo"
}
cache_count() { # $1 = repo, $2 = cb / it -> 回数
    wc -l < "$1/runtime/logs/$2-count" 2>/dev/null | tr -d ' ' || echo 0
}

c_h1_same_tree() {
    local repo; repo="$(cache_repo h1_same)"
    run_hook "$repo" "$SID" >/dev/null
    check "e2e/h1 同じ作業ツリーの 2 回目は通る" "0" "$(run_hook "$repo" "$SID")"
    check "e2e/h1 同じ作業ツリーなら check-backend を再実行しない" "1" "$(cache_count "$repo" cb)"
}
h_defer c_h1_same_tree

c_h1_tracked() {
    local repo; repo="$(cache_repo h1_tracked)"
    run_hook "$repo" "$SID" >/dev/null
    printf '\n// tracked edit\n' >> "$repo/backend/internal/domain/bar/bar.go"
    run_hook "$repo" "$SID" >/dev/null
    check "e2e/h1 追跡ファイルを変えたら再実行する" "2" "$(cache_count "$repo" cb)"
}
h_defer c_h1_tracked

c_h1_untracked() {
    local repo; repo="$(cache_repo h1_untracked)"
    run_hook "$repo" "$SID" >/dev/null
    printf '\n// untracked edit\n' >> "$repo/backend/internal/domain/foo/foo_test.go"
    run_hook "$repo" "$SID" >/dev/null
    check "e2e/h1 untracked の中身を変えたら再実行する" "2" "$(cache_count "$repo" cb)"
}
h_defer c_h1_untracked

c_h1_head() {
    local repo; repo="$(cache_repo h1_head)"
    # 作業ツリーは 2 回とも clean で、違うのは HEAD だけ(diff と untracked では区別できない形)。
    git -C "$repo" add -A && git -C "$repo" commit -qm "commit foo"
    run_hook "$repo" "$SID" >/dev/null
    printf '\n// after commit\n' >> "$repo/backend/internal/domain/foo/foo.go"
    git -C "$repo" commit -qam "second commit"
    run_hook "$repo" "$SID" >/dev/null
    check "e2e/h1 HEAD が動いたら再実行する" "2" "$(cache_count "$repo" cb)"
}
h_defer c_h1_head

c_h1_ignored_config() {
    local repo; repo="$(cache_repo h1_config)"
    printf 'lot: 100\n' > "$repo/configs/bot_config.live.yaml"
    run_hook "$repo" "$SID" >/dev/null
    printf 'lot: 200\n' > "$repo/configs/bot_config.live.yaml"
    run_hook "$repo" "$SID" >/dev/null
    check "e2e/h1 gitignore された configs/ を変えたら再実行する" "2" "$(cache_count "$repo" cb)"
}
h_defer c_h1_ignored_config

c_h1_red_not_cached() {
    local repo; repo="$(cache_repo h1_red)"
    : > "$repo/runtime/logs/cb-fail"
    check "e2e/h1 check-backend が赤なら block" "2" "$(run_hook "$repo" "$SID")"
    run_hook "$repo" "$SID" >/dev/null
    check "e2e/h1 赤は記録しない(同じツリーでも再実行する)" "2" "$(cache_count "$repo" cb)"
}
h_defer c_h1_red_not_cached

c_h1_integration() {
    local repo; repo="$(cache_repo h1_it)"
    mkdir -p "$repo/backend/internal/adapter/repository"
    printf 'package repository\n\nfunc R() int { return 1 }\n' > "$repo/backend/internal/adapter/repository/r.go"
    printf 'package repository\n\nfunc TestR(t *testing.T) {}\n' > "$repo/backend/internal/adapter/repository/r_test.go"
    run_hook "$repo" "$SID" "INTEGRATION_TEST_DB_URL=postgres://x/y_test" >/dev/null
    check "e2e/h1 integration も同じツリーの 2 回目は通る" "0" \
        "$(run_hook "$repo" "$SID" "INTEGRATION_TEST_DB_URL=postgres://x/y_test")"
    check "e2e/h1 同じ作業ツリーなら test-integration を再実行しない" "1" "$(cache_count "$repo" it)"
}
h_defer c_h1_integration

# D10 (2026-10-01): migration の履歴表は DATA_MODEL に一本化した。docs の要求はここだけが持つ
# (docs-sync-check.sh の同じ検査は H2 で外した)ので、DATA_MODEL を要求することと
# MIGRATIONS を要求しないことの両側をここで拘束する。
mig_repo() { # $1 = case id -> 0001 の up/down を untracked に置いたリポを echo
    local repo; repo="$(new_repo "$1")"
    printf 'check-backend:\n\t@true\ntest-integration:\n\t@true\n' > "$repo/Makefile"
    git -C "$repo" add -A && git -C "$repo" commit -qm "mig base"
    mkdir -p "$repo/runtime/logs" "$repo/migrations"
    git -C "$repo" rev-parse HEAD > "$repo/runtime/logs/session-start-mark.selftest"
    printf 'CREATE TABLE t (id int);\n' > "$repo/migrations/0001_init.up.sql"
    printf 'DROP TABLE t;\n' > "$repo/migrations/0001_init.down.sql"
    echo "$repo"
}
mig_err() { # $1 = repo -> stderr
    local repo="$1" ef="$TMPROOT/mig.err.${H_CASE_ID}"
    printf '%s' "$SID" | ( cd "$repo" && env STOCKBOT_PRESTOP_CHECKS=on STOCKBOT_TDD_CHECK=on \
        INTEGRATION_TEST_DB_URL=postgres://x/y_test CLAUDE_PROJECT_DIR="$repo" \
        bash "$repo/.claude/hooks/pre-stop-checks.sh" >/dev/null 2>"$ef" ) || true
    cat "$ef"
}
c_d10_needs_datamodel() {
    local repo; repo="$(mig_repo d10_needs)"
    expect_msg "e2e/d10 migration だけ変えたら DATA_MODEL を要求する" \
        "DATA_MODEL.md が未更新" "$(mig_err "$repo")"
}
h_defer c_d10_needs_datamodel

c_d10_datamodel_only() {
    local repo; repo="$(mig_repo d10_dm)"
    mkdir -p "$repo/docs/runtime"
    printf '# DATA_MODEL\n' > "$repo/docs/runtime/DATA_MODEL.md"
    check "e2e/d10 DATA_MODEL だけ直せば通る(MIGRATIONS は要求しない)" "" "$(mig_err "$repo")"
}
h_defer c_d10_datamodel_only

h_run_deferred
h_tally pre-stop-tdd_test
