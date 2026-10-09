#!/usr/bin/env bash
# enforcement(ルールを機械強制しているファイル)の集合の**正本**。source して使う(RF3 #6)。
#
# 🛑 なぜ 1 か所か。同じ概念の regex が
#   - `pre-stop-checks.sh` Step 4(git の**パス一覧**に当てる。行頭固定)
#   - T5 の Bash / Edit ガード(**コマンド文字列**に当てる。任意位置)
# の 2 綴りで存在し、**`Makefile` の扱いが実際に割れていた**(前者にあり後者に無い)。
# T5 は更に 2 か所へ増やす設計だったので、着手前に中核をここへ寄せた。
# **形が 2 つ要るのは事実**(行頭固定 / 任意位置)なので、中核の集合だけを 1 つ持ち、
# 2 つの形はそこから導く。片方だけ足す事故が構造的に起きない形にすること。
#
# 🛑 `Makefile` は**含める**。`guard:` ターゲット本体(= どの自己テストを回すか)を持っており、
# ここを書き換えれば検査を丸ごと外せる。以前 T5 の下書きから漏れていたのは事故であって
# 判断ではない(pre-stop 側には最初から入っていた)。
#
# 🛑 `.claude/hooks/spec-sync-allowlist.txt` は `\.claude/hooks/` に含まれるので個別指定は不要。
# `.*` を 1 行足すだけで docs-sync 検査を全無効化できるため、対象であることが必須。
# (有効エントリは現在 0 件。`docs-sync-check.sh` が読む。)
#
# 🛑 bash 3.2 の語彙に限定(連想配列 / nameref / ${var^^} は使えない)。

# 中核: enforcement のパス断片。**足すならここだけ**。
#
# 🛑 **ディレクトリそのもの**を対象にする。着手直後の版は `\.claude/hooks/` と綴っていたため、
# **最も素直な綴りが全部素通りしていた**(`rm -rf .claude/hooks` / `mv .claude .claude_off` /
# `rm -rf .githooks` が exit 0。ゲート3 のレビューが実測)。
# 🛑 `.claude/` の中で対象にするのは `hooks/` と `settings*` だけ(2026-10-01 H3)。丸ごと対象に
# していた間は skills を AI が直せず、古い記述が放置された。代わりに `.claude` そのもの
# (`rm -rf .claude` / `mv .claude x`)・先頭要素の glob(`rm -rf .claude/*`)・`..` を含む綴り
# (`.claude/skills/../hooks`)を対象に残す — どれも hooks を名指しせずに hooks を消せる形。
# 🛑 区切りを `/` 1 個に固定しない。`.claude//hooks/x` と `.claude/./hooks/x` はシェル上
# まったく同じファイルなのに、固定すると素通りする(同レビュー実測)。`/+(\.?/+)*` で吸収する。
# 🛑 `Makefile` は**パス境界**を付ける。裸だと `rm /tmp/Makefile.bak` まで deny する(実測)。
# 左境界の `(^|[^A-Za-z0-9_.-])` は grep -E の拡張に依存する(この集合は grep -E からしか使わない)。
# 🛑 `.github/workflows/` も入れる。CI は 2026-08-10 に廃止したので現在この配下は空だが、
# 集合から外さない — 復活させた瞬間にまた「そこにしか無い検査」が生まれる側だから
# (実際、廃止時に CI が回していた自己テスト 8 本のうち 1 本がどこからも走らなくなり、
# 5 本が `make guard` = 人間が手で打つ時だけ、へ降格していた。2026-08-12 に pre-push で回収)。
ENF_SEP='(/|$|[^A-Za-z0-9_.-])'
# 🛑 `.github` も**ディレクトリ単位**。`workflows` まで綴ったときだけ止める形だと
# `rm -rf .github` が素通りする(再レビュー実測)。`.github-notes.md` は `-` が
# 区切りクラスに入らないので当たらない。
# 🛑 `.git/config` と `.git/hooks` も対象。`[core] hooksPath = /dev/null` を 1 行足すだけで
# git hooks 層が丸ごと外れる(再レビュー実測 exit 0)。`git config core.hooksPath` を
# 塞いでもファイル直書きが残っていた。
# 🛑 `scripts/*_test.sh` はここに入れない(下の ENF_TEST_*。削除と空化だけを止める)。
ENF_CLAUDE='\.claude/*($|[^A-Za-z0-9_./-])|\.claude/+(\.?/+)*(hooks|settings[A-Za-z0-9_.-]*)'"$ENF_SEP"'|\.claude/+[^[:space:];&|<>/]*[*?[{]|\.claude/+[^[:space:];&|<>]*\.\.'
ENF_BODY="$ENF_CLAUDE"'|\.githooks'"$ENF_SEP"'|\.github'"$ENF_SEP"'|\.git/+(\.?/+)*(config|hooks'"$ENF_SEP"')|scripts/+(\.?/+)*(arch-guard|secret-scan|pre-commit)\.sh'
# 🛑 `Makefile` は**パスの先頭要素のときだけ**。裸で書くと `rm /tmp/Makefile` や
# `rm backend/testdata/Makefile` まで deny する(再レビュー実測 FD2)。`/` に続く形を
# 除くことで「リポジトリ直下の Makefile」だけを指す。
ENF_MAKE_L='(^|[[:space:];&|(`><]|\./)'
ENF_MAKE='Makefile([^A-Za-z0-9_.-]|$)'
ENF_CORE="$ENF_BODY"'|'"$ENF_MAKE_L$ENF_MAKE"

# 形 (1): git が出す相対パスの一覧に 1 行ずつ当てる(行頭固定)。
ENF_PATH_RE="^($ENF_CORE)"

# 形 (2): コマンド文字列の任意位置に当てる(T5 の Bash ガード用)。
ENF_CMD_RE="($ENF_CORE)"

# 形 (3): **左境界が既に消費されている文脈**用(リダイレクト演算子の直後など)。
# `>Makefile`(空白なし)は `>` が左境界だが、リダイレクト規則が `>` を先に食べてしまうので
# 形 (2) では当たらない(再レビュー実測 H4: `printf … >Makefile` が exit 0)。
ENF_CMD_RE_NL="($ENF_BODY|$ENF_MAKE)"

# scripts の自己テスト。書き換え・新規作成は通し、**削除と空化だけ**を止める(2026-10-01 H3)。
# 丸ごと対象にしていた間は新しい `scripts/x_test.sh` を Write できず、本体は書けるのにテストが
# 書けない = strict TDD と逆向きだった。self-test を消せば検査が消える、という守りの理由は
# 削除と空化だけで満たせる。`.claude/hooks/*_test.sh` は hooks/ 側で丸ごと対象のまま。
ENF_TEST_BODY='scripts/+(\.?/+)*[A-Za-z0-9_.-]*_test\.sh'
ENF_TEST_PATH_RE="^($ENF_TEST_BODY)\$"
ENF_TEST_CMD_RE="($ENF_TEST_BODY)"
