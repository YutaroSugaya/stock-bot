#!/usr/bin/env bash
# PreToolUse deny hook。破壊的な Bash コマンドを exit 2 で deny する。
# 公式仕様上 PreToolUse の exit 2 は defaultMode=bypassPermissions でも貫通する唯一の機械的防壁。
# 逃げ道: 人間の手動実行 / *_test DSN / STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 前置。
# テスト: bash .claude/hooks/pretooluse-deny_test.sh
set -euo pipefail

input="$(cat 2>/dev/null || true)"

tool=""
if command -v jq >/dev/null 2>&1; then
  tool="$(printf '%s' "$input" | jq -r '.tool_name // empty' 2>/dev/null || true)"
fi
[ -z "$tool" ] && tool="$(printf '%s' "$input" | grep -oE '"tool_name"[[:space:]]*:[[:space:]]*"[^"]+"' | head -1 | sed -E 's/.*"([^"]+)"$/\1/')"

[ "$tool" != "Bash" ] && exit 0

cmd=""
if command -v jq >/dev/null 2>&1; then
  cmd="$(printf '%s' "$input" | jq -r '.tool_input.command // empty' 2>/dev/null || true)"
fi
[ -z "$cmd" ] && cmd="$(printf '%s' "$input" | sed -n 's/.*"command"[[:space:]]*:[[:space:]]*"\(.*\)".*/\1/p')"
[ -z "$cmd" ] && cmd="$input"

deny() {
  echo "DENIED by pretooluse-deny.sh (plan §8.3): $1" >&2
  echo "絶対ルール違反です。CLAUDE.md 参照。意図的に必要なら人間が手動実行してください。" >&2
  exit 2
}

# enforcement 対象パスの正本は enforcement-paths.sh(RF3 #6)。綴りを再掲しない(2 綴りになると
# 必ず片方だけ更新されて穴になる)。🛑 読めなければ fail-CLOSE — fail-open にすると
# `: > enforcement-paths.sh` の 1 手でこの保護を無効化できる。
enf_hook_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
enf_path_lib="${enf_hook_dir}/enforcement-paths.sh"
[ -f "$enf_path_lib" ] || deny "enforcement-paths.sh が見つからない($enf_path_lib)。判定不能なので止める。"
set +e
# shellcheck source=/dev/null
. "$enf_path_lib"
enf_path_lib_rc=$?
set -e
[ "$enf_path_lib_rc" -eq 0 ] || deny "enforcement-paths.sh の読み込みに失敗(rc=$enf_path_lib_rc)。判定不能なので止める。"
[ -n "${ENF_CMD_RE:-}" ] || deny "enforcement-paths.sh に ENF_CMD_RE が無い(壊れた lib)。判定不能なので止める。"

# STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 は **live DB への raw write だけを許す narrow escape** で、
# グローバルなバイパスではない(honor するのは下の psql-write 分岐のみ)。DB 破壊 / container
# teardown / integration test / data wipe / postgres・bot kill は前置しても deny される。

# 閲覧・記録系は先に素通し(2026-07-31: 「docker stop」「pg_restore」を**言及しただけ**の
# grep / git commit -m が誤 deny された)。条件は単一行 + パイプ・連結・置換なし +
# 先頭が read/record 動詞。単一行を要求するのは下の先頭動詞判定が grep = 行単位で、複数行だと
# 2 行目以降が何でも素通しするため(2026-08-07 実測: "ls -la<改行>rm -rf .docker-data/postgres" が exit 0)。
NL=$'\n'
BS='\'

# 引用状態を左から 1 文字ずつ走査し、$2 の文字が**引用の外に**現れたら 0 を返す。
# 正規表現で引用領域を剥がす方式は使えない。sed の '[^']*' / "[^"]*" は引用の種類が交互に
# 現れると対応を取り違え、**本物の区切り改行ごと消す**(2026-08-07 実測。剥がす順序を 2 通り
# 試しても両方で消え、bash は 2 行目を実行するのに exit 0 だった):
#   echo "'" '"'  /  dropdb stockbot  /  echo "'"
# 引用が閉じないまま終わったら判定を放棄して「あり」= 安全側に倒す。
# bash は local の引数を**実行前に**全て展開するので、n=${#s} を同じ local に並べると外側の
# (未定義の)s を読んで set -u で落ちる。宣言を 2 行に割る。同じ理由で `[ ... ] && x` の裸の
# AND リストも使わない(偽のとき set -e が hook を殺し、exit 1 = deny ではないので fail-open)。
has_unquoted() {
  local s="$1" want="$2" i=0 ch st=0 close=''   # st: 0=引用の外 1=' の中 2=エスケープの効く引用の中
  local n=${#s}
  # 1 文字ずつの走査は長い入力で重い(実測 40KB で数秒)。長すぎるものは走査せず「あり」を返す
  # (呼び出し側 3 箇所とも「あり」= 早期素通し不可 / escape 不可 = 安全側)。
  if [ "$n" -gt 8000 ]; then return 0; fi
  while [ "$i" -lt "$n" ]; do
    ch="${s:i:1}"
    if [ "$st" -eq 0 ]; then
      case "$ch" in
        "$BS") i=$((i + 1)) ;;          # 引用の外の \ は次の1文字をエスケープ
        "'")   st=1 ;;
        '"')   st=2; close='"' ;;
        # $'...' は " と同じくエスケープが効くが ' で閉じる。' の中(st=1)として扱うと \' で
        # 閉じたと誤認し、後続の ' で状態が戻って本物の改行を引用の中だと見なす(2026-08-07 実測、
        # bash は 2 行目を実行するのに exit 0: `echo $'\''` / `rm -rf .docker-data/postgres` / `echo 'x`)。
        # 閉じ ' の無い `echo $'\'` は 1 個の無害な echo なので素通しで正しい(混同注意)。
        '$')   if [ "${s:$((i + 1)):1}" = "'" ]; then
                 st=2; close="'"; i=$((i + 1))
               else
                 case "$want" in *'$'*) return 0 ;; esac
               fi ;;
        *)     case "$want" in *"$ch"*) return 0 ;; esac ;;
      esac
    elif [ "$st" -eq 1 ]; then
      case "$ch" in "'") st=0 ;; esac   # ' の中ではエスケープは効かない
    else
      case "$ch" in
        "$BS")    i=$((i + 1)) ;;
        "$close") st=0 ;;
      esac
    fi
    i=$((i + 1))
  done
  if [ "$st" -ne 0 ]; then return 0; fi
  return 1
}

# 引用領域を 1 語に畳んだコピー。「引用の中の**言及**」と「引用された 1 個の引数」を分ける道具:
#   git commit -m "fix: rm backend/data の穴"  -> git commit -m  Q      (言及は消える)
#   rm -rf "$HOME/.stockbot"                   -> rm -rf $HOME/.stockbot(引数は残る)
# 中身をそのまま出すときメタ文字は不透明化する。生で戻すと下流の `[^|;&]*` スパンがそれを
# 区切りと誤読してマッチが死ぬ(`rm -rf 'a|b' backend/data` で実削除を確認。bash の argv は
# <a|b> <backend/data> の 2 引数)。`sed -i 's/a|b/c/' backend/data/x.csv` の日常形で起きる。
SBMETA=';&|<>()`'
# 畳み込みの上限は**上限超過時の fail-close 判定と同じ値**にする。ずれると「畳まれないので言及が
# 誤 deny される」帯と「長すぎるので fail-close する」帯の間に隙間ができる(8000 のとき、8100 字の
# コメント 1 行で言及が誤 deny された)。
SBMAXLEN=16000
# 🛑 引用領域は 2 種類あり、**どちらかは深さでは決まらない**(T13 経路1)。
#   code    = 囲っている argv[0] がシェル(`sh -c` / `bash -lc` / `eval` / `… | sh` / `<<<`)。
#             内側シェルが再解釈するので中の `< > |` は本物の演算子 → 引用符ごと再帰して残す。
#   literal = それ以外。1 個の引数なので下流の `[^|;&]*` スパンを切ってはいけない → Q に潰す。
# 深さで索る実装を 2 度書いて 2 度とも P0 回帰(1段=45件 / 2段=81件)。1段は内側シェルの本物の
# 演算子を潰し、2段は本文の中の 2 個目のシェルを literal と誤判定した。
FOLD_MAXDEPTH=8
FOLD_OUT=''
FOLD_PIECE=''

# `out` / `lit` は呼び出し元(_fold_subst)のローカルを動的スコープで共有する。_fold_scan は
# out を自分の local で覆うので、こちらの out を壊さない。
_fold_flush_lit() {
  case "$lit" in
    '')            return 0 ;;
    *[[:space:]]*) out="$out Q " ;;
    *)             out="$out${lit//[$SBMETA]/Q}" ;;
  esac
  lit=''
}

# 二重引用の中身を「置換スパン = コード」「それ以外 = literal」に分けて畳む。
_fold_subst() {
  local b="$1" d="$2" i=0 ch nx out='' lit='' par j k inner
  local n=${#b}
  while [ "$i" -lt "$n" ]; do
    ch="${b:i:1}"; nx="${b:$((i + 1)):1}"
    if [ "$ch" = '`' ]; then
      j=$((i + 1)); inner=''
      while [ "$j" -lt "$n" ] && [ "${b:j:1}" != '`' ]; do inner="$inner${b:j:1}"; j=$((j + 1)); done
      _fold_flush_lit
      _fold_scan "$inner" $((d + 1))
      out="$out\`$FOLD_OUT\`"
      i=$((j + 1)); continue
    fi
    if [ "$ch" = '$' ] && [ "$nx" = '(' ]; then
      j=$((i + 2)); par=1; inner=''
      while [ "$j" -lt "$n" ] && [ "$par" -gt 0 ]; do
        k="${b:j:1}"
        case "$k" in '(') par=$((par + 1)) ;; ')') par=$((par - 1)) ;; esac
        [ "$par" -gt 0 ] && inner="$inner$k"
        j=$((j + 1))
      done
      _fold_flush_lit
      _fold_scan "$inner" $((d + 1))
      out="$out\$($FOLD_OUT)"
      i=$j; continue
    fi
    lit="$lit$ch"; i=$((i + 1))
  done
  _fold_flush_lit
  FOLD_PIECE="$out"
}

