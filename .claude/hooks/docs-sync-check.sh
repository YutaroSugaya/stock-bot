#!/usr/bin/env bash
# Claude Code Stop hook: コード → docs の同期を enforce する。
# pre-stop-checks.sh の逆方向: spec を符号化するソースを変更したら、対応する spec doc を
# 同じ Stop window(未 commit working tree OR 直近 commit OR untracked)で更新する。
#
# 対象は**契約そのものを書いた doc** だけ(H2・2026-10-01)。layers/*.md を exported の変更ごとに
# 要求していた間は、AI に残る出口が「doc に追記する」だけで、layers docs が実装の写しに膨らんだ(D9)。
# migration の docs 要求は pre-stop-checks.sh に一本化してある(同じ違反で 2 本とも block していた)。
#
# Triggers → 必須 doc:
#   backend/internal/port/repository.go           → layers/port.md OR layers/usecase.md
#   backend/internal/safety/*.go                  → layers/safety.md
#   新規 backend/internal/<NEW pkg>/*.go           → ARCHITECTURE.md or layers/*.md
#
# 上記に加えて backend/internal/** の .go を tier で見る(A1):
#   Tier 1 = block  ファイルの新規追加 / 削除、または diff が exported 宣言行に触れた変更
#     domain/position/*     → STATE_MACHINE.md
#     safety/*              → layers/safety.md + FAILURE_MODES.md(両方必須)
#   それ以外(内部実装だけの変更・他の層)は何も出さない。exit 0 の stderr はモデルに届かない。
#
# Allowlist: .claude/hooks/spec-sync-allowlist.txt(1 行 1 regex・人間が編集する)。*_test.go は自動除外。
# STOCKBOT_DOCS_SYNC_CHECKS は未設定なら "on"(下の `${…:-on}`)。=off で無効化。
# 自己テスト: .claude/hooks/docs-sync-check_test.sh(make guard から実行)。
set -euo pipefail

if [ "${STOCKBOT_DOCS_SYNC_CHECKS:-on}" != "on" ]; then
    exit 0
fi
if [ -z "${CLAUDE_PROJECT_DIR:-}" ]; then
    echo "BLOCKED: CLAUDE_PROJECT_DIR is not set" >&2
    exit 2
fi
# 補助ライブラリは「自分と同じディレクトリ」に置く。cd する前に絶対パスへ解決しないと、
# CLAUDE_PROJECT_DIR が別ツリーのときに黙って見つからず、検査が静かに消える。
hook_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

cd "${CLAUDE_PROJECT_DIR}" || { echo "cd project failed" >&2; exit 2; }

mkdir -p runtime/logs
log="runtime/logs/docs-sync-check.log"
{ echo "=== docs-sync check $(date +%FT%T%z) ==="; } > "$log"

# 🛑 fail-CLOSED 3 段(§2.4)。この lib は block_stop より**前**に要る(session_id を
# 決めるのが lib 自身)ので、出口は裸の exit 2。
stop_lib="${hook_dir}/stop-hook-lib.sh"
[ -f "$stop_lib" ] || { echo "BLOCKED: stop-hook-lib.sh が見つからない: $stop_lib" >&2; exit 2; }
set +e
# shellcheck source=/dev/null
. "$stop_lib"
stop_lib_rc=$?
set -e
[ "$stop_lib_rc" -eq 0 ] || {
    echo "BLOCKED: stop-hook-lib.sh の読み込みに失敗(rc=$stop_lib_rc): $stop_lib" >&2; exit 2; }
for stop_fn in hooklib_git hooklib_parse_stop_stdin hooklib_resolve_session_scope hooklib_first_stop; do
    command -v "$stop_fn" >/dev/null 2>&1 || {
        echo "BLOCKED: stop-hook-lib.sh に $stop_fn が無い(壊れた lib): $stop_lib" >&2; exit 2; }
done

# 🛑 パスを出す git は必ず hooklib_git を通す(RF3 #1)。既定(`core.quotePath=true`)だと非 ASCII
# のパスが C-quote されて `^backend/.*\.go$` に当たらず、**分類だけが外れて全ルールをすり抜ける**
# (fail-OPEN)。自己テストの `#1 経路A〜D` が 4 経路を独立に拘束している。

