# ハーネス有効化手順(HARNESS_SETUP)

stock-bot の規律(層規約 / migration / secret / docs 同期 / テストの存在)を**機械的に**
効かせるためのセットアップ。リポジトリで**一度だけ**実施する。

ハーネスは 2 層(CI は無い — §3):

| 層 | 何が走るか | 有効化 |
|---|---|---|
| **Claude Code hooks** | SessionStart(session-start)+ PreToolUse deny(Bash)+ enforcement-guard(Edit/Write)+ Stop(pre-stop-checks + docs-sync) | `.claude/settings.json` コミット済み |
| **git hooks** | pre-commit(secret)/ pre-push(毎回 secret-scan + vet-integration + check-backend + fmt-check + guard-fast。hook の自己テストは条件つき — §2) | `git config core.hooksPath .githooks` |


---

## 1. Claude Code hooks(`.claude/settings.json`)

`.claude/settings.json` は **enforcement / 権限ファイル**で、AI は自己改変しない
(hook や permission の自己書き換えになるため)。**リポジトリにコミット済み**なので clone すれば配線済みで、
新しい Claude Code セッションから自動で有効になる。変更は操作者が
`STOCKBOT_HARNESS_EDIT_APPROVED=1 claude` で起動した承認セッションで行う(下の `enforcement-guard.sh`)。

> `defaultMode` は意図的に設定していない(既存の権限モードを変えない)。`permissions.allow` は
> 開発でよく使う read-only コマンドのプロンプトを減らすだけで、deny hook を弱めない。
>
> **`~/.stockbot` は `additionalDirectories` ではなく `Read(…/**)` で入れてある。**
> `additionalDirectories` は読書とも摩擦ゼロにするので、**Write ツールで
> `~/.stockbot/data/*.csv` を直接上書きできる**ようになる — `pretooluse-deny.sh` が
> Bash 経由の削除・切り詰めを塞いでいても、Edit/Write 経路から **Bash 側の保護を丸ごと迂回できる**。
> 日足 CSV は git 管理外 = pg_dump の対象外で**唯一のコピー**なので、読み取りだけを許す形に留める。
>
> **`Read` だけでは足りない。** auto mode(permission prompt 無し)では
> Write ツールが `~/.stockbot/data/*.csv` / `~/.stockbot/env` / `~/.stockbot/stockbot-routine.sh` /
> `~/Library/LaunchAgents/com.stockbot.*.plist` へそのまま書ける(`enforcement-guard.sh` はリポジトリ外を
> 対象にしない・実測 ALLOW)。launchd が実行するスクリプトと plist を書き換えられる = hook を経由しない
> コード実行経路なので、`permissions.deny` に `Write` / `Edit` の `~/.stockbot/**` と
> `~/Library/LaunchAgents/**` を明示した。**deny は bypass / auto mode でも効く**(allow と違い prompt の
> 省略ではなく拒否)。Bash 経路の削除・切り詰めは従来どおり `pretooluse-deny.sh`。

現行の中身は [`.claude/settings.json`](../../.claude/settings.json) を読む(ここに写しを置くと drift する)。構成:

| キー | 中身 |
|---|---|
| `permissions.allow` | よく使う read-only コマンド(`make check` / `make guard*` / 各自己テスト等)と `Read(~/.stockbot/**)`。プロンプトを減らすだけで deny hook を弱めない |
| `permissions.deny` | `Write` / `Edit` の `~/.stockbot/**` と `~/Library/LaunchAgents/**` |
| `hooks.SessionStart` | `session-start.sh` |
| `hooks.PreToolUse` | Bash → `pretooluse-deny.sh` / Edit・Write・NotebookEdit → `enforcement-guard.sh` |
| `hooks.Stop` | `pre-stop-checks.sh` → `docs-sync-check.sh`(この順。Stop 側の印の扱いはこの順序に依存する) |

`env` は置かない。検査の既定 on は hook 側の `${…:-on}` が持つ(`STOCKBOT_PRESTOP_CHECKS=off claude` の
ような 1 セッションの無効化が settings に上書きされない)。

### hook の挙動

