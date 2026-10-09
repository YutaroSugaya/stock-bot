#!/usr/bin/env bash
# pretooluse-deny_test.sh — deny hook の自己テスト。
set -uo pipefail
# 承認セッション(STOCKBOT_HARNESS_EDIT_APPROVED=1)から pre-push / make guard を回すと、その env が
# 継承されて enforcement の検査が丸ごと飛び、deny を期待するケースが赤くなる(逆に hook を壊しても
# 気づけない)。逃げ道を試すケースはケースの中で明示的に渡す。
unset STOCKBOT_HARNESS_EDIT_APPROVED
HOOK="$(cd "$(dirname "$0")" && pwd)/pretooluse-deny.sh"
pass=0; fail=0

run() {
  local expected="$1" desc="$2" json="$3" rc
  printf '%s' "$json" | bash "$HOOK" >/dev/null 2>&1
  rc=$?
  if [ "$rc" -eq "$expected" ]; then
    pass=$((pass + 1))
  else
    fail=$((fail + 1)); echo "FAIL [$desc] expected exit $expected got $rc"
  fi
}

bj() { printf '{"tool_name":"Bash","tool_input":{"command":"%s"}}' "$1"; }

# エスケープの流儀が2つある。既存ケースの \\" は JSON としては壊れており、hook 内の
# jq が失敗して sed フォールバック経路で判定される(結果は同じなので放置)。
# 新規ケースは単一バックスラッシュ(\n / \")で書く = 有効な JSON になり、実運用と同じ
# jq 経路を通る。実改行を hook に届かせるには \\n では駄目で \n でなければならない
# (2026-08-07 probe 実測: \\n は jq がリテラルのバックスラッシュ+n を返す)。

run 2 "go test -tags integration"   "$(bj 'go test -tags integration ./...')"
run 2 "go test -tags=integration"   "$(bj 'cd backend && go test -tags=integration ./...')"
run 2 "docker compose down"         "$(bj 'docker compose down')"
run 2 "docker compose stop pg"      "$(bj 'docker compose stop postgres')"
run 2 "docker-compose rm -f"        "$(bj 'docker-compose rm -f')"
run 2 "rm -rf docker-data"          "$(bj 'rm -rf .docker-data/postgres')"
run 2 "DROP SCHEMA"                 "$(bj 'psql -c \\"DROP SCHEMA public CASCADE\\"')"
run 2 "DROP DATABASE"              "$(bj 'psql -c \\"DROP DATABASE stockbot\\"')"
run 2 "TRUNCATE trades"            "$(bj 'psql -c \\"TRUNCATE TABLE trades\\"')"
run 2 "pkill postgres"             "$(bj 'pkill -f postgres')"
run 2 "brew services stop pg"      "$(bj 'brew services stop postgresql')"
run 2 "psql INSERT (live)"         "$(bj 'psql stockbot -c \\"INSERT INTO trades VALUES (1)\\"')"
run 2 "psql UPDATE (live)"         "$(bj 'psql -d stockbot -c \\"UPDATE positions SET status=1\\"')"
run 2 "pkill cmd/stockbot"         "$(bj 'pkill -f cmd/stockbot')"
run 2 "killall stockbot"           "$(bj 'killall stockbot')"
run 2 "docker exec psql DELETE"    "$(bj 'docker exec sb-pg psql -U stockbot -d stockbot -c \\"DELETE FROM trades\\"')"

# 2026-07-31 拡張: 監査で見つかったパターン穴(dropdb / pg_ctl 変種 / docker 直 /
# psql write 動詞 / pg_restore / _test escape の緩さ)。
run 2 "dropdb"                     "$(bj 'dropdb stockbot')"
run 2 "DROP TABLE"                 "$(bj 'psql -c \\"DROP TABLE trades\\"')"
run 2 "pg_ctl -D dir stop"         "$(bj 'pg_ctl -D /usr/local/var/postgres stop')"
run 2 "pg_ctl restart"             "$(bj 'pg_ctl restart -D /opt/pg')"
run 2 "killall postgres"           "$(bj 'killall postgres')"
run 2 "brew services restart pg"   "$(bj 'brew services restart postgresql@16')"
run 2 "docker stop container"      "$(bj 'docker stop sb-pg')"
run 2 "docker kill container"      "$(bj 'docker kill sb-pg')"
run 2 "docker rm container"        "$(bj 'docker rm -f sb-pg')"
run 2 "psql ALTER (live)"          "$(bj 'psql stockbot -c \\"ALTER TABLE trades ADD COLUMN x int\\"')"
run 2 "psql CREATE (live)"         "$(bj 'psql stockbot -c \\"CREATE TABLE tmp (id int)\\"')"
run 2 "psql GRANT (live)"          "$(bj 'psql stockbot -c \\"GRANT ALL ON trades TO evil\\"')"
run 2 "psql COPY FROM (live)"      "$(bj 'psql stockbot -c \\"COPY trades FROM STDIN\\"')"
run 2 "psql -f (live)"             "$(bj 'psql stockbot -f patch.sql')"
run 2 "pg_restore (live)"          "$(bj 'pg_restore -d stockbot dump.bin')"
run 2 "pg_restore --clean (approved)" "$(bj 'STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 pg_restore --clean -d stockbot dump.bin')"
run 2 "_test elsewhere no bypass"  "$(bj 'cp x_test.go /tmp && psql stockbot -c \\"UPDATE positions SET x=1\\"')"
run 2 "kill pgrep stockbot"        "$(bj 'kill $(pgrep -f stockbot)')"
run 2 "docker container stop"      "$(bj 'docker container stop sb-pg')"
run 2 "docker container rm"        "$(bj 'docker container rm -f sb-pg')"
run 2 "docker volume rm"           "$(bj 'docker volume rm stockbot_pgdata')"
run 2 "docker system prune"        "$(bj 'docker system prune -af --volumes')"
run 2 "docker compose kill"        "$(bj 'docker compose kill postgres')"
run 2 "psql -f connected"          "$(bj 'psql stockbot -fpatch.sql')"
run 2 "psql < file.sql"            "$(bj 'psql stockbot < patch.sql')"
run 2 "pipe into psql"             "$(bj 'cat patch.sql | psql stockbot')"
run 2 "pgrep pipe xargs kill"      "$(bj 'pgrep -f stockbot | xargs kill')"
run 2 "pgrep postgres xargs kill"  "$(bj 'pgrep -f postgres | xargs kill -9')"

# T1(2026-08-07): 改行バイパス。早期素通しの判定は grep = 行単位なので、先頭行が
# read 動詞なら後続行が何でも exit 0 になっていた。
run 2 "newline: ls then rm docker-data" "$(bj 'ls -la\nrm -rf .docker-data/postgres')"
run 2 "newline: echo then dropdb"       "$(bj 'echo hi\ndropdb stockbot')"
run 2 "newline: grep then psql UPDATE"  "$(bj 'grep -rn foo .\npsql -d stockbot -c \"UPDATE positions SET x=1\"')"
run 2 "newline: find then compose down" "$(bj 'find . -name x\ndocker compose down')"
# 引用の外の改行は区切り。引用済み引数の後ろに続く行を見落とさないこと。
run 2 "newline after quoted arg"        "$(bj 'echo \"a\nb\"\ndropdb stockbot')"
# 引用が閉じていなければ剥がさない(改行が残る = 安全側)。
run 2 "unterminated quote keeps newline" "$(bj 'echo \"hi\nrm -rf .docker-data/postgres')"
# 先頭動詞の判定を原文で行うと、grep の ^ が 2 行目以降に当たって素通しする。
run 2 "read verb hidden on later line"  "$(bj 'rm -rf .docker-data/postgres \"x\ngrep foo\"')"
# 引用の対応を正規表現で取ると、区切りの改行ごと消える 3 系統。いずれも bash は
# 2 行目を実際に実行することをレビューが `bash -c` で実証済み。
# (a) 二重引用符の中のアポストロフィが対になる。
run 2 "apostrophes straddle newline"    "$(bj 'echo \"it'\''s fine\"\nrm -rf .docker-data/postgres\necho \"that'\''s all\"')"
run 2 "bare apostrophes straddle nl"    "$(bj 'echo \"'\''\"\ndropdb stockbot\necho \"'\''\"')"
# (b) 引用の種類が交互に現れる。剥がす順序を 2 通り試しても両方で消える。
run 2 "alternating quotes straddle nl"  "$(bj 'echo \"'\''\" '\''\"'\''\ndropdb stockbot\necho \"'\''\"')"
run 2 "alternating quotes variant"      "$(bj 'echo \"a'\''b\" '\''c\"d'\''\nrm -rf .docker-data/postgres\necho \"a'\''b\"')"
# (c) 引用の外のバックスラッシュはエスケープ。' を開いていない。
run 2 "escaped quote outside quotes"    "$(bj 'echo \\'\''\ndropdb stockbot\necho \\'\''')"
# (d) $'...' はエスケープが効くので \' では閉じない。' の中と同じ扱いにすると、\' で
# 閉じたと誤認し、後続の ' で状態が戻って本物の改行を引用の中だと見なす。
# 下の 2 つは bash が 2 行目を実際に実行することを実測で確認済み。
run 2 "ansi-c quote escaped apostrophe" "$(bj 'echo $'\''\\'\'''\''\nrm -rf .docker-data/postgres\necho '\''x')"
run 2 "ansi-c quote inner apostrophe"   "$(bj 'echo $'\''a\\'\''b'\''\ndropdb stockbot\necho '\''x')"
# T6: 引用の**外**の | ; & は本物の区切り。read 動詞で始まっても素通ししない。
run 2 "grep piped into psql"            "$(bj 'grep -h x q.sql | psql stockbot')"
run 2 "echo then dropdb via semicolon"  "$(bj 'echo hi; dropdb stockbot')"
# T6: リダイレクトは read 動詞を write に変える。> を失格条件に入れる。
# 書き先は「後段のルールが止める」場所にする。SQL を書いただけのファイル(patch.sql)は H6 から
# 実行ではないので通す(書いてから流す 2 段は §3 の既知の限界)。
run 2 "echo into backend/data"          "$(bj 'echo x > backend/data/7203_daily.csv')"
# T6: プロセス置換 <(...) は中のコマンドを実行する。< が無いと read 動詞で素通しできた。
run 2 "process substitution executes"   "$(bj 'cat <(dropdb stockbot)')"
run 2 "process substitution in grep"    "$(bj 'grep x <(docker compose down)')"

# stock-bot の launchd(日足更新 / backup)の停止は人間の判断。
run 2 "launchctl bootout com.stockbot" "$(bj 'launchctl bootout gui/501/com.stockbot.morning')"
run 2 "launchctl unload com.stockbot"  "$(bj 'launchctl unload /Users/USERNAME/Library/LaunchAgents/com.stockbot.morning.plist')"
# launchctl の状態を変える動詞は list/print 以外にもある(help の文言で確認)。
run 2 "launchctl debug com.stockbot"   "$(bj 'launchctl debug gui/501/com.stockbot.db-backup')"
run 2 "launchctl config com.stockbot"  "$(bj 'launchctl config user umask 022 com.stockbot.db-backup')"
run 2 "launchctl attach com.stockbot"  "$(bj 'launchctl attach gui/501/com.stockbot.db-backup')"
# plist を消す・書き換えれば launchctl unload と同じ結果になる。
run 2 "rm com.stockbot plist"          "$(bj 'rm /Users/USERNAME/Library/LaunchAgents/com.stockbot.morning.plist')"
run 2 "sed -i com.stockbot plist"      "$(bj 'sed -i \"\" s/a/b/ /Users/USERNAME/Library/LaunchAgents/com.stockbot.db-backup.plist')"
run 2 "defaults write com.stockbot"    "$(bj 'defaults write /Users/USERNAME/Library/LaunchAgents/com.stockbot.db-backup Disabled -bool true')"
run 2 "plutil -replace com.stockbot"   "$(bj 'plutil -replace Disabled -bool true /Users/USERNAME/Library/LaunchAgents/com.stockbot.db-backup.plist')"

