#!/bin/bash
# 日次/週次ルーチンの launchd エントリポイント。canonical は repo の scripts/stockbot-routine.sh で、
# `make routine-install` が ~/.stockbot/stockbot-routine.sh へコピーして launchd から使う。
#
# 🛑 **launchd ジョブは Desktop(= repo)に触らない**。launchd 下の /bin/bash は
# repo のファイルを読めず(Operation not permitted)、launchd 下のビルド済みバイナリは Desktop に
# 触れた瞬間に SIGKILL される(エラーも出さずジョブごと消える)。よって morning / weekly は
# ~/.stockbot の bin / env / data だけで完結する。repo 側の backend/data はその実体への symlink。
#
# tasks:
#   morning  平日 07:00 — fetch-daily + universe-screen + forward-report。プール全銘柄の取得に
#            約 40 分かかるので、立花の推奨窓(5:30〜8:00)に収まる時刻にしてある。
#            平日の祝日は fetch-daily を飛ばす(tachibana_off_hours)。
#            bot 稼働中でも安全(立花は同一ID再ログインで旧セッションが失効するが、bot 側は
#            p_errno=2 で自動再ログイン+リトライ)。ただし場中はセッションの蹴り合いになるので skip。
#   weekly   金曜 15:40 — 週次 forward-report(READ-ONLY)。
#   catchup  bot 起動前の追いつき(make start が呼ぶ)。人間のシェルで走るので
#            TCC の影響を受けず、launchd が朝ジョブを回せなかった日の本命経路になる。
#            **先頭で ensure_db**(Docker / コンテナ / DB の実在)— 満たせなければ exit 1 で bot を起動させない。
#   db-ensure  ensure_db だけを単独で回す(make start 前の確認用)。
#   cli-update  claude CLI の自己更新(`claude update`)+ 実効モデルの記録。advisor の `--model opus` は
#            CLI バージョンで解決先世代が決まるため、これを回さないとモデルは上がらない。make start が
#            起動前に呼ぶ(自己更新を望まないなら Makefile の start からこの呼び出しを外す)。
#            CLI の置き場は STOCKBOT_CLAUDE_CLI(既定 ~/.local/bin/claude)。
#   probe    診断 — launchd から repo にアクセスできるか(TCC)の確認のみ。
#   gonogo   取引日の 08:15 / 08:40 — 寄り前の銘柄判定(LLM・表示と記録だけ)。catchup の最後でも走る。
#   gonogo-probe 診断 — launchd 配下で claude CLI が認証込みで動くか。
set -uo pipefail
# repo の置き場所。優先順: STOCKBOT_REPO → このスクリプトが repo の scripts/ にあればその親 →
# $HOME/src/stock-bot。~/.stockbot へコピーした launchd 用の実体は plist の EnvironmentVariables で
# STOCKBOT_REPO を渡す。
if [ -z "${STOCKBOT_REPO:-}" ] && [ -f "$(dirname "$0")/../Makefile" ]; then
  STOCKBOT_REPO="$(cd "$(dirname "$0")/.." && pwd)"
fi
REPO="${STOCKBOT_REPO:-$HOME/src/stock-bot}"
# /usr/sbin を落とさない: lsof はここにあり、launchd の既定 PATH に無いと場中スキップの
# 条件が常に偽になる。PATH 側でも塞いでおく。
export PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin
LOGDIR="$HOME/.stockbot/logs"; TMPDIR_R="$HOME/.stockbot/tmp"
mkdir -p "$LOGDIR" "$TMPDIR_R"
VENV_PY="$HOME/.stockbot/venv/bin/python"
# claude CLI の実体。launchd の PATH には ~/.local/bin が無いので絶対パスで呼ぶ
# (既定は CLI の標準インストール先。別の場所なら STOCKBOT_CLAUDE_CLI で指す)。
CLAUDE_CLI="${STOCKBOT_CLAUDE_CLI:-$HOME/.local/bin/claude}"

# 日次ユニバース選定の事前登録パラメータ(選定規則は cmd/universe-screen)。
# 事前登録の規則: 単元 ≤ 200 万円 かつ 売買代金中央値 ≥ 10 億円/60 日(上限なし)。
# 事前登録するのは *リストではなく規則*。触るなら規則を登録し直してから。
UNIV_TOP_N=0
UNIV_MAX_LOT_JPY=2000000
UNIV_LOOKBACK=60
UNIV_MIN_TURNOVER_JPY=1000000000
# 安全弁: 選定結果がこれ未満なら**既存ファイルを書き換えない**(データ欠損の疑い)。
UNIV_MIN_COUNT=150
# 鮮度の許容(暦日)。①最終バーがデータセット最新からこれ以上遅れた銘柄は stale で落とす
# (売買停止銘柄が停止前の流動性で選ばれ続けるのを防ぐ)②データセット全体がこれ以上古ければ
# 選定結果を**書かない**(fetch-daily 停止の疑い)。7 なのは連休(GW は最大6暦日)で誤検知しないため。
UNIV_MAX_STALE_DAYS=7
UNIVERSE_FILE="$HOME/.stockbot/universe/today.txt"

# 集計期間の境界。**forward-report を引数なしで呼ばない**ための唯一の定義(perfcycles の cycles.yaml と揃える)。
STOCKBOT_CYCLE_SINCE="${STOCKBOT_CYCLE_SINCE:-2026-09-14}"

ts() { date '+%F %T'; }
BINDIR="$HOME/.stockbot/bin"

# データセットの**実パス**。repo の backend/data は ~/.stockbot/data への symlink で、launchd から
# repo 経由で辿ると Desktop を通ってしまう(TCC が実行バイナリを SIGKILL する)。
# 旧レイアウト(実体が repo 内)でも動くようフォールバックを残す。
if [ -d "$HOME/.stockbot/data" ]; then
  DATA_DIR="$HOME/.stockbot/data"
else
  DATA_DIR="$REPO/backend/data"
fi

# load_snapshot_env — launchd ジョブ用の env 解決。repo の .env が読めればそれを、読めなければ
# ~/.stockbot/env(make routine-install / make start が写す)を使う。
load_snapshot_env() {
  set -a
  # snapshot を先に、repo の .env を後に読む。**repo が正本**なので、読める環境では .env の値が
  # snapshot を上書きする(人間が .env を直したのに古い写しで走る、を防ぐ)。
  # shellcheck disable=SC1091
  [ -r "$HOME/.stockbot/env" ] && . "$HOME/.stockbot/env" 2>/dev/null
  # shellcheck disable=SC1091
  [ -r "$REPO/.env" ] && . "$REPO/.env" 2>/dev/null
  set +a
  return 0
}