- **`session-start.sh`**(SessionStart)— セッション開始時に「現在地」を context へ注入する。
  出力は上限 40 行: `docs/runtime/STATUS.md`(運用メモ。このリポジトリには含まれない)があれば先頭を表示する(無ければ省略)/ bot の稼働(`lsof` + `STOCKBOT_HTTP_ADDR`)/
  emergency_stop フラグ(research と live。live のパスは `.env` の `STOCKBOT_LIVE_EMERGENCY_FLAG`
  から取り、値は出さない)/ `~/.stockbot/data` の最新 `*_daily.csv` の mtime / `git log --oneline -3`。
  **最上位の契約は「セッション起動を絶対にブロックしない」**なので `-e` を付けず全て `|| true`、
  どの分岐でも exit 0。ネットワークには一切出ない(自己テスト t34 が実行時間の上限を見る)。
  - 副作用は 2 つだけ: `runtime/logs/session-start-sha.<session_id>` と
    `session-start-mark.<session_id>`(中身は開始 SHA。下記)を置く。
    **既にあれば上書きしない**(SessionStart は compact / resume でも再発火するため)。
    **空ファイルは置かない** — Stop hook は `[ -f ]` で存在だけを見るので、空を置くと
    セッション範囲が永久に空のまま固定される。
  - 印は**形を検証してから置く** — commit ゼロのリポでは `git rev-parse HEAD` が rc=128 なのに
    stdout へ文字列 `HEAD` を出し、読み手の `git cat-file -e` はそれに成功してしまう(fail-open)。
  - **副作用として Stop hook の検査範囲がセッション全体に広がる**(従来は「最初の Stop 時点の
    HEAD」)。厳しくなる側。
  - **`session-start-mark.<sid>`(中身は開始 SHA)も置く。**Stop hook 側は
    `session-start-sha` の**有無**では「SessionStart が動いたか」を判定できない —
    Stop 配列は `pre-stop-checks.sh` が先で、そちらが同じ名前を backfill するため、
    `docs-sync-check.sh` から見ると常に「既にある」に見える(fail-OPEN)。
    **空ファイルの別名でも足りない**(backfill された sha を開始点と誤読して 2 本が割れる。
    実測 pre-stop rc=2 / docs-sync rc=0)。**印の中身に SHA を持たせ**、Stop 側は
    印があれば印から開始点を読む。sha を実際に書けたときだけ置く。
  - 残した穴: `.env` は読まないので `STOCKBOT_HTTP_ADDR` を `.env` だけで変えている場合は
    既定の 8090 を見る / **busybox の `lsof` は引数を無視して rc=0** なので Alpine では
    何も listen していなくても `bot: RUNNING` になる(macOS 実機では起きない) /
    `data:` 行は `*_daily.csv` の **max(mtime)** であってデータセット全体の鮮度監視ではない
    (`bench_topix.csv` の古さや売買停止銘柄はこの行では見えない)。