# T3(2026-08-07): _test の escape は「接続先 DB が _test で終わる」ときだけ効く。以前は
# psql 以降のどこかに _test があれば(SQL 本文・コメント・ファイル名でも)全書込が通っていた。
run 2 "_test inside SQL payload"  "$(bj 'psql stockbot -c \"UPDATE positions SET note='\''x_test'\'' WHERE id=1\"')"
run 2 "_test in SQL comment"      "$(bj 'psql -d stockbot -c \"DELETE FROM trades -- _test\"')"
run 2 "_test only in filename"    "$(bj 'psql stockbot -f a_test.sql')"
run 2 "_test in table name"       "$(bj 'psql stockbot -c \"INSERT INTO trades_test_backup VALUES (1)\"')"
run 2 "_test in role name"        "$(bj 'psql -d stockbot -c \"GRANT ALL ON trades TO app_test\"')"
# escape 判定も行単位だった。1 行目が _test DSN なら 2 行目の live 書込が通っていた。
run 2 "multiline _test then live" "$(bj 'psql stockbot_test -c \"SELECT 1\"\npsql -d stockbot -c \"UPDATE positions SET x=1\"')"
run 2 "multiline _test then dump" "$(bj 'psql -d stockbot_test -c \"SELECT 1\"\npg_restore -d stockbot dump.bin')"
# 承認 env の escape も同じ形。言及しただけの行で発火していた。
run 2 "multiline approval mention" "$(bj 'grep -rn STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 Makefile\npsql -d stockbot -c \"UPDATE positions SET x=1\"')"
# escape の判定を -c の**中身**まで見ていると、DSN の形をした文字列を SQL に書くだけで
# 解除できる。接続先は -d stockbot(live)のまま。判定は -c / -f より前だけで行う。
run 2 "dsn shape in SQL value"    "$(bj 'psql -d stockbot -c \"UPDATE t SET note='\''run psql bench_test now'\''\"')"
run 2 "dsn shape in SQL comment"  "$(bj 'psql -d stockbot -c \"DELETE FROM trades -- psql foo_test \"')"
run 2 "dsn url in SQL value"      "$(bj 'psql -d stockbot -c \"UPDATE t SET url='\''postgres://h/x_test'\''\"')"
run 2 "dsn flag in SQL comment"   "$(bj 'psql -d stockbot -c \"DELETE FROM trades -- -d foo_test \"')"
run 2 "approval env in SQL value" "$(bj 'psql -d stockbot -c \"UPDATE t SET n='\''STOCKBOT_HUMAN_APPROVED_DB_WRITE=1'\''\"')"
# 同じ行に && で繋いだ形。改行だけ塞いでも、こちらの方が自然な書き方。
run 2 "test dsn then live via &&"  "$(bj 'psql stockbot_test -c \"SELECT 1\" && psql -d stockbot -c \"UPDATE positions SET x=1\"')"
# 接続先を 2 つ書くと後ろが勝つ。_test を先に書いて live を後ろに置く形。
run 2 "two -d targets test first" "$(bj 'psql -d stockbot_test -d stockbot -c \"UPDATE positions SET x=1\"')"
# -c は値を密着させて書ける。空白前提で切ると切り落としが起きず、上の穴がそのまま戻る。
run 2 "attached -c defeats cut"   "$(bj 'psql -d stockbot -c\"UPDATE t SET n='\''psql foo_test '\''\"')"
run 2 "attached -f defeats cut"   "$(bj 'psql -d stockbot -fa_test.sql')"
# 2つ目の接続先が -c より後ろにあると、切り落とした側だけ見ていては気づけない。
run 2 "second -d after -c"        "$(bj 'psql -d stockbot_test -c \"SELECT 1\" -d stockbot -c \"UPDATE positions SET x=1\"')"
run 2 "second dbname after -c"    "$(bj 'psql -d stockbot_test -c \"SELECT 1\" --dbname=stockbot -c \"UPDATE positions SET x=1\"')"
# 位置引数は -d を上書きする。逆順(bare が先で -d が後)も同じ。
run 2 "positional overrides -d"   "$(bj 'psql -d stockbot_test stockbot -c \"UPDATE positions SET x=1\"')"
run 2 "bare test then -d live"    "$(bj 'psql stockbot_test -d stockbot -c \"UPDATE positions SET x=1\"')"
run 2 "url test then -d live"     "$(bj 'psql postgres://u@h/stockbot_test -d stockbot -c \"UPDATE positions SET x=1\"')"
# コマンド置換は ; & | に当たらないが、中の live 書込は実行される。
run 2 "command subst carries write" "$(bj 'psql -d stockbot_test -c \"\$(psql -d stockbot -c '\''UPDATE positions SET x=1'\'')\"')"
# 接続先は argv の順で決まる。正規表現で「_test を含む形」を潰す blacklist は収束しなかった
# ので、解決した接続先が全て _test のときだけ許す whitelist に反転した。以下は 3 巡目の
# レビューが実測で見つけた 8 形(全て接続先が live)。
run 2 "tab before -c"             "$(bj 'psql -d stockbot\t-c \"UPDATE t SET n='\''psql foo_test '\''\"')"
run 2 "url as positional target"  "$(bj 'psql -d stockbot_test postgres://u@h/stockbot -c \"UPDATE positions SET x=1\"')"
run 2 "conninfo as positional"    "$(bj 'psql -d stockbot_test \"dbname=stockbot\" -c \"UPDATE positions SET x=1\"')"
run 2 "positional with hyphen"    "$(bj 'psql -d stockbot_test stock-bot -c \"UPDATE positions SET x=1\"')"
run 2 "positional with dot"       "$(bj 'psql -d stockbot_test stock.bot -c \"UPDATE positions SET x=1\"')"
run 2 "url test then positional"  "$(bj 'psql postgres://u@h/stockbot_test stockbot -c \"UPDATE positions SET x=1\"')"
run 2 "non-adjacent positional"   "$(bj 'psql -d stockbot_test -h localhost stockbot -c \"UPDATE positions SET x=1\"')"
# psql の -t(tuples-only)と -S(single-line)は**値を取らない**。pg_restore では値を取るので、
# 読み飛ばしの一覧を両者で共有すると live の位置引数を食ってしまう。
run 2 "psql -t does not take value" "$(bj 'psql -d stockbot_test -t stockbot -c \"UPDATE positions SET x=1\"')"
run 2 "psql -S does not take value" "$(bj 'psql -d stockbot_test -S stockbot -c \"UPDATE positions SET x=1\"')"
# 逆向きも同じ: pg_restore の -v は --verbose で値を取らない。psql 用の一覧を無条件に
# 適用すると、直後の -d(live)を食って接続先を見落とす。
run 2 "pg_restore -v no value"    "$(bj 'pg_restore -d stockbot_test -v -d stockbot dump.bin')"
run 2 "pg_restore -R no value"    "$(bj 'pg_restore -d stockbot_test -R -d stockbot dump.bin')"

# T4(2026-08-08): ~/.stockbot/{data,backups,...} は日足 CSV の唯一のコピー(git 管理外・
# pg_dump 対象外)。削除・移動が全て素通りしていた。
run 2 "rm -rf ~/.stockbot/data"        "$(bj 'rm -rf /Users/USERNAME/.stockbot/data')"
run 2 "rm -rf ~/.stockbot/backups"     "$(bj 'rm -rf ~/.stockbot/backups')"
run 2 "rm -rf \$HOME/.stockbot/data"    "$(bj 'rm -rf $HOME/.stockbot/data')"
run 2 "mv ~/.stockbot/retired away"    "$(bj 'mv /Users/USERNAME/.stockbot/retired /tmp/')"
# chmod は当初 SBV に入れていたが、Makefile のレシピそのものなので外した(下の chmod ケース参照)。
run 2 "rm ~/.stockbot/env"             "$(bj 'rm /Users/USERNAME/.stockbot/env')"
# find は早期素通しの動詞リストに入っている。-delete / -exec rm は再帰削除と等価。
run 2 "find -delete csv"               "$(bj 'find /Users/USERNAME/.stockbot/data -name \"*_daily.csv\" -delete')"
run 2 "find -exec rm"                  "$(bj 'find . -name \"*_daily.csv\" -exec rm {} +')"
run 2 "find -exec mv"                  "$(bj 'find . -name \"*_daily.csv\" -exec mv {} /tmp \\;')"
# emergency_stop フラグは write-once。解除は POST /api/emergency-resume(人間)経由。
run 2 "rm emergency flag"              "$(bj 'rm runtime/emergency_stop.flag')"
run 2 "rm emergency flag under backend" "$(bj 'rm -f backend/runtime/emergency_stop.flag')"
# T4 レビュー(2026-08-08)が実測で見つけた、当初案(サブディレクトリ列挙)の穴: 保護対象の
# **親**を消せば同じ結果になる。~/.stockbot 直下には立花の秘密鍵・env・hard_limits.yaml・
# stockbot-routine.sh もあるので、根ごと保護する。
run 2 "rm -rf ~/.stockbot root"        "$(bj 'rm -rf /Users/USERNAME/.stockbot')"
run 2 "rm -rf ~/.stockbot/ trailing"   "$(bj 'rm -rf ~/.stockbot/')"
run 2 "rm -rf ~/.stockbot/* glob"      "$(bj 'rm -rf ~/.stockbot/*')"
run 2 "rm private key"                 "$(bj 'rm /Users/USERNAME/.stockbot/e_api_private_key.pem')"
run 2 "rm hard_limits snapshot"        "$(bj 'rm ~/.stockbot/hard_limits.yaml')"
run 2 "rm -rf ~/.stockbot/universe"    "$(bj 'rm -rf ~/.stockbot/universe')"
run 2 "rm -rf \${HOME}/.stockbot/data"  "$(bj 'rm -rf ${HOME}/.stockbot/data')"
# backend/data は ~/.stockbot/data への symlink(CLAUDE.md)。docs も Makefile も
# こちらの表記なので、実際に打たれるのはこの形。
run 2 "rm backend/data csv"            "$(bj 'rm -f backend/data/7203_daily.csv')"
run 2 "rm backend/data glob"           "$(bj 'rm -f backend/data/*_daily.csv')"
run 2 "mv backend/data csv out"        "$(bj 'mv backend/data/7203_daily.csv /tmp/')"
run 2 "rm -rf backend/data_research"   "$(bj 'rm -rf backend/data_research')"
# パイプ越しの削除。`[^|;&]*` はパイプで止まるので動詞ルールでは捕まらない。
run 2 "find pipe xargs rm"             "$(bj 'find /Users/USERNAME/.stockbot/data -name \"*.csv\" | xargs rm')"
run 2 "ls stockbot data xargs rm"      "$(bj 'ls ~/.stockbot/data | xargs rm -rf')"
# cd してからの相対パス削除。
run 2 "cd stockbot then rm data"       "$(bj 'cd ~/.stockbot && rm -rf data')"
run 2 "cd stockbot data then rm"       "$(bj 'cd /Users/USERNAME/.stockbot/data && rm -f 7203_daily.csv')"
# 唯一のコピーは truncate でも失われる。
run 2 "truncating redirect into data"  "$(bj 'echo \"\" > ~/.stockbot/data/7203_daily.csv')"
run 2 "shred a daily csv"              "$(bj 'shred -u ~/.stockbot/data/7203_daily.csv')"
run 2 "unlink a daily csv"             "$(bj 'unlink /Users/USERNAME/.stockbot/data/7203_daily.csv')"
run 2 "rsync --delete into data"       "$(bj 'rsync -a --delete /tmp/empty/ ~/.stockbot/data/')"
run 2 "sed -i on a daily csv"          "$(bj 'sed -i \"\" -e 1d ~/.stockbot/data/7203_daily.csv')"
# find の削除経路は -exec rm だけではない。
run 2 "find -execdir rm"               "$(bj 'find . -name \"*.csv\" -execdir rm {} +')"
run 2 "find -exec /bin/rm"             "$(bj 'find . -name \"*.csv\" -exec /bin/rm {} +')"
run 2 "find -ok rm"                    "$(bj 'find . -name \"*.csv\" -ok rm {} \\;')"
run 2 "find -exec unlink"              "$(bj 'find . -name \"*.csv\" -exec unlink {} \\;')"
run 2 "nohup find -delete"             "$(bj 'nohup find . -name \"*.csv\" -delete')"
run 2 "find -delete in subshell"       "$(bj '( find . -name \"*.csv\" -delete )')"
# argv[0] アンカーは効いている(区切りの後ろも argv[0])。回帰ガード。
run 2 "semicolon then find -delete"    "$(bj 'ls -la; find . -name \"*.csv\" -delete')"
run 2 "andand then find -delete"       "$(bj 'ls -la && find . -name \"*.csv\" -delete')"
# T4 再レビュー(2026-08-08)が実測で見つけた取りこぼし。
# パス全体を引用で囲む形。終端クラスが引用符を許していなかったため、**サブパスを付けると
# deny されるのに、より破壊的な「根そのもの」が通る**という逆転が起きていた。
run 2 "quoted \$HOME root"              "$(bj 'rm -rf \"$HOME/.stockbot\"')"
run 2 "quoted absolute root"           "$(bj 'rm -rf \"/Users/USERNAME/.stockbot\"')"
run 2 "single-quoted tilde root"       "$(bj 'rm -rf '\''~/.stockbot'\''')"
run 2 "quoted mv root away"            "$(bj 'mv \"$HOME/.stockbot\" /tmp/')"
run 2 "quoted backend/data"            "$(bj 'rm -rf \"backend/data\"')"
run 2 "quoted backend/data_research"   "$(bj 'rm -rf '\''backend/data_research'\''')"
# 切り詰めは backend/data(symlink)にも効かせる。
run 2 "truncate backend/data csv"      "$(bj 'echo \"\" > backend/data/7203_daily.csv')"
run 2 "cat /dev/null into backend/data" "$(bj 'cat /dev/null > backend/data/7203_daily.csv')"
run 2 "cp /dev/null onto a daily csv"  "$(bj 'cp /dev/null ~/.stockbot/data/7203_daily.csv')"
run 2 "tee onto a daily csv"           "$(bj 'tee ~/.stockbot/data/7203_daily.csv < /dev/null')"
# cd ルールは backend/data も見る / 区切りは 1 個目に限らない / 改行も区切り。
run 2 "cd backend/data then rm"        "$(bj 'cd backend/data && rm -f 7203_daily.csv')"
run 2 "cd backend then rm -rf data"    "$(bj 'cd backend && rm -rf data')"
run 2 "cd stockbot ls then rm"         "$(bj 'cd ~/.stockbot && ls && rm -rf data')"
run 2 "cd stockbot newline then rm"    "$(bj 'cd ~/.stockbot/data\nrm -f 7203_daily.csv')"
# find の前置動詞の抜け。
run 2 "command find -delete"           "$(bj 'command find . -name \"*.csv\" -delete')"
run 2 "ionice find -delete"            "$(bj 'ionice find . -name \"*.csv\" -delete')"
# -exec sh -c の中に削除動詞があるとき。
run 2 "find -exec sh -c rm"            "$(bj 'find . -name \"*.csv\" -exec sh -c '\''rm \"$1\"'\'' _ {} \\;')"
# T4 3巡目(2026-08-08): fold_quoted の導入で**新たに開いた**穴。引用の中身に空白があると
# 畳まれるので、シェルに渡した実行コードごと不可視になっていた。fold は「畳む/畳まない」の
# 2分岐なので、必ず両側にテストを置く。
run 2 "bash -c rm data"                "$(bj 'bash -c \"rm -rf ~/.stockbot/data\"')"
run 2 "sh -c rm backend/data"          "$(bj 'sh -c '\''rm -rf backend/data'\''')"
run 2 "zsh -c rm root"                 "$(bj 'zsh -c \"rm -rf $HOME/.stockbot\"')"
run 2 "bash -lc rm data_research"      "$(bj 'bash -lc \"rm -rf backend/data_research\"')"
run 2 "eval rm data"                   "$(bj 'eval \"rm -rf ~/.stockbot/data\"')"
run 2 "env prefix sh -c rm"            "$(bj 'env FOO=1 sh -c \"rm -rf ~/.stockbot/data\"')"
run 2 "nohup bash -c rm root"          "$(bj 'nohup bash -c \"rm -rf ~/.stockbot\"')"
run 2 "su -c rm root"                  "$(bj 'su -c \"rm -rf ~/.stockbot\" USERNAME')"
run 2 "xargs -I sh -c rm"              "$(bj 'ls ~/.stockbot/data | xargs -I{} sh -c \"rm {}\"')"
run 2 "xargs -I bash -c rm"            "$(bj 'ls ~/.stockbot/data | xargs -I{} bash -c '\''rm -f {}'\''')"
# T4 4巡目(2026-08-08): シェルはパス付きでも起動できる(前置クラスに / が無かった)。
run 2 "/bin/sh -c rm"                  "$(bj '/bin/sh -c \"rm -rf ~/.stockbot/data\"')"
run 2 "/bin/bash -c rm"                "$(bj '/bin/bash -c \"rm -rf backend/data\"')"
# macOS に標準で入っているシェルは bash 系だけではない。
run 2 "ksh -c rm"                      "$(bj 'ksh -c \"rm -rf ~/.stockbot/data\"')"
run 2 "tcsh -c rm"                     "$(bj 'tcsh -c \"rm -rf ~/.stockbot/data\"')"
run 2 "fish -c rm"                     "$(bj 'fish -c \"rm -rf ~/.stockbot/data\"')"
# cd ルールを grep から [[ =~ ]] に替えた副作用。bash の ^ は**文字列先頭のみ**で、
# grep の行単位 ^ とは違う。1 行前に置くだけでルールが丸ごと無効になっていた(T1 と同じ形)。
run 2 "newline before cd then rm"      "$(bj 'ls -la\ncd /Users/USERNAME/.stockbot/data\nrm -f 7203_daily.csv')"
run 2 "newline before cd root then rm" "$(bj 'make check\ncd ~/.stockbot\nrm -rf data')"
# 根に trailing slash を付けた形。
run 2 "cd root trailing slash then rm" "$(bj 'cd ~/.stockbot/ && rm -rf data')"
# T4 5巡目(2026-08-08): NOFOLD を argv[0] 位置に絞った際の前置の列挙漏れ。
# `env FOO=1 sh -c` は 2 なのに `FOO=1 sh -c` は 0 という内部矛盾が根拠。
run 2 "env assign prefix bash -c rm"   "$(bj 'FOO=1 bash -c \"rm -rf ~/.stockbot/data\"')"
run 2 "env assign prefix sh -c rm"     "$(bj 'STOCKBOT_X=1 sh -c \"rm -rf backend/data\"')"
run 2 "subshell paren bash -c rm"      "$(bj '( bash -c \"rm -rf ~/.stockbot/data\" )')"
run 2 "brace group bash -c rm"         "$(bj '{ bash -c \"rm -rf ~/.stockbot/data\"; }')"
run 2 "if then bash -c rm"             "$(bj 'if true; then bash -c \"rm -rf ~/.stockbot/data\"; fi')"
run 2 "for do bash -c rm"              "$(bj 'for i in 1; do bash -c \"rm -rf ~/.stockbot/data\"; done')"
# T4 6巡目(2026-08-08): -c を使わずシェルの stdin にコードを流す形。T4 以前からの残余。
# heredoc 経由(sh -s <<EOF)は本文が引用でないので元から deny されていた。
run 2 "pipe code into sh"              "$(bj 'echo \"rm -rf ~/.stockbot/data\" | sh')"
run 2 "pipe code into bash"            "$(bj 'printf '\''rm -rf ~/.stockbot/data\\n'\'' | bash')"
run 2 "herestring into bash"           "$(bj 'bash <<< \"rm -rf ~/.stockbot/data\"')"
run 2 "backtick bash -c rm"            "$(bj 'echo `bash -c \"rm -rf ~/.stockbot/data\"`')"

