#!/usr/bin/env bash
# Claude Code SessionStart hook: セッション開始時に「現在地」を context へ注入する(plan §3 H1)。
#
# 動機: CLAUDE.md は「現在地の SSOT は STATUS.md」と書いているが、STATUS.md を読むかは
# 毎回モデルの判断任せだった。docs の鮮度がセッションの正確さに直結するのを構造で断ち切る。
#
# 🛑 最上位の契約は「セッション起動を絶対にブロックしない」。どの分岐を通っても exit 0。
# そのため `-e` は付けず(`set -uo pipefail` のみ)、全てのコマンドに `|| true` を添える。
# ネットワーク・立花 API・`fetch-daily -check` は一切呼ばない(総実行 1 秒未満が前提)。
#
# 副作用は 2 つだけ: T12 が読む `runtime/logs/session-start-sha.<session_id>` と、
# 「SessionStart が実際に動いた」ことを示す `session-start-mark.<session_id>`(中身は同じ SHA)を置く。
set -uo pipefail

proj="${CLAUDE_PROJECT_DIR:-$PWD}"
cd "$proj" 2>/dev/null || true

# 🚨 **git が hook の子プロセスへ渡す GIT_DIR / GIT_WORK_TREE を捨てる**(2026-08-21)。
#
# これらは cwd より優先されるので、残したままだと上の `cd "$proj"` が**無効化**され、
# `git rev-parse HEAD` が $proj ではなく**呼び出し元のリポ**の HEAD を返す。その SHA が
# session-start-sha.<sid> に書かれ、Stop 側(T12)は「別リポの HEAD からの差分」を
# セッション範囲として読む = 範囲が黙って壊れる(fail-open)。
#
# 実害の出方: `git push` の pre-push フックから `make guard` が走ると、この hook の
# 自己テストが作る「コミットゼロの一時リポ」が実リポの HEAD を拾い、t29 / t29c / t29d が
# 落ちて **push そのものが通らなくなる**(ブランチを問わず再現。2026-08-21 にサイクル3 の
# push で発覚し、単体実行では緑・`git push` 経由でだけ赤、という形で切り分けた)。
#
# この hook は自分で $proj へ cd する以上、**外から渡された git 環境は常に間違い**。
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_COMMON_DIR

# stdin の session_id は他 hook(pre-stop-checks.sh)と同じ jq + grep フォールバックで取る。
hook_stdin="$(cat 2>/dev/null || true)"
session_id=""
if command -v jq >/dev/null 2>&1 && [ -n "$hook_stdin" ]; then
    session_id="$(printf '%s' "$hook_stdin" | jq -r '.session_id // empty' 2>/dev/null || true)"
fi
if [ -z "$session_id" ]; then
    session_id="$(printf '%s' "$hook_stdin" \
        | grep -oE '"session_id"[[:space:]]*:[[:space:]]*"[^"]+"' | head -1 \
        | sed -E 's/.*"([^"]+)"$/\1/' || true)"
fi
# 名前を PPID で分けるのは pre-stop-checks.sh に合わせるため(印が他セッションと混ざらない)。
[ -z "$session_id" ] && session_id="nosession-${PPID:-0}"

# ---------------------------------------------------------------------------
# T12 が読むセッション開始 SHA。**既にあれば上書きしない** — SessionStart は
# compact / resume でも再発火するので、上書きするとセッション開始点を失う。
# 空ファイルも置かない: Stop hook は `[ -f ]` で存在だけを見るため、空を置くと
# セッション範囲が永久に空のまま固定される。
# ---------------------------------------------------------------------------
mkdir -p runtime/logs 2>/dev/null || true
sha_file="runtime/logs/session-start-sha.${session_id}"
if [ ! -f "$sha_file" ]; then
    # 非空だけでは足りない: commit ゼロのリポでは `git rev-parse HEAD` が **rc=128 なのに
    # stdout へ文字列 `HEAD` を出す**。読み手の `git cat-file -e "${sha}^{commit}"` は
    # `HEAD` に対して成功するので、`git diff HEAD..HEAD` = 空 = セッション範囲が黙って
    # 消える fail-open になる。mtime と同じく**形を検証してから採用**する。
    sha="$(git rev-parse HEAD 2>/dev/null || true)"
    printf '%s' "$sha" | grep -qE '^[0-9a-f]{7,64}$' || sha=""
    if [ -n "$sha" ]; then
        # `> file 2>/dev/null` ではリダイレクト自体の失敗(書けないディレクトリ / sid に `/`)が
        # 素の stderr に出る — SessionStart の stderr はオーナーの画面に出るので、外側で囲む。
        { printf '%s\n' "$sha" > "$sha_file"; } 2>/dev/null || true
    fi