- **`pretooluse-deny.sh`**(PreToolUse:Bash)— 破壊的コマンドを `exit 2` で deny。
  **integration タグ付きテストの実行** — 判定は「どう呼ばれたか」ではなく**タグそのもの**
  (`-tags …integration` / `GOFLAGS=…integration` / ビルド済みテストバイナリの直接実行)。
  許可はセグメント単位で、**実行を伴わない invocation** だけ
  (`go vet|build|list|doc|fmt` と `staticcheck` 等。`go run` / `go generate` は不可)。
  `make test-integration` は `-tags` を含まないので元から対象外(Makefile 側が `_test` DSN を強制)/
  `docker (compose|container) down|stop|kill|rm`・`docker volume rm|prune`・`docker system prune` / `rm -rf *docker-data` /
  `DROP`/`TRUNCATE`/`dropdb`/`pg_restore --clean` / postgres stop(pg_ctl/pkill/brew services)/
  live への raw psql・pg_restore 書込(書込動詞 + `-f` + COPY FROM)/
  ※ docker・`DROP`/`TRUNCATE`・psql 書込は、docker / psql 系 / インタプリタが**セグメントの argv[0]** に
  あるときだけ見る(commit message や `grep … | head` の言及は通す。SQL の文字列リテラル `'delete'` も
  書込動詞と読まない)/
  **`~/.stockbot`(= `backend/data` の実体)の削除・移動・切り詰め** — 日足 CSV は git 管理外 =
  pg_dump の対象外で**唯一のコピー**。`cd` してからの相対パス削除・`xargs` 越し・`rsync --delete`・
  `find -delete` / `-exec rm` も同じ結果になるので同時に塞ぐ /
  **`make stop` / `reset-trades` / `migrate-down` / `migrate-live-up` / `migrate-live-down` / `restore-drill`**(bot 停止・forward 台帳の消去・
  DB rollback・live DB の migration。hook からは Makefile の中身が見えないのでターゲット名を名指しする)/
  **`com.stockbot.*` の launchd ジョブ操作と plist の削除・書換**(日足更新・backup の停止は人間の判断)/
  **制御 API の変更系エンドポイント**: `curl` / `wget` / `xh` が argv[0] で同じ
  セグメントに `/api/(live/)?(emergency-resume|flatten-all|positions/(close|extend)|protective/*|advisor-trigger)`
  があるとき、またはインタプリタ(`python -c` 等)が argv[0] で生のコマンドにそのパスがあるとき deny。
  HTTP 層は loopback の同一ユーザーなら無認証なので、「解除は人間の POST のみ」(CLAUDE.md)は
  これが入るまで文章だけだった(curl 6 形が全部 exit 0)。**読み取り(GET)と止める向きの
  `emergency-stop` は通す**。運用スクリプトが中で curl する形は見えない(2 段の既知の限界)。
  Override は `STOCKBOT_HUMAN_APPROVED_DB_WRITE=1` 前置 or **接続先 DB 名が `_test` で終わる**とき
  (破壊系はどちらでも貫通不可)。自己テスト: `bash .claude/hooks/pretooluse-deny_test.sh`。
  - `_test` の判定は **argv を順に歩いて接続先を全て解決し、その全部が `_test` で終わるとき**
    だけ成立する(`-d` / `--dbname` / `dbname=` / `postgres://` / psql の第1位置引数)。
    正規表現で「`_test` を含む形」を潰す方式は回避形が尽きず捨てた。
  - **多行・引用の外の `; & |` ・`$( )` / backtick を含むコマンドは override 不可**
    (前半が `_test` DSN でも後半の live 書込に効いてしまうため)。分けて実行する。
    4000 文字を超えるコマンドも override 不可(走査コストのため fail-close)。
  - **照会は通す**: `launchctl list / print / blame`、`plutil -p / -lint`、`defaults read`。
    止める・書き換える動詞だけを deny する。
  - 早期素通し(閲覧・記録コマンドの誤 deny 回避)の判定は**引用状態を走査**して行う。
    引用の中の改行・`| ; & < >` は区切りではない。`has_unquoted()` 参照。
- **`stop-hook-lib.sh`**(Stop 系 2 本が source する共有ライブラリ)—
  「このセッションはどこから始まったか」の解決だけを持つ。`hooklib_git`(`core.quotePath=false`)/
  `hooklib_parse_stop_stdin`(session_id・jq フォールバック込み)/ `hooklib_resolve_session_scope`
  (SessionStart の印の形検証・backfill・`prev_commit_in_scope`)/ `hooklib_first_stop`。
  **なぜ 1 か所か**: 抽出前は「直前コミットを範囲に入れるか」の述語が **3 か所・2 綴り**、
  SHA の形検証が **4 か所**にコピーで存在し、**1 か所忘れれば fail-OPEN** だった。`block_stop` / log / block カウンタは
  **入れない**(メッセージも印の名前も 2 本で違い、共有すると「どちらの hook が止めたか」が
  stderr から消える)。`session-start.sh` も載せない — あちらは「どの分岐でも exit 0」で契約が逆。
  呼び出し側は必ず 3 段で守る(`[ -f ]` → `set +e` で source して rc → 代表関数の `command -v`)。
  自己テストは `pre-stop-tdd_test.sh` の e21a〜f と `docs-sync-check_test.sh` の #5 群。