# T7(2026-08-08): hook は Bash 文字列しか見ないので Makefile の中身は不可視。make stop は
# live bot に SIGTERM、reset-trades は forward 台帳の DELETE、migrate-down は DB rollback、
# restore-drill は破壊的 SQL を含む。
run 2 "make stop"                      "$(bj 'make stop')"
run 2 "make reset-trades"              "$(bj 'make reset-trades')"
run 2 "make migrate-down"              "$(bj 'make migrate-down')"
run 2 "make restore-drill"             "$(bj 'make restore-drill')"
run 2 "make -C backend stop"           "$(bj 'make -C backend stop')"
run 2 "approved env + make reset"      "$(bj 'STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 make reset-trades')"
run 2 "make stop then start"           "$(bj 'make stop && make start')"
# 終端が ([[:space:]]|$) だと**閉じ引用符**を許さず、シェル文字列経由が全滅していた
# (T4 の SBEND と同じ欠陥)。上の `&& make start` 付きは stop の後ろが空白なので緑になり、
# 欠陥を隠していた。空白なしで引用が閉じる形を必ず置く。
run 2 "bash -c make stop"              "$(bj 'bash -c \"make stop\"')"
run 2 "sh -c make reset-trades"        "$(bj 'sh -c '\''make reset-trades'\''')"
run 2 "bash -c cd then migrate-down"   "$(bj 'bash -c \"cd backend && make migrate-down\"')"
run 2 "eval make stop"                 "$(bj 'eval \"make stop\"')"
run 2 "pipe make restore-drill to bash" "$(bj 'echo \"make restore-drill\" | bash')"
run 2 "env prefix bash -c make stop"   "$(bj 'FOO=1 bash -c \"make stop\"')"
# S6(2026-10-01): live DB の migration は `migrate-live-*` で、`migrate-down` だけを見ていた
# T7 を素通りしていた(research の down は拒否されるのに live は通る逆転)。cmd/migrate の
# 承認 env は AI が自分で前置できるので、壁にならない。status は read-only なので通す。
run 2 "make migrate-live-down"         "$(bj 'make migrate-live-down')"
run 2 "approved env + migrate-live-up" "$(bj 'STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 make migrate-live-up')"
run 2 "bash -c migrate-live-up"        "$(bj 'bash -c \"make migrate-live-up\"')"
run 0 "make migrate-live-status"       "$(bj 'make migrate-live-status')"
# gmake(Homebrew の GNU make)は \bmake\b に当たらず、外側ゲートでも弾かれていた。
run 2 "gmake stop"                     "$(bj 'gmake stop')"
run 2 "gmake reset-trades"             "$(bj 'gmake reset-trades')"
# bot の起動も人間だけ(2026-10-02 オーナー決定)。make を経ない go run / ビルド済みバイナリも塞ぐ。
run 2 "bash -c make start"             "$(bj 'bash -c \"make start\"')"
run 2 "gmake start"                    "$(bj 'gmake start')"
run 2 "cd backend && go run stockbot"  "$(bj 'cd backend && go run ./cmd/stockbot')"
run 2 "go run ./cmd/stockbot"          "$(bj 'go run ./cmd/stockbot')"
run 2 "./bin/stockbot"                 "$(bj './bin/stockbot')"
run 0 "go test ./cmd/stockbot/"        "$(bj 'go test ./cmd/stockbot/')"
run 0 "go build -o ../bin/"            "$(bj 'go build -o ../bin/ ./...')"
run 0 "grep make start in docs"        "$(bj 'grep -rn \"make start\" docs/')"

