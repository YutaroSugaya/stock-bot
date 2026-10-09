#!/usr/bin/env bash
# enforcement-guard.sh — PreToolUse deny hook(Edit / Write / NotebookEdit 用・T5)。
# 「ルールを機械強制しているファイル」を AI 自身が書き換えられない状態にする。
# Bash 経由の改変は pretooluse-deny.sh が同じ集合(enforcement-paths.sh)で拾う。
#
# 🛑 これが無いと enforcement は自己防衛できない。唯一の検出は pre-stop-checks.sh Step 4 の
# warn-once(2 回目の Stop で通過)で、その pre-stop-checks.sh 自体を書き換えれば
# 次の Stop から無効になる。**Edit/Write 経路については、この hook が本当の境界**
# (逃げ道はセッション env だけで、AI 側からは設定できない)。
#
# 🛑 **Bash 経路は境界ではない。**逃げ道がコマンド先頭の env 前置でも効く以上、
# AI は自分で書ける。加えて変数間接(`H=.claude/hooks; rm $H/x`)や書いてから実行する
# 2 段は正規表現の原理的限界で拾えない(§3 の既知の穴と同じ性質)。
# Bash 側の価値は「うっかり / 事故で自分の検査を消す」ことを止める点にある。
#
# 逃げ道: `STOCKBOT_HARNESS_EDIT_APPROVED=1 claude` で起動する(人間の明示的な承認)。
# narrow escape であってグローバルバイパスではない — 効くのはこの guard と
# pretooluse-deny.sh の enforcement ルールだけで、他の deny は従来どおり効く。
#
# 対象パスの集合は .claude/hooks/enforcement-paths.sh が正本(RF3 #6)。
# 🛑 ここに綴りを再掲しない。2 綴りになると必ず片方だけ更新されて穴になる
# (Makefile が pre-stop 側にだけ入っていた実例がある)。
#
# fail-open の向き: **入力が壊れているときは通す**。この hook は
# 「AI の自己改変」という限定的な脅威に対する守りで、Edit/Write を止めると日常作業が
# 全部死ぬ。集合の定義が読めないときだけは fail-CLOSE する(deny 対象ゼロに縮退させない)。
#
# テスト: bash .claude/hooks/enforcement-guard_test.sh
set -uo pipefail

# 人間が承認して起動したセッションでは何もしない。
if [ "${STOCKBOT_HARNESS_EDIT_APPROVED:-0}" = "1" ]; then
  exit 0
fi

input="$(cat 2>/dev/null || true)"

tool=""
if command -v jq >/dev/null 2>&1; then
  tool="$(printf '%s' "$input" | jq -r '.tool_name // empty' 2>/dev/null || true)"
fi
[ -z "$tool" ] && tool="$(printf '%s' "$input" | grep -oE '"tool_name"[[:space:]]*:[[:space:]]*"[^"]+"' | head -1 | sed -E 's/.*"([^"]+)"$/\1/')"

# 🛑 その時点で存在する編集系ツール名だけを入れる。2026-08 時点は Edit / Write / NotebookEdit。
# `MultiEdit` は**存在しない**(transcript 全走査で実 tool_use 0 件だったので T11 は棄却)。
case "$tool" in
  Edit|Write|NotebookEdit) ;;
  *) exit 0 ;;
esac

path=""
if command -v jq >/dev/null 2>&1; then
  path="$(printf '%s' "$input" | jq -r '.tool_input.file_path // .tool_input.notebook_path // empty' 2>/dev/null || true)"
fi
[ -z "$path" ] && path="$(printf '%s' "$input" | grep -oE '"(file_path|notebook_path)"[[:space:]]*:[[:space:]]*"[^"]+"' | head -1 | sed -E 's/.*"([^"]+)"$/\1/')"
[ -z "$path" ] && exit 0

hook_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
enf_lib="${hook_dir}/enforcement-paths.sh"
# 🛑 集合が読めないときだけ fail-CLOSE。ここを fail-open にすると
# `: > enforcement-paths.sh` の 1 手でこの guard が無効化できる。
if [ ! -f "$enf_lib" ]; then
  echo "DENIED by enforcement-guard.sh: enforcement-paths.sh が見つからない($enf_lib)。判定不能なので止める。" >&2
  exit 2
fi
# shellcheck source=/dev/null
. "$enf_lib" 2>/dev/null
if [ -z "${ENF_PATH_RE:-}" ]; then
  echo "DENIED by enforcement-guard.sh: enforcement-paths.sh に ENF_PATH_RE が無い(壊れた lib)。判定不能なので止める。" >&2
  exit 2
