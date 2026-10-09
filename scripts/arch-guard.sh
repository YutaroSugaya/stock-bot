#!/usr/bin/env bash
# arch-guard.sh — 層規約の grep ガード(SoT)。CLAUDE.md §層規約 / docs/architecture/layers/ を
# 機械的に enforce する唯一の正本。`make guard` / .claude/hooks/pre-stop-checks.sh /
# .githooks/pre-push の 3 者がこのスクリプトを呼ぶ(grep をベタ書きで重複させない)。
#
# 依存方向の本体は `go build ./...`(import cycle 検出)が強制する。本スクリプトは build では
# 落ちない「import はできるが規約違反」(domain 純粋性 / R1 / handler→repo 等)を補完する。
#
# 違反時は該当 docs/architecture/layers/*.md を stderr に示して exit 1。違反ゼロで exit 0。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

BE="backend/internal"
MOD="stockbot/backend/internal"
violations=0

report() {
  echo "ARCH-GUARD 違反: $1" >&2
  echo "  該当ファイル:" >&2
  echo "$2" | sed 's/^/    /' >&2
  echo "  参照: $3" >&2
  echo "" >&2
  violations=$((violations + 1))
}

# (a) domain 純粋性: I/O フレームワーク等の import 禁止(_test.go は除外)。
v=$(grep -rEln '"(github\.com/jackc/pgx[^"]*|net/http|log/slog|os|os/[a-z/]+|math/rand|crypto/rand)"' \
      "$BE/domain/" --include='*.go' 2>/dev/null | grep -v '_test\.go' || true)
[ -n "$v" ] && report "domain が pgx/net/http/log/slog/os/rand を import している" "$v" \
  "docs/architecture/layers/domain.md(domain は純粋。I/O は usecase→port→adapter 経由)"

# (b) domain での time.Now() 直呼び。clock.System() (domain/clock/clock.go) のみ正当。
#     コメント行(行頭 //)は除外して誤検出を防ぐ。
v=$(grep -rEn 'time\.Now\(' "$BE/domain/" --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' \
      | grep -v 'domain/clock/clock\.go' \
      | grep -vE '^[^:]+:[0-9]+:[[:space:]]*//' \
      | cut -d: -f1 | sort -u || true)
[ -n "$v" ] && report "domain が time.Now() を直呼び(clock.Clock を注入せよ)" "$v" \
  "docs/architecture/layers/domain.md(time.Now 禁止 / clock 注入)"

# (c) domain / port が上位層(adapter/app/usecase)を import している。
v=$(grep -rEln "\"$MOD/(adapter|app|usecase)" "$BE/domain/" "$BE/port/" --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' || true)
[ -n "$v" ] && report "domain/port が上位層(adapter/app/usecase)を import している" "$v" \
  "docs/architecture/layers/domain.md, docs/architecture/layers/port.md(依存は下向きのみ)"

# (d) port が config を import している(R1 ガード)。
v=$(grep -rln "\"$MOD/config\"" "$BE/port/" --include='*.go' 2>/dev/null | grep -v '_test\.go' || true)
[ -n "$v" ] && report "port が config を import している(R1 違反。Mode 等は string で渡す)" "$v" \
  "docs/architecture/layers/port.md(§R1: port は config 非依存)"

# (e) usecase 本番コードが具体 adapter を import している(test の配線は許容)。
v=$(grep -rln "\"$MOD/adapter" "$BE/usecase/" --include='*.go' 2>/dev/null | grep -v '_test\.go' || true)
[ -n "$v" ] && report "usecase 本番が adapter を直 import している(port 経由にせよ)" "$v" \
  "docs/architecture/layers/usecase.md(usecase は port interface 経由のみ)"

# (f) handler が repository adapter を直 import している。
v=$(grep -rln "\"$MOD/adapter/repository" "$BE/app/handler/" --include='*.go' 2>/dev/null || true)
[ -n "$v" ] && report "handler が repository adapter を直 import している" "$v" \
  "docs/architecture/layers/handler.md(handler は parse→usecase→encode の薄い層)"

# (g) usecase 本番コードの I/O import 禁止(usecase.md「やらないこと」)。
#     ログが要るなら local interface(Info(string, ...any) 等)を切って注入する。
v=$(grep -rEln '"(github\.com/jackc/pgx[^"]*|net/http|log/slog|os|os/[a-z/]+|math/rand|crypto/rand)"' \
      "$BE/usecase/" --include='*.go' 2>/dev/null | grep -v '_test\.go' || true)
[ -n "$v" ] && report "usecase が pgx/net/http/log/slog/os/rand を import している" "$v" \
  "docs/architecture/layers/usecase.md(usecase は純粋なオーケストレーション。I/O は port 経由)"

# (h) CQRS 分離: command と query の相互 import 禁止。
v=$( { grep -rln "\"$MOD/usecase/query\"" "$BE/usecase/command/" --include='*.go' 2>/dev/null
      grep -rln "\"$MOD/usecase/command\"" "$BE/usecase/query/" --include='*.go' 2>/dev/null
    } | grep -v '_test\.go' || true)
[ -n "$v" ] && report "usecase/command と usecase/query が相互 import している(CQRS 分離違反)" "$v" \
  "docs/architecture/layers/usecase.md(command=状態変更・Tx / query=read-only・View DTO)"

# (i) adapter が config / usecase / app を import している(依存は下向きのみ)。
v=$(grep -rEln "\"$MOD/(config\"|usecase|app)" "$BE/adapter/" --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' || true)
[ -n "$v" ] && report "adapter が config/usecase/app を import している" "$v" \
  "docs/architecture/layers/adapter.md(adapter は port と domain だけを見る)"

# (j) handler が config を import している(mode 等は usecase/query の DTO で受ける)。
v=$(grep -rln "\"$MOD/config\"" "$BE/app/handler/" --include='*.go' 2>/dev/null | grep -v '_test\.go' || true)
[ -n "$v" ] && report "handler が config を import している" "$v" \
  "docs/architecture/layers/handler.md(handler は parse→usecase→encode の薄い層)"

# (k) domain の mutex は market の rolling window 2 ファイルのみ(例外条項の封じ込め)。
v=$(grep -rln '"sync"' "$BE/domain/" --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' \
      | grep -vE 'domain/market/(candle|aggregator)\.go$' || true)
[ -n "$v" ] && report "domain で sync(mutex)を使う新ファイルが増えた(例外は market/candle.go + market/aggregator.go のみ)" "$v" \
  "docs/architecture/layers/domain.md(例外は domain/market の rolling window に限定)"

# (l) domain → config import は risk/gate.go と strategy/ のみ(許容違反の封じ込め)。
v=$(grep -rln "\"$MOD/config\"" "$BE/domain/" --include='*.go' 2>/dev/null \
      | grep -v '_test\.go' \
      | grep -vE 'domain/(risk/gate\.go$|strategy/)' || true)
[ -n "$v" ] && report "domain → config の import が許容範囲(risk/gate.go + strategy/)の外に増えた" "$v" \
  "docs/architecture/layers/domain.md(domain→config は封じ込め済みの許容違反)"

if [ "$violations" -gt 0 ]; then
  echo "arch-guard: ${violations} 件の層規約違反。該当 docs を読んで import を直すこと。" >&2
  exit 1
fi

echo "arch-guard: 層規約 OK(違反ゼロ)"
