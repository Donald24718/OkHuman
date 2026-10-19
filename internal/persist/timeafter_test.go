package persist

import "time"

func timeAfter(ms int) <-chan time.Time { return time.After(time.Duration(ms) * time.Millisecond) }
