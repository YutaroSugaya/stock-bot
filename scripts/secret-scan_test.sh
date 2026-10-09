#!/usr/bin/env bash
# secret-scan.sh と pre-stop-checks.sh Step 3 の共有パターン(.claude/hooks/secret-patterns.sh)の
# 自己テスト。使い捨ての git リポジトリを作り、両方の消費側に実際にサンプルを流す。
#
# 🛑 このファイルにサンプルの綴りをそのまま書いてはいけない。
#   - `secret-scan.sh` は tracked file を git grep するので、このファイル自身が引っかかる
#     (excludes に `:!scripts/secret-scan_test.sh` を入れてあるが、それに頼らない)
#   - `pre-stop-checks.sh` の added-diff / untracked スキャンには**除外リストが無い**ので、
#     このファイルをコミットしようとした瞬間に自分の Stop が block される
# そこで**全サンプルを実行時に文字列連結で組み立てる**。各サンプルは「連結前の綴りが
# パターンに一致しない」ように切ってある(切り方の根拠は各行のコメント)。
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCAN="$ROOT/scripts/secret-scan.sh"
PRECOMMIT="$ROOT/scripts/pre-commit.sh"          # 3 つ目の消費側(staged 追加行を grep)
PRESTOP="$ROOT/.claude/hooks/pre-stop-checks.sh"
PATLIB="$ROOT/.claude/hooks/secret-patterns.sh"
STOPLIB="$ROOT/.claude/hooks/stop-hook-lib.sh"   # pre-stop が source する共有 lib
ENFLIB="$ROOT/.claude/hooks/enforcement-paths.sh" # 同上
TMPROOT="$(mktemp -d)"
trap 'rm -rf "$TMPROOT"' EXIT

pass=0
fail=0
ok()  { pass=$((pass + 1)); }
bad() { fail=$((fail + 1)); echo "FAIL: $1"; }

# ---------------------------------------------------------------------------
# サンプル(連結で組み立てる)
# ---------------------------------------------------------------------------
# 名前 / 値 の 2 列。値は「1 行に書いたときに秘密として検出されるべき」文字列。
mk_samples() {
    local a b
    SAMPLE_NAMES=(); SAMPLE_VALUES=()
    add() { SAMPLE_NAMES+=("$1"); SAMPLE_VALUES+=("$2"); }

    # AWS: 'AKIA' の直後は `;` になるので連結前は不一致
    a='AKIA'; b='ZZZZZZZZZZZZZZZZ';            add "aws-access-key"   "$a$b"
    # GitHub: 30 文字(secret-scan 側は {36,} だったので**取りこぼしていた**長さ)
    a='ghp_'; b='abcdefghij0123456789abcdefghij'; add "github-ghp-30" "$a$b"
    # GitHub: gho_ 36 文字(pre-stop 側は ghp_ しか見ておらず**取りこぼしていた**接頭辞)
    a='gho_'; b='abcdefghij0123456789abcdefghij012345'; add "github-gho-36" "$a$b"
    # Slack
    a='xoxb-'; b='0123456789abcdef';           add "slack-token"     "$a$b"
    # Anthropic: 連結前は `03-` の後ろが行末なので不一致
    a='sk-ant-api03-'; b='0123456789abcdefghij0123456789abcdefghij'; add "anthropic-key" "$a$b"
    # OpenAI 形式
    a='sk-'; b='0123456789abcdef0123456789abcdef';  add "openai-key"  "$a$b"
    # PEM: 連結前は `PRIVATE ` の後ろが `'` なので不一致
    a='-----BEGIN RSA PRIVATE '; b='KEY-----'; add "pem-block"       "$a$b"
    # DER の base64(立花の e_api_private_key.der は PEM ヘッダを持たない)
    # MII の後ろに base64 が 60 文字以上必要(パターンが {60,})。短いと**サンプル側の不備**で
    # 落ちるので、意図的に余裕を持たせる。
    a='MII'; b='EvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQDabcdefghijklmnopqrstuvwxyz0123456789'
    add "der-base64" "$a$b"
    # 立花のセッション仮想 URL(パスがトークン相当)。**版ごとに検体を持つ** —
    # v4r9 廃止(2026-09-27)後も旧版の貼り付けを拾い続け、かつ新版 v4r10 の
    # パスが素通りしないこと(検体が旧版だけだと、版を上げた日に穴が開く)。
    a='https://demo-kabuka.e-shiten.jp/e_api_v4r9/'; b='ABCDEFGH12345678'
    add "tachibana-session-url" "$a$b"
    a='https://demo-kabuka.e-shiten.jp/e_api_v4r10/'; b='ABCDEFGH12345678'
    add "tachibana-session-url-v4r10" "$a$b"
    # 認証レスポンスの URL フィールド
    a='"sUrlRequest"'; b=': "https://demo-kabuka.e-shiten.jp/x"'
    add "sUrlRequest-json" "$a$b"
    # 取引口座の資格情報(実値つき)
    a='TACHIBANA_PASS'; b='WORD=realpw1'
    add "tachibana-password" "$a$b"
    # 秘密鍵のパス(pre-stop 側だけが持っていた。secret-scan は**取りこぼしていた**)
    a='STOCKBOT_TACHIBANA_PRIVATE_'; b='KEY=/Users/x/secrets/e_api_private_key.der'
    add "tachibana-private-key-path" "$a$b"
}