# in_session_hours — 立花が「大量かつ頻繁な履歴取得は控えて」と案内している時間帯(AM8:00〜PM15:30)に
# 余裕を持たせた 08:54〜15:36 か。**morning と catchup が同じ判定を使う**(片方だけガードが無いと、
# 逃した朝の埋め合わせが場中の一括取得に化ける)。外部コマンドには依存しない。
in_session_hours() {
  local hm; hm=$(date +%H%M)
  [[ "$hm" > "0854" && "$hm" < "1536" ]]
}

# tachibana_off_hours <hard_limits.yaml> [YYYY-MM-DD] [HHMM] — 立花に触ってはいけない時間か(真 = 触らない)。
#
# 立花には**取引日の 06:00〜24:00 だけ触る**(閉局・休日・メンテ中は触らない)。06:00 は閉局
# (03:30〜05:30)明けに余裕を持たせた値。
# 取引日は bot と同じ休場カレンダー(土日 + holidays。calendar_through を過ぎたら休場扱い)で決める。
# 🛑 カレンダーが読めない / 期限切れ / 日付が読めないときは**触らない側**に倒す(bot の IsTradingDay と同じ)。
# morning は launchd(TCC で repo を読めない)なので catchup が置く ~/.stockbot/hard_limits.yaml を渡し、
# catchup は repo の configs/hard_limits.yaml を渡す。
tachibana_off_hours() {
  local cal="$1" d="${2:-$(date +%F)}" hm="${3:-$(date +%H%M)}" wd through
  wd=$(date -j -f "%Y-%m-%d" "$d" +%u 2>/dev/null) || return 0
  [ "$wd" -ge 6 ] && return 0
  [[ "$hm" < "0600" ]] && return 0
  [ -r "$cal" ] || return 0
  through=$(ymd_in_file "$cal" '^[[:space:]]*calendar_through:')
  [ -n "$(ymd_epoch "$through")" ] || return 0
  [[ "$d" > "$through" ]] && return 0
  # holidays: の直下のリストだけを見る(コメント行と行末コメントの日付は数えない)。
  awk -v d="$d" '
    /^[[:space:]]*holidays:/ { inb = 1; next }
    inb && /^[[:space:]]*-/ { if ($0 ~ "^[[:space:]]*-[[:space:]]*\"" d "\"") hit = 1; next }
    inb && /^[[:space:]]*(#.*)?$/ { next }
    inb { inb = 0 }
    END { exit hit ? 0 : 1 }' "$cal" && return 0
  return 1
}

# daily_updated_today — 日足 CSV が今日すでに書き換えられているか(mtime 基準)。
#
# 🛑 **morning と catchup が同じ判定を使う**。片方だけだと、07:00 の朝ジョブより前に `make start` を
# 打ったとき fetch-daily が丸ごと 2 回走る(プール全銘柄ぶんのリクエストが無駄になる)。
#
# mtime で見るのは「その日すでに引いたか」を知りたいから(CSV の中身の日付ではない)。
# 失敗した銘柄は書き換えられないので mtime も古いまま = 次の機会に再試行される。
daily_updated_today() {
  local ref fresh
  ref=$(mktemp); touch -t "$(date +%Y%m%d)0000" "$ref"
  fresh=$(find "$DATA_DIR" -name '*_daily.csv' -newer "$ref" -print 2>/dev/null | head -1)
  rm -f "$ref"
  [ -n "$fresh" ]
}

# ensure_db — 起動前に Postgres を「繋がる状態」にする。bot 本体より先に、原因の分かる言葉で止める。
#
# 再起動直後や久しぶりの起動では Docker Desktop の VM がまだ上がっていない / コンテナが止まっている
# ことがあり、Docker のデータが作り直されてボリュームごと消えることもある。bot 本体の ping 失敗で
# 気づくのでは遅い。
#
# やること(上から順に。どこかで満たせなければ exit 1 = make start は bot を起動しない):
#   1. Docker Desktop が応答しなければ起動して最大 180 秒待つ
#   2. コンテナが止まって / 無ければ `docker compose up -d db`(起こすだけ。止める・消す操作はしない)
#   3. pg_isready を最大 60 秒待つ
#   4. DSN(research / live)が指す DB が**存在して中身がある**かを確かめる
#   5. 欠けていれば scripts/db-restore.sh で最新 dump から戻し、4 をやり直す
# 5 は `make start` を打つこと自体を DB 書込の人間承認とみなす(CLAUDE.md §1)。
# 戻すのは**無い / 空の DB だけ**で、**dump より後に bot が動いていたら戻さず止まる**(db-restore.sh の
# 規約。欠けた台帳で live を起動しない)。空の DB のまま起動すると live は broker の実建玉を
# 「台帳に無い建玉」として読むので、戻せなかったときも黙って進まない。
ensure_db() {
  local dsns=() dsn db hostport container="${STOCKBOT_PG_CONTAINER:-stockbot-postgres}" i
  [ -n "${STOCKBOT_DATABASE_URL:-}" ] && dsns+=("$STOCKBOT_DATABASE_URL")
  [ -n "${STOCKBOT_LIVE_DATABASE_URL:-}" ] && dsns+=("$STOCKBOT_LIVE_DATABASE_URL")
  [ ${#dsns[@]} -eq 0 ] && return 0 # in-memory(paper / test)— DB 不要

  # この repo の compose(localhost:5434)以外を指す DSN は面倒を見ない。
  local ours=0
  for dsn in "${dsns[@]}"; do
    hostport=$(printf '%s' "$dsn" | sed -E 's#^[a-z]+://([^@/]*@)?([^/?]+).*#\2#')
    case "$hostport" in localhost:5434|127.0.0.1:5434) ours=1 ;; esac
  done
  [ "$ours" = "1" ] || return 0

  if ! docker info >/dev/null 2>&1; then
    echo "[$(ts)] Docker が応答しない → Docker Desktop を起動して待つ(最大 180 秒)"
    open -a Docker 2>/dev/null || { echo "[$(ts)] 🚨 Docker Desktop を起動できない(/Applications/Docker.app を確認)"; return 1; }
    for i in $(seq 1 90); do docker info >/dev/null 2>&1 && break; sleep 2; done
    docker info >/dev/null 2>&1 || { echo "[$(ts)] 🚨 180 秒待っても Docker が応答しない — Docker Desktop の画面を確認してから make start"; return 1; }
  fi

  if [ "$(docker inspect -f '{{.State.Running}}' "$container" 2>/dev/null)" != "true" ]; then
    echo "[$(ts)] $container が動いていない → docker compose up -d db"
    docker compose -f "$REPO/docker-compose.yml" up -d db || { echo "[$(ts)] 🚨 docker compose up -d db が失敗"; return 1; }
  fi
  for i in $(seq 1 30); do docker exec "$container" pg_isready -U stockbot -d postgres -q 2>/dev/null && break; sleep 2; done
  docker exec "$container" pg_isready -U stockbot -d postgres -q 2>/dev/null \
    || { echo "[$(ts)] 🚨 Postgres が 60 秒待っても応答しない(docker logs $container を確認)"; return 1; }

  local missing=()
  missing_dbs() {
    missing=()
    for dsn in "${dsns[@]}"; do
      db=$(printf '%s' "$dsn" | sed -E 's#.*/([^/?]+)(\?.*)?$#\1#')
      if [ "$(docker exec "$container" psql -U stockbot -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname = '$db'" 2>/dev/null)" != "1" ] \
         || [ "$(docker exec "$container" psql -U stockbot -d "$db" -tAc "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'" 2>/dev/null)" = "0" ]; then
        missing+=("$db")
      fi
    done
  }
  missing_dbs
  if [ ${#missing[@]} -gt 0 ]; then
    echo "[$(ts)] ⚠ DB が無い / 空: ${missing[*]}(Docker のデータが作り直された可能性)→ 最新バックアップから復元する"
    # 引数なし = db-restore.sh が知る DB 全部(他の台帳も一緒に消えているので)。
    STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 bash "$REPO/scripts/db-restore.sh" | sed "s/^/[$(ts)]    /"
    missing_dbs
    if [ ${#missing[@]} -gt 0 ]; then
      echo "[$(ts)] 🚨 復元できなかった DB: ${missing[*]} — **bot は起動しません**(理由は直上の db-restore の出力)"
      echo "[$(ts)]    新しい DB を作ったばかりなら: STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 make migrate-up"
      return 1
    fi
    echo "[$(ts)] ✅ 復元した。起動後に /api/live/dashboard で live の建玉が broker と一致するか確認すること"
  fi
  echo "[$(ts)] DB ok($(printf '%s ' "${dsns[@]}" | sed -E 's#[a-z]+://[^@]*@##g; s#\?[^ ]*##g'))"
  return 0
}

# ymd_epoch — "YYYY-MM-DD" を epoch 秒に。読めなければ**何も出さない**(呼び手は空で判定する)。
# BSD date: launchd 上の macOS 専用。GNU の `date -d` はここでは使えない。
# 00:00:00 に固定するのは、省くと date が「今の時刻」を埋めて2回の呼び出しで日数が1ずれるため。
ymd_epoch() { date -j -f "%Y-%m-%d %H:%M:%S" "$1 00:00:00" +%s 2>/dev/null; }

# ymd_in_file — <file> の <正規表現> に最初にマッチする行から、引用された日付を取り出す。
# `[^"]*` で**行内の最初**の引用に固定するのが要点。`.*"…"` は貪欲なので**最後**の引用を拾い、
# `calendar_through: "2027-12-31" # 前回 "2026-12-31"` から**古い方**を取ってしまう
# (取れた値は日付として妥当なので検証もすり抜ける)。
ymd_in_file() { grep -E "$2" "$1" 2>/dev/null | head -1 | sed -E 's/[^"]*"([0-9-]+)".*/\1/'; }

# write_expiry_manifest — 年次更新が必要な定数の写しを ~/.stockbot/expiry-manifest に置く。
# morning が読む唯一の入力で、**repo を読めるここでしか採れない**(launchd は TCC で Desktop を読めない)。
# <repo> を引数に取るのは、テストが合成ツリーで抽出規則を殺せるようにするため。.tmp + mv は部分書き込み対策。
# 抽出に失敗しても黙って空を書かない — 空の manifest は morning の警告が丸ごと消えるのと同じで、
# 年次期限チェックが防ごうとしている無音故障になる。**決して非 0 で終わらない**(make start をここで止めない)。
write_expiry_manifest() {
  local repo="$1" mfst="$HOME/.stockbot/expiry-manifest" cal fine loan
  mkdir -p "$HOME/.stockbot" 2>/dev/null
  cal=$(ymd_in_file "$repo/configs/hard_limits.yaml" '^[[:space:]]*calendar_through:')
  # 🛑 日付リテラルの置き場は 2 か所ありうる。tick_fine.go の `FineTickAsOf` が生成側の定数への
  # 委譲(`= FineTickDerivedOn`)になっていると日付が取れず、年次の tick 表チェックが黙って死ぬ
  # (合成ツリーのテストは日付リテラルを置いているので緑のまま通る)。
  # まず委譲先の実体を見て、無ければ従来の形にも当てる。
  fine=$(ymd_in_file "$repo/backend/internal/domain/market/tick_fine_gen.go" '^const FineTickDerivedOn')
  [ -n "$(ymd_epoch "$fine")" ] \
    || fine=$(ymd_in_file "$repo/backend/internal/domain/market/tick_fine.go" '^const FineTickAsOf')
  [ -n "$(ymd_epoch "$cal")" ] \
    || echo "[$(ts)] ⚠ configs/hard_limits.yaml から calendar_through を取り出せない('$cal')— 書式が変わった?"
  [ -n "$(ymd_epoch "$fine")" ] \
    || echo "[$(ts)] ⚠ tick_fine{,_gen}.go から FineTickAsOf を取り出せない('$fine')— 書式が変わった?"
  # 🛑 貸借銘柄一覧の鮮度。**ここでキーの不在を警告しない。** 存在保証は yaml を読む側(Go の
  # `TestLoanableAsOfKeyExistsAndParses`)の責任で、シェルは日数計算だけを持つ。ここで警告すると、
  # 合成 repo を食わせる stockbot-routine_test.sh の正常系が落ちる。取れなければ manifest に空行が入り、
  # check_expiries が黙って skip する — その状態は Go テストが先に落ちるので到達しない。
  loan=$(ymd_in_file "$repo/configs/hard_limits.yaml" '^[[:space:]]*loanable_symbols_as_of:')
  if { echo "calendar_through=$cal"
       echo "fine_tick_as_of=$fine"
       echo "loanable_as_of=$loan"
       echo "snapshot_at=$(date +%F)"; } > "$mfst".tmp 2>/dev/null && mv "$mfst".tmp "$mfst" 2>/dev/null; then
    echo "[$(ts)] expiry-manifest 更新: $(tr '\n' ' ' < "$mfst")"
  else
    rm -f "$mfst".tmp
    echo "[$(ts)] ⚠ expiry-manifest を書けない($mfst)— 年次期限の警告は出ません"
  fi
  return 0
}

# sync_routine_assets — launchd が起動する写しを repo に合わせる(**配布する**)。
#
# launchd の写しが repo から取り残されると、直した封鎖が朝ジョブに届かないまま動き続ける
# (警告は make start の出力に流れて誰も見ない)。catchup はバイナリ / hard_limits.yaml / env snapshot を
# 毎回配っているので、このスクリプト本体と plist も同じ扱いにする。
#
# 🛑 **原子的に置く**(tmp + mv)。Ctrl-C で切れた写しを朝ジョブが実行すると**無音で死ぬ**
# (launchd はエラーを人に見せない)。inode ごと差し替わるので、走っている最中の写しがあっても
# そちらは古い inode を読み続けて壊れない。
# 🛑 **空 / 不在の元では上書きしない**(fail-close)。取り込み失敗で写しを壊さない。
# 🛑 **失敗しても make start を止めない**(常に 0 を返す)。配布は起動の前提条件ではない。
# sync_asset は repo の 1 本を ~/.stockbot へ配る(内容が違うときだけ)。配布対象は一覧で回す
# (個別に書くと足したときに配布を忘れる)。
sync_asset() {
  local src="$1" dst="$2" label="$3" hint="$4"
  [ -s "$src" ] || return 0
  cmp -s "$src" "$dst" && return 0
  mkdir -p "$HOME/.stockbot" 2>/dev/null
  if cp "$src" "$dst.tmp" 2>/dev/null && chmod +x "$dst.tmp" 2>/dev/null && mv "$dst.tmp" "$dst" 2>/dev/null; then
    echo "[$(ts)] ${label}を更新しました(repo → ~/.stockbot)"
  else
    rm -f "$dst.tmp" 2>/dev/null
    echo "[$(ts)] ⚠ ${label}を更新できない — 旧版のまま動きます(${hint} を試す)"
  fi
  return 0
}

sync_routine_assets() {
  local repo="$1" src dst name
  sync_asset "$repo/scripts/stockbot-routine.sh" "$HOME/.stockbot/stockbot-routine.sh" \
    "朝ジョブのスクリプト" "make routine-install"
  sync_asset "$repo/scripts/db-backup.sh" "$HOME/.stockbot/db-backup.sh" \
    "DB バックアップスクリプト" "make backup-install"
  # launchd の定義は**変わったときだけ**貼り替える。毎回 unload/load すると、起動のたびに
  # エージェントが一瞬消える(その瞬間に発火時刻が来ると1回落ちる)。
  #
  # db-backup も一覧に入れる(漏れると plist を直しても launchd に届かない)。
  # gonogo は寄り前の銘柄判定(取引日の 08:15 / 08:40・表示と記録だけ)。
  # repo の plist は置き場を /Users/USERNAME(repo は /Users/USERNAME/src/stock-bot)の仮名で
  # 書いてある。写すときに実際の repo と HOME へ置換してから比較する(素の cp だと存在しない
  # パスのジョブが載り、launchd が黙って失敗する)。
  for name in morning weekly db-backup gonogo; do
    src="$repo/scripts/com.stockbot.$name.plist"
    dst="$HOME/Library/LaunchAgents/com.stockbot.$name.plist"
    [ -s "$src" ] || continue
    if ! sed -e "s#/Users/USERNAME/src/stock-bot#$repo#g" -e "s#/Users/USERNAME#$HOME#g" "$src" > "$dst.tmp" 2>/dev/null; then
      rm -f "$dst.tmp" 2>/dev/null
      echo "[$(ts)] ⚠ com.stockbot.$name.plist を更新できない"
      continue
    fi
    if cmp -s "$dst.tmp" "$dst"; then rm -f "$dst.tmp"; continue; fi
    if mv "$dst.tmp" "$dst" 2>/dev/null; then
      launchctl unload "$dst" 2>/dev/null || true
      if launchctl load "$dst" 2>/dev/null; then
        echo "[$(ts)] launchd を更新しました: com.stockbot.$name"
      else
        echo "[$(ts)] ⚠ com.stockbot.$name を再読込できない — make routine-install を試す"
      fi
    else
      rm -f "$dst.tmp" 2>/dev/null
      echo "[$(ts)] ⚠ com.stockbot.$name.plist を更新できない"
    fi
  done
  return 0
}

# forward_report_brief <全文のファイル> — 起動前の forward-report の要約(期間と ¥1M 正規化の 2 行)と全文の置き場を出す。
# 全文は約 45 行で起動のたびに端末を埋めていた。戦略別の内訳は全文のファイルと画面の戦績タブで読む。
forward_report_brief() {
  local f="$1" lines
  lines=$(grep -E '^(期間:|¥1M notional 正規化:)' "$f" 2>/dev/null)
  if [ -z "$lines" ]; then
    echo "[$(ts)] forward-report の集計を読めない(DB 停止中?)— 起動は続行。出力: $f"
    return 0
  fi
  printf '%s\n' "$lines" | sed 's/^/  /'
  echo "  (全文: $f)"
}

# copy_gonogo_configs — gonogo(launchd でも動く)が読む設定の写しを ~/.stockbot/configs へ置き、
# 一覧 gonogo-configs.txt(paper= / live_bot= / live_strategy=)を原子的に書き直す。
# 🛑 launchd 配下の gonogo は repo(Desktop)に触れた瞬間に止められるので、bot と同じ env が指す
# 設定をここ(人間のシェル)で写す。live が無効なら live の行を書かない(= live は判定しない)。
# 失敗しても make start を止めない(判定は表示と記録だけ)。
copy_gonogo_configs() {
  local dst="$HOME/.stockbot/configs" tmp f p
  mkdir -p "$dst" || return 0
  tmp="$dst/gonogo-configs.txt.tmp"
  : > "$tmp" || return 0
  # env のパスは bot の作業ディレクトリ(backend/)基準の `../configs/…` か repo 基準の `configs/…`。
  gonogo_src() { for p in "$1" "$REPO/$1" "$REPO/${1#../}" "$REPO/backend/$1"; do [ -r "$p" ] && { echo "$p"; return 0; }; done; return 1; }
  if f=$(gonogo_src "${STOCKBOT_BOT_CONFIG:-configs/bot_config.advisor.yaml}"); then
    cp "$f" "$dst/$(basename "$f")" && echo "paper=$(basename "$f")" >> "$tmp"
  fi
  if [ -n "${STOCKBOT_LIVE_BOT_CONFIG:-}" ] && [ "${STOCKBOT_LIVE_DISABLED:-}" != "1" ]; then
    if f=$(gonogo_src "$STOCKBOT_LIVE_BOT_CONFIG"); then
      cp "$f" "$dst/$(basename "$f")" && chmod 600 "$dst/$(basename "$f")" && echo "live_bot=$(basename "$f")" >> "$tmp"
    fi
    IFS=',' read -ra parts <<< "${STOCKBOT_LIVE_STRATEGY_CONFIG:-}"
    for p in "${parts[@]}"; do
      p="${p// /}"; [ -n "$p" ] || continue
      if f=$(gonogo_src "$p"); then
        cp "$f" "$dst/$(basename "$f")" && chmod 600 "$dst/$(basename "$f")" && echo "live_strategy=$(basename "$f")" >> "$tmp"
      fi
    done
  fi
  mv "$tmp" "$dst/gonogo-configs.txt" && echo "[$(ts)] gonogo の設定の写し: $(tr '\n' ' ' < "$dst/gonogo-configs.txt")"
  return 0
}

# mf_days — manifest の <key> を読み、値を $MF_VAL に、今日から見た残り日数(過去なら負)を $MF_DAYS に。
# **値が空でも黙らない**のが要点: 黙るとチェックごと消えて「警告が無い = 健全」と誤読される。
# 変数で返すのは、警告を stdout に出したまま日数も渡すため($(…) で受けると警告が呼び手に飲まれる)。
mf_days() {
  local mfst="$1" key="$2" e_today="$3" e
  MF_VAL=$(sed -n "s/^$key=//p" "$mfst" | head -1); MF_DAYS=""
  # 「再生成」だけだと、抽出元の書式が変わって空が書かれた場合に**打っても直らない**指示になる。逃げ道まで書く。
  [ -n "$MF_VAL" ] || { echo "[$(ts)] ⚠ expiry-manifest に $key が無い — make start で再生成(直らなければ抽出元の書式変更。make start の出力を見る)"; return 1; }
  e=$(ymd_epoch "$MF_VAL")
  [ -n "$e" ] || { echo "[$(ts)] ⚠ expiry-manifest の $key が日付として読めない('$MF_VAL')— make start で再生成"; return 1; }
  MF_DAYS=$(( (e - e_today) / 86400 ))
}

# check_expiries — 年次更新が必要な定数の期限を朝ログで警告する。期限切れは runtime の
# fail-close で**取引停止**になる(🛑 新規だけでなく**建玉の出口も止まる** — IsTradingDay が
# 全日 false になるので MaxHold もトレールの床もペアの決済も発火しない)が、ダッシュボードは
# 正常に見えるので目視では気づけない。値は
# catchup が書いた manifest から読む(launchd の morning は TCC で hard_limits.yaml / tick_fine.go を読めない)。
#
# **決して非 0 で終わらず、exit もしない。**壊れた manifest が日足更新を止めるのは、このチェックが防ごうと
# している「朝が黙って no-op になる」事故そのもの。値が空/読めないときも黙らない。
check_expiries() {
  local mfst="$HOME/.stockbot/expiry-manifest"
  [ -f "$mfst" ] || { echo "[$(ts)] ⚠ expiry-manifest が無い(make start で生成される)"; return 0; }
  local e_today left age
  e_today=$(ymd_epoch "$(date +%F)")
  [ -n "$e_today" ] || { echo "[$(ts)] ⚠ 本日の日付を epoch に変換できない — 期限チェックを skip"; return 0; }

  if mf_days "$mfst" calendar_through "$e_today"; then
    left=$MF_DAYS
    if   [ "$left" -lt 0 ];  then echo "[$(ts)] 🛑 休場カレンダー期限切れ($MF_VAL)— 全日が「取引日でない」に倒れ、**新規だけでなく建玉の出口も全部止まっている**(MaxHold・トレールの床・ペアの決済・引け前フラット化)。https://www.jpx.co.jp/corporate/about-jpx/calendar/ で holidays と calendar_through を更新"
    elif [ "$left" -le 30 ]; then echo "[$(ts)] 🛑 休場カレンダー残り ${left} 日($MF_VAL)— 今すぐ更新。切れると**新規が止まるだけでなく、その時点の建玉の出口(MaxHold・トレール・決済)も全部止まる**"
    elif [ "$left" -le 60 ]; then echo "[$(ts)] ⚠ 休場カレンダー残り ${left} 日($MF_VAL)— 年次更新の時期"
    fi
  fi

  if mf_days "$mfst" fine_tick_as_of "$e_today"; then
    age=$(( -MF_DAYS ))
    if   [ "$age" -ge 365 ]; then echo "[$(ts)] 🛑 FineTickAsOf が ${age} 日前($MF_VAL)— TOPIX500 の10月見直し未反映の可能性。tick_fine.go を確認"
    elif [ "$age" -ge 300 ]; then echo "[$(ts)] ⚠ FineTickAsOf が ${age} 日前($MF_VAL)— 10月見直しが近い"
    fi
  fi

  # 貸借銘柄一覧の鮮度(45 / 60 日)。
  if mf_days "$mfst" loanable_as_of "$e_today"; then
    age=$(( -MF_DAYS ))
    if   [ "$age" -ge 60 ]; then echo "[$(ts)] 🛑 貸借銘柄一覧が ${age} 日前($MF_VAL)。cmd/loanable-fetch で再生成する"
    elif [ "$age" -ge 45 ]; then echo "[$(ts)] ⚠ 貸借銘柄一覧が ${age} 日前($MF_VAL)。60 日までに cmd/loanable-fetch で再生成する"
    fi
  fi

  if mf_days "$mfst" snapshot_at "$e_today"; then
    age=$(( -MF_DAYS ))
    [ "$age" -ge 30 ] && echo "[$(ts)] ⚠ expiry-manifest が ${age} 日前 — make start で更新される"
  fi
  return 0
}

# run_bin — ~/.stockbot/bin のビルド済みバイナリを叩く。repo の make / go には依存しない
# (launchd から TCC で回せないため)。
run_bin() {
  local name="$1"; shift
  if [ ! -x "$BINDIR/$name" ]; then
    echo "[$(ts)] $name のバイナリが無い($BINDIR/$name)— make routine-install を実行"
    return 1
  fi
  "$BINDIR/$name" "$@"
}

# select_universe — その日のユニバースを選び直して $UNIVERSE_FILE へ書く。
# ローカルの日足 CSV を読むだけで、ブローカーには一切触れない。
#
# **失敗しても前日のファイルを消さない**。bot は symbols_file を読めないと起動を拒否するので、
# ここで消すと朝の選定が転んだ日に bot が上がらない。「古いユニバースで動く」方が「動かない」より
# まし — ただし必ず警告を残す。
#
# **allowed_symbols の写し(~/.stockbot/hard_limits.yaml)が無ければ選定しない**(前日のファイルを
# 残す)。データディレクトリはホワイトリストと独立に育つので、外の銘柄が today.txt に入ると
# **翌朝 bot が起動しない**(fail-close)。縮退先は「昨日のユニバース」であって「無検査の選定」ではない。
# 写しが**古い**場合、減る方向(売買停止銘柄を allowed_symbols から外したのに写しが古い)は
# 消したはずの銘柄を選んでやはり起動不能になる。だから catchup は毎回 cp する。
select_universe() {
  if [ ! -r "$HOME/.stockbot/hard_limits.yaml" ]; then
    echo "[$(ts)] ⚠⚠ ~/.stockbot/hard_limits.yaml が無い — 選定を中止(前日のユニバースを使い続けます)"
    echo "[$(ts)]    → 人間のシェルで make routine-install を実行"
    return 1
  fi
  if run_bin universe-screen -data "$DATA_DIR" \
      -top-n "$UNIV_TOP_N" -max-lot-jpy "$UNIV_MAX_LOT_JPY" \
      -lookback "$UNIV_LOOKBACK" -min-turnover-jpy "$UNIV_MIN_TURNOVER_JPY" \
      -max-stale-days "$UNIV_MAX_STALE_DAYS" \
      -allow-file "$HOME/.stockbot/hard_limits.yaml" \
      -archive-dir "$HOME/.stockbot/universe/archive" \
      -out "$UNIVERSE_FILE" -min-count "$UNIV_MIN_COUNT"; then
    echo "[$(ts)] ユニバース更新: $UNIVERSE_FILE ($(wc -l < "$UNIVERSE_FILE" | tr -d ' ') 銘柄)"
    return 0
  fi
  echo "[$(ts)] ⚠⚠ ユニバース選定に失敗 — $UNIVERSE_FILE は**前回のまま**。bot は古いユニバースで動きます"
  if [ -f "$UNIVERSE_FILE" ]; then
    echo "[$(ts)]    現行ファイル: $(wc -l < "$UNIVERSE_FILE" | tr -d ' ') 銘柄 / 更新 $(date -r "$UNIVERSE_FILE" '+%F %T')"
  else
    echo "[$(ts)]    ⚠ ファイル自体が存在しない — この状態では bot は起動を拒否します(fail-close)"
  fi
  return 1
}

case "${1:-}" in
  morning)
    # **repo にも Desktop にも触らない**(冒頭の TCC 注記)。ここが触るのは $DATA_DIR と $BINDIR だけ。
    # 年次期限の警告は**morning の最初の一手**。この後は fetch-daily(プール全銘柄・約 40 分)も分足補填も
    # ネットワーク待ちなので、後ろに置くと詰まった朝に警告が1行も出ない。
    # **load_snapshot_env より前**なのも同じ理由: snapshot に未定義変数参照が1つでも混じると
    # `set -u` が非対話 shell を即殺し、`2>/dev/null` がその理由まで飲む。
    # 末尾に置くと morning の exit code が check_expiries の 0 で上書きされ、補填の失敗が launchd から見えなくなる。
    check_expiries
    load_snapshot_env
    # 場中は無条件 skip。理由は2つあり、どちらも bot の有無に依らない:
    #   1. 立花が「大量・頻繁な履歴取得は AM8:00〜PM15:30 を控えて」と明示している
    #   2. bot が動いていれば同一 ID の再ログインでセッションを蹴り合う
    # 🛑 段の目印(`== fetch-daily ==`)は **skip する日も必ず出す**。self-test は
    # 「morning がこの段まで到達したか」「年次期限の警告がこの段より前か」を目印の
    # 行番号で見ており、skip の分岐だけ目印を出さないと**その日の morning が
    # 途中で死んだのか skip したのか区別できない**。
    echo "[$(ts)] == fetch-daily =="
    if tachibana_off_hours "$HOME/.stockbot/hard_limits.yaml"; then
      echo "[$(ts)]   skip: 立花の休場日・時間外(土日・祝日・06:00 前・カレンダーが読めない)— 立花に触らない"
    elif in_session_hours; then
      echo "[$(ts)]   skip: 場中(立花が控えるよう案内している時間帯 — 明朝に追い付く)"
    elif daily_updated_today; then
      # 07:00 より前に bot を起動すると catchup が先に引いている。同じものを
      # もう一度引くとプール全銘柄ぶんのリクエストが丸ごと無駄になる。
      echo "[$(ts)]   skip: 日足は本日すでに更新済み(catch-up が先に引いた)"
    else
      run_bin fetch-daily -out "$DATA_DIR" -bench-symbol "${STOCKBOT_BENCH_SYMBOL:-}" \
        || echo "[$(ts)] fetch-daily 失敗(CSV は無傷)— 明朝に再試行"
    fi
    # fetch が skip / 失敗した日も選び直す: 前日までの CSV で選んだ方が、選び直さずにさらに古い
    # リストを使い続けるよりよい(ローカル計算なので API も叩かない)。
    echo "[$(ts)] == universe-screen(その日のユニバース)=="
    select_universe || true
    echo "[$(ts)] == forward-report =="
    # 「DB 停止中?」だけだと、TCC で env が読めず DSN 未解決のときも DB 障害に見える。
    run_bin forward-report -since "$STOCKBOT_CYCLE_SINCE" || echo "[$(ts)] forward-report 失敗(DB 停止 / TCC / snapshot 未生成 のいずれか — 直上の行を確認)"
    ;;
  weekly)
    # 週次 forward-report。READ-ONLY(SELECT のみ)なので bot 稼働中でも安全。repo に一切依存しない
    # (バイナリは ~/.stockbot/bin、DSN は repo .env → ~/.stockbot/env の順で解決)。
    load_snapshot_env
    if [ -z "${STOCKBOT_DATABASE_URL:-}" ]; then
      echo "[$(ts)] STOCKBOT_DATABASE_URL を解決できない(.env も snapshot も読めない)— skip"
      exit 1
    fi
    echo "[$(ts)] == weekly forward-report =="
    run_bin forward-report -since "$STOCKBOT_CYCLE_SINCE" || echo "[$(ts)] forward-report 失敗(DB 停止 / TCC / snapshot 未生成 のいずれか — 直上の行を確認)"
    ;;
  db-ensure)
    cd "$REPO" || exit 1
    if [ -r ./.env ]; then set -a; . ./.env 2>/dev/null; set +a; fi
    ensure_db || exit 1
    ;;
  catchup)
    cd "$REPO" || exit 1
    # 🛑 **DB を最初に確かめる**。この下の fetch-daily は最大 40 分かかるので、DB が
    # 落ちているのに 40 分待たせてから bot の ping で落ちる、をしない。失敗は exit 1 = make start は
    # bot を起動しない(空の台帳で live を回すより、止まって人間に復元を頼む方が安全)。
    if [ -r ./.env ]; then set -a; . ./.env 2>/dev/null; set +a; fi
    ensure_db || exit 1
    # **catchup の先頭**で写す。この下に make fetch-daily(約40分)があり、朝を逃した日に中断されると写しが更新されない。
    write_expiry_manifest "$REPO"
    sync_routine_assets "$REPO"
    if daily_updated_today; then
      echo "[$(ts)] 日足は本日更新済み — catch-up 不要"
    elif tachibana_off_hours "$REPO/configs/hard_limits.yaml"; then
      # 土日・祝日・早朝に make start しても履歴は引かない。
      # 日足は前営業日の終値までしか使わないので、次の取引日の朝ジョブで追い付けば足りる。
      echo "[$(ts)] 日足が本日未更新だが**立花の休場日・時間外なので fetch-daily は skip**(土日・祝日・06:00 前)"
    elif in_session_hours; then
      # **場中は追いつかない。** プール全銘柄の履歴取得(1 銘柄 1 リクエスト × 約 40 分)を、立花が
      # 控えるよう案内している時間帯に出さない。
      # 日足は前日終値までしか使わないので、1日古いまま起動しても戦略の入力は変わらない。
      echo "[$(ts)] 日足が本日未更新だが**場中なので fetch-daily は skip**(履歴取得は立花の推奨時間帯外)"
      echo "[$(ts)]   → 前日までの日足で起動します。翌朝 07:00 の朝ジョブで追い付きます"
    else
      echo "[$(ts)] 日足が本日未更新(朝ジョブを逃した?)→ 起動前に fetch-daily で追いつく"
      echo "[$(ts)]   ⏳ プール全銘柄なので **約 40 分** かかります(この間 make start は待ちます)"
      [ -n "${STOCKBOT_TACHIBANA_AUTH_ID:-}" ] || { set -a; source ./.env; set +a; }
      make fetch-daily
    fi
    # ユニバースは **1営業日に1回だけ** 選ぶ。make stop/start は任意のタイミングで打たれる前提なので、
    # 起動のたびに選び直すと「同じ日なのに再起動でユニバースが変わった」が起こりうる。
    # ホワイトリストの写しは **毎回** 更新する(選定を skip する日も)。ここが if の中にあると、
    # 朝ジョブが成功している限り写しは make routine-install まで永久に古いままになり、
    # allowed_symbols から**削除**した銘柄が選ばれて翌朝 bot が起動しない。
    mkdir -p "$HOME/.stockbot"
    cp configs/hard_limits.yaml "$HOME/.stockbot/hard_limits.yaml" \
      || echo "[$(ts)] ⚠ hard_limits.yaml の写しを更新できない(古い写しのままです)"
    if [ -f "$UNIVERSE_FILE" ] && [ -n "$(find "$UNIVERSE_FILE" -newermt "$(date +%F) 00:00:00" -print 2>/dev/null)" ]; then
      echo "[$(ts)] ユニバースは本日選定済み($(wc -l < "$UNIVERSE_FILE" | tr -d ' ') 銘柄)— 選び直さない"
    else
      echo "[$(ts)] == universe-screen(本日ぶん未生成 → 起動前に選定)=="
      select_universe || true
    fi
    # 🛑 **repo の .env を正本として必ず読む**(シェルに残った別の DSN で集計しない)。bot 本体(Makefile の
    # LOADENV)も launchd 側(load_snapshot_env)も .env が勝つ。
    if [ -r ./.env ]; then set -a; . ./.env 2>/dev/null; set +a; fi
    if [ -n "${STOCKBOT_DATABASE_URL:-}" ]; then
      echo "[$(ts)] == forward-report(起動前の現在地)=="
      mkdir -p "$LOGDIR"
      make forward-report ARGS="-since $STOCKBOT_CYCLE_SINCE" > "$LOGDIR/forward-report-start.txt" 2>&1 \
        || echo "[$(ts)] forward-report 失敗(DB 停止中?)— 起動は続行"
      forward_report_brief "$LOGDIR/forward-report-start.txt"
    fi
    # launchd ジョブ(morning / weekly)が使うバイナリと env snapshot を、人間のシェルで更新しておく。
    # launchd は TCC で repo の make/go を回せないため、ここが唯一の鮮度維持ポイント。
    mkdir -p "$BINDIR"
    for c in forward-report fetch-daily universe-screen gonogo; do
      (cd backend && go build -o "$BINDIR/$c" "./cmd/$c") \
        || echo "[$(ts)] 🚨 $c の再ビルド失敗 — **旧バイナリは古い立花 API 版を叩く**。朝ジョブが静かに no-op になるので必ず直すこと"
      # 🛑 ビルドが成功していても、ここを通らなかった日(TCC / go が無い)は旧版のまま残る。
      # 比較パスは "$REPO/..." と絶対で書く — 相対だと cwd 次第で黙って偽になり警告が消える。
      [ "$BINDIR/$c" -nt "$REPO/backend/internal/adapter/broker/tachibana.go" ] \
        || echo "[$(ts)] 🚨 $BINDIR/$c が tachibana.go より古い — 立花 API の版が食い違っている可能性"
    done
    bash scripts/snapshot-env.sh "$REPO" || echo "[$(ts)] env snapshot の更新失敗(旧 snapshot で続行)"
    # 寄り前の銘柄判定(go/no-go・表示と記録だけ)。08:15 の launchd が動けなかった日(ネットワーク断・
    # スリープ)はここでやり直す。**バックグラウンド**で起動して bot の起動を待たせない。冪等なので
    # 判定済みの銘柄は判定し直さない。前提(取引日・今日のユニバース・日足・場中)は gonogo 自身が見る。
    copy_gonogo_configs
    if [ -x "$BINDIR/gonogo" ]; then
      STOCKBOT_GONOGO_TRIGGER=catchup nohup "$BINDIR/gonogo" >> "$LOGDIR/gonogo.log" 2>&1 < /dev/null &
      echo "[$(ts)] gonogo をバックグラウンドで起動(ログ ~/.stockbot/logs/gonogo.log・判定 ~/.stockbot/gonogo/$(date +%F).md)"
    fi
    ;;
  gonogo)
    # launchd(取引日の 08:15 と 08:40)。bot が起動していなくても判定する。**repo に触らない**
    # (~/.stockbot の bin / data / universe / configs の写しだけ)。前提を満たさなければ gonogo 自身が
    # 何もせず終わり、make start の catchup が拾う。
    echo "[$(ts)] == gonogo =="
    STOCKBOT_GONOGO_TRIGGER=launchd run_bin gonogo || echo "[$(ts)] gonogo 失敗(直上の行を確認)— make start の catchup がやり直す"
    ;;
  gonogo-probe)
    # 診断 — launchd 配下で claude CLI が**認証込みで**動くか。
    # 人間のシェルの bot の中でしか LLM を動かしたことが無いので、launchd から 1 回だけ試す。
    cd "$TMPDIR_R" || exit 1
    out=$(echo "say ok" | "$CLAUDE_CLI" -p --no-session-persistence --tools "" \
      --model opus --output-format json 2>&1)
    if printf '%s' "$out" | grep -q '"type":"result"' && ! printf '%s' "$out" | grep -q '"is_error":true'; then
      echo "[$(ts)] gonogo-probe: OK — launchd 配下で claude CLI が認証込みで動く"
    else
      echo "[$(ts)] gonogo-probe: NG — launchd 配下で claude CLI が動かない(判定は make start の catchup と手動だけになる)"
      echo "[$(ts)]   $(printf '%s' "$out" | head -c 300)"
      exit 1
    fi
    ;;
  cli-update)
    # claude CLI の自己更新 + 使用モデルの記録。失敗しても後続を止めない(exit 0)。
    CLI="$CLAUDE_CLI"
    # 更新イベントはここにしか残らないのでログにも積む(run 単位の正本は DB の advisor_runs.model)。
    CLILOG="$LOGDIR/cliupdate.log"
    say() { echo "[$(ts)] $*" | tee -a "$CLILOG"; }
    if [ ! -x "$CLI" ]; then
      say "claude CLI が $CLI に無い — 更新 skip(which claude を確認)"
      exit 0
    fi
    before=$("$CLI" --version 2>/dev/null | awk '{print $1}')
    "$CLI" update >/dev/null 2>&1 || say "claude update 失敗(次回に再試行)"
    after=$("$CLI" --version 2>/dev/null | awk '{print $1}')
    [ "$before" != "$after" ] && say "== claude CLI 更新: $before -> $after =="

    # 実効モデルは **毎回** 実測して残す。CLI バージョンが変わらなくてもエイリアスの解決先が変わる
    # ケースを拾うため。変化検出のため前回値を ~/.stockbot/last-advisor-model に保存する。
    model=$(echo "say ok" | "$CLI" -p --no-session-persistence --tools "" \
      --model opus --output-format json 2>/dev/null \
      | "$VENV_PY" -c "import json,sys; print(','.join(json.load(sys.stdin).get('modelUsage',{}).keys()) or 'unknown')" 2>/dev/null)
    model="${model:-unknown}"
    stamp="$HOME/.stockbot/last-advisor-model"
    prev=$(cat "$stamp" 2>/dev/null || echo "")
    say "claude CLI $after / advisor 実効モデル(--model opus): $model"
    if [ -n "$prev" ] && [ "$prev" != "$model" ] && [ "$model" != "unknown" ]; then
      say "🔺 モデル世代が変わった: $prev -> $model"
      say "※ モデル世代の変わり目を記録する(この日を境に別世代)"
    fi
    # 🚨 **ここを `[ ... ] && cmd` で書かない。** 分岐の最後の文なので、条件が偽だと
    # 短絡して**スクリプトの終了コードが 1 になり `make start` ごと落ちる**。
    # 実効モデルは**記録**であって起動のゲートではない。判定できなくても止めない。
    if [ "$model" != "unknown" ]; then
      echo "$model" > "$stamp"
    else
      say "⚠ 実効モデルを判定できなかった(CLI の応答が想定外 / 一時的な失敗)。記録は据え置き、**起動は止めない**"
    fi
    exit 0
    ;;
  probe)
    # 2段構えで診断する。bash と バイナリでは TCC の可否が**別々に決まる**(実測: launchd 起動の
    # /bin/bash は repo の .env を読めないが、launchd 起動の python は backend/data に書けた)。
    if cd "$REPO" && ls "$REPO/backend/data" >/dev/null 2>&1; then
      echo "[$(ts)] probe(bash): OK — launchd の bash から repo が見える"
    else
      echo "[$(ts)] probe(bash): NG — TCC で拒否(Full Disk Access に /bin/bash を追加)"
    fi
    # 本命: 朝ジョブと同じ経路(ビルド済みバイナリ)でデータに手が届くか。
    # -check は立花 API に繋がないので場中に打っても bot のセッションを蹴らない。
    if run_bin fetch-daily -check -out "$DATA_DIR" -backup-dir ""; then
      echo "[$(ts)] probe(binary): OK — 朝ジョブは日足 CSV に到達できる"
    else
      echo "[$(ts)] probe(binary): NG — 朝ジョブは日足を更新できない"
      echo "[$(ts)]   → Full Disk Access に $BINDIR/fetch-daily を追加するか、backend/data を Desktop 外へ移す"
      exit 1
    fi
    ;;
  *)
    echo "usage: stockbot-routine.sh morning|weekly|catchup|db-ensure|cli-update|probe|gonogo|gonogo-probe"
    exit 2
    ;;
esac
