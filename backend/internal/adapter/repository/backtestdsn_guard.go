package repository

import (
	"fmt"
	"net/url"
	"strings"
)

// 二重壁: db 名が "_backtest" で終わり、かつ live DSN と異なること。書込経路側に置くので `go run` でも fail-close。
// protectedDSNs は「これと一致してはいけない」実データの DSN。**可変長**なのは
// hybrid で守る対象が 2 本(research の STOCKBOT_DATABASE_URL と live track の
// STOCKBOT_LIVE_DATABASE_URL)になったため — 片方だけ渡すと、渡さなかった側は
// 二重壁の外に出る。
func SafeBacktestDSN(targetDSN string, protectedDSNs ...string) error {
	name := dbNameFromDSN(targetDSN)
	if name == "" {
		return fmt.Errorf("backtest DSN: cannot parse db name from %q", targetDSN)
	}
	if !strings.HasSuffix(name, "_backtest") {
		return fmt.Errorf("backtest DSN db %q must end in _backtest", name)
	}
	for _, p := range protectedDSNs {
		if p != "" && targetDSN == p {
			return fmt.Errorf("backtest DSN must differ from a real DATABASE_URL")
		}
	}
	return nil
}

// 同じ二重壁の integration 版("_test" で終わり、live DSN と異なること)。
func SafeIntegrationTestDSN(testDSN string, protectedDSNs ...string) error {
	name := dbNameFromDSN(testDSN)
	if name == "" {
		return fmt.Errorf("integration DSN: cannot parse db name from %q", testDSN)
	}
	if !strings.HasSuffix(name, "_test") {
		return fmt.Errorf("integration DSN db %q must end in _test", name)
	}
	for _, p := range protectedDSNs {
		if p != "" && testDSN == p {
			return fmt.Errorf("integration DSN must differ from a real DATABASE_URL")
		}
	}
	return nil
}

func dbNameFromDSN(dsn string) string {
	if dsn == "" {
		return ""
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return ""
		}
		return strings.TrimPrefix(u.Path, "/")
	}
	// keyword 形式: "... dbname=foo ..."
	for _, field := range strings.Fields(dsn) {
		if strings.HasPrefix(field, "dbname=") {
			return strings.TrimPrefix(field, "dbname=")
		}
	}
	return ""
}
