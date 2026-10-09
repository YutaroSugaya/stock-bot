#!/usr/bin/env bash
# pre-commit.sh — staged 差分の秘密漏洩チェック。.githooks/pre-commit から呼ばれる
# (有効化: `git config core.hooksPath .githooks`)。staged の追加行のみを見る。
set -euo pipefail

# パターンの正本は .claude/hooks/secret-patterns.sh(RF3 #4)。以前はここが独自の 10 個の
# 配列を持っていて正本から drift し、**commit 時のゲートが push 時より弱かった**:
# `gh[ousr]_`(ghp_ 以外)/ OpenAI 形式 `sk-…` / 立花のセッション仮想 URL / `sUrlRequest`
# が素通りしていた(実キー文字列で実測)。後ろ 2 つは 2026-08-06 の実際の事故を受けて
# 正本に足したパターンで、**その事故と同じ種類が commit 時だけ抜けていた**。
# pre-commit は secret がローカル履歴に入る前に止まる唯一の層なので、ここが一番弱いのは
# 向きが逆。正本を source して drift を構造的に不可能にする。
#
# 🛑 `cd` する前にスクリプト自身の位置から解決する(呼び出し元の cwd に依存しない)。
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$(git rev-parse --show-toplevel)"

pattern_lib="$script_dir/../.claude/hooks/secret-patterns.sh"
# fail-CLOSED: パターンが無い / 読めない / 空なら commit を通さない。
# 秘密が 1 件も無いことと、スキャナが動いていないことを取り違えない。
[ -f "$pattern_lib" ] || { echo "✗ pre-commit: パターン定義が見つからない: $pattern_lib" >&2; exit 2; }
set +e
# shellcheck source=/dev/null
. "$pattern_lib"
pattern_lib_rc=$?
set -e
[ "$pattern_lib_rc" -eq 0 ] || { echo "✗ pre-commit: パターン定義の読み込みに失敗(rc=$pattern_lib_rc)" >&2; exit 2; }
[ "${#SECRET_PATTERNS[@]}" -gt 0 ] || { echo "✗ pre-commit: SECRET_PATTERNS が空" >&2; exit 2; }
PATTERNS=("${SECRET_PATTERNS[@]}")

# ADDED 行のみ。削除行(-)とヘッダ(+++)は除外して誤検出を防ぐ。
staged_added=$(git diff --cached --no-color | grep -E '^\+' | grep -Ev '^\+\+\+ ' || true)

violations=0
if [ -n "${staged_added}" ]; then
  for pat in "${PATTERNS[@]}"; do
    # `-e` で先頭が `-` のパターン(-----BEGIN…)を flag と誤認させない。
    # 🛑 `echo … | grep -q` にしない。`grep -q` が最初の一致で抜けると上流の echo が
    # SIGPIPE(141)で死に、`set -o pipefail` が条件を**偽**にする = **秘密があるのに
    # 素通り**する。閾値はパイプバッファ(64KB)なので普通の作業の staged diff で超える
    # (実測: 227KB の diff の先頭に AWS key を置くと rc=0 で通った)。
    # here-string はパイプではないので SIGPIPE が原理的に起きない
    # (pre-stop-checks.sh が同じ穴を塞いだのと同じ処方)。
    if grep -E -q -e "${pat}" <<< "${staged_added}"; then
      echo "✗ pre-commit: 秘密情報らしきパターン検出: ${pat}"
      violations=$((violations + 1))
    fi
  done
fi

# 秘密ファイルを staged したら拒否。*.der / *.pfx は PEM と同じ鍵の別形式で、
# 以前はここも .gitignore の拡張子パターンも素通りしていた(2026-08-06 監査)。
# secrets/ 配下は形式を問わず一律拒否する。
staged_files=$(git diff --cached --name-only --diff-filter=AM || true)
while IFS= read -r f; do
  case "${f}" in
    .env|*/.env|*.env.local|*.pem|*.key|*.der|*.p12|*.pfx|secrets/*|*/secrets/*)
      echo "✗ pre-commit: 秘密情報ファイルの commit を拒否: ${f}"
      violations=$((violations + 1))
      ;;
  esac
done <<< "${staged_files}"

if [ "${violations}" -gt 0 ]; then
  echo ""
  echo "Commit を中止しました。上記の秘密情報を除去するか、ファイルを unstage してください。"
  exit 1
fi
exit 0
