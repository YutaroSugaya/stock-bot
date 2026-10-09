package repository

import "testing"

func TestSafeBacktestDSN(t *testing.T) {
	live := "postgres://u:p@h:5432/stockbot"
	cases := []struct {
		name    string
		target  string
		live    string
		wantErr bool
	}{
		{"valid backtest db", "postgres://u:p@h:5432/stockbot_backtest", live, false},
		{"missing suffix", "postgres://u:p@h:5432/stockbot", live, true},
		{"equals live", live, live, true},
		{"unparseable", "", live, true},
		{"keyword form ok", "host=h dbname=foo_backtest user=u", live, false},
		{"keyword form bad", "host=h dbname=foo user=u", live, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := SafeBacktestDSN(tc.target, tc.live)
			if (err != nil) != tc.wantErr {
				t.Fatalf("SafeBacktestDSN(%q) err=%v, wantErr=%v", tc.target, err, tc.wantErr)
			}
		})
	}
}

func TestSafeIntegrationTestDSN(t *testing.T) {
	live := "postgres://u:p@h:5432/stockbot"
	if err := SafeIntegrationTestDSN("postgres://u:p@h:5432/stockbot_test", live); err != nil {
		t.Fatalf("valid _test dsn rejected: %v", err)
	}
	if err := SafeIntegrationTestDSN("postgres://u:p@h:5432/stockbot", live); err == nil {
		t.Fatal("non-_test dsn must be rejected")
	}
}