hook_stdin="$(cat 2>/dev/null || true)"
hooklib_parse_stop_stdin "$hook_stdin"
session_id="$HOOKLIB_SESSION_ID"
stop_hook_active="$HOOKLIB_STOP_HOOK_ACTIVE"
# フォールバック ID は PPID を含むので、親が毎回変わる呼ばれ方だとルール (7) の印が毎回別名に
# なり「常に最初の Stop」= 前セッションのコミットで永久 block、かつ連続 3 回の逃げ道も発火しない。
# そこで session_id が取れないときはルール (7) の範囲を作業ツリーだけに絞る。
session_known="$HOOKLIB_SESSION_KNOWN"

MAX_CONSECUTIVE_BLOCKS=3
block_count_file="runtime/logs/docs-sync-block-count.${session_id}"
block_count=0
[ -f "$block_count_file" ] && block_count="$(cat "$block_count_file" 2>/dev/null || echo 0)"

block_stop() {
    local reason="$1"
    if [ "$stop_hook_active" = "true" ] && [ "$block_count" -ge "$MAX_CONSECUTIVE_BLOCKS" ]; then
        { echo "WARNING: docs-sync '${reason}' は ${block_count} 回連続 fail。無限ループ防止で通過。drift は $log。"; } | tee -a "$log" >&2
        rm -f "$block_count_file"; exit 0
    fi
    echo "$((block_count + 1))" > "$block_count_file"
    echo "BLOCKED: $reason" >&2
    exit 2
}

# pre-stop-checks.sh と同じ session 開始 SHA を使う(1 セッションで複数 commit したとき
# 2 つ前以前の commit の spec 変更が漏れていた。2026-07-31 監査)。解決は stop-hook-lib.sh
# (RF3 #5)— **述語が 2 本に割れると必ず片方が fail-OPEN する**ので綴りを持たせない。
hooklib_resolve_session_scope "$session_id"
sessionstart_sha="$HOOKLIB_SESSIONSTART_SHA"
session_sha_from_sessionstart="$HOOKLIB_SESSION_SHA_FROM_SESSIONSTART"
session_start_sha="$HOOKLIB_SESSION_START_SHA"
session_range_ok="$HOOKLIB_SESSION_RANGE_OK"
prev_commit_in_scope="$HOOKLIB_PREV_COMMIT_IN_SCOPE"

# ルール (7) の範囲決めに使う「この hook がこのセッションで初めて走ったか」。
docs_sync_seen_file="runtime/logs/docs-sync-seen.${session_id}"
session_first_stop=0
if hooklib_first_stop "$docs_sync_seen_file"; then
    session_first_stop=1
fi

session_range_files=""
session_added_files=""
if [ "$session_range_ok" = "1" ]; then
    session_range_files=$(hooklib_git diff --name-only "${session_start_sha}..HEAD" 2>/dev/null || true)
    session_added_files=$(hooklib_git diff --name-only --diff-filter=A "${session_start_sha}..HEAD" 2>/dev/null || true)
fi

prev_commit_files=""
[ "$prev_commit_in_scope" = "1" ] && \
    prev_commit_files=$(hooklib_git diff --name-only HEAD~..HEAD 2>/dev/null || true)

changed_files=$( {
    hooklib_git diff --name-only HEAD 2>/dev/null
    echo "$prev_commit_files"
    echo "$session_range_files"
    hooklib_git ls-files --others --exclude-standard 2>/dev/null
} | sed '/^$/d' | sort -u || true )
changed_files=$(echo "$changed_files" | grep -vE '_test\.go$' || true)

