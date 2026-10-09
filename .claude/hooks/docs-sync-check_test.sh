#!/usr/bin/env bash
# docs-sync-check.sh の自己テスト。
# 使い捨ての git リポジトリを毎ケース作り、hook を実際に流して exit code を見る。
# 既知のハーネス乖離: テンプレートは spec-sync-allowlist.txt を置かない(allowlisted
# ケースだけが自前で作る)。実リポの allowlist は現在コメントのみで実質空なので等価だが、
# 実 allowlist にパターンが入ると「テンプレでは緑・実リポでは無効化」の乖離が生まれる。
# 変更ファイル一覧を注入する形にしなかったのは、この hook のロジックの大半が
# 「git から何を拾うか」(working tree / 直近 commit / session 範囲 / untracked)そのもので、
# 注入するとその部分がテストの外に出てしまうため。
set -uo pipefail

HOOKDIR="$(cd "$(dirname "$0")" && pwd)"
HOOK="$HOOKDIR/docs-sync-check.sh"
TMPROOT="$(mktemp -d)"
trap 'rm -rf "$TMPROOT"' EXIT

# ケースの並列実行と集計は共有ハーネス(RF3 #8)。無いまま走ると全ケースが黙って
# 消えて「pass=0 fail=0」の嘘の緑になるので fail-CLOSED にする。
# shellcheck source=/dev/null
. "$HOOKDIR/e2e-harness.sh" 2>/dev/null || true
command -v h_defer >/dev/null 2>&1 || { echo "e2e-harness.sh が読めない" >&2; exit 1; }
h_init "$TMPROOT"

# テンプレートリポジトリ(1 回だけ作って以降は cp -r で複製する)
TPL="$TMPROOT/_tpl"

build_template() {
    mkdir -p "$TPL"/backend/internal/domain/risk \
             "$TPL"/backend/internal/domain/position \
             "$TPL"/backend/internal/safety \
             "$TPL"/backend/internal/port \
             "$TPL"/backend/internal/app/handler \
             "$TPL"/backend/internal/usecase/command \
             "$TPL"/backend/internal/usecase/query \
             "$TPL"/backend/internal/adapter/repository \
             "$TPL"/backend/cmd/stockbot \
             "$TPL"/docs/architecture/layers \
             "$TPL"/docs/runtime \
             "$TPL"/docs/workflows \
             "$TPL"/migrations

    # 先頭 `/` 必須: 無いと gitignore は任意階層に当たり docs/runtime/ まで無視する
    # (DATA_MODEL.md / STATE_MACHINE.md が git から見えなくなり、テストが嘘の緑になる)。
    printf '/runtime/\n' > "$TPL/.gitignore"

    go_stub() { # $1 = path, $2 = package
        printf 'package %s\n\nfunc helper() int { return 1 }\n' "$2" > "$1"
    }
    go_stub "$TPL/backend/internal/domain/risk/gate.go" risk
    go_stub "$TPL/backend/internal/safety/circuit.go" safety
    go_stub "$TPL/backend/internal/port/repository.go" port
    go_stub "$TPL/backend/internal/port/broker.go" port
    go_stub "$TPL/backend/internal/adapter/repository/pg.go" repository
    mkdir -p "$TPL/backend/internal/adapter/tachibana"
    go_stub "$TPL/backend/internal/adapter/tachibana/client.go" tachibana

    # 実リポと同じ形(grouped const / struct / interface)を持たせる。
    # 契約変更の主要形はこの 3 つで、どれも 0 桁目には現れない。
    cat > "$TPL/backend/internal/domain/position/state.go" <<'GOEOF'
package position

type Status string

const (
	StatusOpen   Status = "OPEN"
	StatusClosed Status = "CLOSED"
)

type Phase int

const (
	PhaseIdle Phase = iota
	PhaseDone
)

type Base struct{}

type Position struct {
	Base
	ID   int64
	Name string
}

func Compute(a int) int {
	helper(1)
	return a
}
GOEOF
    # 0 桁目に exported 宣言を持たない(= grouped だけの)ファイル。rename の分解経路が
    # 効いているかは、これでないと測れない(state.go は type Status で拾われてしまう)。
    cat > "$TPL/backend/internal/domain/position/enums.go" <<'GOEOF'
package position

const (
	KindA = "A"
	KindB = "B"
)
GOEOF
    # decl_lines の判定(行末コメント・文字列内の //・関数本体・複数行シグネチャ)を測る
    # 素材。Tier 1 に残る層(domain/position)に置かないと、判定を壊しても緑のままになる。
    cat > "$TPL/backend/internal/domain/position/contract.go" <<'GOEOF'
package position

type Broker interface {
	Place(id int64) error
}

// MaxRetry は行末コメントつきの exported 定数(コメントだけの修正で誤 deny しないか用)
const MaxRetry = 3 // リトライ回数

// 値の中に // を含む exported 変数(コメント除去が文字列を食わないか用)
var DefaultEndpoint = "https://api.example.com/v1/orders"

// 本体を持つ exported 変数(ctx が var X = func(){ になる形)。
// 本体の変数は大文字で始める — 小文字だと「ctx が func( ならインデント行を見ない」の
// ガードを外しても判定に入らず、回帰を検出できないテストになる。
var Handler = func() int {
	Local := 1
	return Local
}

// 複数行シグネチャの exported 関数(引数だけの変更を拾えるか用)
func Compute(
	a int,
	b int,
) int {
	return a + b
}
GOEOF
    go_stub "$TPL/backend/internal/app/handler/handler.go" handler
    go_stub "$TPL/backend/internal/usecase/command/execute.go" command
    go_stub "$TPL/backend/internal/usecase/query/view.go" query
    # 実リポの adapter/repository と同じ形: 関数本体にインデントされた生文字列 SQL。
    # (置き場所は domain/position。adapter は Tier 1 から外したので、そこでは判定を測れない)
    # SQL の行は「名前 型」とまったく同じ形なので、引数と誤認すると 1 語直しただけで
    # block する。単一行(ctx 判別子 `^func .*\($`)と複数行(タブ1個限定)の両方を置く。
    cat > "$TPL/backend/internal/domain/position/pg.go" <<'GOEOF'
package position

func helper() int { return 1 }

func ListRuns(limit int) string {
	return `
	SELECT id
	FROM advisor_runs
	ORDER BY started_at DESC
`
}

func ListScores(
	limit int,
	offset int,
) string {
	return `
		SELECT id
		FROM positions p
		ORDER BY opened_at DESC
`
}

func ListMixed(
	limit int,
	offset int,
) string {
	return `
	 SELECT id
	 FROM mixed m
	 ORDER BY id DESC
`
}
GOEOF
    go_stub "$TPL/backend/cmd/stockbot/main.go" main

    for d in domain position port usecase handler adapter safety; do
        printf '# %s\n\nbaseline\n' "$d" > "$TPL/docs/architecture/layers/$d.md"
    done
    printf '# FAILURE_MODES\n\nbaseline\n'  > "$TPL/docs/architecture/FAILURE_MODES.md"
    printf '# STATE_MACHINE\n\nbaseline\n'  > "$TPL/docs/runtime/STATE_MACHINE.md"
    printf '# DATA_MODEL\n\nbaseline\n'     > "$TPL/docs/runtime/DATA_MODEL.md"
    printf '# MIGRATIONS\n\nbaseline\n'     > "$TPL/docs/workflows/MIGRATIONS.md"
    printf '# ARCHITECTURE\n\nbaseline\n'   > "$TPL/docs/ARCHITECTURE.md"
    printf 'CREATE TABLE t (id int);\n'     > "$TPL/migrations/0001_init.up.sql"

    git -C "$TPL" init -q
    git -C "$TPL" config user.email test@example.com
    git -C "$TPL" config user.name test
    git -C "$TPL" add -A
    git -C "$TPL" commit -qm base1
    # HEAD~ が必ず存在するように 2 コミット作る(hook は HEAD~..HEAD も見る)。
    printf 'second\n' >> "$TPL/docs/ARCHITECTURE.md"
    git -C "$TPL" add -A
    git -C "$TPL" commit -qm base2
}