# 検出されては**いけない**もの(誤検知の回帰テスト)
mk_benign() {
    local a b
    BENIGN_NAMES=(); BENIGN_VALUES=()
    addb() { BENIGN_NAMES+=("$1"); BENIGN_VALUES+=("$2"); }

    a='TACHIBANA_PASS'; b='WORD=********';     addb "masked-password"  "$a$b"
    a='SECOND_PASS';    b='WORD=<第二PW>';     addb "placeholder-pw"   "$a$b"
    a='sUrl';           b='Request は認証レスポンスのセッション仮想 URL を返す'
    addb "prose-mention" "$a$b"
    a='AKIA';           b=' で始まるのが AWS のアクセスキー ID'
    addb "prose-aws"    "$a$b"
    a='https://demo-kabuka.e-shiten.jp/'; addb "bare-host" "$a"
    # 認証専用 URL(版を含むが**セッションではない**)。起動ログと docs に出るので
    # 誤検出すると make secret-scan が恒久的に赤くなる。
    a='https://kabuka.e-shiten.jp/e_api_v4r10/'; b='auth/'
    addb "auth-base-url" "$a$b"
}

mk_samples
mk_benign

# ---------------------------------------------------------------------------
# 使い捨てリポジトリ
# ---------------------------------------------------------------------------
new_repo() { # $1 = id -> path を echo
    local r="$TMPROOT/$1"
    mkdir -p "$r/scripts" "$r/.claude/hooks" "$r/docs" "$r/runtime/logs"
    cp "$SCAN" "$PRECOMMIT" "$r/scripts/"
    # 🛑 pre-stop-checks.sh が source する lib は**全部**持ち込む。忘れると fail-CLOSED 側の
    # ガードが一斉に発火し、Step 3 まで到達せず全件が赤くなる。
    cp "$PATLIB" "$STOPLIB" "$ENFLIB" "$PRESTOP" "$r/.claude/hooks/" 2>/dev/null || true
    printf '/runtime/\n' > "$r/.gitignore"
    printf 'placeholder\n' > "$r/docs/notes.md"
    ( cd "$r" && git init -q . \
        && git add -A \
        && git -c user.email=t@t -c user.name=t commit -qm init ) >/dev/null 2>&1
    echo "$r"
}

run_scan() { # $1 = repo -> exit code
    local rc=0
    ( cd "$1" && bash "$1/scripts/secret-scan.sh" >/dev/null 2>&1 ) || rc=$?
    echo "$rc"
}

run_precommit() { # $1 = repo -> exit code(staged 追加行で止まるか)
    local rc=0
    ( cd "$1" && bash "$1/scripts/pre-commit.sh" >/dev/null 2>&1 ) || rc=$?
    echo "$rc"
}

run_prestop() { # $1 = repo -> exit code(Step 3 の secret scan で止まるか)
    local rc=0
    printf '{"session_id":"sstest"}' \
        | ( cd "$1" && env STOCKBOT_PRESTOP_CHECKS=on STOCKBOT_TDD_CHECK=off \
            CLAUDE_PROJECT_DIR="$1" bash "$1/.claude/hooks/pre-stop-checks.sh" \
            >/dev/null 2>"$TMPROOT/prestop.err" ) || rc=$?
    echo "$rc"
}

run_prestop_unset() { # run_prestop と同じだが STOCKBOT_PRESTOP_CHECKS を渡さない
    local rc=0
    printf '{"session_id":"sstest"}' \
        | ( cd "$1" && env -u STOCKBOT_PRESTOP_CHECKS STOCKBOT_TDD_CHECK=off \
            CLAUDE_PROJECT_DIR="$1" bash "$1/.claude/hooks/pre-stop-checks.sh" \
            >/dev/null 2>"$TMPROOT/prestop.err" ) || rc=$?
    echo "$rc"
}