# 「セッション開始時点に存在しなかった」判定: untracked だけでなく session 中に
# commit された新規ファイル/ディレクトリも新規として扱う。
new_files_all=$( {
    hooklib_git ls-files --others --exclude-standard 2>/dev/null
    echo "$session_added_files"
} | sed '/^$/d' | sort -u || true )
is_new_since_session() { # $1 = path(dir); セッション開始時点に無ければ true
    if [ -n "$session_start_sha" ] && git cat-file -e "${session_start_sha}^{commit}" 2>/dev/null; then
        [ -z "$(hooklib_git ls-tree -r --name-only "$session_start_sha" -- "$1" 2>/dev/null)" ]
    else
        [ -z "$(hooklib_git ls-files -- "$1" 2>/dev/null)" ]
    fi
}

if [ -z "$changed_files" ]; then
    echo "no source changes — skip" >> "$log"; rm -f "$block_count_file"; exit 0
fi

# ルール (7) は「このセッションが触った範囲」だけを見る(既存ルール 1〜6 の範囲は変えない)。
# --no-renames: リネームを R として 1 パスに畳まれると「新規追加でも削除でもない」に見えて
# Tier 1 を外れる。分解して両側を新規/削除として扱う。
tier_revs() { # ルール (7) が見る範囲。作業ツリー + セッション範囲(+ 最初の Stop なら直前コミット)
    echo "HEAD"
    [ "$session_known" = "0" ] && return 0
    # T12: 開始点が分かっていれば直前コミットは前セッションのもの。上の changed_files と
    # 同じ条件で外す(検出範囲がここだけ広いと、前セッション由来の block が復活する)。
    [ "$session_first_stop" = "1" ] && [ "$prev_commit_in_scope" = "1" ] && echo "HEAD~..HEAD"
    if [ -n "$session_start_sha" ] && git cat-file -e "${session_start_sha}^{commit}" 2>/dev/null; then
        echo "${session_start_sha}..HEAD"
    fi
    return 0
}
tier_range_diff() { # $1 = pathspec("" なら全体)、$2.. = git diff のオプション
    local p="$1"; shift
    local rev
    while IFS= read -r rev; do
        [ -z "$rev" ] && continue
        if [ -n "$p" ]; then
            hooklib_git diff "$@" "$rev" -- "$p" 2>/dev/null || true
        else
            hooklib_git diff "$@" "$rev" 2>/dev/null || true
        fi
    done <<< "$(tier_revs)"
}
tier_scope_files=$( {
    tier_range_diff "" --name-only --no-renames
    hooklib_git ls-files --others --exclude-standard 2>/dev/null
} | sed '/^$/d' | sort -u || true )
tier_scope_files=$(echo "$tier_scope_files" | grep -vE '_test\.go$' || true)
tier_added_files=$( {
    tier_range_diff "" --name-only --no-renames --diff-filter=A
    hooklib_git ls-files --others --exclude-standard 2>/dev/null
} | sed '/^$/d' | sort -u || true )
tier_deleted_files=$(tier_range_diff "" --name-only --no-renames --diff-filter=D | sed '/^$/d' | sort -u || true)

allowlist=".claude/hooks/spec-sync-allowlist.txt"
if [ -f "$allowlist" ]; then
    while IFS= read -r pattern; do
        case "$pattern" in ''|\#*) continue ;; esac
        pattern="${pattern%% #*}"; [ -z "$pattern" ] && continue
        set +e; printf '' | grep -qE -e "$pattern" 2>/dev/null; regex_rc=$?; set -e
        if [ "$regex_rc" -ge 2 ]; then echo "WARNING: invalid allowlist regex skipped: $pattern" | tee -a "$log" >&2; continue; fi
        changed_files=$(echo "$changed_files" | grep -vE "$pattern" || true)
        tier_scope_files=$(echo "$tier_scope_files" | grep -vE "$pattern" || true)
    done < "$allowlist"
fi
if [ -z "$changed_files" ]; then
    echo "all changes allowlisted — skip" >> "$log"; rm -f "$block_count_file"; exit 0
fi

doc_changed() { echo "$changed_files" | grep -qx "$1"; }
violations=()
add_violation() { violations+=("  $1"); violations+=("    → 期待する doc 変更: $2"); }

