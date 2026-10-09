package command

// OpsCounters is a local interface so the sagas never import the app layer.
// Injection is optional (nil-safe): counters are best-effort observability and
// must never sit on the trading/safety path.
type OpsCounters interface {
	IncrCompensations()
	IncrExternalAdoptions()
	// 値幅制限の外に出た利確指値を落として SL のみ置いた回数。黙って縮退させない
	// (画面で「TP は板に無く bot が見ている」状態を数えられるようにする)。
	IncrProtectiveTPDropped()
}