# ---------------------------------------------------------------------------
# 1) secret-scan.sh(tracked file を git grep)
# ---------------------------------------------------------------------------
i=0
while [ "$i" -lt "${#SAMPLE_NAMES[@]}" ]; do
    name="${SAMPLE_NAMES[$i]}"; val="${SAMPLE_VALUES[$i]}"
    r="$(new_repo "scan-$i")"
    printf '%s\n' "$val" >> "$r/docs/notes.md"
    ( cd "$r" && git add -A && git -c user.email=t@t -c user.name=t commit -qm add ) >/dev/null 2>&1
    rc="$(run_scan "$r")"
    if [ "$rc" = "1" ]; then ok; else bad "secret-scan が $name を検出しない (rc=$rc)"; fi
    i=$((i + 1))
done

# 誤検知の回帰
i=0
while [ "$i" -lt "${#BENIGN_NAMES[@]}" ]; do
    name="${BENIGN_NAMES[$i]}"; val="${BENIGN_VALUES[$i]}"
    r="$(new_repo "scanb-$i")"
    printf '%s\n' "$val" >> "$r/docs/notes.md"
    ( cd "$r" && git add -A && git -c user.email=t@t -c user.name=t commit -qm add ) >/dev/null 2>&1
    rc="$(run_scan "$r")"
    if [ "$rc" = "0" ]; then ok; else bad "secret-scan が $name を誤検出 (rc=$rc)"; fi
    i=$((i + 1))
done

# ---------------------------------------------------------------------------
# 2) pre-stop-checks.sh Step 3(untracked file を grep)
# ---------------------------------------------------------------------------
# 🛑 ここが「配列の drift」を潰す本体。同じサンプルが**両方の消費側**で検出されること。
i=0
while [ "$i" -lt "${#SAMPLE_NAMES[@]}" ]; do
    name="${SAMPLE_NAMES[$i]}"; val="${SAMPLE_VALUES[$i]}"
    r="$(new_repo "ps-$i")"
    printf '%s\n' "$val" > "$r/docs/leak.md"     # untracked のまま
    rc="$(run_prestop "$r")"
    if [ "$rc" = "2" ] && grep -q "SECRET SCAN FAILED" "$TMPROOT/prestop.err" 2>/dev/null; then
        ok
    else
        bad "pre-stop Step 3 が $name を検出しない (rc=$rc)"
    fi
    i=$((i + 1))
done

# STOCKBOT_PRESTOP_CHECKS が未設定でも pre-stop は走る(既定 on)。settings.json の env は
# もう既定を持たないので、hook の `${…:-on}` が唯一の支え。1 サンプルで足りる。
r="$(new_repo "ps-unset")"
printf '%s\n' "${SAMPLE_VALUES[0]}" > "$r/docs/leak.md"
rc="$(run_prestop_unset "$r")"
if [ "$rc" = "2" ] && grep -q "SECRET SCAN FAILED" "$TMPROOT/prestop.err" 2>/dev/null; then
    ok
else
    bad "STOCKBOT_PRESTOP_CHECKS 未設定で pre-stop が走らない (rc=$rc)"
fi

# ---------------------------------------------------------------------------
# 3) pre-commit.sh(staged の追加行を grep)
# ---------------------------------------------------------------------------
# 🛑 3 つ目の消費側。ここが独自配列を持っていたせいで **commit 時のゲートが push 時より
# 弱い**状態が続いていた(gh[ousr]_ / OpenAI 形式 / 立花セッション仮想 URL / sUrlRequest が
# 素通り)。pre-commit は secret がローカル履歴に入る前に止まる唯一の層なので、
# 同じサンプルが **3 つ全部**で検出されることを拘束する。
i=0
while [ "$i" -lt "${#SAMPLE_NAMES[@]}" ]; do
    name="${SAMPLE_NAMES[$i]}"; val="${SAMPLE_VALUES[$i]}"
    r="$(new_repo "pc-$i")"
    printf '%s\n' "$val" >> "$r/docs/notes.md"
    ( cd "$r" && git add -A ) >/dev/null 2>&1        # staged のまま commit しない
    rc="$(run_precommit "$r")"
    if [ "$rc" = "1" ]; then ok; else bad "pre-commit が $name を検出しない (rc=$rc)"; fi
    i=$((i + 1))
done

