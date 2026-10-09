package app

// 空文字 = ファイルが無い / データ行が無い(呼び出し側が意味を決める)。
// 末尾行の切り出しは recorder の lastRowTime と共有する — 各所で自前に切ると
// CSV フォーマットを変えたとき片方だけ静かにずれる。
func LastRowDayJST(path string) (string, error) {
	ts, err := lastRowTime(path)
	if err != nil {
		return "", err
	}
	if ts.IsZero() {
		return "", nil
	}
	return ts.In(recorderJST).Format("2006-01-02"), nil
}
