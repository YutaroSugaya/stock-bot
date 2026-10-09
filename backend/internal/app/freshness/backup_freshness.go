package freshness

// backup_freshness.go — DB dump(~/.stockbot/backups)がどこまで新しいかの観測。
// **読むだけ**: pg_dump を起動せず、ファイルも書かず、名前とサイズを見るだけ。
//
// dump は forward 記録(trades / positions / advisor_runs)の**唯一のコピー**で、
// 落ちたぶんは戻せない。定期 dump が落ちた朝に pgdata ボリュームも消えると、
// 前日の dump が無い限りその期間の記録は復旧不能になる。
//
// **exit code も log も信用しない**のがこの観測の要点: db-backup.sh は container
// 不在を skip して exit 0 するので launchd からは成功に見え、log は誰も読まない。

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// BackupMaxAge: 定期は毎日 03:10 JST。24h 以下だと正常運用でも毎日鳴いて狼少年に
// なり、48h 以上だと1回飛ばしても翌朝に鳴かない。26h は**1回の欠落を翌朝までに必ず
// 捕まえる**最小の窓(03:10 に落ちたら翌 05:10 に鳴る = 人間が画面を見る前)。
const BackupMaxAge = 26 * time.Hour

// dumpStampLayout は db-backup.sh の `date -u +%Y%m%dT%H%M%SZ` と対。
const dumpStampLayout = "20060102T150405Z"

// dumpGlob は db-backup.sh が書く `<db名>-<stamp>.sql.gz`。
// 🚨 **DB は 1 本ではない**: research はサイクルごとに切る(`stockbot_c4` 等)/
// 実弾 = `stockbot_live`。`stockbot-*` だけを見ていた頃は
// **`stockbot_live` の欠落を構造的に検知できなかった**。DB 名を決め打ちにせず
// 「dump が 1 本でもある DB は全部監視する」形にする — サイクルごとに DB を切るので、
// 一覧を焼き込むと新しい DB が黙って監視外になる。
const dumpGlob = "*-*" + dumpSuffix
const dumpSuffix = ".sql.gz"

// BackupStatus is the read-only view of how current the newest DB dump is.
type BackupStatus struct {
	Newest   string  `json:"newest"` // 空 = 有効な dump がゼロ
	AgeHours float64 `json:"age_hours"`
	// JSON キーを1文字にすると DashboardConsumesBackupKeys が**何にでも一致して**
	// 素通りする(html のどこかに "n" は必ずある)。
	N int `json:"dumps"`
	// Stale は「鳴らすべきか」。**dump ゼロも true**(fail-close)。
	Stale bool `json:"stale"`
	// StaleDBs は古い側の DB 名(昇順)。どれが止まっているか分からないと、
	// 「バックアップが古い」の後に人間が探索することになる。
	StaleDBs []string `json:"stale_dbs,omitempty"`
	// MissingDBs は**このプロセスが使っているのに dump が 1 本も無い** DB(昇順)。
	//
	// 🚨 「1 本でも取れたら以後は監視対象」という規則は、まだ作られていない DB で
	// 鳴かないためのものだが、そのせいで**新しく切った DB が「dump されない ×
	// 監視されない」に同時に落ちる**(実際に新しい DB が無保護のまま走りかける)。
	// 使っている DB 名を渡してもらえば塞げる。
	MissingDBs []string `json:"missing_dbs,omitempty"`
}