# 引用を閉じたときの出力片を決める。$1 = ここまでの出力(= 引用の直前までの綴り)。
_fold_close() {
  if [ "$6" = "1" ] || [[ $1 =~ $SHCODEAT ]]; then
    # 深すぎる入れ子は畳まずそのまま返す(fail-close 側: 生と同じ = 従来の NOFOLD 相当)。
    if [ "$5" -lt "$FOLD_MAXDEPTH" ]; then
      _fold_scan "$4" $(($5 + 1))
      FOLD_PIECE="$2$FOLD_OUT$3"
    else
      FOLD_PIECE="$2$4$3"
    fi
    return 0
  fi
  # 🛑 `-tags` / `GOFLAGS=` の**値**は畳まない。畳むと T8(integration タグ)の判定材料が丸ごと
  # 消える(`GOFLAGS="-mod=mod -tags=integration"` が `GOFLAGS= Q ` になる)。畳む前に sed で
  # 引用を外す旧案は**深さ 0 でしか効かず**入れ子の引用を壊したので、位置(直前の綴り)で見る。
  if [[ $1 =~ $SBKEEPAT ]] || [[ $4 =~ ^GOFLAGS= ]]; then
    FOLD_PIECE="${4//[$SBMETA]/Q}"
    return 0
  fi
  # 🛑 二重引用の中の `$( )` / backtick は bash が**実行する**(単一引用の中は literal)。畳んで
  # 消すと `git commit -m "docs: `bash -c \"rm -rf ~/.stockbot/data\"` の穴"` が素通りする。
  # ただし**中身を丸ごとコード扱いにしてはいけない** — 走るのは置換の中だけで本文は走らないので、
  # `git commit -m "docs: go test -tags=integration $(go list ./...) の穴"` を 14 件誤 deny した。
  if [ "$2" = '"' ] && [ "$5" -lt "$FOLD_MAXDEPTH" ]; then
    case "$4" in
      *'`'*|*'$('*) _fold_subst "$4" "$5"; return 0 ;;
    esac
  fi
  case "$4" in
    *[[:space:]]*) FOLD_PIECE=' Q ' ;;
    *)             FOLD_PIECE="${4//[$SBMETA]/Q}" ;;
  esac
}

# 結果は FOLD_OUT に置く(`$( )` で受けると末尾の改行が落ちるうえ、入れ子ごとに fork する)。
_fold_scan() {
  local s="$1" depth="$2" i=0 ch st=0 close='' openq='' out='' buf='' ech='' codeall=0
  local n=${#s}
  # 🛑 「stdin でシェルに渡る」形(`… | sh` / `<<<`)はコードが**引用より後ろ**のシェルへ行くので、
  # 引用の直前の綴りでは判定できない。その階層の引用は全て code。
  # 🛑 **階層ごとに判定する。**全体で 1 回だけだと (a) 全深さに効かせれば内側の `'a|b'` まで
  # code になって穴が 1 段奥で再現し、(b) 深さ 0 に限れば `bash -c "echo \"…\" | sh"` の内側で
  # 判定が消える(実測 190 件)。
  if [[ $s =~ $NOFOLDINX ]]; then codeall=1; fi
  while [ "$i" -lt "$n" ]; do
    ch="${s:i:1}"
    if [ "$st" -eq 0 ]; then
      case "$ch" in
        # エスケープされた文字は**残す**(捨てると `go test -tags=\integration` の argv が bash 上
        # `-tags=integration` なのに `-tags=ntegration` になって素通りした。2026-08-08 実測)。
        # ただし**メタ文字だったものを生で戻してはいけない** — 下流の `[^|;&]*` スパンが区切りと
        # 誤読し、`rm -rf \; backend/data` が exit 0 になって使い捨てツリーの backend/data が実際に
        # 消えた(bash は `;` を rm の**引数**として渡す)。`\<改行>` は行継続なので bash と同じく消す。
        "$BS") i=$((i + 1)); ech="${s:i:1}"
               case "$ech" in
                 "$NL")                                 ;;
                 ';'|'&'|'|'|'<'|'>'|'('|')'|'`'|' ')   out="$out Q " ;;
                 *)                                     out="$out$ech" ;;
               esac ;;
        "'")   st=1; openq="'"; close="'"; buf='' ;;
        '"')   st=2; openq='"'; close='"'; buf='' ;;
        '$')   if [ "${s:$((i + 1)):1}" = "'" ]; then st=2; openq="\$'"; close="'"; buf=''; i=$((i + 1))
               else out="$out$ch"; fi ;;
        *)     out="$out$ch" ;;
      esac
    elif [ "$st" -eq 1 ]; then
      case "$ch" in
        "'") st=0; _fold_close "$out" "$openq" "$close" "$buf" "$depth" "$codeall"; out="$out$FOLD_PIECE" ;;
        *)   buf="$buf$ch" ;;
      esac
    else
      case "$ch" in
        # 二重引用の中の `\X` は**中身を残す**。捨てると `\$HOME` が `HOME` になり SBROOT が当たらない
        # (外側の " が \$ を literal $ にし、内側シェルが展開するので実際に消える)。
        "$BS")    i=$((i + 1)); buf="$buf${s:i:1}" ;;
        "$close") st=0; _fold_close "$out" "$openq" "$close" "$buf" "$depth" "$codeall"; out="$out$FOLD_PIECE" ;;
        *)        buf="$buf$ch" ;;
      esac
    fi
    i=$((i + 1))
  done
  FOLD_OUT="$out"
}

fold_quoted() { _fold_scan "$1" 0; printf '%s' "$FOLD_OUT"; }

NOFOLDW='(([A-Za-z_][A-Za-z0-9_]*=[^[:space:]]*|env|nohup|time|command|xargs|su|sudo|then|do|else|elif)[[:space:]]+[^|;&]*)*'
NOFOLDSH='(/[A-Za-z0-9_/.-]*)?((ba|z|da|a|k|c|tc)?sh|fish|su)'
# シェルはコードを -c 以外に stdin でも受け取る(`echo "rm …" | sh` / `bash <<< "rm …"`)。フラグだけ
# でなく**オペランド**も許す — `sh -s -- foo` / `bash -s x y z` で NOFOLD が外れ T8 も T4 も素通りした。
NOFOLDIN='(\|[[:space:]]*(sudo[[:space:]]+)?'"$NOFOLDSH"'\b([[:space:]]+[^[:space:];&|]+)*[[:space:]]*($|[;&|])|'"$NOFOLDSH"'\b[^|;&]*<<<)'
NOFOLD='(^|[;&|(){}`])[[:space:]]*'"$NOFOLDW"'(eval\b|'"$NOFOLDSH"'\b[^|;&]*[[:space:]]-[a-zA-Z]*c\b)|'"$NOFOLDIN"
# 「この引用はシェルのコード引数か」= NOFOLD の -c / eval 側を**引用の直前で終わる形**にしたもの。
# 🛑 `\b` を使わない。bash の `[[ =~ ]]` は regcomp なので **`\b` は非対応**、黙って非マッチ = 全ての
# 引用が literal 扱いになり 50 件が一斉に赤くなった(境界は先頭のアンカーと末尾の `$` が与える)。
# 🛑 アンカークラスに**改行を入れる**。bash の `^` は文字列の先頭だけなので、忘れると 2 行目以降の
# シェル起動が当たらず `ls -la<改行>bash -c "rm runtime/emergency_stop"` が exit 0(実削除確認)。
# SBSEG / SBSEGX を 2 綴りに分けているのと**同じ罠**。片方だけ直すと必ずここで再発する。
SHCODEAT='(^|['"$NL"';&|(){}`])[[:space:]]*'"$NOFOLDW"'(eval|'"$NOFOLDSH"'[^|;&]*[[:space:]]-[a-zA-Z]*c)[[:space:]]*$'
# NOFOLDIN の `[[ =~ ]]` 版(同上の理由で `\b` を外す。境界は後続の要求で担保される)。
# 畳まずに残す「フラグの値」の直前の綴り(`-tags=` / `-tags ` / `GOFLAGS=`)。
SBKEEPAT='(--?tags[=[:space:]]+|GOFLAGS=)$'
NOFOLDINX='(\|[[:space:]]*(sudo[[:space:]]+)?'"$NOFOLDSH"'([[:space:]]+[^[:space:];&|]+)*[[:space:]]*($|['"$NL"';&|])|'"$NOFOLDSH"'[^|;&]*<<<)'
SQ="'"

# 走査は改行を含むコマンドのときだけ(通常のコマンドにコストを掛けない)。裸の AND リストに
# しないのは has_unquoted の注記と同じ理由(set -e が hook を殺し fail-open になる)。
is_single_line=1
case "$cmd" in
  *"$NL"*) if has_unquoted "$cmd" "$NL"; then is_single_line=0; fi ;;
esac

# 先頭動詞は**1 行目だけ**を見る。原文全体に grep をかけると ^ が 2 行目以降にも当たり、
# `rm -rf .docker-data/postgres "x<改行>grep foo"` が素通しする。
first_line="${cmd%%"$NL"*}"

# メタ文字も「引用の外にあるか」で判定する。原文を grep すると検索パターン内の | をパイプと取り違える
# (2026-08-07: `grep -nEi 'drop|truncate|pg_restore' Makefile` が誤 deny)。`>` は read 動詞を write に
# 変え、`<` はプロセス置換が中を**実行する**(`cat <(dropdb stockbot)` は exit 0)。`$(` と backtick は
# "..." の中でも発火するので**原文のまま**見る。

