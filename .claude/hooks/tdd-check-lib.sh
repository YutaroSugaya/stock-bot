#!/usr/bin/env bash
# A2: 「テストが存在すること」だけを機械強制する判定ロジック(pre-stop-checks.sh Step 8.5)。
#
# 強制するのは存在まで。**Red を先に見た順序は事後の diff から復元できないので強制しない**
# (CLAUDE.md の strict TDD と pr-merge-check スキルが担う)。アサートが意味を持つかも見ない。
# ここを曖昧にすると「緑だから TDD できている」と誤読される。
#
# 副作用を持たない(関数定義のみ)ので、pre-stop-checks.sh からも
# 自己テスト .claude/hooks/pre-stop-tdd_test.sh からも source できる。
# git には触らない — 何を拾うかは呼び出し側の責務で、e2e 側でテストする。

# 判定対象外なら 0 を返す。
tdd_is_exempt() { # $1 = path
    case "$1" in
        *_test.go)                    return 0 ;;
        backend/cmd/*)                return 0 ;;  # 配線コード
        backend/internal/testutil/*)  return 0 ;;  # テスト支援そのもの
        */testdata/*)                 return 0 ;;  # Go はビルドもテストもしない領域
        *_string.go|*.pb.go|*_gen.go|*_generated.go) return 0 ;;
        backend/*.go)                 return 1 ;;
        *)                            return 0 ;;
    esac
}

# 同一ディレクトリ(= Go のパッケージ単位)に *_test.go があれば 0。
tdd_pkg_has_test() { # $1 = path
    local dir f
    dir="$(dirname "$1")"
    for f in "$dir"/*_test.go; do
        [ -e "$f" ] && return 0
    done
    return 1
}

# unified diff(stdin)から「exported 宣言を追加している」ファイルのパスを列挙する。
# 括りの中(grouped const / struct フィールド / interface メソッド)は**見ない** —
# docs-sync の Tier 1 と違い、ここは「テストが 1 つも無い」を叩く強い block なので、
# 誤検知を増やすより取りこぼす側に倒す(誤検知するガードは必ず無視されるようになる)。
#
# 同じ宣言行が - 側にも現れたら相殺する(A1 の exported_touched と同じ考え方)。
# 相殺しないと**ファイル分割 / rename** が「exported を追加している」という事実と違う
# メッセージで block される。相殺は行単位なので、シグネチャ変更(`int` → `int64`)は
# 別の行として残り、契約変更は取りこぼさない。
tdd_files_adding_exported() {
    awk '
        # 行末コメントを落とす。**引用の外の `//` だけ**を見る —
        # 「`"` を見たら諦める」形だと、文字列を含む宣言行のコメントだけを直したときに
        # 相殺できず誤 block する(「コメントだけの変更では止めない」という意図が、
        # 文字列を含む行でだけ死んでいた)。生文字列の中の // もコメントではない。
        function strip_comment(line,   i, n, c, q) {
            n = length(line)
            for (i = 1; i <= n; i++) {
                c = substr(line, i, 1)
                if (q == "\"") {
                    if (c == "\\") { i++; continue }
                    if (c == "\"") q = ""
                    continue
                }
                if (q == "`") { if (c == "`") q = ""; continue }
                if (c == "\"" || c == "`") { q = c; continue }
                if (c == "\047") {              # rune literal: '\047' = シングルクォート
                    i++
                    if (substr(line, i, 1) == "\\") i++
                    i++
                    continue
                }
                if (c == "/" && substr(line, i + 1, 1) == "/") return substr(line, 1, i - 1)
            }
            return line
        }
        function norm(raw,   line) {
            line = strip_comment(raw)
            gsub(/[ \t]+/, " ", line)
            sub(/^ +/, "", line); sub(/ +$/, "", line)
            return line
        }
        # ファイルヘッダは**直前が `--- ` かつ hunk の外**のときだけ採用する。
        # 中身の行は diff で `+` / `-` を前置されるので、`-- x` の削除は `--- x`、
        # `++ b/…` の追加は `+++ b/…` になり、**hunk の中でヘッダのペアがそのまま作れる**。
        # `--- ` は中身から作れるが、`@@`(hunk 開始)と `diff --git`(ファイル境界)の
        # 並びは作れないので、そこで区別する。**`in_hunk` を落とすのは `diff ` 行だけ**
        # (`--- ` で落とすと中身から hunk を抜けたことにできてしまう)。
        # パスは `$2` ではなくヘッダ全体から取る。`$2` だと空白入りのパスが切り詰められ、
        # `.go` で終わらなくなって tdd_is_exempt に無言で落とされる。
        /^\+\+\+ / {
            if (prev_minus && !in_hunk) {
                path = $0
                sub(/^\+\+\+ /, "", path)
                sub(/\t.*$/, "", path)
                sub(/^b\//, "", path)
            }
            prev_minus = 0
            next
        }
        /^--- / { prev_minus = 1; next }
        /^@@/   { in_hunk = 1; prev_minus = 0; next }
        /^diff / { in_hunk = 0; prev_minus = 0; next }
        /^index / { prev_minus = 0; next }
        {
            prev_minus = 0
            sign = substr($0, 1, 1)
            if (sign != "+" && sign != "-") next
            line = norm(substr($0, 2))
            if (line !~ /^(func|type|const|var) [A-Z]/ && line !~ /^func \([^)]*\) [A-Z]/) next
            if (sign == "+") adds[path SUBSEP line] = 1; else dels[line] = 1
        }
        END {
            for (k in adds) {
                split(k, a, SUBSEP)
                if (!(a[2] in dels)) print a[1]
            }
        }
    ' | sort -u
}

# 違反を 1 行 1 件で stdout に出す。常に 0 で返る(判定であって制御ではない)。
#   $1 = このセッション範囲で新規追加されたファイル一覧(改行区切り)
#   $2 = 同範囲で変更された全ファイル一覧(改行区切り)
#   $3 = 同範囲の unified diff(`+++ b/<path>` ヘッダ必須)
# パッケージにテストがあるかは cwd 起点のファイルシステムで見る(untracked な
# _test.go も数えたいので、git の索引ではなく実体を見る)。
# $4(任意)= Tier 2 の**免除**だけに使う変更一覧。省略時は $2。
# 🛑 検出範囲と免除範囲は **検出 ⊇ 免除**。同じにすると、前セッションのコミットを
# 免除から外すために検出からも外すことになり、そのコミットが「唯一のテストを消した」
# 「テストも触りつつテスト無しパッケージを足した」ときに Tier 1/3 が盲目になる。
tdd_violations() {
    local added="$1" changed="$2" diff_text="$3"
    local exempt_src
    if [ "$#" -ge 4 ]; then exempt_src="$4"; else exempt_src="$changed"; fi
    local f test_changed exported_files

    [ "${STOCKBOT_TDD_CHECK:-on}" = "on" ] || return 0

    # Tier 1 — 新規追加された非テスト .go のパッケージにテストが 1 つも無い。
    while IFS= read -r f; do
        [ -z "$f" ] && continue
        tdd_is_exempt "$f" && continue
        tdd_pkg_has_test "$f" && continue
        echo "新規 $f にテストが無い(パッケージ $(dirname "$f") に *_test.go が 1 つも無い) → 例: $(dirname "$f")/$(basename "$f" .go)_test.go"
    done <<< "$added"

    # Tier 3 — このセッションで *_test.go が消え、パッケージのテストが 0 個になった。
    # 「テストの存在」を最も直接に壊す形なのに、Tier 1(新規ファイルだけが対象)にも
    # Tier 2(テストを 1 つでも触れば免除)にも当たらず、**消せば黙る**状態だった。
    # Tier 2 の早期 return より前に置く(消すこと自体が「触った」に数えられてしまうため)。
    # 照合は完全一致(改行区切り)。空白区切りの部分文字列照合だと、空白を含む
    # ディレクトリ名で「短い方が長い方に含まれる」ため違反が黙って消える。
    local d g seen_dirs="" has_src
    while IFS= read -r f; do
        [ -z "$f" ] && continue
        case "$f" in *_test.go) ;; *) continue ;; esac
        d="$(dirname "$f")"
        printf '%s\n' "$seen_dirs" | grep -Fqx "$d" && continue
        seen_dirs="$seen_dirs$d
"
        tdd_pkg_has_test "$d/x.go" && continue  # テストがまだ残っている(修正 / rename)
        # パッケージごと消えた場合はディレクトリが無く、下の glob が 1 件も返さないので
        # has_src=0 のまま落ちる(`[ -d "$d" ]` の事前チェックは等価だったので置かない)。
        has_src=0
        for g in "$d"/*.go; do
            [ -e "$g" ] || continue
            tdd_is_exempt "$g" && continue
            has_src=1
            break
        done
        [ "$has_src" = "1" ] && echo "$d のコードは残っているのにテストが 0 個になった($f が消えた)"
    done <<< "$changed"

    # Tier 2 — exported を増やしたのに、このセッションでテストを 1 つも触っていない。
    # testdata/ の *_test.go は Go が絶対に走らせない(除外側と同じ理由)。
    # 「テストに数えない」と宣言した領域が、免除にだけ数えられる矛盾を残さない。
    test_changed="$(printf '%s\n' "$exempt_src" | grep -E '_test\.go$' | grep -v '/testdata/' || true)"
    [ -n "$test_changed" ] && return 0
    exported_files="$(printf '%s' "$diff_text" | tdd_files_adding_exported || true)"
    while IFS= read -r f; do
        [ -z "$f" ] && continue
        tdd_is_exempt "$f" && continue
        # Tier 1 が既に報告したものだけスキップする(同じ変更を 2 回報告しない)。
        # 「新規追加なら全部 Tier 2 の対象外」にすると、**テストのあるパッケージへ新規
        # ファイルを足す**という機能追加の最も普通の形が、exported を何個増やしても
        # 素通りする(Tier 1 はパッケージにテストがあるので通す)。
        { printf '%s\n' "$added" | grep -Fqx "$f" && ! tdd_pkg_has_test "$f"; } && continue
        # 「追加」と言い切らない — 相殺は行単位なので、宣言行の**変更**(シグネチャ / 定数値 /
        # 1 行 → 複数行の整形)も残る。block 自体は妥当だが、事実と違う文言で止めない。
        echo "$f が exported 宣言行を追加 / 変更しているが、このセッションで *_test.go を 1 つも変更していない"
    done <<< "$exported_files"

    return 0
}