# 🛑 パイプバッファ(64KB)超えの staged diff で fail-open しないこと。`echo … | grep -q` は
# 最初の一致で grep が抜け、上流が SIGPIPE(141)で死んで pipefail が条件を偽にするため、
# **秘密があるのに素通り**する。小さいサンプルでは 1 件も再現しないので明示的に大きくする。
r="$(new_repo "pc-bigdiff")"
{
    printf '%s\n' "${SAMPLE_VALUES[0]}"    # 先頭に秘密(grep はここで抜ける)
    i=0
    while [ "$i" -lt 4000 ]; do
        printf 'harmless padding line %d aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n' "$i"
        i=$((i + 1))
    done
} >> "$r/docs/notes.md"
( cd "$r" && git add -A ) >/dev/null 2>&1
rc="$(run_precommit "$r")"
if [ "$rc" = "1" ]; then ok; else bad "pre-commit が 64KB 超の staged diff で fail-open (rc=$rc)"; fi

# 誤検知の回帰(commit を恒久的に止めるのは CI が赤いより痛い)
i=0
while [ "$i" -lt "${#BENIGN_NAMES[@]}" ]; do
    name="${BENIGN_NAMES[$i]}"; val="${BENIGN_VALUES[$i]}"
    r="$(new_repo "pcb-$i")"
    printf '%s\n' "$val" >> "$r/docs/notes.md"
    ( cd "$r" && git add -A ) >/dev/null 2>&1
    rc="$(run_precommit "$r")"
    if [ "$rc" = "0" ]; then ok; else bad "pre-commit が $name を誤検出 (rc=$rc)"; fi
    i=$((i + 1))
done

# パターン定義が読めないときは fail-CLOSED(commit を通さない)。ここが fail-open だと
# `: > secret-patterns.sh` の 1 手で最初の層が無効化できる。
r="$(new_repo "pc-failclosed")"
rm -f "$r/.claude/hooks/secret-patterns.sh"
printf '%s\n' "${SAMPLE_VALUES[0]}" >> "$r/docs/notes.md"
( cd "$r" && git add -A ) >/dev/null 2>&1
rc="$(run_precommit "$r")"
if [ "$rc" != "0" ]; then ok; else bad "pre-commit がパターン定義の欠落を fail-open している (rc=$rc)"; fi

# ---------------------------------------------------------------------------
# 4) 配列そのものの健全性
# ---------------------------------------------------------------------------
if [ -f "$PATLIB" ]; then
    # shellcheck source=/dev/null
    . "$PATLIB"
    if [ "${#SECRET_PATTERNS[@]}" -ge 10 ]; then ok; else bad "SECRET_PATTERNS が少なすぎる (${#SECRET_PATTERNS[@]})"; fi
    # 各パターンが ERE として妥当か(壊れた regex は「常に非マッチ = fail-OPEN」になる)
    for p in "${SECRET_PATTERNS[@]}"; do
        grc=0
        printf 'x\n' | grep -E -q -e "$p" >/dev/null 2>&1 || grc=$?
        # 0 = 一致 / 1 = 不一致 のどちらも「regex として妥当」。2 以上は regex が壊れている
        # (壊れた regex は git grep / grep が失敗するだけで、握り潰されると fail-OPEN になる)。
        if [ "$grc" -le 1 ]; then ok; else bad "SECRET_PATTERNS に不正な ERE: $p"; fi
    done
    # 🛑 3 つの消費側が**同じ配列を読んでいる**こと(綴りではなく構造で見る)。
    if grep -q 'secret-patterns\.sh' "$PRESTOP" && grep -q 'secret-patterns\.sh' "$SCAN" \
        && grep -q 'secret-patterns\.sh' "$PRECOMMIT"; then
        ok
    else
        bad "pre-stop-checks.sh / secret-scan.sh / pre-commit.sh が共有パターンを読んでいない"
    fi
    # 🛑 どの消費側も**独自の配列を再導入していない**こと。正本への集約から pre-commit が
    # 漏れていた再発を、綴りではなく「正本の展開以外で patterns 配列を宣言していないか」で
    # 止める。`patterns=("${SECRET_PATTERNS[@]}")` は正本の写しなので許す。
    for consumer in "$PRECOMMIT" "$SCAN" "$PRESTOP"; do
        if grep -iE '^[[:space:]]*[a-z_]*patterns=\(' "$consumer" | grep -qvF 'SECRET_PATTERNS[@]'; then
            bad "$(basename "$consumer") が独自のパターン配列を持っている(正本から drift する)"
        else
            ok
        fi
    done
else
    bad ".claude/hooks/secret-patterns.sh が無い"
fi

echo "secret-scan_test: pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
