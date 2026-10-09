# 立花証券 e支店 API — アダプタの実装仕様

> 立花 e支店 API を `internal/adapter/broker/{tachibana,tachibana_transport,tachibana_market,tachibana_orders}.go` に実装し、
> `port.Broker` / `port.LiveBroker` を満たすための仕様。Mac/Linux ネイティブ(常駐アプリ不要・純 HTTP/JSON)。
> 実装済みのメソッドは transport / login / RefreshToken / GetAccountMargin / GetPositions / GetTicker / GetTickers /
> GetKlines(日足)/ PlaceOrder / ClosePosition / CancelOrder / ResolveExecution / GetActiveOrders / GetExecutions /
> ListProtectiveOrders / PlaceSettleOCO / ResolveSettleLegs。
>
> **API 版**: 叩く版は `tachibana.go` の `defaultTachibanaAPIVersion` の 1 行で決まる。login URL は
> `https://{host}/e_api_{ver}/auth/`。旧 v4r7(`sUserId`+`sPassword`)は 2025-11-29 に、v4r8 は 2026-06-27 に、
> v4r9 は 2026-09-27 に立花が廃止した。
>
> **認証は公開鍵認証**: login は `sAuthId` のみ送信し、応答の仮想 URL 群を RSA-OAEP(SHA-256)+ base64 で復号する
> (`decryptVirtualURL`)。`NewTachibana(env, authID, *rsa.PrivateKey, secondPW, …)` + `ParseTachibanaPrivateKey`(PKCS#1/#8)。
> 電話認証(2FA)は v4r8 廃止と同時に無くなったので、無人の RefreshToken に 2FA は入らない。
> 認証 ID / 秘密鍵はデモと本番で別のセット(標準 Web 画面で取得)。出典: 公式 pubkey サンプル
> `github.com/e-shiten-jp/e_api_login_pubkey.py`(`e_api_encode_auth.py` の PKCS1_OAEP+SHA256 と一致)。
>
> **ホスト**: 本番 `kabuka.e-shiten.jp` / デモ `demo-kabuka.e-shiten.jp`(`env` で切り替える)。
>
> **執行区分は現物と制度信用 6 ヶ月だけ**(`config.BrokerKind.SupportsExecKind`)。現金信用区分
> (`tachibana_orders.go` の `genkinShinyouKubun`): `0`=現物 / `2`=制度信用新規 / `4`=制度信用返済、建日種類 `2`=建日順
> (返済はナンピン禁止で 1 建玉 = 一意)。一般信用(`6` / `8`)は仕様にあるが口座の種別によっては拒否されるので起動時に弾く。
> 一日信用は API に区分が無い。建玉は `CLMShinyouTategyokuList`(`aShinyouTategyokuList`)、維持率は
> `CLMZanRealHosyoukinRitu`(`sItakuHosyoukinRitu`)で取得し、維持率ブレーカーを駆動する。
>
> **e支店の信用条件(公式の案内から)**: 委託保証金 最低 30 万円・委託保証金率 33%・維持率 30%。
> (a) 強制決済(信用ロスカット)は維持率 15% を割ったとき。判定は前場と後場の終了時点の 1 日 2 回で、
> 含み損で計算されるので決済していなくても発動する。bot 側のブレーカー `hard_limits.margin.maintenance_ratio`
> はこれより手前に置く(`catastrophe_guards_test.go` が床を固定)。
> (b) 新規建余力はその日の可能額の最小値で、受渡日をまたぐ現物代金・代用有価証券の評価で日々変わる。
> 発注前 collateral チェックはその日の最小値を前提にする(余力 ≈ 保証金 ÷ `required_rate`)。
> (c) 現引・現渡には事務手数料がかかる。bot は現引・現渡を使わない(決済は反対売買のみ)。
> 追証の発生ラインと差し入れ期限は案内に明示が無い(§7)。
>
> **守り(`PlaceSettleOCO`)は `STOCKBOT_TACHIBANA_OCO_VERIFIED=1` が無ければ fail-close**。
> 逆指値の発動方向 と bot 停止下の約定 は未実証(§5)。
>
> 項目名の出典は公式 pubkey サンプルで照合した全 CLM の request/response。公式リファレンスは Shift-JIS で
> 逐語確認が一部できないので、版ごとの差分は最新スペックとデモで確かめる(§7)。

## 0. 採用判断(守りは broker 側に置けるか)= 条件付き YES

- **逆指値(stop)は broker 側に置ける**: 現物決済売り(`CLMKabuNewOrder`, `BaibaiKubun='1'`, `GenkinShinyouKubun='0'`)に
  `GyakusasiOrderType='1'`(逆指値)+ `GyakusasiZyouken`(トリガー価格)+ `GyakusasiPrice`(発動後 成行 `'0'` / 指値)を指定すると、
  発注時点で証券会社側に常駐し bot が落ちても約定する。CLAUDE.md「TP/SL は broker 側」の最低条件(stop が broker 側)を満たす。
- **フル OCO(利確指値 + 損切り逆指値の同時)は未検証 → fail-close**: `GyakusasiOrderType='2'`(通常 + 逆指値 = 立花「ダブル注文」)で
  1 注文に利確指値(`sOrderPrice`)+ 逆指値を同梱し OCO 相当にする。「現物で同時同梱が受理されるか」「片脚約定で他脚が自動失効するか」が
  未検証なので、`STOCKBOT_TACHIBANA_OCO_VERIFIED=1`(人間がデモ検証後に立てる)まで `PlaceSettleOCO` のフル OCO は error。
- **恒久解はダブル注文(`'2'`)**: stop-only(逆指値返済)は OCO 検証前のフォールバック。`PlaceSettleOCO` は
  TP 無し(trail 等)は stop-only `'1'` + TP / ratchet は OnTick、TP ありは `'2'`。
  参照実装の警告(独立 2 本の返済注文が両約定して逆ポジを建てる危険)は、単一注文のダブル注文には該当しない。
- **逆指値の発動方向は API に UnderOver 相当のフィールドが無く、売買区分から暗黙決定**(売り = 下落で発火)。
  方向を誤ると即約定(損失確定)か永久不発動(守りなし)なので、「現値より下にトリガーを置いた売り stop が
  即約定しないこと」をデモで最初に確認する(§5)。

## 1. 接続・セッション

- login URL = `https://{host}/e_api_{ver}/auth/`(`ver` は `defaultTachibanaAPIVersion` の 1 箇所)。
- **セッション = login 応答の仮想 URL 群そのものがトークン**(別途トークンヘッダ無し):
  `sUrlRequest`(発注 / 照会 / 余力 / 建玉)・`sUrlMaster`(マスタ)・`sUrlPrice`(時価)・`sUrlEvent` / `sUrlEventWebSocket`(push)。
- 失効: 大引け後・API 用サーバ閉局 03:30・同一 ID の再ログイン。日中は同じ仮想 URL を使い回す(都度ログインしない)。
- **ログインは集計窓(5:30 起点)に 1 回が原則**(立花は仮想 URL を 1 営業日に 1 度取得して継続利用することを求めている)。
  bot が login するのは ①起動時 ②`p_errno=2` を受けたとき ③閉局をまたいで動き続け、窓の中にまだ成功した login が無いとき
  (`runDailyLoginLoop`・10 分ごとに `LastLoginAt` を見るだけ・閉局窓 3〜6 時は張らない)の 3 つだけ。
  ②は `refreshSession` が間隔を空ける(1 分から倍々・天井 30 分・1 時間張り直さなければ最短に戻す)。
  間隔の中の照会はログインを撃たずに `errTachiReloginThrottled` で失敗する。明示の `RefreshToken`(①③・CLI)は間隔に縛られない。
  別プロセス(`fetch-daily` 等)の login は同一 ID なので bot のセッションを失効させ、bot 側は②で 1 回張り直す。
  1 日の実数は `~/.stockbot/state/api-usage` の `CLMAuthLoginRequest` で測る。
- **`RefreshToken()` = 再ログイン本体**(paper の no-op とは違い立花では必須):
  1. 既存 session を `CLMAuthLogoutRequest` で破棄(同一 ID の二重ログイン回避)
  2. `CLMAuthLoginRequest` を auth URL へ。`p_no=1` 起点・`p_sd_date=now`・`sJsonOfmt=6`。`sAuthId` のみ送信。
     応答の `sUrlRequest` / `sUrlPrice` / … を秘密鍵で `decryptVirtualURL` 復号してからセッション確立
  3. 成立 4 条件: `p_errno=='0'` ∧ `sCLMID=='CLMAuthLoginAck'` ∧ `sResultCode=='0'` ∧ `sKinsyouhouMidokuFlg!='1'`(金商法書面未読でない)
  4. 仮想 URL 一式 + `sZyoutoekiKazeiC`(発注に必要)を保存し、`lastRequestNo` を応答 `p_no` で再初期化
  - トリガ: `p_errno=='2'`(無効セッション)受信時(間隔つき)/ 起動時 / 閉局跨ぎ(窓に 1 回)。
    全メソッドを「p_errno==2 なら RefreshToken して 1 回リトライ」のラッパーで包む。
  - **張り直しは単一化する(single-flight)**。素直に「p_errno=2 を見たら自分で logout+login」にすると、
    他の経路の張り直しと衝突して互いの新しいセッションを logout し合い「再ログインしてもまだ無効」になる。
    - セッションに世代(`sessionGeneration`・login 成功のたびに +1)を持ち、`requestOnce` は実際に送信に使った世代を返す。
      p_errno=2 を見たら `refreshSession(ctx, gen)` が `refreshMu` を取り、その世代がまだ現役のときだけ張り直す。
      誰かが先に張り直していたら何もせず retry するだけ。
    - `RefreshToken`(公開・ループが使う)も同じ `refreshMu` を通るので、張り直しは常に 1 本。
    - `refreshMu` は `t.mu` とは別のロック。`t.mu` は 1 リクエストの送信中ずっと握られるので、
      そこへ相乗りさせると照会 1 本ごとに更新が直列化して詰まる。
    - 世代を進めるのは `login` がセッションを差し替える 1 行だけ。忘れると `refreshSession` が常に「現役」と読む。
- 起動 health: `CLMSystemStatus` で `sLoginKyokaKubun='1'`(許可)/ `sSystemStatus='1'`(開局)を確認してから entry gate を開ける。

## 1.5 呼び出しレート上限

立花は 1 日の API 利用を **1 万回以下**にするよう求めている。仕様書に明示のレート制限は無く、上限は
他の利用者との比率で決まる相対値として説明されているので、今後さらに下がる前提で設計する。
呼び出し回数は監視銘柄数ではなく **レーン数 × 頻度** で決まる(1 リクエストに 120 銘柄まで積める)。

三重の歯止め(どれか 1 つでも外すと再発する):

1. **transport のハード天井** — `broker.DefaultTachibanaRPS` / `DefaultTachibanaBurst`。`ratelimit.go` の
   トークンバケツを `requestOnce` が必ず通る。銘柄数・ループ本数・戦略数のどれにも依存しない天井。
   超過分は捨てずに待たせる(価格を落とすと紙約定が歪むため)。`SetRateLimit(0,0)` の opt-out は
   数回しか叩かない probe 専用で、ポーリングする bot では使わない。
2. **価格ループ間隔** — 一括取得できる broker は `batchedPriceInterval`、できない実フィードは `realFeedPriceInterval`
   (`broker.QuotesBatched()` が決める。速いまま素通しに戻る配線は無い)。`STOCKBOT_PRICE_INTERVAL_SEC` で
   人間が詰められるが、壊れた値は既定に落ちる。日足・数日保有で TP/SL が建値からの円幅固定な戦略には秒解像度は
   判断を変えないが、日中戦略(`bnf_intraday_reversion`)には効く(§1.6-1)。
3. **日足は持っていれば叩かない** — `session.LastClosedTradingDay` で「今存在しうる最新の日足」を求め、
   既にそこまで持つ銘柄は `refreshDailyCandles` が API を出さずにスキップ。再起動のコストもゼロになる。

4. **時価は一括取得** — `CLMMfdsGetMarketPrice` の `sTargetIssueCode` はカンマ区切りで複数銘柄を受け付ける。
   `Tachibana.GetTickers` がチャンクして投げ、`BatchQuoteFeed`(`batch_feed.go`)が bundle 側の per-symbol
   `GetTicker` を 1 リクエストに畳む。これで呼び出し数が銘柄数から切り離されている。

   - **1 リクエストの上限は 120 銘柄**。121 銘柄以上は末尾が落ち、しかも `p_errno` は 0 のまま
     (サイレント切り詰め)。欠けるのは必ず要求の末尾なので、`fetchQuoteChunk` はその signature を見て残りを
     取り直す(上限が将来下がっても自動追随。散在する欠け = 売買停止等は取り直さない。回数は `maxTruncationRetries`)。
   - **やってはいけないこと**: 単一取得の `pickPriceRow`(該当なしなら先頭行)を batch 経路に持ち込むこと。
     30 銘柄要求して 1 行だけ返ったとき 29 銘柄へ他人の価格を配り、紙約定が静かに毒される。
     `GetTickers` は銘柄コードで厳密照合し、落ちた銘柄は結果マップに入れない
     (`TestGetTickersNeverSubstitutesAnotherSymbolsPrice` が固定)。
   - `BatchQuoteFeed` は古い価格を新鮮なふりで返さない: フィード断や応答落ちは error にする
     (paper_live_feed の「古い価格で約定させない」契約と同じ極性)。

5. **立花の公式アナウンスに従った時間帯・頻度**(出典 <https://www.e-shiten.jp/api/20260310.html>)。
   回数の上限は非公表なので、守れるのは時間帯と頻度:

   | 立花の案内(要旨) | 実装 |
   |---|---|
   | 大量かつ頻繁な株価取得(CLMMfdsGetMarketPrice)と頻繁な情報照会(ポーリング)は **AM8:00〜PM15:30** は控える | 価格ループを単一レーン(`batchedPriceInterval`)にし、1 日の回数を `dailyQuoteRequestBudget` で縛る(§1.5-6) |
   | CLMMfdsGetMarketPriceHistory やマスターデータの取得は **PM18:00〜翌3:30 / AM5:30〜AM8:00** を推奨 | `inHistoryFetchWindow`(`history_window.go`)で定期リフレッシュをこの窓に限定。窓の終端 3:30 は API 閉局と一致 |

   - **当日の日足は引け後すぐには配信されない**(翌朝には入っている)。全銘柄を引いて初めて「まだ無い」と分かる
     作りだと推奨窓の毎時ティックで監視ユニバース × 回数の空振りになるので、`probeDailyCanaries` が先に
     3 銘柄だけで配信済みかを確かめ、無ければその回を打ち切る(取得済みバーは本取得で再利用)。
   - **起動時の日足取得だけは窓外でも走る**。日足が欠けたまま day-horizon 戦略を回す方が害が大きいため
     (鮮度ゲートがあるので通常は 0 リクエスト)。窓外の起動は Warn を出す — AM8:00 より前に起動する運用にする。
   - `rawDailyCandles` の broker フォールバックにも同じ窓ゲートを掛けてある(`AllowHistoryFetch`)。ここは
     毎ティック評価される経路なので、pg が一時的に空を返すと 1 日 1 本の履歴取得が秒間ポーリングに化ける。
   - `login` も天井を通す(`tachibana_transport.go`)。`requestOnce` を通らない唯一の経路で、
     セッション不安定時の `refreshWithRetry` が無制限にならないようにする。

6. **回数は数えて縛る**(間隔という代理指標で縛らない)

   - 回数は監視銘柄数ではなく **レーン数 × 頻度** で決まる。hot 銘柄を別レーンにするとその回数がまるごと
     足し算になるので、時価は単一レーンにする(段階ウォッチは §1.7 の未実装の案)。
   - **数える手段**: `Tachibana.APIRequests()` が `doGET`(唯一の HTTP 送信点)で計上する = 相手のサーバから見た回数。
     見積りは黙って古くなる(プール拡張で日足の本数が増えても誰も気づかない)。
   - **予算をテストで縛る**: `inSessionQuoteRequests` が立会時間からレーンごとの回数を足し上げ、
     `dailyQuoteRequestBudget` を超えたら `TestInSessionQuoteRequestsStayWithinBudget` が落ちる。
     「`priceInterval >= N 秒`」のような代理指標はレーンを足したときに素通りするので、間隔ではなく回数で縛る。

7. **立花の数え方**

   立花が数えるのは **https の要求回数**(REST API へのリクエスト回数と WebSocket の接続回数)で、
   集計期間は**開局している時間帯(5:30〜翌 3:30)**、暦日ではない。集計は **CLMID 単位**。
   **login は先方の集計に現れない**(auth ホスト宛のみ別扱い)。弊方は送っている以上数えるが、
   先方基準と比べるときは除く(`Usage.TotalExcludingLogin`)。WebSocket は 1 本も張っていない
   (`sUrlEventWebSocket` は復号すらしていない)。

   これに合わせた 4 つの対処:

   - **予算を不変条件にする**。実効間隔 = `batchedPriceInterval × ceil(監視銘柄数 ÷ 120)`
     (`BatchQuoteFeed.effectiveMaxAge`)にして、1 日の総リクエスト数を監視銘柄数から切り離す。
     121 銘柄目で通信量が階段状に倍増する経路は無い — 代わりに時価の解像度が段数ぶん落ちる
     (`quote_chunks` が 2 以上なら 1 分足のサンプル密度が落ちているので、検定に使う期間のメタデータに記録する)。
   - **障害中の再取得ストームを塞ぐ**。エラー時にキャッシュを触らない設計の副作用で全 bundle が各自 fetch を
     やり直すと、リミッタが頭打ちにしても発注が再取得の行列の後ろで待たされる(リミッタに優先度が無い)。
     直近の失敗を次の取り直し時刻まで覚えることで止める。
   - **1 日の消費を数える**: `internal/adapter/apiusage`(窓 5:30 起点・全プロセス合算・CLMID 別・再起動で消えない)。
     dashboard に「N / 10,000」と内訳が出る。
   - **朝の `fetch-daily` の二重実行を封鎖**(`scripts/stockbot-routine.sh` の `daily_updated_today`)。

   小さな増幅経路(上限付き・監視銘柄数で増えない): 末尾欠けリトライ(`maxTruncationRetries`)、`p_errno=2` の再送、
   夜間再起動のカナリア。プールの日足取得(`cmd/fetch-daily`・1 銘柄 1 リクエスト)は 1 日 1 回・推奨窓の内側に収める。
   プールを広げるときは、この内訳を必ず更新する。

7.5 **口座照会のレーンは時価と別に数える**

   口座照会(建玉 `GetPositions` / 維持率 `GetAccountMargin`)は実 broker を挿したときだけ wire に出る。
   - 維持率は保有中 1 時間に 1 回 / 建玉の有無が変わったらその場 / 無保有は起動後の 1 回だけ(`MarginPollInterval`。
     paper は据え置き)。判断が付かないときは必ず「照会する」側へ倒す(`HoldsPosition` は repo 未配線 / 読めないとき true)。
   - reconcile は「場中 × 建玉あり」のときだけ broker を叩く(`reconcileSchedule`)。寄り前の同期 reconcile 1 周は残す。
   - 予算ガードは `inSessionRequests`(= 時価 + 口座照会)で、`TestInSessionRequestsStayWithinBudget` が合計で
     `dailyQuoteRequestBudget` を守らせる。時価レーンだけを数えて緑になる構成には戻れない。

7.6 **口座照会を払う順序と回数**

   回数は頻度だけでなく **判定の順序** と **「その質問の答えが銘柄ごとに違うか」** でも決まる。
   - **ゲートを 2 相に割る**。`EvaluateStructural`(broker 由来フィールドを読まない = 建玉枠・ナンピン禁止・セッション・
     日次損失・窓・方向・スプレッド)と `EvaluateCollateral`(余力・レバ上限)。`TradingCycle` は
     `BuildStructural` → 構造ゲート → 通ったものだけ `FillCollateral` → フルゲート。枠が満杯の間、口座照会は 0 回。
     `BuildStructural` は `MarginStatusUnknown=true` を立てて返し、未照会は fail-close(`margin_status_unavailable`)へ倒す。
     `BuildStructural + FillCollateral` をまとめた `Build` は置かない(呼ぶだけで照会を払う入口は構造ゲートの前で呼ばれる)。
     予算モデルにはエントリー試行の項(`liveEntryAttemptsPerDayAllowance`)がある。
   - **口座照会はイベント駆動**(`command.AccountMarginCache`)。保証金は口座単位の量なので銘柄数・ティック数ぶん払わない。
     wire を打つのは ①初回 ②約定 / 決済(`SymbolBundle` が建玉の有無の変化で `Invalidate`)③maxAge(1 時間)経過だけ。
     維持率ブレーカー(`CheckMaintenance`)は必ず実照会し、その結果でキャッシュを温める。失敗はキャッシュしない。
   - **selector が資金枠を先に見る**(`Selector.WithLeverageHeadroom`)。「そもそも 1 本でも建てられるか」を先に判定し、
     駄目なら 1 銘柄も arm しない(`fundless`)。判定式は `risk.WithinGrossNotionalCap` をそのまま呼ぶ(発注前ゲートと同一)。
     照会が読めないときは arm しない(fail-close)。

> レートリミッタは立会時間を知らない。`DefaultTachibanaRPS` × 86,400 秒 が理論上の絶対天井で、これは立花の上限
> (1 万回)を桁で超える。実際に低く収まっているのはリミッタではなくループ側のゲート(場外は価格ループが止まる /
> 履歴は窓の中だけ / 鮮度ゲート)である。24 時間回る経路を新設するときは、天井があるからと安心せずゲートを付けること。

## 1.6 この削減が払っているコスト(既知のトレードオフ)

1. **1 分足の値幅が系統的に狭く記録される(測定バイアス)**: 1 分足は価格ループの tick から集約されるので、
   高値 / 安値のサンプル数は間隔で決まる。日中の行き過ぎを狙う戦略ではトリガー回数が過小(エッジを見逃す方向)に
   なると同時に逆行の深さも過小(エッジを過大評価する方向)になり、「安全側に転ぶ」とは言えない。
3. **一括取得は「全滅か全成功」になる(故障が相関する)**: 1 本に畳んだリクエストが失敗すると監視全銘柄が 1 tick 分
   まっ暗になる(`BatchQuoteFeed.refresh` は失敗時にキャッシュを更新せず error を返す)。監視が 120 を超えて
   2 チャンクになると、2 本目の失敗で 1 本目の結果も捨てる。次 tick で復旧するが、持続的な障害時の挙動は未検証。
4. **場中に日足の穴を自己修復できない**: `rawDailyCandles` の broker フォールバックに時間帯ゲート(`AllowHistoryFetch`)が
   あるので、場中に 1 銘柄の日足が欠けるとその日ずっと `insufficient_daily_history` で対象外になる。
   監視すべき兆候: 同一銘柄の `insufficient_daily_history` が連続する。
6. **transport の天井は発注バーストを待たせる**: 多数建玉の引け前フラット化は全部出し終わるまで
   建玉数 ÷ `DefaultTachibanaRPS` 秒かかり(+OCO 脚)、緊急フラット化も同じキューを通る。
   実弾の建玉数を増やすときは burst 設定を見直す。

## 1.7 未実装の案: 段階ウォッチ(Tier A / Tier B)

一括取得(§1.5-4)で呼び出し数は既に銘柄数から切り離されているので、120 銘柄以下なら銘柄を絞っても通信量は減らない。
効く唯一のレバーは間隔で、絞ることの価値は浮いた予算で注視銘柄の間隔を詰められることにある。
監視が 120 を超えてチャンクが増える構成では、注視銘柄(建玉中はその建玉だけ)を短間隔、全銘柄スイープを低頻度に
分ける段階ウォッチが得になる。実装するときの制約: 建玉が閉じたら決済イベント駆動でフラットへ戻す(スイープを待たない)/
Tier B の価格で発注しない(昇格させて新鮮な時価を引き直し、`Ticker.Stale` を経由させる)/ 建玉銘柄は絶対に降格させない
(TP/SL 管理・維持率ブレーカー・引け前フラット化は全部 `PriceTick` に乗っている)/ 1 秒に戻すのは銘柄を絞ったうえでの話で、
判断が分かれるなら遅い側を採る。さらに減らす手札は、プールの日足取得を場の無い独立した集計窓(土曜)へ寄せること、
プッシュ配信(`sUrlEventWebSocket`・接続回数も計上対象)、時価の間隔延長(最後の手段・1 分足のサンプル密度が落ちる)。

## 1.8 アクセスしてよい日と時間帯

立花は、閉局(サービス停止)中・休日・メンテナンス時間帯のアクセスを禁じている。メンテナンス時間帯は
エラー応答を見て叩き続けないこと。

**規則**: 立花に触ってよいのは**取引日の 06:00〜24:00 だけ**。

- 取引日は bot と同じ休場カレンダー(`hard_limits.yaml` の `session_hours`: 土日 + `holidays`、
  `calendar_through` を過ぎたら休場扱い)。カレンダーが読めないときは触らない側に倒す。
- 06:00 は閉局(03:30〜)明けに余裕を持たせた値。公式の開局時刻は手元の資料で確かめられていない。
  集計窓の起点は 5:30(`apiusage.WindowStart`)。
- メンテナンス・時間外の応答は `p_errno=-62`(「システム、情報提供時間外。」)。ログインも照会も同じ値で返る。
  受けたら叩き続けない。

| 経路 | 休場日・時間外の扱い |
|---|---|
| 朝ジョブ(launchd `com.stockbot.morning`) | 平日だけ発火する。平日の祝日・06:00 前・カレンダーが読めない日は `fetch-daily` を撃たない(`tachibana_off_hours`・`~/.stockbot/hard_limits.yaml` を読む) |
| `make start` の起動前処理(catchup) | 同じ判定で `fetch-daily` を撃たない(repo の `configs/hard_limits.yaml` を読む) |
| bot の価格ループ・reconcile・板の写し・期日の置き直し・引け後ジョブ | 取引日・取引時間で絞っている |
| bot のそれ以外(守りの自動復旧・selector の余力照会・日次ログイン・日足の定期取得・起動時の login / 同期 reconcile / 日足取得)と `live-probe` / `loanable-fetch` | 見ていない。bot を休場日・時間外に動かさないことで守る |

**どのプロセスがいつ叩いたか**は `~/.stockbot/state/api-usage/` に、集計窓(5:30 起点)ごと・プロセスごとの
ファイルで残る(1 回叩くたびに書く)。立花のアダプタを作るコマンドは全部ここに残す
(`TestEveryTachibanaCommandRecordsUsage`)。保持する窓の数は `apiusage.keepWindows`。

## 2. 共通エンベロープ / エンコード(トランスポート層)

- **文字コード Shift-JIS**: リクエスト = JSON を UTF-8 → Shift-JIS 変換後 `url.QueryEscape` し GET の RawQuery に載せる(ボディ無し)。
  レスポンス = body が Shift-JIS → UTF-8 に decode してから `json.Unmarshal`。`golang.org/x/text/encoding/japanese.ShiftJIS`。
- **`p_no`(送信通番)** `json:"p_no,string"`: login 応答 p_no で初期化、以後厳密に単調増加。`session.mtx` 下で Lock → ++ → 送信。
  巻き戻し / 重複 = `p_errno='6'`。
- **`p_sd_date`(送信日時)** 形式 `2006.01.02-15:04:05.000`。サーバ時刻と ±30 秒超過で `p_errno='8'`。`clock.Clock` を adapter 層に注入・
  ホスト NTP 同期前提(未来方向の許容幅は §7)。
- **`sJsonOfmt=6`**(= 2 Wrapped | 4 WordKey)必須。WordKey 無しだと項目番号キーで返り、全 `sXxx` 前提が崩れる。
- **`sCLMID`** で応答振り分け(Req=`CLMxxxRequest`、Ack=`CLMxxxAck`)。業務エラーは各 CLM の `sResultCode` / `sWarningCode`。
- **`p_errno`**: `0`=OK / `1`=データ無 / **`2`=無効セッション(→ 再ログイン + リトライ)** / `6`=通番巻戻し / `8`=時刻ズレ /
  `9`=停止中 / `-1` 引数 / `-12` システム停止 / `-62` 提供時間外。
- **数値は文字列で来る**: `StringFloat64` / `StringInt` のカスタム Unmarshal。特殊値 `*`=変更なし / `-`=なし / ``=空 を欠損扱い。
  `OrderPrice` Marshal: 未指定 → `*` / なし → `-` / 成行 → `0` / 指値 → 値段文字列。日付 `Ymd`(YYYYMMDD)/ `YmdHms`。
- **全 CLM が同じ転送形式**(`t.request` 経由 = limiter と `p_no` の直列化に載る)。マスタ取得は個別マスタ問合
  (`CLMStkGetIssueSizyouMstKabu` / `CLMStkGetIssueSizyouKiseiKabu`)の単発 JSON で、chunked stream の専用経路は無い。
  完全性の担保は terminator ではなく下限件数ガード([adapter.md](../architecture/layers/adapter.md))。
- **第二パスワード `sSecondPassword`** が New / Correct / Cancel 全発注系で必須。secret(env)注入・ログ厳禁
  (SetDebug はリクエスト全文を log するので本番は強制 OFF)。

## 3. port.Broker / LiveBroker マッピング

| port メソッド | 立花 CLM(URL) | 可否 | 要点 |
|---|---|---|---|
| GetTicker / GetTickers | `CLMMfdsGetMarketPrice`(Price) | 可 | Last=`pDPP`, Bid=`pQBP`(空 → `pGBP1`), Ask=`pQAP`(空 → `pGAP1`)。寄り前は Last ← `pPRP`(前日終値)。複数銘柄は §1.5-4 |
| GetKlines | `CLMMfdsGetMarketPriceHistory`(Price) | 日足のみ | `pDOP/pDHP/pDLP/pDPP/pDV`、分割調整は `pXxxxK`。1m/5m/1h は error(非対応)。遡及本数は `fetch-daily -bars` の既定(250 本)まで返る |
| GetAccountMargin | `CLMZanKaiKanougaku`(+`ZanKaiSummary`・`CLMZanRealHosyoukinRitu`・`CLMZanShinkiKanoIjiritu`)(Req) | 可 | AvailableJPY=`sSummaryGenkabuKaituke`(現物買付可能額)。`sHusokukinHasseiFlg`=不足金で発注前ゲート。現物のみなら MarginRatio は 1.0 |
| GetPositions | `CLMGenbutuKabuList` / `CLMShinyouTategyokuList`(Req) | 可 | 1 銘柄 = 1 Position。現物は Sym=`sUriOrderIssueCode`, Qty=`sUriOrderZanKabuSuryou`, Entry=`sUriOrderGaisanBokaTanka`, Side=BUY 固定。現物に建玉番号は無いので BrokerPositionID は symbol 合成(例 `genbutu:7203:特定`) |
| GetActiveOrders | `CLMOrderList`(Req) | 可 | working = 全部約定でない かつ 残数量 `sOrderCurrentSuryou` > 0。約定状態だけでは取消済と区別できない(取消済も未約定 "0" のまま)。`sOrderGyakusasiOrderType`(0 通常 / 1 逆指値 / 2 通常 + 逆指値)を `order.Order.HasStopLeg` に写す — 守り(SL)を持つ注文の判別子 |
| GetExecutions | `CLMOrderList` → `CLMOrderListDetail`(Req) | 可 | Detail の `aYakuzyouSikkouList[]` を展開。手数料は Detail でしか出ない(`sBaiBaiTesuryo+sShouhizei`) |
| PlaceOrder(買い) | `CLMKabuNewOrder`(`BaibaiKubun='3'`)(Req) | 可 | 成行: `Condition='0'`+`OrderPrice='0'`、数量を単元丸め、`SecondPassword`。orderID → `sEigyouDay` を内部マップ保存(取消 / 詳細で必須) |
| ClosePosition(売り) | `CLMKabuNewOrder`(`BaibaiKubun='1'`)(Req) | 可 | 専用 Close 無し = 売り新規(信用は返済区分)。FilledPrice は即返らない → 0 を返し ResolveExecution で確定(port 契約どおり) |
| CancelOrder | `CLMKabuCancelOrder`(Req) | 可 | `sOrderNumber`+保存済 `sEigyouDay`+`SecondPassword`。port は EigyouDay 引数を持たないので内部マップ必須 |
| RefreshToken | logout → login(§1) | 可 | 立花では実装必須。p_errno==2 / 起動 / 日次でトリガ |
| ResolveExecution | `CLMOrderList` → `CLMOrderListDetail`(Req) | 可 | `sOrderCurrentSuryou==0` で fill 確定。分割約定は Σ(price×qty)/Σqty 加重平均 |
| **PlaceSettleOCO** | `CLMKabuNewOrder` `GyakusasiOrderType='2'`(Req) | fail-close | フル OCO(TP 指値 + SL 逆指値)は `STOCKBOT_TACHIBANA_OCO_VERIFIED=1` まで error。stop-only(`'1'`)は検証前の守り。多日建玉は `sOrderExpireDay` に期日を入れる(`settleOrderExpiry`) |
| ResolveSettleLegs | `CLMOrderList`(Req) | 一部 | 立花ダブル注文は脚が割れず単一 `sOrderNumber` → tp/sl 両方に同一 ID を返す。生存判定 = `sOrderGyakusasiOrderType` ∧ 残数量 > 0(`orderIsActive`) |
| **ListProtectiveOrders** | `CLMOrderList`(Req) | 可 | 板に残る決済注文を期日つきで返す。`sOrderOrderExpireDay` が `"0"` / 空 / 解釈不能なら zero(= 当日限り扱い)— 読めなかったものを「先の日付」と読むと、切れる守りを「まだ大丈夫」と誤判定する |

注文の訂正(`CLMKabuCorrectOrder`)は使わない。訂正では発注日起点の期日の天井を超えられないので、
守りの期日は取消 → 同条件で再発注で置き直す(`command.ReplaceProtectiveOrder`・寄り前だけ)。

### 3.1 注文訂正 `CLMKabuCorrectOrder` のセンチネル

訂正注文のセンチネルは新規注文と違う。新規のセンチネルを訂正に転用しない。

| 項目 | 新規注文 | 訂正注文 |
|---|---|---|
| `sGyakusasiZyouken` | `0`=指定なし / 値=トリガー価格。`*` は定義に無い(拒否される) | `*`=変更なし / `0`=**成行に変更(トリガーを消す)** / 値=変更 |

訂正で `0` を送るとトリガーが消えたまま注文だけが板に残り、`sOrderGyakusasiOrderType` は `'1'` / `'2'` のままなので
`orderHasStopLeg` も reconcile も「守りあり」と読み続ける。`sEigyouDay` は照会の応答(`sOrderSikkouDay`)から運ぶ
(プロセス内 map は発注したプロセスにしか無い)。

### 3.2 株式分割・併合の権利落ち(立花の公式の扱い)

出典: [信用建玉の取扱い](https://www.e-shiten.jp/TorihikiRule/margin3/kenrisyori.html) /
[注文の注意事項](https://www.e-shiten.jp/TorihikiRule/rule/order-op.html)。

1. 分割・併合の権利落ちがある銘柄の繰越注文は、翌朝のバッチ(3:30〜5:30)で「権利落ちのため」失効する
   (板の守りは消える → `RearmUnguarded` が置き直す)。
2. 整数倍の分割は、権利付最終日の夜間に建玉を株数 ×r・建単価 ÷r へ言い直す(live は建玉照会でこれを確かめてから台帳を言い直す)。
3. 整数倍以外(1:1.2 / 1:1.5 など)は株数を増やさず、建単価から権利処理価格を差し引く。比を確定できないので
   bot は言い直さず、その建玉の自動決済を止めて緊急停止する(`command.SplitGuard`)。

公式どおりに動くかの実測は無い。分割を事前に知る手段は無い(マスタの権利落ちフラグは配当落ちにも立つ)。
bot 側の仕組みは `position.SplitAdjusted` / `market.ExDateSplitRatio` / `command.SplitGuard`。

### 注文状態コードの実値

実注文の照会で計測した値。`sOrderStatusCode` に「常駐 = `50`」のような値は現れない。

| | `sOrderStatus` | `sOrderStatusCode` | `sOrderCurrentSuryou`(残数量) |
|---|---|---|---|
| 有効(板に常駐) | 未約定 | `1` | 100 |
| 取消済 | 取消完了 | `7` | **0** |

両者とも約定状態(`sOrderYakuzyouStatus`)は "0"(未約定)なので、約定状態だけでは区別がつかない。
`orderIsActive` は残数量で判定する — 状態コードの全体表を一次資料で確認できていない以上 whitelist は書けないが、
残数量はコード表に依存しない。

他の項目: `sOrderGyakusasiZyouken`(逆指値トリガー価格)・`sOrderOrderExpireDay`(注文期日)・
`sOrderCorrectCancelKahiFlg`(訂正取消可否)。生レコードは `cmd/live-probe -probe-order-rows` で見る。

### 守り判定の不変条件

**「決済側に注文がある」は「守られている」ではない。** 利確指値だけが板にある建玉は下方向に裸で、SL はどこにも存在しない。
守りを数える述語は**必ず `HasStopLeg` を要求する**:

- `protectiveOrderIsResting`(entry saga の守り実在確認)
- `Reconcile.hasWorkingCloseOrder`(`external_adopt_unprotected` の trip 判定)
- `cmd/live-probe` の「守りあり」表示

side と数量だけを見ると、売り注文が 1 本でもあれば守りありと読んでしまう(立花の OCO は注文番号 1 本に通常 + 逆指値を
同居させるので、逆指値が「無い」ようにも見える)。`HasStopLeg` が取れない構成では false(fail-close)—
「確かめられなかった」を「大丈夫だった」と読むと、守り検査が丸ごと無効化される。

## 4.5 wire の注意: レコード 0 件のリストは文字列で返る

レコード 0 件のリスト項目は空配列ではなく文字列(`""` / `"*"`)で返る。素の `[]T` で受けると json エラーになり
「建玉なし」が取得失敗扱い → Reconcile が fail-close し平常時に bot が止まる。`slist[T]`(tachibana.go)で吸収し、
`aGenbutuKabuList` / `aShinyouTategyokuList` / `aOrderList` / `aYakuzyouSikkouList` / `aCLMMfdsMarketPrice` /
`aCLMMfdsMarketPriceHistory` に適用している。

## 5. `STOCKBOT_TACHIBANA_OCO_VERIFIED` を立てる前の確認項目

参照系(login / RefreshToken / GetTicker / GetKlines / GetPositions / GetAccountMargin)は `cmd/live-probe`(発注 API を持たない)で
確かめられる。発注と守りは残高のある口座(デモまたは最小実弾)で、次の順に確かめてから人間がフラグを立てる:

1. 成行買い 1 単元(`sResultCode='0'`、`sOrderNumber` / `sEigyouDay` が返る)と約定解決(`sOrderCurrentSuryou==0` → Detail で FilledPrice / Fee)。
2. **stop-only 逆指値返済売り**(`'1'` + トリガー価格 + 発動後成行): (a) 受理 (b) 板に常駐(残数量 > 0)
   (c) 現値より下にトリガーを置いた売り stop が即約定しない(= 発動方向「以下」が正)。
3. **bot 非依存**: 2 の逆指値を bot 停止状態でトリガーを跨がせ、実際に約定するか。これで初めて「守りが broker 側に残る」が実証される。
4. 通常売り返済の一巡 + 未約定逆指値の CancelOrder。
5. **フル OCO**: `'2'` ダブル注文で TP 指値 + SL 逆指値を決済売りに同梱。(a) 受理 (b) 片脚約定で他脚が自動失効する (c) 脚別番号か単一か。
6. **日跨ぎ**: 期日付き(`sOrderExpireDay`)の逆指値 / ダブル注文が翌営業日の寄り後も `GetActiveOrders` で working か。
   不可なら多日保有戦略を live に上げない。

## 6. 実装リスク(事故り得る点)

1. **逆指値「発動方向」**(§0): API に UnderOver 相当フィールドは無く、売買区分から暗黙決定(返済 / 現物売りは下落タッチで発火)。
   残るは「売り stop が現値より下のトリガーで即約定しない」実挙動の確認のみ。
2. **`OrderPrice` 単一フィールド**: ダブル注文で TP 指値を OrderPrice に入れる解釈が未実証 → 誤ると「利確のつもりが寄り成り買い増し」事故。
   フル OCO は実証まで封印。
3. **p_no 競合**: RefreshToken の Session 丸ごと差し替えで in-flight と p_no 空間が混線すると `'6'` が多発する。
   再ログインは in-flight をドレインしてから差し替える。
4. **Shift-JIS ラウンドトリップ**: 第二 PW に Shift-JIS 不能文字があると発注不能。約定価格 / 数量の数値パース失敗は欠損として明示扱い。
5. **SecondPassword ログ漏れ**: debug はリクエスト全文を log する。本番 debug を構造的に禁止 + マスク。
6. **現物 BrokerPositionID 合成 と売却可能数突合**: 課税区分混在 / external 保有で破綻し得る。ナンピン禁止が効けば 1 銘柄 1 ポジで実害小。

## 7. open gaps(人間 / デモ / 最新スペックで確定する)

- 逆指値の発動方向の実挙動(§6-1)。
- フル OCO(ダブル注文)の受理・片脚自動失効・脚別番号(§5-5)。
- `p_sd_date` 未来方向の許容幅。`sSecondPasswordOmit` で第二 PW が不要になるか。`AccountMargin.Equity` の合成式
  (買付可能額での近似。正確な純資産には評価額合計が要る)。
- 維持率 CLM(`CLMZanRealHosyoukinRitu`)の CLM 名・フィールド名は公式サンプル未収載。
- 追証の発生ラインと差し入れ期限。公式の案内で明示されているのはロスカット 15% のみで、「維持率 30%」が追証ラインか
  委託保証金維持率の別基準かは未確定。`hard_limits.margin.maintenance_ratio` の妥当性はこれで再判定する
  (30% は 15% より十分手前なので、未確定のままでも fail-safe 側)。

---
**結論**: 立花は Mac ネイティブで port.Broker / LiveBroker を実装できる。守りは逆指値(stop)で broker 側 = 最低条件を満たし、
フル OCO は検証まで fail-close。§5 の守りの 5 点(受理・常駐・方向・bot 停止下約定・日跨ぎ生存)を実証するまで
`STOCKBOT_TACHIBANA_OCO_VERIFIED` は立てない。
