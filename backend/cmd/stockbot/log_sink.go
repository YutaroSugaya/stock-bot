package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"stockbot/backend/internal/domain/clock"
)

// ログの二重化(stdout + 日付ファイル)。
//
// 🛑 **`make start` だけでログが残るようにする**。
// それまでは `make start 2>&1 | tee ~/.stockbot/logs/bot-$(date +%F).log` を
// 人間が付ける前提で、実際に付け忘れが起きて**訂正 API の実機初検証のログが
// 1 行も残らなかった**。Makefile は enforcement ファイルで AI が触れないので、
// bot 自身が書く。起動のしかたに依存しないぶん、こちらのほうが確実でもある。
//
// 🛑 **stdout は止めない。** 前景で走らせている人間の画面から消してはいけない。

// defaultLogDir はログの置き場。**`~/.stockbot` 配下**に置く —— Desktop 配下は
// macOS TCC で launchd から触れず、ビルド済みバイナリは触れた瞬間 SIGKILL される。
func defaultLogDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".stockbot", "logs")
}

// openLogSink は当日ぶんのログファイルを**追記で**開く。
//
// 🛑 truncate しない。同じ日に再起動すると朝のログが消える(`tee` はまさに
// これを起こす)。1 日 1 ファイルで、再起動ぶんは後ろに積む。
func openLogSink(dir string, now time.Time) (*os.File, string, error) {
	if dir == "" {
		return nil, "", fmt.Errorf("ログの置き場が決まらない(HOME を取得できない)")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", fmt.Errorf("ログ用ディレクトリを作れない %s: %w", dir, err)
	}
	path := filepath.Join(dir, "bot-"+now.In(clock.JST).Format("2006-01-02")+".log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, "", fmt.Errorf("ログファイルを開けない %s: %w", path, err)
	}
	return f, path, nil
}

// logWriter は stdout と当日ファイルの両方へ書く io.Writer を返す。
// ファイルを開けなければ **stdout だけ**で続行する(起動は止めない) ——
// ログが残らないことより bot が上がらないことのほうが害が大きい。
// 第 2 戻り値は「人間に伝えるべき注意」(空 = 問題なし)。
func logWriter(now time.Time) (io.Writer, string, func()) {
	f, path, err := openLogSink(defaultLogDir(), now)
	if err != nil {
		return os.Stdout, "⚠ ログをファイルに残せない(stdout のみ): " + err.Error(), func() {}
	}
	return io.MultiWriter(os.Stdout, f), "ログの写し: " + path, func() { _ = f.Close() }
}
