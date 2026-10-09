package order

// 執行トラック。intraday = 一日信用(引け前に強制フラット・carry 0)/ multiday = 現物・一般信用(建越し)。建玉時に凍結。
type HoldingMode string

const (
	HoldingIntraday HoldingMode = "intraday"
	HoldingMultiday HoldingMode = "multiday"
)

func (m HoldingMode) Valid() bool { return m == HoldingIntraday || m == HoldingMultiday }

type ExecKind string

const (
	ExecCash         ExecKind = "cash"          // 現物
	ExecMarginOneday ExecKind = "margin_oneday" // 一日信用
	// ExecMarginGeneral は一般信用(6ヶ月)。**立花 e支店の口座では通らなかった**
	// (4751 で「現金信用区分に誤りがあります」)。定義は残すが、
	// 立花で使うなら口座側の取扱い確認が先。
	ExecMarginGeneral ExecKind = "margin_general"
	// ExecMarginSystem は制度信用(6ヶ月)。立花 e支店で**実際に通る**信用区分で、
	// 公式サンプルの信用注文6例も全てこちら。
	ExecMarginSystem ExecKind = "margin_system"
)

// IsMargin は信用建玉かどうか。**判定はここ 1 か所**に集約する — 維持率ポーリング /
// carry / collateral / 建玉照会が各自 `== ExecMarginGeneral` を書いていると、
// 区分が増えたときに必ずどこかが漏れる(制度信用を足したときに顕在化)。
func (e ExecKind) IsMargin() bool { return e != ExecCash && e != "" }
