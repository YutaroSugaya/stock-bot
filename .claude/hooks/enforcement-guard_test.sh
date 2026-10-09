#!/usr/bin/env bash
# enforcement-guard_test.sh — enforcement-guard.sh(T5・Edit/Write 経路)の自己テスト。
#
# 🛑 Bash 経路(pretooluse-deny.sh の enforcement ルール)も同じ集合を使うので、
# **両方の消費側**をここで見る。片方だけ拘束すると、もう一方が黙って穴になる
# (RF3 #4 の secret パターンで実際に起きた形)。
set -uo pipefail
# 承認セッション(STOCKBOT_HARNESS_EDIT_APPROVED=1)から pre-push / make guard を回すと、その env が
# 継承されて enforcement の検査が丸ごと飛び、deny を期待するケースが赤くなる(逆に hook を壊しても
# 気づけない)。逃げ道を試すケースはケースの中で明示的に渡す。
unset STOCKBOT_HARNESS_EDIT_APPROVED
HOOKDIR="$(cd "$(dirname "$0")" && pwd)"
HOOK="$HOOKDIR/enforcement-guard.sh"
BASHHOOK="$HOOKDIR/pretooluse-deny.sh"
ENFLIB="$HOOKDIR/enforcement-paths.sh"
TMPROOT="$(mktemp -d)"
trap 'rm -rf "$TMPROOT"' EXIT
pass=0; fail=0

run() { # $1 = 期待 exit, $2 = 説明, $3 = JSON
  local expected="$1" desc="$2" json="$3" rc
  printf '%s' "$json" | bash "$HOOK" >/dev/null 2>&1
  rc=$?
  if [ "$rc" -eq "$expected" ]; then pass=$((pass + 1))
  else fail=$((fail + 1)); echo "FAIL [$desc] expected exit $expected got $rc"; fi
}
runb() { # Bash 経路。$1 = 期待 exit, $2 = 説明, $3 = コマンド
  local expected="$1" desc="$2" rc
  printf '{"tool_name":"Bash","tool_input":{"command":%s}}' "$(printf '%s' "$3" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))')" \
    | CLAUDE_PROJECT_DIR="$(cd "$HOOKDIR/../.." && pwd)" bash "$BASHHOOK" >/dev/null 2>&1
  rc=$?
  if [ "$rc" -eq "$expected" ]; then pass=$((pass + 1))
  else fail=$((fail + 1)); echo "FAIL [bash: $desc] expected exit $expected got $rc"; fi
}
ej() { printf '{"tool_name":"%s","tool_input":{"file_path":"%s"}}' "$1" "$2"; }

R="$(cd "$HOOKDIR/../.." && pwd)"