# T8(2026-08-08): 判定を `go test` の綴りではなく**タグそのもの**に移す。ラッパ /
# バージョン付き go / ビルド済みバイナリが全て素通りしていた(実測 exit 0)。
run 2 "gotestsum integration"          "$(bj 'gotestsum -- -tags=integration ./...')"
run 2 "go1.24.0 integration"           "$(bj 'go1.24.0 test -tags=integration ./...')"
run 2 "go run test wrapper integration" "$(bj 'cd backend && richgo test -tags integration ./internal/adapter/repository')"
run 2 "GOFLAGS integration"            "$(bj 'GOFLAGS=-tags=integration go test ./...')"
run 2 "export GOFLAGS integration"     "$(bj 'export GOFLAGS=-tags=integration && go test ./...')"
run 2 "prebuilt test binary"           "$(bj './backend/pkg.test -test.run TestIntegrationTrades')"
run 2 "prebuilt binary -test.run=x"    "$(bj './repository.test -test.run=TestIntegration_Trades')"
# タグは `,` で連結できる。単独綴りだけを見ていると抜ける。
run 2 "comma-joined tags"              "$(bj 'go test -tags=unit,integration ./...')"
# シェル経由はコードを渡しているので NOFOLD 側で畳まれない = 生のまま捕まる。
run 2 "bash -c integration"            "$(bj 'bash -c \"go test -tags integration ./...\"')"
# ★ 許可経路(go vet / make …-integration)の判定は**セグメント単位**でなければならない。
# コマンド全体で探すと、無関係な vet を前に置くだけでルール全体が解除できる(実測 exit 0)。
run 2 "vet prefix then raw test"       "$(bj 'go vet ./... && go test -tags integration ./...')"
run 2 "make target then raw test"      "$(bj 'make vet-integration; go test -tags integration ./...')"
run 2 "vet suffix after raw test"      "$(bj 'go test -tags integration ./... && go vet ./...')"
# T8 レビュー(2026-08-08)が実測で見つけた、**HEAD より弱くなっていた**形。
# F1: フラグの値そのものが引用されていると fold_quoted が Q に潰して丸ごと消える。
# 隔離モジュールで実際に //go:build integration のテストが走ったことを確認済み。
# 生の $cmd を見ていた HEAD は 6 形とも deny していた = 判定を sb_cmd に移した代償。
run 2 "GOFLAGS quoted multiword"       "$(bj 'GOFLAGS=\"-tags=integration -count=1\" go test ./...')"
run 2 "GOFLAGS quoted with -mod"       "$(bj 'cd backend && GOFLAGS=\"-mod=mod -tags=integration\" go test ./internal/adapter/repository')"
run 2 "GOFLAGS single-quoted"          "$(bj 'GOFLAGS='\''-tags=integration -v'\'' go test ./...')"
# 代入ごと引用する形(`env "GOFLAGS=..."`)は引用が GOFLAGS= の**前**に来るので別の綴り。
run 2 "env quoted whole assignment"    "$(bj 'env \"GOFLAGS=-tags=integration -count=1\" go test ./...')"
run 2 "env sq whole assignment"        "$(bj 'env '\''GOFLAGS=-tags=integration -v'\'' go test ./...')"
run 2 "tags quoted list, first"        "$(bj 'go test -tags \"integration slow\" ./...')"
run 2 "tags quoted list, last"         "$(bj 'cd backend && go test -tags \"slow integration\" ./internal/adapter/repository')"
# F3: # はセグメント区切りではないので、コメントが許可経路を供給していた。
# bash は go test を実行してコメントを捨てる(実測で PASS が出た)。
run 2 "comment supplies go vet"        "$(bj 'go test -tags=integration ./... # go vet first')"
run 2 "comment supplies make target"   "$(bj 'go test -tags=integration ./... # make test-integration')"
# F2: バイナリを作る側(-c / -o)を塞ぎ、素の *.test 実行も止める。
# `-test.run …Integration` の綴りを要求していたが、その綴り無しでも同じバイナリは走る。
run 2 "go test -c with tag"            "$(bj 'cd backend && go test -c -tags=integration ./internal/adapter/repository/pg')"
run 2 "go test -o with tag"            "$(bj 'cd backend && go test -o /tmp/it -c -tags=integration ./internal/adapter/repository/pg')"
run 2 "bare prebuilt .test"            "$(bj './backend/repository.test')"
run 2 "prebuilt .test -test.v"         "$(bj './backend/repository.test -test.v')"
run 2 "prebuilt .test -test.run other" "$(bj './backend/pkg.test -test.run TestTrades')"
# F5: fold_quoted は引用の外の \ を「次の1文字ごと」捨てていた。bash は \i を i として渡す
# (argv 実測)ので、捨てると綴りが壊れて素通りする。エスケープされた文字は残すのが正しい。
run 2 "backslash-escaped tag"          "$(bj 'go test -tags=\\integration ./...')"
# F4: set -o pipefail + 末尾 grep -q は、上流が SIGPIPE(141)で死ぬと**非決定的に fail-open** する。
# レビュー実測: 6105 字で 30 回中 14 回 ALLOW、9900 字で 25/25 ALLOW。
LONGPAD="$(printf 'a%.0s' {1..9900})"
run 2 "long tag segment (pipefail)"    "$(bj "cd backend && go test -tags=integration ./internal/${LONGPAD}")"
# T8 3巡目レビュー(2026-08-08) R1: 許可条件をセグメントの**どこでも**探していたため、
# `$(go list ./...)` という日常のイディオムがそのまま解除条件になっていた。$( ) / backtick /
# リダイレクトも区切りではなかった。許可判定は argv[0] の位置でだけ見る。
run 2 "tag with cmd subst go list"     "$(bj 'go test -tags=integration $(go list ./internal/adapter/repository/...)')"
run 2 "tag with backtick go list"      "$(bj 'go test -tags=integration `go list ./...`')"
run 2 "cd + tag + cmd subst"           "$(bj 'cd backend && go test -tags integration $(go list ./...)')"
run 2 "GOFLAGS subst + tag"            "$(bj 'GOFLAGS=$(go env GOFLAGS) go test -tags=integration ./...')"
run 2 "tag with -exec staticcheck"     "$(bj 'cd backend && go test -tags=integration ./... -exec staticcheck')"
run 2 "tag with -o revive"             "$(bj 'cd backend && go test -tags=integration ./... -o /tmp/revive')"
run 2 "tag redirect to lint log"       "$(bj 'go test -tags=integration ./... > golangci-lint.log')"
run 2 "go test -c with cmd subst"      "$(bj 'cd backend && go test -c -tags=integration $(go list ./internal/adapter/repository/pg)')"
# R2: `| sh` の後ろにフラグが 1 個付くだけで NOFOLD が外れ、タグごと畳まれていた。
run 2 "pipe tag to sh -s"              "$(bj 'echo \"go test -tags=integration ./...\" | sh -s')"
run 2 "pipe tag to bash -s"            "$(bj 'echo \"go test -tags=integration ./...\" | bash -s')"
run 2 "pipe tag to sh -e"              "$(bj 'echo \"go test -tags=integration ./...\" | sh -e')"
run 2 "pipe tag to bash -s --"         "$(bj 'echo \"cd backend && go test -tags integration ./...\" | bash -s --')"
# R3: *.test の argv[0] 判定の前置リストが sudo|env|time|nohup しか無かった。
run 2 "exec prebuilt .test"            "$(bj 'exec ./backend/repository.test')"
run 2 "command prebuilt .test"         "$(bj 'command ./backend/repository.test')"
run 2 "stdbuf prebuilt .test"          "$(bj 'stdbuf -o0 ./backend/repository.test')"
run 2 "nice prebuilt .test"            "$(bj 'nice ./backend/repository.test')"
run 2 "sh -c prebuilt .test"           "$(bj 'sh -c \"./backend/repository.test\"')"
run 2 "cd && exec prebuilt .test"      "$(bj 'cd backend && exec ./repository.test')"
run 2 "timeout N prebuilt .test"       "$(bj 'timeout 60 ./backend/repository.test')"
# T8 4巡目: R1/R2/R3 の修正がいずれも**半分**だった。
# R1: 区切りに > を入れたことで、**先頭**のリダイレクト先ファイル名が argv[0] の位置に来て
# 許可条件を満たしていた。リダイレクトは畳む前に落とす(先も後ろも同じ扱いにする)。
run 2 "leading redirect + tag"         "$(bj '> golangci-lint.log go test -tags=integration ./...')"
run 2 "leading 2> redirect + tag"      "$(bj '2>staticcheck.log go test -tags=integration ./...')"
run 2 "leading >> redirect + tag"      "$(bj '>> golangci-lint.log go test -tags=integration ./...')"
run 2 "leading redirect revive"        "$(bj '> revive go test -tags=integration ./...')"
run 0 "go vet tag with output redir"   "$(bj 'cd backend && go vet -tags integration ./... > /tmp/vet.log')"
# R2: NOFOLDIN はフラグしか許していなかったので、**オペランド**が 1 個付くと外れた。
run 2 "pipe tag to sh -s -- foo"       "$(bj 'echo \"go test -tags=integration ./...\" | sh -s -- foo')"
run 2 "pipe tag to sh -s foo"          "$(bj 'echo \"go test -tags=integration ./...\" | sh -s foo')"
run 2 "pipe tag to bash -s x y z"      "$(bj 'echo \"go test -tags=integration ./...\" | bash -s x y z')"
run 2 "pipe rm to sh -s -- foo"        "$(bj 'echo \"rm -rf ~/.stockbot/data\" | sh -s -- foo')"
run 2 "pipe rm to bash -s -- x"        "$(bj 'echo \"rm -rf backend/data\" | bash -s -- x')"
# R3: TBINW に VAR= 前置が無く、アンカーに { と ! が無かった。
run 2 "VAR= prefix prebuilt .test"     "$(bj 'PATH=/usr/bin ./backend/repository.test')"
run 2 "env VAR= prebuilt .test"        "$(bj 'env GOFLAGS=-mod=mod ./backend/repository.test')"
run 2 "env two vars prebuilt"          "$(bj 'env A=1 B=2 ./backend/repository.test')"
run 2 "sudo -u prebuilt .test"         "$(bj 'sudo -u postgres ./backend/repository.test')"
run 2 "brace group prebuilt .test"     "$(bj '{ ./backend/repository.test; }')"
run 2 "bang prebuilt .test"            "$(bj '! ./backend/repository.test')"
run 2 "eval prebuilt .test"            "$(bj 'eval ./backend/repository.test')"
run 2 "xargs prebuilt .test"           "$(bj 'echo x | xargs ./backend/repository.test')"
run 2 "strace prebuilt .test"          "$(bj 'strace -f ./backend/repository.test')"
run 2 "watch prebuilt .test"           "$(bj 'watch ./backend/repository.test')"
# TAGSEP($( ) と backtick)を**単独で**担保する。argv[0] アンカーだけでは通ってしまう形:
# 外側が許可経路でも、コマンド置換の中身は bash が先に実行する。
run 2 "tag inside cmd subst of vet"    "$(bj 'go vet -tags=unit $(go test -tags=integration ./...)')"
run 2 "tag inside backtick of vet"     "$(bj 'go vet -tags=unit `go test -tags=integration ./...`')"
# T8 5巡目レビュー(2026-08-08)P0-3(**4巡目が入れた回帰**): リダイレクト除去の末尾クラスが
# ( ) backtick を除外していなかったので、置換の開き括弧まで食って TAGSEP の境界を壊し、
# 中のコマンドが許可経路のセグメントに融合していた。10 形すべてでタグ付きテストが走ることを確認。
run 2 "vet with procsub tag"           "$(bj 'go vet -tags=unit <(go test -tags=integration ./...)')"
run 2 "vet with >procsub tag"          "$(bj 'go vet -tags=unit >(go test -tags=integration ./...)')"
run 2 "vet redirect to subst"          "$(bj 'go vet -tags=unit > $(go test -tags=integration ./...)')"
run 2 "vet redirect from subst"        "$(bj 'go vet -tags=unit < $(go test -tags=integration ./...)')"
run 2 "vet redirect backtick"          "$(bj 'go vet -tags=unit < `go test -tags=integration ./...`')"
run 2 "vet 2> subst tag"               "$(bj 'go vet -tags=unit 2> $(go test -tags=integration ./...)')"
run 2 "vet >> subst tag"               "$(bj 'go vet -tags=unit >> $(go test -tags=integration ./...)')"
run 2 "staticcheck procsub tag"        "$(bj 'staticcheck ./... <(go test -tags=integration ./...)')"
run 2 "golangci procsub tag"           "$(bj 'golangci-lint run <(go test -tags=integration ./...)')"
# P0-4: >&file は & で末尾が止まるので除去されず、先頭リダイレクトの経路が生き残っていた。
run 2 "redirect dup leading"           "$(bj '>&golangci-lint.log go test -tags=integration ./...')"
run 2 "redirect dup spaced"            "$(bj '>& golangci-lint.log go test -tags=integration ./...')"
run 2 "redirect dup revive"            "$(bj '>&revive go test -tags=integration ./...')"
run 2 "cd && redirect dup"             "$(bj 'cd backend && >&golangci-lint.log go test -tags=integration ./...')"
# 制御: fd 複製そのものは日常操作。許可経路を壊さない。
run 0 "go vet tag with 2>&1"           "$(bj 'cd backend && go vet -tags integration ./... 2>&1 | tail -5')"
# T8 6巡目レビュー(2026-08-08): 同じ機構の**3つ目の綴り**。演算子クラスに 1 文字
# 足りないだけで先頭リダイレクト先が argv[0] に残る。今回は演算子側を網羅する形に直す。
run 2 "noclobber redirect leading"     "$(bj '>|revive go test -tags=integration ./...')"
run 2 "noclobber redirect spaced"      "$(bj '>| revive go test -tags=integration ./...')"
run 2 "noclobber staticcheck"          "$(bj '>|staticcheck go test -tags=integration ./...')"
run 2 "noclobber lint log"             "$(bj '>|golangci-lint.log go test -tags=integration ./...')"
run 2 "noclobber 2>| revive"           "$(bj '2>|revive go test -tags=integration ./...')"
run 2 "cd && noclobber revive"         "$(bj 'cd backend && >|revive go test -tags=integration ./...')"
# {name}> の fd 変数形。変数名が許可リストのツール名だと TAGPRE の [{(!]? を抜ける。
run 2 "fd var revive redirect"         "$(bj '{revive}>/tmp/x go test -tags=integration ./...')"
run 2 "fd var staticcheck redirect"    "$(bj '{staticcheck}>/tmp/x go test -tags=integration ./...')"
run 2 "fd var fd redirect"             "$(bj '{fd}>/tmp/x go test -tags=integration ./...')"
# パイプは区切りのまま(リダイレクト演算子として食い過ぎていないことの担保)。
run 0 "go vet tag redirect then pipe"  "$(bj 'cd backend && go vet -tags integration ./... > /tmp/v.log')"
run 0 "go build then pipe grep"        "$(bj 'cd backend && go build -tags integration ./... | grep -c error')"
# ラッパの引数に任意の語を許すと、純粋な read を巻き込む。フラグと数字始まりだけにする。
run 0 "time cat a .test file"          "$(bj 'time cat backend/fixtures/golden.test')"
# ラッパのフラグに値を許す枝が、値の位置で read コマンドを食って誤 deny していた。
# 値を取るのは -u / -n 等の**特定のフラグだけ**なので、そこだけ許す(time -p / strace -f は取らない)。
run 0 "time -p cat a .test file"       "$(bj 'time -p cat backend/fixtures/golden.test')"
run 0 "strace -f cat a .test file"     "$(bj 'strace -f cat backend/fixtures/golden.test')"
run 0 "nice -n 10 cat a .test file"    "$(bj 'nice -n 10 cat backend/fixtures/golden.test')"
# 値を取るフラグの列挙漏れ(env -C / --chdir)。絞り込みの代償として開いた分を塞ぐ。
run 2 "env -C prebuilt .test"          "$(bj 'env -C /tmp ./backend/repository.test')"
run 2 "env --chdir prebuilt .test"     "$(bj 'env --chdir /tmp ./backend/repository.test')"
run 0 "env -C then cat .test file"     "$(bj 'env -C /tmp cat backend/fixtures/golden.test')"
# TAGPRE が TBINW / NOFOLDW より狭く、許可経路の argv[0] が前置語で隠れて誤 deny していた。
run 0 "timeout + go vet tag"           "$(bj 'timeout 60 go vet -tags integration ./...')"
run 0 "for/do + go vet tag"            "$(bj 'for d in a b; do go vet -tags integration $d; done')"
run 0 "if/then + go vet tag"           "$(bj 'if true; then go vet -tags integration ./...; fi')"
run 0 "brace group go vet tag"         "$(bj '{ go vet -tags integration ./...; }')"
# R4: sed は行単位なので、引用された値の中の改行で正規化が効かなくなっていた。
run 2 "GOFLAGS newline in value"       "$(bj 'GOFLAGS=\"-tags=integration\n-count=1\" go test ./...')"
# Gap 1: *.test という名前でないビルド済みバイナリ(go test -o の産物)。ルール B の唯一の担保。
run 2 "unnamed prebuilt binary"        "$(bj '/tmp/it -test.run TestIntegration')"
# Gap 3: go generate はコードを実行するので carve-out に入れない(負の方向の担保)。
run 2 "go generate with tag"           "$(bj 'cd backend && go generate -tags integration ./...')"

# T9(2026-08-08): stock-bot 自身の作業ツリーは無防備だった。git add / commit は
# 早期素通しリスト入りだが reset / clean / checkout -- . は判定対象ですらなかった。
run 2 "git reset --hard"               "$(bj 'git reset --hard HEAD~5')"
run 2 "git reset --hard (no arg)"      "$(bj 'git reset --hard')"
run 2 "git reset --merge"              "$(bj 'git reset --merge')"
run 2 "git clean -fdx"                 "$(bj 'git clean -fdx')"
run 2 "git clean -xdf (順序違い)"      "$(bj 'git clean -xdf')"
run 2 "git clean -f"                   "$(bj 'git clean -f')"
run 2 "git clean -d"                   "$(bj 'git clean -d')"
run 2 "git checkout -- ."              "$(bj 'git checkout -- .')"
run 2 "git checkout -- ./"             "$(bj 'git checkout -- ./')"
run 2 "git restore ."                  "$(bj 'git restore .')"
run 2 "git restore --worktree ."       "$(bj 'git restore --worktree .')"
# 前置オプション(-C)とチェーン。T4/T7 で 2 回踏んだ「前置きの変種」。
run 2 "git -C path reset --hard"       "$(bj 'git -C /repo reset --hard')"
run 2 "cd then git clean -fdx"         "$(bj 'cd backend && git clean -fdx')"
run 2 "git stash then reset --hard"    "$(bj 'git stash && git reset --hard origin/main')"
# 引用された 1 語で終わる形。T4/T7 で 2 回踏んだ終端クラスの穴。
run 2 "bash -c git reset --hard"       "$(bj 'bash -c \"git reset --hard\"')"
run 2 "bash -c git clean -fdx"         "$(bj 'bash -c \"git clean -fdx\"')"
# T9 ALLOW: 対象を明示した操作と、言及しただけの記録系。
run 0 "git reset <file>"               "$(bj 'git reset backend/main.go')"
run 0 "git reset (index only)"         "$(bj 'git reset')"
run 0 "git reset --soft"               "$(bj 'git reset --soft HEAD~1')"
run 0 "git checkout <branch>"          "$(bj 'git checkout main')"
run 0 "git checkout -b <branch>"       "$(bj 'git checkout -b feature/x')"
run 0 "git checkout -- <file>"         "$(bj 'git checkout -- backend/main.go')"
run 0 "git checkout -- ./backend"      "$(bj 'git checkout -- ./backend')"
run 0 "git clean -n (dry run)"         "$(bj 'git clean -n')"
run 0 "git clean --dry-run"            "$(bj 'git clean --dry-run')"
run 0 "git restore --staged ."         "$(bj 'git restore --staged .')"
run 0 "git restore <file>"             "$(bj 'git restore backend/main.go')"
run 0 "git status/diff"                "$(bj 'git status --short && git diff --stat')"
run 0 "commit msg mentions reset"      "$(bj 'git add -A && git commit -m \"docs: git reset --hard を deny した\"')"
run 0 "commit msg mentions clean"      "$(bj 'git add -A && git commit -m \"fix(hooks): git clean -fdx も塞いだ\"')"
run 0 "grep mentions git clean"        "$(bj 'grep -rn \"git clean -fdx\" docs/')"
# T9 レビュー(2026-08-08): 綴りではなく**操作**で見る。使い捨てリポジトリで
# 「実際に作業が消えるか」を確認した 16 形が素通りしていた。
run 2 "git checkout . (-- 無し)"       "$(bj 'git checkout .')"
run 2 "git checkout ./"                "$(bj 'git checkout ./')"
run 2 "git checkout HEAD -- ."         "$(bj 'git checkout HEAD -- .')"
run 2 "git checkout HEAD ."            "$(bj 'git checkout HEAD .')"
run 2 "git checkout -f"                "$(bj 'git checkout -f')"
run 2 "git checkout --force main"      "$(bj 'git checkout --force main')"
run 2 "git clean --force"              "$(bj 'git clean --force')"
run 2 "git clean --force -d"           "$(bj 'git clean --force -d')"
run 2 "git clean -x -f"                "$(bj 'git clean -x -f')"
run 2 "git clean -q -f -d"             "$(bj 'git clean -q -f -d')"
run 2 "git clean -e keepme -fd"        "$(bj 'git clean -e keepme -fd')"
run 2 "git restore :/"                 "$(bj 'git restore :/')"
run 2 "git restore glob"               "$(bj 'git restore \"*\"')"
run 2 "git checkout -- glob"           "$(bj 'git checkout -- \"*\"')"
run 2 "git switch --discard-changes"   "$(bj 'git switch --discard-changes main')"
run 2 "git rm -rf ."                   "$(bj 'git rm -rf .')"
run 2 "git stash drop"                 "$(bj 'git stash drop')"
run 2 "git stash clear"                "$(bj 'git stash clear')"
run 2 "git reflog expire --all"        "$(bj 'git reflog expire --expire=now --all')"
# 長いフラグの誤爆に注意: --dry-run は d を含むが破壊しない。
run 0 "git clean --dry-run (再確認)"   "$(bj 'git clean --dry-run')"
run 0 "git clean -n -d"                "$(bj 'git clean -n -d')"
run 0 "git clean -i"                   "$(bj 'git clean -i')"
run 0 "git switch main"                "$(bj 'git switch main')"
run 0 "git switch -c feature"          "$(bj 'git switch -c feature/x')"
run 0 "git rm --cached file"           "$(bj 'git rm --cached backend/main.go')"
run 0 "git rm -f file"                 "$(bj 'git rm -f backend/old.go')"
run 0 "git stash list"                 "$(bj 'git stash list')"
run 0 "git stash push -m"              "$(bj 'git stash push -m wip')"
run 0 "git reflog"                     "$(bj 'git reflog')"

