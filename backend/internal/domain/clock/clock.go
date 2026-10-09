// Package clock は注入可能な時刻源。domain 内で package-level の time.Now() は禁止。
package clock

import "time"

type Clock func() time.Time

func System() Clock { return time.Now }

func Fixed(t time.Time) Clock { return func() time.Time { return t } }
