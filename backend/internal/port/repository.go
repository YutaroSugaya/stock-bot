package port

import (
	"context"
	"slices"
	"time"

	"stockbot/backend/internal/domain/market"
	"stockbot/backend/internal/domain/order"
	"stockbot/backend/internal/domain/position"
)

// PositionInsertInput is the row written when an entry saga completes. config_id
// and the full exit geometry are FROZEN here — switching the active config must
// not change an existing position (exception: ExtendMaxHold).
type PositionInsertInput struct {
	BrokerPositionID string
	Symbol           string
	Side             order.Side
	Quantity         int
	EntryPrice       float64

	// *JPY は円/株の「幅」、*Price は約定値に幅を足した**絶対価格**(呼値グリッド
	// 丸め済み)。broker の OCO と EvaluateExit は後者をそのまま使うので取り違え禁止。
	TakeProfitJPY          float64
	StopLossJPY            float64
	TakeProfitPrice        float64
	StopLossPrice          float64
	MaxHoldMinutes         int
	ExtensionMaxMinutes    int
	ExtensionUnrealizedJPY float64
	EarlyExitWindowMinutes int
	EarlyExitTargetJPY     float64
	RatchetArmJPY          float64
	RatchetGivebackJPY     float64

	// RatchetFloorAtArm はトレールの床が効く建玉かどうかを
	// **entry 時に凍結する**。床は凍結値から実行時に計算されるので、フラグが無いと
	// コード変更が harvest トラックの旧建玉にもその場で効き、測定対象が途中で入れ替わる。
	// 🛑 **新規建玉は true**(呼び手が忘れると旧規則で建ってしまうので guard test で縛る)。
	RatchetFloorAtArm bool

	StrategyConfigID string
	// StrategyName は凍結 config の戦略名(migration 0015)。ナンピン禁止のキーが
	// (銘柄, 側, 戦略)になったので、エントリー経路が join なしで引けるよう建玉に写す。
	StrategyName    string
	HoldingMode     order.HoldingMode
	ExecKind        order.ExecKind
	TickSizeAtEntry float64
	Source          position.Source

	EntryFeeJPY *float64 // nil = not yet reported; 0 = broker reported 0

	OpenedAt time.Time
}

type MaxHoldExtended struct {
	PositionID    int64
	NewMaxMinutes int
	// Warning は「延ばしたが、それだけでは守られない」ことの明示。
	//
	// 🚨 max_hold を伸ばしても **broker 側の守り(逆指値)の期日は動かない**。
	// 多日保有では守りの期日は有限(立花は最大 10 営業日・発注日起点)で、伸ばすのは
	// 寄り前の `ReplaceProtectiveOrder`(取消 → 再発注)だけ。200 を返すだけだと、人間は
	// 「30 日持てるようにした」と読んで**その間ずっと SL があると思い込む**。
	// 多日保有の延長では必ず載せる。
	Warning string `json:"warning,omitempty"`
}

// OpenCounts は口座全体の建玉の集計(CountOpenAcross の返り値)。
//
// 🚨 **Positions と Symbols は別物**。監視集合(= 時価取得の 1 リクエスト 120 銘柄枠)に
// 効くのは**銘柄数**だけで、同じ銘柄に 12 アーム乗っても 1 銘柄。本数の cap
// (account_max_open_positions)は建玉の総量、銘柄数の cap
// (account_max_open_symbols)は通信と時価の解像度を守る — 別々の目的なので両方要る。
type OpenCounts struct {
	// Positions は OPEN/CLOSING の建玉本数(external 含む)。
	Positions int
	// Symbols は同上の **銘柄数**(重複排除)。
	Symbols int
	// EntryArmSymbols は entryArms のいずれかを戦略名に持つ建玉の**銘柄数**。
	// 兄弟アーム(`X` / `X_trail`)は同じ銘柄に乗るので、本数ではなく銘柄で数える。
	EntryArmSymbols int
}

