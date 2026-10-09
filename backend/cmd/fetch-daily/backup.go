package main

// backup.go — 日足 CSV を書き換える**直前に同じプロセスで**退避する。守りたい相手が
// まさにこの書き換え(merge の lost update / chain-link の誤検出 / 同時実行)なので、
// 別プロセスの定期バックアップだと壊した後の世代しか残らない窓ができる。
//
// 対象は fetch-daily が書き換えるファイル(<sym>_daily.csv + bench_topix.csv)に加え、
// **再取得できない記録**(jpx_stops*.csv)。後者は書き換えないが、退避が他に無い。
// 分足は 96MB あり 30世代で数GBになるうえ、ここでは書き換えない(recorder が O_APPEND)。

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"stockbot/backend/internal/adapter/candlecsv"
	"stockbot/backend/internal/app"
)

const (
	// タイムスタンプは UTC 固定幅なので**辞書順 = 時系列順**。rotation が mtime に依存しない。
	backupArchivePrefix = "daily-"
	// benchCSVName is the TOPIX benchmark series (edge-eval のベンチ超過ゲートの入力)。
	benchCSVName = "bench_topix.csv"
	// jpx-stops の記録。fetch-daily は**書き換えない**が、退避対象に入れる:
	// JPX は当日分しか置かないので**再取得が原理的に不可能**で、しかも git 管理外・
	// db-backup の対象外 = 唯一のコピー。日足より失うと痛い(日足は再取得できる)。
	jpxStopsName     = "jpx_stops.csv"
	jpxStopsDaysName = "jpx_stops_days.csv"
)

// defaultBackupDir は退避先の既定。Desktop 配下に置くと launchd から触れないので
// ~/.stockbot/backups の下に置く。HOME 未解決なら空 = 退避なし。
func defaultBackupDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".stockbot", "backups", "data")
}

// backupDailyCSVs snapshots outDir's daily CSVs into backupDir as a timestamped
// tar.gz and prunes all but the newest `keep` generations. It returns the
// archive path, or "" when there was nothing to back up (an empty archive would
// push a real generation out of the window — the worst failure being "backups
// exist but are empty"). 書き込みは tmp + rename。
func backupDailyCSVs(outDir, backupDir string, at time.Time, keep int) (string, error) {
	files, err := backupTargets(outDir)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", nil
	}
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return "", fmt.Errorf("退避先を作れない: %w", err)
	}
	out := filepath.Join(backupDir, backupArchivePrefix+at.UTC().Format("20060102T150405Z")+".tar.gz")
	tmp := out + ".tmp"
	if err := writeArchive(tmp, outDir, files); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, out); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("退避の rename 失敗: %w", err)
	}
	if err := rotateBackups(backupDir, keep); err != nil {
		// 退避自体は成功。古い世代が消せないだけで取得を止める理由にはならない。
		return out, nil
	}
	return out, nil
}

// backupTargets lists the files fetch-daily is about to rewrite, relative to dir.
func backupTargets(dir string) ([]string, error) {
	syms, err := candlecsv.DailySymbols(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(syms)+1)
	for _, sym := range syms {
		names = append(names, filepath.Base(candlecsv.DailyFile(dir, sym)))
	}
	for _, extra := range []string{benchCSVName, jpxStopsName, jpxStopsDaysName} {
		if _, err := os.Stat(filepath.Join(dir, extra)); err == nil {
			names = append(names, extra)
		}
	}
	sort.Strings(names) // アーカイブの中身を実行ごとに同じ順にする(差分を読める)
	return names, nil
}

func writeArchive(path, srcDir string, names []string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("退避ファイルを作れない: %w", err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, name := range names {
		src := filepath.Join(srcDir, name)
		b, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("退避元を読めない %s: %w", name, err)
		}
		st, err := os.Stat(src)
		if err != nil {
			return fmt.Errorf("退避元を stat できない %s: %w", name, err)
		}
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(b)), ModTime: st.ModTime()}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("退避 header 書き込み失敗 %s: %w", name, err)
		}
		if _, err := tw.Write(b); err != nil {
			return fmt.Errorf("退避 body 書き込み失敗 %s: %w", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("退避 tar を閉じられない: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("退避 gzip を閉じられない: %w", err)
	}
	return f.Sync()
}

// dailyDayRange reports the oldest / newest last-bar day across the symbols'
// CSVs ("2006-01-02"), skipping files it cannot read. 末尾行の読み口は
// app.LastRowDayJST に一本化(自前で切り出すとフォーマット変更で片方だけずれる)。
func dailyDayRange(dir string, symbols []string) (oldest, newest string) {
	for _, sym := range symbols {
		day, err := app.LastRowDayJST(candlecsv.DailyFile(dir, sym))
		if err != nil || day == "" {
			continue
		}
		if oldest == "" || day < oldest {
			oldest = day
		}
		if newest == "" || day > newest {
			newest = day
		}
	}
	return oldest, newest
}

func orDash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

// rotateBackups keeps the newest `keep` archives (名前順 = 時系列順).
func rotateBackups(dir string, keep int) error {
	if keep <= 0 {
		return nil
	}
	got, err := filepath.Glob(filepath.Join(dir, backupArchivePrefix+"*.tar.gz"))
	if err != nil {
		return err
	}
	if len(got) <= keep {
		return nil
	}
	sort.Strings(got)
	for _, old := range got[:len(got)-keep] {
		if err := os.Remove(old); err != nil {
			return err
		}
	}
	return nil
}
