package command

import (
	"context"
	"time"
)

// ReasonEntryLockTimeout は口座単位の entry 排他を待ちきれずに見送った理由。
const ReasonEntryLockTimeout = "entry_lock_timeout"

// EntrySerializer は**口座単位**で entry 経路(snapshot の取得 → ゲート → 発注)を直列化する。
//
// 🚨 口座の建玉枠・同時保有銘柄数・建玉金額の上限は、どれも **snapshot の件数・金額**で
// 判定している。評価は銘柄ごとの goroutine から走るので、排他が無いと 2 銘柄が同じ空き枠を
// 見て両方建てる(TOCTOU・`TestEntryRace_ReproducesWithoutTheLock`)。**トラックに 1 つ**
// 持たせ、決済と守り(OnTick / reconcile / 置き直し)は対象にしない。
//
// 🛑 **待ちには上限がある**(wait)。立花の entry は約定待ちだけで最大 15 秒かかり、その間
// 他の銘柄の price goroutine がここで止まると、その銘柄の次のティック(= 決済判定)も遅れる。
// 上限を過ぎたら `entry_lock_timeout` で見送り、次のティックで取り直す。
// 排他の**内側**の処理(発注の往復)は取り消さない — 途中で ctx を切ると、約定の照会や
// 補償(cancel・巻き戻し)まで同じ ctx で落ち、約定した建玉を裸で残しうる。内側の長さは
// adapter の HTTP timeout と約定待ちの上限が縛る。
type EntrySerializer struct {
	sem  chan struct{}
	wait time.Duration
}

// NewEntrySerializer は待ちの上限つきの口座単位の排他を作る。
func NewEntrySerializer(wait time.Duration) *EntrySerializer {
	return &EntrySerializer{sem: make(chan struct{}, 1), wait: wait}
}

// acquire は排他を取る。wait を過ぎるか ctx が終われば ok=false(release は呼ばなくてよい)。
func (s *EntrySerializer) acquire(ctx context.Context) (release func(), ok bool) {
	t := time.NewTimer(s.wait)
	defer t.Stop()
	select {
	case s.sem <- struct{}{}:
		return func() { <-s.sem }, true
	case <-t.C:
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
}