new_repo() { # $1 = case id -> repo path を echo
    local repo="$TMPROOT/$1"
    cp -r "$TPL" "$repo"
    echo "$repo"
}

run_hook() { # $1 = repo, $2 = stderr 出力先; exit code を echo
    local repo="$1" errfile="$2" rc=0
    # 人間のシェルに STOCKBOT_DOCS_SYNC_CHECKS=off が export されていても
    # テストが素通りしない(= make guard が嘘の緑にならない)よう明示的に on を渡す。
    printf '{"session_id":"selftest"}' \
        | ( cd "$repo" && STOCKBOT_DOCS_SYNC_CHECKS=on CLAUDE_PROJECT_DIR="$repo" \
            bash "$HOOK" >/dev/null 2>"$errfile" ) || rc=$?
    echo "$rc"
}

# seed=seed を渡すと「このセッションで docs-sync が既に 1 回走った」状態を作る
# (session 開始 SHA + docs-sync 自前の印の両方)。渡さないとセッション最初の Stop 相当。
# 🛑 呼んだ時点では走らない(RF3 #8)。全ケースを登録し終えてから h_run_deferred が
# 並列に流す。ケースは自分専用の repo しか触らないので順序に依存しない。
case_run() { h_defer case_run_body "$@"; }

case_run_body() {
    local id="$1" want="$2" desc="$3" setup="$4" expect="${5:-}" seed="${6:-}"
    local repo; repo="$(new_repo "$id")"
    if [ "$seed" = "seed" ]; then
        mkdir -p "$repo/runtime/logs"
        git -C "$repo" rev-parse HEAD > "$repo/runtime/logs/session-start-sha.selftest"
        : > "$repo/runtime/logs/docs-sync-seen.selftest"
    fi
    local head_before; head_before="$(git -C "$repo" rev-parse HEAD)"
    # セットアップの失敗を握り潰さない(パスのタイポで「何も変更していない」状態になり、
    # want=0 のケースが嘘の緑になる)。
    if ! ( cd "$repo" && eval "$setup" ) >/dev/null 2>&1; then
        h_fail "$desc (セットアップが失敗)"; return
    fi
    # sed / perl はパターンが一致しなくても exit 0 なので、終了コードだけでは
    # 「1 バイトも変更していない」を検出できない。作業ツリーか HEAD が動いたことまで見る。
    if [ "$id" != "nochange" ] \
       && [ -z "$(git -C "$repo" status --porcelain)" ] \
       && [ "$head_before" = "$(git -C "$repo" rev-parse HEAD)" ]; then
        h_fail "$desc (セットアップが何も変更していない)"; return
    fi
    local got; got="$(run_hook "$repo" "$TMPROOT/$id.err")"
    check "$desc" "$want" "$got"
    # block ケースは exit 2 だけを見ると hook のクラッシュ(bash の構文エラーも 2)と
    # 区別できないので、違反メッセージに期待する語が出ているかまで見る。
    if [ -n "$expect" ] && [ "$got" = "$want" ]; then
        check "$desc (stderr に '$expect')" "ok" \
            "$(grep -q -- "$expect" "$TMPROOT/$id.err" 2>/dev/null && echo ok || echo missing)"
    fi
}

touch_doc() { printf 'updated\n' >> "$1"; }

build_template

# 既存 6 ルールの回帰(A1 で 1 つも壊してはいけない)
# H2: migration の docs 要求は pre-stop-checks.sh に一本化した(同じ違反で 2 本とも block していた)。
case_run mig-pass 0 'migration 変更だけ → pass(検査は pre-stop 側)' \
    'printf "ALTER TABLE t ADD COLUMN c int;\n" > migrations/0021_x.up.sql'

# 案内文は「AI が自分でできること」だけを書く。allowlist は enforcement 側にあって AI からは
# 編集できないので、そこへの追加は人間に頼む形で案内する。
case_run port-block 2 'port/repository.go 変更 / doc 未更新 → block' \
    'printf "\nfunc other() {}\n" >> backend/internal/port/repository.go' \
    '人間に頼む'

case_run port-ok 0 'port/repository.go 変更 + port.md → pass' \
    'printf "\nfunc other() {}\n" >> backend/internal/port/repository.go
     printf "updated\n" >> docs/architecture/layers/port.md'

case_run safety-internal-block 2 'safety の内部変更 / safety.md 未更新 → block(既存ルール3)' \
    'printf "\nfunc other() {}\n" >> backend/internal/safety/circuit.go'

case_run safety-internal-ok 0 'safety の内部変更 + safety.md → pass' \
    'printf "\nfunc other() {}\n" >> backend/internal/safety/circuit.go
     printf "updated\n" >> docs/architecture/layers/safety.md'

# H2: layers/*.md は契約だけを持つ(D9)。新規ファイルや exported の追加のたびに追記を
# 要求していたのが、layers docs が実装の写しに膨らんだ原因だった。
case_run newcmd-pass 0 '新規 usecase/command → pass(layers は要求しない)' \
    'printf "package command\n\nfunc NewCmd() {}\n" > backend/internal/usecase/command/newcmd.go'

case_run newadapter-pass 0 '新規 adapter サブディレクトリ → pass(layers は要求しない)' \
    'mkdir -p backend/internal/adapter/notifier
     printf "package notifier\n\nfunc NewSlack() {}\n" > backend/internal/adapter/notifier/slack.go'

case_run testonly 0 '*_test.go だけの変更 → pass' \
    'printf "package risk\n\nfunc TestX() {}\n" > backend/internal/domain/risk/gate_test.go'

case_run docsonly 0 'docs だけの変更 → pass' \
    'printf "updated\n" >> docs/architecture/layers/domain.md'

case_run nochange 0 '変更なし → pass' 'true'

case_run allowlisted 0 'allowlist にある変更 → pass' \
    'mkdir -p .claude/hooks
     printf "backend/internal/domain/position/.*\n" > .claude/hooks/spec-sync-allowlist.txt
     printf "\nfunc NewGate() int { return 1 }\n" >> backend/internal/domain/position/state.go'

