# 口座なしでエッジ検証を始める — データ取得(A: 無料 / B: J-Quants)

> **証券口座・Postgres・実弾は不要**。バックテストハーネス(`cmd/backtest` + `cmd/edge-eval` / `cmd/edge-judge`)は
> 完全オフラインで、過去ローソク足 CSV だけで動く。必要なのは口座ではなく**データ**。
> 判定の規律は [EDGE_METHODOLOGY.md](EDGE_METHODOLOGY.md)。本書はその前段 = データをハーネスの CSV 形式に落とす手順。

---

## 0. ハーネスが食う CSV 形式(これに合わせる)
セミコロン区切り。ヘッダ行は任意(自動判定でスキップ)。時刻は JST、内部で UTC 保存。

- 日足(`-interval 1d`): `DateJST;Open;High;Low;Close;Volume`(日付 `YYYY-MM-DD`)
- 分足(`-interval 1m|5m|1h`): `TimestampJST;Open;High;Low;Close;Volume`(`YYYY-MM-DD HH:MM:SS`)

例(日足):
```
DateJST;Open;High;Low;Close;Volume
2025-04-01;3100;3180;3090;3150;4200000
```

---

## データは 2 系統で混ぜない

| ディレクトリ | 中身 | 用途 |
|---|---|---|
| `backend/data` | 立花の日足(`cmd/fetch-daily` が書く。分割未調整)。実体は `~/.stockbot/data` への symlink | **運用**。bot が読む唯一のソース |
| `backend/data_research` | 調整済みの研究用日足(gitignore・読者が用意する) | **検定**。`cmd/backtest -csv` にこちらのファイルを渡す |

- `backend/data` へは手で書かない。運用の日足は分割未調整なので、bot 側は `market.ChainLinkSplits` で分割を吸収する。
- `fetch-daily` は書き換える前に必ず退避する(`-backup-dir`・CSV は唯一のコピー)。
- `bench_topix.csv`(TOPIX ベンチ。`cmd/edge-eval -benchmark` の超過 gate 用)は TOPIX 連動 ETF の日足。
  `STOCKBOT_BENCH_SYMBOL` を `.env` に置くと `fetch-daily -bench-symbol` が毎朝継ぎ足す(空だと更新されない)。
  ETF の分割は `mergeBenchmark` の chain-link で分割前の履歴を比で調整するので、リターンは連続(水準だけが変わる)。
  分割で説明できない水準差は fail-close で拒否される(別系列の接ぎ木を防ぐ)。
  chain-link は出来高も比で調整するが、ベンチはリターンにしか使わないので実害はない。

---

## 方法A — 無料データ(口座も API キーも不要・パイプライン確認向け)

> 目的: **まずハーネスが通ることを確認**する。無料・キー不要で日本株日足 CSV を得る。
> 調整・生存バイアスの保証が弱いので採否判断には使わない(方法B のクリーンデータで判定)。

### A-1. stooq
stooq は自動取得の経路が安定しないので使わない。方法A2(Yahoo)を使う。

### A2. Yahoo Finance(キー不要・調整値あり)
Yahoo Finance の chart データは日足 OHLCV + 調整値(adjclose)を含む。**データは読者自身が提供元の利用規約の
範囲で取得すること**(本リポジトリは取得手段を提供しない)。ハーネスが要るのは §0 の CSV 形式だけで、
Yahoo chart 形式の JSON(`{"data": {"<sym>": [[date, o, h, l, c, v], ...]}}`)を §0 の CSV に変換して
既存ファイルへ日付でマージするのが `scripts/merge_daily_from_json.py`。

- **調整**: `f = adjclose/close` を OHLC に乗じて分割 / 配当調整する(価格系シグナルの split 偽ギャップを除去)。
- 日付は JST の `YYYY-MM-DD`、`;` 区切りで `DateJST;Open;High;Low;Close;Volume`。
- 出力は `backend/data_research/<sym>_daily.csv` に保存。`backend/data` へは書かない(bot が読む立花単一ソース)。
- Yahoo も調整規則・生存バイアスは保証が弱い(現存銘柄のみ)。採否判断は方法B で。

### A-2. 検定を回す(コード強制スクリーン `cmd/edge-eval` 経由)
手組みの集計で verdict を作らない(EDGE_METHODOLOGY §0)。銘柄ごとに `cmd/backtest -json` を回し、`cmd/edge-eval` に食わせる:
```bash
cd backend
OUT=/tmp/edge; mkdir -p "$OUT"
for f in data_research/[0-9]*_daily.csv; do
  s=$(basename "$f" _daily.csv)
  go run ./cmd/backtest -csv "$f" -symbol "$s" -interval 1d \
    -strategy time_series_momentum -tp 100 -sl 50 -holding multiday \
    -spread-ticks 1 -fee-rate 0.05 -json > "$OUT/${s}.json"
done
go run ./cmd/edge-eval -glob "$OUT/*.json" -benchmark data_research/bench_topix.csv \
  -label my_candidate
# ¥1M 正規化 / 3レジーム / day-block CI / holdout 予約 / β gate をコードが強制する
```
これが通れば「口座なしでエッジ検証が回る」状態。あとは銘柄を増やし、データをクリーンにする(方法B)。

