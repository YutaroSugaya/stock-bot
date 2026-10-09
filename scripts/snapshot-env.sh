#!/usr/bin/env bash
# repo の .env から launchd ジョブ用の env snapshot を作る。
#
# なぜ必要か: launchd から起動された /bin/bash は macOS TCC で Desktop 配下を**読めない**
# (`./.env: Operation not permitted`)。朝ジョブ・週次ジョブは repo の .env を
# source できないので、repo 非依存で動くための値を人間のシェルから写しておく。
#
# 秘密鍵は **~/.stockbot/ 側へ複製**し、snapshot の _PRIVATE_KEY_FILE はその複製を指す
# (repo = Desktop 配下への依存を 1 つ減らすため)。発注系の env は**写さない**
# (このスナップショットを読むジョブは参照系だけ)。出力は 0600 / ディレクトリは 0700。

set -euo pipefail

REPO="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
DEST_DIR="$HOME/.stockbot"
DEST="$DEST_DIR/env"
KEY_DEST="$DEST_DIR/e_api_private_key.pem"

if [ ! -r "$REPO/.env" ]; then
  echo "snapshot-env: $REPO/.env を読めないので snapshot は据え置き" >&2
  exit 0
fi

umask 077
mkdir -p "$DEST_DIR"

tmp="$DEST.tmp"
(
  set -a
  # shellcheck disable=SC1090
  . "$REPO/.env"
  set +a

  : > "$tmp"
  # 🛑 STOCKBOT_TACHIBANA_SECOND_PASSWORD は**写さない**。第二暗証は発注・取消にだけ要る秘密で、
  # この snapshot を読む launchd ジョブ(fetch-daily / universe-screen / forward-report)は全部参照専用
  # (fetch-daily は requireSecondPW=false)。launchd 権限で読める場所へ要らない秘密を置かない。
  for k in STOCKBOT_DATABASE_URL STOCKBOT_TACHIBANA_ENV STOCKBOT_TACHIBANA_AUTH_ID \
           STOCKBOT_BENCH_SYMBOL; do
    v="$(eval "printf '%s' \"\${$k:-}\"")"
    [ -n "$v" ] && printf '%s=%s\n' "$k" "$v" >> "$tmp"
  done

  src="${STOCKBOT_TACHIBANA_PRIVATE_KEY_FILE:-}"
  if [ -n "$src" ] && [ -r "$src" ]; then
    cp "$src" "$KEY_DEST"
    chmod 600 "$KEY_DEST"
    printf 'STOCKBOT_TACHIBANA_PRIVATE_KEY_FILE=%s\n' "$KEY_DEST" >> "$tmp"
  fi
)
mv "$tmp" "$DEST"
chmod 600 "$DEST"
echo "snapshot-env: $DEST を更新($(wc -l < "$DEST" | tr -d ' ') keys)"