// PositionRepository is the position aggregate store. ClaimForClose is the CAS
// that prevents a double-close.
type PositionRepository interface {
	Insert(ctx context.Context, in PositionInsertInput) (int64, error)
	ListOpenOrClosing(ctx context.Context, symbol string) ([]position.Position, error)
	CountOpenAllSymbols(ctx context.Context) (int, error)

	// CountOpenAcross は口座全体の建玉の集計を **1 クエリ**で返す。entryArms は
	// 入口ごとの枠を数える対象のアーム名(基 + `_trail`。空なら EntryArm* は 0)。
	//
	// 🛑 まとめて返すのは往復を増やさないため: この集計は **armed 銘柄のシグナルごと**
	// (場中は 3〜6 秒おき)に走る。本数・銘柄数・入口の銘柄数を別々に訊くと、
	// 同じスナップショットのために 3 往復払うことになる。
	CountOpenAcross(ctx context.Context, entryArms []string) (OpenCounts, error)

	// CountOpenedSinceBySymbolStrategy は since 以降に (銘柄, 戦略) で建てた **bot 建玉**の数
	// (状態を問わない = 決済済みも数える)。日中保有の「同日 1 回転」の材料。
	CountOpenedSinceBySymbolStrategy(ctx context.Context, symbol, strategy string, since time.Time) (int, error)

	// CountOpenedSince は since 以降に口座全体で建てた **bot 建玉**の数(銘柄・戦略・状態を問わない・
	// external は数えない)。口座全体の「1 営業日の新規本数」の上限の材料。
	CountOpenedSince(ctx context.Context, since time.Time) (int, error)

	// UpdateProtectivePrices は **人間が板の守りの値段を変えたとき**に台帳の TP/SL(価格と幅)を
	// 同じ値へ揃える(OPEN の建玉だけ・config 凍結の例外)。ok=false = OPEN でない / id 不明。
	// 🚨 無いと、板の守りが消えたとき RearmUnguarded が古い凍結値で置き直す。
	UpdateProtectivePrices(ctx context.Context, id int64, tpPrice, slPrice, tpJPY, slJPY float64) (bool, error)

	// ApplySplit は株式分割(併合)の権利落ちで言い直した建玉(`position.SplitAdjusted` の結果)を
	// 書く。株数・円/株の凍結値・split_factor・split_adjusted_on を更新する(migration 0022)。
	// 🛑 CAS: **OPEN かつ adj.SplitAdjustedOn の日にまだ調整していない**行だけ。権利落ちの判定は
	// その日のうちは毎ティック・再起動後も成立するので、この条件が無いと 1:5 を 25 分の 1 にする。
	// ok=false = 調整済み / OPEN でない / id 不明。
	ApplySplit(ctx context.Context, adj position.Position) (bool, error)

	// 全銘柄を返す(id 昇順)。config の symbols で回すと、日次ユニバースから外れた
	// 銘柄の建玉を起動時に取りこぼし、永久に決済できなくなる。
	ListOpenAllSymbols(ctx context.Context) ([]position.Position, error)

	// GetByID returns one position whatever its status, or (nil, nil) when the id
	// is unknown — 手動決済が「そんな id は無い(404)」と「もう OPEN ではない(409)」を
	// 区別するため。OPEN/CLOSING しか返さない一覧では CLOSED が「未知」に見える。
	GetByID(ctx context.Context, id int64) (*position.Position, error)

	ClaimForClose(ctx context.Context, id int64, claimedAt time.Time) (ok bool, err error)
	MarkClosed(ctx context.Context, id int64, closedAt time.Time) error
	// UpdateExcursion persists the MFE/MAE record (円/株) plus the ratchet arm
	// flag. **全建玉**が対象で、ratchet を持たない建玉でも peak/trough は動く —
	// トレールを事後に反実仮想で測るための記録。
	UpdateExcursion(ctx context.Context, id int64, peakUnrealized, troughUnrealized float64, armed bool) error
	ExtendMaxHold(ctx context.Context, id int64, addMinutes int) (*MaxHoldExtended, error)

	// AdoptExternal records a naked broker position found at reconcile: shown and
	// counted for collateral, but NOT managed (no bot-side exit).
	AdoptExternal(ctx context.Context, bp BrokerPosition, now time.Time) (int64, error)
}

// TradeRecord is the round-trip ledger row. ProfitLossJPY is GROSS; net is
// derived downstream as gross - FeeJPY + CarryJPY, and the daily-loss cap judges
// on that net.
type TradeRecord struct {
	PositionID    int64
	Symbol        string
	Side          order.Side
	Quantity      int
	EntryPrice    float64
	ClosePrice    float64
	ProfitLossJPY float64
	FeeJPY        float64
	CarryJPY      float64
	FeeEstimated  bool
	// enum: take_profit|stop_loss|max_hold|early_exit|ratchet_takeprofit|manual|
	//       reconcile_cold_close|broker_close|forced_flat|entry_compensated|external_close|
	//       ratchet_giveback_loss|harvest_expiry
	// **後ろ 2 つは戦略の出口ではない**のでエッジ判定からは除外する
	// (CloseReasonEntryCompensated / CloseReasonExternalClose を参照)。
	CloseReason string
	ClosedAt    time.Time
}

