package notifier

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"stockbot/backend/internal/domain/clock"
)

func TestStdout_Notify(t *testing.T) {
	var buf bytes.Buffer
	at := time.Date(2026, 6, 18, 5, 30, 0, 0, time.UTC)
	n := &Stdout{w: &buf, clock: clock.Fixed(at)}

	if err := n.Notify(context.Background(), "warn", "emergency", "margin breach"); err != nil {
		t.Fatal(err)
	}

	got := buf.String()
	for _, want := range []string{"2026-06-18T05:30:00Z", "level=warn", `title="emergency"`, `message="margin breach"`} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q missing %q", got, want)
		}
	}
}