# find は read 動詞リストにあるので、早期素通しに任せると -delete / -exec rm が通る(2026-08-08 実測:
# `find . -name "*_daily.csv" -exec rm {} +` が exit 0)。この 1 ルールだけ早期素通しより**前**に置き、
# 誤 deny を避けるため find が argv[0] の位置にあるときだけ見る。削除経路は -execdir / -ok /
# -exec /bin/rm / -exec sh -c の 5 形。ただし `-exec sh -c` を丸ごと deny すると read-only の常用形
# (`-exec sh -c 'gofmt -l "$1"' _ {} \;`)まで死ぬので、シェル経由は引数側に削除動詞があるときだけ。
# パスでスコープしないのは意図的(どこで打っても再帰削除は再帰削除)。畳むのは `find … -regex 'a|b'
# -delete` の | が span を切るため。**「find を含むか」では枝刈りしない** — 畳むこと自体がトークンを
# 作る(`'fi'nd . -delete` は畳むと find になる)。
find_src="$cmd"
case "$cmd" in
  *[\"\'\\]*)
    # 🛑 **NOFOLD で丸ごと畳み込みを止めない**(T13 経路1)。止めると引用の中のメタ文字が生で
    # 下流の `[^|;&]*` スパンに届き、`bash -c "rm -rf 'a|b' backend/data"` が素通りする。
    # code / literal は `_fold_scan` が引用ごとに判定する(囲っている argv[0] がシェルか)。
    if [ "${#cmd}" -le "$SBMAXLEN" ]; then
      find_src="$(fold_quoted "$cmd")"
    fi ;;
esac
# 上限を超えて畳めなかったときに **fail-open しない**。畳み込みが無効だと引用・エスケープされた
# メタ文字が下流の `[^|;&]*` スパンを切って全ての保護が外れる。末尾に長いコメントを足すだけで
# 到達でき、実削除を確認済み(T13 経路3)。一律 deny にはせず**保護対象に言及しているときだけ**止める。
if [ "${#cmd}" -gt "$SBMAXLEN" ]; then
  # **read / 記録系が argv[0] なら見送る**(長い grep や commit message の誤 deny は
  # 2026-07-31 の再来)。早期素通しと同じ動詞リスト。
  if ! printf '%s' "${cmd%%"$NL"*}" | grep -qE '^[[:space:]]*(grep|rg|ag|cat|head|tail|less|ls|wc|echo|printf)[[:space:]]|^[[:space:]]*git[[:space:]]+(log|show|diff|status|blame|add|commit)[[:space:]]'; then
    case "$cmd" in
      *[\"\'\\]*)
        # 語彙にプロセス / サービス名も入れる(パスだけ見ていたので `kill $(pgrep 'a|b' stockbot)`
        # と `brew services stop 'a|b' postgresql` が抜けていた)。
        # launchd の com.stockbot.* は語の stockbot が拾う(別の項は要らない)。
        printf '%s' "$cmd" | grep -qE '\.stockbot|backend/data|runtime/[^[:space:]]*emergency|\b(psql|pg_restore|dropdb|pg_ctl|launchctl|docker|pgrep|pkill|killall|brew|postgres|stockbot)\b|\b(g)?make\b' \
          && deny "${SBMAXLEN}字を超えるコマンドは引用の解析ができず、保護判定が信頼できない。保護対象(~/.stockbot / backend/data / emergency_stop / DB / launchd / プロセス / make)に触れるものは短く分割して実行する。" ;;
    esac
  fi
fi
FINDPRE='(^|[;&|(])[[:space:]]*((sudo|env|time|nohup|xargs|command|ionice)[[:space:]]+)*\\?find\b'
FINDX='-(exec|execdir|ok|okdir)[[:space:]]+(sudo[[:space:]]+)?(/[a-z/]*)?'
FINDRE="$FINDPRE"'[^|;&]*(-delete\b|'"$FINDX"'(rm|mv|truncate|unlink|shred)\b|'"$FINDX"'(sh|bash|zsh)\b[^|;&]*\b(rm|unlink|shred|truncate)\b)'
# **生と畳んだ版の両方**で見る。生だけでは引用の中の | が span を切り
# (`find backend/data -regex 'a|b' -delete`)、畳むだけでは `-exec sh -c 'rm "$1"'` の削除動詞が
# Q に消える(NOFOLD のアンカーが区切りの直後を要求するのでこの位置の sh -c を拾わない)。
# union なので fail-close 側にしか動かない。
if printf '%s' "$cmd" | grep -qE "$FINDRE" || printf '%s' "$find_src" | grep -qE "$FINDRE"; then
  deny "find -delete / -exec rm は再帰削除と等価。対象を明示した rm を人間が実行する。"
fi

# T5: enforcement ファイルの自己改変を塞ぐ。
# 🛑 **早期素通しより前**に置く。`printf` は read/record 動詞なので、後ろに置くと
# `printf "exit 0\n" > .claude/hooks/pretooluse-deny.sh` がそのまま通る(rm / chmod -x /
# `echo "{}" > settings.json` も同様)。判定は畳んだコピー($find_src)— 生の $cmd だと
# `git commit -m "docs: rm .claude/hooks/x.sh の穴"` のような言及が誤 deny される。
# 動詞は「その操作を実現する綴りを全部」挙げる(§地雷帳: 綴りではなく操作を見る)。
# 🛑 逃げ道は **(a) セッション env** か **(b) コマンド先頭の env 前置**だけ。着手直後の版は
# `grep -q` で**任意位置**を見ていたため `rm .claude/hooks/x # STOCKBOT_HARNESS_EDIT_APPROVED=1`
# と**コメントに書くだけ**で解除できた(exit 0 を実測)。区切りの直後に限定する。
ENF_OK='(^|[;&|(`])[[:space:]]*(env[[:space:]]+)?STOCKBOT_HARNESS_EDIT_APPROVED=1[[:space:]]'
ENFV='\b(rm|mv|chmod|chown|truncate|shred|unlink|ln|install|dd|patch|ex|tee|g?sed[[:space:]]+-[a-zA-Z]*i|perl[[:space:]]+-[a-zA-Z]*i)\b'
# 🛑 `cp` / `rsync` は**コピー先**が enforcement のときだけ止める。区別せず動詞リストに入れると
# `cp .claude/hooks/x /tmp/backup.sh`(退避・比較のための取り出し)まで deny する(しかも `cp -r`
# と `tar` は通るので一貫性も無かった)。「フラグを除いた第1引数の**後ろ**」= コピー先で切り分ける。
ENFCP='\b(cp|rsync)\b([[:space:]]+-[^[:space:]]+)*[[:space:]]+[^-[:space:];&|][^[:space:];&|]*[[:space:]][^|;&]*'
if [ "${STOCKBOT_HARNESS_EDIT_APPROVED:-0}" != "1" ] \
   && ! printf '%s' "$cmd" | grep -qE "$ENF_OK"; then
  enf_hit="$(printf '%s' "$find_src" | grep -oE '('"$ENFV"'[^|;&]*'"$ENF_CMD_RE"'|'"$ENFCP$ENF_CMD_RE"'|(\{[A-Za-z_][A-Za-z0-9_]*\})?[0-9]*>>?[&|]?[[:space:]]*[^[:space:];&|<>]*'"$ENF_CMD_RE_NL"'|\bgit\b[^|;&]*\b(checkout|restore|stash|clean)\b[^|;&]*'"$ENF_CMD_RE"'|\bgit\b[^|;&]*--no-verify\b)' || true)"
  # 🛑 `git config core.hooksPath` は**書き換えだけ**止める。read(`--get` / `--list`)や
  # HARNESS_SETUP.md が指示する `git config core.hooksPath .githooks`(有効化そのもの)まで
  # deny すると手順書どおりの操作が実行できない。
  # 値を渡さない `git config core.hooksPath` も `--get` と同じ読み取り(H6)。ただし値を取らない
  # 書込フラグ(--unset 等)が付いていれば書込。値の有無は生の $cmd で見る — 畳んだコピーでは
  # 空の値 `""` が消えて「値なし」に見える。
  if [ -z "$enf_hit" ] \
     && printf '%s' "$find_src" | grep -qE '\bgit\b[^|;&]*\bconfig\b[^|;&]*core\.hooksPath'; then
    hp_read=0
    printf '%s' "$find_src" \
      | grep -qE '\bcore\.hooksPath([[:space:]]+|=)\.githooks([^A-Za-z0-9_.-]|$)|--(get|list|get-all|get-regexp)\b' \
      && hp_read=1
    if [ "$hp_read" -eq 0 ] \
       && printf '%s' "$cmd" | grep -qE '\bcore\.hooksPath[[:space:]]*($|[;&|)])' \
       && ! printf '%s' "$find_src" | grep -qE '[[:space:]]--(unset|unset-all|replace-all|add|remove-section|rename-section)\b'; then
      hp_read=1
    fi
    [ "$hp_read" -eq 1 ] || enf_hit="hooksPath"
  fi
  # 🛑 インタプリタに渡すコード文字列は**畳まれる**(シェルではないので code 位置にならず Q に
  # 潰れる)。畳んだコピーでは見えないので、この 1 ルールだけ**生の $cmd** を見る。誤 deny を
  # 避けるため、インタプリタが**セグメントの argv[0] の位置**にあるときだけ(= 言及ではなく実行)。
  if [ -z "$enf_hit" ]; then
    printf '%s' "$cmd" \
      | grep -qE '(^|[;&|(`])[[:space:]]*([A-Za-z_][A-Za-z0-9_]*=[^[:space:]]*[[:space:]]+|env[[:space:]]+|sudo[[:space:]]+)*(python3?|perl|ruby|node)\b[^|&]*[[:space:]]-(c|e)\b[^|&]*'"$ENF_CMD_RE" \
      && enf_hit="interp"
  fi
  # 🛑 `cd .claude/hooks && rm x` は**パスがコマンドから消える**ので上のスパン照合では拾えない
  # (実測 exit 0)。cd の引数が enforcement で、かつコマンドのどこかに破壊動詞かリダイレクトが
  # あるときだけ deny する(`cd` 単体は通す)。
  if [ -z "$enf_hit" ] && printf '%s' "$find_src" | grep -qE '\bcd\b[^|;&]*'"$ENF_CMD_RE"; then
    printf '%s' "$find_src" | # 🛑 リダイレクト先が絶対パス(`> /tmp/out.txt`)なら、それはテスト出力の退避であって
    # enforcement の改変ではない(区別しないと `cd .claude/hooks && bash x_test.sh > /tmp/o` まで deny)。
    printf '%s' "$find_src" | grep -qE "$ENFV"'|(^|[^>])>[[:space:]]*[^/[:space:]]' && enf_hit="cd+verb"
  fi
  # 🛑 パイプの向こうで動詞が走る形(`echo <path> | xargs rm` / `find … -exec chmod`)。`[^|;&]*` は
  # `|` を越えられないので、**「どこかに enforcement パスがある」×「xargs / -exec が破壊動詞を
  # 持つ」**の 2 本の独立した grep で見る。
  if [ -z "$enf_hit" ] && printf '%s' "$find_src" | grep -qE "$ENF_CMD_RE"; then
    printf '%s' "$find_src" \
      | grep -qE '(\bxargs\b[^|;&]*'"$ENFV"'|-(exec|execdir|ok|okdir)[[:space:]]+[^|;&]*'"$ENFV"')' \
      && enf_hit="pipe+verb"
  fi
  [ -n "$enf_hit" ] \
    && deny "enforcement ファイル(hook / settings / githooks / CI / guard / Makefile)の Bash 経由の改変・削除は禁止。ルールを機械強制しているファイルなので、AI が自分で書き換えられない状態にしてある。人間の承認のうえ Edit で行い、コマンド先頭に STOCKBOT_HARNESS_EDIT_APPROVED=1 を置く(またはその env でセッションを起動する)。"
  # H3: scripts の自己テストは書き換え(sed -i / >> / Edit)を通し、削除と空化だけを止める。
  # 削除動詞は**セグメントの argv[0]**(`git rm` / `xargs rm` / `-exec rm` を含む)で見る —
  # 任意位置にすると `grep -n rm scripts/x_test.sh` まで deny する。空化は単独の `>`(`>>` は追記)と
  # /dev/null からのコピー。
  if [ -n "${ENF_TEST_CMD_RE:-}" ]; then
    TDELPRE='((^|[;&|(`{!"'"$SQ"'])[[:space:]]*((sudo|env|command|nohup|time|xargs)([[:space:]]+-[^[:space:]]+)*[[:space:]]+)*(git[[:space:]]+)?|-(exec|execdir|ok|okdir)[[:space:]]+)([^[:space:]]*/)?'
    tdel_hit="$(printf '%s' "$find_src" | grep -oE '('"$TDELPRE"'(rm|mv|truncate|shred|unlink|ln|dd)\b[^|;&]*'"$ENF_TEST_CMD_RE"'|(^|[^>])(\{[A-Za-z_][A-Za-z0-9_]*\})?[0-9]*>[&|]?[[:space:]]*[^[:space:];&|<>]*'"$ENF_TEST_CMD_RE"'|\b(cp|rsync|install)\b[^|;&]*/dev/null[^|;&]*'"$ENF_TEST_CMD_RE"')' || true)"
    if [ -z "$tdel_hit" ] && printf '%s' "$find_src" | grep -qE "$ENF_TEST_CMD_RE"; then
      tdel_hit="$(printf '%s' "$find_src" | grep -oE '\bxargs\b[^|;&]*\b(rm|mv|truncate|shred|unlink)\b' || true)"
    fi
    [ -n "$tdel_hit" ] \
      && deny "scripts の自己テスト(scripts/*_test.sh)の削除・空化は禁止(self-test を消せば検査が消える)。書き換えと新規作成はできる — Edit / Write か sed -i / >> を使う。"
  fi
fi

if [ "$is_single_line" -eq 1 ] \
   && ! has_unquoted "$cmd" '|;&<>' \
   && ! printf '%s' "$cmd" | grep -qE '\$\(|`'; then
  if printf '%s' "$first_line" | grep -qE '^[[:space:]]*(grep|rg|ag|cat|head|tail|less|ls|find|wc|echo|printf)[[:space:]]|^[[:space:]]*git[[:space:]]+(log|show|diff|status|blame|add|commit)[[:space:]]'; then
    exit 0
  fi
fi

# **引用の中の言及では発火させない。**早期素通しは && / | / >> があると失格するので、そこまで
# 来た `git add -A && git commit -m "fix: rm backend/data の穴"` を動詞ルールが掴んでいた
# (2026-07-31 の誤 deny の再来。docs はこの文字列だらけなので T4 自身のコミットが刺さる)。
# ただし**引用がコードを渡している**ときは畳んではいけない — `bash -c "rm -rf ~/.stockbot/data"`
# の実行コードごと消え、`sh -c` / `eval` / `xargs -I{} sh -c` の 5 系統が通っていた(2026-08-08)。
# シェル / eval は**コマンドの位置**(区切りの直後)にあるときだけ見る。行のどこにあっても見ると
# `git commit -m "... eval \"rm -rf ~/.stockbot/data\" ..."` のような**言及**で NOFOLD が真になり、
# 生判定に落ちて誤 deny する。前置クラスに / (パス付き起動)と env 代入と複合コマンドの導入語、
# アンカーに ( ) { } を入れるのは、いずれも列挙漏れで実際に抜けられた実測から。
sb_cmd="$find_src"

# integration tests は trades を truncate する -> make test-integration 経由のみ。
# `go[[:space:]]+test` を前提にしていた頃は同じことをする 3 経路が素通りしていた(2026-08-08 実測、
# いずれも exit 0): ラッパ(`gotestsum -- -tags=integration`)/ バージョン付き go(`go1.24.0 test …`)/
# ビルド済みバイナリ(`./pkg.test -test.run TestIntegration`)。**タグそのもの**を見る形に反転し、
# 実行を伴わない経路 —`go vet -tags integration`(TestMain を走らせない)と `make (test|vet)-integration`
# (Makefile 側が _test 末尾 DSN を強制)— だけを名指しで許す。
# 判定は sb_cmd。生の $cmd だと `git commit -m "... -tags integration ..."` が誤 deny される。
# タグ名は**完全一致**で見る(`[^[:space:]]*integration` だと `-tags integrationless` という別のタグ
# まで deny した)。誤検知するガードは必ず無視されるようになるので狭める側に倒す。区切りは
# ビルドタグに使えない文字(= [A-Za-z0-9_.] 以外)。`-tags=unit,integration` も同じ規則で拾える。
TAGV='([A-Za-z0-9_.]+[,[:space:]]+)*integration([^A-Za-z0-9_.]|$)'
# 前置に = を許すのは GOFLAGS=-tags=integration(値の中に -tags が入る)。区切りクラスに
# 引用符を入れるのは、NOFOLD で畳まれなかった `bash -c "… -tags='integration'"` が残るため。
SQ="'"
TAGRE='((^|[[:space:]]|=|"|'"$SQ"')--?tags[=[:space:]"'"$SQ"']+'"$TAGV"'|GOFLAGS=[^[:space:]]*'"$TAGV"')'
tag_cmd="$sb_cmd"

# 許すのは**実行を伴わない経路**であって `go vet` という綴りではない。vet だけを見ていたため
# `go list -tags integration`(何も動かさない)が deny で `go test … # go vet`(全部動く)が allow
# という逆転が起きていた。go run / go generate はコードを走らせるので入れない。make の carve-out は
# 削除した — `make test-integration` は -tags を含まず外側の条件が偽になるので、唯一の観測可能な
# 効果が `go test … # make test-integration` を通すことだった。
# 許可判定は**セグメントの argv[0] の位置**でだけ行う。セグメントのどこでも探していた頃は
# `go test -tags=integration $(go list ./...)` が exit 0 で、隔離モジュールでタグ付きテストが実際に
# 走った(`-exec staticcheck` / `-o /tmp/revive` / `> golangci-lint.log` でも同じ原因で解除できた)。
# 前置語の列挙は TBINW / NOFOLDW と揃える。狭いと**許可経路の argv[0] が前置語に隠れて誤 deny**
# する(`timeout 60 go vet …` / `for … do go vet …` / `{ go vet …; }` が全て deny だった)。
TAGPRE='^[[:space:]]*[{(!]?[[:space:]]*(([A-Za-z_][A-Za-z0-9_]*=[^[:space:]]*|sudo|env|time|nohup|command|xargs|timeout|stdbuf|nice|ionice|setsid|exec|then|do|else|elif)[[:space:]]+([0-9][^[:space:]]*[[:space:]]+)?)*'
TAGOK="$TAGPRE"'(go[[:space:]]+(vet|build|list|doc|fmt|env|version)\b|(staticcheck|golangci-lint|revive)\b)'
# 判定は**セグメント単位**。コマンド全体で許可経路を探すと、無関係な vet を前置するだけでルール
# 全体が解除できた(`go vet ./... && go test -tags integration ./...` が exit 0)。区切りは `; & |`
# では足りない: `#`(bash はコメントを捨てて前半を実行する)/ `$( )` と backtick(中は別のコマンド
# なので区切らないと許可条件を供給できる)/ `< >`(リダイレクト先はコマンドではなくファイル名)。
# 末尾に grep -q を置かない。`set -o pipefail` の下では grep -q が最初のマッチで抜けた瞬間に上流が
# SIGPIPE(141)で死に、それが全体の status になって**非決定的に fail-open** する(実測: 6105 字で
# 30 回中 14 回 ALLOW、9900 字で 25/25 ALLOW)。変数に受ければ grep は入力を読み切る。
# **リダイレクトは畳む前に落とす。**`>` を区切りにしただけだと**先頭**に置かれたリダイレクト先の
# ファイル名が argv[0] の位置に来て許可条件を満たした(`> golangci-lint.log go test -tags=integration
# ./...` が exit 0 で、隔離モジュールでタグ付きテストが実際に走った)。前後どちらでも除去する。
tag_flat="$tag_cmd"
case "$tag_cmd" in
  # 末尾クラスから **( ) backtick を除外する**。しないと `<(` `$(` の開き括弧まで食って TAGSEP の
  # 境界を壊し、置換の中のコマンドが許可経路のセグメントに融合する(`go vet -tags=unit
  # <(go test -tags=integration ./...)` が exit 0。マーカーファイルで実行を確認)。
  # 演算子側は**綴りを網羅する**。1 文字足りないだけで先頭リダイレクト先が argv[0] に残り、同じ
  # 機構で 3 巡続けて抜けられた: `>file` -> `>&file` -> `>|file`。`{name}>` の fd 変数形も前置と
  # して食う(変数名が許可リストのツール名だと TAGPRE の `[{(!]?` を抜ける)。
  *[\<\>]*) tag_flat="$(printf '%s' "$tag_cmd" | sed -E 's/(\{[A-Za-z_][A-Za-z0-9_]*\})?[0-9]*(>>?|<)[&|]?[[:space:]]*[^[:space:];&|<>()`]*/ /g')" ;;
esac
TAGSEP=';&|#()<>`'
tag_segs="$(printf '%s' "$tag_flat" | tr "$TAGSEP" '\n\n\n\n\n\n\n\n\n' | grep -aiE "$TAGRE" || true)"
if [ -n "$tag_segs" ]; then
  tag_bad="$(printf '%s\n' "$tag_segs" | grep -avE "$TAGOK" || true)"
  if [ -n "$tag_bad" ]; then
    deny "integration タグ付きテストの直接実行は実 DB を truncate する。make test-integration 経由のみ(plan §8.5)。"
  fi
fi
# ビルド済みテストバイナリ。作る側(`go test -c` / `-o`)は上のタグルールが塞ぐ。
# **綴りに Integration を要求しない** — タグ付きでビルドされたバイナリはどのテストを走らせても
# TRUNCATE に届く(`./repository.test` 素の実行が exit 0 だった)。argv[0] の位置でだけ見るので
# `cat foo.test` / `ls *.test` は通る。終端は「識別子文字でない何か」(閉じ引用符対策)。
# 前置は起動ラッパを網羅する(sudo|env|time|nohup だけだった頃は exec / command / stdbuf / nice /
# sh -c 経由の 6 形が素通り。`env` はあるのに `env A=1 ./x.test` が通る内部矛盾が列挙漏れの根拠)。
# アンカーに引用符を足すのは `sh -c "./x.test"` のように**引用の直後が argv[0]** になる形のため。
# ラッパの引数はフラグ / 数字始まり / **値を取る特定のフラグの値**だけ許す。任意の語を許すと
# `time cat foo.test` のような純粋な read まで巻き込み、任意のフラグに値を許すと値の位置で read を
# 食って誤 deny する(`time -p cat …golden.test` / `strace -f cat …golden.test`。-p と -f は値を取らない)。
TBINA='((-[^[:space:]]+|[0-9][^[:space:]]*)[[:space:]]+|-(u|U|n|s|o|e|i|g|k|C|-user|-signal|-niceness|-chdir)[[:space:]]+[^-[:space:]][^[:space:]]*[[:space:]]+)*'
TBINW='([A-Za-z_][A-Za-z0-9_]*=[^[:space:]]*|sudo|env|time|nohup|exec|command|stdbuf|nice|ionice|setsid|timeout|eval|xargs|strace|ltrace|watch)[[:space:]]+'"$TBINA"
# セグメントの argv[0](起動ラッパの後ろ・引用の直後を含む)。T14 / H6 の DB・docker ルールも使う。
ARGV0PRE='(^|[;&|(`{!"'"$SQ"'])[[:space:]]*('"$TBINW"')*'
printf '%s' "$sb_cmd" | grep -qE "$ARGV0PRE"'[^[:space:];&|]*\.test([^A-Za-z0-9_.-]|$)' \
  && deny "ビルド済みテストバイナリの直接実行は、integration タグ付きなら実 DB を truncate する。make test-integration 経由(plan §8.5)。"
# 名前が *.test でなくても(`go test -o /tmp/it`)、-test.run は go test 由来のバイナリにしか無い。
printf '%s' "$sb_cmd" | grep -qE '\-test\.run[=[:space:]][^|;&]*[Ii]ntegration' \
  && deny "ビルド済み integration テストバイナリの直接実行も実 DB を truncate する。make test-integration 経由(plan §8.5)。"

# T9: 作業ツリーを守る。git add / commit は早期素通しリスト入りだが reset --hard / clean -fd /
# checkout -- . は判定対象ですらなかった。対象を明示しない全体操作だけを deny する
# (`git reset <file>` / `git checkout <branch>` / `git clean -n` は許可のまま)。
# 判定は sb_cmd(生だと `git commit -m "… git reset --hard …"` が誤 deny)。前置は `git -C <path>`
# 等を許す([^|;&]* でセグメント内に限定)。終端は「識別子文字でない何か」— ([[:space:]]|$) だと
# `bash -c "git reset --hard"` の閉じ引用符を取りこぼす(T4/T7 で 2 回踏んだ穴)。
# `git restore .` は `git checkout -- .` と同一の破壊。--staged / -S だけなら index を戻すだけなので対象外。
# **綴りではなく操作で見る。**当初は `checkout -- .` と `clean -<短フラグ>` しか見ておらず、使い捨て
# リポジトリでの実測で 16 形が素通りしていた: `git checkout .`(-- 無し。こちらが一般的な綴り)/
# `clean --force` / `clean -x -f`(先頭以外のフラグ)/ `switch --discard-changes` / `git rm -rf .` / stash の破棄。
GITW='([^|;&]*[[:space:]])?'                       # 途中のオプション・リビジョン指定
GITT='(\.|\./|:/|\*)([^A-Za-z0-9_./-]|$)'          # 作業ツリー全体を指す target
GITF='([^A-Za-z0-9_-]|$)'
# clean: **短フラグは f/d を含むものすべて**(先頭とは限らない)。長オプションは --force だけを
# 名指しする — `-[a-zA-Z]*[fd]` を長オプションにも当てると `--dry-run` の d を拾って誤 deny する。
GITD='\bgit\b[^|;&]*[[:space:]]('\
'reset[[:space:]]+(--hard|--merge)'"$GITF"\
'|checkout[[:space:]]'"$GITW"'(--[[:space:]]+)?'"$GITT"\
'|checkout[^|;&]*[[:space:]](-f|--force)'"$GITF"\
'|switch[^|;&]*[[:space:]](--discard-changes|-f|--force)'"$GITF"\
'|rm[^|;&]*[[:space:]](-[a-zA-Z]*[rf][a-zA-Z]*|--force)[^|;&]*[[:space:]]'"$GITT"\
'|stash[[:space:]]+(drop|clear)'"$GITF"\
'|reflog[[:space:]]+expire'"$GITF"\
')'
printf '%s' "$sb_cmd" | grep -qE "$GITD" \
  && deny "作業ツリー全体を捨てる git 操作(reset --hard / checkout . / switch --discard-changes / rm -rf . / stash drop)は未コミット作業と untracked を消す。人間が判断する。"
# clean は 2 段。**-n / --dry-run があれば何も消さない**ので、フラグに f/d があっても許可する
# (`git clean -n -d` は一覧表示だけ。1 本の正規表現でやると必ずどちらかを誤る)。
GITCL='\bgit\b[^|;&]*[[:space:]]clean\b[^|;&]*[[:space:]]'
if printf '%s' "$sb_cmd" | grep -qE "$GITCL"'(-[a-zA-Z]*[fd][a-zA-Z]*|--force)'"$GITF"; then
  if ! printf '%s' "$sb_cmd" | grep -qE "$GITCL"'(-[a-zA-Z]*n[a-zA-Z]*|--dry-run)'"$GITF"; then
    deny "git clean -f / -d は untracked を消す。何が消えるか見るなら git clean -n。人間が判断する。"
  fi
fi
# 中間のフラグ列は**省略可能**にする。必須にすると `git restore .`(空白 1 個)が素通りし、
# 逆に `git restore --staged .` の --staged 検出も外れて誤 deny する(両方向に壊れる)。
GITRE='\bgit\b[^|;&]*[[:space:]]restore[[:space:]]+([^|;&]*[[:space:]]+)?'
if printf '%s' "$sb_cmd" | grep -qE "$GITRE$GITT"; then
  if ! printf '%s' "$sb_cmd" | grep -qE "$GITRE"'(--staged|-S)([^A-Za-z0-9_-]|$)' \
     || printf '%s' "$sb_cmd" | grep -qE '[[:space:]](--worktree|-W)([^A-Za-z0-9_-]|$)'; then
    deny "git restore . は git checkout -- . と同じく作業ツリーを捨てる。人間が判断する(--staged のみなら可)。"
  fi
fi

# H6: docker が**セグメントの argv[0]** のときだけ見る。生の $cmd に `.*` を当てていた頃は
# パイプを越えて `docker compose logs db | grep -i stop` を deny していた。スパンは sb_cmd の
# `[^|;&]*`(引用の中のメタ文字は不透明化済み)。
printf '%s' "$sb_cmd" | grep -qiE "$ARGV0PRE"'([^[:space:]]*/)?docker(-|[[:space:]]+)compose\b[^|;&]*[[:space:]](down|stop|rm|kill)\b' \
  && deny "docker compose down/stop/rm/kill は postgres/DB を止める・消す(plan §8.3)。"
printf '%s' "$sb_cmd" | grep -qiE "$ARGV0PRE"'([^[:space:]]*/)?docker[[:space:]]+((container[[:space:]]+)?(stop|kill|rm)|volume[[:space:]]+(rm|prune)|system[[:space:]]+prune)\b' \
  && deny "docker (container) stop/kill/rm・volume rm/prune・system prune は稼働中コンテナや forward 台帳の volume を消す(plan §8.3)。必要なら人間が手動実行。"
printf '%s' "$cmd" | grep -qiE 'rm[[:space:]]+-[a-z]*r[a-z]*f?[a-z]*[[:space:]].*docker-data' \
  && deny "rm -rf docker-data は postgres のデータを消す(plan §8.3)。"

# ~/.stockbot 配下の運用データ。日足 CSV は git 管理外 = pg_dump の対象外で**唯一のコピー**
# (CLAUDE.md)。当初案の {data,backups,retired,env,bin} 列挙は、2026-08-08 のレビューが**親を
# 消せば同じ**(`rm -rf ~/.stockbot` が exit 0)ことを実測したので**根ごと**保護に変えた(直下に
# 立花の秘密鍵 / env / hard_limits.yaml / stockbot-routine.sh もある)。~user 形・${HOME} 形も
# 素通りしていたので綴りを増やす。
SBROOT='(\$HOME|\$\{HOME\}|~[A-Za-z0-9_.-]*|/Users/[^/[:space:]]+)/\.stockbot'
# 終端は「パス文字でない何か」。(/|$|[[:space:]]) だと**引用符で終わる形**を取りこぼし、
# `rm -rf "$HOME/.stockbot/data"` は deny なのに `rm -rf "$HOME/.stockbot"`(根こそぎの方が
# 破壊的)が通るという逆転が起きていた(2026-08-08 実測)。
SBEND='([^A-Za-z0-9_.-]|$)'
# backend/data は ~/.stockbot/data への symlink(CLAUDE.md)。docs も Makefile もこの表記なので、
# エージェントが実際に打つのはこの形。data_research(検定用)も同格。
# SBPBODY はパス本体、SBP は「直前がパス文字でない」ことも要求する版(`rm -rf mybackend/data` を
# 拾わないため)。cd のように区切りを既に食っている文脈では SBPBODY 側を使う。
SBPBODY='('"$SBROOT"'|backend/data(_research)?)'"$SBEND"
SBP='(^|[^A-Za-z0-9_.-])'"$SBPBODY"
# 消えたら復元できないものだけ。**logs/ は入れない** — plist の StandardOutPath を手で
# 再現するときに `> ~/.stockbot/logs/weekly.log` を打つ(レビュー実測の誤 deny)。
SBDATA='('"$SBROOT"'/(data|backups|universe|env|hard_limits[^[:space:]]*|e_api_private_key[^[:space:]]*)|(^|[^A-Za-z0-9_.-])backend/data(_research)?)'"$SBEND"
# 削除動詞。**chmod / chown は入れない** — `chmod +x ~/.stockbot/db-backup.sh` は Makefile の
# backup-install / routine-install のレシピそのもの。cp / tee / >> も日常の設置操作なので入れない。
SBV='(rm|mv|truncate|dd|shred|unlink)'
SBSEG='(^|[;&|])[[:space:]]*(sudo[[:space:]]+)?'
# [[ =~ ]] 用は**改行を明示**する。grep の ^ は行頭を拾うが bash の ^ は文字列先頭のみで、cd ルールを
# grep から =~ に替えた際に改行区切りが漏れた(`ls -la<改行>cd ~/.stockbot/data<改行>rm -f x` が exit 0)。
# **grep のパターンに生の改行は入れられない**(壊れた正規表現が 2 本になり常に非マッチ)。だから 2 綴り。
SBSEGX='(^|['"$NL"';&|])[[:space:]]*(sudo[[:space:]]+)?'
if printf '%s' "$cmd" | grep -qE '\.stockbot|backend|\b(g)?make\b'; then
  # T7: hook は Bash 文字列しか見ないので Makefile の中身は不可視。停止・台帳消去・DB rollback を
  # 伴うターゲットだけを名指しで deny する。人間の端末からの make は hook を通らない。
  # 承認 env(STOCKBOT_HUMAN_APPROVED_DB_WRITE=1)でも解除しない — あれは live DB への raw write 用の
  # narrow escape であって、bot 停止や台帳消去の承認ではない。
  # 終端は「識別子文字でない何か」。([[:space:]]|$) だと**閉じ引用符**を許さず `bash -c "make stop"` が
  # 素通りする(SBEND と同じ欠陥)。- を除外クラスに残すので stop-check / restore-drill は保たれる。
  # gmake は Homebrew の GNU make。\bmake\b では当たらない。
  # live DB の migration(migrate-live-up / -down)も同じ扱い。cmd/migrate の承認 env は AI が
  # 自分で前置できるので壁にならない(S6)。migrate-live-status は read-only なので通す。
  # make start も人間だけ(2026-10-02 オーナー決定)。二重起動のガードが無く(DEBTS #20)、2 つ目の
  # プロセスも立花にログインして 1 つ目のセッションを失効させる。
  printf '%s' "$sb_cmd" | grep -qE '\b(g)?make\b([[:space:]]+-[^[:space:]]+)*[[:space:]]+([^[:space:]]+[[:space:]]+)*(start|stop|reset-trades|migrate-down|migrate-live-(up|down)|restore-drill)([^A-Za-z0-9_-]|$)' \
    && deny "make start / stop / reset-trades / migrate-down / migrate-live-up / migrate-live-down / restore-drill は bot の起動・停止・forward 台帳の消去・DB の rollback か live DB への書込を伴う。bot の起動と停止・消す・戻す側と live DB の migration は人間だけ(CLAUDE.md §1)。"

  if printf '%s' "$sb_cmd" | grep -qE '\.stockbot|backend/data'; then
    printf '%s' "$sb_cmd" | grep -qE '\b'"$SBV"'\b[^|;&]*'"$SBP" \
      && deny "~/.stockbot(= backend/data の実体)は日足 CSV の唯一のコピー(git 外・pg_dump 対象外)。削除・移動は人間が判断する。"
    # `[^|;&]*` はパイプで止まるので、動詞ルールだけではパイプ越しの削除を捕まえられない
    # (上の if が既に「保護対象に言及している」ことを保証する)。
    printf '%s' "$sb_cmd" | grep -qE '\bxargs\b[^|;&]*\b(rm|mv|truncate|shred|unlink)\b' \
      && deny "~/.stockbot 配下を xargs 経由で削除しない。対象を明示した操作を人間が実行する。"
    printf '%s' "$sb_cmd" | grep -qE '\brsync\b[^|;&]*--delete\b' \
      && deny "rsync --delete は同期先の余剰ファイルを消す。~/.stockbot 相手は人間が判断する。"
    # 切り詰めは唯一のコピーを空にする。>> (追記)と logs/ は対象外。
    printf '%s' "$sb_cmd" | grep -qE '(^|[^>])>[[:space:]]*'"$SBDATA" \
      && deny "> による切り詰めは日足 CSV の唯一のコピーを空にする。人間が判断する。"
    printf '%s' "$sb_cmd" | grep -qE '(sed[[:space:]]+-[a-zA-Z]*i|\btee\b|\bcp\b[^|;&]*/dev/null)[^|;&]*'"$SBDATA" \
      && deny "sed -i / tee / cp /dev/null は唯一のコピーを in-place で潰す。人間が判断する。"
  fi

  # cd してからの相対パス削除。動詞は**区切りの直後**(= argv[0])のときだけ見る
  # (`cd ~/.stockbot/data && grep -c rm x.csv` のような read を巻き込まないため)。cd 先に backend を
  # 含めるのは `cd backend && rm -rf data`(このリポの標準作業形)が symlink 越しに同じ実体を消すため。
  # (a) 保護パスそのものに cd したなら、以降の削除動詞は対象を問わず保護対象を指す。cd 先から
  # logs / tmp / venv は外す(logs は「唯一のコピー」ではなく、tmp は routine-install の作業
  # ディレクトリ)。そのため終端に / を許さない SBCDEND を使う。
  SBCDEND='([^A-Za-z0-9_./-]|$)'
  # 末尾の /? は `cd ~/.stockbot/ && rm -rf data`(trailing slash)のため。
  SBCD='('"$SBROOT"'(/(data|backups|universe|retired)[^[:space:]]*)?/?|backend/data(_research)?)'"$SBCDEND"
  # **cd より後ろだけ**を見る。cd と削除動詞を独立に grep すると cd の**前**にある無関係な削除
  # (`rm -rf /tmp/build && cd ~/.stockbot`)まで巻き込む。位置の切り出しは改行をまたげる =~ で行う。
  if [[ "$sb_cmd" =~ $SBSEGX(cd|pushd)[[:space:]]+([^\;\&\|]*[^A-Za-z0-9_.-])?$SBCD ]]; then
    # 削除対象が / で始まる絶対パスなら cwd に依存しないので巻き込まない。除外文字に - も入れる —
    # フラグ列は 0 回マッチを選べるので `rm -f /tmp/x` の `-f` を第1引数と読んで誤 deny する。
    printf '%s' "${sb_cmd#*"${BASH_REMATCH[0]}"}" \
      | grep -qE "$SBSEG$SBV"'\b([[:space:]]+-[^[:space:]]+)*[[:space:]]+[^-/[:space:]]' \
      && deny "~/.stockbot / backend/data に cd してからの削除も同じ結果になる。人間が判断する。"
  fi
  # (b) backend/ に cd しただけなら repo 内の通常作業。相対パスが data/ を指すときだけ見る。
  if printf '%s' "$sb_cmd" | grep -qE "$SBSEG"'(cd|pushd)[[:space:]]+[^;&|]*backend'; then
    printf '%s' "$sb_cmd" | grep -qE "$SBSEG$SBV"'\b[^|;&]*[[:space:]](\./)?data(_research)?/?([[:space:]]|$)' \
      && deny "cd 後の相対パス data/ は backend/data(= ~/.stockbot/data の symlink)を指す。人間が判断する。"
  fi
fi
# 以下は `[^|;&]*` スパンを使うので **sb_cmd**(メタ文字を不透明化したコピー)で見る。生の $cmd だと
# 引用・エスケープされたメタ文字が区切りと誤読される(T13 経路2: `rm 'a|b' runtime/emergency_stop` が
# exit 0 で実削除を確認済み)。
printf '%s' "$sb_cmd" | grep -qE '\b(rm|mv|truncate)\b[^|;&]*runtime/[^[:space:]]*emergency' \
  && deny "emergency_stop フラグは write-once。解除は POST /api/emergency-resume(人間)経由(CLAUDE.md)。"
# bot の起動は人間だけ(2026-10-02 オーナー決定・CLAUDE.md §1)。make を経ない起動 2 形も塞ぐ。
# T7 の if の外に置くのは、`go run ./cmd/stockbot` が .stockbot / backend / make のどれにも当たらないため。
# go run はパッケージの後ろの区切りで見る(cmd/stockbot-foo は別物)。go test / go build は通す。
printf '%s' "$sb_cmd" | grep -qE '\bgo([0-9.]+)?[[:space:]]+run\b[^|;&]*cmd/stockbot([^A-Za-z0-9_-]|$)' \
  && deny "go run ./cmd/stockbot は bot を起動する。2 つ目のプロセスも立花にログインして 1 つ目のセッションを失効させる。bot の起動は人間だけ(CLAUDE.md §1)。"
# ビルド済みバイナリは argv[0] の位置でだけ見る(`ls bin/stockbot` / `go build -o bin/stockbot` は通す)。
printf '%s' "$sb_cmd" | grep -qE "$ARGV0PRE"'([^[:space:]]*/)?bin/stockbot([^A-Za-z0-9_.-]|$)' \
  && deny "bin/stockbot の実行は bot を起動する。bot の起動は人間だけ(CLAUDE.md §1)。"
# DROP / TRUNCATE / dropdb / pg_restore --clean は承認 env でも上書きできない(psql 分岐より先に置く)。
# H6: DROP / TRUNCATE の SQL は、DB に届くコマンド(psql 系・docker・インタプリタ)が**どこかの
# セグメントの argv[0]** にあるときだけ見る。SQL 本文は引用の中なので生の $cmd で見るが、
# 実行者の有無は sb_cmd で見る — commit message や grep の言及では argv[0] に何も立たない。
# パイプの向こう(`echo "DROP …" | psql`)と `bash -c "psql …"` は argv[0] に psql が立つので拾う。
DBEXEC="$ARGV0PRE"'([^[:space:]]*/)?(psql|pg_restore|pgcli|usql|docker|docker-compose|podman|python[0-9.]*|node|ruby|perl|php|deno|bun)\b'
db_exec=0
printf '%s' "$sb_cmd" | grep -qiE "$DBEXEC" && db_exec=1
[ "$db_exec" -eq 1 ] && printf '%s' "$cmd" | grep -qiE 'DROP[[:space:]]+(SCHEMA|DATABASE|TABLE)\b' \
  && deny "DROP SCHEMA/DATABASE/TABLE は DB を破壊する(plan §8.3)。migration の down は make migrate-down 経由。"
printf '%s' "$cmd" | grep -qiE '\bdropdb\b' \
  && deny "dropdb は DB を破壊する(plan §8.3)。"
printf '%s' "$cmd" | grep -qiE '\bpg_restore\b[^|;&]*(--clean|[[:space:]]-c\b)' \
  && deny "pg_restore --clean は既存オブジェクトを DROP してから restore する(plan §8.3)。restore-drill は make restore-drill 経由。"
[ "$db_exec" -eq 1 ] && printf '%s' "$cmd" | grep -qiE '\bTRUNCATE[[:space:]]+(TABLE[[:space:]]+)?[a-z]' \
  && deny "TRUNCATE は取引履歴(監査証跡)を消す(plan §8.3)。"

printf '%s' "$sb_cmd" | grep -qiE '(pg_ctl[^|;&]*\b(stop|restart)\b|(pkill|killall)[[:space:]].*postgres|brew[[:space:]]+services[[:space:]]+(stop|restart)[[:space:]]+postgres)' \
  && deny "postgres を止めると bot の DB 接続が切れる(plan §8.3)。"

printf '%s' "$sb_cmd" | grep -qiE '((pkill|killall)[[:space:]].*stockbot|\bkill\b[^|;&]*pgrep[^|;&]*(stockbot|postgres)|pgrep[^;&]*(stockbot|postgres)[^;&]*\|[^;&]*\bkill\b)' \
  && deny "live bot(stockbot)/ postgres プロセスを止めない。再起動が必要なら人間に依頼して待つ(CLAUDE.md)。"

# 状態を変える launchctl サブコマンドだけを deny する(list / print / blame 等の照会は許可)。
# attach / debug / config / limit は launchctl help 自身が「configures」「modifies」と書く状態変更系。
# examine / bsexec / asuser も実行を伴う(2026-08-07 レビューで洗い出し)。
LCW='(unload|load|bootout|bootstrap|remove|disable|enable|stop|start|kill|kickstart|submit|setenv|unsetenv|attach|debug|config|limit|examine|bsexec|asuser)'
printf '%s' "$sb_cmd" | grep -qiE 'launchctl[[:space:]]+'"$LCW"'\b[^|;&]*com\.stockbot' \
  && deny "launchd ジョブ(日足更新 / backup)の操作は人間が判断する。launchctl list / print は可。"
# plist を消す・書き換えるのは launchctl unload と同じ結果になる(launchctl だけ塞いでも、それが
# 読むファイルを消せば同じ)。plutil / defaults は書き換え系サブコマンドだけを見る(-p / -lint /
# read は照会なので launchctl print と同じ理由で通す)。ファイル操作側は .plist で終わることを
# 要求する(defaults はドメイン名で拡張子が付かないので別枝)。
printf '%s' "$sb_cmd" | grep -qiE '((\b(rm|mv|cp|ln|dd|truncate|chmod|chown)\b|sed[[:space:]]+-[a-zA-Z]*i|\btee\b|>>?)[^|;&]*com\.stockbot[^|;&]*\.plist|plutil[[:space:]]+-(replace|insert|remove|convert)\b[^|;&]*com\.stockbot|defaults[[:space:]]+(write|delete|import)\b[^|;&]*com\.stockbot)' \
  && deny "launchd の plist(com.stockbot.*)の削除・書換は launchctl unload と同じ。人間が判断する。"

# raw psql / pg_restore 書込(SELECT 読み取りは許可)。書込動詞 / -f(SQL ファイル実行)/ pg_restore は
# _test DSN か人間承認(STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 前置)で許す narrow escape。
# DROP/TRUNCATE/dropdb/--clean は上の standalone deny が先に拾うので承認でも通らない。
# H6: 入口は psql / pg_restore が**セグメントの argv[0]** にあるとき(docker exec 越しを含む)。
# どこかに綴りがあれば入っていた頃は `grep -n 'psql\|UPDATE' x | head` を deny していた。
if printf '%s' "$sb_cmd" | grep -qE "$ARGV0PRE"'([^[:space:]]*/)?((psql|pg_restore)\b|(docker|docker-compose|podman)\b[^|;&]*\b(psql|pg_restore)\b)'; then
  # escape は「**接続先の DB 名**が _test で終わる」ときだけ効く。以前の
  # `\b(psql|pg_restore)\b[^|;&]*_test\b` は psql 以降のどこかに _test があれば全書込を通していた
  # (2026-08-07 実測。SQL 本文の値・コメント・ファイル名でも発火し、以下が全て exit 0 だった):
  #   psql stockbot -c "UPDATE positions SET note='x_test' WHERE id=1"
  #   psql -d stockbot -c "DELETE FROM trades -- _test"  /  psql stockbot -f a_test.sql
  # **正規表現では argv を解析できない。**「_test を含む形」を 1 つずつ塞ぐ blacklist は収束せず、
  # レビュー 3 巡で回避形が 2 → 3 → 8 と増え続けた(位置引数 / URL / conninfo / 名前にハイフンや
  # ドット / 非隣接 / タブ区切り)。**whitelist に反転する**: 解決した接続先が 1 つ以上あり、その
  # **全て**が _test で終わるときだけ escape を許す。どれが最終的に勝つか(psql は位置引数が -d を
  # 上書きする)を知らなくても、全部が _test なら安全という性質を使う。想定外の形は fail-close。
  # 空白で分割する前に引用領域を 1 語に畳む。畳まないと -c "DELETE FROM trades" の FROM / trades が
  # 位置引数 = 接続先候補に見え、逆に丸ごと落とすと `psql "dbname=stockbot_test"` の正当な DSN が消える。
  resolve_targets() {
    local s="$1" tok want='' cmdname='' pos=0
    for tok in $s; do
      case "$want" in
        dbname) printf '%s\n' "$tok"; want=''; continue ;;
        skip)   want=''; continue ;;
      esac
      case "$tok" in
        psql|*/psql)             cmdname=psql;       continue ;;
        pg_restore|*/pg_restore) cmdname=pg_restore; continue ;;
      esac
      # psql / pg_restore より前(docker exec / env 前置など)は接続先ではない。
      if [ -z "$cmdname" ]; then continue; fi
      case "$tok" in
        -d|--dbname)   want=dbname ;;
        --dbname=*)    printf '%s\n' "${tok#--dbname=}" ;;
        -d?*)          printf '%s\n' "${tok#-d}" ;;
        dbname=*)      printf '%s\n' "${tok#dbname=}" ;;
        postgres://*|postgresql://*) printf '%s\n' "$tok" ;;
        # 値を取るオプションは**コマンドごとに違う**。psql の -t(tuples-only)/ -S(single-line)は
        # 値を取らないのに pg_restore の -t table / -S superuser と同じ一覧で読み飛ばすと、直後の
        # **位置引数 = live の dbname を食ってしまう**(2026-08-07 実測)。
        -h|--host|-p|--port|-U|--username|-f|--file|-c|--command|-F|-L|-o|--output|-P|-T)
                       want=skip ;;
        # psql 側だけが値を取る。pg_restore の -v は --verbose で値を取らないので、
        # 無条件に読み飛ばすと直後の -d(live)を食って接続先を見落とす(-t/-S の裏返し)。
        -R|--record-separator|-v|--set|--variable|--field-separator|--pset|--log-file)
                       if [ "$cmdname" = psql ]; then want=skip; fi ;;
        -j|--jobs|-n|--schema|--trigger|--use-list|-N|--exclude-schema|-S|--superuser|--function|-t|--table|-I|--index|--role)
                       if [ "$cmdname" = pg_restore ]; then want=skip; fi ;;
        -*)            ;;
        # psql の第1位置引数は dbname(-d を上書きする)。pg_restore の位置引数は入力ファイル。
        *)             if [ "$cmdname" = psql ] && [ "$pos" -eq 0 ]; then printf '%s\n' "$tok"; fi
                       pos=$((pos + 1)) ;;
      esac
    done
  }

  # 解決した接続先が 1 つ以上 && 全て _test で終わる か。URL はクエリとパスを落として見る。
  targets_all_test() {
    local t n=0 ok=1 IFS='
'
    for t in $1; do
      if [ -z "$t" ]; then continue; fi
      n=$((n + 1))
      t="${t%%\?*}"
      t="${t##*/}"
      case "$t" in *_test) ;; *) ok=0 ;; esac
    done
    if [ "$n" -eq 0 ]; then return 1; fi
    if [ "$ok" -eq 0 ]; then return 1; fi
    return 0
  }

  # **escape の判定は -c / -f より前の部分だけで行う。**全体を見ると、DSN の形をした文字列を SQL
  # 本文に書くだけで解除できる(2026-08-07 実測。接続先は live のまま exit 0 だった):
  #   psql -d stockbot -c "UPDATE t SET note='run psql bench_test now'"
  #   psql -d stockbot -c "UPDATE t SET n='STOCKBOT_HUMAN_APPROVED_DB_WRITE=1'"
  # %% は最長一致の suffix 除去 = 最初の該当位置で切る。承認 env は前置なので残る。
  # 切るのは**空白付きの " -c"**。"-c" だけだとホスト名の foo-cluster などで切り位置を誤って正当な
  # _test DSN を落とし、空白を必須にしないと -c"..." の密着形で切り落としが起きず SQL 本文が判定
  # 対象に戻る。区切りは空白文字クラスで見る — リテラルの空白 1 個だと**タブに変えるだけで抜けられる**。
  dsn_part="${cmd%%[[:space:]]-c*}"
  dsn_part="${dsn_part%%[[:space:]]--command*}"
  dsn_part="${dsn_part%%[[:space:]]-f*}"
  dsn_part="${dsn_part%%[[:space:]]--file*}"

  # **多行、または引用の外に ; & | があれば escape を一切認めない。**grep は行単位なので、1 行目
  # (や && の前)が _test DSN でも後続の live 書込に escape が効いてしまう(承認 env 側も同じ形で、
  # 言及しただけの行でも発火していた)。$( とバックティックも同様に不可。
  # 代償: `cd x && psql stockbot_test -c "..."` は deny される(コマンドを分ければ通る)。
  # 判定は AND リストにしない(偽のとき set -e が hook を殺し exit 1 = fail-open になる)。
  # 走査は 1 文字ずつで遅い(実測 16KB で約 2.5 秒)ので、長すぎるものは走査せず fail-close。
  is_escaped=0
  if [ "${#cmd}" -le 4000 ] && [ "$is_single_line" -eq 1 ] && ! has_unquoted "$cmd" ';&|' \
     && ! printf '%s' "$cmd" | grep -qE '\$\(|`'; then
    # 2 つ目の接続先は -c より後ろにも書けるので、解決は**切り落とす前の全体**で行う。
    targets="$(resolve_targets "$(fold_quoted "$cmd")")"
    if targets_all_test "$targets"; then is_escaped=1; fi
    # 承認 env は前置なので、SQL 本文に書かれた文字列と区別するため dsn_part 側で見る。
    if printf '%s' "$dsn_part" | grep -qE 'STOCKBOT_HUMAN_APPROVED_DB_WRITE=1'; then is_escaped=1; fi
  fi

  if [ "$is_escaped" -eq 0 ]; then
    # 書込指標を**2 本に分ける。**SQL 本文(書込動詞 / COPY <table> FROM。COPY (SELECT…) TO は read)は
    # 引用の中の「実行されるコード」なので生の $cmd で見る — sb_cmd だと `-c "INSERT …"` が畳まれて
    # 検出できない。一方 `-f`(連結形 -fpatch.sql 含む)/ stdin リダイレクト / パイプ / pg_restore は
    # **argv の形**なので `[^|;&]*` スパン = sb_cmd で見る(T13 経路2: `psql 'a|b' -d stockbot -f
    # patch.sql` が exit 0 だった)。
    # T10: psql のメタコマンド。`\copy … from` は `\` の直後が語境界なので COPY FROM ルールが拾う。
    # 単一引用符の中では ERE の `\\` がリテラルのバックスラッシュ 1 個(4 個は 2 個にマッチするので誤り)。
    # 🛑 **`\!` は任意のシェル実行**で、ここでしか止められない — `-c "…"` の本文は空白を含むので
    # fold_quoted が丸ごと Q に潰し、T4(`~/.stockbot`)も T7(`make stop`)も構造的に見えない
    # (実測: `psql -c "\! rm -rf backend/data"` が exit 0)。
    # 終端の `([^A-Za-z_]|$)` は、綴りが接頭辞になっている読み取り系を巻き込まないための条件
    # (無いと `\e`→`\echo` / `\s`→`\set` / `\w`→`\watch` / `\i`→`\if` を誤 deny する)。
    # ファイルに書く系(`\o \g \gx \w \write \s`)は**引数があるときだけ**見る(`\g` 単体は無害)。
    PSQLMETA='(\\(i|ir|include|include_relative|gexec|e|ef|!)([^A-Za-z_]|$)'\