fi

# 🛑 **リポジトリ相対パスに正規化してから行頭固定の ENF_PATH_RE で見る。**
# 任意位置マッチ(ENF_CMD_RE)をパスに当てると、scratchpad や testdata の `Makefile` まで
# deny する(ゲート3 レビュー実測)。リポジトリ外のファイルは元から対象外。
repo_root="$(cd "$hook_dir/../.." 2>/dev/null && pwd)"

# 🛑 **綴りを正規化してから当てる。**`^` 固定の regex に生のパスを当てると、
# `/repo/./.claude/hooks/x` / `/repo//.claude/…` / `/repo/backend/../.claude/…` /
# `//Users/…` が全て素通りする(再レビュー実測。4 綴りとも同じ inode に解決する)。
# `.claude` **より後ろ**の揺れだけ直しても意味が無い。
enf_norm() { # $1 = パス -> 正規化して echo
  printf '%s' "$1" | sed -e 's|//*|/|g' -e 's|/\./|/|g' -e 's|^\./||' \
    | awk 'BEGIN{FS=OFS="/"} {n=0; for(i=1;i<=NF;i++){ if($i==".."&&n>0&&a[n]!=".."&&a[n]!=""){n--} else {a[++n]=$i} } s=a[1]; for(i=2;i<=n;i++) s=s"/"a[i]; print s}'
}
np="$(enf_norm "$path")"
rel=""
case "$np" in
  "$repo_root"/*) rel="${np#"$repo_root"/}" ;;
  # 🛑 ユーザ階層は **settings ファイルだけ**。`~/.claude/` を丸ごと対象にすると
  # memory / keybindings / agents まで deny する(実測)。危険なのは `"env"` ブロックを
  # 持つ settings で、そこに承認フラグを書けば次セッションから境界が消える点。
  "$HOME"/.claude/settings.json|"$HOME"/.claude/settings.local.json)
      rel=".claude/${np#"$HOME"/.claude/}" ;;
  /*)             exit 0 ;;
  *)              rel="$np" ;;
esac
[ -z "$rel" ] && exit 0

if printf '%s' "$rel" | grep -qE "$ENF_PATH_RE"; then
  echo "DENIED by enforcement-guard.sh: enforcement ファイル(hook / settings / githooks / guard / Makefile)の編集は禁止。" >&2
  echo "ルールを機械強制しているファイルなので、AI が自分で書き換えられない状態にしてある。" >&2
  echo "意図的に変更するなら人間の承認のうえ STOCKBOT_HARNESS_EDIT_APPROVED=1 claude で起動する。" >&2
  exit 2
fi

# H3: scripts の自己テストは書き換え・新規作成を通し、**空にする**書き込みだけ止める
# (削除は Bash 経路の側)。中身は jq でしか取り出せないので、jq が無ければ見ない(fail-open。
# この hook の他の入力不良と同じ向き)。
if [ -n "${ENF_TEST_PATH_RE:-}" ] && printf '%s' "$rel" | grep -qE "$ENF_TEST_PATH_RE" \
   && command -v jq >/dev/null 2>&1; then
  blank() { [ -z "$(printf '%s' "$1" | tr -d '[:space:]')" ]; }
  emptied=0
  case "$tool" in
    Write)
      blank "$(printf '%s' "$input" | jq -r '.tool_input.content // ""' 2>/dev/null)" && emptied=1 ;;
    Edit)
      # 置換後の全文を組み立てて空かを見る。new_string が空でないなら結果は空にならない。
      new_s="$(printf '%s' "$input" | jq -r '.tool_input.new_string // ""' 2>/dev/null)"
      if blank "$new_s" && [ -f "$np" ]; then
        old_s="$(printf '%s' "$input" | jq -r '.tool_input.old_string // ""' 2>/dev/null)"
        cur="$(cat "$np" 2>/dev/null)"
        if [ "$(printf '%s' "$input" | jq -r '.tool_input.replace_all // false' 2>/dev/null)" = "true" ]; then
          rest="${cur//"$old_s"/}"
        else
          rest="${cur/"$old_s"/}"
        fi
        [ -n "$old_s" ] && blank "$rest" && emptied=1
      fi ;;
  esac
  if [ "$emptied" -eq 1 ]; then
    echo "DENIED by enforcement-guard.sh: scripts の自己テスト(scripts/*_test.sh)を空にする書き込みは禁止。" >&2
    echo "書き換え・新規作成はできる。self-test を消せば検査が消えるので、削除と空化だけを止めている。" >&2
    exit 2
  fi
fi

exit 0
