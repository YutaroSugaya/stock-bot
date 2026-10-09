package main

import (
	"errors"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/usecase/command"
)

// 🛑 守りの置き直しの**成功の証拠が消えてはいけない**。
//
// `runReplaceProtectiveExpiry` は寄り前の窓で周期的に回り、毎回 `set` で状態を上書きする。
// 置き直しが要る建玉は 1 度置き直せば次の周回では対象外になるので、**成功した直後の周回で
// renewed が 0 に戻る**。運用者が引け後に見るのはその 0 のほうで、
// 「一度も置き直しが要らなかった日」と区別が付かない。だから通算の証拠は消さずに持つ。
func TestRenewStateKeepsTheEvidenceOfASuccessfulRenewal(t *testing.T) {
	var s protectiveReplaceState
	at := func(h, m int) time.Time {
		return time.Date(2026, 8, 24, h, m, 0, 0, clock.JST)
	}

	s.set(at(9, 0), command.ReplaceProtectiveResult{Replaced: 2}, nil)  // 寄り後の初回 — 2 本延ばした(訂正 API が受理された)
	s.set(at(9, 30), command.ReplaceProtectiveResult{Replaced: 0}, nil) // 次の周回 — もう対象が無いので 0
	s.set(at(14, 30), command.ReplaceProtectiveResult{Replaced: 0}, nil)

	v := s.view()
	if v["ran"] != true {
		t.Fatalf("ran=%v, want true", v["ran"])
	}
	if got := v["renewed_total"]; got != 2 {
		t.Errorf("renewed_total=%v, want 2 — 成功の証拠が後の周回で消えている", got)
	}
	if v["last_renew_at"] == nil || v["last_renew_at"] == "" {
		t.Errorf("last_renew_at が空 — いつ受理されたのかが残らない")
	}
	if got := v["renewed"]; got != 0 {
		t.Errorf("renewed=%v, want 0(最後の周回ぶん)", got)
	}
}

// 🛑 失敗も消えてはいけない。失敗した周回のあとに「対象なし」の周回が来ると
// errors が 0 に戻り、受入スクリプトが緑を出す(= 守りが切れるのに合格と出る)。
func TestRenewStateKeepsTheEvidenceOfAFailure(t *testing.T) {
	var s protectiveReplaceState
	at := func(h, m int) time.Time {
		return time.Date(2026, 8, 24, h, m, 0, 0, clock.JST)
	}

	s.set(at(9, 0), command.ReplaceProtectiveResult{Replaced: 0}, []error{errors.New("訂正注文が拒否された")})
	s.set(at(9, 30), command.ReplaceProtectiveResult{Replaced: 0}, nil) // 建玉が決済されて対象が消えた等

	v := s.view()
	if got := v["errors_total"]; got != 1 {
		t.Errorf("errors_total=%v, want 1 — 失敗の証拠が消えている", got)
	}
	if got, _ := v["last_error"].(string); got != "訂正注文が拒否された" {
		t.Errorf("last_error=%q, want 訂正注文が拒否された", got)
	}
	if v["last_error_at"] == nil || v["last_error_at"] == "" {
		t.Errorf("last_error_at が空 — いつ失敗したのかが残らない")
	}
}

// 一度も走っていない状態を「エラーなし = 健全」と読ませない。
func TestRenewStateNeverRanIsNotHealthy(t *testing.T) {
	var s protectiveReplaceState
	v := s.view()
	if v["ran"] != false {
		t.Fatalf("ran=%v, want false", v["ran"])
	}
	if got := v["renewed_total"]; got != 0 {
		t.Errorf("renewed_total=%v, want 0", got)
	}
}