- **`enforcement-guard.sh`**(PreToolUse:Edit|Write|NotebookEdit)— **enforcement ファイルの
  自己改変を塞ぐ。**`enforcement-paths.sh` の `ENF_CMD_RE` にパスが当たったら deny。
  Bash 経路は `pretooluse-deny.sh` が**同じ集合**で同じことをする(早期素通しブロックの**前**に
  置いてある — `printf` は read/record 動詞として素通しリストに入っているので、後ろに置くと
  `printf "exit 0" > .claude/hooks/pretooluse-deny.sh` がそのまま通る)。
  **これが無いと enforcement は自己防衛できない。**唯一の検出は `pre-stop-checks.sh` Step 4 の
  warn-once(2 回目の Stop で通過)で、その `pre-stop-checks.sh` 自体を書き換えれば次の Stop から
  無効になる。
  - **逃げ道**: `STOCKBOT_HARNESS_EDIT_APPROVED=1 claude` で起動する(人間の明示的な承認)。
    Bash 経路はコマンドの**先頭**に `STOCKBOT_HARNESS_EDIT_APPROVED=1` を置く(任意位置では
    効かない — 任意位置を見ていた版は**コメントに書き足すだけ**で解除できた)。
    narrow escape であってグローバルバイパスではない — 効くのは enforcement ルールだけ。
  - **どこまでが「境界」かを正確に**: **Edit/Write 経路は本当の境界**(逃げ道はセッション env
    だけで、AI 側からは設定できない)。**Bash 経路は境界ではない** — 逃げ道が前置で効く以上
    AI は自分で書けるし、変数間接(`H=.claude/hooks; rm $H/x`)や「書いてから実行する 2 段」は
    正規表現の原理的限界で拾えない(§3 の既知の穴と同じ性質)。Bash 側の価値は
    **うっかり / 事故で自分の検査を消すことを止める**点にある。「1 手で外せる経路」
    (`rm -rf .claude/hooks` / `cd` で相対化 / `python3 -c` / `git checkout` で巻き戻し /
    `xargs` / `--no-verify` / CI の削除)は**全て塞いだ**が、原理的限界そのものは残る。
  - 判定は**リポジトリ相対パスに正規化**してから行う(`./` / `//` / `..` / 先頭 `//` の
    綴りの揺れは全部同じファイルに解決する)。リポジトリ外は対象外だが、**ユーザ階層の
    `~/.claude/settings.json` と `settings.local.json` だけは対象** — `"env"` に承認フラグを
    書けば次セッションから境界が消えるため。`~/.claude/` を**丸ごと**対象にしてはいけない
    (auto-memory / keybindings / agents まで deny する。実測)。
  - fail-open の向き: **入力が壊れているときは通す**(Edit/Write を全部止めると日常作業が死ぬ)。
    ただし**集合の定義が読めないときは fail-CLOSE** する — fail-open にすると
    `: > enforcement-paths.sh` の 1 手でこの guard を無効化できてしまう。
  - 自己テスト: `bash .claude/hooks/enforcement-guard_test.sh`(`make guard-hooks`)。テストは **Edit/Write 経路と Bash 経路の両方**を見る —
    片方だけ拘束すると、もう一方が黙って穴になる(secret パターンで実際に起きた形)。

- **`enforcement-paths.sh`**(enforcement 対象パスの正本)— 「ルールを機械強制している
  ファイル」の集合を 1 か所に置く。中核は `ENF_CORE`(`.claude/hooks/` / `.claude/settings[.local].json` /
  `.githooks/` / `scripts/{arch-guard,secret-scan,pre-commit}.sh` / `Makefile`)で、そこから
  **`ENF_PATH_RE`**(git の相対パス一覧に当てる・行頭固定)と **`ENF_CMD_RE`**(コマンド文字列の
  任意位置に当てる)を導く。**形が 2 つ要るのは事実だが、集合は 1 つ。**2 綴りにすると
  **`Makefile` の扱いが割れる**。`Makefile` は**含める** — `guard:` ターゲット本体を持つので、
  ここを書き換えれば自己テストを丸ごと外せる。`spec-sync-allowlist.txt` は `.claude/hooks/` に
  あるので個別指定は不要(`.*` を 1 行足すだけで docs-sync 検査を全無効化できるため対象必須)。
  `.claude/` の中で対象なのは `hooks/` と `settings*` だけで、skills / agents は AI が直せる。
  代わりに `.claude` そのもの・先頭要素の glob(`.claude/*`)・`..` を含む綴りは対象に残す(どれも
  hooks を名指しせずに消せる形)。`scripts/*_test.sh` は別の集合(`ENF_TEST_*`)で、書き換えと
  新規作成は通し、**削除と空化だけ**を止める(Edit/Write は空になる書き込み、Bash は argv[0] の
  削除動詞・単独の `>`・`/dev/null` からのコピー・`xargs rm`)。丸ごと対象にすると新しい
  テストを Write できず、strict TDD と逆向きになる。

- **`e2e-harness.sh`**(Stop 系 2 本の**自己テスト**が source する)— 本番の hook 経路には
  一切入らない。ケースを `h_defer` で登録して `h_run_deferred` が並列に流し、`h_tally` が集計する。
  既定の並列度はコア数 − 2(上げると遅くなる)。
  集計は**ケースごとの結果ファイル**で行う(親シェルの `pass`/`fail` 変数はサブプロセス化で
  消えて「pass=0 fail=0」の嘘の緑になる)。`h_tally` は**結果を 1 件も残さなかったケースを
  failure として数える** — 並列化すると「ケースが死んでも PASS が減るだけで緑」が成立するため。

