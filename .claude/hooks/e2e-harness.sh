#!/usr/bin/env bash
# Stop 系 hook の e2e 自己テストが共有するハーネス(RF3 #8)。source して使う。
#
# 目的は速度。`make guard` は自ら定めた閾値 30 秒に対して **78 秒**かかっており、
# 内訳は `cp -r`(1 件 0.019s)ではなく **hook 実行が支配的**(1 件 0.276s・hook 1 回で
# git プロセス 27 個)だった。ホットスポットは無く平坦なので、**ケース単位の並列化**以外に
# 落とす手が無い(重い 2 本を別レイヤへ逃がす案は却下 — CI は 2026-08-10 に廃止され、
# `.githooks/pre-push` が回す hook 自己テストは pretooluse-deny_test.sh の 1 本だけ。
# Stop 系の e2e をそこへ移すと壊れた hook を push してから気づくことになるし、
# そもそも Stop hook は壊れると自分自身の検査をすり抜ける)。
#
# 🛑 集計を親シェルの変数(`pass`/`fail`)でやると、ケースをサブプロセスにした瞬間に
# **加算が全部消えて「pass=0 fail=0」の嘘の緑**になる。ケースごとに結果ファイルへ書き、
# 最後に親が集計する形にしてある。
#
# 🛑 並列化はケース境界でだけ行う。1 ケースの中で hook を 2〜3 回流すケース
# (warn-once の消化 / block カウンタ)があり、その順序は意味を持つ。
#
# 🛑 bash 3.2(macOS の /bin/bash)の語彙に限定する。`wait -n` も `$BASHPID` も無いので、
# 空きスロットは `jobs -rp` のポーリングで待ち、ケースの識別子は `$$` ではなく
# **呼び出し側が渡す id**($H_CASE_ID)を使う(サブシェルの `$$` は親の PID のまま)。

h_init() { # $1 = TMPROOT
    H_TMPROOT="$1"
    H_RESULTS="$1/.results"
    mkdir -p "$H_RESULTS"
    H_CASE_ID="main"
    H_NJOBS=0
    H_JOB_CMD=()
    H_ORDER=""
    # hook 1 回が git を 27 プロセス起こすので、コア数そのままだと詰まる。
    H_PAR="${STOCKBOT_TEST_JOBS:-0}"
    if [ "$H_PAR" -le 0 ]; then
        H_PAR="$(sysctl -n hw.ncpu 2>/dev/null || nproc 2>/dev/null || echo 4)"
        H_PAR=$((H_PAR - 2))
    fi
    [ "$H_PAR" -lt 1 ] && H_PAR=1
    [ "$H_PAR" -gt 8 ] && H_PAR=8
}

check() { # $1 = 説明, $2 = 期待, $3 = 実際
    if [ "$2" = "$3" ]; then
        printf 'PASS\n' >> "$H_RESULTS/$H_CASE_ID"
    else
        printf 'FAIL: %s (expected [%s], got [%s])\n' "$1" "$2" "$3" >> "$H_RESULTS/$H_CASE_ID"
    fi
}

expect_msg() { # $1 = 説明, $2 = 期待する語, $3 = 実際の stderr
    case "$3" in
        *"$2"*) printf 'PASS\n' >> "$H_RESULTS/$H_CASE_ID" ;;
        *) printf 'FAIL: %s (期待 [%s] が無い): %s\n' "$1" "$2" \
               "$(printf '%s' "$3" | head -2 | tr '\n' ' ')" >> "$H_RESULTS/$H_CASE_ID" ;;
    esac
}

h_fail() { # $1 = メッセージ(check/expect_msg に載らない失敗)
    printf 'FAIL: %s\n' "$1" >> "$H_RESULTS/$H_CASE_ID"
}

# ケースを登録する(この時点では実行しない)。引数は %q でクォートして持つので、
# 空白を含む setup 文字列をそのまま渡してよい。
h_defer() { # $@ = 実行するコマンドと引数
    H_NJOBS=$((H_NJOBS + 1))
    H_JOB_CMD[$H_NJOBS]="$(printf '%q ' "$@")"
    H_ORDER="$H_ORDER $H_NJOBS"
}

# 🛑 空きスロットの数え方に外部コマンドを使わない。`jobs -rp | wc -l | tr -d ' '` は
# ポーリング 1 回につき 2 プロセス fork するので、ケースが短い(0.3 秒)このワークロードでは
# スケジューラ自身が無視できないコストになる。bash の for で数えれば fork ゼロ。
h_run_deferred() {
    local i j running
    for i in $H_ORDER; do
        while :; do
            running=0
            for j in $(jobs -rp); do running=$((running + 1)); done
            [ "$running" -lt "$H_PAR" ] && break
            sleep 0.02
        done
        ( H_CASE_ID="job$i"; eval "${H_JOB_CMD[$i]}" ) &
    done
    wait
}

# 集計して 1 行で報告する。失敗があれば内訳を出して非ゼロで返す。
# 🛑 出力順はケースの完了順(非決定)なので、失敗メッセージは id を含めること。
#
# 🛑 **結果を 1 件も残さなかったケースを failure として数える。**サブプロセスにした
# 途端「ケースが構文エラー / unbound variable で死んでも、書き込まれた PASS が減るだけで
# 全体は緑」という嘘の緑が成立する(親シェル集計だった頃には無かった経路)。
# 件数を人間が目視で比べる運用に頼らない。
h_tally() { # $1 = ラベル
    local p f i missing=0
    for i in $H_ORDER; do
        if [ ! -s "$H_RESULTS/job$i" ]; then
            missing=$((missing + 1))
            printf 'FAIL: job%s (%s) が結果を 1 件も残していない — 途中で死んだ\n' \
                "$i" "${H_JOB_CMD[$i]}" >> "$H_RESULTS/job$i"
        fi
    done
    p="$(cat "$H_RESULTS"/* 2>/dev/null | grep -c '^PASS' || true)"
    f="$(cat "$H_RESULTS"/* 2>/dev/null | grep -c '^FAIL' || true)"
    if [ "$f" -gt 0 ]; then
        cat "$H_RESULTS"/* 2>/dev/null | grep '^FAIL' || true
    fi
    echo "$1: pass=$p fail=$f"
    [ "$f" -eq 0 ]
}
