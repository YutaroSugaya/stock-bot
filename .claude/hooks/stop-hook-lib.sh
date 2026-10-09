#!/usr/bin/env bash
# Stop 系 hook(pre-stop-checks.sh / docs-sync-check.sh)が共有する「セッション範囲の解決」。
# 単体では何もしない(source 用)。副作用を持つのは runtime/logs 配下の印の作成だけ。
#
# 🛑 なぜ 1 か所にするか(RF3 #5)。抽出前、この 60 行あまりは 2 本にコピーで存在し、
#   - 「直前コミットを範囲に入れるか」の述語が **3 か所・2 綴り**(片方は De Morgan の裏返し)
#   - SHA の形検証が **4 か所**
# に散っていた。どちらも**1 か所忘れたら fail-OPEN** という性質で、T12 のレビューが
# 2 巡かかった原因は全てこのブロックの中にあった。
#
# 🛑 契約: この lib は **fail-CLOSE 前提**の Stop 系専用。「どの分岐でも exit 0」が契約の
# session-start.sh はここに載せない(逆向きの契約を 1 つの lib に同居させない)。
# 呼び出し側は必ず 3 段で守る(§2.4 の地雷): [ -f ] → set +e で source して rc →
# 代表関数の command -v。`. "$lib" || {…}` は set -e の下では到達しない。
#
# 🛑 bash 3.2(macOS の /bin/bash)の語彙だけを使う。連想配列 / nameref / ${var^^} は不可。
# 戻り値は HOOKLIB_* のグローバル変数で返す(nameref が使えないため)。
#
# 🛑 ここに block_stop / log / block カウンタは入れない。メッセージも印の名前も 2 本で違い、
# 共有すると stderr から「どちらの hook が止めたか」が消える。lib 欠落時の fail-close 出口に
# block_stop が要る鶏卵もできる。

# 🛑 パスを出す git は必ずこれを通す(RF3 #1)。既定(`core.quotePath=true`)だと非 ASCII の
# パスが `"backend/internal/domain/\346\226\260…"` と C-quote されて出るため
# `^backend/.*\.go$` に当たらず、**分類だけが外れて全ルールをすり抜ける**(fail-OPEN)。
# 症状は「変更ゼロに見えて早期 exit」ではないので、早期 exit のテストでは捕まらない。
hooklib_git() { git -c core.quotePath=false "$@"; }

# stdin(Stop hook の JSON)から session_id / stop_hook_active を取る。
# 返り値: HOOKLIB_SESSION_ID / HOOKLIB_STOP_HOOK_ACTIVE / HOOKLIB_SESSION_KNOWN
#
# 🛑 jq が無い / 壊れているときの grep フォールバックを外さない(RF3 #3)。縮退すると
# (a) session_known=0 で検出範囲が作業ツリーだけに縮む(fail-OPEN 方向)、
# (b) Stop 配列の 2 本が別々の `session-start-sha.*` を書き、T12 が依存する
#     「sha と mark の対」が 1 セッション内で割れる。
# `session-start_test.sh` が jq 不在ケースを持っている = jq 不在はサポート対象。
hooklib_parse_stop_stdin() { # $1 = stdin の中身
    local raw="${1:-}"
    HOOKLIB_SESSION_ID=""
    HOOKLIB_STOP_HOOK_ACTIVE="false"
    if command -v jq >/dev/null 2>&1 && [ -n "$raw" ]; then
        HOOKLIB_SESSION_ID="$(printf '%s' "$raw" | jq -r '.session_id // empty' 2>/dev/null || true)"
        HOOKLIB_STOP_HOOK_ACTIVE="$(printf '%s' "$raw" | jq -r '.stop_hook_active // false' 2>/dev/null || echo false)"
    fi
    [ -z "$HOOKLIB_SESSION_ID" ] && HOOKLIB_SESSION_ID="$(printf '%s' "$raw" \
        | grep -oE '"session_id"[[:space:]]*:[[:space:]]*"[^"]+"' | head -1 \
        | sed -E 's/.*"([^"]+)"$/\1/' || true)"
    # stdin が空 / jq 不在でも block カウンタが他セッションと混ざらないよう PPID で分ける。
    HOOKLIB_SESSION_KNOWN=1
    if [ -z "$HOOKLIB_SESSION_ID" ]; then
        HOOKLIB_SESSION_KNOWN=0
        HOOKLIB_SESSION_ID="nosession-${PPID:-0}"
    fi
    return 0
}

