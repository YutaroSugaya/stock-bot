package main

import (
	"strconv"
	"sync/atomic"
)

// counter is a monotonic id source for SignalIDs. domain は rand を使えない
// (決定論の要求)ので、生成器は cmd から注入する。
type counter struct{ n atomic.Int64 }

func (c *counter) next() string { return strconv.FormatInt(c.n.Add(1), 10) }
