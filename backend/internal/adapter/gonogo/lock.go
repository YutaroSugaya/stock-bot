package gonogo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// AcquireLock は実行の排他(~/.stockbot/run/gonogo.lock に pid を書く)。取れなければ ok=false で、
// 呼び手は何もせず終わる。pid が死んでいる古いロックは取り直す。release は自分のロックだけ消す。
func AcquireLock(path string) (release func(), ok bool, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			pid := os.Getpid()
			_, werr := fmt.Fprintf(f, "%d\n", pid)
			_ = f.Close()
			if werr != nil {
				_ = os.Remove(path)
				return nil, false, werr
			}
			return func() {
				if holder, _ := lockHolder(path); holder == pid {
					_ = os.Remove(path)
				}
			}, true, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, false, err
		}
		holder, rerr := lockHolder(path)
		if rerr == nil && holder > 0 && processAlive(holder) {
			return nil, false, nil
		}
		// 古いロック(pid が死んでいる・読めない)は消して取り直す。
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, false, err
		}
	}
	return nil, false, nil
}

func lockHolder(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(raw)))
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