fi

# 🛑 Stop hook 側は `session-start-sha` の**有無**で「SessionStart が動いたか」を判定できない —
# 判定に使うと、SessionStart が動かなかった環境で pre-stop-checks.sh が自分で作った印を
# docs-sync-check.sh が「セッション開始点」と誤読し、直前コミットが検査から黙って落ちる
# (fail-OPEN。plan §5 A1 の地雷そのもので、自己テスト `prestop-sha-first` が実際に捕まえた)。
#
# 🛑 **空ファイルの別名では足りない。**Stop 配列は pre-stop-checks.sh が先で、そちらが
# `session-start-sha` を backfill してしまうため、「別名の印あり + sha 欠落」の状態では
# 2 番目の docs-sync-check.sh から見ると sha が既に存在し、**pre-stop が書いた HEAD を
# セッション開始点と誤読する**(2 巡目レビュー実測: pre-stop rc=2 / docs-sync rc=0 と割れた)。
# **印そのものに SHA を持たせ**、Stop 側は「印があるなら印から開始点を読む」形にする。
# こうすると backfill された sha が SessionStart の印に化けられない。
sha_now="$(cat "$sha_file" 2>/dev/null || true)"
if printf '%s' "$sha_now" | grep -qE '^[0-9a-f]{7,64}$'; then
    { printf '%s\n' "$sha_now" > "runtime/logs/session-start-mark.${session_id}"; } 2>/dev/null || true
fi

# ---------------------------------------------------------------------------
# runtime/logs の GC(RF3 #7)。hook が置く印は誰も掃除しておらず、棚卸し時点で
# 208 ファイル / 最古 2026-06-18 が積んでいた(RF1)。
#
# 🛑 **消してよいのは下の prefix だけ。**`*.log` や人間が置いたファイルを巻き込まない。
# 🛑 **`find` は使わない** — `find … -delete` / `-exec rm` は T4 の deny ルールに当たるので、
#    実装も手動検証もやりにくい(RF2 実測)。シェル glob ループなら全経路で通る。
# 🛑 **当該セッションの印は消さない。**`docs-sync-seen` / `tdd-check-seen` / `*-block-count` は
#    「この検査がこのセッションで既に走ったか」の印で、消すと `session_first_stop=1` に
#    戻って `HEAD~..HEAD` が範囲に復活する(誤検知の復活)。30 日閾値なら現実には
#    踏まないが、原理的な穴を残さない。
# 🛑 `sha` と `mark` は同じ閾値で一緒に落ちる。なお T12 の現行設計では
#    「sha だけ消える」は無害(印が SHA を持つ)で、危険なのは mark 側 — そちらも
#    fail-CLOSED(従来動作に戻るだけ)なので、mtime ベースの GC は安全側に倒れる。
# この節はどの分岐でも exit 0 を壊さない(全て `|| true`)。
# ---------------------------------------------------------------------------
gc_now="$(date +%s 2>/dev/null || echo 0)"
if [ "$gc_now" -gt 0 ] 2>/dev/null; then
    gc_cutoff=$(( gc_now - 30 * 24 * 60 * 60 ))
    for gc_pfx in session-start-sha session-start-mark enforcement-ack \
                  pre-stop-block-count docs-sync-block-count \
                  docs-sync-seen tdd-check-seen; do
        for gc_f in runtime/logs/"$gc_pfx".*; do
            [ -f "$gc_f" ] || continue
            # 当該セッションの印は対象外(prefix.<session_id> / prefix.<session_id>.<digest>)
            case "$gc_f" in
                "runtime/logs/${gc_pfx}.${session_id}"|"runtime/logs/${gc_pfx}.${session_id}."*) continue ;;
            esac
            # mtime の取り方は GNU(-c %Y)と BSD(-f %m)で違う。ここは下の日足鮮度と同じ罠 —
            # GNU の `-f` は **filesystem 表示**で、rc=1 で終わる前に stdout へ FS 情報を吐く
            # (`2>/dev/null` は stderr しか止めない)。BSD 形を先に試すと Linux では
            # 「FS ダンプ + 改行 + 正しい epoch」が入り、次行の整数比較が落ちて GC が一度も
            # 動かない。GNU 形を先に試し、**どちらも数値の形を検証してから**採用する。
            gc_mt=""
            gc_cand="$(stat -c %Y "$gc_f" 2>/dev/null || true)"
            case "$gc_cand" in ''|*[!0-9]*) ;; *) gc_mt="$gc_cand" ;; esac
            if [ -z "$gc_mt" ]; then
                gc_cand="$(stat -f %m "$gc_f" 2>/dev/null || true)"
                case "$gc_cand" in ''|*[!0-9]*) ;; *) gc_mt="$gc_cand" ;; esac
            fi
            [ -n "$gc_mt" ] || continue
            [ "$gc_mt" -lt "$gc_cutoff" ] 2>/dev/null && rm -f "$gc_f" 2>/dev/null
        done
    done