# --- Edit/Write 経路: DENY ---------------------------------------------------
run 2 "Edit hook 本体"         "$(ej Edit "$R/.claude/hooks/pretooluse-deny.sh")"
run 2 "Write hook 本体"        "$(ej Write "$R/.claude/hooks/pre-stop-checks.sh")"
run 2 "Edit 相対パスの hook"   "$(ej Edit .claude/hooks/docs-sync-check.sh)"
run 2 "Edit settings.json"     "$(ej Edit "$R/.claude/settings.json")"
run 2 "Edit settings.local"    "$(ej Edit "$R/.claude/settings.local.json")"
run 2 "Write githooks"         "$(ej Write "$R/.githooks/pre-push")"
run 2 "Edit arch-guard"        "$(ej Edit "$R/scripts/arch-guard.sh")"
run 2 "Edit secret-scan"       "$(ej Edit "$R/scripts/secret-scan.sh")"
run 2 "Edit pre-commit"        "$(ej Edit "$R/scripts/pre-commit.sh")"
run 2 "Edit Makefile"          "$(ej Edit "$R/Makefile")"
# `.*` を 1 行足すだけで docs-sync 検査を全無効化できる。hooks/ 配下なので同じ集合で覆われる。
run 2 "Edit spec-sync-allowlist" "$(ej Edit "$R/.claude/hooks/spec-sync-allowlist.txt")"
run 2 "Edit 集合の定義そのもの" "$(ej Edit "$R/.claude/hooks/enforcement-paths.sh")"
run 2 "Edit この guard 自身"   "$(ej Edit "$R/.claude/hooks/enforcement-guard.sh")"
run 2 "NotebookEdit hook"      "$(printf '{"tool_name":"NotebookEdit","tool_input":{"notebook_path":"%s"}}' "$R/.claude/hooks/a.ipynb")"
# 🛑 判定は**リポジトリ相対パス**に正規化してから行う。任意位置マッチをパスに当てていた頃は
# scratchpad や testdata の `Makefile` まで deny していた(ゲート3 レビュー実測)。
run 0 "リポ外の Makefile"      "$(ej Write /private/tmp/claude-501/scratchpad/Makefile)"
run 0 "testdata の Makefile"   "$(ej Write "$R/backend/testdata/Makefile")"
# ディレクトリ / 区切りの揺れ(シェル上は同じファイル)。
run 2 "Edit .claude//hooks"    "$(ej Edit "$R/.claude//hooks/pretooluse-deny.sh")"
run 2 "Edit .claude/./hooks"   "$(ej Edit "$R/.claude/./hooks/pretooluse-deny.sh")"
run 0 "Edit .claude 直下(agents は対象外)" "$(ej Edit "$R/.claude/agents/x.md")"
run 2 "Edit CI ワークフロー"   "$(ej Edit "$R/.github/workflows/ci.yml")"
# 🛑 **綴りを正規化してから当てる。**`^` 固定の regex に生のパスを当てると、
# `/repo/./.claude/…` / `/repo//.claude/…` / `/repo/backend/../.claude/…` / `//Users/…` が
# 全部素通りする(再レビューが 4 綴りとも同じ inode に解決することを実測)。
run 2 "Edit ./ を挟む"         "$(ej Edit "$R/./.claude/hooks/pretooluse-deny.sh")"
run 2 "Edit // を挟む"         "$(ej Edit "$R//.claude/hooks/pretooluse-deny.sh")"
run 2 "Edit .. を挟む"         "$(ej Edit "$R/backend/../.claude/hooks/pretooluse-deny.sh")"
run 2 "Edit 先頭 //"           "$(ej Edit "//${R#/}/.claude/hooks/pretooluse-deny.sh")"
run 2 "Edit ./Makefile"        "$(ej Edit "$R/./Makefile")"
# 🛑 ユーザ階層の `~/.claude/settings.json` も harness。`"env"` に承認フラグを書けば
# 次セッションから Edit 経路の境界が消える(再レビュー指摘)。
run 2 "Edit ~/.claude/settings" "$(ej Edit "$HOME/.claude/settings.json")"
run 2 "Edit ~/.claude/settings.local" "$(ej Edit "$HOME/.claude/settings.local.json")"
# 🛑 ユーザ階層で対象なのは **settings ファイルだけ**。`~/.claude/` を丸ごと対象にすると
# auto-memory / keybindings / agents まで deny する(実測)。危険なのは `"env"` を持つ settings。
run 0 "~/.claude の auto-memory" "$(ej Write "$HOME/.claude/projects/-Users-USERNAME-Desktop-stock-bot/memory/x.md")"
run 0 "~/.claude/keybindings"  "$(ej Edit "$HOME/.claude/keybindings.json")"
run 0 "~/.claude/agents"       "$(ej Write "$HOME/.claude/agents/foo.md")"
run 0 "リポ外の .claude 以外"  "$(ej Edit /tmp/othertree/.claude/hooks/x.sh)"