c_disabled() {
    local disabled_repo disabled_rc=0
    disabled_repo="$(new_repo disabled)"
    ( cd "$disabled_repo" && printf '\nfunc NewGate() int { return 1 }\n' >> backend/internal/domain/position/state.go )
    printf '{"session_id":"selftest"}' \
        | ( cd "$disabled_repo" && STOCKBOT_DOCS_SYNC_CHECKS=off CLAUDE_PROJECT_DIR="$disabled_repo" \
            bash "$HOOK" >/dev/null 2>&1 ) || disabled_rc=$?
    check 'STOCKBOT_DOCS_SYNC_CHECKS=off → 常に pass' 0 "$disabled_rc"
}
h_defer c_disabled

# 未設定なら検査が走る(既定 on)。settings.json の env はもう既定を持たない(H8)ので、
# hook の `${…:-on}` が唯一の支え。ここが `:-off` に化けると検査が黙って全部止まる。
c_default_on() {
    local unset_repo unset_rc=0
    unset_repo="$(new_repo default-on)"
    ( cd "$unset_repo" && printf '\nfunc NewGate() int { return 1 }\n' >> backend/internal/domain/position/state.go )
    printf '{"session_id":"selftest"}' \
        | ( cd "$unset_repo" && env -u STOCKBOT_DOCS_SYNC_CHECKS CLAUDE_PROJECT_DIR="$unset_repo" \
            bash "$HOOK" >/dev/null 2>&1 ) || unset_rc=$?
    check 'STOCKBOT_DOCS_SYNC_CHECKS 未設定 → 検査が走る(block)' 2 "$unset_rc"
}
h_defer c_default_on

# A1: Tier 1(block)— 新規追加 / 削除 / exported 識別子に触れた変更。
# H2 で Tier 1 は契約の doc を持つ層(domain/position → STATE_MACHINE、safety → safety +
# FAILURE_MODES)だけに絞った。他の層の exported 変更は pass。
case_run domain-exported-pass 0 'domain(position 以外)に exported 追加 → pass' \
    'printf "\nfunc NewGate() int { return 1 }\n" >> backend/internal/domain/risk/gate.go'

case_run position-exported-block 2 'domain/position に exported 追加 / STATE_MACHINE 未更新 → block' \
    'printf "\nfunc NewGate() int { return 1 }\n" >> backend/internal/domain/position/state.go' \
    'STATE_MACHINE'

case_run position-method-block 2 'domain/position にメソッド形の exported 追加 → block' \
    'printf "\nfunc (g *Gate) Evaluate() int { return 1 }\n" >> backend/internal/domain/position/state.go' \
    'STATE_MACHINE'

case_run position-type-block 2 'domain/position に exported type 追加 → block' \
    'printf "\ntype Gate struct{}\n" >> backend/internal/domain/position/state.go' \
    'STATE_MACHINE'

case_run position-internal-pass 0 'domain/position の内部実装のみ変更 → pass' \
    'printf "\nfunc other() int { return 2 }\n" >> backend/internal/domain/position/state.go'

case_run position-newfile-block 2 'domain/position に新規ファイル(exported 無し) → block(新規追加は Tier 1)' \
    'printf "package position\n\nfunc other() {}\n" > backend/internal/domain/position/helper.go' \
    'STATE_MACHINE'

case_run position-delete-block 2 'domain/position のファイル削除 → block' \
    'rm backend/internal/domain/position/enums.go' \
    'STATE_MACHINE'

case_run position-domain-only-block 2 'domain/position の exported 変更 + domain.md のみ → block(STATE_MACHINE 必須)' \
    'printf "\nfunc NewState() int { return 1 }\n" >> backend/internal/domain/position/state.go
     printf "updated\n" >> docs/architecture/layers/domain.md' \
    'STATE_MACHINE'

case_run position-statemachine-ok 0 'domain/position の exported 変更 + STATE_MACHINE.md だけ → pass(domain.md は要求しない)' \
    'printf "\nfunc NewState() int { return 1 }\n" >> backend/internal/domain/position/state.go
     printf "updated\n" >> docs/runtime/STATE_MACHINE.md'

case_run safety-exported-partial-block 2 'safety の exported 変更 + safety.md のみ → block(FAILURE_MODES 必須)' \
    'printf "\nfunc NewCircuit() int { return 1 }\n" >> backend/internal/safety/circuit.go
     printf "updated\n" >> docs/architecture/layers/safety.md'

case_run safety-exported-both-ok 0 'safety の exported 変更 + safety.md + FAILURE_MODES.md → pass' \
    'printf "\nfunc NewCircuit() int { return 1 }\n" >> backend/internal/safety/circuit.go
     printf "updated\n" >> docs/architecture/layers/safety.md
     printf "updated\n" >> docs/architecture/FAILURE_MODES.md'

case_run handler-exported-pass 0 'handler の exported 変更 → pass(layers は要求しない)' \
    'printf "\nfunc NewHandler() int { return 1 }\n" >> backend/internal/app/handler/handler.go'

case_run query-exported-pass 0 'usecase/query の exported 変更 → pass(layers は要求しない)' \
    'printf "\nfunc NewView() int { return 1 }\n" >> backend/internal/usecase/query/view.go'

case_run repo-exported-pass 0 'adapter/repository の exported 変更 → pass(layers は要求しない)' \
    'printf "\nfunc NewPG() int { return 1 }\n" >> backend/internal/adapter/repository/pg.go'

case_run cmd-exported-pass 0 'backend/cmd の exported 変更 → pass(対象外)' \
    'printf "\nfunc NewWiring() int { return 1 }\n" >> backend/cmd/stockbot/main.go'

case_run committed-exported-block 2 'セッション最初の Stop: 直近コミットの exported 変更 → block' \
    'printf "\nfunc NewGate() int { return 1 }\n" >> backend/internal/domain/position/state.go
     git add -A && git commit -qm change'

# A1 レビュー指摘の回帰(F1 / F2 / G1 / G3 / T1 / T2)

# F1: 契約変更の主要形は 0 桁目に現れない(grouped const / struct フィールド / interface メソッド)
case_run grouped-const-block 2 'grouped const の enum 追加 + domain.md のみ → block(STATE_MACHINE 必須)' \
    'perl -0pi -e "s/\tStatusClosed Status = \"CLOSED\"\n/\tStatusClosed Status = \"CLOSED\"\n\tStatusPartial Status = \"PARTIAL\"\n/ or die" backend/internal/domain/position/state.go
     printf "updated\n" >> docs/architecture/layers/domain.md' \
    'STATE_MACHINE'

case_run grouped-const-ok 0 'grouped const の enum 追加 + domain.md + STATE_MACHINE.md → pass' \
    'perl -0pi -e "s/\tStatusClosed Status = \"CLOSED\"\n/\tStatusClosed Status = \"CLOSED\"\n\tStatusPartial Status = \"PARTIAL\"\n/ or die" backend/internal/domain/position/state.go
     printf "updated\n" >> docs/architecture/layers/domain.md
     printf "updated\n" >> docs/runtime/STATE_MACHINE.md'