# (2) port/repository.go → port.md OR usecase.md
if echo "$changed_files" | grep -qx 'backend/internal/port/repository.go'; then
    if ! doc_changed "docs/architecture/layers/port.md" && ! doc_changed "docs/architecture/layers/usecase.md"; then
        add_violation "backend/internal/port/repository.go" "docs/architecture/layers/port.md OR docs/architecture/layers/usecase.md"
    fi
fi

# (3) safety/*.go → safety.md
safety_changed=$(echo "$changed_files" | grep -E '^backend/internal/safety/.*\.go$' || true)
if [ -n "$safety_changed" ] && [ -f "docs/architecture/layers/safety.md" ]; then
    doc_changed "docs/architecture/layers/safety.md" || while IFS= read -r f; do [ -n "$f" ] && add_violation "$f" "docs/architecture/layers/safety.md"; done <<< "$safety_changed"
fi

# (6) 新規 internal パッケージ → ARCHITECTURE.md or layers/*.md
new_internal_files=$(echo "$new_files_all" | grep -E '^backend/internal/[^/]+/[^/]+\.go$' | grep -vE '_test\.go$' || true)
if [ -n "$new_internal_files" ]; then
    layers_doc_changed=$(echo "$changed_files" | grep -E '^docs/(ARCHITECTURE\.md|architecture/layers/.*\.md)$' || true)
    seen_new_pkgs=""
    while IFS= read -r f; do
        [ -z "$f" ] && continue
        pkg_dir=$(dirname "$f")
        case " $seen_new_pkgs " in *" $pkg_dir "*) continue ;; esac
        seen_new_pkgs="$seen_new_pkgs $pkg_dir"
        if is_new_since_session "$pkg_dir" && [ -z "$layers_doc_changed" ]; then
            add_violation "$f" "docs/ARCHITECTURE.md or docs/architecture/layers/*.md (新規 internal package: $pkg_dir)"
        fi
    done <<< "$new_internal_files"
fi

# (7) 契約の doc を持つ層だけ、契約が動いたときに block する。全変更を block にすると bugfix
# 1 行ごとに doc を要求することになり、「無視されるガード」になって enforcement 自体が死ぬ。
is_added_file()   { echo "$tier_added_files" | grep -qx "$1"; }
is_deleted_file() { echo "$tier_deleted_files" | grep -qx "$1"; }