---

## 方法B — J-Quants(クリーンデータ・本検定向け / 口座は不要)

> J-Quants は JPX 公式データ API。**登録は要るが証券口座ではない**(無料枠あり、PIT 財務 / 決算時刻や
> 長期履歴は有料プラン)。調整後終値・上場銘柄一覧が取れるので、生存バイアス / 分割調整に対処できる。
> 無料枠のデータ遅延・履歴範囲とエンドポイント仕様は改定され得る(J-Quants の現行ドキュメントで確認)。

### B-1. 認証(メール / パスワードは J-Quants 登録のもの。口座とは別)
```bash
export JQ_MAIL='you@example.com'
export JQ_PASS='********'
REFRESH=$(curl -s -X POST 'https://api.jquants.com/v1/token/auth_user' \
  -H 'Content-Type: application/json' \
  -d "{\"mailaddress\":\"$JQ_MAIL\",\"password\":\"$JQ_PASS\"}" | jq -r .refreshToken)
IDTOKEN=$(curl -s -X POST "https://api.jquants.com/v1/token/auth_refresh?refreshtoken=$REFRESH" | jq -r .idToken)
echo "${IDTOKEN:0:12}…"   # 取得できていれば先頭が出る
```

### B-2. 日足 → ハーネス CSV(調整後を使う)
J-Quants の銘柄コードは 5 桁(7203 → `72030`)。`AdjustmentClose` 等が分割 / 配当調整後。
価格系シグナルは調整後 OHLC を使うのが安全(フィールド名と調整規則は J-Quants の現行ドキュメントで確認)。
```bash
echo "DateJST;Open;High;Low;Close;Volume" > 7203_daily.csv
curl -s -H "Authorization: Bearer $IDTOKEN" \
  'https://api.jquants.com/v1/prices/daily_quotes?code=72030' \
| jq -r '.daily_quotes[]
    | [.Date, .AdjustmentOpen, .AdjustmentHigh, .AdjustmentLow, .AdjustmentClose, .AdjustmentVolume]
    | @csv' \
| tr -d '"' | tr ',' ';' >> 7203_daily.csv
```
> ページングがある場合は応答の `pagination_key` を `&pagination_key=…` で辿って追記(J-Quants の現行ドキュメントで確認)。

### B-3. 生存バイアス除去(ユニバース復元)
現存銘柄だけで検定するとモメンタムが過大評価される(EDGE_METHODOLOGY §4)。`/v1/listed/info` で当時の上場銘柄
一覧(上場廃止含む)を取り、**検定期間の各時点で実在した銘柄**をユニバースにする。
```bash
curl -s -H "Authorization: Bearer $IDTOKEN" 'https://api.jquants.com/v1/listed/info' \
  | jq -r '.info[] | [.Code, .CompanyName, .MarketCode] | @csv' > universe.csv
```
(決算発表時刻の PIT が要る戦略は有料の財務 / 適時開示エンドポイントが必要。J-Quants の現行ドキュメントで確認)

### B-4. 検定を回す(方法A の A-2 と同じパイプライン)
銘柄ごとの `cmd/backtest -json` 出力を 1 ディレクトリに集め、`cmd/edge-eval -glob` に渡す
(プール・¥1M 正規化・universe_n・嘘発見器ゲートはコードが強制する)。

---

## どちらを使う?
| | 方法A2(Yahoo) | 方法B(J-Quants) |
|---|---|---|
| 口座 | 不要 | 不要(J-Quants 登録のみ) |
| API キー | 不要(取得は読者が規約の範囲で) | 要(無料枠可) |
| 調整・生存除去 | 調整△(adjclose)/ 生存除去 弱い | 対応可 |
| 用途 | **パイプライン確認 + 大型での粗検定** | **本検定(採否判断)** |

**進め方**: まず A でハーネスを通す → B でクリーンデータを揃え、嘘発見器ユニバース(TOPIX Core30 / 大型)で判定する。
しきい値は `judge.go`。採否は人間が commit する。

---

## 既知の注意(self-deception 防止)
- 汚れたデータは**必ず偽のエッジ**を生む(分割未調整・配当落ちギャップ・look-ahead・生存バイアス)。
- 粗いデータ(A)で「勝った」は信用しない。判定は **B のクリーンデータ + 嘘発見器ユニバース**で。
- `cmd/backtest` は read-only。書き込み(候補の保存等)は専用 `*_backtest` DB のみ(`SafeBacktestDSN`)。
- forward 記録の 1 分足(`backend/data/*_1m.csv`・`STOCKBOT_RECORD_CANDLES_DIR` の recorder)は価格ループの tick から
  集約しているので、高値 / 安値の忠実度は価格ループの間隔に直接依存する。間隔が粗いほど記録される値幅は系統的に狭く、
  日中の行き過ぎを狙う戦略ではトリガー回数と逆行の深さが同時に過小になる(方向が揃わないので「保守的に見ておけば安全」
  ではない)。間隔が違う区間を混ぜて検定しない(間隔は [TACHIBANA_API_NOTES.md §1.5](TACHIBANA_API_NOTES.md) の
  `batchedPriceInterval` と `quote_chunks`)。