'|\\(o|out|g|gx|w|write|s)[[:space:]]*[|>]'\
'|\\(o|out|g|gx|w|write|s)[[:space:]]+[^[:space:]|>])'
    # 書込動詞の直後が `'` なら SQL の文字列リテラル(`where action='delete'`)で、文ではない(H6)。
    # 動詞の後ろが空白でなくても文は成立する(`DELETE/**/FROM t`)ので、除くのは `'` だけ。
    printf '%s' "$cmd" | grep -qiE '(\b(INSERT|UPDATE|DELETE|ALTER|CREATE|GRANT|REVOKE)([^'"$SQ"'A-Za-z0-9_]|$)|\bCOPY[[:space:]]+[a-zA-Z_"][a-zA-Z0-9_$."]*[[:space:]]+FROM\b|'"$PSQLMETA"')' \
      && deny "live DB は read-only(SELECT のみ)。書込は migration / 人間承認(STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 前置)経由(plan §8.3)。"
    printf '%s' "$sb_cmd" | grep -qiE '(\bpsql\b[^|;&]*[[:space:]](-f[^[:space:]]*|--file[[:space:]=][^[:space:]]*)|\bpsql\b[^|;&]*<|\|[[:space:]]*psql\b|\bpg_restore\b)' \
      && deny "live DB は read-only(SELECT のみ)。書込は migration / 人間承認(STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 前置)経由(plan §8.3)。"
  fi