fi
true   # GC の結果で hook の exit code を変えない

# ---------------------------------------------------------------------------
# 出力(合計 40 行以内)。内訳は 見出し1 + STATUS 27(見出し1 + 本文25 + 切った旨1)
# + 実行状態 5 + コミット 4 = 37 行。
# 一律の `head -40` で頭打ちにはしない — それだと STATUS.md が伸びたときに
# 後ろ(コミット履歴)が黙って消え、自己テストの行数検査も素通りしてしまう。
# ---------------------------------------------------------------------------
echo "=== stock-bot: セッション開始時の現在地(SessionStart hook) ==="

# STATUS.md は注入する範囲を `<!-- now:begin -->` / `<!-- now:end -->` で囲んでいる
# (REVIEW_TASKS D1)。先頭 N 行で切ると表や文の途中で切れる。
# マーカーが揃っていない・中身が空のときは先頭 25 行に倒す(黙って空にしない)。
# 節が長すぎるときは 25 行で切り、切ったことを 1 行で言う(40 行の予算を守る)。
status_max=25
if [ -r docs/runtime/STATUS.md ]; then
    now_sec="$(awk '/<!-- now:begin -->/ { f = 1; b = 1; next }
                    /<!-- now:end -->/   { if (f) { e = 1; exit } }
                    f { print }
                    END { if (!(b && e)) exit 3 }' docs/runtime/STATUS.md 2>/dev/null)"
    now_rc=$?
    if [ "$now_rc" -eq 0 ] && [ -n "$now_sec" ]; then
        echo "--- docs/runtime/STATUS.md(現在地の節・SSOT) ---"
        now_n="$(printf '%s\n' "$now_sec" | wc -l | tr -d ' ')"
        printf '%s\n' "$now_sec" | head -"$status_max" 2>/dev/null || true
        if [ "$now_n" -gt "$status_max" ] 2>/dev/null; then
            echo "(現在地の節が ${now_n} 行あるので ${status_max} 行で切った — 続きは STATUS.md を読む)"
        fi
    else
        echo "--- docs/runtime/STATUS.md(先頭 ${status_max} 行・now マーカーが揃っていない) ---"
        head -"$status_max" docs/runtime/STATUS.md 2>/dev/null || true
    fi
else
    echo "--- docs/runtime/STATUS.md ---"
    echo "(docs/runtime/STATUS.md が読めない)"
fi

echo "--- 実行状態 ---"
# ポートは Makefile の STOCKBOT_ADDR と同じ導出。8090 を焼き込むと、.env で
# STOCKBOT_HTTP_ADDR を変えて起動した日に「稼働中の bot を not running」と誤報告する。
bot_addr="${STOCKBOT_HTTP_ADDR:-127.0.0.1:8090}"
bot_port="$(printf '%s' "$bot_addr" | sed 's/.*://' || true)"
if command -v lsof >/dev/null 2>&1; then
    # `-nP` は名前解決を止めるため(付けないと数百 ms かかる日がある)。
    if lsof -nP -tiTCP:"$bot_port" -sTCP:LISTEN >/dev/null 2>&1; then
        echo "bot: RUNNING ($bot_addr)"
    else
        echo "bot: not running ($bot_addr)"
    fi
else
    echo "bot: 不明(lsof が PATH に無い)"
fi

# 既定値は backend/internal/config/env.go の `runtime/emergency_stop.flag`。bot の cwd は
# backend なのでリポジトリ直下から見ると backend/runtime/... になる(Makefile の reset-trades と同じ)。
emg_found=""
for f in "${STOCKBOT_EMERGENCY_FLAG:-}" backend/runtime/emergency_stop.flag runtime/emergency_stop.flag; do
    if [ -n "$f" ] && [ -e "$f" ]; then emg_found="$f"; break; fi
done
if [ -n "$emg_found" ]; then
    echo "emergency_stop(research): 🛑 発動中 ($emg_found) — 新規 entry 停止中。再開は POST /api/emergency-resume"
else
    echo "emergency_stop(research): 無し"