// CheckBackup reports how old the newest usable dump in dir is.
// 「使える dump」の条件を絞ってあるのは、**壊れた退避が本物の警告を打ち消す**のを
// 防ぐため(0バイト = pg_dump 途中失敗の残骸、.tmp = 書き込み中)。
//
// expected(このプロセスが実際に使っている DB 名)も見る。空なら「dump がある DB だけ」を監視する。
func CheckBackup(dir string, expected []string, now time.Time, maxAge time.Duration) BackupStatus {
	out := BackupStatus{}
	// Glob のエラーも一致ゼロも dir 不在も同じ扱い: 「バックアップが確認できない」= 鳴らす。
	paths, _ := filepath.Glob(filepath.Join(dir, dumpGlob))

	// DB ごとの「最新 dump」。**一番古い DB を代表値にする** — 新しい方で上書きすると、
	// 3 本のうち 1 本が止まっている状態が一番静かになる。
	newestByDB := map[string]time.Time{}
	for _, p := range paths {
		name := strings.TrimSuffix(filepath.Base(p), dumpSuffix)
		cut := strings.LastIndex(name, "-")
		if cut <= 0 {
			continue
		}
		db, stamp := name[:cut], name[cut+1:]
		ts, err := time.Parse(dumpStampLayout, stamp)
		if err != nil {
			// 手で置いたファイルや別命名の退避。監視を殺さないよう黙って飛ばす。
			continue
		}
		info, err := os.Stat(p)
		if err != nil || info.Size() == 0 {
			continue
		}
		out.N++
		if ts.After(newestByDB[db]) {
			newestByDB[db] = ts
		}
	}

	// 🛑 **使っているのに dump が 1 本も無い DB** は、dump がある DB の新しさとは
	// 無関係に鳴らす。ここが無かったので、他の DB の dump でダッシュボードは緑のまま
	// だった。
	for _, db := range expected {
		if db == "" {
			continue
		}
		if _, ok := newestByDB[db]; !ok {
			out.MissingDBs = append(out.MissingDBs, db)
		}
	}
	sort.Strings(out.MissingDBs)

	if out.N == 0 {
		// ここを健全側へ倒すと**一番まずい状態が一番静かになる**。
		out.Stale = true
		return out
	}
	// 🛑 dump が 1 本も無い DB は監視できない(まだ作られていないだけかもしれない)。
	// サイクルごとに DB を切る運用では「これから作る DB」が常に居るので、不在で
	// 鳴らすと狼少年になる。1 本でも取れたら以後は監視対象。
	var oldest time.Time
	for db, ts := range newestByDB {
		if now.Sub(ts) > maxAge {
			out.StaleDBs = append(out.StaleDBs, db)
		}
		if oldest.IsZero() || ts.Before(oldest) {
			oldest = ts
			out.Newest = ts.Format(dumpStampLayout)
		}
	}
	sort.Strings(out.StaleDBs)
	out.AgeHours = now.Sub(oldest).Hours()
	out.Stale = len(out.StaleDBs) > 0 || len(out.MissingDBs) > 0
	return out
}

// DefaultBackupDir は db-backup.sh の BACKUP_DIR と対。HOME が引けなければ空を
// 返し、呼び出し側が監視を張らない(存在しない dir を鳴らし続けない)。
func DefaultBackupDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".stockbot", "backups")
}

// BackupWatch caches the latest snapshot so the dashboard's 2 秒ポーリングが
// stat を撒かない。
type BackupWatch struct {
	mu   sync.RWMutex
	last BackupStatus
}

func (w *BackupWatch) set(s BackupStatus) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.last = s
}

func (w *BackupWatch) Get() BackupStatus {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.last
}

// refreshFor re-runs the check and warns while the newest dump is stale. 警告は毎回
// 出す — 1回流れるだけの log では見落としを防げない。expected はこのプロセスが
// 使っている DB 名(dump が 1 本も無い DB を名指しで鳴らす)。
func (w *BackupWatch) RefreshFor(dir string, expected []string, now time.Time, logger *slog.Logger) {
	s := CheckBackup(dir, expected, now, BackupMaxAge)
	w.set(s)
	if len(s.MissingDBs) > 0 {
		logger.Error("🚨 使用中の DB に dump が 1 本も無い — その台帳は**無保護**です。"+
			"`make backup-install` で 3 DB 対応版を配備し、`make backup-now` を 1 回打つこと",
			"dir", dir, "missing_dbs", s.MissingDBs)
	}
	if !s.Stale {
		return
	}
	if len(s.StaleDBs) == 0 && s.N > 0 {
		return // 鳴らす理由は MissingDBs だけ(上で出した)
	}
	logger.Warn("DB バックアップが古い — forward 記録(trades / positions / advisor_runs)は"+
		"dump が唯一のコピーです。`make backup-now` と ~/.stockbot/logs/db-backup.log、"+
		"docker が起動しているかを確認してください",
		"dir", dir, "newest", s.Newest, "age_hours", s.AgeHours,
		"max_age_hours", BackupMaxAge.Hours(), "dumps", s.N)
}
