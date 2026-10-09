package main

import (
	"log/slog"
	"os"
	"time"

	"stockbot/backend/internal/config"
	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/domain/session"
)

// 日次ユニバースの鮮度**継続**監視(読むだけ)。
//
// 🛑 **bot は `symbols_file` を起動時に一度しか読まない**(`logUniverseProvenance` の
// コメントに既出)。監視銘柄は起動時に bundle として配線されるので、走らせたまま日を
// 跨いでも銘柄は入れ替わらない。つまり **`make start` を毎日打たないと「毎朝選び直す
// 200銘柄」という事前登録が黙って破れる**。
//
// 既存の警告は**起動時の1回だけ**で、まさに問題になる場面 —「起動しっぱなしで日を
// 跨ぐ」— では二度と鳴らない(起動した日は正常だったのだから)。だから定期的に鳴らす。
// 日足 CSV / DB dump の鮮度監視と同じ設計(無音故障を作らない)。
//
// 🛑 **mtime を毎回 stat し直してはいけない。** 朝ジョブは同じパスを書き換えるので、
// 走っているプロセスが読んだ集合とは無関係に mtime だけ当日になる = 永久に鳴らない。
// 比較するのは**起動時に掴んだ mtime**(= いま実際に使っている集合の日付)。

// universeModTime は起動時に読んだユニバースファイルの mtime。読めなければゼロ値。
func universeModTime(symbolsFile string) time.Time {
	if symbolsFile == "" {
		return time.Time{}
	}
	path, err := config.ExpandHome(symbolsFile)
	if err != nil {
		return time.Time{}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// staleUniverse は「起動時に読んだユニバースが古いか」を返す。
//
// 警告するのは **取引日の 07:00 以降**だけ:
//   - 休場日はそもそも選び直されないので、古くて当然(狼少年にしない)
//   - 取引日でも 07:00 の選定より前は、その日ぶんがまだ存在しない
func staleUniverse(loaded, now time.Time, hours session.TradingHours) (days int, warn bool) {
	if loaded.IsZero() {
		return 0, false
	}
	l := loaded.In(clock.JST)
	n := now.In(clock.JST)
	lDay := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, clock.JST)
	nDay := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, clock.JST)
	days = int(nDay.Sub(lDay).Hours() / 24)
	if days <= 0 {
		return 0, false
	}
	if !hours.IsTradingDay(n) || n.Hour() < universeScreenHourJST {
		return days, false
	}
	return days, true
}

// 朝ジョブ(launchd)がその日のユニバースを書く時刻。これより前は「まだ無い」が正常。
const universeScreenHourJST = 7

func warnStaleUniverse(loaded time.Time, now time.Time, hours session.TradingHours, symbolsFile string, logger *slog.Logger) {
	days, warn := staleUniverse(loaded, now, hours)
	if !warn || logger == nil {
		return
	}
	logger.Warn("日次ユニバースが古い — bot は起動時にしか読まない。make stop → make start で入れ替わる",
		"file", symbolsFile,
		"loaded", loaded.In(clock.JST).Format("2006-01-02 15:04"),
		"days_old", days)
}
