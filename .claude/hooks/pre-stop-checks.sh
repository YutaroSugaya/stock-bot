#!/usr/bin/env bash
# Claude Code Stop hook: 層規約 + migration 規律 + secret + make check-backend を
# Claude がターンを終える前に enforce する。
# STOCKBOT_PRESTOP_CHECKS は未設定なら "on"(下の `${…:-on}`。settings.json には書かない)。
# 1 セッションだけ無効化するなら `STOCKBOT_PRESTOP_CHECKS=off claude` で起動する。
# Exit: 0 = 通過、2 = stop をブロック(stderr に理由)。
set -euo pipefail

# fail-CLOSED: env 未設定でも検査を走らせる。無効化は明示 =off のみ。
if [ "${STOCKBOT_PRESTOP_CHECKS:-on}" != "on" ]; then
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
log="runtime/logs/pre-stop-checks.log"
{ echo "=== pre-stop checks $(date +%FT%T%z) ==="; } > "$log"

# 🛑 fail-CLOSED 3 段(§2.4)。ここは block_stop より**前**なので出口は裸の exit 2 —
# session_id を決めるのがこの lib 自身で、block カウンタがまだ無い(鶏卵)。
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

hook_stdin="$(cat 2>/dev/null || true)"
hooklib_parse_stop_stdin "$hook_stdin"
session_id="$HOOKLIB_SESSION_ID"
stop_hook_active="$HOOKLIB_STOP_HOOK_ACTIVE"
session_known="$HOOKLIB_SESSION_KNOWN"

MAX_CONSECUTIVE_BLOCKS=3
block_count_file="runtime/logs/pre-stop-block-count.${session_id}"
block_count=0
[ -f "$block_count_file" ] && block_count="$(cat "$block_count_file" 2>/dev/null || echo 0)"

block_stop() {
    local reason="$1"
    if [ "$stop_hook_active" = "true" ] && [ "$block_count" -ge "$MAX_CONSECUTIVE_BLOCKS" ]; then
        {
            echo "WARNING: pre-stop check '${reason}' は ${block_count} 回連続で fail。"
            echo "無限ループ防止のため通過させる。.githooks/pre-push が最終ゲート。"
            echo "未解決の失敗は $log を参照。"
        } | tee -a "$log" >&2
        rm -f "$block_count_file"
        exit 0
    fi
    echo "$((block_count + 1))" > "$block_count_file"
    echo "BLOCKED: $reason" >&2
    exit 2
}

# 🛑 パスを出す git は hooklib_git を通す。既定では非 ASCII のパスが C-quote されて出るため
# `^backend/.*\.go$` に当たらず、**early exit で hook ごと素通り**する(A2 だけでなく arch-guard /
# migration 整合 / make check-backend も走らなくなる)。開始点の解決は stop-hook-lib.sh
# (RF3 #5)— 述語と SHA の形検証を 2 本の hook で割らないための集約。
hooklib_resolve_session_scope "$session_id"
sessionstart_sha="$HOOKLIB_SESSIONSTART_SHA"
session_sha_from_sessionstart="$HOOKLIB_SESSION_SHA_FROM_SESSIONSTART"
session_start_sha="$HOOKLIB_SESSION_START_SHA"
session_range_ok="$HOOKLIB_SESSION_RANGE_OK"

session_range_files=""
if [ "$session_range_ok" = "1" ]; then
    session_range_files=$(hooklib_git diff --name-only "${session_start_sha}..HEAD" 2>/dev/null || true)
fi

# T12: 無条件の `HEAD~..HEAD` は、セッション開始点が分かっているなら**有害**。作業ツリーが
# clean で当セッションが何もしていなくても直前コミットのファイルが載り、Step 5 の early exit を
# 抜けて make check-backend まで走る。直前コミットが repository/ を触っていると Step 10 が
# INTEGRATION_TEST_DB_URL を要求し、**何もしていないターンで 3 連続ブロック**された。
prev_commit_files=""
if [ "$HOOKLIB_PREV_COMMIT_IN_SCOPE" = "1" ]; then
    prev_commit_files=$(hooklib_git diff --name-only HEAD~..HEAD 2>/dev/null || true)