case_run struct-field-block 2 'exported struct フィールド追加 → block' \
    'perl -0pi -e "s/\tName string\n/\tName string\n\tQty  int\n/ or die" backend/internal/domain/position/state.go' \
    'STATE_MACHINE'

case_run interface-method-block 2 'interface メソッド追加 → block' \
    'perl -0pi -e "s/\tPlace\(id int64\) error\n/\tPlace(id int64) error\n\tCancelAll(id int64) error\n/ or die" backend/internal/domain/position/contract.go' \
    'STATE_MACHINE'

# exported 関数の本体であること。unexported 関数だと ctx が `func [A-Z]` に当たらず、
# 「文脈が func ならインデント行を見ない」判定を壊しても緑のままになる。
case_run funcbody-call-pass 0 'exported 関数の本体に大文字始まりの行を追加 → pass(宣言ではない)' \
    'perl -0pi -e "s/\thelper\(1\)\n/\thelper(1)\n\tResult := Extra(1)\n/ or die" backend/internal/domain/position/state.go'

case_run iota-add-block 2 'iota 列挙の継続行(裸の識別子)を追加 → block' \
    'perl -0pi -e "s/\tPhaseDone\n/\tPhaseDone\n\tPhaseCancelled\n/ or die" backend/internal/domain/position/state.go' \
    'STATE_MACHINE'

case_run embedded-field-block 2 '埋め込みフィールド(裸の識別子)を追加 → block' \
    'perl -0pi -e "s/\tBase\n\tID/\tBase\n\tStatus\n\tID/ or die" backend/internal/domain/position/state.go' \
    'STATE_MACHINE'

# G3: コメント / 整形だけの修正で契約は動いていない
case_run comment-only-pass 0 '行末コメントの文言修正のみ → pass' \
    'sed -i.bak "s|// リトライ回数|// リトライ上限回数|" backend/internal/domain/position/contract.go && rm -f backend/internal/domain/position/contract.go.bak'

case_run gofmt-align-pass 0 'exported 宣言の空白整列だけの変更 → pass' \
    'sed -i.bak "s|const MaxRetry = 3|const MaxRetry   =  3|" backend/internal/domain/position/contract.go && rm -f backend/internal/domain/position/contract.go.bak'

# 行末コメント除去が文字列リテラルの中の // を食うと、URL の版数変更が見えなくなる
case_run url-in-string-block 2 '値の中に // を含む exported 変数の変更 → block' \
    'sed -i.bak "s|/v1/orders|/v2/orders|" backend/internal/domain/position/contract.go && rm -f backend/internal/domain/position/contract.go.bak' \
    'STATE_MACHINE'

# ctx が `var X = func(){` のときは本体なので宣言として数えない
case_run varfunc-body-pass 0 'exported var の関数本体の局所変更 → pass' \
    'perl -0pi -e "s/\tLocal := 1\n/\tLocal := 9\n/ or die" backend/internal/domain/position/contract.go'

# N2: 複数行シグネチャの引数変更は契約変更
case_run multiline-sig-block 2 '複数行シグネチャに引数を追加 → block' \
    'perl -0pi -e "s/\tb int,\n/\tb int,\n\tc int,\n/ or die" backend/internal/domain/position/contract.go' \
    'STATE_MACHINE'

case_run multiline-sig-body-pass 0 '同じ関数の本体だけの変更 → pass' \
    'perl -0pi -e "s/\treturn a \+ b\n/\tResult := a + b\n\treturn Result\n/ or die" backend/internal/domain/position/contract.go'

# 単一行シグネチャの関数本体にある生文字列 SQL(タブ1個)。ctx の判別子
# (複数行シグネチャだけが開き括弧で終わる)を緩めると、これが引数と誤認される。
case_run sql-singleline-pass 0 '単一行シグネチャの本体にある SQL の 1 語変更 → pass' \
    'perl -0pi -e "s/\tORDER BY started_at DESC\n/\tORDER BY started_at ASC\n/ or die" backend/internal/domain/position/pg.go'

# 複数行シグネチャの関数本体にある生文字列 SQL(タブ2個)。引数行は gofmt により
# 必ずタブ1個なので、タブ数で分離できる。
case_run sql-multiline-sig-pass 0 '複数行シグネチャの本体にある SQL の 1 語変更 → pass' \
    'perl -0pi -e "s/\t\tORDER BY opened_at DESC\n/\t\tORDER BY opened_at ASC\n/ or die" backend/internal/domain/position/pg.go'

# タブ + スペースのインデント。引数行はタブだけなので、スペース混じりは引数ではない。
case_run sql-tab-space-pass 0 'タブ+スペースでインデントされた SQL の 1 語変更 → pass' \
    'perl -0pi -e "s/\t ORDER BY id DESC\n/\t ORDER BY id ASC\n/ or die" backend/internal/domain/position/pg.go'

# F2: rename は git のリネーム検出で D にも A にも出ない。exported_touched 側で拾われない
# enums.go(grouped だけ)で測らないと分解経路を通らない。
case_run rename-block 2 'grouped だけのファイルの rename → block(新規追加/削除と同じ扱い)' \
    'git mv backend/internal/domain/position/enums.go backend/internal/domain/position/enums2.go
     git commit -qm rename' \
    'STATE_MACHINE'

# G1: 2 回目以降の Stop では直前コミットを見ない(= 無変更のターンで繰り返し block しない)
case_run prev-session-pass 0 '2 回目以降の Stop・当セッションは無変更 → pass(繰り返し block しない)' \
    'printf "\nfunc NewSizer() int { return 1 }\n" >> backend/internal/domain/position/state.go
     git add -A && git commit -qm "以前のコミット"
     mkdir -p runtime/logs && git rev-parse HEAD > runtime/logs/session-start-sha.selftest
     : > runtime/logs/docs-sync-seen.selftest'

# C1: settings.json の Stop 配列は pre-stop-checks.sh が先。そちらが session 開始 SHA を
# 先に作るので、SHA の有無で「最初の Stop か」を判定すると本番で必ず素通りする。
case_run prestop-sha-first 2 'pre-stop が先に session SHA を作っても、最初の docs-sync は直前コミットを見る' \
    'printf "\nfunc NewGate() int { return 1 }\n" >> backend/internal/domain/position/state.go
     git add -A && git commit -qm "セッション中のコミット"
     mkdir -p runtime/logs && git rev-parse HEAD > runtime/logs/session-start-sha.selftest' \
    'STATE_MACHINE'

# T1: session 範囲(HEAD~ より前に落ちた当セッションのコミット)を見ているか
case_run session-range-block 2 'session 中の 2 コミット目より前の exported 変更 → block' \
    'printf "\nfunc NewSizer() int { return 1 }\n" >> backend/internal/domain/position/state.go
     git add -A && git commit -qm "commit1: exported 追加"
     printf "note\n" >> docs/ARCHITECTURE.md && git add -A && git commit -qm "commit2: 無関係"' \
    'STATE_MACHINE' seed

