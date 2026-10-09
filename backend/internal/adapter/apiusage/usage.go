// Package apiusage は立花 API への wire 呼び出し回数を、**立花と同じ集計単位**で
// 永続的に数える。
//
// なぜ必要か: 立花には 1 日の API 利用回数の上限があり、「その日いくつ叩いたか」に
// 答えられる必要がある。プロセス内のカウンタでは観測の定義が3点ずれる —
//
//	① プロセス内 atomic なので make stop/start でゼロに戻る
//	② bot のカウンタなので朝の fetch-daily(1,551回)が丸ごと入らない
//	③ 暦日で考えがちだが、立花の集計期間は開局時間帯(5:30〜翌3:30)
//
// ここは3点とも立花側に合わせる: 窓は 5:30 起点、プロセスを跨いで合算、CLMID 別に残す
// (立花側の集計が CLMID 単位なので、突き合わせられる粒度でないと原因特定に使えない)。
//
// 実装方針: **プロセスごとに別ファイルへ書き、読むときに合算する**。ロックを持たない
// ので、bot と fetch-daily が同時に走っても互いを踏み潰さず、ロック待ちが発注経路の
// レイテンシに乗ることもない。
package apiusage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"stockbot/backend/internal/domain/clock"
)

// 立花の集計期間は開局している時間帯(5:30〜翌3:30)。
// 3:30〜5:30 の閉局中は API が動かないが、万一の呼び出しは直前の窓へ寄せる
// (「どの窓にも属さない回数」を作らない = 合計が必ずどこかに現れる)。
const windowStartHour, windowStartMinute = 5, 30

// clmLogin は立花側の集計に現れない唯一の CLM(auth ホスト宛のみ別扱い)。
// bot は送っている以上数えるが、立花側の数字と比べるときはこれを除く。
const clmLogin = "CLMAuthLoginRequest"

// 保持する窓の数。台帳ではなく運用の観測点なので、直近ぶんだけあれば足りる。
const keepWindows = 14

// BrokerDailyRequestCap は立花が求める 1 日あたりの API 利用回数の上限(1 万回以下)。
// **bot 側の予算ではなく broker 側の上限**。
//
// 🛑 ここに置くのは、これを見る側が **1 プロセスではない**から。bot は dashboard に
// 「N / 10,000」を出し、`fetch-daily` は自分の 1,550 リクエストを打つ前に残枠を見る。
// 数を 2 か所に書くと、片方だけ直したときに**通信量の判断が静かに食い違う**
// (数え口が既にこのパッケージに集まっているので、上限も同じ場所に置く)。
const BrokerDailyRequestCap = 10000

// WindowStart は t が属する集計窓の起点(JST 5:30)。
func WindowStart(t time.Time) time.Time {
	l := t.In(clock.JST)
	start := time.Date(l.Year(), l.Month(), l.Day(), windowStartHour, windowStartMinute, 0, 0, l.Location())
	if l.Before(start) {
		start = start.AddDate(0, 0, -1)
	}
	return start
}

// Usage は 1 つの集計窓ぶんの実数。
type Usage struct {
	WindowStart time.Time        `json:"window_start"`
	Total       int64            `json:"total"`
	ByCLMID     map[string]int64 `json:"by_clmid"`
	// TotalExcludingLogin は立花側の集計と直接比較するための数(立花側は login を数えない)。
	TotalExcludingLogin int64 `json:"total_excluding_login"`
}

// Counter は 1 プロセスぶんの数え口。Record は wire 送信のたびに呼ばれるので、
// 失敗しても**絶対に取引を止めない**(観測のために発注経路が落ちるのは本末転倒)。
type Counter struct {
	dir      string
	name     string // 役割の識別子(stockbot / fetch-daily)
	instance string // このプロセス固有の識別子(起動時刻+pid)
	clock    clock.Clock

	mu     sync.Mutex
	window time.Time
	counts map[string]int64
}