fi

changed_files=$( {
    hooklib_git diff --name-only HEAD 2>/dev/null
    echo "$prev_commit_files"
    echo "$session_range_files"
    hooklib_git ls-files --others --exclude-standard 2>/dev/null
} | sed '/^$/d' | sort -u || true )

changed_go=$(echo "$changed_files" | grep -E '^backend/.*\.go$' || true)
changed_migration_files=$(echo "$changed_files" | grep -E '^migrations/' || true)
changed_db_go=$(echo "$changed_go" | grep -E '^backend/internal/adapter/repository/|^backend/internal/port/repository\.go$|^backend/cmd/migrate/' || true)

# パターンの正本は secret-patterns.sh(RF3 #4)。以前はここと scripts/secret-scan.sh に別々の
# 配列があり**双方向に取りこぼしていた** — 「wire 裏取り中に実レスポンスを docs へ貼る」事故
# (2026-08-06 監査)で secret-scan 側に足された立花のセッション仮想 URL / sUrlRequest /
# DER base64 がこちらには無かった。回帰は scripts/secret-scan_test.sh が両消費側で拘束する。
secret_pattern_lib="${hook_dir}/secret-patterns.sh"
[ -f "$secret_pattern_lib" ] || {
    echo "secret-patterns.sh が見つからない: $secret_pattern_lib" | tee -a "$log" >&2
    block_stop "secret pattern library missing"
}
# 🛑 `set +e` で囲う理由は tdd-check-lib.sh と同じ(素で source すると lib 内の失敗で bash が
# exit 1 して死に、Stop hook のプロトコル上 exit 1 は「block しない」= 検査が黙って飛ぶ)。
set +e
# shellcheck source=/dev/null
. "$secret_pattern_lib"
secret_pattern_lib_rc=$?
set -e
[ "$secret_pattern_lib_rc" -eq 0 ] || {
    echo "secret-patterns.sh の読み込みに失敗(rc=$secret_pattern_lib_rc)" | tee -a "$log" >&2
    block_stop "secret pattern library failed to load"
}
[ "${#SECRET_PATTERNS[@]}" -gt 0 ] || {
    echo "SECRET_PATTERNS が空(壊れた lib): $secret_pattern_lib" | tee -a "$log" >&2
    block_stop "secret pattern library empty"
}
secret_patterns=("${SECRET_PATTERNS[@]}")
secret_errors=()
changed_added_lines=$(git diff --no-color --unified=0 HEAD 2>/dev/null | grep -E '^\+[^+]' || true)
# 🛑 `echo … | grep -q` にしない。`grep -q` が最初のマッチで抜けると上流が SIGPIPE(141)で死に、
# `set -o pipefail` で条件が**偽**になる = **秘密が 1 文字も変わっていないのに素通り**する
# (閾値はパイプバッファ 64KB なので普通の実装セッションの追加 diff で超える)。
# here-string はパイプではないので SIGPIPE が原理的に起きない。
for pat in "${secret_patterns[@]}"; do
    if [ -n "$changed_added_lines" ] && grep -E -q -e "$pat" <<< "$changed_added_lines"; then
        secret_errors+=("added diff に秘密情報らしきパターン: $pat")
    fi
done
while IFS= read -r f; do
    [ -z "$f" ] && continue
    case "$f" in
        .env|*/.env|*.pem|*.key|*.p12)
            [ -e "$f" ] && secret_errors+=("secret-like file changed: $f") ;;
    esac
    if git ls-files --others --exclude-standard -- "$f" | grep -Fxq "$f" && [ -f "$f" ]; then
        for pat in "${secret_patterns[@]}"; do
            if grep -E -Iq -e "$pat" "$f"; then
                secret_errors+=("untracked file に秘密情報らしきパターン: $f ($pat)")
            fi
        done
    fi
done <<< "$changed_files"

