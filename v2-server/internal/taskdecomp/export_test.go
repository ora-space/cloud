package taskdecomp

import "time"

// SetPollIntervalForTest shortens the watcher poll, so tests do not wait the production interval
// (or, with a long one, keep a watcher from re-reading a run before the test moves on).
func SetPollIntervalForTest(h *Host, d time.Duration) { h.poll = d }