type LastClose struct {
	ClosedAt time.Time
	NetJPY   float64 // gross − fee + carry(台帳と同じ規約)
}

// PositionCloser flips a position to CLOSED and records its trade in ONE
// transaction — split, a crash between them loses or double-counts the trade.
// close_reason のうち **戦略の出口ではない** 3 つ(と関連の理由)。migration の CHECK と値を揃える
// (0011 / 0012 / 0022)。文字列を各層に散らすとずれても全部緑で通るので、正本はここ。
const (
	// CloseReasonEntryCompensated は約定した**後**に守りを board に置けず、entry saga が
	// 巻き戻した往復(migration 0011)。**bot 自身の行動**なので再入場ゲートは締める側に数える。
	CloseReasonEntryCompensated = "entry_compensated"
	// CloseReasonExternalClose は人間が証券アプリで建てた external 建玉の決済
	// (migration 0012)。**bot の行動ではない**。
	CloseReasonExternalClose = "external_close"

	// CloseReasonStopLoss / CloseReasonTakeProfit は **板の守りが約定した**ときの出口。
	// 値は `domain/position` の ExitDecision.Reason と同じ(`close_reason_test` が固定)。
	// 🛑 bot が出した決済と broker 側の守りの約定で**同じ理由を書く** — 同じ出口が
	// 経路によって別の名前で台帳に載ると、戦略別の集計が経路の分だけ割れる。
	CloseReasonStopLoss   = "stop_loss"
	CloseReasonTakeProfit = "take_profit"
	// CloseReasonBrokerClose は **broker 側で決済されたが、凍結 TP/SL のどちら側でも
	// なかった**往復(migration 0001 から CHECK にはあった)。人間が証券アプリで
	// 締めた / broker の都合で決済された、が主。
	//
	// 🛑 `reconcile_cold_close` と分ける。あちらは「**値段すら観測できず時価で
	// 近似した**」という記録の質のラベルで、出口の種類ではない。
	// 板の逆指値が約定した損切りなのに、実約定を一度も見に行かないまま後の
	// 時価で `reconcile_cold_close` として書かれる形を防ぐ。
	CloseReasonBrokerClose = "broker_close"

	// CloseReasonRatchetTakeProfit / CloseReasonRatchetGivebackLoss はトレール決済の
	// 2 つのラベル。**どちらも戦略の出口**なのでエッジ標本に入れる(上の 2 つとは扱いが違う)。
	CloseReasonRatchetTakeProfit   = "ratchet_takeprofit"
	CloseReasonRatchetGivebackLoss = "ratchet_giveback_loss" // migration 0013

	// CloseReasonHarvestExpiry は **後から課した保有期限**で落ちた建玉(migration 0018)。
	// 旧台帳(harvest トラック)専用で、`holding.max_hold_override_days` が
	// 有効なときだけ出る。
	//
	// 🛑 `max_hold` と分けるのは、あちらが「建玉時にその戦略が決めた出口」なのに対し、
	// これは **人間がトラックに課した打ち切り = 検閲標本**だから。同じ理由で記録すると、
	// エッジ判定も保有期間分布も検閲を自然決済として数える(期間の締めで残玉を
	// 潰したときと同じ壊れ方)。`cmd/counterfactual` の既定対象に入れてあるのはそのため。
	//
	// 🛑 **`IsNonStrategyClose` には足さない** — `forced_flat` / `max_hold` と同じ扱いで
	// エッジ標本には入れ、検閲であることは**理由の名前**と counterfactual で読む
	// (「残玉は検閲標本として台帳に記録」)。
	CloseReasonHarvestExpiry = "harvest_expiry"

	// CloseReasonSplitMisfire は **株式分割(併合)の権利落ちを損切り/利確と誤読して決済した**
	// 往復(migration 0022)。建玉の分割調整(`position.SplitAdjusted`)が入る前に起きた
	// paper の往復を訂正するための理由で、株数・値段は分割調整後で記録する。
	// 🛑 **戦略の出口ではない** = `IsNonStrategyClose` に入れる(エッジ標本から外す)。
	CloseReasonSplitMisfire = "split_misfire"
)

