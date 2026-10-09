#!/usr/bin/env bash
# secret-scan.sh — committed-secret スキャン。
# `make secret-scan` と pre-push hook(.githooks/pre-push)が共用する。
#
# 実際のキー形式(本体まで)を要求するので、解説文字列では誤検出しない。
# tracked files のみ走査。**docs(*.md)も対象**。除外は本スクリプト / hook 群 / *.example のみ。
set -euo pipefail

# パターンの正本は .claude/hooks/secret-patterns.sh(RF3 #4 で 1 か所に寄せた。以前は
# ここと pre-stop-checks.sh に別々の配列があり、双方向に取りこぼしていた)。
# 🛑 `cd` する前にスクリプト自身の位置から解決する(呼び出し元の cwd に依存しない)。
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$(git rev-parse --show-toplevel)"

pattern_lib="$script_dir/../.claude/hooks/secret-patterns.sh"
# fail-CLOSED: パターンが無い / 読めない / 空なら「clean」と報告しない。
# 秘密が 1 件も無いことと、スキャナが動いていないことを取り違えない。
[ -f "$pattern_lib" ] || { echo "!! secret-scan: パターン定義が見つからない: $pattern_lib" >&2; exit 2; }
set +e
# shellcheck source=/dev/null
. "$pattern_lib"
pattern_lib_rc=$?
set -e
[ "$pattern_lib_rc" -eq 0 ] || { echo "!! secret-scan: パターン定義の読み込みに失敗(rc=$pattern_lib_rc)" >&2; exit 2; }
[ "${#SECRET_PATTERNS[@]}" -gt 0 ] || { echo "!! secret-scan: SECRET_PATTERNS が空" >&2; exit 2; }
patterns=("${SECRET_PATTERNS[@]}")

# 除外: 本スクリプト / その自己テスト / hook 群(パターンを持つ) / examples / .env.example。
#
# docs(*.md)は **除外しない**。以前は除外していたが、wire 裏取りノートに実 API
# レスポンス(セッション仮想 URL = トークン相当)を貼る事故を誰も検知できない
# 恒久的な穴になっていた(2026-08-06 監査)。マスク表記を素通しするようパターン側を
# 直したので、docs も走査対象に含める。
excludes=(':!scripts/secret-scan.sh' ':!scripts/secret-scan_test.sh' ':!.claude/hooks/*' ':!.githooks/*' ':!*.example.*' ':!*.example')

# `-e` は必須。これが無いと `-----BEGIN…` のような `-` 始まりのパターンを git grep が
# **オプションとして解釈**して `error: unknown option` で終了し、その失敗を 2>/dev/null が
# 握り潰して「一致なし」に化ける。秘密鍵パターンはこれで一度も動いていなかった
# (2026-08-06 に実証)。同じ事故を二度と起こさないよう、以下では
#   rc=0 一致あり / rc=1 一致なし / rc>1 スキャナ自体の故障
# を区別し、**故障は clean と report せず exit 2 で落とす**(fail-close)。
found=0
for p in "${patterns[@]}"; do
  out=$(git grep -nIE -e "$p" -- "${excludes[@]}" 2>&1) && rc=0 || rc=$?
  case "$rc" in
    0) printf '%s\n' "$out"; found=1 ;;
    1) ;; # 一致なし
    *) echo "!! secret-scan: スキャナ自体が失敗しました(pattern: $p)" >&2
       printf '%s\n' "$out" >&2
       exit 2 ;;
  esac
done

if [ "$found" = "1" ]; then
  echo "!! secret-scan: 上記に秘密情報らしき文字列が tracked file に含まれています。除去して履歴も確認してください。" >&2
  exit 1
fi
echo "secret-scan: clean"
exit 0
