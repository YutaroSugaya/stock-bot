package config

import (
	"fmt"
	"os"
)

// LedgerDSN は読み出し系 cmd の `-db` を DSN に解決する(forward-report / pair-diff /
// holding-period / daily-review / counterfactual が共有する)。
//
//   - research(既定・空)… STOCKBOT_DATABASE_URL
//   - live … STOCKBOT_LIVE_DATABASE_URL。**既定にしない**: 空の live DB を読んで
//     「取引ゼロ」と報告するより、明示的に `-db live` と打たせる方が誤読が起きない。
//   - harvest … STOCKBOT_HARVEST_DATABASE_URL(旧台帳)。これが無いとシェルで
//     DSN を差し替えるしかなくなり、失敗すると「別の台帳の数字を読む」形で静かに壊れる。
//
// 🛑 未設定の台帳は既定へ落ちず error(取り違えを静かに通さない)。
func LedgerDSN(db string) (string, error) {
	switch db {
	case "research", "":
		url := os.Getenv("STOCKBOT_DATABASE_URL")
		if url == "" {
			return "", fmt.Errorf("STOCKBOT_DATABASE_URL が未設定です。forward 記録は Postgres 稼働時のみ残ります(in-memory は停止で消える)。source .env してから実行してください")
		}
		return url, nil
	case "live":
		url := os.Getenv("STOCKBOT_LIVE_DATABASE_URL")
		if url == "" {
			return "", fmt.Errorf("-db live には STOCKBOT_LIVE_DATABASE_URL が必要です(live track の台帳は research とは別 DB)")
		}
		return url, nil
	case "harvest":
		url := os.Getenv("STOCKBOT_HARVEST_DATABASE_URL")
		if url == "" {
			return "", fmt.Errorf("-db harvest には STOCKBOT_HARVEST_DATABASE_URL が必要です(harvest track の台帳は research とは別 DB)")
		}
		return url, nil
	default:
		return "", fmt.Errorf("-db は research / harvest / live のいずれか: %q", db)
	}
}