// RatchetCloseReason はトレール決済のラベルを **realized gross の符号**で決める。
// ratchet 以外の理由は素通し。
//
// 🚨 なぜ要るか: `ratchet_takeprofit` が**損失で出る**ことがある。旧規則の trail 決済はこれが
// 大半で、段2(LLM)も台帳も「利確が発火した」と読む。giveback > arm である限り、旧規則ではその帯が
// 構造的に損になる(床はその原因側を直す変更で、こちらはラベルの是正)。
//
// 🛑 **「決済値 vs 建値」の価格比較で分類しない** — SELL 建玉が入るので
// 空売りで逆になる。側に依らない gross(fee/carry 前)の符号だけを見る。
// 🛑 分類は**約定値から**行う(判定時の時価ではない)。新規則で giveback_loss が出るのは
// ギャップで床を飛ばした場合だけなので、その 1 円の差がそのままラベルの差になる。
func RatchetCloseReason(reason string, grossJPY float64) string {
	if reason != CloseReasonRatchetTakeProfit {
		return reason
	}
	if grossJPY < 0 {
		return CloseReasonRatchetGivebackLoss
	}
	return CloseReasonRatchetTakeProfit
}

// IsNonStrategyClose は「この決済は **bot の戦略が出した出口ではない**か」。
// **エッジ標本(cmd/forward-report → cmd/edge-judge)から外す判定の正本**で、
// 読み手が増えるたびに同じ switch を書き直すと 1 箇所ずれても全部緑で通る。
//
// 🛑 **除外するのはエッジ判定だけ**。画面の戦績と引け後
// パケットは**口座ベース**で、この 2 つも普通のトレードとして戦略に計上する —
// 別枠に逃がすと全件がそれだった日に「何も起きなかった」と読める。
func IsNonStrategyClose(closeReason string) bool {
	return slices.Contains(NonStrategyCloseReasons(), closeReason)
}

// NonStrategyCloseReasons is the set IsNonStrategyClose excludes, for SQL readers
// (pg.PairRepo passes it as a parameter instead of re-typing the literals).
func NonStrategyCloseReasons() []string {
	return []string{CloseReasonEntryCompensated, CloseReasonExternalClose, CloseReasonSplitMisfire}
}

// CountsTowardEntryGates は「この決済を **per-symbol の再入場ゲート**(連敗 /
// cooldown / 窓の取引回数)の入力に数えてよいか」。
//
// 🚨 external_close だけ false。人間が同じ銘柄で**勝って**決済すると、それが最新の
// 決済行になって bot の連敗 streak を切り、cooldown の種別を after_loss から
// after_take_profit(既定 0 秒 = 実質無効)に変えてしまう —— **人間の売買で bot の
// 安全ゲートが解除される**。
//
// 🛑 entry_compensated は true。あれは bot 自身の往復で、ゲートを**締める**ために
// 台帳へ書いた(migration 0011。書かなかったせいで live で 5 往復した)。
//
// 🛑 日次損失の合計(SumClosedLossJPYSince*)は**別**で、そちらは両方数える —
// 委託保証金・維持率には人間の損失も効くので、締める側の情報は落とさない。
func CountsTowardEntryGates(closeReason string) bool {
	return closeReason != CloseReasonExternalClose
}

type PositionCloser interface {
	CloseAndRecord(ctx context.Context, positionID int64, closedAt time.Time, trade TradeRecord) (ok bool, err error)
}

// TradeRepository exposes the aggregates the risk gate snapshot needs. The
// *BySymbol family keeps a per-symbol gate from being polluted by siblings.
type TradeRepository interface {
	Insert(ctx context.Context, t TradeRecord) (int64, error)

	SumClosedLossJPYSinceBySymbol(ctx context.Context, symbol string, since time.Time) (int, error)
	CountTradesSinceBySymbol(ctx context.Context, symbol string, since time.Time) (int, error)
	ConsecutiveLossesBySymbol(ctx context.Context, symbol string) (int, error)

	SumClosedLossJPYSince(ctx context.Context, since time.Time) (int, error)

	// nil = 決済履歴なし。損切り/利確直後のクールダウン判定に使う — これが無いと
	// 損切りした次の秒に同じ銘柄へ再エントリーできてしまう(チャーン)。
	LastCloseBySymbol(ctx context.Context, symbol string) (*LastClose, error)

	ClosedTradeReader
}

