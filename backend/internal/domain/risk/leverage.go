package risk

// WithinGrossNotionalCap reports whether adding newGrossJPY keeps the account's
// total 建玉金額 within collateralJPY × ratio.
//
// ratio<=0 は無効(既存構成・research を壊さない)。
//
// 🛑 保証金が読めない(0 以下)ときは **false = 拒否**。照会失敗のたびに上限が
// 消えるのは、レバ規律が一番要る場面(口座が不安定なとき)で規律が外れることを意味する。
// margin_status_unavailable と同じ極性で fail-close。
func WithinGrossNotionalCap(collateralJPY, openGrossJPY, newGrossJPY int, ratio float64) bool {
	if ratio <= 0 {
		return true // 無効
	}
	if collateralJPY <= 0 {
		return false // fail-close
	}
	cap := int(float64(collateralJPY) * ratio)
	return openGrossJPY+newGrossJPY <= cap
}
