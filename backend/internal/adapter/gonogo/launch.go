package gonogo

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"stockbot/backend/internal/port"
)

// Launcher は bot の画面のボタンから cmd/gonogo を別プロセスで起動する(`gonogo -symbols …`・起動経路 button)。
// 待たずに戻る。判定の結果はいつもどおり判定ファイルに書かれ、画面は次の更新で読む。
//
// 🛑 **表示と記録だけ**。bot は判定を起動するだけで、結果を発注経路に渡さない。
// 子は別のプロセスグループで動かす(bot を止めても判定は最後まで書き切る)。
type Launcher struct {
	Bin      string // ~/.stockbot/bin/gonogo(catchup が配る)
	LogPath  string // ~/.stockbot/logs/gonogo.log(追記)
	LockPath string // ~/.stockbot/run/gonogo.lock(gonogo 自身が取る排他)
}

// Launch は symbols の判定を起動する。実行中なら port.ErrGoNoGoRunning。
func (l *Launcher) Launch(symbols []string) error {
	if holder, err := lockHolder(l.LockPath); err == nil && holder > 0 && processAlive(holder) {
		return port.ErrGoNoGoRunning
	}
	if st, err := os.Stat(l.Bin); err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
		return fmt.Errorf("gonogo のバイナリが無い(%s): catchup がまだ配っていない", l.Bin)
	}
	if err := os.MkdirAll(filepath.Dir(l.LogPath), 0o755); err != nil {
		return err
	}
	logf, err := os.OpenFile(l.LogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(l.Bin, "-symbols", strings.Join(symbols, ","))
	cmd.Env = append(os.Environ(), "STOCKBOT_GONOGO_TRIGGER=button")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		return err
	}
	go func() {
		_ = cmd.Wait() // 終わった子を回収する(zombie を残さない)
		_ = logf.Close()
	}()
	return nil
}