# --- Edit/Write 経路: ALLOW --------------------------------------------------
run 0 "Edit backend の Go"     "$(ej Edit "$R/backend/internal/domain/risk/gate.go")"
run 0 "Write docs"             "$(ej Write "$R/docs/runtime/STATUS.md")"
run 0 "Edit configs"           "$(ej Edit "$R/configs/hard_limits.yaml")"
run 0 "Write scratchpad"       "$(ej Write /private/tmp/claude-501/scratch/x.sh)"
run 0 "Edit ~/.stockbot"       "$(ej Edit "$HOME/.stockbot/env.snapshot")"
# 名前に似ているだけのファイルは通す(誤 deny するガードは無視されるようになる)。
run 0 "Edit docs の Makefile 言及" "$(ej Edit "$R/docs/workflows/MAKEFILE_NOTES.md")"
run 0 "Read 系ツールは対象外"  '{"tool_name":"Read","tool_input":{"file_path":"/x/.claude/hooks/a.sh"}}'
run 0 "Bash ツールは対象外"    '{"tool_name":"Bash","tool_input":{"command":"rm .claude/hooks/x.sh"}}'
# 入力が壊れているときは通す(Edit/Write を止めると日常作業が全部死ぬ)。
run 0 "空 stdin"               ""
run 0 "JSON でない"            "not json at all"
run 0 "file_path が無い"       '{"tool_name":"Edit","tool_input":{}}'

# --- 逃げ道 -----------------------------------------------------------------
approved_rc=0
printf '%s' "$(ej Edit "$R/.claude/hooks/pretooluse-deny.sh")" \
  | STOCKBOT_HARNESS_EDIT_APPROVED=1 bash "$HOOK" >/dev/null 2>&1 || approved_rc=$?
if [ "$approved_rc" -eq 0 ]; then pass=$((pass + 1))
else fail=$((fail + 1)); echo "FAIL [承認 env で通る] got $approved_rc"; fi
# 🛑 「逃げ道が効くこと」を試したら、必ず「逃げ道が無いとき検査が走ること」も試す
# (${VAR:-0} の既定値そのものが拘束されていない、という事故が A2 で実際に起きた)。
unset_rc=0
printf '%s' "$(ej Edit "$R/.claude/hooks/pretooluse-deny.sh")" \
  | STOCKBOT_HARNESS_EDIT_APPROVED= bash "$HOOK" >/dev/null 2>&1 || unset_rc=$?
if [ "$unset_rc" -eq 2 ]; then pass=$((pass + 1))
else fail=$((fail + 1)); echo "FAIL [承認 env が空なら検査する] got $unset_rc"; fi

# --- 集合の定義が壊れたら fail-CLOSE ----------------------------------------
# 🛑 fail-open にすると `: > enforcement-paths.sh` の 1 手でこの guard を無効化できる。
copy_hook() { # $1 = 変異(空文字なら正常コピー) -> hook のパスを echo
  local d="$TMPROOT/$2"; mkdir -p "$d"
  cp "$HOOK" "$d/"
  case "$1" in
    missing) ;;
    empty)   : > "$d/enforcement-paths.sh" ;;
    *)       cp "$ENFLIB" "$d/" ;;
  esac
  echo "$d/enforcement-guard.sh"
}
for m in missing empty; do
  h="$(copy_hook "$m" "c_$m")"
  rc=0
  printf '%s' "$(ej Edit "$R/.claude/hooks/pretooluse-deny.sh")" | bash "$h" >/dev/null 2>&1 || rc=$?
  if [ "$rc" -eq 2 ]; then pass=$((pass + 1))
  else fail=$((fail + 1)); echo "FAIL [集合の定義 $m は fail-CLOSE] got $rc"; fi
done
# 対照: 正常な定義を隣に置けば ALLOW も従来どおり出る(「何をしても 2」ではない)。
h="$(copy_hook ok c_ok)"
rc=0
printf '%s' "$(ej Edit "$R/backend/internal/domain/risk/gate.go")" | bash "$h" >/dev/null 2>&1 || rc=$?
if [ "$rc" -eq 0 ]; then pass=$((pass + 1))
else fail=$((fail + 1)); echo "FAIL [対照: 正常な定義で ALLOW] got $rc"; fi

