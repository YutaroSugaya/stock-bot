// Package notifier は port.Notifier の実装。
package notifier

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"stockbot/backend/internal/domain/clock"
	"stockbot/backend/internal/port"
)

type Stdout struct {
	w     io.Writer
	clock clock.Clock
}

func NewStdout() *Stdout {
	return &Stdout{w: os.Stdout, clock: clock.System()}
}

// 通知は best-effort。書込エラーは返すだけで、取引経路を止める判断は呼び手に委ねる。
func (s *Stdout) Notify(_ context.Context, level, title, message string) error {
	ts := s.clock().UTC().Format(time.RFC3339)
	_, err := fmt.Fprintf(s.w, "%s NOTIFY level=%s title=%q message=%q\n", ts, level, title, message)
	return err
}

var _ port.Notifier = (*Stdout)(nil)