// ClosedTradeReader は決済台帳の read 側だけ(戦績の集計が使う唯一の口)。複数 DB の
// 合算(adapter/repository.MergedTrades)が書込系を持たずに満たせるよう分けてある。
type ClosedTradeReader interface {
	// closed_at 昇順。forward 台帳の read 側で、これが唯一のエッジの証拠。
	ListClosedSince(ctx context.Context, since time.Time) ([]TradeRecord, error)
}

// TradeStrategyResolver maps position IDs to the strategy that OPENED them
// (positions.strategy_name。空の行だけ positions.config_id → strategy_configs.strategy_name)。advisor は複数戦略を
// 同時に建てるので、戦略別の分計でしか「どれが損益を出したか」は判定できない。
// read 専用・Postgres のみ(in-memory では forward 記録自体が揮発する)。
type TradeStrategyResolver interface {
	StrategyByPositionID(ctx context.Context, positionIDs []int64) (map[int64]string, error)
}

// TradeScore is the screener score that strategy emitted at entry time
// (advisor_runs.input_json の advisory.screens[] 由来)。RunID は復元元(監査)。
type TradeScore struct {
	Score float64
	RunID string
}

// TradeScoreResolver restores entry-time screener scores (Postgres のみ)。
// 復元できなかった position は返り値の map に**現れない** — 呼び出し側は欠落を
// 「復元不能 N 件」として明示すること。黙って落とすと母数が縮んで検定が歪む。
type TradeScoreResolver interface {
	ScoreByPositionID(ctx context.Context, positionIDs []int64) (map[int64]TradeScore, error)
}

// StrategyConfigRecord is the port-level view of an active strategy config.
// Mode/status cross as plain strings, not config types (R1).
//
// YAML (the configs/ outbox) is the config source-of-truth today, so there is no
// StrategyConfig *port* here: both a StrategyConfigPromoter and a read-side
// StrategyConfigRepository lived here with no production caller and were removed
// rather than left as phantom capabilities. Re-add a port (1-Tx pg) only when the
// DB becomes the SoT.
type StrategyConfigRecord struct {
	ConfigID     string
	Symbol       string
	Mode         string
	StrategyName string
	Status       string // "active" | "expired" | "rejected"
	RawYAML      string
	AdvisorRunID string // "" = 人手/テスト/sentinel config → NULL で保存
	ActivatedAt  time.Time
}

type CandleRepository interface {
	Upsert(ctx context.Context, symbol string, candles []market.Candle) error
	List(ctx context.Context, symbol string, period KlinePeriod, limit int) ([]market.Candle, error)
}

// SignalRejection is the "なぜ今日エントリーしなかったのか" audit trail.
// Observability only: best-effort, never blocks the trading cycle.
//
// migration 0009: Reason は GROUP BY できる安定種別(gate reason の
// 先頭トークン: cooldown / open_positions / daily_loss …)、可変部分(時刻・数量)は
// Detail。挿入はエッジ記録(同一 symbol × 種別の連続は書かない)。0009 以前の行は
// 旧形式(Reason に全文・Detail 空)のまま残るので、集計はそれを見込むこと。
type SignalRejection struct {
	Symbol    string
	ConfigID  string
	Reason    string
	Detail    string
	CreatedAt time.Time
}

type SignalRejectionRepository interface {
	InsertRejection(ctx context.Context, r SignalRejection) error
}

// ScreenSnapshot is one (symbol × strategy) screen result at one LLM round.
// advisor_runs には選ばれた銘柄しか残らないので、「枠に入らなかった候補のその後」
// = 枠配分ロジック自体の検証には全候補のスコアが要る。Picked = そのラウンドで
// per-strategy round-robin の枠を得たか。監査・検定専用 — 取引経路は読まない。
type ScreenSnapshot struct {
	RoundAt   time.Time
	Symbol    string
	Strategy  string
	Triggered bool
	Score     float64
	Picked    bool
	// Side は入口が成立した**向き**("BUY" / "SELL" / 空 = 買い専用スクリーナー)。
	// 🛑 これが無いと **「発火したが建たなかった売り」がどこにも残らない**。
	// 建玉になった売りは positions.side に残るが、事前登録した切り分け
	// (売りの標本ゼロ = 一覧が空 / 真に非貸借 / そもそも発火していない)の
	// 3 つ目はスキャン側の向きでしか読めない。
	Side string
}

// Best-effort observability: a failed insert must never block the round.
type ScreenSnapshotRepository interface {
	InsertRound(ctx context.Context, rows []ScreenSnapshot) error
}