# T2: port/repository.go 以外の port と adapter は契約の doc を持たない(H2)。
case_run port-broker-pass 0 'port/broker.go の exported 変更 → pass(port の契約 doc は repository.go だけ)' \
    'printf "\nfunc NewBrokerHelper() int { return 1 }\n" >> backend/internal/port/broker.go'

case_run adapter-nonrepo-pass 0 'adapter/tachibana の exported 変更 → pass' \
    'printf "\nfunc NewClient() int { return 1 }\n" >> backend/internal/adapter/tachibana/client.go'

# H2: Tier 2 の WARN は exit 0 の stderr で、モデルには届かない。出さない。
c_tier2_silent() {
    local warn_repo warn_rc
    warn_repo="$(new_repo tier2-silent)"
    ( cd "$warn_repo" && printf '\nfunc other() int { return 2 }\n' >> backend/internal/domain/position/state.go )
    warn_rc="$(run_hook "$warn_repo" "$TMPROOT/tier2-silent.err")"
    check 'Tier 2 は exit 0' 0 "$warn_rc"
    check 'Tier 2 は stderr に何も出さない' "empty" \
        "$([ -s "$TMPROOT/tier2-silent.err" ] && echo "non-empty: $(head -1 "$TMPROOT/tier2-silent.err")" || echo empty)"
}
h_defer c_tier2_silent

# session_id が取れない(jq 不在 / stdin に無い)ときは、ルール (7) の範囲を作業ツリーに
# 絞る。フォールバック ID は PPID を含むので、親が毎回変わると印が毎回別名になり
# 「常に最初の Stop」= 前のコミットで永久 block + 連続 3 回の逃げ道も死ぬ。
# 🛑 2 つの check は同じリポを順に使うので 1 ケースに畳んである(分けると並列で競合する)。
c_nosession() {
    local nosid_repo nosid_fail=0 rc nosid_wt_rc=0
    nosid_repo="$(new_repo nosession)"
    ( cd "$nosid_repo" \
      && printf '\nfunc NewGate() int { return 1 }\n' >> backend/internal/domain/position/state.go \
      && git add -A && git commit -qm "以前のコミット" ) >/dev/null 2>&1
    for _ in 1 2 3; do
        rc=0
        printf '{}' | ( cd "$nosid_repo" && STOCKBOT_DOCS_SYNC_CHECKS=on CLAUDE_PROJECT_DIR="$nosid_repo" \
            bash "$HOOK" >/dev/null 2>&1 ) || rc=$?
        [ "$rc" = "0" ] || nosid_fail=1
    done
    check 'session_id 無しで繰り返し呼んでも block されない(印が毎回別名)' "0" "$nosid_fail"

    # 同じ状況でも、作業ツリーの変更は session_id 無しでも見る(範囲を絞っただけで無効化しない)
    ( cd "$nosid_repo" && printf '\nfunc NewOther() int { return 1 }\n' >> backend/internal/domain/position/state.go )
    printf '{}' | ( cd "$nosid_repo" && STOCKBOT_DOCS_SYNC_CHECKS=on CLAUDE_PROJECT_DIR="$nosid_repo" \
        bash "$HOOK" >/dev/null 2>&1 ) || nosid_wt_rc=$?
    check 'session_id 無しでも作業ツリーの exported 変更は block' 2 "$nosid_wt_rc"
}
h_defer c_nosession

# T12: 無条件の `HEAD~..HEAD` を外す(セッション開始 SHA が SessionStart 由来なら不要)
seed_session_sha() { # $1 = repo; SessionStart(H1)が置いた 2 つの印を模す
    mkdir -p "$1/runtime/logs"
    git -C "$1" rev-parse HEAD > "$1/runtime/logs/session-start-sha.selftest"
    # 🛑 印は**空ファイルではなく SHA を持つ**。空にすると、Stop 配列 2 番目の
    # docs-sync-check.sh が pre-stop の backfill を開始点と誤読する(T12 2 巡目レビュー)。
    git -C "$1" rev-parse HEAD > "$1/runtime/logs/session-start-mark.selftest"
}

# 🛑 期待した経路(BLOCKED メッセージ)で止まっていることまで見る。**1 回しか流さない** —
# 2 回目は session_first_stop=0 になって範囲が変わるので、再実行では判別できない。
# 🛑 stderr の受け皿はケースごとに別名にする。共有名にすると並列実行で他のケースの
# 出力を読んでしまう(RF3 #8)。
check_block() { # $1 = 説明, $2 = repo
    local err="$TMPROOT/blk.${H_CASE_ID}.err" rc
    rc="$(run_hook "$2" "$err")"
    if [ "$rc" = "2" ] && grep -q "BLOCKED:" "$err" 2>/dev/null; then
        check "$1" "ok" "ok"
    else
        check "$1" "ok" "rc=$rc stderr=[$(cat "$err" 2>/dev/null | head -1)]"
    fi
}

# 作業ツリーが clean なら、次のセッションは直前コミット(spec 変更・docs 未同期)を見てはいけない。
c_t12a() {
    local t12_repo t12_rc_noseed
    t12_repo="$(new_repo t12a)"
    printf '\nfunc ExportedFromPrevSession() int { return 1 }\n' \
        >> "$t12_repo/backend/internal/domain/position/state.go"
    git -C "$t12_repo" add -A
    git -C "$t12_repo" -c user.email=t@t -c user.name=t commit -qm "prev session commit (docs 未同期)"
    t12_rc_noseed=$(run_hook "$t12_repo" /dev/null)
    check 'T12: 印が無ければ従来どおり直前コミットを見る' 2 "$t12_rc_noseed"
}
h_defer c_t12a

c_t12b() {
    local t12_repo2
    t12_repo2="$(new_repo t12b)"
    printf '\nfunc ExportedFromPrevSession() int { return 1 }\n' \
        >> "$t12_repo2/backend/internal/domain/position/state.go"
    git -C "$t12_repo2" add -A
    git -C "$t12_repo2" -c user.email=t@t -c user.name=t commit -qm "prev session commit (docs 未同期)"
    seed_session_sha "$t12_repo2"
    check 'T12: 印があれば無変更ターンで直前コミットを見ない' 0 "$(run_hook "$t12_repo2" /dev/null)"
}
h_defer c_t12b

# 印があっても、セッション中のコミットは当然見る(範囲を絞っただけで無効化しない)。
c_t12c() {
    local t12_repo3
    t12_repo3="$(new_repo t12c)"
    seed_session_sha "$t12_repo3"
    printf '\nfunc ExportedThisSession() int { return 1 }\n' \
        >> "$t12_repo3/backend/internal/domain/position/state.go"
    git -C "$t12_repo3" add -A
    git -C "$t12_repo3" -c user.email=t@t -c user.name=t commit -qm "this session commit (docs 未同期)"
    check_block 'T12: 印があってもセッション中のコミットは見る' "$t12_repo3"
}
h_defer c_t12c