# --- Bash 経路(pretooluse-deny.sh)-----------------------------------------
runb 2 "rm hook"               'rm .claude/hooks/pretooluse-deny.sh'
runb 2 "printf > hook"         'printf "exit 0\n" > .claude/hooks/pretooluse-deny.sh'
runb 2 "chmod -x hook"         'chmod -x .claude/hooks/pretooluse-deny.sh'
runb 2 "echo {} > settings"    'echo "{}" > .claude/settings.json'
runb 2 "sed -i arch-guard"     'sed -i "" s/foo/bar/ scripts/arch-guard.sh'
runb 2 "mv hook away"          'mv .claude/hooks/pre-stop-checks.sh /tmp/'
runb 2 "tee into hook"         'cat /tmp/x | tee .claude/hooks/pretooluse-deny.sh'
runb 2 "truncate Makefile"     'truncate -s 0 Makefile'
runb 2 "ln -sf over hook"      'ln -sf /dev/null .claude/hooks/session-start.sh'
runb 2 "append to githooks"    'echo x >> .githooks/pre-push'
# ALLOW: 読む / テストを走らせる / 承認 env つき
runb 0 "cat the hook"          'cat .claude/hooks/pretooluse-deny.sh'
runb 0 "run the hook test"     'bash .claude/hooks/pretooluse-deny_test.sh'
runb 0 "grep in hooks"         'grep -rn NOFOLD .claude/hooks/'
runb 0 "make guard"            'make guard'
runb 0 "approved env"          'STOCKBOT_HARNESS_EDIT_APPROVED=1 sed -i "" s/a/b/ .claude/hooks/pretooluse-deny.sh'
# 🛑 逆側: 承認 env が無ければ同じ綴りが deny されること(既定値そのものを拘束する)。
runb 2 "approved 無しの sed -i" 'sed -i "" s/a/b/ .claude/hooks/pretooluse-deny.sh'
# 🛑 逃げ道は**コマンド先頭の env 前置**だけ。任意位置を見ていた頃は、コメントに
# 書き足すだけで解除できた(ゲート3 レビュー実測 exit 0)。
runb 2 "コメントで自己承認"    'rm .claude/hooks/pretooluse-deny.sh # STOCKBOT_HARNESS_EDIT_APPROVED=1'
runb 2 "前段で自己承認"        'echo "STOCKBOT_HARNESS_EDIT_APPROVED=1 is the escape" && rm .claude/hooks/pretooluse-deny.sh'
runb 2 "commit message で承認" 'git commit -m "STOCKBOT_HARNESS_EDIT_APPROVED=1" && rm .claude/hooks/pretooluse-deny.sh'
# 🛑 **ディレクトリ単位**。最も素直な「hook を全部消す」綴りが全部通っていた。
runb 2 "rm -rf hooks ディレクトリ" 'rm -rf .claude/hooks'
runb 2 "rm -rf .claude"        'rm -rf .claude'
runb 2 "mv hooks ディレクトリ" 'mv .claude/hooks .claude/hooks_disabled'
runb 2 "mv .claude"            'mv .claude .claude_off'
runb 2 "rm -rf .githooks"      'rm -rf .githooks'
# 区切りの揺れ。
runb 2 "二重スラッシュ"        'rm .claude//hooks/pretooluse-deny.sh'
runb 2 "ドットスラッシュ"      'rm .claude/./hooks/pretooluse-deny.sh'
# cd で綴りが消える形(パスがコマンドから消えるのでスパン照合では拾えない)。
runb 2 "cd してから rm"        'cd .claude/hooks && rm pretooluse-deny.sh'
runb 2 "cd してから切り詰め"   '(cd .claude/hooks; : > pretooluse-deny.sh)'
runb 2 "入れ子シェルの中で cd" 'bash -c "cd .claude/hooks && rm pretooluse-deny.sh"'
runb 0 "cd 単体は通す"         'cd .claude/hooks && ls -la'
# インタプリタ経由の書込み。
runb 2 "python3 -c で上書き"   'python3 -c "open('"'"'.claude/hooks/pretooluse-deny.sh'"'"','"'"'w'"'"').write('"'"'exit 0'"'"')"'
runb 2 "node -e で上書き"      'node -e "require('"'"'fs'"'"').writeFileSync('"'"'.claude/settings.json'"'"','"'"'{}'"'"')"'
runb 0 "python3 でスクリプト実行" 'python3 scripts/merge_daily_from_json.py /tmp/x.json backend/data'
# git 経由の巻き戻し(旧版への差し替え = 実質的な書き換え)。
runb 2 "git checkout で巻き戻し" 'git checkout HEAD~5 -- .claude/hooks/pretooluse-deny.sh'
runb 2 "git restore で巻き戻し"  'git restore --source=HEAD~5 .claude/hooks/pretooluse-deny.sh'
runb 2 "git stash で退避"        'git stash push .claude/hooks/pretooluse-deny.sh'
runb 0 "git diff は通す"         'git diff .claude/hooks/pretooluse-deny.sh'
runb 0 "git add は通す"          'git add .claude/hooks/pretooluse-deny.sh'
# enforcement を「ファイルに触らず」外す経路。
runb 2 "hooksPath を潰す"      'git config core.hooksPath /dev/null'
runb 2 "--no-verify で commit" 'git commit --no-verify -m "bypass"'
runb 2 "CI ワークフロー削除"   'rm .github/workflows/ci.yml'
# パイプの向こうで動詞が走る形。
runb 2 "xargs rm"              'echo .claude/hooks/pretooluse-deny.sh | xargs rm'
runb 2 "xargs chmod -x"        'ls .claude/hooks/*.sh | xargs chmod -x'
runb 2 "find -exec chmod"      'find .claude/hooks -name "pretooluse-deny.sh" -exec chmod -x {} +'
# 動詞の綴り漏れ。
runb 2 "rsync で上書き"        'rsync /tmp/evil.sh .claude/hooks/pretooluse-deny.sh'
# 🛑 cp / rsync は**コピー先**が enforcement のときだけ止める。区別しないと
# 「退避のための取り出し」まで deny して、ガードそのものが無視されるようになる。
runb 2 "cp で上書き"           'cp /tmp/evil.sh .claude/hooks/pretooluse-deny.sh'
runb 2 "cp -f で settings 上書き" 'cp -f /tmp/evil.sh .claude/settings.json'
runb 0 "cp で退避(取り出し)"   'cp .claude/hooks/pretooluse-deny.sh /tmp/backup.sh'
runb 0 "cp で settings を退避" 'cp .claude/settings.json /tmp/settings.bak'
runb 0 "cp で Makefile を退避" 'cp Makefile /tmp/Makefile.orig'
# 🛑 フラグ付きでも「取り出し」は通す。`([[:space:]]+-[^[:space:]]+)*` を省略可能にしたまま
# 第1オペランドを `[^[:space:];&|]+` にすると、正規表現がバックトラックして**フラグ自体**を
# コピー元と解釈し、`cp -p <hook> /tmp/` まで deny した(再レビュー実測)。
runb 0 "cp -p で退避"          'cp -p .claude/settings.json /tmp/s.bak'
runb 0 "cp -r で退避"          'cp -r .claude /tmp/backup'
runb 0 "rsync -a で退避"       'rsync -a .claude/ /tmp/backup/'
runb 2 "cp -r で上書き"        'cp -r /tmp/evil .claude/hooks'
# 🛑 インタプリタのコード文字列は `;` を含む。スパンを `[^|;&]*` にしていると
# `import os;…` の実用的な one-liner がほぼ全部通る(再レビュー実測)。
runb 2 "python3 import 付き"   'python3 -c "import os;open('"'"'.claude/hooks/pretooluse-deny.sh'"'"','"'"'w'"'"').write('"'"'exit 0'"'"')"'
runb 2 "perl -e で unlink"     'perl -e "use strict;unlink '"'"'.claude/hooks/pretooluse-deny.sh'"'"'"'
# 🛑 `.github` もディレクトリ単位。`workflows` を綴ったときだけ止める形では素通りする。
runb 2 "rm -rf .github"        'rm -rf .github'
runb 2 "mv .github"            'mv .github /tmp/github_off'
# 🛑 `>` の直後に空白が無い形。リダイレクト規則が `>` を先に食べるので、Makefile の
# **左境界**が消費できず素通りしていた(再レビュー実測)。ENF_CMD_RE_NL がこれを埋める。
runb 2 "空白なしリダイレクト"  'printf "guard:\n\t@true\n" >Makefile'
runb 2 "cat >Makefile"         'cat >Makefile'
# 🛑 `.git/config` 直書きは `git config core.hooksPath` と等価。
runb 2 ".git/config 直書き"    'printf "[core]\n\thooksPath = /dev/null\n" >> .git/config'
runb 2 "self-test の削除"      'rm scripts/secret-scan_test.sh'
runb 2 "gsed -i"               'gsed -i s/a/b/ .claude/hooks/pretooluse-deny.sh'
# 🛑 `git config core.hooksPath` は**書き換えだけ**止める。read や手順書どおりの設定まで
# deny すると、HARNESS_SETUP.md の有効化手順そのものが実行できない。
runb 0 "hooksPath を読む"      'git config --get core.hooksPath'
runb 0 "hooksPath を手順どおり" 'git config core.hooksPath .githooks'
runb 2 "hooksPath を潰す(再)"  'git config core.hooksPath /dev/null'
# 🛑 Makefile は**パスの先頭要素のときだけ**。裸だとマシン上の全 Makefile が保護対象になる。
runb 0 "リポ外の Makefile 削除" 'rm /private/tmp/claude-501/scratchpad/Makefile'
runb 0 "testdata の Makefile"  'rm backend/testdata/Makefile'
runb 2 "直下の Makefile 切り詰め" 'truncate -s 0 Makefile'
# 🛑 `--no-verify` は git の文脈でだけ。docs の grep を誤 deny しない。
runb 0 "docs で --no-verify を grep" 'grep -rn -- "--no-verify" docs/'
# 🛑 cd ルールは**相対パスへの書込**だけを見る。テスト出力の退避を止めない。
runb 0 "cd してテスト出力を退避" 'cd .claude/hooks && bash pretooluse-deny_test.sh > /tmp/out.txt'
runb 0 "cd して wc を退避"     'cd .claude/hooks && wc -l *.sh > /tmp/counts'
# 対照: 無関係なパスは通す。
runb 0 "無関係な .claude-cache" 'rm -rf node_modules/.claude-cache'
runb 0 ".github-notes.md"      'echo x >> docs/.github-notes.md\'
runb 2 "patch を当てる"        'patch -p1 .claude/hooks/pretooluse-deny.sh < /tmp/x.diff'