fi

# T14: 制御 API の変更系エンドポイント(2026-09-05 監査)。HTTP 層は loopback の同一ユーザーなら
# 無認証で、「解除は人間の POST のみ」(CLAUDE.md)は文章だけだった — curl 1 本で emergency-resume /
# flatten-all / 守りの cancel→再発注(protective/replace)が通っていた。
# HTTP クライアントが**セグメントの argv[0] の位置**にあり、同じセグメントに変更系パスがあるときだけ
# deny する。読み取り(GET /api/status 等)と**止める向き**の emergency-stop は通す。
# インタプリタ(python -c 等)はコードが Q に畳まれて見えないので、argv[0] がインタプリタで
# **生の $cmd** に変更系パスがあるときに deny(言及との区別はここでは付かないが、インタプリタの
# 引数にこのパスを書く読み取り用途は無い)。`grep` / `git commit -m` の言及は早期素通しか
# 畳み込みで消える。スクリプトの中で curl する形は hook から見えない = 「書いてから実行する 2 段」の
# 既知の限界。
# 🛑 `http` / `https`(HTTPie)は入れない — `"http://…/api/flatten-all"` の引用が argv[0] 相当の位置に
# 立つと URL そのものが当たり、言及を誤 deny する(この機械に HTTPie は無い)。
CTLPATH='/api/(live/)?(emergency-resume|flatten-all|positions/(close|extend)|protective/(replace|arm|reprice)|advisor-trigger)([^A-Za-z0-9_/-]|$)'
CTLPRE="$ARGV0PRE"
printf '%s' "$sb_cmd" | grep -qE "$CTLPRE"'(curl|wget|xh)\b[^|;&]*'"$CTLPATH" \
  && deny "制御 API の変更系(emergency-resume / flatten-all / positions/close|extend / protective/*)は人間専用(CLAUDE.md)。AI は叩かない。読み取り(GET /api/status 等)と emergency-stop(止める向き)は可。"
if printf '%s' "$sb_cmd" | grep -qE "$CTLPRE"'(python[0-9.]*|node|ruby|perl|php|deno|bun)\b'; then
  printf '%s' "$cmd" | grep -qE "$CTLPATH" \
    && deny "制御 API の変更系エンドポイントをインタプリタ経由で叩かない(人間専用・CLAUDE.md)。"
fi

exit 0