# 印が解決できない SHA なら従来どおりに倒す(縮めると fail-OPEN)。
c_t12d() {
    local t12_repo4
    t12_repo4="$(new_repo t12d)"
    printf '\nfunc ExportedFromPrevSession() int { return 1 }\n' \
        >> "$t12_repo4/backend/internal/domain/position/state.go"
    git -C "$t12_repo4" add -A
    git -C "$t12_repo4" -c user.email=t@t -c user.name=t commit -qm "prev session commit (docs 未同期)"
    mkdir -p "$t12_repo4/runtime/logs"
    echo "0000000000000000000000000000000000000000" > "$t12_repo4/runtime/logs/session-start-sha.selftest"
    check_block 'T12: 印が解決できなければ従来どおり' "$t12_repo4"
}
h_defer c_t12d

# changed_files を絞るだけでは足りない — ルール (7) の tier_revs も最初の Stop で
# HEAD~..HEAD を足すので、そこを直さないと「当セッションが別のファイルを触っていれば
# 前セッションのコミットで block される」形が残る。
c_t12e() {
    local t12_repo5
    t12_repo5="$(new_repo t12e)"
    printf '\nfunc ExportedFromPrevSession() int { return 1 }\n' \
        >> "$t12_repo5/backend/internal/domain/position/state.go"
    git -C "$t12_repo5" add -A
    git -C "$t12_repo5" -c user.email=t@t -c user.name=t commit -qm "prev session commit (docs 未同期)"
    seed_session_sha "$t12_repo5"
    printf 'note\n' >> "$t12_repo5/docs/ARCHITECTURE.md"
    check 'T12: tier_revs にも直前コミットを入れない' 0 "$(run_hook "$t12_repo5" /dev/null)"
}
h_defer c_t12e

# 同じ状況で印が無ければ従来どおり block する(検査ごと消したのではないこと)。
c_t12f() {
    local t12_repo6
    t12_repo6="$(new_repo t12f)"
    printf '\nfunc ExportedFromPrevSession() int { return 1 }\n' \
        >> "$t12_repo6/backend/internal/domain/position/state.go"
    git -C "$t12_repo6" add -A
    git -C "$t12_repo6" -c user.email=t@t -c user.name=t commit -qm "prev session commit (docs 未同期)"
    printf 'note\n' >> "$t12_repo6/docs/ARCHITECTURE.md"
    check_block 'T12: 印が無ければ tier_revs は従来どおり' "$t12_repo6"
}
h_defer c_t12f

# 🛑 `changed_files` は「違反を見つける側」だけでなく「**要求を満たす側**」(doc_changed が
# 見る窓)でもある。ここを絞らないと、**前セッションが更新した doc で今セッションの要求が
# 満たされる** = 誤 pass になる(レビューの生存変異 M4)。tier 経由のケースでは踏めない。
c_t12g() {
    local t12_repo7
    t12_repo7="$(new_repo t12g)"
    printf '\nprev session note\n' >> "$t12_repo7/docs/runtime/STATE_MACHINE.md"
    git -C "$t12_repo7" add -A
    git -C "$t12_repo7" -c user.email=t@t -c user.name=t commit -qm "prev session: STATE_MACHINE.md だけ更新"
    seed_session_sha "$t12_repo7"
    printf '\nfunc ExportedThisSession() int { return 1 }\n' \
        >> "$t12_repo7/backend/internal/domain/position/state.go"
    check_block 'T12: 前セッションの doc 更新で今セッションの要求を満たさない' "$t12_repo7"
}
h_defer c_t12g

# mark はあるが sha が解決できない → 従来どおり(`session_range_ok` を条件から落とすと
# 素通りする。レビューの生存変異 M11)。
c_t12h() {
    local t12_repo8
    t12_repo8="$(new_repo t12h)"
    printf '\nfunc ExportedFromPrevSession() int { return 1 }\n' \
        >> "$t12_repo8/backend/internal/domain/position/state.go"
    git -C "$t12_repo8" add -A
    git -C "$t12_repo8" -c user.email=t@t -c user.name=t commit -qm "prev session commit (docs 未同期)"
    mkdir -p "$t12_repo8/runtime/logs"
    echo "0000000000000000000000000000000000000000" > "$t12_repo8/runtime/logs/session-start-mark.selftest"
    check_block 'T12: 印の SHA が解決できなければ従来どおり' "$t12_repo8"
}
h_defer c_t12h

# mark はあるが sha ファイルが無い → backfill が今の HEAD を書いて範囲が空になる形。
c_t12i() {
    local t12_repo9
    t12_repo9="$(new_repo t12i)"
    printf '\nfunc ExportedFromPrevSession() int { return 1 }\n' \
        >> "$t12_repo9/backend/internal/domain/position/state.go"
    git -C "$t12_repo9" add -A
    git -C "$t12_repo9" -c user.email=t@t -c user.name=t commit -qm "prev session commit (docs 未同期)"
    mkdir -p "$t12_repo9/runtime/logs"; : > "$t12_repo9/runtime/logs/session-start-mark.selftest"
    check_block 'T12: 印が空なら従来どおり' "$t12_repo9"
}
h_defer c_t12i

# 🛑 本番の Stop 配列は pre-stop-checks.sh が先で `session-start-sha` を backfill する。
# 印の**中身**ではなくファイルの有無で判定していると、この hook はその backfill を
# セッション開始点と誤読する(2 巡目レビュー実測: pre-stop rc=2 / docs-sync rc=0 と割れた)。
c_t12j() {
    local t12_repo10
    t12_repo10="$(new_repo t12j)"
    printf '\nfunc ExportedFromPrevSession() int { return 1 }\n' \
        >> "$t12_repo10/backend/internal/domain/position/state.go"
    git -C "$t12_repo10" add -A
    git -C "$t12_repo10" -c user.email=t@t -c user.name=t commit -qm "prev session commit (docs 未同期)"
    mkdir -p "$t12_repo10/runtime/logs"
    git -C "$t12_repo10" rev-parse HEAD > "$t12_repo10/runtime/logs/session-start-sha.selftest"
    check_block 'T12: 先行 hook の backfill を開始点と誤読しない' "$t12_repo10"
}
h_defer c_t12j

# RF3 #1: 非 ASCII ファイル名(git の C-quote)で検査が素通りしないこと。
# git は既定(`core.quotePath=true`)で非 ASCII のパスを
# `"backend/internal/domain/\346\226\260…"` と C-quote して出す。この形は
# `^backend/.*\.go$` に当たらないので、**変更が 1 件も無いように見えて hook が素通りする**。
# pre-stop-checks.sh は 2026-08 の時点で `-c core.quotePath=false` を付けていたが、
# 同じ収集ブロックを持つこちらには 1 つも入っておらず fail-OPEN だった(RF2 実測)。
# 🛑 収集経路は 4 本あり、1 本ずつ独立に拘束する。1 本だけ直すと他が残る。
# 各ケースに **ASCII の対照**を置く — 対照が落ちたらハーネス側の壊れなので切り分けられる。
NONASCII_GO='backend/internal/domain/position/新規規則.go'

# 経路A: untracked(`git ls-files --others`)
c_q1a_ctl() {
    local q_a_ctl; q_a_ctl="$(new_repo q1a_ctl)"
    printf 'package risk\n\nfunc ExportedUntracked() int { return 1 }\n' \
        > "$q_a_ctl/backend/internal/domain/position/newrule.go"
    check_block '非ASCII対照: untracked の ASCII 新規 .go は block' "$q_a_ctl"
}
h_defer c_q1a_ctl