# T10(2026-08-08): psql メタコマンド。\copy … from は既存の COPY FROM ルールが
# 拾っていたが、\i(SQL ファイル実行)と \o(出力先の上書き)が抜けていた。
run 2 "psql \\i patch.sql"             "$(bj 'psql stockbot -c \"\\\\i patch.sql\"')"
run 2 "psql \\ir patch.sql"            "$(bj 'psql stockbot -c \"\\\\ir patch.sql\"')"
run 2 "psql \\include patch.sql"       "$(bj 'psql stockbot -c \"\\\\include patch.sql\"')"
run 2 "psql \\o out.txt"               "$(bj 'psql stockbot -c \"\\\\o /tmp/out.txt\"')"
run 2 "psql \\copy from csv"           "$(bj 'psql stockbot -c \"\\\\copy trades from x.csv\"')"
run 2 "psql -f then \\i"               "$(bj 'psql -d stockbot -c \"\\\\i /tmp/patch.sql\"')"
# _test DSN と承認 env の escape はメタコマンドにも効く(既存 escape の回帰)。
run 0 "psql _test \\i patch.sql"       "$(bj 'psql stockbot_test -c \"\\\\i patch.sql\"')"
run 0 "approved \\i patch.sql"         "$(bj 'STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 psql stockbot -c \"\\\\i patch.sql\"')"
# 読み取り系メタコマンドは通す。
run 0 "psql \\dt"                      "$(bj 'psql stockbot -c \"\\\\dt\"')"
run 0 "psql \\d trades"                "$(bj 'psql stockbot -c \"\\\\d trades\"')"
run 0 "psql \\l"                       "$(bj 'psql stockbot -c \"\\\\l\"')"
run 0 "psql \\copy to stdout"          "$(bj 'psql stockbot -c \"\\\\copy (SELECT 1) to stdout\"')"
run 0 "psql \\timing"                  "$(bj 'psql stockbot -c \"\\\\timing\"')"
# T10 レビュー(2026-08-08): \! は**任意のシェル実行**。-c 本文は空白を含むので
# fold で丸ごと Q になり、T4/T7 からは構造的に見えない = ここでしか止められない。
run 2 "psql \\! rm -rf backend/data"   "$(bj 'psql stockbot -c \"\\\\! rm -rf backend/data\"')"
run 2 "psql \\! make stop"             "$(bj 'psql stockbot -c \"\\\\! make stop\"')"
run 2 "psql SELECT then \\!"           "$(bj 'psql -d stockbot -c \"SELECT 1 \\\\! rm -rf backend/data\"')"
run 2 "psql \\gexec"                   "$(bj 'psql stockbot -c \"\\\\gexec\"')"
run 2 "psql \\g to file"               "$(bj 'psql stockbot -c \"SELECT 1 \\\\g /tmp/out.txt\"')"
run 2 "psql \\gx to file"              "$(bj 'psql stockbot -c \"SELECT 1 \\\\gx /tmp/out.txt\"')"
run 2 "psql \\w buffer"                "$(bj 'psql stockbot -c \"\\\\w /tmp/buf.sql\"')"
run 2 "psql \\e editor"                "$(bj 'psql stockbot -c \"\\\\e\"')"
run 2 "psql \\ef func"                 "$(bj 'psql stockbot -c \"\\\\ef myfunc\"')"
run 2 "psql \\s history"               "$(bj 'psql stockbot -c \"\\\\s /tmp/hist.txt\"')"
run 2 "psql \\o pipe (空白なし)"       "$(bj 'psql stockbot -c \"\\\\o|cat\"')"
# 綴りが似ている読み取り系を巻き込まないこと(\e vs \echo, \s vs \set, \w vs \watch, \i vs \if)。
run 0 "psql \\echo"                    "$(bj 'psql stockbot -c \"\\\\echo hello\"')"
run 0 "psql \\set"                     "$(bj 'psql stockbot -c \"\\\\set x 1\"')"
run 0 "psql \\watch"                   "$(bj 'psql stockbot -c \"SELECT 1 \\\\watch 5\"')"
run 0 "psql \\if"                      "$(bj 'psql stockbot -c \"\\\\if :x\"')"
run 0 "psql \\g (ファイル無し)"        "$(bj 'psql stockbot -c \"SELECT 1 \\\\g\"')"
run 0 "psql \\gset"                    "$(bj 'psql stockbot -c \"SELECT 1 AS a \\\\gset\"')"

# approval env must NOT bypass destructive/non-write rules.
run 2 "approved + DROP DATABASE"   "$(bj 'STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 psql -c \\"DROP DATABASE stockbot\\"')"
run 2 "approved + docker down"     "$(bj 'STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 docker compose down -v')"
run 2 "approved + integration"     "$(bj 'STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 go test -tags integration ./...')"
run 2 "approved + rm docker-data"  "$(bj 'STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 rm -rf .docker-data/postgres')"
run 2 "approved + TRUNCATE"        "$(bj 'STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 psql -c \\"TRUNCATE trades\\"')"