# --- H3(2026-10-01): 対象を hooks / settings / githooks / guard / Makefile に絞る ---------
# `.claude/` の中で守るのは hooks/ と settings* だけ。skills / agents は AI が直してよい。
run 0 "H3 Edit skills"                "$(ej Edit "$R/.claude/skills/add-migration/SKILL.md")"
run 0 "H3 Write 新しい skill"         "$(ej Write "$R/.claude/skills/new-skill/SKILL.md")"
run 2 "H3 Edit settings 系の別名"     "$(ej Edit "$R/.claude/settings.backup.json")"
run 2 "H3 Edit hooks の下の新規"      "$(ej Write "$R/.claude/hooks/new-hook.sh")"
# `..` で hooks を指す綴りは正規化で拾う(skills が対象外になったので、ここが新しい抜け道になりうる)。
run 2 "H3 Edit skills/../hooks"       "$(ej Edit "$R/.claude/skills/../hooks/pretooluse-deny.sh")"
runb 0 "H3 rm skill ファイル"          'rm .claude/skills/add-migration/old.md'
runb 0 "H3 sed -i skill"               'sed -i "" s/a/b/ .claude/skills/add-migration/SKILL.md'
runb 2 "H3 rm -rf .claude/*"           'rm -rf .claude/*'
runb 2 "H3 rm -rf .claude/h*"          'rm -rf .claude/h*'
runb 2 "H3 rm -rf .claude/"            'rm -rf .claude/'
runb 2 "H3 skills/.. で hooks を消す"  'rm -rf .claude/skills/../hooks'
runb 2 "H3 settings を mv"             'mv .claude/settings.json /tmp/s.json'