// Open は dir 配下にこのプロセスぶんのファイルを持つカウンタを作る。dir が使えない
// 場合も nil を返さない(呼び出し側に nil 判定を強いると数え漏れの穴になる)。
func Open(dir, name string, c clock.Clock) *Counter {
	if c == nil {
		c = clock.System()
	}
	now := c()
	ct := &Counter{
		dir:  dir,
		name: sanitize(name),
		// 同じ name で開き直しても別ファイルになるようにする(再起動で消さない)。
		instance: fmt.Sprintf("%s-%d", now.In(clock.JST).Format("150405"), os.Getpid()),
		clock:    c,
		counts:   map[string]int64{},
	}
	ct.window = WindowStart(now)
	_ = os.MkdirAll(dir, 0o700)
	ct.prune()
	return ct
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

func sanitize(s string) string {
	s = unsafeName.ReplaceAllString(s, "-")
	if s == "" {
		s = "proc"
	}
	return s
}

func windowKey(t time.Time) string { return t.Format("20060102") }

// 🛑 ファイル名は **プロセスごとに一意**でなければならない。name だけだと
// `make stop` → `make start` が同じ名前("stockbot")で開き直すので、
// **前のプロセスのぶんを上書きして当日のカウントがゼロに戻る**
// (実測: 3,320 → 4)。要件そのものが壊れる。
// pid だけでは再利用で衝突しうるので、起動時刻(窓内での HHMMSS)も混ぜる。
func (c *Counter) file() string {
	return filepath.Join(c.dir, windowKey(c.window)+"-"+c.name+"-"+c.instance+".json")
}

// Record は wire 1 本を数える。clmid が空でも 1 本は 1 本として数える
// (数えられない送信を作らないため — 過少申告は「原因不明の差」に直結する)。
func (c *Counter) Record(clmid string) {
	if clmid == "" {
		clmid = "unknown"
	}
	c.mu.Lock()
	if w := WindowStart(c.clock()); !w.Equal(c.window) {
		c.window = w
		c.counts = map[string]int64{}
	}
	c.counts[clmid]++
	snap := c.snapshotSelfLocked()
	path := c.file()
	c.mu.Unlock()

	// 書けなくてもプロセス内の数字は保つ(次の Record で書き直しを試みる)。
	_ = writeAtomic(path, snap)
}

func (c *Counter) snapshotSelfLocked() Usage {
	u := Usage{WindowStart: c.window, ByCLMID: map[string]int64{}}
	for k, v := range c.counts {
		u.ByCLMID[k] = v
		u.Total += v
		if k != clmLogin {
			u.TotalExcludingLogin += v
		}
	}
	return u
}

// Snapshot は**現在の窓の全プロセスぶんの合算**。自プロセスの値はメモリから、
// 他プロセスのぶんはファイルから読む(自分のファイルは二重計上しない)。
func (c *Counter) Snapshot() Usage {
	c.mu.Lock()
	if w := WindowStart(c.clock()); !w.Equal(c.window) {
		c.window = w
		c.counts = map[string]int64{}
	}
	out := c.snapshotSelfLocked()
	self := filepath.Base(c.file())
	window := c.window
	c.mu.Unlock()

	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return out
	}
	prefix := windowKey(window) + "-"
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || n == self || !strings.HasPrefix(n, prefix) || !strings.HasSuffix(n, ".json") {
			continue
		}
		var u Usage
		b, err := os.ReadFile(filepath.Join(c.dir, n))
		if err != nil || json.Unmarshal(b, &u) != nil {
			continue // 書き込み途中/壊れたファイルは黙って飛ばす(観測が取引を止めない)
		}
		for k, v := range u.ByCLMID {
			out.ByCLMID[k] += v
			out.Total += v
			if k != clmLogin {
				out.TotalExcludingLogin += v
			}
		}
	}
	return out
}

// prune は古い窓のファイルを落とす(観測点なので直近ぶんだけ残す)。判定は
// **経過日数**で行い、名前を解釈できないファイルには触らない(このディレクトリに
// 人間が置いた控えを消さない)。
func (c *Counter) prune() {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	cutoff := c.window.AddDate(0, 0, -keepWindows)
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || len(n) < 8 || !strings.HasSuffix(n, ".json") {
			continue
		}
		day, err := time.ParseInLocation("20060102", n[:8], clock.JST)
		if err != nil || !day.Before(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(c.dir, n))
	}
}

// 中断で壊れた JSON を残さない(読み手は壊れたファイルを飛ばすが、飛ばした分は
// 黙って過少申告になるので、そもそも作らない)。
func writeAtomic(path string, u Usage) error {
	b, err := json.Marshal(u)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// DefaultDir はカウンタの置き場。**Desktop 配下には置かない** — macOS TCC で
// launchd 起動のバイナリが触れず(実測: 触れた瞬間 SIGKILL)、朝の
// fetch-daily のぶんが黙って数え漏れる。運用データと同じ ~/.stockbot 配下に置く。
// bot と fetch-daily が**同じディレクトリ**を見ることが合算の前提なので、
// 置き場の決定は 1 か所にまとめる。
func DefaultDir() string {
	if v := os.Getenv("STOCKBOT_API_USAGE_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "stockbot-api-usage")
	}
	return filepath.Join(home, ".stockbot", "state", "api-usage")
}