# 契約変更の主要形は 0 桁目に現れない — grouped const の enum 値・struct のフィールド・
# interface のメソッドは全てインデントされている。そこで hunk ヘッダの文脈(git の既定 funcname
# = 直前の 0 桁目の行)で括りの中かを判定し、文脈が `func` のときはインデント行を見ない
# (関数本体の Exported(...) 呼び出しを拾わないため)。行末コメントを落として空白を潰すので、
# コメントだけの修正と gofmt の整列は差分にならない。
decl_lines() { # $1 = '+' or '-'
    awk -v sign="$1" '
        /^(\+\+\+|---)/ { next }
        /^@@/ { ctx = ""; if (match($0, /@@[^@]*@@ ?/)) ctx = substr($0, RSTART + RLENGTH); next }
        substr($0, 1, 1) != sign { next }
        {
            raw = substr($0, 2)
            indented = (raw ~ /^[ \t]/)
            line = raw
            # 行末コメントは落とすが、文字列の中の // は落とさない
            # (var X = "https://…/v1" の v1→v2 が見えなくなる)。
            q = index(line, "\"")
            c = index(line, "//")
            if (c > 0 && (q == 0 || c < q)) sub(/[ \t]*\/\/.*$/, "", line)
            gsub(/[ \t]+/, " ", line)
            sub(/^ +/, "", line); sub(/ +$/, "", line)
            if (line == "") next
            if (!indented) {
                if (line ~ /^(func|type|const|var) [A-Z]/ || line ~ /^func \([^)]*\) [A-Z]/) print line
                next
            }
            # 括りの中の宣言。ctx に func( があるものは var X = func(){…} の本体なので除く。
            if (ctx !~ /func\(/ && (ctx ~ /^(type|const|var) [A-Z]/ || ctx ~ /^(type|const|var) \(/)) {
                # 行末で終わる裸の識別子も宣言(iota の継続行 / 埋め込みフィールド)。
                if (line ~ /^[A-Z][A-Za-z0-9_]*([ (,]|$)/) print line
                next
            }
            # 複数行シグネチャの引数。git の funcname は複数行シグネチャの関数だけ開き括弧で
            # 終わる ctx(`func Compute(`)を出す = 関数本体を巻き込まない判別子になる。
            # 文キーワードを除くのは `return Result` のような本体 1 行が `名前 型` と同形だから。
            # raw のタブ 1 個限定: 引数行は gofmt により必ずタブ 1 個。本体にインデントされた
            # 生文字列 SQL(タブ 2 個以上)も同形なので、無いと SQL を 1 語直しただけで block する。
            if (ctx ~ /^func .*\($/ && raw ~ /^\t[^\t ]/ && line !~ /[=:{}()]/ \
                && line !~ /^(return|if|else|for|switch|case|go|defer|var|const|type|func|break|continue|range|select|fallthrough|goto)([ \t]|$)/ \
                && line ~ /^[a-zA-Z_][A-Za-z0-9_]*( [][*a-zA-Z0-9_.]+)+,?$/) print "param " line
        }
    ' | sort
}

exported_touched() { # $1 = path。exported 宣言の集合が + と - で食い違えば契約が動いている
    local d adds dels
    d=$(tier_range_diff "$1" -U0 || true)
    [ -z "$d" ] && return 1
    adds=$(printf '%s\n' "$d" | decl_lines '+')
    dels=$(printf '%s\n' "$d" | decl_lines '-')
    [ "$adds" != "$dels" ]
}

# ALL = 列挙した doc を全て要求 / ANY = どれか 1 つ。
tier_docs_for() { # $1 = path
    case "$1" in
        backend/internal/domain/position/*)    echo "ALL:docs/runtime/STATE_MACHINE.md";;
        backend/internal/safety/*)             echo "ALL:docs/architecture/layers/safety.md|docs/architecture/FAILURE_MODES.md";;
        *) echo "";;
    esac
}

tier_candidates=$(echo "$tier_scope_files" | grep -E '^backend/internal/.*\.go$' || true)
while IFS= read -r f; do
    [ -z "$f" ] && continue
    spec="$(tier_docs_for "$f")"
    [ -z "$spec" ] && continue
    mode="${spec%%:*}"
    docs="${spec#*:}"
    if is_added_file "$f" || is_deleted_file "$f" || exported_touched "$f"; then
        satisfied=0
        missing=""
        while IFS= read -r d; do
            [ -z "$d" ] && continue
            if doc_changed "$d"; then satisfied=1; else missing="$missing $d"; fi
        done <<< "$(echo "$docs" | tr '|' '\n')"
        if [ "$mode" = "ANY" ]; then
            [ "$satisfied" -eq 1 ] || add_violation "$f" "$(echo "$docs" | tr '|' ' ') のいずれか"
        else
            [ -z "$missing" ] || add_violation "$f" "${missing# } (全て必須)"
        fi
    fi
done <<< "$tier_candidates"

{ echo; echo "--- changed files (post-allowlist) ---"; echo "$changed_files" | sed 's/^/  /'; } >> "$log"

if [ ${#violations[@]} -gt 0 ]; then
    {
        echo; echo "=== SPEC DOC SYNC CHECK BLOCKED ==="
        echo "ソースを変更したが、その契約を符号化する spec doc が同じ Stop window で更新されていない。"
        echo "  - 契約が変わったなら該当 doc を直す(現在形の事実に上書きする。経緯は積まない)、または"
        echo "  - 契約を変えない編集(typo/log/整形)なら、.claude/hooks/spec-sync-allowlist.txt への"
        echo "    regex の追加を人間に頼む(allowlist は enforcement 側にあり AI からは編集できない)。"
        echo
        printf '%s\n' "${violations[@]}"
    } | tee -a "$log" >&2
    block_stop "docs sync drift"
fi

rm -f "$block_count_file"
echo "docs-sync check passed (no spec/doc drift)" >> "$log"
exit 0