c_q1a() {
    local q_a; q_a="$(new_repo q1a)"
    printf 'package risk\n\nfunc ExportedUntracked() int { return 1 }\n' > "$q_a/$NONASCII_GO"
    check_block '#1 経路A: untracked の非ASCII 新規 .go も block(quotePath)' "$q_a"
}
h_defer c_q1a

# 経路B: 作業ツリーの追跡済み変更(`git diff --name-only HEAD`)
c_q1b_ctl() {
    local q_b_ctl; q_b_ctl="$(new_repo q1b_ctl)"
    printf 'package risk\n\nfunc Placeholder() int { return 0 }\n' \
        > "$q_b_ctl/backend/internal/domain/position/tracked.go"
    git -C "$q_b_ctl" add -A
    git -C "$q_b_ctl" -c user.email=t@t -c user.name=t commit -qm "seed tracked ascii"
    printf '\nfunc ExportedInWorktree() int { return 1 }\n' \
        >> "$q_b_ctl/backend/internal/domain/position/tracked.go"
    seed_session_sha "$q_b_ctl"
    check_block '非ASCII対照: 作業ツリーの ASCII 変更は block' "$q_b_ctl"
}
h_defer c_q1b_ctl

c_q1b() {
    local q_b; q_b="$(new_repo q1b)"
    printf 'package risk\n\nfunc Placeholder() int { return 0 }\n' > "$q_b/$NONASCII_GO"
    git -C "$q_b" add -A
    git -C "$q_b" -c user.email=t@t -c user.name=t commit -qm "seed tracked non-ascii"
    printf '\nfunc ExportedInWorktree() int { return 1 }\n' >> "$q_b/$NONASCII_GO"
    seed_session_sha "$q_b"
    check_block '#1 経路B: 作業ツリーの非ASCII 変更も block(quotePath)' "$q_b"
}
h_defer c_q1b

# 経路C: セッション中のコミット(`<開始SHA>..HEAD`)
c_q1c() {
    local q_c; q_c="$(new_repo q1c)"
    printf 'package risk\n\nfunc Placeholder() int { return 0 }\n' > "$q_c/$NONASCII_GO"
    git -C "$q_c" add -A
    git -C "$q_c" -c user.email=t@t -c user.name=t commit -qm "seed non-ascii"
    seed_session_sha "$q_c"   # ここをセッション開始点にする
    printf '\nfunc ExportedThisSession() int { return 1 }\n' >> "$q_c/$NONASCII_GO"
    git -C "$q_c" add -A
    git -C "$q_c" -c user.email=t@t -c user.name=t commit -qm "this session commit (docs 未同期)"
    check_block '#1 経路C: セッション中コミットの非ASCII 変更も block(quotePath)' "$q_c"
}
h_defer c_q1c

# 経路D: 直前コミット(`HEAD~..HEAD`。印が無いときの従来経路)
c_q1d() {
    local q_d; q_d="$(new_repo q1d)"
    printf 'package risk\n\nfunc Placeholder() int { return 0 }\n' > "$q_d/$NONASCII_GO"
    git -C "$q_d" add -A
    git -C "$q_d" -c user.email=t@t -c user.name=t commit -qm "seed non-ascii"
    printf '\nfunc ExportedFromPrevSession() int { return 1 }\n' >> "$q_d/$NONASCII_GO"
    git -C "$q_d" add -A
    git -C "$q_d" -c user.email=t@t -c user.name=t commit -qm "prev session commit (docs 未同期)"
    check_block '#1 経路D: 直前コミットの非ASCII 変更も block(quotePath)' "$q_d"
}
h_defer c_q1d

# 🛑 上の A〜D は**ルール (7)(`tier_*` 経由)しか拘束していない**(変異テストで実測:
# `changed_files` 側だけ quotePath を外しても 4 件とも緑のまま)。`changed_files` は
# early exit と既存ルール 1〜6 を駆動する**別の消費側**なので、独立に縛る必要がある。
# ルール (3)(`safety/*.go` → `safety.md`)を **unexported な変更**で撃つと
# ルール (7) は発火しないので、`changed_files` だけを拘束できる。
c_q1e_ctl() {
    local q_e_ctl; q_e_ctl="$(new_repo q1e_ctl)"
    printf 'package safety\n\nfunc seed() int { return 0 }\n' \
        > "$q_e_ctl/backend/internal/safety/circuit2.go"
    git -C "$q_e_ctl" add -A
    git -C "$q_e_ctl" -c user.email=t@t -c user.name=t commit -qm "seed ascii safety file"
    printf '\nfunc other() {}\n' >> "$q_e_ctl/backend/internal/safety/circuit2.go"
    seed_session_sha "$q_e_ctl"
    check_block '非ASCII対照: ルール(3) は ASCII の unexported 変更で block' "$q_e_ctl"
}
h_defer c_q1e_ctl

c_q1e() {
    local q_e; q_e="$(new_repo q1e)"
    printf 'package safety\n\nfunc seed() int { return 0 }\n' \
        > "$q_e/backend/internal/safety/回路.go"
    git -C "$q_e" add -A
    git -C "$q_e" -c user.email=t@t -c user.name=t commit -qm "seed non-ascii safety file"
    printf '\nfunc other() {}\n' >> "$q_e/backend/internal/safety/回路.go"
    seed_session_sha "$q_e"
    check_block '#1 changed_files: ルール(3) は非ASCII の unexported 変更でも block' "$q_e"
}
h_defer c_q1e


# RF3 #3: jq が使えなくても session_id を grep で復元すること。
# pre-stop-checks.sh:54 は grep フォールバックを持つのに、この hook には無かった。
# 無いと `nosession-<PPID>` へ縮退し、(a) `session_known=0` になってルール (7) の
# tier_revs が `HEAD` だけに縮む(検出範囲が作業ツリーだけ = fail-OPEN 方向)、
# (b) 2 本の hook が別々の `session-start-sha.*` を書き、T12 が依存する
# 「sha と mark の対」が 1 セッション内で割れる。
# 本機に jq はあるが、`session-start_test.sh:150` と同じくサポート対象なので固定する。
q3_stub="$TMPROOT/stubbin"; mkdir -p "$q3_stub"
printf '#!/bin/sh\nexit 1\n' > "$q3_stub/jq"; chmod +x "$q3_stub/jq"

c_q3() {
    local q3_repo; q3_repo="$(new_repo q3)"
    printf '\nfunc ExportedForSid() int { return 1 }\n' \
        >> "$q3_repo/backend/internal/domain/position/state.go"
    printf '{"session_id":"selftest"}' \
        | ( cd "$q3_repo" && PATH="$q3_stub:$PATH" STOCKBOT_DOCS_SYNC_CHECKS=on \
            CLAUDE_PROJECT_DIR="$q3_repo" bash "$HOOK" >/dev/null 2>&1 ) || true
    check '#3 jq が壊れていても session_id を復元する' "ok" \
        "$([ -f "$q3_repo/runtime/logs/docs-sync-seen.selftest" ] && echo ok \
           || echo "作られた印: $(ls "$q3_repo/runtime/logs" 2>/dev/null | tr '\n' ' ')")"
}
h_defer c_q3