run 0 "plain go test"              "$(bj 'cd backend && go test ./...')"
run 0 "make test-integration"     "$(bj 'make test-integration')"
run 0 "psql SELECT"               "$(bj 'psql stockbot -c \\"SELECT count(*) FROM trades\\"')"
run 0 "psql INSERT on _test db"   "$(bj 'psql stockbot_test -c \\"INSERT INTO trades VALUES (1)\\"')"
run 0 "human-approved write"      "$(bj 'STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 psql stockbot -c \\"UPDATE positions SET x=1\\"')"
run 0 "non-Bash tool"             '{"tool_name":"Read","tool_input":{"file_path":"x"}}'
run 0 "ls"                        "$(bj 'ls -la')"
run 0 "docker compose up"         "$(bj 'docker compose up -d')"
run 0 "make migrate-up"           "$(bj 'make migrate-up')"
# T7: 起動・停止・破壊を伴わない make ターゲットは全て許可のまま(make start は 2026-10-02 から deny)。
run 2 "make start"                "$(bj 'make start')"
run 0 "make check"                "$(bj 'make check')"
run 0 "make migrate-status"       "$(bj 'make migrate-status')"
run 0 "make guard-fast"           "$(bj 'make guard-fast')"
run 0 "make backup-now"           "$(bj 'make backup-now')"
run 0 "make routine-status"       "$(bj 'make routine-status')"
# T7: 「言及しただけ」は通す(2026-07-31 の誤 deny の再来を避ける)。
run 0 "grep make stop in docs"     "$(bj 'grep -rn \"make stop\" docs/')"
run 0 "commit msg mentions make stop" "$(bj 'git add -A && git commit -m \"docs: make stop は人間が実行する\"')"
run 0 "make stop inside a word"    "$(bj 'make stop-check')"
run 0 "make stopwatch"            "$(bj 'make stopwatch')"
run 0 "cmake target stop"         "$(bj 'cmake --build . --target stop')"
# false-positive regressions (word-boundary correctness)
run 0 "psql SELECT updated_at"    "$(bj 'psql stockbot -c \\"SELECT id, updated_at FROM positions ORDER BY updated_at\\"')"
run 0 "non-psql UPDATE word"      "$(bj 'git commit -m \\"docs: UPDATE handling notes\\"')"
run 0 "pkill unrelated proc"      "$(bj 'pkill -f some_other_daemon')"
# approved write still allowed (the narrow escape it is for)
run 0 "approved psql UPDATE"      "$(bj 'STOCKBOT_HUMAN_APPROVED_DB_WRITE=1 psql stockbot -c \\"UPDATE positions SET x=1\\"')"
# _test DSN の narrow escape は新設の write 指標(-f / pg_restore)にも効く
run 0 "pg_restore into _test"     "$(bj 'pg_restore -d stockbot_test dump.bin')"
run 0 "psql -f on _test"          "$(bj 'psql stockbot_test -f patch.sql')"
# T3: 接続先が _test なら従来どおり許可。3 形(bare / -d / URL)+ docker exec 経由。
run 0 "-d _test dbname"           "$(bj 'psql -d stockbot_test -c \"DELETE FROM trades\"')"
run 0 "--dbname= _test"           "$(bj 'psql --dbname=stockbot_test -c \"DELETE FROM trades\"')"
run 0 "docker exec -d _test"      "$(bj 'docker exec stockbot-postgres psql -U stockbot -d stockbot_test -c \"DELETE FROM trades\"')"
run 0 "url _test dsn"             "$(bj 'psql postgres://stockbot:stockbot@localhost:5434/stockbot_test -c \"DELETE FROM trades\"')"
run 0 "url _test dsn quoted"      "$(bj 'psql \"postgresql://stockbot@localhost:5434/stockbot_test\" -c \"DELETE FROM trades\"')"
# 開発者が実際に打つ形。区切り無しの -d と conninfo 形も escape を効かせる。
run 0 "-d attached _test dbname"  "$(bj 'psql -dstockbot_test -c \"DELETE FROM trades\"')"
run 0 "conninfo dbname= _test"    "$(bj 'psql \"dbname=stockbot_test\" -c \"DELETE FROM trades\"')"
run 0 "--dbname space _test"      "$(bj 'psql --dbname stockbot_test -c \"DELETE FROM trades\"')"
# 接続先が 1 つなら、他のオプションが後ろに並んでいても escape は効く。
run 0 "_test then -h -p -U"       "$(bj 'psql -d stockbot_test -h localhost -p 5434 -U stockbot -c \"DELETE FROM trades\"')"
run 0 "pg_restore _test with -j"  "$(bj 'pg_restore -d stockbot_test -j 4 dump.bin')"
# ホスト名に -c が含まれても切り落とし位置を誤らない(空白付きの -c だけを見る)。
run 0 "host name contains -c"     "$(bj 'psql -h foo-cluster -d stockbot_test -c \"DELETE FROM trades\"')"
# SQL 本文の中の -d はデコイ。引用領域を 1 語に畳むので接続先とは見なさない。
run 0 "decoy -d inside SQL"       "$(bj 'psql -d stockbot_test -c \"DELETE FROM trades WHERE note = '\''-d foo'\''\"')"
# 引用された正当な DSN は畳んでも残す(中に空白が無いので中身をそのまま 1 語にする)。
run 0 "quoted conninfo target"    "$(bj 'psql \"dbname=stockbot_test\" -c \"DELETE FROM trades\"')"
run 0 "psql _test with username"  "$(bj 'psql -d stockbot_test -U stockbot -c \"DELETE FROM trades\"')"
# psql の用法は [OPTION]... [DBNAME [USERNAME]]。**第2位置引数はユーザ名で接続先ではない。**
# ここを接続先と数えると正当な形を deny する(レビューはこれを素通しと報告したが誤り)。
run 0 "positional dbname+user"    "$(bj 'psql stockbot_test stockbot -c \"UPDATE positions SET x=1\"')"
# psql 側で値を取るオプションは読み飛ばす(-R / --variable が漏れていた)。
run 0 "psql -R takes a value"     "$(bj 'psql -d stockbot_test -R X -c \"DELETE FROM trades\"')"
run 0 "psql --variable"           "$(bj 'psql -d stockbot_test --variable x=1 -c \"DELETE FROM trades\"')"
# pg_restore 側では -t / -S が値を取る。こちらは読み飛ばして escape を保つ。
run 0 "pg_restore -t takes value" "$(bj 'pg_restore -d stockbot_test -t trades dump.bin')"
run 0 "pg_restore -S takes value" "$(bj 'pg_restore -d stockbot_test -S postgres dump.bin')"
run 0 "psql -T takes a value"     "$(bj 'psql -d stockbot_test -T border=1 -c \"DELETE FROM trades\"')"
run 0 "pg_restore -v verbose"     "$(bj 'pg_restore -d stockbot_test -v dump.bin')"
run 0 "kill unrelated pgrep"      "$(bj 'kill $(pgrep -f some_daemon)')"
# 2026-07-31 レビューで実測された誤 deny(言及しただけの閲覧・記録コマンド)
run 0 "grep mentions pg_restore"  "$(bj 'grep -rn pg_restore Makefile')"
run 0 "grep mentions docker stop" "$(bj 'grep -rn \\"docker stop\\" README.md')"
run 0 "commit msg mentions deny"  "$(bj 'git commit -m \\"feat(hooks): deny docker stop/kill/rm\\"')"
run 0 "COPY TO STDOUT is read"    "$(bj 'psql stockbot -c \\"COPY (SELECT * FROM trades) TO STDOUT WITH CSV HEADER\\"')"
# T1: 改行を含んでも、全行が無害なら通常判定を全て抜けて許可される。
run 0 "newline: ls then ls"       "$(bj 'ls -la\nls backend')"
run 0 "newline: cd + go test"     "$(bj 'cd backend\ngo test ./internal/...')"
# T1 の回帰防止。引用符の中の改行は区切りではない。本文が複数行の commit message が
# 禁止語に**言及しただけ**で deny されるのは 2026-07-31 の誤 deny の再来(レビュー実測)。
run 0 "multiline commit body: rm -rf" "$(bj 'git commit -m \"fix(hooks): 改行バイパス\n\nrm -rf .docker-data/postgres が exit 0 だった\"')"
run 0 "multiline commit body: dropdb" "$(bj 'git commit -m \"docs(runbook): dropdb は人間のみ\n\n本文2行目\"')"
run 0 "quoted newline in echo text"   "$(bj 'echo \"line1\ndropdb は禁止\"')"
# 順序を 2 通り見る対策が、アポストロフィを含む正当な本文まで deny しないこと。
run 0 "commit body with 1 apostrophe"  "$(bj 'git commit -m \"fix: don'\''t drop it\n\n本文2行目\"')"
run 0 "commit body with 2 apostrophes" "$(bj 'git commit -m \"fix: don'\''t drop it\n\nit won'\''t dropdb anything\"')"
run 0 "single-quoted multiline text"   "$(bj 'echo '\''say \"hi\"\nbye — dropdb は禁止'\''')"
# 二重引用符の中のエスケープされた " は閉じない。\ + 改行は行継続で区切りではない。
run 0 "escaped dquote inside dquote"   "$(bj 'echo \"a\\\"b\ndropdb は禁止\"')"
run 0 "backslash line continuation"    "$(bj 'ls -la \\\n  -h')"
# 閉じている $'...' の中の改行は区切りではない。
run 0 "ansi-c quote spanning newline"  "$(bj 'echo $'\''line1\ndropdb は禁止'\''')"
# T6(2026-08-07): 引用の中の | ; & はシェルのメタ文字ではない。監査中に実際に誤 deny された。
# 全ケース「引用の中にメタ文字」+「そのままだと発火するルール名への言及」の組で、
# 修正前は exit 2 になる(= 本当に T6 を証明する)。
run 0 "grep alternation Makefile"      "$(bj 'grep -nEi \"drop|truncate|pg_restore\" Makefile')"
run 0 "grep alternation dquote"        "$(bj 'grep -rn \"docker stop|docker kill\" README.md')"
run 0 "grep alternation squote"        "$(bj 'grep -rn '\''docker stop|docker kill'\'' README.md')"
run 0 "commit msg with semicolon"      "$(bj 'git commit -m \"feat(hooks): deny docker stop; kill; rm\"')"
run 0 "commit msg with ampersand"      "$(bj 'git commit -m \"deny docker stop & docker kill\"')"
# < を失格条件に入れても、ただの stdin リダイレクトは通常判定を抜けて許可される。
run 0 "plain stdin redirect"           "$(bj 'cat < patch.sql')"
# plist の**読み取り**は許可(launchctl print を許可しているのと同じ理由)。
run 0 "plutil -p is read"              "$(bj 'plutil -p /Users/USERNAME/Library/LaunchAgents/com.stockbot.morning.plist')"
run 0 "plutil -lint is read"           "$(bj 'plutil -lint /Users/USERNAME/Library/LaunchAgents/com.stockbot.morning.plist')"
run 0 "defaults read is read"          "$(bj 'defaults read /Users/USERNAME/Library/LaunchAgents/com.stockbot.morning')"
run 0 "write unrelated com.stockbot"   "$(bj 'echo x > /tmp/com.stockbot.txt')"
run 0 "launchctl list"                 "$(bj 'launchctl list')"
run 0 "launchctl list grep is read"    "$(bj 'launchctl list | grep stockbot')"
run 0 "launchctl print stockbot"       "$(bj 'launchctl print gui/501/com.stockbot.morning')"
# T4 が ~/.stockbot を根ごと保護している(中の一時ファイルでも削除は人間)。
run 2 "rm inside ~/.stockbot scratch"  "$(bj 'rm /Users/USERNAME/.stockbot/tmp/scratch.json')"
run 0 "rm unrelated stockbot path"     "$(bj 'rm /tmp/stockbot-scratch/x.json')"
# T4: read は全て許可。保護しているのは削除・移動・切り詰めだけ。
run 0 "ls ~/.stockbot/data"            "$(bj 'ls -la /Users/USERNAME/.stockbot/data')"
run 0 "cat a daily csv"                "$(bj 'cat /Users/USERNAME/.stockbot/data/7203_daily.csv')"
run 0 "head backend/data csv"          "$(bj 'head -3 backend/data/7203_daily.csv')"
run 0 "find without delete"            "$(bj 'find /Users/USERNAME/.stockbot/data -name \"*_daily.csv\"')"
run 0 "find backend/data no delete"    "$(bj 'find backend/data -name \"*_daily.csv\"')"
run 0 "rm scratch file"                "$(bj 'rm /tmp/scratch.json')"
# Makefile が実際に打つ形(routine-install / backup-install / fetch-daily)。
# 設置(cp / go build -o / mkdir)は削除ではないので通す。
run 0 "make routine-install"           "$(bj 'make routine-install')"
run 0 "make fetch-daily"               "$(bj 'make fetch-daily')"
run 0 "go build into ~/.stockbot/bin"  "$(bj 'cd backend && go build -o ~/.stockbot/bin/fetch-daily ./cmd/fetch-daily')"
run 0 "cp hard_limits snapshot"        "$(bj 'cp configs/hard_limits.yaml ~/.stockbot/hard_limits.yaml')"
run 0 "mkdir -p ~/.stockbot/logs"      "$(bj 'mkdir -p ~/.stockbot/logs ~/.stockbot/tmp')"
run 0 "list latest backup"             "$(bj 'ls -1t ~/.stockbot/backups/stockbot-*.sql.gz | head -1')"
run 0 "tar -tzf data backup"           "$(bj 'tar -tzf ~/.stockbot/backups/data/daily-20260807.tar.gz')"
# backend/data のトークンを含まない通常の repo 操作は無関係。
run 0 "mv a go file"                   "$(bj 'mv backend/internal/foo.go backend/internal/bar.go')"
run 0 "rm a build artifact"            "$(bj 'rm backend/bin/stockbot')"
# T4 再レビュー(2026-08-08): 2026-07-31 の誤 deny の**再来**。早期素通しは && / | / >> が
# あると失格するので、その先で「動詞 + 保護パス」ルールが**引用の中の言及**を掴んでいた。
# CLAUDE.md も DAILY_UNIVERSE.md もこの文字列だらけで、T4 自身のコミットが刺さる。
run 0 "add && commit mentions rm data" "$(bj 'git add -A && git commit -m \"fix: rm backend/data の穴\"')"
run 0 "commit mentions rm && push"     "$(bj 'git commit -m \"fix: rm -rf ~/.stockbot の穴\" && git push')"
run 0 "grep mentions rm piped to head" "$(bj 'grep -rn \"rm -rf ~/.stockbot/data\" docs/ | head -5')"
run 0 "append note mentioning rm"      "$(bj 'echo \"rm -rf ~/.stockbot/data は deny\" >> docs/notes.md')"
# chmod / chown は「削除・移動・切り詰め」ではない。Makefile の routine-install /
# backup-install の**レシピそのもの**なので、動詞リストから外す。
run 0 "chmod +x db-backup.sh"          "$(bj 'chmod +x ~/.stockbot/db-backup.sh')"
run 0 "chmod +x stockbot-routine.sh"   "$(bj 'chmod +x ~/.stockbot/stockbot-routine.sh')"
run 0 "chmod 600 ~/.stockbot/env"      "$(bj 'chmod 600 ~/.stockbot/env')"
# ログは「唯一のコピー」ではない。> を ~/.stockbot 全体に掛けると plist の再現が打てない。
run 0 "forward-report into logs"       "$(bj '~/.stockbot/bin/forward-report -since 2026-07-24 > ~/.stockbot/logs/weekly.log 2>&1')"
run 0 "fetch-daily appends to log"     "$(bj '~/.stockbot/bin/fetch-daily >> ~/.stockbot/logs/morning.log 2>&1')"
run 0 "universe-screen writes today"   "$(bj '~/.stockbot/bin/universe-screen -out ~/.stockbot/universe/today.txt')"
# read-only な -exec sh -c は常用イディオム。削除動詞があるときだけ見る。
run 0 "find -exec sh -c gofmt"         "$(bj 'find . -name \"*.go\" -exec sh -c '\''gofmt -l \"$1\"'\'' _ {} \\;')"
run 0 "find -exec gofmt directly"      "$(bj 'find . -name \"*.go\" -exec gofmt -l {} +')"
# DAILY_UNIVERSE.md の手順(read-only なパイプライン)。
run 0 "list data then sed sort"        "$(bj 'ls ~/.stockbot/data/*_daily.csv | sed '\''s|.*/||'\'' | sort > /tmp/have.txt')"
run 0 "diff stockbot vs backend data"  "$(bj 'diff <(ls ~/.stockbot/data) <(ls backend/data)')"
run 0 "tar czf data backup out"        "$(bj 'tar -czf /tmp/x.tar.gz -C ~/.stockbot data')"
run 0 "du -sh ~/.stockbot/data"        "$(bj 'du -sh ~/.stockbot/data')"
run 0 "count backend/data csv"         "$(bj 'find backend/data -name \"*_daily.csv\" | wc -l')"
# cd してからの read は巻き込まない。
run 0 "cd stockbot data then grep"     "$(bj 'cd ~/.stockbot/data && grep -c rm 7203_daily.csv')"
run 0 "cd backend then go test"        "$(bj 'cd backend && go test ./internal/...')"
# 保護対象の外の bare data は無関係。
run 0 "rm unrelated nested data dir"   "$(bj 'rm -rf /tmp/foo/data')"
# T4 3巡目: cd ルールは (1) cd より**後ろ**だけを見る (2) cd 先から logs/tmp を外す。
# cd と削除動詞を独立に grep していたため、cd の**前**の無関係な削除まで巻き込んでいた。
run 0 "rm tmp then cd stockbot"        "$(bj 'rm -rf /tmp/build && cd ~/.stockbot')"
run 0 "rm tmp; cd stockbot then ls"    "$(bj 'rm /tmp/x.json; cd ~/.stockbot/data && ls')"
run 0 "cd data then wc then rm tmp"    "$(bj 'cd ~/.stockbot/data && wc -l *.csv; rm /tmp/x')"
# logs/ は「唯一のコピー」ではない(> ルールから外したのと同じ理由)。tmp/ は
# routine-install が mkdir する作業ディレクトリ。cd 先から外さないと自己矛盾になる。
run 0 "cd logs then rm old log"        "$(bj 'cd ~/.stockbot/logs && rm -f old.log')"
run 0 "cd tmp then rm scratch"         "$(bj 'cd ~/.stockbot/tmp && rm -f scratch.json')"
run 0 "cd data then tail then popd"    "$(bj 'pushd ~/.stockbot/data >/dev/null && wc -l *.csv; popd')"
# シェルに渡していない引用(コミットメッセージ等)は畳んだままにする = 誤 deny しない。
run 0 "commit body mentions bash -c"   "$(bj 'git commit -m \"docs: bash -c \\\"rm -rf backend/data\\\" も deny した\"')"
# T4 4巡目: 上のケースは早期素通しで抜けており、fold を丸ごと外しても結果が変わらない =
# **判別力が無い**(3種の hook で 0/0/0)。&& を付けて早期素通しを失格させると、
# 現行 0 / 常時 fold 0 / fold OFF 2 に分かれて NOFOLD の広がりすぎを検出できる。
run 0 "add && commit mentions eval rm" "$(bj 'git add -A && git commit -m \"fix(hooks): eval \\\"rm -rf ~/.stockbot/data\\\" が素通りしていた\"')"
run 0 "commit mentions sh -c rm"       "$(bj 'git add -A && git commit -m \"docs: sh -c '\''rm -rf backend/data'\'' を既知の穴に記録\"')"
# 絶対パスの削除は cwd に依存しない。フラグの有無で判定が変わってはいけない。
run 0 "cd data then rm -f abs path"    "$(bj 'cd ~/.stockbot/data && rm -f /tmp/scratch.json')"
run 0 "cd data then rm -rf abs path"   "$(bj 'cd ~/.stockbot/data && rm -rf /tmp/build')"
run 0 "cd backups then rm -f abs"      "$(bj 'cd ~/.stockbot/backups && rm -f /tmp/old.tar.gz')"
run 0 "cd data then mv -f abs"         "$(bj 'cd ~/.stockbot/data && mv -f /tmp/a /tmp/b')"
# シェル起動そのものは無害。中身が削除でなければ通す。
run 0 "bash -c go test"                "$(bj 'bash -c \"cd backend && go test ./...\"')"
run 0 "sh -c count csv"                "$(bj 'sh -c \"ls ~/.stockbot/data | wc -l\"')"
run 0 "eval direnv hook"               "$(bj 'eval \"$(direnv hook zsh)\"')"
# パイプの先がシェルでも、保護パスを含まなければ生判定を素通りする。
run 0 "pipe into sh unrelated"         "$(bj 'echo \"echo hi\" | sh')"
run 0 "count csv piped to wc"          "$(bj 'ls ~/.stockbot/data | wc -l')"
# find ルールを早期素通しの**前**に置く代償を argv[0] スコープで抑える。
# 言及しただけの grep / commit message は 2026-07-31 の誤 deny の再来なので必ず通す。
run 0 "grep mentions find -delete"     "$(bj 'grep -rn \"find -delete\" docs/')"
run 0 "commit msg mentions find -exec" "$(bj 'git commit -m \"docs(hooks): find -exec rm を deny した\"')"
run 0 "grep mentions rm ~/.stockbot"   "$(bj 'grep -rn \"rm -rf ~/.stockbot/data\" docs/')"