fi

# live の flag は track 別で既定値が無く(config/hybrid.go の STOCKBOT_LIVE_EMERGENCY_FLAG)、
# パスは .env にしか無い。research の flag だけを見ていると、live が止まっていても「無し」と出る。
# 🛑 .env からはこの 1 キーの値だけを取り出し、**値(パス)は出力しない**(.env はキー名だけ読む規約)。
# .env は make が `set -a; . ./.env` で読むが、ここでは source しない(他の秘密値をこのプロセスに
# 載せない)。形は `[export ]KEY=値` で、値の両端の引用と先頭の ~/ ・$HOME/ ・${HOME}/ だけを解く。
# 相対パスは bot の cwd(backend/)基準で解く(make start は cd backend してから起動する)。
live_flag=""
live_key_found=0
if [ -r .env ]; then
    live_line="$(grep -E '^[[:space:]]*(export[[:space:]]+)?STOCKBOT_LIVE_EMERGENCY_FLAG=' .env 2>/dev/null | tail -1 || true)"
    if [ -n "$live_line" ]; then
        live_key_found=1
        live_flag="${live_line#*=}"
        case "$live_flag" in
            \"*\") live_flag="${live_flag#\"}"; live_flag="${live_flag%\"}" ;;
            \'*\') live_flag="${live_flag#\'}"; live_flag="${live_flag%\'}" ;;
        esac
    fi
fi
if [ "$live_key_found" -eq 0 ] && [ -n "${STOCKBOT_LIVE_EMERGENCY_FLAG:-}" ]; then
    live_key_found=1
    live_flag="$STOCKBOT_LIVE_EMERGENCY_FLAG"
fi
case "$live_flag" in
    "~/"*)        live_flag="${HOME:-}/${live_flag#\~/}" ;;
    "\$HOME/"*)   live_flag="${HOME:-}/${live_flag#\$HOME/}" ;;
    "\${HOME}/"*) live_flag="${HOME:-}/${live_flag#\$\{HOME\}/}" ;;
esac
if [ -z "$live_flag" ]; then
    echo "emergency_stop(live): 不明(.env に STOCKBOT_LIVE_EMERGENCY_FLAG が無い — 上の表示は research のみ)"
else
    live_hit=0
    case "$live_flag" in
        /*) [ -e "$live_flag" ] && live_hit=1 ;;
        *)  { [ -e "backend/$live_flag" ] || [ -e "$live_flag" ]; } && live_hit=1 ;;
    esac
    if [ "$live_hit" -eq 1 ]; then
        echo "emergency_stop(live): 🛑 発動中(.env の STOCKBOT_LIVE_EMERGENCY_FLAG)— 新規 entry 停止中。再開は POST /api/live/emergency-resume"
    else
        echo "emergency_stop(live): 無し"
    fi
fi

# backend/data は ~/.stockbot/data への symlink。壊れていたら実体を直接見る。
# `$HOME` は `${HOME:-}` で守る — 裸で書くと `set -u` が for のリスト展開で発火し、
# backend/data が在っても hook が exit 1 で途中終了する(= 最上位の契約を破る)。
data_dir=""
for d in backend/data "${HOME:-}/.stockbot/data"; do
    if [ -d "$d" ]; then data_dir="$d"; break; fi
done
newest=""
[ -n "$data_dir" ] && newest="$(ls -t "$data_dir"/*_daily.csv 2>/dev/null | head -1 || true)"
if [ -n "$newest" ]; then
    # 🛑 GNU stat の `-f` は **filesystem 表示**で、rc=1 なのに stdout へ FS 情報を吐く。
    # BSD 形を先に試すと Linux でそのゴミが mtime 欄に出る(自己テストは緑のまま)。
    # GNU 形を先に試し、**どちらも出力の形を検証してから**採用する。
    mt=""
    cand="$(stat -c '%y' "$newest" 2>/dev/null | cut -c1-16 || true)"
    printf '%s' "$cand" | grep -qE '^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}$' && mt="$cand"
    if [ -z "$mt" ]; then
        cand="$(stat -f '%Sm' -t '%F %H:%M' "$newest" 2>/dev/null || true)"
        printf '%s' "$cand" | grep -qE '^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}$' && mt="$cand"
    fi
    echo "data: 最新 $(basename "$newest") = ${mt:-不明} ($data_dir)"
else
    echo "data: *_daily.csv が見つからない(${data_dir:-データディレクトリ不明})"
fi

echo "--- 直近コミット ---"
git log --oneline -3 2>/dev/null || true

exit 0