if [ ${#secret_errors[@]} -gt 0 ]; then
    { echo; echo "=== SECRET SCAN FAILED (${#secret_errors[@]}) ==="; printf '  %s\n' "${secret_errors[@]}"; } | tee -a "$log" >&2
    block_stop "potential secret leak"
fi

# 対象の集合は enforcement-paths.sh が正本(RF3 #6)。T5 が同じ集合を Bash / Edit 経路でも
# 使うので、綴りをここに置かない。🛑 欠けたら「enforcement 対象ゼロ」に黙って縮退するので
# fail-CLOSED 3 段(§2.4)で守る。
enforcement_path_lib="${hook_dir}/enforcement-paths.sh"
[ -f "$enforcement_path_lib" ] || {
    echo "enforcement-paths.sh が見つからない: $enforcement_path_lib" | tee -a "$log" >&2
    block_stop "enforcement path definition missing"
}
set +e
# shellcheck source=/dev/null
. "$enforcement_path_lib"
enforcement_path_lib_rc=$?
set -e
[ "$enforcement_path_lib_rc" -eq 0 ] || {
    echo "enforcement-paths.sh の読み込みに失敗(rc=$enforcement_path_lib_rc)" | tee -a "$log" >&2
    block_stop "enforcement path definition failed to load"
}
[ -n "${ENF_PATH_RE:-}" ] || {
    echo "enforcement-paths.sh に ENF_PATH_RE が無い(壊れた lib): $enforcement_path_lib" | tee -a "$log" >&2
    block_stop "enforcement path definition incomplete"
}
enforcement_changed=$(echo "$changed_files" | grep -E "$ENF_PATH_RE" || true)
if [ -n "$enforcement_changed" ]; then
    enforcement_digest=$(printf '%s' "$enforcement_changed" | /sbin/md5 -q 2>/dev/null || printf '%s' "$enforcement_changed" | md5sum | cut -d' ' -f1)
    enforcement_ack="runtime/logs/enforcement-ack.${session_id}.${enforcement_digest}"
    if [ ! -f "$enforcement_ack" ]; then
        touch "$enforcement_ack"
        {
            echo; echo "=== ENFORCEMENT FILES CHANGED THIS SESSION (warn-once) ==="
            echo "$enforcement_changed" | sed 's/^/  /'
            echo "hook / settings / git-hooks / guard スクリプトへの変更を検知。ルール(CLAUDE.md)の"
            echo "弱体化でないか git diff で確認。意図的なら、そのまま再度 stop すれば通過する。"
        } | tee -a "$log" >&2
        block_stop "enforcement files changed — confirm the diff is intentional"
    fi
    echo "enforcement change acknowledged: $(echo "$enforcement_changed" | tr '\n' ' ')" >> "$log"
fi

# Early exit: Go も migration も変わってなければ層/テスト検査をスキップ(高速)。
if [ -z "$changed_go" ] && [ -z "$changed_migration_files" ]; then
    echo "no backend Go or migration changes — skip architecture + test checks" >> "$log"
    rm -f "$block_count_file"
    exit 0
fi

{
    echo; echo "--- changed relevant files ---"
    [ -n "$changed_go" ] && { echo "[go]"; echo "$changed_go" | sed 's/^/  /'; }
    [ -n "$changed_migration_files" ] && { echo "[migrations]"; echo "$changed_migration_files" | sed 's/^/  /'; }
} >> "$log"

if [ -n "$changed_go" ]; then
    if ! arch_out=$(bash scripts/arch-guard.sh 2>&1); then
        { echo; echo "=== ARCH-GUARD VIOLATIONS ==="; echo "$arch_out"; } | tee -a "$log" >&2
        block_stop "architecture rule violations (scripts/arch-guard.sh)"
    fi
    echo "$arch_out" >> "$log"
fi

# repository / port / cmd/migrate の Go に CREATE/ALTER/DROP TABLE 等を直書きしたら、
# 対の migration を作るよう促す(schema-as-code の正本は migrations/)。
if [ -n "$changed_db_go" ]; then
    # working tree だけでなく session 中に commit 済みの DDL も見る(2026-07-31 監査で修正)。
    ddl=$( {
        git diff --no-color HEAD -- backend/internal/adapter/repository backend/internal/port/repository.go backend/cmd/migrate 2>/dev/null
        if [ -n "$session_start_sha" ] && git cat-file -e "${session_start_sha}^{commit}" 2>/dev/null; then
            git diff --no-color "${session_start_sha}..HEAD" -- backend/internal/adapter/repository backend/internal/port/repository.go backend/cmd/migrate 2>/dev/null
        fi
    } | grep -Ei '^\+[^+].*(create table|alter table|drop table|create index|drop index|add column|comment on|truncate)' || true)
    if [ -n "$ddl" ]; then
        { echo; echo "=== DDL OUTSIDE migrations/ ==="; echo "$ddl"; echo "→ migrations/ に対の up/down migration を作ること。"; } | tee -a "$log" >&2
        block_stop "schema DDL added outside migrations/"
    fi
fi

if [ -n "$changed_migration_files" ]; then
    migration_errors=()
    migration_files=$(find migrations -maxdepth 1 -type f 2>/dev/null | sort || true)
    up_versions=()
    [ -z "$migration_files" ] && migration_errors+=("migrations/ に migration ファイルがありません")

    while IFS= read -r f; do
        [ -z "$f" ] && continue
        base="${f##*/}"
        if ! [[ "$base" =~ ^[0-9]{4}_[a-z0-9_]+\.(up|down)\.sql$ ]]; then
            migration_errors+=("$f は命名違反; 期待 NNNN_name.(up|down).sql"); continue
        fi
        case "$f" in
            *.up.sql)
                pair="${f%.up.sql}.down.sql"
                [ ! -e "$pair" ] && migration_errors+=("$f に対応する down がありません: $pair")
                up_versions+=("${base%%_*}") ;;
            *.down.sql)
                pair="${f%.down.sql}.up.sql"
                [ ! -e "$pair" ] && migration_errors+=("$f に対応する up がありません: $pair") ;;
        esac
    done <<< "$migration_files"

    if [ ${#up_versions[@]} -gt 0 ]; then
        unique_versions=$(printf '%s\n' "${up_versions[@]}" | sort -u)
        # 各 version の up ファイルは厳密に 1 個(重複検出)。
        while IFS= read -r version; do
            [ -z "$version" ] && continue
            count=$(printf '%s\n' "${up_versions[@]}" | grep -c "^${version}$" || true)
            [ "$count" -ne 1 ] && migration_errors+=("version $version の up ファイルが $count 個あります(期待 1)")
        done <<< "$unique_versions"
        # 0001 からの連番(gap 検出)。
        expected=1
        while IFS= read -r version; do
            [ -z "$version" ] && continue
            actual=$((10#$version))
            if [ "$actual" -ne "$expected" ]; then
                migration_errors+=("migration version は 0001 から連番である必要があります; 期待 $(printf '%04d' "$expected") だが $version")
                expected=$((actual + 1))
            else
                expected=$((expected + 1))
            fi
        done <<< "$unique_versions"
    fi

    # 変更された migration ファイルの命名を再検証(find ベースの走査が漏らした場合の back-stop)。
    while IFS= read -r f; do
        [ -z "$f" ] && continue
        base="${f##*/}"
        if ! [[ "$base" =~ ^[0-9]{4}_[a-z0-9_]+\.(up|down)\.sql$ ]]; then
            migration_errors+=("$f は命名違反; 期待 NNNN_name.(up|down).sql")
        fi
    done <<< "$changed_migration_files"

    # migration の履歴表は DATA_MODEL だけが持つ(D10)。MIGRATIONS は手順の doc なので要求しない。
    # docs の要求はここが唯一の持ち主(docs-sync-check.sh の同じ検査は H2 で外した)。
    echo "$changed_files" | grep -qx 'docs/runtime/DATA_MODEL.md' || migration_errors+=("migration を変更したが docs/runtime/DATA_MODEL.md が未更新")

    if [ ${#migration_errors[@]} -gt 0 ]; then
        { echo; echo "=== MIGRATION CHECKS FAILED (${#migration_errors[@]}) ==="; printf '  %s\n' "${migration_errors[@]}"; } | tee -a "$log" >&2
        block_stop "migration checks failed"
    fi
fi

{
    echo; echo "--- changed files → relevant docs ---"
    printf '%s\n%s\n' "$changed_go" "$changed_migration_files" | sed '/^$/d' | while IFS= read -r f; do
        case "$f" in
            migrations/*)                               echo "  $f → docs/runtime/DATA_MODEL.md";;
            backend/internal/app/handler/*)             echo "  $f → docs/architecture/layers/handler.md";;
            backend/internal/usecase/command/*)         echo "  $f → docs/architecture/layers/usecase.md";;
            backend/internal/usecase/query/*)           echo "  $f → docs/architecture/layers/usecase.md (Query)";;
            backend/internal/domain/position/*)         echo "  $f → docs/architecture/layers/domain.md, docs/runtime/STATE_MACHINE.md";;
            backend/internal/domain/*)                  echo "  $f → docs/architecture/layers/domain.md";;
            backend/internal/port/*)                    echo "  $f → docs/architecture/layers/port.md";;
            backend/internal/adapter/repository/*)      echo "  $f → docs/architecture/layers/adapter.md, docs/runtime/DATA_MODEL.md";;
            backend/internal/adapter/*)                 echo "  $f → docs/architecture/layers/adapter.md";;
            backend/internal/safety/*)                  echo "  $f → docs/architecture/layers/safety.md, docs/architecture/FAILURE_MODES.md";;
            backend/internal/backtest/*)                echo "  $f → docs/workflows/BACKTEST.md";;
            backend/cmd/stockbot/*)                     echo "  $f → docs/ARCHITECTURE.md (wiring)";;
            *)                                          echo "  $f → (no specific layer doc)";;
        esac
    done
} >> "$log"

# テストの存在(A2)。**強制するのは存在まで** — Red-first は事後の diff から復元できないので
# 人間規律(CLAUDE.md)。判定は tdd-check-lib.sh(自己テスト pre-stop-tdd_test.sh)で、ここは
# 「git から何を拾うか」だけを持つ。範囲は作業ツリー + session 範囲 +(この検査がこのセッションで
# 初めて走ったときだけ)直前コミット。
# 🛑 「初めてか」を session-start-sha で判定してはいけない — この hook 自身が上でそのファイルを
# 作るので常に「2 回目以降」になり、「実装 → コミット → Stop」の標準フローで直前コミットを
# 一度も見ないことになる。自前の印を持つ(A1 で実測した罠)。
tdd_lib="${hook_dir}/tdd-check-lib.sh"
if [ "${STOCKBOT_TDD_CHECK:-on}" = "on" ]; then
    # fail-CLOSED: ライブラリが無いのは配線の破壊。黙って検査を落とさない。
    [ -f "$tdd_lib" ] || {
        echo "tdd-check-lib.sh が見つからない: $tdd_lib" | tee -a "$log" >&2
        block_stop "test-existence check library missing"
    }
    # 🛑 「在ること」だけでは足りない(RF3 #2)。判定は `tdd_out="$(tdd_violations … || true)"` で
    # 受けるので、**lib が空 / 判定関数を欠いていると 127 を `|| true` が飲み、出力が空 =
    # 「違反なし」に化けて A2 が黙って消える**(`: > tdd-check-lib.sh` の 1 手で無効化できた)。
    # source の失敗と、source 後に判定関数が実在することまで見る。
    # 🛑 `set +e` で囲うのが要点。素で source すると lib の中で失敗した時点で bash 自身が exit 1
    # して死に、Stop hook では **exit 2 だけが block** なので **A2 が黙って飛んだうえで Stop が
    # 通る**(実測)。一旦 -e を外して rc を受け取り、こちらの言葉で block_stop する。
    # 自己テストの e11(空) / e12(関数欠落) / e13(対照) / e14b(構文エラー)が各状態を拘束する。
    # shellcheck source=/dev/null
    set +e
    . "$tdd_lib"
    tdd_lib_rc=$?
    set -e
    [ "$tdd_lib_rc" -eq 0 ] || {
        echo "tdd-check-lib.sh の読み込みに失敗(rc=$tdd_lib_rc): $tdd_lib" | tee -a "$log" >&2
        block_stop "test-existence check library failed to load"
    }
    for tdd_fn in tdd_violations tdd_is_exempt; do
        command -v "$tdd_fn" >/dev/null 2>&1 || {
            echo "tdd-check-lib.sh に $tdd_fn が無い(壊れた lib): $tdd_lib" | tee -a "$log" >&2
            block_stop "test-existence check library incomplete ($tdd_fn missing)"
        }
    done

    tdd_seen_file="runtime/logs/tdd-check-seen.${session_id}"
    tdd_first_stop=0
    if hooklib_first_stop "$tdd_seen_file"; then
        tdd_first_stop=1
    fi

    # git の出力形式は config で変えられ、既定のままだと検査が黙って消える: core.quotePath
    # (非 ASCII パスの C-quote で `.go` に当たらない)/ diff.mnemonicPrefix(接頭辞が c/ w/ に
    # なり path が剥がれない)/ diff.external(diff 本文が空)。quotePath への対処は hooklib_git
    # (RF3 #5)、残り 2 つは呼び出し側で明示する。

    # 🛑 最初の Stop で直前コミットを範囲に入れるのは「実装 → コミット → Stop」で**テストを
    # 持たない**コミットを拾うためだけ。**自分でテストを持っているコミットは範囲に入れない** —
    # 範囲は Tier 2 の免除判定(「*_test.go を触ったか」)の入力でもあり、前セッション最後の
    # コミットがテストを触っていると**今セッションの作業ツリー変更が丸ごと免除される**
    # (実リポでは backend を触った 11 コミットが 11 件ともテストを触っており、ほぼ常時この状態)。
    # 代償: テストも触りつつテスト無しパッケージも足した混在コミットは Tier 1/3 の 1 回きりの
    # block を失う(Tier 2 は元から免除)。
    # 🛑 末尾 `grep -q` にしない。最初のマッチで抜けると上流が SIGPIPE(141)で死に、それが
    # パイプライン全体の status になって判定が**非決定的に反転**する(前コミットが大きいほど出る)。
    tdd_prev_has_test() {
        local hits
        hits="$(hooklib_git diff --name-only HEAD~..HEAD -- backend 2>/dev/null \
            | grep -vE '/testdata/' | grep -E '_test\.go$' || true)"
        [ -n "$hits" ]
    }
    # T12: セッション開始点が SessionStart から分かっているなら `HEAD~..HEAD` は**前セッションの
    # 最後のコミット**でしかない。`<sha>..HEAD` が当セッションのコミットを漏れなく覆うので、
    # 入れると誤検知(前セッション由来の 1 回きりの block / 免除の漏れ)だけが残る。
    tdd_prev_in_scope=1
    if [ "$session_sha_from_sessionstart" = "1" ] && [ "$session_range_ok" = "1" ]; then
        tdd_prev_in_scope=0
    fi
    tdd_use_prev=0
    if [ "$session_known" = "1" ] && [ "$tdd_first_stop" = "1" ] && [ "$tdd_prev_in_scope" = "1" ] \
       && git cat-file -e 'HEAD~^{commit}' 2>/dev/null && ! tdd_prev_has_test; then
        tdd_use_prev=1
    fi
    tdd_revs() {
        echo "HEAD"
        # session_id が取れないとき、印の名前は PPID 依存で毎回変わる = 常に「最初の Stop」。
        # 前セッションのコミットで永久 block になるので作業ツリーだけに縮退する。
        [ "$session_known" = "0" ] && return 0
        [ "$tdd_first_stop" = "1" ] && [ "$tdd_prev_in_scope" = "1" ] && echo "HEAD~..HEAD"
        if [ -n "$session_start_sha" ] && git cat-file -e "${session_start_sha}^{commit}" 2>/dev/null; then
            echo "${session_start_sha}..HEAD"
        fi
        return 0
    }
    tdd_range_diff() { # $@ = git diff のオプション
        local rev
        while IFS= read -r rev; do
            [ -z "$rev" ] && continue
            hooklib_git diff --no-ext-diff --src-prefix=a/ --dst-prefix=b/ "$@" "$rev" -- backend 2>/dev/null || true
        done <<< "$(tdd_revs)"
    }

    tdd_changed=$( {
        tdd_range_diff --name-only --no-renames
        hooklib_git ls-files --others --exclude-standard -- backend 2>/dev/null
    } | sed '/^$/d' | sort -u || true )
    # --no-renames: リネームを R に畳まれると新規追加に見えなくなる。
    tdd_added=$( {
        tdd_range_diff --name-only --no-renames --diff-filter=A
        hooklib_git ls-files --others --exclude-standard -- backend 2>/dev/null
    } | sed '/^$/d' | sort -u || true )
    # 🛑 untracked も diff に足す。git diff は追跡外のファイルを一切出さないので、これが無いと
    # Tier 2 は「Write で作ってまだコミットしていない新規ファイル」= Claude が実際に作業して
    # いる状態を構造的に見られない(Tier 1 だけが untracked を見ていた)。全行を + として合成する。
    tdd_untracked_diff() {
        local f
        while IFS= read -r f; do
            [ -z "$f" ] && continue
            [ -f "$f" ] || continue
            # 実 git diff と同じ枠を付ける。パーサは「hunk の外で、直前が `--- `」のときだけ
            # ヘッダを採用するので、枠が無いと**中身の 1 行目**が `++ b/…` のときに偽装が成立する。
            printf -- 'diff --git a/%s b/%s\n--- /dev/null\n+++ b/%s\n@@ -0,0 +1,1 @@\n' "$f" "$f" "$f"
            # `sed 's/^/+/'` は入力に最終改行が無ければ出力にも付けないので、次のヘッダが最終行に
            # 連結されて消える(= path が前のファイルのまま据え置かれる)。awk の print は最終
            # レコードにも必ず ORS を付ける。gofmt 前の新規ファイルで実際に踏むので sed に戻さない。
            awk '{ print "+" $0 }' "$f" 2>/dev/null || true
        # .go に絞るのは**判定のため**ではなく(非 .go は tdd_is_exempt が落とす)、
        # untracked のバイナリや巨大生成物を sed に流し込まないため。
        done <<< "$(hooklib_git ls-files --others --exclude-standard -- backend 2>/dev/null | grep -E '\.go$' || true)"
    }
    # 🛑 Tier 2 に渡す本文は**継ぎ目のない 1 本の diff**にする。範囲を連結すると片方の `-宣言行`
    # が他方の `+宣言行` を相殺して block が黙って消える(session 範囲で足した宣言を、作業ツリーで
    # 行末コメントを 1 語直すだけで打ち消せた)。`--name-only` 側は和集合なので連結のままでよい。
    tdd_diff_base() {
        local base="HEAD"
        [ "$session_known" = "0" ] && { echo "$base"; return 0; }
        if [ -n "$session_start_sha" ] && git cat-file -e "${session_start_sha}^{commit}" 2>/dev/null; then
            base="$session_start_sha"
        fi
        # 最初の Stop では session 開始 SHA が「今の HEAD」なので直前コミットが範囲から
        # 落ちる。HEAD~ の方が古いときだけそちらへ広げる(tdd_revs と同じ条件で判断する —
        # 検出の範囲と免除の範囲がずれると、片方だけが前セッションを見ることになる)。
        if [ "$tdd_use_prev" = "1" ] && git cat-file -e 'HEAD~^{commit}' 2>/dev/null; then
            if [ "$base" = "HEAD" ] || ! git merge-base --is-ancestor "$base" 'HEAD~' 2>/dev/null; then
                base="HEAD~"
            fi
        fi
        echo "$base"
    }
    tdd_base="$(tdd_diff_base)"
    tdd_diff=$( {
        hooklib_git diff --no-ext-diff --src-prefix=a/ --dst-prefix=b/ -U0 --no-renames \
            "$tdd_base" -- backend 2>/dev/null
        tdd_untracked_diff
    } || true )

    # Tier 2 の**免除**は「このセッションがテストを触ったか」なので、Tier 2 の検出と
    # **同じ範囲**(tdd_diff_base)から作る。Tier 1/3 の検出範囲(tdd_changed)は広いまま。
    tdd_exempt_src=$( {
        hooklib_git diff --no-ext-diff --name-only --no-renames "$tdd_base" -- backend 2>/dev/null
        hooklib_git ls-files --others --exclude-standard -- backend 2>/dev/null
    } | sed '/^$/d' | sort -u || true )

    tdd_out="$(tdd_violations "$tdd_added" "$tdd_changed" "$tdd_diff" "$tdd_exempt_src" || true)"
    if [ -n "$tdd_out" ]; then
        {
            echo; echo "=== TEST EXISTENCE CHECK BLOCKED (A2) ==="
            echo "backend のコードを足したが、対応するテストが存在しない / 触られていない。"
            echo "  - テストを書く(strict TDD: 先に Red を見る)、または"
            echo "  - 意図的にテストを持たない変更なら STOCKBOT_TDD_CHECK=off で 1 セッション無効化。"
            echo
            printf '%s\n' "$tdd_out" | sed 's/^/  /'
        } | tee -a "$log" >&2
        block_stop "tests missing for changed backend code"
    fi
    echo "test-existence check passed" >> "$log"
fi

# H1: 最後に緑だった作業ツリーと同じなら make を再実行しない。セッション範囲(開始 SHA..HEAD)は
# Go を 1 度 commit すると毎ターン載るので、何もしていないターンでも -race の全テストが走っていた
# (clean tree で 14 秒)。最終ゲートは .githooks/pre-push の make check で、ここは縮めない。
# 🛑 鍵から 1 つでも漏れると古い緑で新しいコードが通る。HEAD・追跡ファイルの diff・untracked の
# 中身・configs/ の全ファイル(gitignore された live の yaml を含む。Go のモジュール外なので
# Makefile が -count=1 を付けている理由と同じ)を入れる。
# 🛑 赤は記録しない。記録は make が緑を返した後だけ。shasum が無ければ鍵が空 = 毎回走る。
green_digest() {
    local f
    {
        hooklib_git rev-parse HEAD || true
        hooklib_git diff --no-ext-diff --binary --src-prefix=a/ --dst-prefix=b/ HEAD || true
        hooklib_git ls-files --others --exclude-standard | while IFS= read -r f; do
            [ -f "$f" ] && { echo "$f"; shasum < "$f"; }
        done || true
        find configs -type f 2>/dev/null | LC_ALL=C sort | while IFS= read -r f; do
            echo "$f"; shasum < "$f"
        done || true
    } 2>/dev/null | shasum 2>/dev/null | cut -d' ' -f1
}
green_tree="$(green_digest || true)"
green_mark="runtime/logs/pre-stop-green"
green_cached() { # $1 = 検査名 -> 0 = 同じツリーで緑だった
    [ -n "$green_tree" ] && [ "$(cat "${green_mark}.$1" 2>/dev/null || true)" = "$green_tree" ]
}

{ echo; echo "--- make check-backend ---"; } >> "$log"
if green_cached check-backend; then
    echo "skip: 最後に緑だった作業ツリーと同じ ($green_tree)" >> "$log"
else
    if ! make check-backend >> "$log" 2>&1; then
        tail -40 "$log" >&2
        block_stop "make check-backend failed"
    fi
    if [ -n "$green_tree" ]; then echo "$green_tree" > "${green_mark}.check-backend"; fi
fi

if [ -n "$changed_migration_files" ] || [ -n "$changed_db_go" ]; then
    { echo; echo "--- make test-integration (DB-related changes) ---"; } >> "$log"
    if [ -z "${INTEGRATION_TEST_DB_URL:-}" ]; then
        {
            echo; echo "=== DB INTEGRATION CHECK BLOCKED ==="
            echo "DB 関連ファイルが変更されたが INTEGRATION_TEST_DB_URL 未設定。"
            echo "*_test DB を指す DSN を export して再実行: make test-integration"
        } | tee -a "$log" >&2
        block_stop "DB integration test environment missing"
    fi
    if green_cached test-integration; then
        echo "skip: 最後に緑だった作業ツリーと同じ ($green_tree)" >> "$log"
    else
        if ! make test-integration >> "$log" 2>&1; then
            tail -40 "$log" >&2
            block_stop "make test-integration failed after DB-related changes"
        fi
        if [ -n "$green_tree" ]; then echo "$green_tree" > "${green_mark}.test-integration"; fi
    fi
fi

rm -f "$block_count_file"
echo "pre-stop checks passed (層規約 OK / migration OK / tests green)"
exit 0