# RF3 #5: stop-hook-lib.sh の fail-CLOSED 3 段。
# 実リポの lib は壊せないので、hook を使い捨てリポの中へ**コピー**して動かす
# (hook は補助 lib を自分の隣から解決するので、コピー先の中身だけで挙動が決まる)。
# 🛑 exit code だけでは拘束できない。この hook は docs drift でも 2 になるので、
# 変更を一切入れないリポ(= 本来 exit 0)を使い、**メッセージで**見る。
STOPLIB="$HOOKDIR/stop-hook-lib.sh"

stoplib_repo() { # $1 = case id -> repo を作り hook + lib を中へコピーして repo を echo
    local repo; repo="$(new_repo "$1")"
    mkdir -p "$repo/.claude/hooks"
    cp "$HOOK" "$repo/.claude/hooks/"
    cp "$STOPLIB" "$repo/.claude/hooks/" 2>/dev/null || true
    echo "$repo"
}
stoplib_err() { # $1 = repo -> コピーした hook を流して stderr を echo
    # 🛑 `$$` はサブシェルでも親の PID なので、並列実行では受け皿が衝突する。
    # ケース id で分ける(RF3 #8)。
    local repo="$1" ef="$TMPROOT/stoplib.err.${H_CASE_ID}"
    printf '{"session_id":"selftest"}' \
        | ( cd "$repo" && STOCKBOT_DOCS_SYNC_CHECKS=on CLAUDE_PROJECT_DIR="$repo" \
            bash "$repo/.claude/hooks/docs-sync-check.sh" >/dev/null 2>"$ef" ) || true
    cat "$ef"
}
stoplib_rc() { # $1 = repo -> コピーした hook の exit code を echo
    local repo="$1" rc=0
    printf '{"session_id":"selftest"}' \
        | ( cd "$repo" && STOCKBOT_DOCS_SYNC_CHECKS=on CLAUDE_PROJECT_DIR="$repo" \
            bash "$repo/.claude/hooks/docs-sync-check.sh" >/dev/null 2>&1 ) || rc=$?
    echo "$rc"
}

c_stoplib_missing() {
    local s_repo; s_repo="$(stoplib_repo stoplib_missing)"
    rm -f "$s_repo/.claude/hooks/stop-hook-lib.sh"
    expect_msg "#5 stop-hook-lib 欠落は fail-CLOSED" \
        "stop-hook-lib.sh が見つからない" "$(stoplib_err "$s_repo")"
    check "#5 stop-hook-lib 欠落は exit 2" "2" "$(stoplib_rc "$s_repo")"
}
h_defer c_stoplib_missing

c_stoplib_empty() {
    local s_repo; s_repo="$(stoplib_repo stoplib_empty)"
    : > "$s_repo/.claude/hooks/stop-hook-lib.sh"
    expect_msg "#5 stop-hook-lib が空でも fail-CLOSED" \
        "stop-hook-lib.sh に hooklib_" "$(stoplib_err "$s_repo")"
}
h_defer c_stoplib_empty

c_stoplib_false() {
    local s_repo; s_repo="$(stoplib_repo stoplib_false)"
    { cat "$STOPLIB" 2>/dev/null; printf '\nfalse\n'; } > "$s_repo/.claude/hooks/stop-hook-lib.sh"
    expect_msg "#5 stop-hook-lib 内のコマンド失敗で block(exit 1 で素通りしない)" \
        "stop-hook-lib.sh の読み込みに失敗" "$(stoplib_err "$s_repo")"
    check "#5 stop-hook-lib の読み込み失敗は exit 2" "2" "$(stoplib_rc "$s_repo")"
}
h_defer c_stoplib_false

c_stoplib_syntax() {
    local s_repo; s_repo="$(stoplib_repo stoplib_syntax)"
    { cat "$STOPLIB" 2>/dev/null; printf '\nif [ ; then\n'; } > "$s_repo/.claude/hooks/stop-hook-lib.sh"
    expect_msg "#5 stop-hook-lib が構文エラーなら黙って通さない" \
        "syntax error" "$(stoplib_err "$s_repo")"
}
h_defer c_stoplib_syntax

# 対照: lib が正常なら、同じリポ・同じ流し方で通る(上の 4 件が「何をしても 2」でないこと)。
c_stoplib_ok() {
    local s_repo; s_repo="$(stoplib_repo stoplib_ok)"
    check "#5 対照: 正常な stop-hook-lib なら通る" "0" "$(stoplib_rc "$s_repo")"
}
h_defer c_stoplib_ok

# 🛑 SessionStart の印の形検証: 印が SHA の形かを見ないと、壊れた印が「SessionStart が
# 動いた」と読まれて session_start_sha がゴミになり、**セッション範囲の commit が 1 つも見えなくなる**
# (fail-OPEN)。T12 がこの検証を 2 本の hook に入れたとき**どちらにもテストが無く**、
# RF3 #5 の変異テストで初めて survive として発覚した。
# 違反コミットを HEAD~ より前に置くのがこのテストの肝 — HEAD~..HEAD で拾えてしまうと
# 「セッション範囲が生きているか」を測れない。
sha_shape_case() { # $1 = case id, $2 = 印の中身("" なら印を作らない) -> exit code を echo
    local repo; repo="$(new_repo "$1")"
    mkdir -p "$repo/runtime/logs"
    git -C "$repo" rev-parse HEAD > "$repo/runtime/logs/session-start-sha.selftest"
    [ -n "$2" ] && printf '%s' "$2" > "$repo/runtime/logs/session-start-mark.selftest"
    : > "$repo/runtime/logs/docs-sync-seen.selftest"
    printf '\nfunc AddedInSession() int { return 2 }\n' >> "$repo/backend/internal/safety/circuit.go"
    git -C "$repo" add -A >/dev/null 2>&1; git -C "$repo" commit -qm "safety change (no doc)" >/dev/null 2>&1
    printf 'noise\n' >> "$repo/docs/ARCHITECTURE.md"
    git -C "$repo" add -A >/dev/null 2>&1; git -C "$repo" commit -qm "harmless" >/dev/null 2>&1
    run_hook "$repo" "$TMPROOT/$1.err"
}
c_sha_garbage() {
    check "#5 壊れた SessionStart 印でもセッション範囲が生きている" "2" \
        "$(sha_shape_case sha_garbage 'zzzz-not-a-sha')"
}
c_sha_nomark() {
    check "#5 対照: 印が無いとき(backfill 経路)も同じ" "2" "$(sha_shape_case sha_nomark '')"
}
c_sha_valid() {
    check "#5 対照: 正しい印なら従来どおり" "2" \
        "$(sha_shape_case sha_valid "$(git -C "$TPL" rev-parse HEAD)")"
}
h_defer c_sha_garbage
h_defer c_sha_nomark
h_defer c_sha_valid

h_run_deferred
h_tally docs-sync-check_test