# T8 3巡目レビュー(2026-08-08): fold_quoted のエスケープ修正が **T4 を壊していた**(P0)。
# 引用の外の \; を生の ; として戻すと、下流の [^|;&] スパンがそれを**区切り**と誤読して
# マッチが死ぬ。bash は \; を rm の**引数**として渡すので backend/data は実際に消える
# (レビューが使い捨てツリーで実削除を確認)。エスケープされたメタ文字は不透明な 1 語に置く。
run 2 "rm -rf escaped semicolon"       "$(bj 'rm -rf \\; backend/data')"
run 2 "rm -rf escaped ampersand"       "$(bj 'rm -rf \\& backend/data')"
run 2 "rm -rf escaped pipe"            "$(bj 'rm -rf \\| backend/data')"
run 2 "mv escaped semicolon"           "$(bj 'mv \\; backend/data /tmp')"
run 2 "rm -f escaped semi stockbot"    "$(bj 'rm -f \\; ~/.stockbot/data/1234_daily.csv')"
run 2 "truncate escaped semi"          "$(bj 'truncate -s0 \\; backend/data/x.csv')"
run 2 "rsync escaped semi --delete"    "$(bj 'rsync \\; --delete ~/.stockbot/data /tmp')"
# エスケープ**されていない**綴りは従来どおり(エスケープ修正で閉じた側の回帰)。
run 2 "rm -rf escaped path"            "$(bj 'rm -rf \\backend/data')"
run 2 "rm -rf escaped tilde"           "$(bj 'rm -rf \\~/.stockbot')"
run 2 "rm -rf escaped dollar HOME"     "$(bj 'rm -rf \\$HOME/.stockbot')"
# R2: NOFOLDIN はシェルが**セグメント末尾に来る**ことを要求していたので、フラグ 1 個で外れた。
# 外れると code を運ぶ引用が畳まれて、中の削除・タグが消える。
run 2 "pipe rm stockbot to sh -s"      "$(bj 'echo \"rm -rf ~/.stockbot/data\" | sh -s')"
run 2 "pipe rm backend/data to bash -s" "$(bj 'echo \"rm -rf backend/data\" | bash -s')"
# T8 4巡目レビュー(2026-08-08): エスケープ側(Q5)と**同じ欠陥が引用側にもあった**(P0)。
# 空白を含まない引用は中身をそのまま出すので、`'a|b'` の | が生で sb_cmd に落ちて
# 下流の [^|;&] スパンを殺す。使い捨てツリーで backend/data の実削除を確認済み。
# HEAD からある穴で、T8 が作ったものではない。
run 2 "rm quoted pipe arg"             "$(bj 'rm -rf '\''a|b'\'' backend/data')"
run 2 "rm quoted semicolon arg"        "$(bj 'rm -rf '\'';'\'' backend/data')"
run 2 "rm dq pipe arg"                 "$(bj 'rm -rf \"a|b\" backend/data')"
run 2 "rm quoted amp stockbot"         "$(bj 'rm -rf '\''&'\'' ~/.stockbot')"
run 2 "mv quoted pipe arg"             "$(bj 'mv '\''|'\'' backend/data /tmp')"
# 日常の綴り(正規表現の交替・除外パターン)でそのまま起きる。
run 2 "sed -i quoted alternation"      "$(bj 'sed -i '\''s/a|b/c/'\'' backend/data/1234_daily.csv')"
run 2 "rsync exclude alternation"      "$(bj 'rsync -a --exclude='\''*|~'\'' --delete /tmp/ ~/.stockbot/data/')"
run 2 "find regex alternation delete"  "$(bj 'find backend/data -regex '\''a|b'\'' -delete')"
run 2 "test.run regex alternation"     "$(bj '/tmp/it -test.run \"TestFoo|TestIntegration\"')"
# 空白を含む引用は従来どおり畳まれる(= 保護対象を指さないので通る)。両分岐を必ず置く。
run 0 "rm quoted spaced alternation"   "$(bj 'rm -rf '\''a | b'\'' /tmp/x')"

# T13(2026-08-08): メタ文字が生で [^|;&] スパンに届く残り3経路。T8 起因ではなく
# HEAD からある穴で、T8 の 5〜6 巡目レビューが実測(実削除つき)で見つけた。
# 経路1: NOFOLD が畳み込みを丸ごと止めるので、その中の引用メタ文字が生で残る。
# 畳まない(コードを消さない)まま、メタ文字だけ不透明化する必要がある。
# 🛑 経路1 は「囲っている argv[0] がシェルか」で code / literal を決める形で塞いだ。
# **深さでは決まらない** — 深さで索く実装は 2 度とも P0 回帰を出した(1段=45件 / 2段=81件)。
# code 側は**引用符ごと再帰して残す**ので、ネストが無い入力に対しては生の $cmd と 1 バイトも
# 変わらない = 45/81 件の回帰が原理的に起きない形にしてある。
run 2 "経路1 bash -c rm quoted pipe"  "$(bj 'bash -c \"rm -rf '\''a|b'\'' backend/data\"')"
run 2 "経路1 sh -c rm quoted semi"    "$(bj 'sh -c \"rm -rf '\'';'\'' backend/data\"')"
run 2 "経路1 eval rm quoted pipe"     "$(bj 'eval \"rm -rf '\''a|b'\'' backend/data\"')"
run 2 "経路1 bash -c rm quoted sb"    "$(bj 'bash -c \"rm -rf '\''a|b'\'' ~/.stockbot/data\"')"
run 2 "経路1 pipe sh rm quoted pipe"  "$(bj 'echo \"rm -rf '\''a|b'\'' backend/data\" | sh')"
run 2 "経路1 sh <<< quoted pipe"      "$(bj 'sh <<< \"rm -rf '\''a|b'\'' backend/data\"')"
# 🛑 **深さ 2 以上**。テストがここを 1 件も見ていなかったのが、591 全緑のまま
# 45 件 / 81 件の回帰を見逃した原因(md の T13)。code はどれだけ深くても code。
run 2 "経路1 深さ2 bash>sh rm"        "$(bj 'bash -c \"sh -c \\\"rm -rf backend/data\\\"\"')"
run 2 "経路1 深さ2 eval>sh rm"        "$(bj 'eval \"sh -c \\\"rm -rf ~/.stockbot/data\\\"\"')"
run 2 "経路1 深さ2 の中の引用メタ"    "$(bj 'bash -c \"sh -c \\\"rm -rf '\''a|b'\'' backend/data\\\"\"')"
# 逆側: **内側シェルの本物の演算子は演算子のまま残す**(1段モデルが潰した 45 件の型)。
run 2 "経路1 sh -c の中の live 書込"  "$(bj 'sh -c \"psql -d stockbot < patch.sql\"')"
run 2 "経路1 深さ2 の中の live 書込"  "$(bj 'bash -c \"sh -c \\\"psql -d stockbot < patch.sql\\\"\"')"
run 2 "経路1 sh -c の中の CSV 上書き" "$(bj 'sh -c \"echo x > backend/data/1234_daily.csv\"')"
# 逆側: シェルのコード位置に**無い**引用は従来どおり畳む(言及で誤 deny しない)。
run 0 "経路1 言及は畳まれたまま"      "$(bj 'git add -A && git commit -m \"docs: bash -c rm -rf backend/data の穴\"')"
# 🛑 **改行の後ろのシェル起動**。`[[ =~ ]]` の `^` は文字列先頭だけなので、アンカークラスに
# 改行を入れ忘れると 2 行目以降の `bash -c` が literal 扱いになり本文が丸ごと消える
# (レビュー実測で 13 件の P0・emergency_stop の削除まで通っていた)。
# grep 用と `[[ =~ ]]` 用で改行の扱いが違うという §2.1 の罠そのもの。
run 2 "経路1 改行の後ろの bash -c"    "$(bj 'ls -la\nbash -c \"rm -rf backend/data\"')"
run 2 "経路1 改行の後ろの emergency"  "$(bj 'ls -la\nbash -c \"rm runtime/emergency_stop\"')"
run 2 "経路1 code 本文の中の改行"     "$(bj 'bash -c \"ls\nsh -c \\\"rm -rf backend/data\\\"\"')"
run 2 "経路1 改行の後ろの eval"       "$(bj 'ls -la\neval \"rm -rf ~/.stockbot/data\"')"
# SBKEEPAT(フラグの値)は**深さに依らず**効くこと。旧実装は深さ 0 の sed だったので
# 入れ子で壊れていた(全件差分で実測)。空白を含む値でないとこの経路を踏めない。
run 2 "経路1 深さ2 の -tags 空白値"   "$(bj 'bash -c \"sh -c \\\"go test -tags '\''integration slow'\'' ./...\\\"\"')"
run 2 "経路1 深さ2 の GOFLAGS 空白値" "$(bj 'bash -c \"sh -c \\\"GOFLAGS='\''-tags=integration -v'\'' go test ./...\\\"\"')"
# ★ **隣接引用の連結は未解決**(HEAD からある穴で、T13 経路1 が作ったものではない。
# certified main も同じく exit 0)。`'\''` / `'"'"'` は bash の標準的な書き方で、
# **1 語**が「引用片 + エスケープ + 引用片」の連結になる。今の走査は引用を 1 つずつ見るので、
# 2 つ目以降の断片は「直前の綴りが `-c` で終わっていない」ため literal 扱いになり、
# 末尾の ` backend/data` が空白を含む引用として Q に消える。
# 🛑 **正しい設計**: code 位置の引用に当たったら、そこから**語の終わりまで**(空白 / 引用の
# 外のメタ文字まで)を 1 語として**値を組み立て**、その値に対して 1 回だけ再帰する。
# 引用を 1 つずつ処理する限りこの形は閉じない。**exit 0 は「正しい」ではなく「まだ塞げていない」。**
run 0 "★未解決 隣接引用の連結(dq経由)"  "$(bj 'bash -c '\''rm -rf '\''\"'\''\"'\''a|b'\''\"'\''\"'\'' backend/data'\''')"
# NOFOLD の本来の目的(コードを運ぶ引用を畳まない)は保つこと。
run 2 "bash -c rm stockbot plain"      "$(bj 'bash -c \"rm -rf ~/.stockbot/data\"')"
run 2 "bash -c make stop"              "$(bj 'bash -c \"make stop\"')"
run 0 "bash -c go test"                "$(bj 'bash -c \"cd backend && go test ./...\"')"
# T13 レビュー(2026-08-08): meta-only モードが**2つの P0 回帰**を作っていた。
# 契約は「生の $cmd との差はメタ文字が Q になることだけ」だったが、実際は
# (a) 二重引用の中の \X を**捨てて**いた(st=2 の \ 分岐を継承)。\$HOME が HOME になり
# SBROOT が当たらない。実削除を確認。
run 2 "bash -c rm escaped HOME"        "$(bj 'bash -c \"rm -rf \\$HOME/.stockbot\"')"
run 2 "bash -c rm escaped HOME data"   "$(bj 'bash -c \"rm -rf \\$HOME/.stockbot/data\"')"
run 2 "sh -c rm escaped braces HOME"   "$(bj 'sh -c \"rm -rf \\${HOME}/.stockbot\"')"
run 2 "eval rm escaped HOME"           "$(bj 'eval \"rm -rf \\$HOME/.stockbot\"')"
run 2 "rm quoted escaped HOME"         "$(bj 'rm -rf \"\\$HOME/.stockbot\"')"
# (b) NOFOLD 本文の中の < > | は**内側シェルの本物の演算子**なのに Q に潰していた。
# 指標そのものが消えて、live DB の SQL ファイル実行や CSV 切り詰めが素通りした。
run 2 "sh -c psql redirect in"         "$(bj 'sh -c \"psql -d stockbot < patch.sql\"')"
run 2 "bash -c psql redirect in"       "$(bj 'bash -c \"psql -d stockbot < patch.sql\"')"
run 2 "eval psql redirect in"          "$(bj 'eval \"psql -d stockbot < patch.sql\"')"
run 2 "sh -c cat pipe psql"            "$(bj 'sh -c \"cat p.sql | psql -d stockbot\"')"
run 2 "sh -c truncate daily csv"       "$(bj 'sh -c \"echo x > backend/data/1234_daily.csv\"')"
run 2 "sh -c append stockbot plist"    "$(bj 'sh -c \"echo x >> ~/Library/LaunchAgents/com.stockbot.daily.plist\"')"
# 経路2: 生の $cmd を見るルールが [^|;&] スパンを持ったまま。
# emergency_stop は CLAUDE.md の write-once 不変条件(実削除を確認済み)。
run 2 "rm quoted pipe emergency_stop"  "$(bj 'rm '\''a|b'\'' runtime/emergency_stop')"
run 2 "rm escaped pipe emergency_stop" "$(bj 'rm \\| runtime/emergency_stop')"
run 2 "pg_ctl quoted pipe stop"        "$(bj 'pg_ctl '\''a|b'\'' -D /data stop')"
run 2 "launchctl quoted pipe stockbot" "$(bj 'launchctl bootout '\''a|b'\'' com.stockbot.daily')"
run 2 "rm quoted pipe stockbot plist"  "$(bj 'rm '\''a|b'\'' ~/Library/LaunchAgents/com.stockbot.daily.plist')"
run 2 "psql quoted pipe -f"            "$(bj 'psql '\''a|b'\'' -d stockbot -f patch.sql')"
run 2 "pkill quoted pipe postgres"     "$(bj 'pkill '\''a|b'\'' -f postgres')"
# psql の書込指標は SQL 本文(= 引用の中の実行されるコード)を見るので生の $cmd のまま。
run 2 "psql DELETE stays denied"       "$(bj 'psql stockbot -c \\"DELETE FROM trades\\"')"
run 2 "psql UPDATE stays denied"       "$(bj 'psql -d stockbot -c \\"UPDATE positions SET status=1\\"')"
run 0 "psql SELECT stays allowed"      "$(bj 'psql stockbot -c \\"SELECT count(*) FROM trades\\"')"
run 0 "psql _test write stays allowed" "$(bj 'psql stockbot_test -c \\"INSERT INTO trades VALUES (1)\\"')"
# 経路3: 8000字を超えると畳み込みが無効になり、エスケープ修正も引用修正も**両方**外れる。
# 末尾に長いコメントを足すだけでよい(bash はコメントを捨てるので素のコマンドと等価)。
# 実削除を確認済み。上限超過時に fail-open しない形にする。
LONGCM="$(printf 'a%.0s' {1..8100})"
run 2 "quoted pipe + long comment"     "$(bj "rm -rf 'a|b' backend/data # ${LONGCM}")"
run 2 "quoted pipe stockbot + long"    "$(bj "rm -rf 'a|b' \$HOME/.stockbot/data # ${LONGCM}")"
run 2 "quoted pipe emergency + long"   "$(bj "rm 'a|b' runtime/emergency_stop # ${LONGCM}")"
# 長いだけで保護対象に触れないコマンドは通す(上限超過を一律 deny にはしない)。
run 0 "long harmless command"          "$(bj "cd backend && go build ./internal/${LONGCM}")"
run 0 "long quoted harmless command"   "$(bj "grep -rn 'x|y' ./internal/${LONGCM}")"
# 引用の中のメタ文字は kill / pgrep 経路でもスパンを切る(長さとは無関係の T13 経路2)。
run 2 "kill pgrep quoted pipe"         "$(bj "kill \$(pgrep 'a|b' stockbot)")"
run 2 "long kill pgrep stockbot"       "$(bj "kill \$(pgrep 'a|b' stockbot) # ${LONGCM}")"
# 上限(SBMAXLEN)を**超える**帯だけが fail-close の担当。超えなければ普通に畳んで判定する。
HUGECM="$(printf 'a%.0s' {1..17000})"
run 2 "over-limit quoted destructive"  "$(bj "rm -rf 'a|b' backend/data # ${HUGECM}")"
run 2 "over-limit quoted emergency"    "$(bj "rm 'a|b' runtime/emergency_stop # ${HUGECM}")"
# 引用の | が plist ルールのスパンを切るので、ここを止めるのは上限超過帯の語彙(語の stockbot)だけ。
run 2 "over-limit quoted plist write"  "$(bj "plutil -replace Disabled -bool true 'a|b' /Users/USERNAME/Library/LaunchAgents/com.stockbot.morning.plist # ${HUGECM}")"
run 0 "over-limit harmless"            "$(bj "cd backend && go build ./internal/${HUGECM}")"
# ★ 上限超過帯では「言及」と「実行」を区別できない(引用が解析できない)ので fail-close する。
# 受け入れた誤 deny。回避策はコマンドを短く分けること。上限以下なら畳めるので通る。
run 2 "over-limit grep mention"        "$(bj "grep -rn 'make stop' docs/${HUGECM}")"
run 0 "under-limit grep mention"       "$(bj "grep -rn 'make stop' docs/${LONGCM}")"
# 逆に、長いだけの**言及**(read / 記録系)まで deny していた。2026-07-31 の誤 deny の再来。
run 0 "long grep mentions make stop"   "$(bj "grep -rn 'make stop' docs/${LONGCM}")"
run 0 "long grep mentions backend/data" "$(bj "grep -rn \\"backend/data\\" docs/${LONGCM}")"
run 0 "long commit mentions data"      "$(bj "git commit -m \\"docs: backend/data の説明 ${LONGCM}\\"")"