# scripts の自己テスト: 書き換え・新規作成は通し、削除と空化だけ止める。
T_EXIST="$R/scripts/stop-backup_test.sh"
wj() { # $1 = path, $2 = content -> Write の JSON
  python3 -c 'import json,sys;print(json.dumps({"tool_name":"Write","tool_input":{"file_path":sys.argv[1],"content":sys.argv[2]}}))' "$1" "$2"
}
edj() { # $1 = path, $2 = old, $3 = new, $4 = replace_all(true/false) -> Edit の JSON
  python3 -c 'import json,sys;print(json.dumps({"tool_name":"Edit","tool_input":{"file_path":sys.argv[1],"old_string":sys.argv[2],"new_string":sys.argv[3],"replace_all":sys.argv[4]=="true"}}))' "$1" "$2" "$3" "$4"
}
run 0 "H3 Write 新しい scripts テスト"  "$(wj "$R/scripts/new-thing_test.sh" '#!/usr/bin/env bash
echo ok')"
run 0 "H3 Edit 既存 scripts テスト"     "$(edj "$T_EXIST" 'set -uo pipefail' 'set -euo pipefail' false)"
run 2 "H3 Write で空にする"              "$(wj "$T_EXIST" '')"
run 2 "H3 Write で空白だけにする"        "$(wj "$T_EXIST" '

  ')"
