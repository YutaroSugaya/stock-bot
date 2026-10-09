package clock

import "time"

// JST is the Asia/Tokyo location, resolved once at init.
//
// tzdata が読めない環境(distroless / CGO 無効の一部構成)では LoadLocation が
// 失敗する。そこで UTC に落ちると日足の暦日境界も 14:50 引け前フラット化も
// 9 時間ずれるので、固定 +09:00 へフォールバックする(日本の DST は 1951 年で終了)。
var JST = func() *time.Location {
	if l, err := time.LoadLocation("Asia/Tokyo"); err == nil {
		return l
	}
	return time.FixedZone("JST", 9*3600)
}()