- **`pre-stop-checks.sh`**(Stop)— Go/migration を触ったセッションの終了前に
  secret scan → `scripts/arch-guard.sh`(層規約)→ migration 整合 → **テストの存在**
  → `make check-backend` を回す。
  docs-only セッションは早期 exit で高速。`STOCKBOT_PRESTOP_CHECKS=off` で 1 セッション無効化。
  `make check-backend` / `make test-integration` は、最後に緑だった作業ツリー(HEAD・追跡ファイルの
  diff・untracked の中身・`configs/` の全ファイル)と同じなら再実行しない。鍵は
  `runtime/logs/pre-stop-green.*` にあり、赤は記録しない。最終ゲートの pre-push は check-backend を毎回回す。

  **テストの存在(Step 8.5)**: 判定は [`tdd-check-lib.sh`](../../.claude/hooks/tdd-check-lib.sh)
  (副作用の無い関数だけ。hook と自己テストの両方が source する)。無効化は `STOCKBOT_TDD_CHECK=off`。

  | 種類 | block する条件 |
  |---|---|
  | Tier 1 | 新規追加された `backend/**` の非テスト `.go` の**パッケージに `*_test.go` が 1 つも無い** |
  | Tier 3 | このセッションで `*_test.go` が消え、**パッケージのコードは残っているのにテストが 0 個**になった。Tier 1(新規のみ)にも Tier 2(テストを触れば免除)にも当たらず「消せば黙る」状態だったので足した |
  | Tier 2 | **exported 宣言行を追加 / 変更**(`func`/`type`/`const`/`var` + 大文字、メソッド形含む)したのに、**このセッションで `*_test.go` を 1 つも変更していない**。Tier 1 が既に報告したファイルだけ除外する(「新規なら対象外」にすると**テストのあるパッケージへの新規ファイル**が素通りする)。`+` と `-` に同じ宣言行が現れたら相殺する(移動 / rename を「追加」と呼ばない。シグネチャ変更は別の行なので残る) |

  除外は `backend/cmd/**`(配線)/ `backend/internal/testutil/**` / **`*/testdata/**`**
  (Go はビルドもテストもしないので `*_test.go` を要求できない = 必ず誤検知する)/ 生成コード
  (`*_string.go` `*.pb.go` `*_gen.go` `*_generated.go`)/ `*_test.go` 自身。
  **強制するのは存在まで。Red を先に見た順序は事後の diff から復元できない**ので
  機械強制しない(CLAUDE.md の規律 + `pr-merge-check` スキル + 人間レビュー)。
  範囲は docs-sync のルール (7) と同じ「作業ツリー + session 範囲 +(この検査がこの
  セッションで初めて走ったときだけ)直前コミット」。**untracked は Tier 1 だけでなく
  Tier 2 の diff にも合成して足す** — `git diff` は追跡外を一切出さないので、これが無いと
  「Write で作ってまだコミットしていない新規ファイル」= Claude が実際に作業している状態を
  Tier 2 が構造的に見られない。合成は `--- /dev/null` + `+++ b/<path>` + 全行 `+` 前置で、
  **本文は `awk '{print "+" $0}'`(`sed` は最終改行の無いファイルで次のヘッダを飲み込む)**、
  **ヘッダは「hunk の外 + 直前が `--- `」のときだけ採用**する。中身の行は diff で `+`/`-` を
  前置されるので、`-- x` の削除は `--- x`、`++ b/…` の追加は `+++ b/…` になり、**実 diff でも
  hunk の中でヘッダのペアが作れる**。`@@` と `diff --git` の並びは中身から作れないのでそこで
  区別する(合成側にも同じ枠を付ける)。**git の出力形式は config で変えられる**ので、
  `-c core.quotePath=false`(非 ASCII パスの C-quote)/ `--no-ext-diff --src-prefix=a/
  --dst-prefix=b/`(`diff.mnemonicPrefix` / `diff.external`)を明示する — 付けないと
  fail-CLOSED を掲げた検査が git config 1 行で黙って無効化される。
  **`core.quotePath` は Step 2(変更ファイル収集)にも要る** — そこが Step 8.5 に到達するかを
  決める early exit の入力で、非 ASCII 名の `.go` だけを触ったセッションは
  **hook ごと素通り**していた(arch-guard / migration 整合 / `make check-backend` も含めて)。
  **「初めてか」は自前の印
  `runtime/logs/tdd-check-seen.<sid>` で見る** — `session-start-sha.<sid>` は
  この hook 自身が Step 2 で作るので、使うと常に「2 回目以降」= 標準フローが素通りする。
  補助ライブラリは `cd` する前に `BASH_SOURCE` から絶対パスで解決し、**欠落したら
  fail-CLOSED で block** する(検査が黙って消えるのを防ぐ)。
  **誤検知率**は実 hook を全履歴の backend コミットに流して測る(テストファイルを外して流すと
  block することも併せて見る = ルールが死んでいないことの確認)。
  自己テスト: `bash .claude/hooks/pre-stop-tdd_test.sh`(`make guard-hooks` から実行)。
  単体(判定関数)+ e2e(git から何を拾うか)の 2 部構成。**凍結**(新しいケースを足さない。
  別の Step / lib の回帰は小さな別ファイルの自己テストに書く)。
  **e2e は exit code だけでなく「報告されたパス」も見る** — 別ファイルの名前で block しても
  exit 2 は同じなので、合成 diff のヘッダが効いているかは exit code では判別できない。
  **Tier 1(untracked 経路を含む)/ Tier 2 / Tier 3 / 除外 / 範囲 / 逃げ道 / lib 解決 /
  相殺 / コメント除去 / 合成 diff の枠 / git の出力形式固定(Step 2 の early exit 側 4 経路を
  含む)の各ルールを 1 箇所ずつ変異させ、自己テストが赤くなることを確認済み**
  (「テストが実装と別経路を見ている」対策)。

  **残した穴**(いずれも実測で確認済み・意図的に受容):
  - **検出範囲 ⊇ 免除範囲**。Tier 1/3 の検出は最初の Stop で直前コミットを**常に**含む
    (含めないと「唯一のテストを消したコミット」「テストも触りつつテスト無しパッケージも
    足した混在コミット」が見えなくなる。実リポの backend コミットは 10/10 がテストを
    触っているので後者は例外形ではなく標準形)。一方 **Tier 2 の免除**(`*_test.go` を
    触ったか)は Tier 2 の検出と同じ狭い範囲から作る — 広い範囲から作ると、前セッションの
    最後のコミットがテストを触っているだけで今セッションの Tier 2 が丸ごと免除される。
    **同じにするのではなく、免除だけを狭める。**
  - **「直前コミット由来の block が 1 セッション 1 回で消費される」ことは無い**。
    SessionStart が `session-start-mark.<sid>` を置くのでセッション開始点が
    確定し、**前セッションのコミットをそもそも範囲に入れない**。作業ツリー由来(e7b)と session 範囲由来(e31)の block は持続する。
    **ただし SessionStart が動かない環境**(hook 未登録 / `session_id` 不明 /
    `runtime/logs` が書けない)**では従来動作に戻る** — 縮める方向に倒すと fail-OPEN になるため。
  - **無関係な `*_test.go` を 1 つ触る(削除を含む)だけで Tier 2 は全ファイル分が無効**になる
    (仕様)。Tier 1 / Tier 3 は影響を受けない。`*/testdata/**` の `*_test.go` は
    Go が走らせないので免除に数えない。
  - **非 ASCII の rune リテラル**(`const Yen = '¥'`)を含む宣言行は、mawk(バイト)と
    gawk(文字)で `strip_comment` の位置がずれる。壊れ方は誤検知側で、実リポの Go には
    非 ASCII rune が 0 件なので受容。
  - **Tier 3 はパッケージの一部だけを別ディレクトリへ切り出すと block する**(切り出し元に
    テストの無いコードが残るため。メッセージは事実どおりで policy としても妥当だが、
    Tier 表からは読み取れないので明記する)。宣言を持たない `doc.go` も「コード」に数える。
  - **`"` / タブ / 改行を含むパス**は `core.quotePath=false` でも quote されるので、
    先頭が `"` になって無言で除外される。Go のファイル名としては非現実的なので受容。
  - **生文字列リテラルの中の 0 桁目 Go コード片**(`const doc = \`` の次行の `func Handler…`)を
    exported 追加と誤認する。実リポの該当 8 行は全て `*_test.go` 内 = 除外対象なので実害ゼロ。
  - **テストの無いパッケージへ `git mv`** するとパッケージ分割だけで block する(新パッケージには
    テストを書く、という意図どおり)。**テストの有るパッケージ内**の移動 / rename は
    `+` と `-` の宣言行を相殺するので block しない。
  - **テストの有るパッケージに unexported だけの新規ファイル**を足すと通る(Tier 1 は
    パッケージにテストがあるので通し、Tier 2 は exported が増えていないので当たらない)。
  - **相殺は行単位**なので、宣言行の**整形**(1 行 → 複数行のシグネチャ)や定数値の変更でも
    block する。契約に触ったらテストを触れ、という policy としては妥当なのでそのまま。
    メッセージは「追加 / 変更」と書く(「追加」と言い切ると事実と違う)。
  - **`+`/`-` の相殺はファイル横断**なので、無関係な 2 箇所で**同一の宣言行**が同じ Stop window に
    消えて増えると取りこぼす(実リポに同一宣言行の重複は 5 グループ。例 `type Result struct {`)。
    ディレクトリ単位に絞るとパッケージ間の移動が誤 block になるため、取りこぼす側に倒した。
