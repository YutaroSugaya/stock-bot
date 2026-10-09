package market

// 東証の呼値は銘柄で2種類(細かいテーブルとそれ以外の粗い TickSize)。config は TP/SL を tick 数で書き
// 紙執行の滑りも `simulated_slippage_ticks × 呼値` なので、テーブルを間違えると利確・損切りの位置と
// 執行コストがその倍率でずれる(3,000〜5,000円帯で 5倍、1,000円以下で 10倍)。
//
// **どの銘柄がどちらかは手で並べない。** 表は cmd/tick-table が hard_limits.allowed_symbols 全銘柄を
// 実約定値で判定して生成する(tick_fine_gen.go / 規則は InferTickRegime)。手書きだった頃は
// 両方向へ壊れていた: 表が旧・静的222銘柄ぶんしか無く日次ユニバースの新規46銘柄が全て未収録
// (往復コスト中央値 50.6bps 対 継続銘柄 8.4bps)、逆に実際は粗い4件(2395/3865/8628/8803)を細かい側に
// 載せていた(実弾なら刻み違反の TP/SL を作って broker に弾かれる向き)。
//
// 未知の銘柄は粗いテーブルへ倒す — 粗いグリッド上の値段は細かいグリッドにも必ず乗る(逆は起きる)。
// 出典は JPX「呼値の単位」だが、**構成銘柄リストを外部から取ってこない**のが設計の要点:
// 年次見直し(毎年10月)の追従漏れがそのまま穴になるので、実データを正とする。
// 表に無い銘柄は false = 粗いテーブル(fail-safe)。
func IsFineTickSymbol(symbol string) bool {
	_, ok := fineTickSymbols[symbol]
	return ok
}

// 銘柄不明(空文字)は粗いテーブル — 刻み違反の注文を作らない側へ倒す。
func TickSizeOf(symbol string, price float64) float64 {
	if IsFineTickSymbol(symbol) {
		return fineTickSize(price)
	}
	return TickSize(price)
}

func RoundToTickOf(symbol string, price float64) float64 {
	return roundTo(price, TickSizeOf(symbol, price))
}

// 細刻みテーブル。境界は下側の帯に含む(1,000円「以下」/ 1,000円「超」)。
func fineTickSize(price float64) float64 {
	switch {
	case price <= 1000:
		return 0.1
	case price <= 3000:
		return 0.5
	case price <= 10000:
		return 1
	case price <= 30000:
		return 5
	case price <= 100000:
		return 10
	case price <= 300000:
		return 50
	case price <= 1000000:
		return 100
	case price <= 3000000:
		return 500
	case price <= 10000000:
		return 1000
	case price <= 30000000:
		return 5000
	default:
		return 10000
	}
}