# T8 ALLOW: 実行を伴わない 2 経路と、タグに言及しただけの記録系。
# go vet はコンパイル検査のみで TestMain を走らせない = DB を触らない。
run 0 "go vet -tags integration"       "$(bj 'cd backend && go vet -tags integration ./...')"
run 0 "make vet-integration"           "$(bj 'make vet-integration')"
run 0 "make test-integration flags"    "$(bj 'make test-integration ARGS=-run=TestTrades')"
# 早期素通しが効かない形(&& 付き)で「言及しただけ」を通す。2026-07-31 の誤 deny の再来防止。
# ★ 現行実装はここで exit 2 になる(生の $cmd を見ているため)。fold で閉じる。
run 0 "commit msg mentions tags"       "$(bj 'git add -A && git commit -m \"docs: go test -tags integration を deny した\"')"
# ★ 受け入れた誤 deny。fold_quoted は**空白を含む**引用領域しか畳まないので(`rm -rf
# "$HOME/.stockbot"` を捕まえるための契約)、空白の無い引用 "-tags=integration" は素通しできない。
# 単一行なら早期素通しで通る(下のケース)。&& を足した形だけが刺さる。
# 回避策: && を外して 2 コマンドに分ける。fail-close 側に倒す判断(HARNESS_PITFALLS.md §3)。
run 2 "grep quoted tag token && echo"  "$(bj 'grep -rn \"-tags=integration\" docs/ && echo done')"
run 0 "grep quoted tag token (単一行)" "$(bj 'grep -rn \"-tags=integration\" docs/')"
# integration という語だけでは発火しない(テスト名・ディレクトリ名は日常語)。
run 0 "plain test named integration"   "$(bj 'cd backend && go test ./internal/adapter/repository -run TestIntegrationHelpers')"
run 0 "ls integration dir"             "$(bj 'ls -la backend/internal/integration && echo ok')"
# ★ タグ名は完全一致で見る。部分一致だと**別のタグ**まで巻き込む(実測 exit 2)。
# 誤検知するガードは必ず無視されるようになる(md §A2)ので、ここは狭める側に倒す。
run 0 "different tag: integrationless" "$(bj 'go build -tags integrationless ./...')"
run 0 "different tag: nointegration"   "$(bj 'cd backend && go test -tags nointegration ./...')"
run 0 "vet with comma-joined tags"     "$(bj 'cd backend && go vet -tags=unit,integration ./...')"
# ★ G1(レビュー指摘): 許すのは「実行を伴わない経路」であって `go vet` という綴りではない。
# go list / go build / go doc / linter はテストを 1 つも走らせないのに deny していた。
# `go list -tags integration`(何も動かさない)が deny で、`go test … # go vet`(全部動く)が
# allow という逆転が起きていた。**go run / go generate はコードを実行するので carve-out に入れない。**
run 0 "go build with tag"              "$(bj 'cd backend && go build -tags integration ./...')"
run 0 "go list with tag"               "$(bj 'cd backend && go list -tags integration ./...')"
run 0 "go doc with tag"                "$(bj 'cd backend && go doc -tags integration ./internal/adapter/repository/pg')"
run 0 "staticcheck with tag"           "$(bj 'cd backend && staticcheck -tags integration ./...')"
run 2 "go run with tag"                "$(bj 'cd backend && go run -tags integration ./cmd/stockbot')"
# 保護対象でない .test 以外のバイナリは対象外(argv[0] スコープの確認)。
run 0 "cat a file named .test"         "$(bj 'cat backend/fixtures/golden.test && echo ok')"
run 0 "ls test binaries"               "$(bj 'ls -la backend/*.test && echo ok')"
run 0 "rm a stale test binary"         "$(bj 'rm -f backend/repository.test && echo ok')"
run 0 "redirect output to .test file"  "$(bj 'cd backend && go test ./... > /tmp/out.test')"
# Gap 2: TAGOK の各枝を両方向で担保する(5枝が 1 ケースも通っていなかった)。
run 0 "go fmt with tag"                "$(bj 'cd backend && go fmt -tags integration ./...')"
run 0 "go env with tag"                "$(bj 'go env -tags integration')"
run 0 "go version with tag"            "$(bj 'go version -tags integration')"
run 0 "golangci-lint with tag"         "$(bj 'cd backend && golangci-lint run -tags integration ./...')"
run 0 "revive with tag"                "$(bj 'cd backend && revive -tags integration ./...')"

# ★ T14(2026-09-05 監査): 制御 API の変更系エンドポイント。HTTP 層は同一マシンなら無認証で、
# 「解除は人間の POST のみ」を機械強制していなかった(curl 6 形が全部 exit 0 だった)。
run 2 "T14 curl POST emergency-resume"        "$(bj 'curl -s -X POST http://127.0.0.1:8090/api/emergency-resume')"
run 2 "T14 curl POST live emergency-resume"   "$(bj 'curl -X POST http://127.0.0.1:8090/api/live/emergency-resume')"
run 2 "T14 curl POST flatten-all (host only)" "$(bj 'curl -s -X POST localhost:8090/api/flatten-all')"
run 2 "T14 curl POST protective replace"      "$(bj 'curl -s -X POST http://127.0.0.1:8090/api/live/protective/replace -d position_id=1')"
run 2 "T14 curl POST protective arm"          "$(bj 'curl -X POST http://127.0.0.1:8090/api/live/protective/arm -d position_id=1')"
run 2 "T14 curl POST live positions close"    "$(bj 'curl -X POST http://127.0.0.1:8090/api/live/positions/close -d position_id=1')"
run 2 "T14 curl POST positions extend"        "$(bj 'curl -X POST http://127.0.0.1:8090/api/positions/extend -d position_id=1')"
run 2 "T14 curl POST advisor-trigger"         "$(bj 'curl -X POST http://127.0.0.1:8090/api/advisor-trigger')"
run 2 "T14 curl quoted URL"                   "$(bj 'curl -X POST \"http://127.0.0.1:8090/api/emergency-resume\"')"
run 2 "T14 curl after && "                    "$(bj 'make check-backend && curl -X POST http://127.0.0.1:8090/api/emergency-resume')"
run 2 "T14 wget post-data"                    "$(bj 'wget -q --post-data= -O- http://127.0.0.1:8090/api/emergency-resume')"
run 2 "T14 bash -c curl POST"                 "$(bj 'bash -c \"curl -X POST http://127.0.0.1:8090/api/emergency-resume\"')"
run 2 "T14 env-prefixed curl"                 "$(bj 'env FOO=1 curl -X POST http://127.0.0.1:8090/api/emergency-resume')"
run 2 "T14 python -c URL as argv"             "$(bj 'python3 -c \"import urllib.request as u,sys; u.urlopen(u.Request(sys.argv[1], data=bytes()))\" http://127.0.0.1:8090/api/emergency-resume')"
run 2 "T14 python -c URL in code"             "$(bj 'python3 -c \"import urllib.request as u; u.urlopen(u.Request(\\\"http://127.0.0.1:8090/api/emergency-resume\\\", data=bytes()))\"')"
run 2 "T14 node -e fetch"                     "$(bj 'node -e \"fetch(process.argv[1],{method:process.argv[2]})\" http://127.0.0.1:8090/api/flatten-all POST')"
# 読み取り・止める向き・言及は通す。
run 0 "T14 curl GET status"                   "$(bj 'curl -s http://127.0.0.1:8090/api/status')"
run 0 "T14 curl GET live dashboard | head"    "$(bj 'curl -s http://127.0.0.1:8090/api/live/dashboard | head -c 400')"
run 0 "T14 curl GET positions"                "$(bj 'curl -s http://127.0.0.1:8090/api/live/positions')"
run 0 "T14 curl healthz"                      "$(bj 'curl -sf http://127.0.0.1:8090/healthz')"
run 0 "T14 curl POST emergency-stop (止める向き)" "$(bj 'curl -X POST http://127.0.0.1:8090/api/emergency-stop')"
run 0 "T14 curl POST live emergency-stop"     "$(bj 'curl -X POST http://127.0.0.1:8090/api/live/emergency-stop')"
run 0 "T14 grep mention (bare)"               "$(bj 'grep -rn emergency-resume backend/internal/app/handler/ | head')"
run 0 "T14 grep mention (full path)"          "$(bj 'grep -rn \"/api/emergency-resume\" backend/ | head')"
run 0 "T14 commit message mention"            "$(bj 'git add -A && git commit -m \"docs: POST /api/emergency-resume は人間専用\"')"
run 0 "T14 echo mention with redirect"        "$(bj 'echo \"see http://127.0.0.1:8090/api/flatten-all\" >> /tmp/notes.md')"
run 0 "T14 python script unrelated"           "$(bj 'python3 scripts/merge_daily_from_json.py /tmp/x.json backend/data')"
run 0 "T14 curl unrelated host"               "$(bj 'curl -s -X POST https://example.com/api/flatten-all-stats')"

# H6(2026-10-01): 読み取り・言及の誤 deny。DROP / TRUNCATE / docker compose / psql 分岐は、
# DB に届くコマンド(psql / pg_restore / docker / インタプリタ)が**セグメントの argv[0]** に
# あるときだけ見る(T14 の CTLPRE と同じ方式)。下の deny 側は「言及の誤 deny を直した結果、
# 実行の deny まで外れていないこと」を拘束する。
run 0 "H6 git config hooksPath を読む"       "$(bj 'git config core.hooksPath')"
run 0 "H6 commit message に truncate/DROP"   "$(bj 'git add -A && git commit -m \"docs: truncate the log と DROP TABLE の説明\"')"
run 0 "H6 grep truncate | head"              "$(bj 'grep -rn \"truncate the\" docs | head')"
run 0 "H6 compose logs | grep stop"          "$(bj 'docker compose logs db | grep -i stop')"
run 0 "H6 psql select の文字列に delete"     "$(bj 'psql -d stockbot -c \"select count(*) from audit where action='\''delete'\''\"')"
run 0 "H6 grep psql と UPDATE | head"        "$(bj 'grep -n '\''curl\\|psql\\|UPDATE'\'' scripts/db-backup.sh | head')"
run 0 "H6 SQL をファイルに書くだけ"          "$(bj 'echo \"DROP TABLE trades\" > patch.sql')"
run 2 "H6 echo DROP | psql"                  "$(bj 'echo \"DROP TABLE x\" | psql stockbot')"
run 2 "H6 compose exec psql TRUNCATE"        "$(bj 'docker compose exec db psql -U x -c \"TRUNCATE trades\"')"
run 2 "H6 compose exec psql UPDATE"          "$(bj 'docker compose exec db psql -U x -d stockbot -c \"UPDATE t SET a=1\"')"
run 2 "H6 bash -c psql DROP"                 "$(bj 'bash -c \"psql -c '\''DROP TABLE t'\''\"')"
run 2 "H6 python -c DROP"                    "$(bj 'python3 -c \"import psycopg; c.execute('\''DROP TABLE trades'\'')\"')"
run 2 "H6 sudo compose down"                 "$(bj 'sudo docker compose down')"
run 2 "H6 cd してから compose down"          "$(bj 'cd infra && docker compose down')"
run 2 "H6 compose logs の後の down"          "$(bj 'docker compose logs db && docker compose down')"
run 2 "H6 docker stop"                       "$(bj 'docker stop pg')"
run 2 "H6 psql 単一引用の DELETE"            "$(bj 'psql -d stockbot -c '\''DELETE FROM trades'\''')"
run 2 "H6 文字列に delete があっても DELETE" "$(bj 'psql -d stockbot -c \"DELETE FROM t WHERE a='\''delete'\''\"')"
run 2 "H6 grep psql の後ろの psql UPDATE"    "$(bj 'grep -n psql x | psql -d stockbot -c \"UPDATE t SET a=1\"')"
run 2 "H6 hooksPath を空にする"              "$(bj 'git config core.hooksPath \"\"')"
run 2 "H6 hooksPath を --unset"              "$(bj 'git config --unset core.hooksPath')"

# argv[0] でない docker の言及(引用されていない語)。畳まれないので argv[0] の条件だけが区別する。
run 0 "H6 echo で compose の言及"            "$(bj 'echo docker compose down は人間が打つ | head')"
run 0 "H6 echo で docker stop の言及"        "$(bj 'echo docker stop は人間が打つ | head')"
# psql 分岐の入口(argv[0])だけが区別する言及。UPDATE の後ろが空白なので動詞の条件では通らない。
run 0 "H6 grep で psql と UPDATE を言及"     "$(bj 'grep -rn psql docs | grep UPDATE | head')"
# 動詞の直後で除くのは `'` だけ。空白の代わりに SQL コメントを挟んでも文は成立する。
run 2 "H6 DELETE の後ろが SQL コメント"      "$(bj 'psql -d stockbot -c \"DELETE/**/FROM trades\"')"

# H6 の死文: 撤去済みのものを根拠や対象に書き続けない(読む人が実在すると思って探す)。
dead_text_rc=0
grep -qE 'harvest/|live-extend-holds' "$HOOK" && dead_text_rc=1
if [ "$dead_text_rc" -eq 0 ]; then pass=$((pass + 1))
else fail=$((fail + 1)); echo "FAIL [H6 撤去済みの harvest / live-extend-holds を hook に書かない]"; fi

echo ""
echo "pretooluse-deny_test: pass=${pass} fail=${fail}"
[ "$fail" -eq 0 ] || exit 1