- **`docs-sync-check.sh`**(Stop)— **契約そのものを書いた doc** を持つソース(`port/repository.go`・
  safety・`domain/position`・新規 package)を変更したら、その doc も同じ Stop window で更新するよう強制。
  migration の docs 要求は `pre-stop-checks.sh` だけが持つ(DATA_MODEL)。layers/*.md を exported の
  変更ごとに要求すると、AI に残る出口が「doc に追記する」だけになり、layers docs が実装の写しに
  膨らむ(だから契約の doc だけに絞る)。契約を変えない編集の除外は
  [`.claude/hooks/spec-sync-allowlist.txt`](../../.claude/hooks/spec-sync-allowlist.txt)(enforcement 側なので人間が足す)。
  `backend/internal/**` の .go は次の 2 層だけを判定する:

  | 変更の種類 | 扱い |
  |---|---|
  | ファイルの新規追加 / 削除、または diff が **exported 宣言行**(`func`/`type`/`const`/`var` + 大文字、メソッド形含む)に触れた | **block** |
  | 内部実装だけの変更 | 通す(何も出さない。exit 0 の stderr はモデルに届かない) |

  | ディレクトリ | 要求する doc |
  |---|---|
  | `domain/position/**` | `STATE_MACHINE.md` |
  | `safety/**` | `layers/safety.md` **+** `FAILURE_MODES.md`(両方必須) |

  全変更を block にしない理由は「誤検知するガードは必ず無視されるようになる」から。
  **判定はコメント/整形に反応しない** — diff の宣言行を行末コメント除去 + 空白圧縮して
  `+` 側と `-` 側の集合を比べ、食い違ったときだけ契約が動いたとみなす。
  括りの中(grouped const の enum 値 / iota の継続行 / struct フィールド / 埋め込み /
  interface メソッド)は hunk ヘッダの文脈で判定するので、**関数本体の `Exported(...)`
  呼び出しや `var X = func(){…}` の本体は拾わない**。
  自己テスト: `bash .claude/hooks/docs-sync-check_test.sh`(`make guard-hooks` から実行)。

  **範囲の決め方(ここを間違えると本番だけ素通りする)**: 「このセッションで docs-sync が
  初めて走ったか」は **docs-sync 自前の印**(`runtime/logs/docs-sync-seen.<sid>`)で見る。
  `session-start-sha.<sid>` で判定してはいけない — Stop 配列は `pre-stop-checks.sh` が先で、
  そちらが同じファイルを先に作るため、常に「2 回目以降」と誤判定して
  **「実装 → コミット → Stop」という標準フローが丸ごと素通りする**(レビュー実測)。

  複数行シグネチャの引数変更も拾う。git の funcname は**シグネチャが複数行の関数だけ
  開き括弧で終わる ctx**(`func Compute(`)を出すので、単一行シグネチャの本体変更
  (`… {` で終わる)と区別できる。文キーワード(`return` 等)を除くのは、本体の
  `return Result` が「名前 型」と同形になり誤 deny したため(実測)。

  **残した穴**(いずれも実測で確認済み・意図的に受容):
  - **「前のセッションが doc 未更新のまま残したコミットで 1 回 block される」**と
    **「最初の Stop より前に 2 つ以上コミットすると 1 個目が見えない」は起きない**。
    SessionStart がセッション開始点を確定させるので、範囲は
    `<session 開始 SHA>..HEAD` + 作業ツリーの 1 本になる。
    **SessionStart が動かない環境では従来動作**(どちらの穴も復活する)。
  - `session_id` が取れないとき(jq 不在等)はルール (7) の範囲を**作業ツリーだけ**に絞る。
    フォールバック ID が PPID を含むため、親が毎回変わると印が別名になり
    「常に最初の Stop」= 永久 block + 連続 3 回の逃げ道も死ぬ(実測)。
    **縮退の代償**: その間は**コミット済み**の `domain` / `handler` / `usecase/query` /
    `adapter` / `port` の変更をルール (7) が見ない(既存ルール 1〜6 は従来どおり効く)。
  - 引数の検出は「タブ 1 個 + `名前 型`」に限る。そのため
    `a, b int → a, b, c int`(名前まとめ)、`fn func(int) error` の型変更、
    戻り値だけの変更(`) error {` → `) (int, error) {`)は Tier 2 に落ちる(見逃し)。
    逆に「**複数行シグネチャ かつ 本体の生文字列がタブ 1 個**」だと SQL の行を引数と
    誤認して block する(**誤 deny**・実測)。実リポの該当は現在 0 件
    (複数行シグネチャ 0/323 関数・タブ 1 個の該当行 0)。
  - 宣言行の**文字列リテラルだけ**の修正(エラー文言の typo 等)は block する。
    コメント・整形は反応しないが、文字列は spec の一部とみなしている
    (`ErrInvalidExtendMinutes` の "1 and 720" のように文言が仕様の例がある)。

いずれも**無限ループガード**(同一セッションで連続 3 回 block したら警告して通過)付き。
最終ゲートは pre-push が担う(CI は無い — §3)。

---

## 2. git hooks

```bash
git config core.hooksPath .githooks
```

これで [`.githooks/pre-commit`](../../.githooks/pre-commit)(staged の secret scan)と
[`.githooks/pre-push`](../../.githooks/pre-push) が有効になる。Stop hook は ESC 中断 / 強制終了で
発火しないため、push 境界でもう一段ゲートする。

pre-push が回すもの:

| いつ | 何を |
|---|---|
| 毎回 | `secret-scan` → `make vet-integration` → `make check-backend fmt-check guard-fast`(arch-guard + 運用スクリプトの自己テスト) |
| push の範囲に enforcement の変更があるとき・前回の全件から 7 日たったとき | `make guard-hooks`(hook / guard の自己テスト全件。最後まで緑なら `runtime/logs/pre-push-selftest-ok` に時刻を書く) |

- 「enforcement の変更」は `enforcement-paths.sh` の集合 + `scripts/*_test.sh`(rename は分けて見る)。
  判定は [`pre-push-scope.sh`](../../.claude/hooks/pre-push-scope.sh)。**判定できないとき**
  (範囲が空・コミットが解けない・記録が無い / 壊れている / 未来・lib が読めない)は全件に倒す。
- 強制するときは `STOCKBOT_PREPUSH_FULL=1 git push`、手で回すときは `make guard-hooks`。
- 自己テスト: `pre-push-scope_test.sh`(判定)と `pre-push_test.sh`(make を stub にした一時リポで
  pre-push の配線を見る。全ての `*_test.sh` が `guard-fast` / `guard-hooks` のどちらかで回ることも見る)。
- 全件が週 1 回なのは、hook を変えていなくても環境要因(`GIT_DIR` 等)で赤くなることがあるから。
  週 1 回の全件は launchd ではなく pre-push の記録で回す
  (launchd は TCC で repo を読めない)。

---

## 3. CI

CI は無い。Stop hook と `.githooks/pre-push` の 2 つがゲートで、`build` / `vet` / `test` / `arch-guard` /
hook 自己テスト / `secret-scan` は全部が `guard-fast` / `guard-hooks` のどちらかにあり、
`pre-push_test.sh` が置き場所の漏れを検査する。

⚠️ **`git config core.hooksPath .githooks` を設定していないと push 境界の層も無い**(§2)。
Stop hook は ESC 中断 / 強制終了で発火しないので、その場合ゲートはゼロになる。
`make check` を手で回すのが最後の砦。

---

## 関連

- [../../CLAUDE.md](../../CLAUDE.md) — 絶対ルール
- [../architecture/PR_CHECKLIST.md](../architecture/PR_CHECKLIST.md) — push 前チェック
- [../../Makefile](../../Makefile) — `make help`