# セッションの開始点(どこから今セッションか)を解決する。cwd はリポジトリのルート前提。
# 返り値: HOOKLIB_SESSIONSTART_SHA / HOOKLIB_SESSION_START_SHA / HOOKLIB_SESSION_RANGE_OK /
#         HOOKLIB_SESSION_SHA_FROM_SESSIONSTART / HOOKLIB_PREV_COMMIT_IN_SCOPE
#
# 🛑 「SessionStart が動いたか」を `session-start-sha` の**有無**で判定してはいけない(T12)。
# settings.json の Stop 配列は pre-stop-checks.sh が先で、そちらが同じファイルを backfill
# するため、2 番目の docs-sync-check.sh からは SessionStart が動かなかった環境でも
# 「既にある」に見える。空ファイルの別名でも足りない(backfill 済みの sha を開始点と誤読して
# 2 本の hook が割れる)。**SessionStart だけが書く別名の印を持ち、中身に SHA を持たせる。**
hooklib_resolve_session_scope() { # $1 = session_id
    local sid="${1:-}"
    local sha_file="runtime/logs/session-start-sha.${sid}"

    HOOKLIB_SESSIONSTART_SHA="$(cat "runtime/logs/session-start-mark.${sid}" 2>/dev/null || true)"
    # 印の中身は SHA でなければならない。形検証を 1 か所でも忘れると、壊れた印が
    # 「SessionStart が動いた」と読まれて直前コミットが範囲から落ちる(fail-OPEN)。
    printf '%s' "$HOOKLIB_SESSIONSTART_SHA" | grep -qE '^[0-9a-f]{7,64}$' || HOOKLIB_SESSIONSTART_SHA=""
    HOOKLIB_SESSION_SHA_FROM_SESSIONSTART=0
    [ -n "$HOOKLIB_SESSIONSTART_SHA" ] && HOOKLIB_SESSION_SHA_FROM_SESSIONSTART=1

    if [ ! -f "$sha_file" ]; then
        git rev-parse HEAD > "$sha_file" 2>/dev/null || true
    fi
    HOOKLIB_SESSION_START_SHA="$(cat "$sha_file" 2>/dev/null || true)"
    [ -n "$HOOKLIB_SESSIONSTART_SHA" ] && HOOKLIB_SESSION_START_SHA="$HOOKLIB_SESSIONSTART_SHA"

    HOOKLIB_SESSION_RANGE_OK=0
    if [ -n "$HOOKLIB_SESSION_START_SHA" ] \
       && git cat-file -e "${HOOKLIB_SESSION_START_SHA}^{commit}" 2>/dev/null; then
        HOOKLIB_SESSION_RANGE_OK=1
    fi

    # T12: 開始点が SessionStart から分かっているなら `HEAD~..HEAD` は**前セッションの
    # 最後のコミット**でしかない。`<sha>..HEAD` が当セッションを漏れなく覆うので、入れると
    # 「何もしていないターンで前セッションのコミットを咎める」誤検知だけが残る。
    # 分からないときだけ従来どおり見る(縮めると fail-OPEN になる)。
    HOOKLIB_PREV_COMMIT_IN_SCOPE=1
    if [ "$HOOKLIB_SESSION_SHA_FROM_SESSIONSTART" = "1" ] && [ "$HOOKLIB_SESSION_RANGE_OK" = "1" ]; then
        HOOKLIB_PREV_COMMIT_IN_SCOPE=0
    fi
    return 0
}

# 「この hook がこのセッションで初めて走ったか」。0 = 初回、1 = 2 回目以降。
#
# 🛑 これを `session-start-sha` の有無で代用してはいけない(§2.4)。Stop 配列は
# pre-stop-checks.sh が先で、そちらが同じファイルを先に作る。判定に使うと本番だけ常に
# 「2 回目以降」になり、「実装 → コミット → Stop」という標準フローが丸ごと素通りする
# (自己テストは SHA ファイルが無い環境なので緑のまま気づけない)。**hook ごとに別名の印を持つ。**
hooklib_first_stop() { # $1 = 印のパス
    local seen="${1:-}"
    [ -f "$seen" ] && return 1
    : > "$seen" 2>/dev/null || true
    return 0
}
