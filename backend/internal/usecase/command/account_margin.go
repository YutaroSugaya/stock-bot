package command

import (
	"context"
	"sync"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/order"
)

// accountMarginRetryFloor は照会が失敗した後、次に wire を打つまでの最短間隔。
// 障害中に毎ティック再試行すると、削ったはずの通信が障害の形で戻ってくる。
const accountMarginRetryFloor = 30 * time.Second

// AccountMarginSource は口座の保証金照会。broker port のうち必要な 1 メソッドだけ取る。
type AccountMarginSource interface {
	GetAccountMargin(ctx context.Context) (*order.AccountMargin, error)
}

// AccountMarginCache は **口座単位**の保証金照会を 1 か所へ集める。
//
// 🚨 なぜ要るか: entry 経路の `FillCollateral` は **銘柄ごと・
// ティックごと**に `GetAccountMargin` を呼んでいた。armed 銘柄が毎ティック entry
// シグナルを出し、構造ゲートを通ってから `gross_notional_cap`(= 照会の**後**にしか
// 判定できないゲート)で落ちる状態になると、6 秒ごとに立花へ 3 リクエスト飛ぶ。
// 後場だけで口座照会が千回超 = wire 数千回 = 当日の上限(10,000)の 4 割を 1 銘柄が焼いた。
// 「構造ゲートを先に回す」で塞いだ穴が、照会の後ろにあるゲート経由で
// そのまま再来した形。**保証金は口座単位の量なので、銘柄数ぶん払う理由が無い。**
//
// 🛑 **維持率ブレーカーは間引かない。** あれは口座を見に行くことが目的なので
// `Refresh` は必ず wire を打つ。entry 経路が使うのは `Get`(キャッシュ優先)だけ。
//
// 更新のきっかけは 3 つ:
//   - 初回(まだ 1 度も読めていない)
//   - `Invalidate`(**約定 / 決済** — 口座が実際に変わったとき)
//   - maxAge 経過(イベントが一度も来ない日に値が丸一日固まるのを防ぐ最後の栓)
//
// ⚠ 代償: 建玉の含み損で新規建余力は約定と約定の間にも動くので、ここはその変動に
// 追随しない。ずれた結果は「broker に拒否される」か「建てられるのに見送る」の
// どちらかで、**守りが消える方向には倒れない**(維持率ブレーカーは実照会のまま)。
type AccountMarginCache struct {
	src    AccountMarginSource
	maxAge time.Duration
	clock  clock.Clock

	mu        sync.Mutex
	val       *order.AccountMargin
	fetchedAt time.Time
	failedAt  time.Time
}

// NewAccountMarginCache: maxAge<=0 は「期限なし」(テスト配線用)。
func NewAccountMarginCache(src AccountMarginSource, maxAge time.Duration, c clock.Clock) *AccountMarginCache {
	if c == nil {
		c = clock.System()
	}
	return &AccountMarginCache{src: src, maxAge: maxAge, clock: c}
}

// Get は entry 経路用。**キャッシュが使えるなら wire を打たない。**
// 第 2 返り値 false = 不明(呼び手は fail-close すること。ゼロ値を「充分」と読まない)。
func (c *AccountMarginCache) Get(ctx context.Context) (*order.AccountMargin, bool) {
	if c == nil || c.src == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	if c.val != nil && !c.expiredAt(now) {
		return c.val, true
	}
	// 🛑 失敗直後は床が明けるまで打たない。障害中に毎ティック再試行すると、
	// 削ったはずの通信が障害の形で戻ってくる。
	if !c.failedAt.IsZero() && now.Sub(c.failedAt) < accountMarginRetryFloor {
		return nil, false
	}
	return c.fetchLocked(ctx, now)
}

// Refresh は維持率ブレーカー用。**必ず wire を打ち**、結果をキャッシュへ入れる
// (entry 経路はそのぶんタダで新しい値を読める)。
func (c *AccountMarginCache) Refresh(ctx context.Context) (*order.AccountMargin, error) {
	if c == nil || c.src == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	am, err := c.src.GetAccountMargin(ctx)
	c.storeLocked(am, err, c.clock())
	return am, err
}

// Invalidate は「口座が変わった」印。次の Get が訊き直す。
// 🛑 nil-safe: 配線されていない経路から呼ばれても落ちない。
func (c *AccountMarginCache) Invalidate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.val, c.fetchedAt = nil, time.Time{}
	// 失敗の床も落とす — 建玉が動いた直後は「不明のまま 30 秒待つ」より
	// 1 回訊き直すほうが正しい。
	c.failedAt = time.Time{}
}

func (c *AccountMarginCache) expiredAt(now time.Time) bool {
	return c.maxAge > 0 && now.Sub(c.fetchedAt) >= c.maxAge
}

func (c *AccountMarginCache) fetchLocked(ctx context.Context, now time.Time) (*order.AccountMargin, bool) {
	am, err := c.src.GetAccountMargin(ctx)
	c.storeLocked(am, err, now)
	if err != nil || am == nil {
		return nil, false
	}
	return am, true
}

// 🛑 **失敗はキャッシュしない。** 直前の値も捨てる —— 「訊けなかった」を
// 「さっきは充分だった」で埋めると、担保ゲートが静かに fail-open になる。
func (c *AccountMarginCache) storeLocked(am *order.AccountMargin, err error, now time.Time) {
	if err != nil || am == nil {
		c.val, c.fetchedAt, c.failedAt = nil, time.Time{}, now
		return
	}
	c.val, c.fetchedAt, c.failedAt = am, now, time.Time{}
}
