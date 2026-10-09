#!/usr/bin/env bash
# secret-patterns.sh — secret スキャンのパターン定義(唯一の正本)。
#
# 読み手は 3 つ:
#   - scripts/pre-commit.sh           … staged の追加行を grep(履歴に入る前に止める最初の層)
#   - scripts/secret-scan.sh          … tracked file を git grep(pre-push の最終ゲート)
#   - .claude/hooks/pre-stop-checks.sh … added diff + untracked file を grep(Stop hook の早いフィードバック)
#
# 🛑 **副作用を持たない(配列定義のみ)。**source する側が fail-CLOSED で守る。
# 🛑 **bash 3.2 の語彙に限定する**(連想配列 / nameref / ${var^^} は使えない。macOS 実機は 3.2)。
#
# ── なぜ 1 か所に寄せたか(RF2 / RF3 #4) ─────────────────────────────────────
# 以前は同じ役割の配列が 2 か所にあり、**双方向に取りこぼしていた**(自己テストで実測)。
# なお RF3 #4 の集約からは pre-commit が漏れており、独自の 10 パターンのまま残って
# **commit 時のゲートが push 時より弱い**状態が続いていた(2026-08-12 に合流)。
#   secret-scan だけが持っていた … 立花セッション仮想 URL / sUrlRequest / DER の base64 /
#                                   マスク耐性のある資格情報パターン
#   pre-stop だけが持っていた   … ghp_ の 30〜35 文字 / STOCKBOT_TACHIBANA_PRIVATE_KEY
# 重いのは前者で、`secret-scan.sh` のコメントが「wire 裏取り中に実レスポンスを docs へ貼ると
# 当日中は口座照会に使える」と書いている、**実際の事故(2026-08-06 監査)を受けて足した
# パターンが Stop hook 側に無かった**。「片方に足して片方に忘れる」が既に 1 回起きている。
#
# ── このファイルを編集するときの注意 ────────────────────────────────────────
# 🛑 **変数名・キー名は文字列連結で割る。**`secret-scan.sh` の git grep は
# `:!.claude/hooks/*` でこのファイルを除外するが、**`pre-stop-checks.sh` の added-diff /
# untracked スキャンには除外リストが無い**ので、割っていないとこのファイルを編集した
# 瞬間に自分のパターンで自分の Stop が block される(実測済み)。
# 割る位置は「連結前の綴りがパターンに一致しない」ところを選ぶこと。
#
# 🛑 **パターンを緩めるときは実リポで空振りを確認する。**`bash scripts/secret-scan.sh` が
# clean のままであること(誤検知を入れると `make secret-scan` と pre-push が恒久的に赤くなる)。

SECRET_PATTERNS=(
  # --- 汎用のクラウド / SaaS 資格情報 -------------------------------------
  'sk-ant-api[0-9]{2}-[A-Za-z0-9_-]{20,}'   # Anthropic API key
  'sk-[A-Za-z0-9]{32,}'                     # OpenAI 形式
  'AKIA[0-9A-Z]{16}'                        # AWS access key id
  '-----BEGIN[A-Z ]*PRIVATE KEY-----'       # PEM 秘密鍵ブロック
  'xox[baprs]-[A-Za-z0-9-]{10,}'            # Slack token
  # GitHub token。接頭辞は 5 種(p/o/u/s/r)あり、長さは 30 以上で見る。
  # 🛑 統合前は secret-scan が `gh[pousr]_…{36,}`、pre-stop が `ghp_…{30,}` で、
  # **どちらも相手が拾う形を取りこぼしていた**。和集合が正しい。
  'gh[pousr]_[A-Za-z0-9]{30,}'

  # --- 立花証券 e支店 ------------------------------------------------------
  # DER 鍵の base64。secrets/e_api_private_key.der は PEM ヘッダを持たないので
  # 上の BEGIN パターンでは捕まらない。ASN.1 DER は MII で始まる。
  'MII[A-Za-z0-9+/]{60,}'
  # セッション仮想 URL。**パスそのものがセッショントークン**なので、wire 裏取り中に
  # 実レスポンスを docs へ貼ると当日中は口座照会に使える。docs を走査対象に含めた目的がこれ
  # (プレースホルダは <...> なので不一致)。
  'https?://[a-z0-9.-]*e-shiten\.jp/[A-Za-z0-9_/.-]*/[A-Za-z0-9]{16,}'
  '"(sUrlRequest|sUrlMaster|sUrlPrice|sUrlEvent)"[[:space:]]*:[[:space:]]*"https?://'

  # --- 取引口座の資格情報 --------------------------------------------------
  # 実値が付いているものだけ。値のどこかに `*` 以外の文字を要求するので、docs の
  # マスク表記(=********)は素通しし、`*` で始まる実パスワード(=*bcdef1)は捕まえる
  # (プレースホルダ `<第二PW>` は `<` が字種外なので元から不一致)。
  '(STOCKBOT_TACHIBANA_SECOND_PASSWORD|STOCKBOT_TACHIBANA_AUTH_ID|TACHIBANA_PASSWORD|SECOND_PASSWORD|API_PASSWORD)[[:space:]]*[:=][[:space:]]*["'"'"']?\**[A-Za-z0-9!@#$%^&_-][A-Za-z0-9!@#$%^&*_-]{5,}'
  # 秘密鍵ファイルの実パス。鍵そのものではないが、置き場所を書き残さない規律。
  # 🛑 変数名を割ってある(上の注意書きを参照)。
  'STOCKBOT_TACHIBANA_PRIVATE_''KEY=[^[:space:]<*]'
)
