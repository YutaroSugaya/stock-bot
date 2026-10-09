#!/usr/bin/env bash
# pre-push-scope.sh — pre-push が hook の自己テストを全件回すかを決める(REVIEW_TASKS H5・決定 D-10)。
# source して使う。enforcement-paths.sh を先に source しておくこと(ENF_PATH_RE / ENF_TEST_PATH_RE)。
#
# 決定 D-10: push の範囲に enforcement(hooks / settings / githooks / Makefile / guard scripts /
# scripts の自己テスト)の変更があるときだけ全件。加えて、前回の全件から 7 日たっていれば全件。
# 自己テストは全件で 2 分近くかかり、push のたびに回すと重い。一方で hook を変えていなくても
# 環境要因で赤くなることがある(2026-08-21 の GIT_DIR 事故)ので、定期の全件は残す。
#
# 🛑 **判定できないときは全件に倒す**(fail-close)。ここが誤って「省略」を返すと、hook を
# 壊した push が自己テストを通らずに入る。範囲が読めない・コミットが解けない・記録が壊れている、
# はすべて全件。
# 🛑 `--no-renames` で見る。rename 検出が効くと、hook を外へ移す変更が「移し先」だけになり
# 範囲から消える。
# 🛑 bash 3.2 の語彙に限定。

PREPUSH_STAMP_TTL=$((7 * 24 * 60 * 60))

# prepush_full_reason <記録ファイル> <今の epoch> <有効期限(秒)>
# stdin は git が pre-push に渡す行(<local ref> <local sha> <remote ref> <remote sha>)。
# 全件が要るなら理由を 1 行出して 0、要らなければ理由を 1 行出して 1 を返す。
prepush_full_reason() {
    local stamp="$1" now="$2" ttl="$3"
    local input lref lsha rref rsha files changed="" any=0 last age
    # 🛑 **stdin は最初に読み切る。**読まずに早く返すと、呼び出し側の `printf … |` が SIGPIPE で
    # 落ち、pipefail の下では「全件」の判定ごと失敗扱いになる(= 省略に倒れる。自己テスト実測)。
    input="$(cat)"
    if [ "${STOCKBOT_PREPUSH_FULL:-}" = "1" ]; then
        echo "STOCKBOT_PREPUSH_FULL=1"
        return 0
    fi
    if [ -z "${ENF_PATH_RE:-}" ] || [ -z "${ENF_TEST_PATH_RE:-}" ]; then
        echo "enforcement の集合(enforcement-paths.sh)を読めない"
        return 0
    fi
    while read -r lref lsha rref rsha; do
        [ -n "$lsha" ] || continue
        # 削除の push(local が 0 埋め)は持ち込むコミットが無い。
        printf '%s' "$lsha" | grep -q '[^0]' || continue
        any=1
        if [ -z "$rsha" ]; then
            echo "push の行が読めない($lref)"
            return 0
        fi
        if printf '%s' "$rsha" | grep -q '[^0]'; then
            files="$(git log --no-renames --format= --name-only "$rsha..$lsha" 2>/dev/null)" \
                || { echo "範囲を解けない($rsha..$lsha)"; return 0; }
        else
            # 新しいブランチ。どのリモートにも無いコミットだけを見る。
            files="$(git log --no-renames --format= --name-only "$lsha" --not --remotes 2>/dev/null)" \
                || { echo "範囲を解けない($lsha)"; return 0; }
        fi
        changed="$changed
$(printf '%s\n' "$files" | grep -E "$ENF_PATH_RE|$ENF_TEST_PATH_RE" || true)"
    done <<< "$input"
    if [ "$any" -eq 0 ]; then
        echo "push の範囲が空(git から行が渡っていない)"
        return 0
    fi
    changed="$(printf '%s\n' "$changed" | sed '/^$/d' | sort -u | tr '\n' ' ')"
    if [ -n "$changed" ]; then
        echo "enforcement の変更: $changed"
        return 0
    fi
    last="$(cat "$stamp" 2>/dev/null || true)"
    case "$last" in
        ''|*[!0-9]*)
            echo "全件の記録が無い($stamp)"
            return 0 ;;
    esac
    if [ "$last" -gt "$now" ]; then
        echo "全件の記録が未来を指している($stamp)"
        return 0
    fi
    age=$((now - last))
    if [ "$age" -ge "$ttl" ]; then
        echo "前回の全件から $((age / 86400)) 日"
        return 0
    fi
    echo "enforcement の変更なし・前回の全件から $((age / 86400)) 日"
    return 1
}