run 2 "H3 Edit で全文を消す"             "$(edj "$T_EXIST" "$(cat "$T_EXIST")" '' false)"
run 0 "H3 Edit で一部を消す"             "$(edj "$T_EXIST" 'set -uo pipefail' '' false)"
runb 2 "H3 rm scripts テスト"            'rm scripts/stop-backup_test.sh'
runb 2 "H3 git rm scripts テスト"        'git rm scripts/stop-backup_test.sh'
runb 2 "H3 mv scripts テスト"            'mv scripts/stop-backup_test.sh /tmp/'
runb 2 "H3 : > で空にする"               ': > scripts/stop-backup_test.sh'
runb 2 "H3 truncate"                     'truncate -s 0 scripts/stop-backup_test.sh'
runb 2 "H3 cp /dev/null"                 'cp /dev/null scripts/stop-backup_test.sh'
runb 2 "H3 xargs rm"                     'echo scripts/stop-backup_test.sh | xargs rm'
runb 0 "H3 sed -i scripts テスト"        'sed -i "" s/a/b/ scripts/stop-backup_test.sh'
runb 0 "H3 >> で追記"                    'echo "# x" >> scripts/stop-backup_test.sh'
runb 0 "H3 grep rm scripts テスト"       'grep -n rm scripts/stop-backup_test.sh'
runb 0 "H3 scripts テストを走らせる"     'bash scripts/stop-backup_test.sh'
runb 0 "H3 テスト出力を退避"             'bash scripts/stop-backup_test.sh > /tmp/out.txt'

# --- 🛑 配線されている hook に実行ビットがあること -------------------------
# 実行ビットが無いと Claude Code は exit 126(non-blocking error)を受け取り、
# **deny が丸ごと効かなくなる**。テストは `bash "$HOOK"` で呼ぶので 46 件全緑のまま
# 素通りしていた(ゲート3 レビューが実セッションで Edit の通過を確認)。
for h in "$HOOKDIR"/*.sh; do
  if [ -x "$h" ]; then pass=$((pass + 1))
  else fail=$((fail + 1)); echo "FAIL [実行ビット] $h に +x が無い"; fi
done

echo "enforcement-guard_test: pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
